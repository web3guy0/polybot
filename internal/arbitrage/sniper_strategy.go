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
	MinPriceMovePct     decimal.Decimal // Min price move % (e.g., 0.05 = 0.05%)
	MinOddsEntry        decimal.Decimal // Min odds to buy (e.g., 0.79)
	MaxOddsEntry        decimal.Decimal // Max odds to buy (e.g., 0.90)

	// Exit conditions
	QuickFlipTarget    decimal.Decimal // Sell at this price (e.g., 0.99)
	StopLoss           decimal.Decimal // Cut loss at this price (e.g., 0.50)
	HoldToResolution   bool            // Hold to resolution instead of quick flip

	// Position sizing (% based)
	PositionSizePct decimal.Decimal // % of balance per trade (e.g., 0.10 = 10%)

	// Safety
	MaxTradesPerWindow int           // Max 1 trade per window
	CooldownDuration   time.Duration // Cooldown after trade
	
	// Per-Asset Price Movement (CRITICAL - varies by volatility)
	// BTC = 0.02% (stable), ETH = 0.04% (medium), SOL = 0.08% (volatile)
	AssetMinPriceMove map[string]decimal.Decimal
}

// GetAssetConfig returns config for specific asset
// Entry odds and stop loss are GLOBAL, only price movement varies per-asset
func (c *SniperConfig) GetAssetConfig(asset string) (minOdds, maxOdds, stopLoss, minPriceMove decimal.Decimal) {
	// Global settings
	minOdds = c.MinOddsEntry
	maxOdds = c.MaxOddsEntry
	stopLoss = c.StopLoss
	
	// Per-asset price movement (CRITICAL - based on volatility)
	// BTC = 0.02% (stable), ETH = 0.04% (medium), SOL = 0.08% (volatile)
	if c.AssetMinPriceMove != nil {
		if v, ok := c.AssetMinPriceMove[asset]; ok {
			minPriceMove = v
		} else {
			minPriceMove = c.MinPriceMovePct
		}
	} else {
		minPriceMove = c.MinPriceMovePct
	}
	
	return
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
	Size         decimal.Decimal // Actual shares filled (decimal for precision)
	EntryTime    time.Time
	StopLoss     decimal.Decimal
	Target       decimal.Decimal
	WindowEnd    time.Time
	PriceMovePct decimal.Decimal // Price move % at entry (to decide hold vs flip)
	HighPrice    decimal.Decimal // Highest price seen (for trailing stop)
	TrailingStop decimal.Decimal // Dynamic trailing stop level
}

// MomentumTracker tracks price velocity for fast detection
type MomentumTracker struct {
	LastPrice     decimal.Decimal
	LastTime      time.Time
	Velocity      decimal.Decimal // Price change per second
	Accelerating  bool            // Is price accelerating in one direction
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

	// Momentum tracking for fast detection
	momentum map[string]*MomentumTracker // asset -> momentum

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
	positionSizePct decimal.Decimal,
) *SniperStrategy {
	return &SniperStrategy{
		windowScanner:    scanner,
		clobClient:       clobClient,
		positions:        make(map[string]*SniperPosition),
		windowTradeCount: make(map[string]int),
		momentum:         make(map[string]*MomentumTracker), // Fast momentum detection
		totalProfit:      decimal.Zero,
		stopCh:           make(chan struct{}),
		config: SniperConfig{
			MinTimeRemainingMin: 1.0,                             // At least 1 min left
			MaxTimeRemainingMin: 3.0,                             // Max 3 min left (last 3 minutes)
			MinPriceMovePct:     decimal.NewFromFloat(0.05),      // 0.05% price move minimum
			MinOddsEntry:        decimal.NewFromFloat(0.79),      // Buy at 79¢ minimum (new default)
			MaxOddsEntry:        decimal.NewFromFloat(0.90),      // Buy at 90¢ maximum (new default)
			QuickFlipTarget:     decimal.NewFromFloat(0.99),      // Sell at 99¢ (near resolution)
			StopLoss:            decimal.NewFromFloat(0.50),      // Stop at 50¢ (wide SL)
			PositionSizePct:     positionSizePct,                 // % of balance per trade
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
	holdToResolution bool,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	s.config.MinTimeRemainingMin = minTimeMin
	s.config.MaxTimeRemainingMin = maxTimeMin
	s.config.MinPriceMovePct = minPriceMove.Div(decimal.NewFromInt(100)) // Convert 0.05 -> 0.0005
	s.config.MinOddsEntry = minOdds
	s.config.MaxOddsEntry = maxOdds
	s.config.QuickFlipTarget = target
	s.config.StopLoss = stopLoss
	s.config.HoldToResolution = holdToResolution
	
	log.Info().
		Float64("min_time_min", minTimeMin).
		Float64("max_time_min", maxTimeMin).
		Str("min_price_move", minPriceMove.Mul(decimal.NewFromInt(100)).StringFixed(1)+"%").
		Str("min_odds", minOdds.StringFixed(2)).
		Str("max_odds", maxOdds.StringFixed(2)).
		Str("target", target.StringFixed(2)).
		Str("stop", stopLoss.StringFixed(2)).
		Bool("hold_to_resolution", holdToResolution).
		Msg("🎯 [SNIPER] Config updated from env")
}

// SetPerAssetPriceMove sets per-asset price movement thresholds (CRITICAL for volatility)
// BTC = 0.02% (stable, small moves are significant)
// ETH = 0.04% (medium volatility)
// SOL = 0.08% (volatile, needs bigger move to filter noise)
func (s *SniperStrategy) SetPerAssetPriceMove(
	btcMinPriceMove, ethMinPriceMove, solMinPriceMove decimal.Decimal,
) {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	// Initialize price move map
	s.config.AssetMinPriceMove = make(map[string]decimal.Decimal)
	
	// Convert from env format (0.02) to decimal (0.0002)
	s.config.AssetMinPriceMove["BTC"] = btcMinPriceMove.Div(decimal.NewFromInt(100))
	s.config.AssetMinPriceMove["ETH"] = ethMinPriceMove.Div(decimal.NewFromInt(100))
	s.config.AssetMinPriceMove["SOL"] = solMinPriceMove.Div(decimal.NewFromInt(100))
	
	log.Info().
		Str("btc", fmt.Sprintf("%.2f%%", btcMinPriceMove.InexactFloat64())).
		Str("eth", fmt.Sprintf("%.2f%%", ethMinPriceMove.InexactFloat64())).
		Str("sol", fmt.Sprintf("%.2f%%", solMinPriceMove.InexactFloat64())).
		Msg("🎯 [SNIPER] Per-asset price thresholds (volatility-based)")
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
	ticker := time.NewTicker(200 * time.Millisecond) // Fast scanning for opportunities
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

// updateMomentum updates momentum tracking for an asset and returns velocity info
// Returns: velocity (price change per second), isAccelerating, isReady
func (s *SniperStrategy) updateMomentum(asset string, currentPrice decimal.Decimal) (velocity decimal.Decimal, accelerating bool, ready bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	tracker, exists := s.momentum[asset]
	if !exists || time.Since(tracker.LastTime) > 30*time.Second {
		// New tracker or stale data - initialize
		s.momentum[asset] = &MomentumTracker{
			LastPrice: currentPrice,
			LastTime:  now,
			Velocity:  decimal.Zero,
		}
		return decimal.Zero, false, false
	}

	// Calculate velocity (price change per second)
	elapsed := now.Sub(tracker.LastTime).Seconds()
	if elapsed < 0.1 {
		// Too soon, use cached values
		return tracker.Velocity, tracker.Accelerating, true
	}

	priceChange := currentPrice.Sub(tracker.LastPrice)
	newVelocity := priceChange.Div(decimal.NewFromFloat(elapsed))

	// Check if accelerating (velocity magnitude increasing)
	oldVelAbs := tracker.Velocity.Abs()
	newVelAbs := newVelocity.Abs()
	isAccelerating := newVelAbs.GreaterThan(oldVelAbs)

	// Update tracker
	tracker.LastPrice = currentPrice
	tracker.LastTime = now
	tracker.Velocity = newVelocity
	tracker.Accelerating = isAccelerating

	return newVelocity, isAccelerating, true
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

	// Always update dashboard with time remaining (even if we're not in sniper window yet)
	if s.dash != nil && timeRemaining > 0 {
		s.dash.UpdateMarketTime(asset, timeRemaining)
	}

	// Check if we're in the sniper window (last 1-3 minutes)
	if timeRemainingMin > s.config.MaxTimeRemainingMin {
		// Too early, wait - but still update market data for display
		s.updateMarketDataOnly(w, asset)
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

	// 🚀 MOMENTUM TRACKING - rocket speed detection
	velocity, accelerating, momentumReady := s.updateMomentum(asset, currentPrice)
	
	// Calculate price move
	priceMove := currentPrice.Sub(priceToBeat)
	priceMovePct := priceMove.Div(priceToBeat).Abs()

	// Get per-asset min price move (BTC/ETH: 0.05%, SOL: 0.10%)
	_, _, _, minPriceMove := s.config.GetAssetConfig(asset)
	
	// 🚀 FAST ENTRY: If momentum is strong and accelerating, reduce threshold
	// This catches last-second price spikes that travel with price-to-beat
	effectiveMinMove := minPriceMove
	highVelocity := velocity.Abs().GreaterThan(decimal.NewFromFloat(0.50)) // >$0.50/sec
	
	if momentumReady && accelerating && highVelocity && timeRemainingMin < 1.5 {
		// Price accelerating in last 90 seconds - use 50% of threshold
		effectiveMinMove = minPriceMove.Div(decimal.NewFromInt(2))
		log.Debug().
			Str("asset", asset).
			Str("velocity", velocity.StringFixed(2)+"$/sec").
			Bool("accelerating", accelerating).
			Str("reduced_threshold", effectiveMinMove.Mul(decimal.NewFromInt(100)).StringFixed(3)+"%").
			Msg("🚀 [SNIPER] MOMENTUM BOOST - reduced threshold")
	}
	
	// Check if price moved enough (per-asset threshold, adjusted for momentum)
	if priceMovePct.LessThan(effectiveMinMove) {
		log.Debug().
			Str("asset", asset).
			Str("move", priceMovePct.Mul(decimal.NewFromInt(100)).StringFixed(3)+"%").
			Str("need", effectiveMinMove.Mul(decimal.NewFromInt(100)).StringFixed(2)+"%").
			Str("velocity", velocity.StringFixed(2)+"$/sec").
			Msg("🎯 [SNIPER] Price move too small")
		return
	}

	// Determine direction
	priceWentUp := currentPrice.GreaterThan(priceToBeat)

	// Get current odds
	upOdds := w.YesPrice
	downOdds := w.NoPrice

	// Update dashboard with market data and time remaining
	if s.dash != nil {
		s.dash.UpdateMarket(asset, currentPrice, priceToBeat, upOdds, downOdds)
		s.dash.UpdateMarketTime(asset, timeRemaining)
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

	// Check if odds are in our sniper range (per-asset or global)
	minOdds, maxOdds, assetStopLoss, _ := s.config.GetAssetConfig(asset)
	
	if winningOdds.LessThan(minOdds) {
		log.Debug().
			Str("asset", asset).
			Str("side", winningSide).
			Str("odds", winningOdds.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Str("min", minOdds.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Msg("🎯 [SNIPER] Odds too low - waiting for entry")
		return
	}

	if winningOdds.GreaterThan(maxOdds) {
		log.Debug().
			Str("asset", asset).
			Str("side", winningSide).
			Str("odds", winningOdds.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Str("max", maxOdds.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Msg("🎯 [SNIPER] Odds too high - not worth the risk")
		return
	}

	// 🎯 SNIPER OPPORTUNITY FOUND!
	log.Info().
		Str("asset", asset).
		Str("side", winningSide).
		Str("odds", winningOdds.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Str("range", fmt.Sprintf("%.0f¢-%.0f¢", minOdds.Mul(decimal.NewFromInt(100)).InexactFloat64(), maxOdds.Mul(decimal.NewFromInt(100)).InexactFloat64())).
		Str("sl", assetStopLoss.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
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

	// Get per-asset stop loss
	_, _, assetStopLoss, _ := s.config.GetAssetConfig(asset)

	// Calculate position size using % of balance
	tickSize := decimal.NewFromFloat(0.01)
	roundedPrice := odds.Div(tickSize).Floor().Mul(tickSize)

	// Query current balance and use % of it
	var positionUSD decimal.Decimal
	if s.clobClient != nil {
		balance, err := s.clobClient.GetBalance()
		if err != nil {
			log.Error().Err(err).Msg("🎯 [SNIPER] Failed to get balance")
			return
		}
		positionUSD = balance.Mul(s.config.PositionSizePct) // e.g., 10% of balance
		log.Info().
			Str("balance", "$"+balance.StringFixed(2)).
			Str("pct", s.config.PositionSizePct.Mul(decimal.NewFromInt(100)).StringFixed(0)+"%").
			Str("position_usd", "$"+positionUSD.StringFixed(2)).
			Msg("🎯 [SNIPER] Position sizing")
	} else {
		// Paper trading fallback
		positionUSD = decimal.NewFromFloat(10.0)
	}

	// Calculate shares - keep 2 decimal places for precision
	size := positionUSD.Div(roundedPrice).Round(2)
	minSize := decimal.NewFromInt(5) // Polymarket minimum
	if size.LessThan(minSize) {
		size = minSize
	}

	actualCost := roundedPrice.Mul(size)
	potentialProfit := s.config.QuickFlipTarget.Sub(roundedPrice).Mul(size)
	maxLoss := roundedPrice.Sub(assetStopLoss).Mul(size)

	log.Info().
		Str("asset", asset).
		Str("side", side).
		Str("entry", roundedPrice.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Str("target", s.config.QuickFlipTarget.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Str("stop", assetStopLoss.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Str("shares", size.StringFixed(2)).
		Str("cost", "$"+actualCost.StringFixed(2)).
		Str("potential_profit", "+$"+potentialProfit.StringFixed(2)).
		Str("max_loss", "-$"+maxLoss.StringFixed(2)).
		Msg("🎯 [SNIPER] EXECUTING SNIPE!")

	// Create position with per-asset stop loss and trailing stop
	pos := &SniperPosition{
		TradeID:      fmt.Sprintf("snipe-%s-%d", asset, time.Now().UnixNano()),
		Asset:        asset,
		Side:         side,
		TokenID:      tokenID,
		ConditionID:  w.ConditionID,
		WindowID:     w.ID,
		EntryPrice:   roundedPrice,
		Size:         size, // Will be updated with actual filled size
		EntryTime:    time.Now(),
		StopLoss:     assetStopLoss, // Use per-asset stop loss
		Target:       s.config.QuickFlipTarget,
		WindowEnd:    w.EndDate,
		PriceMovePct: priceMovePct, // Track price move to decide hold vs flip
		HighPrice:    roundedPrice, // Initialize high price to entry
		TrailingStop: assetStopLoss, // Start trailing stop at base SL
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
		size, // Use decimal size directly
		"BUY",
	)

	if err != nil {
		log.Error().Err(err).Str("asset", asset).Msg("🎯 [SNIPER] Order failed!")
		if s.dash != nil {
			s.dash.AddLog(fmt.Sprintf("❌ SNIPE FAILED: %s %s - %v", asset, side, err))
		}
		return
	}

	// Query actual filled size after a brief delay
	time.Sleep(500 * time.Millisecond)
	_, filledSize, _, statusErr := s.clobClient.GetOrderStatus(orderID)
	if statusErr == nil && filledSize.GreaterThan(decimal.Zero) {
		pos.Size = filledSize // Use ACTUAL filled size
		log.Info().
			Str("order_id", orderID).
			Str("filled", filledSize.StringFixed(2)).
			Msg("🎯 [SNIPER] Actual fill recorded")
	}

	pos.TradeID = orderID
	s.recordPosition(pos)

	// Notify
	if s.notifier != nil {
		s.notifier.SendTradeAlert(asset, side, roundedPrice, pos.Size.IntPart(), "SNIPE BUY", decimal.Zero)
	}

	if s.dash != nil {
		s.dash.AddLog(fmt.Sprintf("🎯 SNIPE: %s %s @ %s¢ x%s", asset, side, roundedPrice.Mul(decimal.NewFromInt(100)).StringFixed(0), pos.Size.StringFixed(2)))
		s.dash.UpdatePosition(asset, side, roundedPrice, roundedPrice, pos.Size.IntPart(), "SNIPING")
	}

	log.Info().
		Str("asset", asset).
		Str("side", side).
		Str("order_id", orderID).
		Str("filled_size", pos.Size.StringFixed(2)).
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
	ticker := time.NewTicker(100 * time.Millisecond) // Fast position monitoring for quick SL
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
		pnl := currentOdds.Sub(pos.EntryPrice).Mul(pos.Size)
		s.dash.UpdatePosition(pos.Asset, pos.Side, pos.EntryPrice, currentOdds, pos.Size.IntPart(), "ACTIVE")
		
		// Update ML signal display with current state
		edge := currentOdds.Sub(pos.EntryPrice).Mul(decimal.NewFromInt(100))
		s.dash.UpdateMLSignal(pos.Asset, pos.Side, currentOdds, 0.9, edge.StringFixed(0)+"¢", pnl.StringFixed(2), "SNIPING")
	}

	// ═══════════════════════════════════════════════════════════════════════════════
	// TRAILING STOP LOGIC: Lock in profits as price rises
	// ═══════════════════════════════════════════════════════════════════════════════
	
	// Update high water mark
	if currentOdds.GreaterThan(pos.HighPrice) {
		pos.HighPrice = currentOdds
		
		// Update trailing stop: once we're 5¢+ in profit, trail 8¢ below high
		profitFromEntry := currentOdds.Sub(pos.EntryPrice)
		trailingDistance := decimal.NewFromFloat(0.08) // 8¢ trailing distance
		
		if profitFromEntry.GreaterThanOrEqual(decimal.NewFromFloat(0.05)) { // 5¢ in profit
			newTrailingStop := pos.HighPrice.Sub(trailingDistance)
			// Only move trailing stop UP, never down
			if newTrailingStop.GreaterThan(pos.TrailingStop) {
				pos.TrailingStop = newTrailingStop
				log.Info().
					Str("asset", pos.Asset).
					Str("high", pos.HighPrice.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
					Str("trailing_stop", pos.TrailingStop.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
					Msg("📈 [SNIPER] Trailing stop raised!")
			}
		}
	}

	// ═══════════════════════════════════════════════════════════════════════════════
	// EXIT CONDITIONS
	// ═══════════════════════════════════════════════════════════════════════════════

	// 1. TARGET HIT (99¢+) - Take profit
	if currentOdds.GreaterThanOrEqual(s.config.QuickFlipTarget) {
		if s.config.HoldToResolution {
			// Hold to resolution enabled - check if price move is strong enough
			holdThreshold := decimal.NewFromFloat(0.001) // 0.1%
			if pos.PriceMovePct.GreaterThanOrEqual(holdThreshold) {
				log.Info().
					Str("asset", pos.Asset).
					Str("side", pos.Side).
					Str("current", currentOdds.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
					Str("price_move", pos.PriceMovePct.Mul(decimal.NewFromInt(100)).StringFixed(2)+"%").
					Msg("🎯 [SNIPER] STRONG MOVE (0.1%+) - HOLDING TO RESOLUTION for $1!")
				return // Don't quick flip, hold for resolution
			}
		}
		
		// Quick flip at target (default behavior)
		log.Info().
			Str("asset", pos.Asset).
			Str("side", pos.Side).
			Str("current", currentOdds.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Str("target", s.config.QuickFlipTarget.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Msg("🎯 [SNIPER] TARGET HIT! Quick flipping...")
		
		s.exitPosition(pos, currentOdds, "TARGET")
		return
	}

	// 2. TRAILING STOP HIT - Lock in profit
	if pos.TrailingStop.GreaterThan(pos.StopLoss) && currentOdds.LessThanOrEqual(pos.TrailingStop) {
		profit := currentOdds.Sub(pos.EntryPrice).Mul(pos.Size)
		log.Info().
			Str("asset", pos.Asset).
			Str("entry", pos.EntryPrice.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Str("exit", currentOdds.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Str("trailing_stop", pos.TrailingStop.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Str("profit", profit.StringFixed(2)).
			Msg("📈 [SNIPER] TRAILING STOP HIT! Locking in profit...")
		
		s.exitPosition(pos, currentOdds, "TRAILING_STOP")
		return
	}

	// 3. HARD STOP LOSS - Always active, protects capital
	if currentOdds.LessThanOrEqual(pos.StopLoss) {
		log.Warn().
			Str("asset", pos.Asset).
			Str("side", pos.Side).
			Str("current", currentOdds.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Str("stop", pos.StopLoss.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Msg("🛑 [SNIPER] STOP LOSS HIT! Exiting...")
		
		s.exitPosition(pos, currentOdds, "STOP_LOSS")
		return
	}

	// Last 30 seconds - just log, don't do anything special
	// Stop loss still active above, but orders might not fill this close to resolution
	if time.Until(pos.WindowEnd) < 30*time.Second {
		log.Debug().
			Str("asset", pos.Asset).
			Str("current", currentOdds.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Str("time_left", time.Until(pos.WindowEnd).String()).
			Msg("🎯 [SNIPER] Final 30s - stop loss still active")
	}
}

// exitPosition exits a position at market - SELLS 100% OF SHARES
func (s *SniperStrategy) exitPosition(pos *SniperPosition, exitPrice decimal.Decimal, reason string) {
	pnl := exitPrice.Sub(pos.EntryPrice).Mul(pos.Size)

	log.Info().
		Str("asset", pos.Asset).
		Str("side", pos.Side).
		Str("entry", pos.EntryPrice.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Str("exit", exitPrice.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Str("shares", pos.Size.StringFixed(2)).
		Str("pnl", pnl.StringFixed(2)).
		Str("reason", reason).
		Msg("🎯 [SNIPER] Exiting position - SELLING ALL SHARES")

	// Place sell order for EXACT SIZE we hold
	if s.clobClient != nil {
		slippage := decimal.NewFromFloat(0.03) // 3¢ slippage for exit
		sellPrice := exitPrice.Sub(slippage)
		if sellPrice.LessThan(decimal.NewFromFloat(0.01)) {
			sellPrice = decimal.NewFromFloat(0.01)
		}

		_, err := s.clobClient.PlaceLimitOrder(
			pos.TokenID,
			sellPrice,
			pos.Size, // SELL ALL SHARES (decimal precision)
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
		s.notifier.SendTradeAlert(pos.Asset, pos.Side, exitPrice, pos.Size.IntPart(), "SNIPE SELL", pnl)
	}
}

// handleResolution handles position at window end
func (s *SniperStrategy) handleResolution(pos *SniperPosition) {
	// Position held to resolution
	// We'll assume we won since we bet with the price direction
	pnl := decimal.NewFromInt(1).Sub(pos.EntryPrice).Mul(pos.Size)

	log.Info().
		Str("asset", pos.Asset).
		Str("side", pos.Side).
		Str("entry", pos.EntryPrice.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Str("shares", pos.Size.StringFixed(2)).
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
		s.notifier.SendTradeAlert(pos.Asset, pos.Side, decimal.NewFromInt(1), pos.Size.IntPart(), "SNIPE WON", pnl)
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

// updateMarketDataOnly updates dashboard market data without evaluating for trade
func (s *SniperStrategy) updateMarketDataOnly(w *polymarket.PredictionWindow, asset string) {
	if s.dash == nil {
		return
	}

	// Get current prices
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

	// Update dashboard with market data
	upOdds := w.YesPrice
	downOdds := w.NoPrice
	s.dash.UpdateMarket(asset, currentPrice, priceToBeat, upOdds, downOdds)
}
