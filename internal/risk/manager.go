package risk

import (
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	
	"github.com/web3guy0/polybot/internal/strategy"
)

// RiskManager handles all risk-related decisions
// This is the GATEKEEPER - no trade happens without risk approval
type RiskManager struct {
	config    RiskConfig
	state     *RiskState
	mu        sync.RWMutex
}

// RiskConfig defines risk parameters
type RiskConfig struct {
	// Position Sizing
	MaxBetSize       decimal.Decimal // Maximum bet per trade
	MinBetSize       decimal.Decimal // Minimum bet (below this = skip)
	MaxPositionPct   float64         // Max % of bankroll per position
	
	// Daily Limits
	MaxDailyLoss     decimal.Decimal // Stop trading after this loss
	MaxDailyTrades   int             // Maximum trades per day
	MaxDailyExposure decimal.Decimal // Max total exposure
	
	// Confidence Filters
	MinConfidence    float64         // Minimum signal confidence (0-1)
	MinStrength      strategy.Strength // Minimum signal strength
	
	// Market Filters
	MinLiquidity     decimal.Decimal // Don't trade illiquid markets
	MaxSpread        float64         // Max bid-ask spread %
	MinOdds          float64         // Min acceptable odds
	MaxOdds          float64         // Max acceptable odds (avoid extreme longshots)
	
	// Timing Filters
	MinTimeToExpiry  time.Duration   // Don't trade windows about to close
	TradeCooldown    time.Duration   // Wait between trades
	
	// Volatility Filters
	MaxVolatility    float64         // Don't trade during extreme vol
	ChopFilter       bool            // Enable "don't trade during chop" filter
	ChopThreshold    float64         // Score threshold for chop detection
}

// RiskState tracks current risk exposure
type RiskState struct {
	DailyPnL         decimal.Decimal
	DailyTrades      int
	DailyExposure    decimal.Decimal
	OpenPositions    []Position
	LastTradeTime    time.Time
	TradingDay       time.Time
	
	// Window tracking - prevents double-betting same window
	TradedWindows    map[string]time.Time // windowID -> trade time
	
	// Performance tracking
	WinStreak        int
	LossStreak       int
	ConsecutiveLosses int
}

// Position represents an open position
type Position struct {
	ID          string
	Asset       string
	Direction   strategy.Direction
	Size        decimal.Decimal
	EntryPrice  decimal.Decimal
	EntryTime   time.Time
	MarketID    string
}

// TradeDecision is the result of a risk check
type TradeDecision struct {
	Allowed     bool
	Reason      string
	SuggestedSize decimal.Decimal
	Warnings    []string
}

// DefaultRiskConfig returns sensible defaults
func DefaultRiskConfig() RiskConfig {
	return RiskConfig{
		MaxBetSize:       decimal.NewFromFloat(10.0),  // $10 max per trade
		MinBetSize:       decimal.NewFromFloat(1.0),   // $1 minimum
		MaxPositionPct:   0.05,                        // 5% of bankroll max
		MaxDailyLoss:     decimal.NewFromFloat(50.0),  // Stop at $50 daily loss
		MaxDailyTrades:   20,                          // Max 20 trades/day
		MaxDailyExposure: decimal.NewFromFloat(100.0), // $100 max exposure
		MinConfidence:    0.60,                        // 60% minimum confidence
		MinStrength:      strategy.StrengthModerate,
		MinLiquidity:     decimal.NewFromFloat(1000.0),// $1000 min liquidity
		MaxSpread:        0.05,                        // 5% max spread
		MinOdds:          0.20,                        // Min 20 cent odds
		MaxOdds:          0.80,                        // Max 80 cent odds
		MinTimeToExpiry:  2 * time.Minute,             // Don't trade last 2 min
		TradeCooldown:    30 * time.Second,            // 30s between trades
		MaxVolatility:    0.10,                        // 10% max 15m vol
		ChopFilter:       true,
		ChopThreshold:    25.0,                        // Score < 25 = chop
	}
}

// NewRiskManager creates a new risk manager
func NewRiskManager(config RiskConfig) *RiskManager {
	return &RiskManager{
		config: config,
		state: &RiskState{
			TradingDay:     time.Now().Truncate(24 * time.Hour),
			TradedWindows:  make(map[string]time.Time),
		},
	}
}

// CanTrade is the main entry point - checks if a trade is allowed
func (r *RiskManager) CanTrade(signal strategy.Signal, marketCtx strategy.MarketContext, bankroll decimal.Decimal) TradeDecision {
	r.mu.Lock()
	defer r.mu.Unlock()
	
	decision := TradeDecision{
		Allowed:  true,
		Warnings: []string{},
	}
	
	// Reset daily state if new day
	r.checkDayReset()
	
	// 1. Check signal quality
	if err := r.checkSignalQuality(signal); err != "" {
		decision.Allowed = false
		decision.Reason = err
		return decision
	}
	
	// 2. Check daily limits
	if err := r.checkDailyLimits(); err != "" {
		decision.Allowed = false
		decision.Reason = err
		return decision
	}
	
	// 3. Check market conditions
	if err := r.checkMarketConditions(marketCtx); err != "" {
		decision.Allowed = false
		decision.Reason = err
		return decision
	}
	
	// 4. Check timing
	if err := r.checkTiming(marketCtx); err != "" {
		decision.Allowed = false
		decision.Reason = err
		return decision
	}
	
	// 5. Check cooldown
	if err := r.checkCooldown(); err != "" {
		decision.Allowed = false
		decision.Reason = err
		return decision
	}
	
	// 6. Check one-position-per-window
	if err := r.checkWindowPosition(marketCtx); err != "" {
		decision.Allowed = false
		decision.Reason = err
		return decision
	}
	
	// 7. Calculate position size
	size := r.calculatePositionSize(signal, bankroll)
	if size.LessThan(r.config.MinBetSize) {
		decision.Allowed = false
		decision.Reason = "Calculated size below minimum"
		return decision
	}
	decision.SuggestedSize = size
	
	// 8. Add warnings (non-blocking)
	if r.state.ConsecutiveLosses >= 3 {
		decision.Warnings = append(decision.Warnings, "⚠️ On a losing streak")
	}
	if signal.Strength == strategy.StrengthModerate {
		decision.Warnings = append(decision.Warnings, "⚠️ Moderate signal strength")
	}
	
	return decision
}

// checkSignalQuality validates the signal meets quality thresholds
func (r *RiskManager) checkSignalQuality(signal strategy.Signal) string {
	// Check for explicit NO_TRADE signal
	if signal.Direction == strategy.DirectionNoTrade {
		return "Signal explicitly says NO_TRADE: " + signal.Reason
	}
	
	if signal.Confidence < r.config.MinConfidence {
		return "Confidence below minimum threshold"
	}
	
	if !r.strengthMeetsMin(signal.Strength) {
		return "Signal strength below minimum"
	}
	
	if signal.IsExpired() {
		return "Signal has expired"
	}
	
	// Chop filter - don't trade when score is too weak
	if r.config.ChopFilter && abs(signal.Score) < r.config.ChopThreshold {
		return "Market is choppy - score too weak"
	}
	
	return ""
}

// checkDailyLimits validates we haven't hit daily limits
func (r *RiskManager) checkDailyLimits() string {
	if r.state.DailyPnL.LessThan(r.config.MaxDailyLoss.Neg()) {
		return "Daily loss limit reached"
	}
	
	if r.state.DailyTrades >= r.config.MaxDailyTrades {
		return "Daily trade limit reached"
	}
	
	if r.state.DailyExposure.GreaterThan(r.config.MaxDailyExposure) {
		return "Daily exposure limit reached"
	}
	
	return ""
}

// checkMarketConditions validates market is suitable for trading
func (r *RiskManager) checkMarketConditions(market strategy.MarketContext) string {
	if decimal.NewFromFloat(market.Liquidity).LessThan(r.config.MinLiquidity) {
		return "Market liquidity too low"
	}
	
	// Check odds are within acceptable range
	if market.YesPrice < r.config.MinOdds || market.YesPrice > r.config.MaxOdds {
		if market.NoPrice < r.config.MinOdds || market.NoPrice > r.config.MaxOdds {
			return "Odds outside acceptable range"
		}
	}
	
	// Check spread
	if market.OrderBook != nil && market.OrderBook.Spread > r.config.MaxSpread {
		return "Spread too wide"
	}
	
	return ""
}

// checkTiming validates timing is suitable
func (r *RiskManager) checkTiming(market strategy.MarketContext) string {
	if !market.WindowEnd.IsZero() {
		timeToExpiry := time.Until(market.WindowEnd)
		if timeToExpiry < r.config.MinTimeToExpiry {
			return "Too close to window expiry"
		}
	}
	
	return ""
}

// checkCooldown validates enough time has passed since last trade
func (r *RiskManager) checkCooldown() string {
	if !r.state.LastTradeTime.IsZero() {
		if time.Since(r.state.LastTradeTime) < r.config.TradeCooldown {
			return "Trade cooldown active"
		}
	}
	return ""
}

// checkWindowPosition ensures we only trade once per window
func (r *RiskManager) checkWindowPosition(market strategy.MarketContext) string {
	if market.WindowID == "" {
		return "" // No window ID = allow (legacy mode)
	}
	
	if _, exists := r.state.TradedWindows[market.WindowID]; exists {
		return "Already traded this window"
	}
	
	return ""
}

// RecordWindowTrade marks a window as traded
func (r *RiskManager) RecordWindowTrade(windowID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	
	if windowID != "" {
		r.state.TradedWindows[windowID] = time.Now()
	}
}

// calculatePositionSize determines optimal bet size
func (r *RiskManager) calculatePositionSize(signal strategy.Signal, bankroll decimal.Decimal) decimal.Decimal {
	// Base size from max position %
	baseSize := bankroll.Mul(decimal.NewFromFloat(r.config.MaxPositionPct))
	
	// Scale by confidence (0.6 confidence = 60% of base size)
	confidenceMultiplier := decimal.NewFromFloat(signal.Confidence)
	size := baseSize.Mul(confidenceMultiplier)
	
	// Reduce size on losing streaks
	if r.state.ConsecutiveLosses >= 3 {
		size = size.Mul(decimal.NewFromFloat(0.5)) // Half size after 3 losses
	}
	
	// Cap at max bet size
	if size.GreaterThan(r.config.MaxBetSize) {
		size = r.config.MaxBetSize
	}
	
	return size.Round(2)
}

// strengthMeetsMin checks if strength meets minimum requirement
func (r *RiskManager) strengthMeetsMin(strength strategy.Strength) bool {
	strengthRank := map[strategy.Strength]int{
		strategy.StrengthWeak:     1,
		strategy.StrengthModerate: 2,
		strategy.StrengthStrong:   3,
	}
	return strengthRank[strength] >= strengthRank[r.config.MinStrength]
}

// checkDayReset resets daily state if it's a new day
func (r *RiskManager) checkDayReset() {
	today := time.Now().Truncate(24 * time.Hour)
	if today.After(r.state.TradingDay) {
		log.Info().Msg("📅 New trading day - resetting limits")
		r.state.DailyPnL = decimal.Zero
		r.state.DailyTrades = 0
		r.state.DailyExposure = decimal.Zero
		r.state.TradingDay = today
		r.state.TradedWindows = make(map[string]time.Time) // Clear window tracking
	}
}

// RecordTrade updates state after a trade is placed
func (r *RiskManager) RecordTrade(size decimal.Decimal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	
	r.state.DailyTrades++
	r.state.DailyExposure = r.state.DailyExposure.Add(size)
	r.state.LastTradeTime = time.Now()
}

// RecordResult updates state after a trade resolves
func (r *RiskManager) RecordResult(pnl decimal.Decimal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	
	r.state.DailyPnL = r.state.DailyPnL.Add(pnl)
	
	if pnl.IsPositive() {
		r.state.WinStreak++
		r.state.LossStreak = 0
		r.state.ConsecutiveLosses = 0
	} else {
		r.state.WinStreak = 0
		r.state.LossStreak++
		r.state.ConsecutiveLosses++
	}
}

// GetState returns current risk state (read-only snapshot)
func (r *RiskManager) GetState() RiskState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return *r.state
}

// Helper
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
