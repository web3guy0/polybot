// Package arbitrage provides latency arbitrage functionality for Polymarket
//
// engine.go - Core arbitrage engine that exploits the lag between Binance price
// movements and Polymarket odds updates. When BTC moves on Binance but Polymarket
// odds haven't caught up, we buy the mispriced outcome.
//
// Strategy:
// 1. Track window start price from Polymarket
// 2. Monitor real-time BTC price from Binance WebSocket
// 3. Detect significant price moves (>0.2% default)
// 4. Check if Polymarket odds are stale (still ~50/50)
// 5. Calculate edge: if expected value is positive, execute
// 6. Buy the winning side at discounted odds
// 7. Collect $1 on resolution
package arbitrage

import (
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/internal/binance"
	"github.com/web3guy0/polybot/internal/config"
	"github.com/web3guy0/polybot/internal/polymarket"
)

// WindowState tracks the state of a prediction window
type WindowState struct {
	Window           *polymarket.PredictionWindow
	StartPrice       decimal.Decimal // BTC price when window started
	StartTime        time.Time
	LastOddsCheck    time.Time
	CurrentUpOdds    decimal.Decimal
	CurrentDownOdds  decimal.Decimal
	TradesThisWindow int
	LastTradeTime    time.Time
}

// Opportunity represents a detected arbitrage opportunity
type Opportunity struct {
	Window         *polymarket.PredictionWindow
	Direction      string          // "UP" or "DOWN"
	CurrentBTC     decimal.Decimal // Current Binance price
	StartBTC       decimal.Decimal // Window start price
	PriceChangePct decimal.Decimal // % change from window start
	MarketOdds     decimal.Decimal // Current odds on Polymarket for this direction
	FairOdds       decimal.Decimal // What odds should be given the move
	Edge           decimal.Decimal // Expected value per dollar
	Confidence     float64         // How confident we are (0-1)
	DetectedAt     time.Time
}

// Trade represents an executed arbitrage trade
type Trade struct {
	ID             string
	WindowID       string
	Question       string
	Direction      string          // "UP" or "DOWN"
	EntryPrice     decimal.Decimal // What we paid (e.g., 0.45)
	Amount         decimal.Decimal // Position size in USD
	BTCAtEntry     decimal.Decimal // BTC price when we entered
	BTCAtStart     decimal.Decimal // BTC price at window start
	PriceChangePct decimal.Decimal // % move that triggered trade
	Edge           decimal.Decimal // Expected edge at entry
	Status         string          // "open", "won", "lost"
	EnteredAt      time.Time
	ResolvedAt     *time.Time
	Profit         decimal.Decimal
}

// Engine is the core latency arbitrage engine
type Engine struct {
	cfg           *config.Config
	binanceClient *binance.Client
	windowScanner *polymarket.WindowScanner
	clobClient    *CLOBClient // CLOB trading client

	// Window tracking
	windowStates map[string]*WindowState
	windowsMu    sync.RWMutex

	// Trade tracking
	trades        []Trade
	tradesMu      sync.RWMutex
	totalTrades   int
	wonTrades     int
	lostTrades    int
	totalProfit   decimal.Decimal
	dailyPL       decimal.Decimal
	lastDailyReset time.Time

	// Configuration
	minPriceMove     decimal.Decimal // Min % move to trigger (default 0.2%)
	maxOddsForEntry  decimal.Decimal // Max odds we'll pay (default 0.65)
	minEdge          decimal.Decimal // Min expected edge (default 0.10)
	positionSize     decimal.Decimal // Per-trade size in USD
	maxDailyTrades   int             // Max trades per day
	maxTradesPerWindow int           // Max trades per window
	cooldownSeconds  int             // Seconds between trades

	// Callbacks
	onOpportunity func(Opportunity)
	onTrade       func(Trade)

	running bool
	stopCh  chan struct{}
}

// EngineConfig allows runtime configuration of the engine
type EngineConfig struct {
	MinPriceMove       decimal.Decimal
	MaxOddsForEntry    decimal.Decimal
	MinEdge            decimal.Decimal
	PositionSize       decimal.Decimal
	MaxDailyTrades     int
	MaxTradesPerWindow int
	CooldownSeconds    int
}

// NewEngine creates a new arbitrage engine
func NewEngine(cfg *config.Config, bc *binance.Client, ws *polymarket.WindowScanner) *Engine {
	e := &Engine{
		cfg:              cfg,
		binanceClient:    bc,
		windowScanner:    ws,
		windowStates:     make(map[string]*WindowState),
		trades:           make([]Trade, 0),
		totalProfit:      decimal.Zero,
		dailyPL:          decimal.Zero,
		lastDailyReset:   time.Now(),
		stopCh:           make(chan struct{}),

		// Default config - can be overridden by env vars
		minPriceMove:       decimal.NewFromFloat(0.002),  // 0.2%
		maxOddsForEntry:    decimal.NewFromFloat(0.65),   // 65 cents max
		minEdge:            decimal.NewFromFloat(0.10),   // 10% min edge
		positionSize:       decimal.NewFromFloat(100),    // $100 per trade
		maxDailyTrades:     200,
		maxTradesPerWindow: 3,
		cooldownSeconds:    10,
	}

	// Override from config if set
	if cfg.ArbPositionSize.GreaterThan(decimal.Zero) {
		e.positionSize = cfg.ArbPositionSize
	}

	return e
}

// SetCLOBClient sets the CLOB client for order execution
func (e *Engine) SetCLOBClient(client *CLOBClient) {
	e.clobClient = client
	log.Info().Msg("🔗 CLOB client connected to engine")
}

// SetConfig updates engine configuration
func (e *Engine) SetConfig(cfg EngineConfig) {
	if !cfg.MinPriceMove.IsZero() {
		e.minPriceMove = cfg.MinPriceMove
	}
	if !cfg.MaxOddsForEntry.IsZero() {
		e.maxOddsForEntry = cfg.MaxOddsForEntry
	}
	if !cfg.MinEdge.IsZero() {
		e.minEdge = cfg.MinEdge
	}
	if !cfg.PositionSize.IsZero() {
		e.positionSize = cfg.PositionSize
	}
	if cfg.MaxDailyTrades > 0 {
		e.maxDailyTrades = cfg.MaxDailyTrades
	}
	if cfg.MaxTradesPerWindow > 0 {
		e.maxTradesPerWindow = cfg.MaxTradesPerWindow
	}
	if cfg.CooldownSeconds > 0 {
		e.cooldownSeconds = cfg.CooldownSeconds
	}
}

// SetOpportunityCallback sets callback for detected opportunities (for alerts)
func (e *Engine) SetOpportunityCallback(cb func(Opportunity)) {
	e.onOpportunity = cb
}

// SetTradeCallback sets callback for executed trades
func (e *Engine) SetTradeCallback(cb func(Trade)) {
	e.onTrade = cb
}

// SetPositionSize updates the position size at runtime
func (e *Engine) SetPositionSize(size decimal.Decimal) {
	e.positionSize = size
	log.Info().Str("size", size.String()).Msg("Position size updated")
}

// GetPositionSize returns current position size
func (e *Engine) GetPositionSize() decimal.Decimal {
	return e.positionSize
}

// IsLive returns whether live trading is enabled
func (e *Engine) IsLive() bool {
	return !e.cfg.DryRun
}

// Start begins the arbitrage engine
func (e *Engine) Start() {
	e.running = true

	// Main arbitrage loop - check every 500ms for speed
	go e.arbitrageLoop()

	// Window state updater - refresh window start prices
	go e.windowStateLoop()

	// Odds refresher - keep odds fresh
	go e.oddsRefreshLoop()

	log.Info().
		Str("min_move", e.minPriceMove.Mul(decimal.NewFromInt(100)).String()+"%").
		Str("max_odds", e.maxOddsForEntry.String()).
		Str("position_size", e.positionSize.String()).
		Msg("⚡ Latency Arbitrage Engine started")
}

// Stop stops the engine
func (e *Engine) Stop() {
	e.running = false
	close(e.stopCh)
	log.Info().Msg("Arbitrage engine stopped")
}

// arbitrageLoop is the main loop checking for opportunities every 500ms
func (e *Engine) arbitrageLoop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.checkOpportunities()
		case <-e.stopCh:
			return
		}
	}
}

// windowStateLoop updates window states every 30 seconds
func (e *Engine) windowStateLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Initial update
	e.updateWindowStates()

	for {
		select {
		case <-ticker.C:
			e.updateWindowStates()
		case <-e.stopCh:
			return
		}
	}
}

// oddsRefreshLoop refreshes odds every 2 seconds
func (e *Engine) oddsRefreshLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.refreshOdds()
		case <-e.stopCh:
			return
		}
	}
}

// updateWindowStates tracks active windows and their start prices
func (e *Engine) updateWindowStates() {
	windows := e.windowScanner.GetActiveWindows()
	currentBTC := e.binanceClient.GetCurrentPrice()

	log.Debug().
		Int("active_windows", len(windows)).
		Str("btc_price", currentBTC.String()).
		Msg("📊 Window state update")

	e.windowsMu.Lock()
	defer e.windowsMu.Unlock()

	// Track new windows
	for i := range windows {
		w := &windows[i]
		if _, exists := e.windowStates[w.ID]; !exists {
			// New window - record start price
			e.windowStates[w.ID] = &WindowState{
				Window:     w,
				StartPrice: currentBTC,
				StartTime:  time.Now(),
			}
			log.Debug().
				Str("window", truncate(w.Question, 50)).
				Str("start_price", currentBTC.String()).
				Msg("📊 New window tracked")
		}
	}

	// Clean up expired windows
	activeIDs := make(map[string]bool)
	for _, w := range windows {
		activeIDs[w.ID] = true
	}
	for id := range e.windowStates {
		if !activeIDs[id] {
			delete(e.windowStates, id)
		}
	}
}

// refreshOdds fetches latest odds for all tracked windows
func (e *Engine) refreshOdds() {
	e.windowsMu.Lock()
	defer e.windowsMu.Unlock()

	for _, state := range e.windowStates {
		// Get fresh window data from scanner
		freshWindow := e.windowScanner.GetWindowByID(state.Window.ID)
		if freshWindow != nil {
			state.CurrentUpOdds = freshWindow.YesPrice
			state.CurrentDownOdds = freshWindow.NoPrice
			state.LastOddsCheck = time.Now()
		}
	}
}

// checkOpportunities scans for arbitrage opportunities
func (e *Engine) checkOpportunities() {
	currentBTC := e.binanceClient.GetCurrentPrice()
	if currentBTC.IsZero() {
		return
	}

	// Reset daily stats if new day
	if time.Since(e.lastDailyReset) > 24*time.Hour {
		e.dailyPL = decimal.Zero
		e.lastDailyReset = time.Now()
	}

	e.windowsMu.RLock()
	states := make([]*WindowState, 0, len(e.windowStates))
	for _, s := range e.windowStates {
		states = append(states, s)
	}
	e.windowsMu.RUnlock()

	for _, state := range states {
		opp := e.analyzeWindow(state, currentBTC)
		if opp != nil {
			e.handleOpportunity(*opp, state)
		}
	}
}

// analyzeWindow checks if a window has a profitable opportunity
func (e *Engine) analyzeWindow(state *WindowState, currentBTC decimal.Decimal) *Opportunity {
	if state.StartPrice.IsZero() {
		return nil
	}

	// Calculate price change from window start
	priceChange := currentBTC.Sub(state.StartPrice)
	priceChangePct := priceChange.Div(state.StartPrice)

	// Check if move is significant enough
	absChangePct := priceChangePct.Abs()
	if absChangePct.LessThan(e.minPriceMove) {
		return nil
	}

	// Determine direction
	var direction string
	var marketOdds decimal.Decimal

	if priceChangePct.IsPositive() {
		direction = "UP"
		marketOdds = state.CurrentUpOdds
	} else {
		direction = "DOWN"
		marketOdds = state.CurrentDownOdds
	}

	// Check if odds are stale (still cheap despite the move)
	if marketOdds.GreaterThan(e.maxOddsForEntry) {
		return nil // Market has already adjusted
	}

	// Calculate fair odds based on historical data
	// With a significant move in one direction, the probability shifts
	fairOdds := e.calculateFairOdds(absChangePct)

	// Calculate edge: what we expect to win - what we pay
	// If fair odds are 0.80 and we pay 0.50, edge = 0.80 - 0.50 = 0.30 (30%)
	edge := fairOdds.Sub(marketOdds)

	if edge.LessThan(e.minEdge) {
		return nil // Not enough edge
	}

	// Calculate confidence based on move size and time remaining
	confidence := e.calculateConfidence(absChangePct, state.Window.EndDate)

	return &Opportunity{
		Window:         state.Window,
		Direction:      direction,
		CurrentBTC:     currentBTC,
		StartBTC:       state.StartPrice,
		PriceChangePct: priceChangePct.Mul(decimal.NewFromInt(100)),
		MarketOdds:     marketOdds,
		FairOdds:       fairOdds,
		Edge:           edge,
		Confidence:     confidence,
		DetectedAt:     time.Now(),
	}
}

// calculateFairOdds estimates true probability based on price move
func (e *Engine) calculateFairOdds(absChangePct decimal.Decimal) decimal.Decimal {
	// Based on historical data from PurpleThunder's ~85% accuracy:
	// - 0.2% move → ~65% probability of continuing
	// - 0.5% move → ~80% probability of continuing
	// - 1.0% move → ~90% probability of continuing
	//
	// Using a simple model: P = 0.5 + (move% * 80)
	// Capped at 0.95

	movePct, _ := absChangePct.Float64()
	fairProb := 0.50 + (movePct * 80) // 0.2% move → 0.50 + 0.16 = 0.66

	if fairProb > 0.95 {
		fairProb = 0.95
	}

	return decimal.NewFromFloat(fairProb)
}

// calculateConfidence determines how confident we are in the opportunity
func (e *Engine) calculateConfidence(absChangePct decimal.Decimal, endTime time.Time) float64 {
	// Higher confidence with:
	// 1. Larger price moves
	// 2. More time remaining in window

	movePct, _ := absChangePct.Float64()
	moveScore := movePct * 200 // 0.5% move = 1.0 score

	timeRemaining := time.Until(endTime)
	timeScore := 0.5
	if timeRemaining > 10*time.Minute {
		timeScore = 0.3 // More time = less certain (could reverse)
	} else if timeRemaining > 5*time.Minute {
		timeScore = 0.5
	} else if timeRemaining > 2*time.Minute {
		timeScore = 0.7 // Sweet spot
	} else {
		timeScore = 0.9 // Very little time for reversal
	}

	confidence := (moveScore + timeScore) / 2
	if confidence > 1.0 {
		confidence = 1.0
	}

	return confidence
}

// handleOpportunity processes a detected opportunity
func (e *Engine) handleOpportunity(opp Opportunity, state *WindowState) {
	// Fire callback for alerts (always)
	if e.onOpportunity != nil {
		e.onOpportunity(opp)
	}

	// Check if we should trade
	if !e.shouldTrade(state) {
		return
	}

	// Execute trade
	trade := e.executeTrade(opp, state)
	if trade != nil {
		// Update state
		e.windowsMu.Lock()
		state.TradesThisWindow++
		state.LastTradeTime = time.Now()
		e.windowsMu.Unlock()

		// Fire callback
		if e.onTrade != nil {
			e.onTrade(*trade)
		}
	}
}

// shouldTrade determines if we should execute a trade
func (e *Engine) shouldTrade(state *WindowState) bool {
	// Check if in dry run mode
	if e.cfg.DryRun {
		return false
	}

	// Check if arbitrage is enabled
	if !e.cfg.ArbEnabled {
		return false
	}

	// Check daily trade limit
	if e.totalTrades >= e.maxDailyTrades {
		return false
	}

	// Check per-window limit
	if state.TradesThisWindow >= e.maxTradesPerWindow {
		return false
	}

	// Check cooldown
	if time.Since(state.LastTradeTime) < time.Duration(e.cooldownSeconds)*time.Second {
		return false
	}

	return true
}

// executeTrade executes an arbitrage trade
func (e *Engine) executeTrade(opp Opportunity, state *WindowState) *Trade {
	trade := &Trade{
		ID:             fmt.Sprintf("arb_%d", time.Now().UnixNano()),
		WindowID:       opp.Window.ID,
		Question:       opp.Window.Question,
		Direction:      opp.Direction,
		EntryPrice:     opp.MarketOdds,
		Amount:         e.positionSize,
		BTCAtEntry:     opp.CurrentBTC,
		BTCAtStart:     opp.StartBTC,
		PriceChangePct: opp.PriceChangePct,
		Edge:           opp.Edge,
		Status:         "open",
		EnteredAt:      time.Now(),
	}

	// Determine which token to buy (YES=Up, NO=Down)
	var tokenID string
	if opp.Direction == "UP" {
		tokenID = opp.Window.YesTokenID
	} else {
		tokenID = opp.Window.NoTokenID
	}

	// Calculate size: $amount / price = shares
	// E.g., $1 / 0.45 = 2.22 shares
	shares := e.positionSize.Div(opp.MarketOdds).Round(2)

	log.Info().
		Str("direction", trade.Direction).
		Str("entry_price", trade.EntryPrice.String()).
		Str("amount", trade.Amount.String()).
		Str("shares", shares.String()).
		Str("btc_move", trade.PriceChangePct.String()+"%").
		Str("edge", trade.Edge.Mul(decimal.NewFromInt(100)).String()+"%").
		Str("window", truncate(trade.Question, 40)).
		Msg("⚡ ARBITRAGE TRADE SIGNAL")

	// Execute actual trade if CLOB client is available
	if e.clobClient != nil && tokenID != "" {
		orderResp, err := e.clobClient.PlaceMarketBuy(tokenID, shares)
		if err != nil {
			log.Error().
				Err(err).
				Str("token", tokenID).
				Str("shares", shares.String()).
				Msg("❌ Order placement failed")
			trade.Status = "failed"
		} else {
			log.Info().
				Str("order_id", orderResp.OrderID).
				Str("status", orderResp.Status).
				Msg("✅ Order placed successfully")
			trade.ID = orderResp.OrderID
			trade.Status = "filled"
		}
	} else if e.clobClient == nil {
		log.Warn().Msg("⚠️ CLOB client not connected - trade simulated only")
		trade.Status = "simulated"
	} else {
		log.Warn().Str("direction", opp.Direction).Msg("⚠️ No token ID for direction - trade skipped")
		trade.Status = "skipped"
	}

	// Track trade
	e.tradesMu.Lock()
	e.trades = append(e.trades, *trade)
	e.totalTrades++
	e.tradesMu.Unlock()

	return trade
}

// GetStats returns current arbitrage stats
func (e *Engine) GetStats() map[string]interface{} {
	e.tradesMu.RLock()
	defer e.tradesMu.RUnlock()

	winRate := 0.0
	if e.totalTrades > 0 {
		winRate = float64(e.wonTrades) / float64(e.totalTrades) * 100
	}

	return map[string]interface{}{
		"total_trades":   e.totalTrades,
		"won":            e.wonTrades,
		"lost":           e.lostTrades,
		"win_rate":       fmt.Sprintf("%.1f%%", winRate),
		"total_profit":   e.totalProfit.String(),
		"daily_pl":       e.dailyPL.String(),
		"active_windows": len(e.windowStates),
	}
}

// GetActiveOpportunities returns current opportunities (for debugging)
func (e *Engine) GetActiveOpportunities() []Opportunity {
	currentBTC := e.binanceClient.GetCurrentPrice()
	if currentBTC.IsZero() {
		return nil
	}

	e.windowsMu.RLock()
	defer e.windowsMu.RUnlock()

	opps := make([]Opportunity, 0)
	for _, state := range e.windowStates {
		opp := e.analyzeWindow(state, currentBTC)
		if opp != nil {
			opps = append(opps, *opp)
		}
	}

	return opps
}

// GetRecentTrades returns recent trades
func (e *Engine) GetRecentTrades(limit int) []Trade {
	e.tradesMu.RLock()
	defer e.tradesMu.RUnlock()

	if len(e.trades) <= limit {
		result := make([]Trade, len(e.trades))
		copy(result, e.trades)
		return result
	}

	start := len(e.trades) - limit
	result := make([]Trade, limit)
	copy(result, e.trades[start:])
	return result
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
