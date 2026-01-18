package strategy

import (
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/feeds"
)

// ═══════════════════════════════════════════════════════════════════════════════
// SNIPER V3 STRATEGY - Last Minute Confirmed Signals
// ═══════════════════════════════════════════════════════════════════════════════
//
// CORE LOGIC:
//   1. Wait until LAST 15-60 seconds of a 15min window
//   2. Check if BTC/ETH/SOL moved 0.1%+ from "price to beat"
//   3. If YES → outcome is ALMOST CERTAIN (no time to reverse)
//   4. Buy the winning side at 88-93¢
//   5. Exit at 99¢ (TP) or 70¢ (SL for rare reversal)
//
// WHY THIS WORKS:
//   - At 30 sec remaining with 0.1%+ move, reversal is nearly impossible
//   - We're buying CONFIRMED winners, not guessing
//   - Even if market is at 90¢, we know it's going to $1.00
//
// ═══════════════════════════════════════════════════════════════════════════════

// SniperV3 implements the last-minute sniper strategy
type SniperV3 struct {
	mu      sync.RWMutex
	enabled bool

	// Configuration
	minTimeSec float64         // Min time remaining (default 15)
	maxTimeSec float64         // Max time remaining (default 60)
	minOdds    decimal.Decimal // Min entry price (88¢)
	maxOdds    decimal.Decimal // Max entry price (93¢)
	takeProfit decimal.Decimal // TP price (99¢)
	stopLoss   decimal.Decimal // SL price (70¢)

	// Per-asset price move thresholds
	btcMinMove decimal.Decimal // 0.10% for BTC
	ethMinMove decimal.Decimal // 0.10% for ETH
	solMinMove decimal.Decimal // 0.15% for SOL

	// Momentum confirmation
	momentumLookback time.Duration
	minVelocity      decimal.Decimal

	// Detection interval
	detectionIntervalMs int

	// Data sources
	binanceFeed   *feeds.BinanceFeed
	windowScanner *feeds.WindowScanner

	// State tracking
	lastSignal map[string]time.Time // Per-window cooldown
	cooldown   time.Duration
	maxTrades  int

	// Momentum tracking per asset
	priceHistory map[string][]pricePoint // symbol -> price history

	// Stats
	signalCount   int
	opportunityCt int
}

// pricePoint tracks a price at a point in time
type pricePoint struct {
	price     decimal.Decimal
	timestamp time.Time
}

// NewSniperV3 creates a new sniper strategy
func NewSniperV3(binanceFeed *feeds.BinanceFeed, windowScanner *feeds.WindowScanner) *SniperV3 {
	// Load config from environment
	minTimeSec := envFloat("SNIPER_MIN_TIME_SEC", 15)
	maxTimeSec := envFloat("SNIPER_MAX_TIME_SEC", 60)
	minOdds := envDecimal("SNIPER_MIN_ODDS", 0.88)
	maxOdds := envDecimal("SNIPER_MAX_ODDS", 0.93)
	takeProfit := envDecimal("SNIPER_TP", 0.99)
	stopLoss := envDecimal("SNIPER_SL", 0.70)

	btcMinMove := envDecimal("SNIPER_BTC_MIN_MOVE_PCT", 0.10)
	ethMinMove := envDecimal("SNIPER_ETH_MIN_MOVE_PCT", 0.10)
	solMinMove := envDecimal("SNIPER_SOL_MIN_MOVE_PCT", 0.15)

	momentumLookbackSec := envInt("SNIPER_MOMENTUM_LOOKBACK_SEC", 5)
	cooldownSec := envInt("SNIPER_COOLDOWN_SEC", 10)
	maxTrades := envInt("SNIPER_MAX_TRADES_PER_WINDOW", 1)
	detectionMs := envInt("SNIPER_DETECTION_INTERVAL_MS", 200)

	strat := &SniperV3{
		enabled:             true,
		minTimeSec:          minTimeSec,
		maxTimeSec:          maxTimeSec,
		minOdds:             minOdds,
		maxOdds:             maxOdds,
		takeProfit:          takeProfit,
		stopLoss:            stopLoss,
		btcMinMove:          btcMinMove,
		ethMinMove:          ethMinMove,
		solMinMove:          solMinMove,
		momentumLookback:    time.Duration(momentumLookbackSec) * time.Second,
		minVelocity:         decimal.Zero,
		detectionIntervalMs: detectionMs,
		binanceFeed:         binanceFeed,
		windowScanner:       windowScanner,
		lastSignal:          make(map[string]time.Time),
		cooldown:            time.Duration(cooldownSec) * time.Second,
		maxTrades:           maxTrades,
		priceHistory:        make(map[string][]pricePoint),
	}

	log.Info().
		Float64("min_time_sec", minTimeSec).
		Float64("max_time_sec", maxTimeSec).
		Str("entry_range", minOdds.StringFixed(2)+"-"+maxOdds.StringFixed(2)).
		Str("tp", takeProfit.StringFixed(2)).
		Str("sl", stopLoss.StringFixed(2)).
		Int("detection_ms", detectionMs).
		Msg("🎯 Sniper V3 strategy initialized")

	return strat
}

// Name returns the strategy name
func (s *SniperV3) Name() string {
	return "SniperV3"
}

// Enabled returns if strategy is active
func (s *SniperV3) Enabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

// Config returns configuration
func (s *SniperV3) Config() map[string]interface{} {
	return map[string]interface{}{
		"min_time_sec":   s.minTimeSec,
		"max_time_sec":   s.maxTimeSec,
		"min_odds":       s.minOdds.String(),
		"max_odds":       s.maxOdds.String(),
		"take_profit":    s.takeProfit.String(),
		"stop_loss":      s.stopLoss.String(),
		"btc_min_move":   s.btcMinMove.String(),
		"eth_min_move":   s.ethMinMove.String(),
		"sol_min_move":   s.solMinMove.String(),
		"detection_ms":   s.detectionIntervalMs,
	}
}

// OnTick processes a price tick and returns a signal or nil
// This is called by the router for each Polymarket tick
func (s *SniperV3) OnTick(tick feeds.Tick) *Signal {
	// SniperV3 doesn't use the tick-based flow
	// It runs its own 200ms loop scanning windows
	// Return nil here - signals come from RunLoop
	return nil
}

// RunLoop starts the main 200ms detection loop (called by engine)
func (s *SniperV3) RunLoop(signalCh chan<- *Signal) {
	interval := time.Duration(s.detectionIntervalMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	log.Info().Dur("interval", interval).Msg("🔄 Sniper detection loop started")

	for {
		select {
		case <-ticker.C:
			if signal := s.scan(); signal != nil {
				signalCh <- signal
			}
		}
	}
}

// scan checks all windows for sniper opportunities
func (s *SniperV3) scan() *Signal {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.enabled {
		return nil
	}

	// Get windows in sniper zone (15-60 sec remaining)
	windows := s.windowScanner.GetSniperReadyWindows(s.minTimeSec, s.maxTimeSec)

	for _, window := range windows {
		signal := s.evaluateWindow(window)
		if signal != nil {
			return signal
		}
	}

	return nil
}

// evaluateWindow checks if a window has a sniper opportunity
func (s *SniperV3) evaluateWindow(window *feeds.Window) *Signal {
	// Check cooldown
	if lastSig, ok := s.lastSignal[window.ID]; ok {
		if time.Since(lastSig) < s.cooldown {
			return nil
		}
	}

	// Get current Binance price
	symbol := window.Asset + "USDT"
	currentPrice := s.binanceFeed.GetPrice(symbol)
	if currentPrice.IsZero() {
		return nil
	}

	// Update price history for momentum
	s.updatePriceHistory(symbol, currentPrice)

	// Calculate price movement from "price to beat"
	priceToBeat := window.PriceToBeat
	if priceToBeat.IsZero() {
		return nil
	}

	// Calculate % move
	priceMove := currentPrice.Sub(priceToBeat).Div(priceToBeat).Mul(decimal.NewFromInt(100))

	// Get minimum move threshold for this asset
	minMove := s.getMinMoveForAsset(window.Asset)

	// Determine direction
	// Positive move = price is ABOVE target = YES wins
	// Negative move = price is BELOW target = NO wins
	absPriceMove := priceMove.Abs()
	isAbove := priceMove.GreaterThan(decimal.Zero)

	// Check if move is significant enough
	if absPriceMove.LessThan(minMove) {
		return nil
	}

	s.opportunityCt++

	// Determine which side to buy
	var buyTokenID, side string
	var currentOdds decimal.Decimal

	if isAbove {
		// Price is ABOVE target → YES will win
		buyTokenID = window.YesTokenID
		side = "YES"
		currentOdds = window.YesPrice
	} else {
		// Price is BELOW target → NO will win
		buyTokenID = window.NoTokenID
		side = "NO"
		currentOdds = window.NoPrice
	}

	// Check if odds are in entry zone (88-93¢)
	if currentOdds.LessThan(s.minOdds) || currentOdds.GreaterThan(s.maxOdds) {
		log.Debug().
			Str("asset", window.Asset).
			Str("side", side).
			Str("odds", currentOdds.StringFixed(2)).
			Str("zone", s.minOdds.StringFixed(2)+"-"+s.maxOdds.StringFixed(2)).
			Msg("Skipped: odds outside entry zone")
		return nil
	}

	// Momentum confirmation - make sure price isn't reversing
	if !s.confirmMomentum(symbol, isAbove) {
		log.Debug().
			Str("asset", window.Asset).
			Str("direction", side).
			Msg("Skipped: momentum reversing")
		return nil
	}

	// === ALL CHECKS PASSED - GENERATE SIGNAL ===

	s.signalCount++
	s.lastSignal[window.ID] = time.Now()

	timeLeft := window.TimeRemainingSeconds()

	signal := NewSignal().
		Market(window.ID).
		Asset(window.Asset).
		TokenID(buyTokenID).
		Side(side).
		Entry(currentOdds).
		TakeProfit(s.takeProfit).
		StopLoss(s.stopLoss).
		Confidence(s.calculateConfidence(absPriceMove, timeLeft)).
		Reason(formatReason(window.Asset, priceMove, timeLeft, side)).
		Strategy(s.Name()).
		Build()

	log.Info().
		Str("asset", window.Asset).
		Str("side", side).
		Str("odds", currentOdds.StringFixed(2)).
		Str("move", priceMove.StringFixed(3)+"%").
		Float64("time_left_sec", timeLeft).
		Int("signal_#", s.signalCount).
		Msg("🎯 SNIPER SIGNAL")

	return signal
}

// getMinMoveForAsset returns the minimum price move threshold for an asset
func (s *SniperV3) getMinMoveForAsset(asset string) decimal.Decimal {
	switch asset {
	case "BTC":
		return s.btcMinMove
	case "ETH":
		return s.ethMinMove
	case "SOL":
		return s.solMinMove
	default:
		return s.btcMinMove // Default to BTC threshold
	}
}

// updatePriceHistory adds a price to the history for momentum tracking
func (s *SniperV3) updatePriceHistory(symbol string, price decimal.Decimal) {
	point := pricePoint{
		price:     price,
		timestamp: time.Now(),
	}

	history := s.priceHistory[symbol]
	history = append(history, point)

	// Keep only recent history (last 30 seconds)
	cutoff := time.Now().Add(-30 * time.Second)
	filtered := make([]pricePoint, 0, len(history))
	for _, p := range history {
		if p.timestamp.After(cutoff) {
			filtered = append(filtered, p)
		}
	}

	s.priceHistory[symbol] = filtered
}

// confirmMomentum checks if price momentum matches expected direction
func (s *SniperV3) confirmMomentum(symbol string, expectUp bool) bool {
	history := s.priceHistory[symbol]
	if len(history) < 2 {
		return true // Not enough data, allow entry
	}

	// Look at last N seconds of price movement
	cutoff := time.Now().Add(-s.momentumLookback)
	var recentPrices []decimal.Decimal

	for _, p := range history {
		if p.timestamp.After(cutoff) {
			recentPrices = append(recentPrices, p.price)
		}
	}

	if len(recentPrices) < 2 {
		return true
	}

	// Calculate velocity (price change over time)
	first := recentPrices[0]
	last := recentPrices[len(recentPrices)-1]
	velocity := last.Sub(first)

	// If we expect UP, velocity should be positive (or at least not very negative)
	// If we expect DOWN, velocity should be negative (or at least not very positive)
	if expectUp {
		// Allow small negative velocity, but not big reversals
		return velocity.GreaterThan(s.minVelocity.Neg())
	} else {
		// For DOWN, velocity should be negative or small positive
		return velocity.LessThan(s.minVelocity)
	}
}

// calculateConfidence returns confidence based on move size and time remaining
func (s *SniperV3) calculateConfidence(absMove decimal.Decimal, timeLeftSec float64) decimal.Decimal {
	// Base confidence from move size (0.1% = 70%, 0.2% = 80%, 0.3% = 90%)
	moveConfidence := absMove.Mul(decimal.NewFromFloat(100)).InexactFloat64() / 0.3 * 0.2 + 0.7
	if moveConfidence > 0.95 {
		moveConfidence = 0.95
	}

	// Time bonus (less time = higher confidence)
	// 60 sec = no bonus, 30 sec = +5%, 15 sec = +10%
	timeBonus := (60 - timeLeftSec) / 60 * 0.10
	if timeBonus < 0 {
		timeBonus = 0
	}

	confidence := moveConfidence + timeBonus
	if confidence > 0.99 {
		confidence = 0.99
	}

	return decimal.NewFromFloat(confidence)
}

// formatReason creates a human-readable reason for the signal
func formatReason(asset string, move decimal.Decimal, timeLeft float64, side string) string {
	direction := "above"
	if move.IsNegative() {
		direction = "below"
	}
	return asset + " " + move.Abs().StringFixed(3) + "% " + direction +
		" target, " + strconv.FormatFloat(timeLeft, 'f', 0, 64) + "s left → " + side
}

// ═══════════════════════════════════════════════════════════════════════════════
// HELPER FUNCTIONS
// ═══════════════════════════════════════════════════════════════════════════════

func envFloat(key string, fallback float64) float64 {
	if val := os.Getenv(key); val != "" {
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return f
		}
	}
	return fallback
}
