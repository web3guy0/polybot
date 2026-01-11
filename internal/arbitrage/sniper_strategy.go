package arbitrage

import (
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/internal/dashboard"
	"github.com/web3guy0/polybot/internal/database"
	"github.com/web3guy0/polybot/internal/polymarket"
)

// ═══════════════════════════════════════════════════════════════════════════════
// LAST MINUTE SNIPER STRATEGY
// ═══════════════════════════════════════════════════════════════════════════════
//
// Strategy: Buy the "almost certain" winner in the last 2-3 minutes
//
// Logic:
// 1. Wait until last 3 minutes of window
// 2. Check if price moved significantly (>0.2%) from price-to-beat
// 3. If yes, the direction is almost certain
// 4. Buy the winning side at 85-92¢ (cheap compared to $1 resolution)
// 5. Quick flip at 95¢+ OR hold to resolution
// 6. Tight stop-loss at 75¢ if sudden reversal
//
// Math:
// - Buy at 90¢, sell at 95¢ = 5¢ profit (5.5% return)
// - Buy at 90¢, resolution $1 = 10¢ profit (11% return)
// - Buy at 90¢, stop at 75¢ = -15¢ loss
// - Need 75%+ win rate to be profitable
//
// ═══════════════════════════════════════════════════════════════════════════════

// SniperConfig holds configuration for the sniper strategy
type SniperConfig struct {
	// Entry conditions
	MinTimeRemainingMin float64         // Minimum time left (e.g., 1 min)
	MaxTimeRemainingMin float64         // Maximum time left (e.g., 3 min)
	MinPriceMovePct     decimal.Decimal // Min price move to confirm direction (e.g., 0.002 = 0.2%)
	MinOddsEntry        decimal.Decimal // Min odds to buy (e.g., 0.85)
	MaxOddsEntry        decimal.Decimal // Max odds to buy (e.g., 0.92)

	// Exit conditions
	QuickFlipTarget decimal.Decimal // Sell at this price (e.g., 0.95)
	StopLoss        decimal.Decimal // Cut loss at this price (e.g., 0.75)

	// Position sizing
	PositionSizeUSD decimal.Decimal // USD per trade

	// Safety
	MaxTradesPerWindow int           // Max 1 trade per window
	CooldownDuration   time.Duration // Cooldown after trade
}

// SniperPosition represents an active sniper position
type SniperPosition struct {
	TradeID      string
	Asset        string
	Side         string // "UP" or "DOWN"
	TokenID      string
	ConditionID  string
	WindowID     string
	EntryPrice   decimal.Decimal
	Size         int64 // Number of shares
	EntryTime    time.Time
	StopLoss     decimal.Decimal
	Target       decimal.Decimal
	WindowEnd    time.Time
	PriceMovePct decimal.Decimal // Price move % at entry (to decide hold vs flip)
}

// SniperStrategy implements the last-minute sniper trading strategy
type SniperStrategy struct {
	mu sync.RWMutex

	// Components
	windowScanner *polymarket.WindowScanner
	clobClient    *CLOBClient
	engine        *Engine
	db            *database.Database
	notifier      TradeNotifier
	dash          *dashboard.ResponsiveDash

	// Configuration
	config SniperConfig

	// State
	positions        map[string]*SniperPosition // windowID -> position
	windowTradeCount map[string]int             // windowID -> trade count
	lastTradeTime    time.Time
	running          bool
	stopCh           chan struct{}

	// Stats
	totalTrades   int
	winningTrades int
	totalProfit   decimal.Decimal
	cachedBalance decimal.Decimal
}

// NewSniperStrategy creates a new last-minute sniper strategy
func NewSniperStrategy(
	scanner *polymarket.WindowScanner,
	clobClient *CLOBClient,
	positionSize decimal.Decimal,
) *SniperStrategy {
	return &SniperStrategy{
		windowScanner:    scanner,
		clobClient:       clobClient,
		positions:        make(map[string]*SniperPosition),
		windowTradeCount: make(map[string]int),
		totalProfit:      decimal.Zero,
		stopCh:           make(chan struct{}),
		config: SniperConfig{
			MinTimeRemainingMin: 1.0,                             // At least 1 min left
			MaxTimeRemainingMin: 3.0,                             // Max 3 min left (last 3 minutes)
			MinPriceMovePct:     decimal.NewFromFloat(0.002),     // 0.2% price move minimum
			MinOddsEntry:        decimal.NewFromFloat(0.85),      // Buy at 85¢ minimum
			MaxOddsEntry:        decimal.NewFromFloat(0.92),      // Buy at 92¢ maximum
			QuickFlipTarget:     decimal.NewFromFloat(0.95),      // Sell at 95¢
			StopLoss:            decimal.NewFromFloat(0.75),      // Stop at 75¢
			PositionSizeUSD:     positionSize,                    // From config
			MaxTradesPerWindow:  1,                               // Only 1 trade per window
			CooldownDuration:    30 * time.Second,                // 30s cooldown
		},
	}
}

// SetEngine sets the arbitrage engine reference
func (s *SniperStrategy) SetEngine(engine *Engine) {
	s.engine = engine
}

// SetDatabase sets the database for trade logging
func (s *SniperStrategy) SetDatabase(db *database.Database) {
	s.db = db
	log.Info().Msg("📊 [SNIPER] Database connected")
}

// SetNotifier sets the notifier for alerts
func (s *SniperStrategy) SetNotifier(n TradeNotifier) {
	s.notifier = n
	log.Info().Msg("📱 [SNIPER] Notifier connected")
}

// SetConfig updates the sniper configuration from external config
func (s *SniperStrategy) SetConfig(
	minTimeMin, maxTimeMin float64,
	minPriceMove, minOdds, maxOdds, target, stopLoss decimal.Decimal,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	s.config.MinTimeRemainingMin = minTimeMin
	s.config.MaxTimeRemainingMin = maxTimeMin
	s.config.MinPriceMovePct = minPriceMove
	s.config.MinOddsEntry = minOdds
	s.config.MaxOddsEntry = maxOdds
	s.config.QuickFlipTarget = target
	s.config.StopLoss = stopLoss
	
	log.Info().
		Float64("min_time_min", minTimeMin).
		Float64("max_time_min", maxTimeMin).
		Str("min_price_move", minPriceMove.Mul(decimal.NewFromInt(100)).StringFixed(1)+"%").
		Str("min_odds", minOdds.StringFixed(2)).
		Str("max_odds", maxOdds.StringFixed(2)).
		Str("target", target.StringFixed(2)).
		Str("stop", stopLoss.StringFixed(2)).
		Msg("🎯 [SNIPER] Config updated from env")
}

// SetDashboard sets the dashboard
func (s *SniperStrategy) SetDashboard(d *dashboard.ResponsiveDash) {
	s.dash = d
	log.Info().Msg("📺 [SNIPER] Dashboard connected")
}

// Start begins the sniper strategy loop
func (s *SniperStrategy) Start() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()

	go s.mainLoop()
	go s.positionMonitorLoop()

	log.Info().
		Float64("min_time_min", s.config.MinTimeRemainingMin).
		Float64("max_time_min", s.config.MaxTimeRemainingMin).
		Str("min_odds", s.config.MinOddsEntry.String()).
		Str("max_odds", s.config.MaxOddsEntry.String()).
		Str("target", s.config.QuickFlipTarget.String()).
		Str("stop", s.config.StopLoss.String()).
		Msg("🎯 [SNIPER] Last Minute Sniper started!")
}

// Stop stops the sniper strategy
func (s *SniperStrategy) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return
	}

	s.running = false
	close(s.stopCh)
	log.Info().Msg("🛑 [SNIPER] Strategy stopped")
}

// mainLoop is the main scanning loop
func (s *SniperStrategy) mainLoop() {
	ticker := time.NewTicker(500 * time.Millisecond) // Check every 500ms
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.scanForOpportunities()
		}
	}
}

// scanForOpportunities looks for sniper opportunities
func (s *SniperStrategy) scanForOpportunities() {
	windows := s.windowScanner.GetActiveWindows()

	for i := range windows {
		s.evaluateWindow(&windows[i])
	}

	// Update dashboard stats
	s.dashUpdateStats()
}

// evaluateWindow checks if a window is ready for sniping
func (s *SniperStrategy) evaluateWindow(w *polymarket.PredictionWindow) {
	asset := w.Asset
	if asset == "" {
		asset = "BTC"
	}

	// Skip if we already have a position in this window
	s.mu.RLock()
	if _, exists := s.positions[w.ID]; exists {
		s.mu.RUnlock()
		return
	}

	// Check trade count for this window
	if s.windowTradeCount[w.ID] >= s.config.MaxTradesPerWindow {
		s.mu.RUnlock()
		return
	}

	// Check cooldown
	if time.Since(s.lastTradeTime) < s.config.CooldownDuration {
		s.mu.RUnlock()
		return
	}
	s.mu.RUnlock()

	// Calculate time remaining
	timeRemaining := time.Until(w.EndDate)
	timeRemainingMin := timeRemaining.Minutes()

	// Check if we're in the sniper window (last 1-3 minutes)
	if timeRemainingMin > s.config.MaxTimeRemainingMin {
		// Too early, wait
		return
	}
	if timeRemainingMin < s.config.MinTimeRemainingMin {
		// Too late, skip
		return
	}

	// Get price data
	var priceToBeat, currentPrice decimal.Decimal

	if !w.PriceToBeat.IsZero() {
		priceToBeat = w.PriceToBeat
	} else if s.engine != nil {
		if state := s.engine.GetWindowState(w.ID); state != nil {
			priceToBeat = state.StartPrice
		}
	}

	if s.engine != nil {
		currentPrice = s.engine.GetCurrentPrice()
	}

	if priceToBeat.IsZero() || currentPrice.IsZero() {
		return
	}

	// Calculate price move
	priceMove := currentPrice.Sub(priceToBeat)
	priceMovePct := priceMove.Div(priceToBeat).Abs()

	// Check if price moved enough
	if priceMovePct.LessThan(s.config.MinPriceMovePct) {
		log.Debug().
			Str("asset", asset).
			Str("move_pct", priceMovePct.Mul(decimal.NewFromInt(100)).StringFixed(3)+"%").
			Str("required", s.config.MinPriceMovePct.Mul(decimal.NewFromInt(100)).StringFixed(2)+"%").
			Msg("🎯 [SNIPER] Price move too small - waiting")
		return
	}

	// Determine direction
	priceWentUp := currentPrice.GreaterThan(priceToBeat)

	// Get current odds
	upOdds := w.YesPrice
	downOdds := w.NoPrice

	// Update dashboard with market data
	if s.dash != nil {
		s.dash.UpdateMarket(asset, currentPrice, priceToBeat, upOdds, downOdds)
	}

	// Find the winning side
	var winningSide string
	var winningOdds decimal.Decimal
	var tokenID string

	if priceWentUp {
		// Price went UP, so UP should win
		winningSide = "UP"
		winningOdds = upOdds
		tokenID = w.YesTokenID
	} else {
		// Price went DOWN, so DOWN should win
		winningSide = "DOWN"
		winningOdds = downOdds
		tokenID = w.NoTokenID
	}

	// Check if odds are in our sniper range (85-92¢)
	if winningOdds.LessThan(s.config.MinOddsEntry) {
		log.Debug().
			Str("asset", asset).
			Str("side", winningSide).
			Str("odds", winningOdds.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Str("min", s.config.MinOddsEntry.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Msg("🎯 [SNIPER] Odds too low - waiting for 85¢+")
		return
	}

	if winningOdds.GreaterThan(s.config.MaxOddsEntry) {
		log.Debug().
			Str("asset", asset).
			Str("side", winningSide).
			Str("odds", winningOdds.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Str("max", s.config.MaxOddsEntry.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Msg("🎯 [SNIPER] Odds too high - not worth the risk")
		return
	}

	// 🎯 SNIPER OPPORTUNITY FOUND!
	log.Info().
		Str("asset", asset).
		Str("side", winningSide).
		Str("odds", winningOdds.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Str("price_move", priceMovePct.Mul(decimal.NewFromInt(100)).StringFixed(3)+"%").
		Float64("time_remaining_min", timeRemainingMin).
		Bool("price_went_up", priceWentUp).
		Msg("🎯🎯🎯 [SNIPER] OPPORTUNITY DETECTED!")

	// Log to dashboard
	if s.dash != nil {
		s.dash.AddSignal(asset, winningSide, "🎯 SNIPE", winningOdds,
			fmt.Sprintf("Last %.1fmin, move %.2f%%", timeRemainingMin, priceMovePct.Mul(decimal.NewFromInt(100)).InexactFloat64()),
			0.90) // High confidence
	}

	// Execute the snipe!
	s.executeSnipe(w, winningSide, winningOdds, tokenID, timeRemaining, priceMovePct)
}

// executeSnipe executes a sniper trade
func (s *SniperStrategy) executeSnipe(w *polymarket.PredictionWindow, side string, odds decimal.Decimal, tokenID string, timeRemaining time.Duration, priceMovePct decimal.Decimal) {
	asset := w.Asset
	if asset == "" {
		asset = "BTC"
	}

	// Calculate position size
	tickSize := decimal.NewFromFloat(0.01)
	roundedPrice := odds.Div(tickSize).Floor().Mul(tickSize)

	size := s.config.PositionSizeUSD.Div(roundedPrice).Floor().IntPart()
	if size < 1 {
		size = 1
	}

	actualCost := roundedPrice.Mul(decimal.NewFromInt(size))
	potentialProfit := s.config.QuickFlipTarget.Sub(roundedPrice).Mul(decimal.NewFromInt(size))
	maxLoss := roundedPrice.Sub(s.config.StopLoss).Mul(decimal.NewFromInt(size))

	log.Info().
		Str("asset", asset).
		Str("side", side).
		Str("entry", roundedPrice.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Str("target", s.config.QuickFlipTarget.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Str("stop", s.config.StopLoss.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Int64("shares", size).
		Str("cost", "$"+actualCost.StringFixed(2)).
		Str("potential_profit", "+$"+potentialProfit.StringFixed(2)).
		Str("max_loss", "-$"+maxLoss.StringFixed(2)).
		Msg("🎯 [SNIPER] EXECUTING SNIPE!")

	// Create position
	pos := &SniperPosition{
		TradeID:      fmt.Sprintf("snipe-%s-%d", asset, time.Now().UnixNano()),
		Asset:        asset,
		Side:         side,
		TokenID:      tokenID,
		ConditionID:  w.ConditionID,
		WindowID:     w.ID,
		EntryPrice:   roundedPrice,
		Size:         size,
		EntryTime:    time.Now(),
		StopLoss:     s.config.StopLoss,
		Target:       s.config.QuickFlipTarget,
		WindowEnd:    w.EndDate,
		PriceMovePct: priceMovePct, // Track price move to decide hold vs flip
	}

	// Place the order
	if s.clobClient == nil {
		log.Warn().Msg("🎯 [SNIPER] No CLOB client - paper trade only")
		s.recordPosition(pos)
		return
	}

	// Use limit order for fill
	slippage := decimal.NewFromFloat(0.02) // 2¢ slippage allowed
	orderPrice := roundedPrice.Add(slippage)

	orderID, err := s.clobClient.PlaceLimitOrder(
		tokenID,
		orderPrice,
		decimal.NewFromInt(size),
		"BUY",
	)

	if err != nil {
		log.Error().Err(err).Str("asset", asset).Msg("🎯 [SNIPER] Order failed!")
		if s.dash != nil {
			s.dash.AddLog(fmt.Sprintf("❌ SNIPE FAILED: %s %s - %v", asset, side, err))
		}
		return
	}

	pos.TradeID = orderID
	s.recordPosition(pos)

	// Notify
	if s.notifier != nil {
		s.notifier.SendTradeAlert(asset, side, roundedPrice, size, "SNIPE BUY", decimal.Zero)
	}

	if s.dash != nil {
		s.dash.AddLog(fmt.Sprintf("🎯 SNIPE: %s %s @ %s¢ x%d", asset, side, roundedPrice.Mul(decimal.NewFromInt(100)).StringFixed(0), size))
		s.dash.UpdatePosition(asset, side, roundedPrice, roundedPrice, size, "SNIPING")
	}

	log.Info().
		Str("asset", asset).
		Str("side", side).
		Str("order_id", orderID).
		Msg("🎯 [SNIPER] Order placed successfully!")
}

// recordPosition records a new position
func (s *SniperStrategy) recordPosition(pos *SniperPosition) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.positions[pos.WindowID] = pos
	s.windowTradeCount[pos.WindowID]++
	s.lastTradeTime = time.Now()
	s.totalTrades++
}

// positionMonitorLoop monitors positions for exit signals
func (s *SniperStrategy) positionMonitorLoop() {
	ticker := time.NewTicker(200 * time.Millisecond) // Check every 200ms
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.checkPositions()
		}
	}
}

// checkPositions monitors all open positions
func (s *SniperStrategy) checkPositions() {
	s.mu.RLock()
	positions := make([]*SniperPosition, 0, len(s.positions))
	for _, pos := range s.positions {
		positions = append(positions, pos)
	}
	s.mu.RUnlock()

	for _, pos := range positions {
		s.checkPosition(pos)
	}
}

// checkPosition checks a single position for exit
func (s *SniperStrategy) checkPosition(pos *SniperPosition) {
	// Get current market price for this position
	windows := s.windowScanner.GetActiveWindows()
	var currentOdds decimal.Decimal

	for _, w := range windows {
		if w.ID == pos.WindowID {
			if pos.Side == "UP" {
				currentOdds = w.YesPrice
			} else {
				currentOdds = w.NoPrice
			}
			break
		}
	}

	if currentOdds.IsZero() {
		// Window might have ended
		if time.Now().After(pos.WindowEnd) {
			s.handleResolution(pos)
		}
		return
	}

	// Update dashboard
	if s.dash != nil {
		pnl := currentOdds.Sub(pos.EntryPrice).Mul(decimal.NewFromInt(pos.Size))
		s.dash.UpdatePosition(pos.Asset, pos.Side, pos.EntryPrice, currentOdds, pos.Size, "ACTIVE")
		
		// Update ML signal display with current state
		edge := currentOdds.Sub(pos.EntryPrice).Mul(decimal.NewFromInt(100))
		s.dash.UpdateMLSignal(pos.Asset, pos.Side, currentOdds, 0.9, edge.StringFixed(0)+"¢", pnl.StringFixed(2), "SNIPING")
	}

	// Check for quick flip target (95¢+)
	// BUT if price moved 0.1%+ at entry, HOLD to resolution for $1 payout
	holdThreshold := decimal.NewFromFloat(0.001) // 0.1%
	
	if currentOdds.GreaterThanOrEqual(s.config.QuickFlipTarget) {
		if pos.PriceMovePct.GreaterThanOrEqual(holdThreshold) {
			// Strong move - hold to resolution for max profit!
			log.Info().
				Str("asset", pos.Asset).
				Str("side", pos.Side).
				Str("current", currentOdds.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
				Str("price_move", pos.PriceMovePct.Mul(decimal.NewFromInt(100)).StringFixed(2)+"%").
				Msg("🎯 [SNIPER] STRONG MOVE (0.1%+) - HOLDING TO RESOLUTION for $1!")
			return // Don't quick flip, hold for resolution
		}
		
		// Weak move - quick flip at 95¢
		log.Info().
			Str("asset", pos.Asset).
			Str("side", pos.Side).
			Str("current", currentOdds.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Str("target", s.config.QuickFlipTarget.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Str("price_move", pos.PriceMovePct.Mul(decimal.NewFromInt(100)).StringFixed(2)+"%").
			Msg("🎯 [SNIPER] TARGET HIT! Quick flipping (move < 0.1%)...")
		
		s.exitPosition(pos, currentOdds, "TARGET")
		return
	}

	// Check for stop loss (75¢)
	if currentOdds.LessThanOrEqual(s.config.StopLoss) {
		log.Warn().
			Str("asset", pos.Asset).
			Str("side", pos.Side).
			Str("current", currentOdds.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Str("stop", s.config.StopLoss.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Msg("🛑 [SNIPER] STOP LOSS HIT! Cutting losses...")
		
		s.exitPosition(pos, currentOdds, "STOP_LOSS")
		return
	}

	// Check if window is about to end (last 30 seconds) - hold to resolution
	if time.Until(pos.WindowEnd) < 30*time.Second {
		log.Info().
			Str("asset", pos.Asset).
			Str("side", pos.Side).
			Str("time_left", time.Until(pos.WindowEnd).String()).
			Msg("🎯 [SNIPER] Holding to resolution - too late to exit")
	}
}

// exitPosition exits a position at market
func (s *SniperStrategy) exitPosition(pos *SniperPosition, exitPrice decimal.Decimal, reason string) {
	pnl := exitPrice.Sub(pos.EntryPrice).Mul(decimal.NewFromInt(pos.Size))

	log.Info().
		Str("asset", pos.Asset).
		Str("side", pos.Side).
		Str("entry", pos.EntryPrice.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Str("exit", exitPrice.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Str("pnl", pnl.StringFixed(2)).
		Str("reason", reason).
		Msg("🎯 [SNIPER] Exiting position")

	// Place sell order
	if s.clobClient != nil {
		slippage := decimal.NewFromFloat(0.03) // 3¢ slippage for exit
		sellPrice := exitPrice.Sub(slippage)
		if sellPrice.LessThan(decimal.NewFromFloat(0.01)) {
			sellPrice = decimal.NewFromFloat(0.01)
		}

		_, err := s.clobClient.PlaceLimitOrder(
			pos.TokenID,
			sellPrice,
			decimal.NewFromInt(pos.Size),
			"SELL",
		)

		if err != nil {
			log.Error().Err(err).Str("asset", pos.Asset).Msg("🎯 [SNIPER] Sell order failed!")
			// Don't remove position, try again
			return
		}
	}

	// Update stats
	s.mu.Lock()
	s.totalProfit = s.totalProfit.Add(pnl)
	if pnl.GreaterThan(decimal.Zero) {
		s.winningTrades++
	}
	delete(s.positions, pos.WindowID)
	s.mu.Unlock()

	// Update dashboard
	if s.dash != nil {
		result := "✅"
		if pnl.LessThan(decimal.Zero) {
			result = "❌"
		}
		s.dash.AddLog(fmt.Sprintf("%s SNIPE EXIT: %s %s @ %s¢ P&L: %s", 
			result, pos.Asset, pos.Side, 
			exitPrice.Mul(decimal.NewFromInt(100)).StringFixed(0),
			pnl.StringFixed(2)))
		s.dash.RemovePosition(pos.Asset)
	}

	// Notify
	if s.notifier != nil {
		s.notifier.SendTradeAlert(pos.Asset, pos.Side, exitPrice, pos.Size, "SNIPE SELL", pnl)
	}
}

// handleResolution handles position at window end
func (s *SniperStrategy) handleResolution(pos *SniperPosition) {
	// Position held to resolution
	// We'll assume we won since we bet with the price direction
	pnl := decimal.NewFromInt(1).Sub(pos.EntryPrice).Mul(decimal.NewFromInt(pos.Size))

	log.Info().
		Str("asset", pos.Asset).
		Str("side", pos.Side).
		Str("entry", pos.EntryPrice.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Str("resolution", "$1.00").
		Str("pnl", "+$"+pnl.StringFixed(2)).
		Msg("🎯 [SNIPER] Position resolved (assuming win)")

	// Update stats
	s.mu.Lock()
	s.totalProfit = s.totalProfit.Add(pnl)
	s.winningTrades++
	delete(s.positions, pos.WindowID)
	s.mu.Unlock()

	// Update dashboard
	if s.dash != nil {
		s.dash.AddLog(fmt.Sprintf("🏆 SNIPE WON: %s %s → $1.00 P&L: +$%s", 
			pos.Asset, pos.Side, pnl.StringFixed(2)))
		s.dash.RemovePosition(pos.Asset)
	}

	// Notify
	if s.notifier != nil {
		s.notifier.SendTradeAlert(pos.Asset, pos.Side, decimal.NewFromInt(1), pos.Size, "SNIPE WON", pnl)
	}
}

// dashUpdateStats updates dashboard with current stats
func (s *SniperStrategy) dashUpdateStats() {
	if s.dash == nil {
		return
	}

	s.mu.RLock()
	totalTrades := s.totalTrades
	winningTrades := s.winningTrades
	totalProfit := s.totalProfit
	balance := s.cachedBalance
	s.mu.RUnlock()

	// Fetch balance periodically
	if s.clobClient != nil && time.Since(s.lastTradeTime) > 30*time.Second {
		go func() {
			if bal, err := s.clobClient.GetBalance(); err == nil {
				s.mu.Lock()
				s.cachedBalance = bal
				s.mu.Unlock()
			}
		}()
	}

	s.dash.UpdateStats(totalTrades, winningTrades, totalProfit, balance)
}
