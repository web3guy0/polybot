// Package arbitrage provides latency arbitrage functionality for Polymarket
//
// engine.go - Core arbitrage engine that exploits the lag between price
// movements and Polymarket odds updates. When BTC moves but Polymarket
// odds haven't caught up, we buy the mispriced outcome.
//
// Strategy:
// 1. Track window start price from Chainlink on-chain
// 2. Monitor real-time BTC price from CMC (same source as Polymarket!)
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
	"github.com/web3guy0/polybot/internal/chainlink"
	"github.com/web3guy0/polybot/internal/cmc"
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
	TokenID        string          // Token ID we bought
	EntryPrice     decimal.Decimal // What we paid (e.g., 0.45)
	ExitPrice      decimal.Decimal // What we sold at (0 if held to resolution)
	Amount         decimal.Decimal // Position size in USD
	Shares         decimal.Decimal // Number of shares purchased
	BTCAtEntry     decimal.Decimal // BTC price when we entered
	BTCAtStart     decimal.Decimal // BTC price at window start
	PriceChangePct decimal.Decimal // % move that triggered trade
	Edge           decimal.Decimal // Expected edge at entry
	Status         string          // "open", "exited", "won", "lost"
	ExitType       string          // "quick_flip", "resolution", ""
	EnteredAt      time.Time
	ExitedAt       *time.Time
	ResolvedAt     *time.Time
	Profit         decimal.Decimal
}

// OpenPosition represents an active position being monitored for exit
type OpenPosition struct {
	Trade          *Trade
	TokenID        string
	Direction      string          // "UP" or "DOWN"
	EntryPrice     decimal.Decimal
	Shares         decimal.Decimal
	BTCMoveAtEntry decimal.Decimal // The % BTC move when we entered
	WindowEndTime  time.Time
	EnteredAt      time.Time
}

// Engine is the core latency arbitrage engine
type Engine struct {
	cfg             *config.Config
	binanceClient   *binance.Client
	chainlinkClient *chainlink.Client // Chainlink price feed (what Polymarket uses for resolution)
	cmcClient       *cmc.Client       // CMC price feed (fast updates, same source as Data Streams!)
	windowScanner   *polymarket.WindowScanner
	clobClient      *CLOBClient // CLOB trading client

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

	// Open position tracking for exit monitoring
	openPositions map[string]*OpenPosition
	positionsMu   sync.RWMutex

	// Configuration
	minPriceMove     decimal.Decimal // Min % move to trigger (default 0.2%)
	minOddsForEntry  decimal.Decimal // Min odds to buy (default 0.35 for 35¢ floor)
	maxOddsForEntry  decimal.Decimal // Max odds we'll pay (default 0.65 for 35-65¢ range)
	minEdge          decimal.Decimal // Min expected edge (default 0.10)
	positionSize     decimal.Decimal // Per-trade size in USD
	maxDailyTrades   int             // Max trades per day
	maxTradesPerWindow int           // Max trades per window
	cooldownSeconds  int             // Seconds between trades

	// Exit Strategy Configuration (3 paths)
	// Exit Path A: Quick flip when odds reach 75¢+
	// Exit Path B: Hold to resolution when move is large and time is short
	// Exit Path C: STOP-LOSS when odds drop 20% from entry
	exitOddsThreshold  decimal.Decimal // Sell when odds reach this (default 0.75)
	holdThreshold      decimal.Decimal // Hold if price move exceeds this % (default 0.005 = 0.5%)
	stopLossPct        decimal.Decimal // Stop-loss: exit if odds drop this % from entry (default 0.20 = 20%)

	// Callbacks
	onOpportunity func(Opportunity)
	onTrade       func(Trade)
	onExit        func(Trade) // Called when position is exited

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
// Uses CMC for fast price detection (same source as Polymarket Data Streams!)
func NewEngine(cfg *config.Config, bc *binance.Client, cl *chainlink.Client, cmcCl *cmc.Client, ws *polymarket.WindowScanner) *Engine {
	e := &Engine{
		cfg:              cfg,
		binanceClient:    bc,
		chainlinkClient:  cl,
		cmcClient:        cmcCl,
		windowScanner:    ws,
		windowStates:     make(map[string]*WindowState),
		openPositions:    make(map[string]*OpenPosition),
		trades:           make([]Trade, 0),
		totalProfit:      decimal.Zero,
		dailyPL:          decimal.Zero,
		lastDailyReset:   time.Now(),
		stopCh:           make(chan struct{}),

		// Default config - FEE-ADJUSTED strategy (with 3.15% Polymarket fee)
		// Buy at 35-65¢, exit at 75¢ OR stop-loss at -20% OR hold to resolution
		minPriceMove:       decimal.NewFromFloat(0.002),  // 0.2% min move
		minOddsForEntry:    decimal.NewFromFloat(0.35),   // 35 cents min (avoid extreme odds)
		maxOddsForEntry:    decimal.NewFromFloat(0.65),   // 65 cents max (wider range)
		minEdge:            decimal.NewFromFloat(0.10),   // 10% min edge
		positionSize:       decimal.NewFromFloat(100),    // $100 per trade
		maxDailyTrades:     200,
		maxTradesPerWindow: 3,
		cooldownSeconds:    10,

		// Triple exit strategy thresholds
		exitOddsThreshold:  decimal.NewFromFloat(0.75),   // Quick flip at 75¢
		holdThreshold:      decimal.NewFromFloat(0.002),  // Hold if move > 0.2% (~$180 on BTC)
		stopLossPct:        decimal.NewFromFloat(0.20),   // 🛑 STOP-LOSS: exit if odds drop 20%
	}

	// Override from config if set
	if cfg.ArbPositionSize.GreaterThan(decimal.Zero) {
		e.positionSize = cfg.ArbPositionSize
	}
	if cfg.ArbMinOddsForEntry.GreaterThan(decimal.Zero) {
		e.minOddsForEntry = cfg.ArbMinOddsForEntry
	}
	if cfg.ArbMaxOddsForEntry.GreaterThan(decimal.Zero) {
		e.maxOddsForEntry = cfg.ArbMaxOddsForEntry
	}
	if cfg.ArbExitOddsThreshold.GreaterThan(decimal.Zero) {
		e.exitOddsThreshold = cfg.ArbExitOddsThreshold
	}
	if cfg.ArbHoldThreshold.GreaterThan(decimal.Zero) {
		e.holdThreshold = cfg.ArbHoldThreshold
	}
	if cfg.ArbStopLossPct.GreaterThan(decimal.Zero) {
		e.stopLossPct = cfg.ArbStopLossPct
	}

	return e
}

// SetCLOBClient sets the CLOB client for order execution
func (e *Engine) SetCLOBClient(client *CLOBClient) {
	e.clobClient = client
	log.Info().Msg("🔗 CLOB client connected to engine")
}

// SetExitCallback sets callback for position exits
func (e *Engine) SetExitCallback(cb func(Trade)) {
	e.onExit = cb
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

// GetBTCPrice returns the current BTC price from Binance with Chainlink comparison
// Binance is used for real-time detection, Chainlink is what Polymarket uses for resolution
func (e *Engine) GetBTCPrice() (binancePrice, chainlinkPrice decimal.Decimal, priceDiff decimal.Decimal) {
	binancePrice = e.binanceClient.GetCurrentPrice()
	
	if e.chainlinkClient != nil {
		chainlinkPrice = e.chainlinkClient.GetCurrentPrice()
		if !chainlinkPrice.IsZero() && !binancePrice.IsZero() {
			priceDiff = binancePrice.Sub(chainlinkPrice).Div(chainlinkPrice).Mul(decimal.NewFromInt(100))
		}
	}
	
	return binancePrice, chainlinkPrice, priceDiff
}

// GetChainlinkPrice returns the Chainlink price (what Polymarket uses for resolution)
func (e *Engine) GetChainlinkPrice() decimal.Decimal {
	if e.chainlinkClient != nil {
		return e.chainlinkClient.GetCurrentPrice()
	}
	return decimal.Zero
}

// Start begins the arbitrage engine
func (e *Engine) Start() {
	e.running = true

	// Main arbitrage loop - check every 100ms for speed (critical for 30-90s lag window)
	go e.arbitrageLoop()

	// Window state updater - refresh window start prices
	go e.windowStateLoop()

	// Odds refresher - keep odds fresh
	go e.oddsRefreshLoop()

	// Position monitor - check for exit opportunities every 100ms
	go e.positionMonitorLoop()

	log.Info().
		Str("min_move", e.minPriceMove.Mul(decimal.NewFromInt(100)).String()+"%").
		Str("entry_range", e.minOddsForEntry.String()+"-"+e.maxOddsForEntry.String()).
		Str("exit_target", e.exitOddsThreshold.String()).
		Str("stop_loss", e.stopLossPct.Mul(decimal.NewFromInt(100)).String()+"%").
		Str("position_size", e.positionSize.String()).
		Msg("⚡ Latency Arbitrage Engine started (100ms scan)")
}

// Stop stops the engine
func (e *Engine) Stop() {
	e.running = false
	close(e.stopCh)
	log.Info().Msg("Arbitrage engine stopped")
}

// arbitrageLoop is the main loop checking for opportunities every 100ms
// Speed is critical - the 30-90 second lag window requires fast detection
func (e *Engine) arbitrageLoop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	statusTicker := time.NewTicker(30 * time.Second) // Status update every 30s
	defer ticker.Stop()
	defer statusTicker.Stop()

	for {
		select {
		case <-ticker.C:
			e.checkOpportunities()
		case <-statusTicker.C:
			e.logStatus() // Show current market analysis
		case <-e.stopCh:
			return
		}
	}
}

// logStatus logs current market analysis for debugging
func (e *Engine) logStatus() {
	e.windowsMu.RLock()
	defer e.windowsMu.RUnlock()

	// Use CMC price (fastest, same source as Data Streams!)
	var currentPrice decimal.Decimal
	var priceSource string
	
	if e.cmcClient != nil && !e.cmcClient.IsStale() {
		currentPrice = e.cmcClient.GetCurrentPrice()
		priceSource = "CMC"
	}
	if currentPrice.IsZero() && e.chainlinkClient != nil {
		currentPrice = e.chainlinkClient.GetCurrentPrice()
		priceSource = "Chainlink"
	}
	if currentPrice.IsZero() {
		currentPrice = e.binanceClient.GetCurrentPrice()
		priceSource = "Binance"
	}
	binancePrice := e.binanceClient.GetCurrentPrice()

	for _, state := range e.windowStates {
		if state.StartPrice.IsZero() {
			continue
		}

		priceChange := currentPrice.Sub(state.StartPrice)
		priceChangePct := priceChange.Div(state.StartPrice).Mul(decimal.NewFromInt(100))
		absChangePct := priceChangePct.Abs()

		var direction string
		var targetOdds decimal.Decimal
		if priceChangePct.IsPositive() {
			direction = "UP"
			targetOdds = state.CurrentUpOdds
		} else {
			direction = "DOWN"
			targetOdds = state.CurrentDownOdds
		}

		// Determine why no trade
		var reason string
		minMovePct := e.minPriceMove.Mul(decimal.NewFromInt(100)) // Convert 0.002 to 0.2
		
		if absChangePct.LessThan(minMovePct) {
			reason = fmt.Sprintf("⏳ Move too small (%.3f%% < %.1f%%)", absChangePct.InexactFloat64(), minMovePct.InexactFloat64())
		} else if targetOdds.GreaterThan(e.maxOddsForEntry) {
			reason = fmt.Sprintf("📈 Odds too high (%.0f¢ > %.0f¢)", targetOdds.Mul(decimal.NewFromInt(100)).InexactFloat64(), e.maxOddsForEntry.Mul(decimal.NewFromInt(100)).InexactFloat64())
		} else if targetOdds.LessThan(e.minOddsForEntry) {
			reason = fmt.Sprintf("📉 Odds too low (%.0f¢ < %.0f¢)", targetOdds.Mul(decimal.NewFromInt(100)).InexactFloat64(), e.minOddsForEntry.Mul(decimal.NewFromInt(100)).InexactFloat64())
		} else {
			reason = "🚀 READY TO TRADE!"
		}

		log.Info().
			Str("direction", direction).
			Str("btc_move", priceChangePct.StringFixed(3)+"%").
			Str("min_move", minMovePct.StringFixed(1)+"%").
			Str("price", currentPrice.StringFixed(2)).
			Str("source", priceSource).
			Str("binance", binancePrice.StringFixed(2)).
			Str("spread", binancePrice.Sub(currentPrice).StringFixed(2)).
			Str("up_odds", state.CurrentUpOdds.String()).
			Str("down_odds", state.CurrentDownOdds.String()).
			Str("reason", reason).
			Msg("📊 MARKET STATUS")
	}
}

// windowStateLoop updates window states every 30 seconds
func (e *Engine) windowStateLoop() {
	ticker := time.NewTicker(5 * time.Second)
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
	ticker := time.NewTicker(500 * time.Millisecond)
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

// positionMonitorLoop monitors open positions for exit opportunities every 100ms
// Implements dual exit strategy:
// - Quick Flip: Sell when odds reach 70-80¢ (30-90 seconds after entry typically)
// - Hold to Resolution: Keep position if BTC move was large (>0.5%) and time remaining is short
func (e *Engine) positionMonitorLoop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.checkPositionExits()
		case <-e.stopCh:
			return
		}
	}
}

// checkPositionExits evaluates all open positions for exit conditions
func (e *Engine) checkPositionExits() {
	e.positionsMu.RLock()
	positions := make([]*OpenPosition, 0, len(e.openPositions))
	for _, pos := range e.openPositions {
		positions = append(positions, pos)
	}
	e.positionsMu.RUnlock()

	if len(positions) == 0 {
		return
	}

	oddsFetcher := NewOddsFetcher()

	for _, pos := range positions {
		// Get current odds for this token
		odds, err := oddsFetcher.GetLiveOdds(pos.TokenID)
		if err != nil {
			continue
		}

		// Check exit conditions
		exitType, shouldExit := e.shouldExitPosition(pos, odds)
		if shouldExit {
			e.executeExit(pos, odds, exitType)
		}
	}
}

// shouldExitPosition determines if we should exit a position and how
// Returns (exitType, shouldExit)
// Three exit paths:
//   A) Quick flip at 75¢+ (profit taking)
//   B) Hold to resolution if large move + short time
//   C) STOP-LOSS at -20% (risk management - CRITICAL!)
func (e *Engine) shouldExitPosition(pos *OpenPosition, odds *LiveOdds) (string, bool) {
	timeRemaining := time.Until(pos.WindowEndTime)

	// ⚠️ EXIT PATH C: STOP-LOSS - CHECK FIRST! (Most important for risk management)
	// If odds have dropped 20% from entry, cut losses immediately
	// E.g., bought at 50¢, now at 40¢ → 20% drop → STOP LOSS
	if !pos.EntryPrice.IsZero() && !odds.BestBid.IsZero() {
		priceDropPct := pos.EntryPrice.Sub(odds.BestBid).Div(pos.EntryPrice)
		
		if priceDropPct.GreaterThanOrEqual(e.stopLossPct) {
			loss := pos.EntryPrice.Sub(odds.BestBid).Mul(pos.Shares)
			log.Warn().
				Str("position", pos.Trade.ID).
				Str("entry", pos.EntryPrice.String()).
				Str("current", odds.BestBid.String()).
				Str("drop_pct", priceDropPct.Mul(decimal.NewFromInt(100)).StringFixed(1)+"%").
				Str("loss", loss.Neg().String()).
				Msg("🛑 STOP-LOSS TRIGGERED - Cutting losses!")
			return "stop_loss", true
		}
	}

	// Exit Path B: Hold to resolution if move was large AND time is short
	// If BTC moved >0.5% and less than 2 minutes remain, hold for $1.00 payout
	if pos.BTCMoveAtEntry.Abs().GreaterThanOrEqual(e.holdThreshold) && timeRemaining < 2*time.Minute {
		log.Debug().
			Str("position", pos.Trade.ID).
			Str("btc_move", pos.BTCMoveAtEntry.Mul(decimal.NewFromInt(100)).String()+"%").
			Str("time_remaining", timeRemaining.String()).
			Msg("📊 Holding to resolution - large move, short time")
		return "", false // Don't exit, hold to resolution
	}

	// Exit Path A: Quick flip when odds catch up to 75¢+
	// BestBid is what we can sell at
	if odds.BestBid.GreaterThanOrEqual(e.exitOddsThreshold) {
		profit := odds.BestBid.Sub(pos.EntryPrice).Mul(pos.Shares)
		profitPct := odds.BestBid.Sub(pos.EntryPrice).Div(pos.EntryPrice).Mul(decimal.NewFromInt(100))

		log.Info().
			Str("position", pos.Trade.ID).
			Str("entry", pos.EntryPrice.String()).
			Str("exit", odds.BestBid.String()).
			Str("profit", profit.String()).
			Str("profit_pct", profitPct.StringFixed(1)+"%").
			Msg("🎯 Quick flip exit triggered")

		return "quick_flip", true
	}

	// Fallback: If less than 30 seconds remain and we're in profit, exit
	if timeRemaining < 30*time.Second && odds.BestBid.GreaterThan(pos.EntryPrice) {
		return "quick_flip", true
	}

	return "", false
}

// executeExit sells the position
func (e *Engine) executeExit(pos *OpenPosition, odds *LiveOdds, exitType string) {
	if e.cfg.DryRun || e.clobClient == nil {
		// Dry run - just log and update state
		e.recordExit(pos, odds.BestBid, exitType, true)
		return
	}

	// Place market sell order
	resp, err := e.clobClient.PlaceMarketSell(pos.TokenID, pos.Shares)
	if err != nil {
		log.Error().Err(err).Str("position", pos.Trade.ID).Msg("Failed to execute exit")
		return
	}

	if resp.Status == "matched" || resp.Status == "filled" {
		e.recordExit(pos, odds.BestBid, exitType, true)
	} else {
		log.Warn().Str("status", resp.Status).Str("position", pos.Trade.ID).Msg("Exit order not filled")
	}
}

// recordExit updates trade state after exit
func (e *Engine) recordExit(pos *OpenPosition, exitPrice decimal.Decimal, exitType string, success bool) {
	now := time.Now()
	profit := exitPrice.Sub(pos.EntryPrice).Mul(pos.Shares)

	// Update trade
	e.tradesMu.Lock()
	for i := range e.trades {
		if e.trades[i].ID == pos.Trade.ID {
			e.trades[i].Status = "exited"
			e.trades[i].ExitType = exitType
			e.trades[i].ExitPrice = exitPrice
			e.trades[i].ExitedAt = &now
			e.trades[i].Profit = profit

			// Update stats
			e.totalProfit = e.totalProfit.Add(profit)
			e.dailyPL = e.dailyPL.Add(profit)
			if profit.IsPositive() {
				e.wonTrades++
			} else {
				e.lostTrades++
			}

			// Fire callback
			if e.onExit != nil {
				e.onExit(e.trades[i])
			}
			break
		}
	}
	e.tradesMu.Unlock()

	// Remove from open positions
	e.positionsMu.Lock()
	delete(e.openPositions, pos.Trade.ID)
	e.positionsMu.Unlock()

	log.Info().
		Str("id", pos.Trade.ID).
		Str("direction", pos.Direction).
		Str("entry", pos.EntryPrice.String()).
		Str("exit", exitPrice.String()).
		Str("profit", profit.String()).
		Str("exit_type", exitType).
		Msg("💰 Position exited")
}

// updateWindowStates tracks active windows and their start prices
func (e *Engine) updateWindowStates() {
	windows := e.windowScanner.GetActiveWindows()
	binancePrice, chainlinkPrice, priceDiff := e.GetBTCPrice()

	// Log both price sources for comparison
	logEvent := log.Debug().
		Int("active_windows", len(windows)).
		Str("binance", binancePrice.StringFixed(2))
	
	if !chainlinkPrice.IsZero() {
		logEvent = logEvent.
			Str("chainlink", chainlinkPrice.StringFixed(2)).
			Str("diff_pct", priceDiff.StringFixed(4)+"%")
	}
	logEvent.Msg("📊 Window state update")

	e.windowsMu.Lock()
	defer e.windowsMu.Unlock()

	// Track new windows
	for i := range windows {
		w := &windows[i]
		if _, exists := e.windowStates[w.ID]; !exists {
			// New window - get the "Price to Beat"
			// 
			// IMPORTANT: Use BINANCE for both current price AND Price to Beat
			// This ensures CONSISTENCY - even if Binance differs from Chainlink Data Streams
			// by ~$30-50, the DIRECTION calculation will be correct because we're comparing
			// Binance-to-Binance, not Binance-to-Chainlink
			//
			// Polymarket uses Chainlink Data Streams (paid, sub-second) for resolution
			// We can't access that for free, so we use the fastest free source: Binance
			var startPrice decimal.Decimal
			
			// Primary method: Binance 1s kline at window start (FAST + CONSISTENT)
			if e.binanceClient != nil && !w.StartDate.IsZero() {
				historicalPrice, err := e.binanceClient.GetPriceAtTime(w.StartDate)
				if err == nil && !historicalPrice.IsZero() {
					startPrice = historicalPrice
					log.Info().
						Str("window_start", w.StartDate.Format("15:04:05")).
						Str("price_to_beat", startPrice.StringFixed(2)).
						Msg("📈 Got Price to Beat from Binance (consistent with detection)")
				} else {
					log.Debug().Err(err).Msg("Could not get Binance historical price")
				}
			}
			
			// Fallback to Chainlink on-chain if Binance fails
			if startPrice.IsZero() && e.chainlinkClient != nil && !w.StartDate.IsZero() {
				historicalPrice, err := e.chainlinkClient.GetHistoricalPrice(w.StartDate)
				if err == nil && !historicalPrice.IsZero() {
					startPrice = historicalPrice
					log.Info().
						Str("window_start", w.StartDate.Format("15:04:05")).
						Str("price_to_beat", startPrice.StringFixed(2)).
						Msg("⛓️ Got Price to Beat from Chainlink on-chain (fallback)")
				}
			}
			
			// Fallback to current Binance price
			if startPrice.IsZero() {
				startPrice = binancePrice
				log.Debug().Str("price", startPrice.StringFixed(2)).Msg("Using current Binance price as fallback")
			}
			
			e.windowStates[w.ID] = &WindowState{
				Window:     w,
				StartPrice: startPrice,
				StartTime:  w.StartDate, // Use actual window start, not when we saw it
			}
			
			// If StartTime is zero, use current time
			if e.windowStates[w.ID].StartTime.IsZero() {
				e.windowStates[w.ID].StartTime = time.Now()
			}
			
			log.Info().
				Str("window", truncate(w.Question, 50)).
				Str("price_to_beat", startPrice.StringFixed(2)).
				Str("start_time", e.windowStates[w.ID].StartTime.Format("15:04:05")).
				Msg("📊 Window tracked")
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
// Uses direct CLOB API for faster price updates when available
func (e *Engine) refreshOdds() {
	e.windowsMu.Lock()
	defer e.windowsMu.Unlock()

	for _, state := range e.windowStates {
		// Try direct CLOB price fetch first (faster)
		if e.clobClient != nil && state.Window.YesTokenID != "" {
			// Fetch Yes token price directly from CLOB
			yesPrice, err := e.clobClient.GetMidPrice(state.Window.YesTokenID)
			if err == nil && !yesPrice.IsZero() {
				state.CurrentUpOdds = yesPrice
				state.CurrentDownOdds = decimal.NewFromInt(1).Sub(yesPrice)
				state.LastOddsCheck = time.Now()
				continue
			}
		}
		
		// Fallback to scanner cache
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
	// ⚡ PRIORITY ORDER for current price:
	// 1. CMC - Updates every 1s, same source as Polymarket Data Streams (~$1 diff)
	// 2. Chainlink on-chain - Updates every 20-60s, ~$1 diff from Data Streams
	// 3. Binance - Fast but ~$80 diff from Data Streams (direction mismatch!)
	var currentBTC decimal.Decimal
	
	// Try CMC first (fastest + most accurate)
	if e.cmcClient != nil && !e.cmcClient.IsStale() {
		currentBTC = e.cmcClient.GetCurrentPrice()
	}
	// Fallback to Chainlink on-chain
	if currentBTC.IsZero() && e.chainlinkClient != nil {
		currentBTC = e.chainlinkClient.GetCurrentPrice()
	}
	// Last resort: Binance (less accurate but always available)
	if currentBTC.IsZero() {
		currentBTC = e.binanceClient.GetCurrentPrice()
	}
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
		return nil // Move too small, no opportunity
	}

	// Determine direction we'd bet on
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
	// Entry range: 35¢-65¢ (with new Polymarket fees, wider range is better)
	if marketOdds.GreaterThan(e.maxOddsForEntry) {
		log.Debug().
			Str("odds", marketOdds.String()).
			Str("max", e.maxOddsForEntry.String()).
			Msg("⏭️ Odds too high - market already adjusted")
		return nil // Too expensive - market has already adjusted
	}
	if marketOdds.LessThan(e.minOddsForEntry) {
		log.Debug().
			Str("odds", marketOdds.String()).
			Str("min", e.minOddsForEntry.String()).
			Msg("⏭️ Odds too low - skipping")
		return nil // Too cheap - something is wrong or too risky
	}

	// Calculate fair odds based on historical data
	// With a significant move in one direction, the probability shifts
	fairOdds := e.calculateFairOdds(absChangePct)

	// Calculate edge: what we expect to win - what we pay
	// If fair odds are 0.80 and we pay 0.50, edge = 0.80 - 0.50 = 0.30 (30%)
	edge := fairOdds.Sub(marketOdds)

	if edge.LessThan(e.minEdge) {
		log.Debug().
			Str("edge", edge.Mul(decimal.NewFromInt(100)).String()+"%").
			Str("min_edge", e.minEdge.Mul(decimal.NewFromInt(100)).String()+"%").
			Msg("⏭️ Edge too small")
		return nil // Not enough edge
	}

	// 🚀 OPPORTUNITY FOUND! Log it prominently
	log.Info().
		Str("direction", direction).
		Str("btc_move", absChangePct.Mul(decimal.NewFromInt(100)).StringFixed(2)+"%").
		Str("odds", marketOdds.String()).
		Str("edge", edge.Mul(decimal.NewFromInt(100)).StringFixed(1)+"%").
		Msg("🎯 OPPORTUNITY DETECTED!")

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
		log.Debug().Msg("❌ Trade blocked: DRY_RUN=true")
		return false
	}

	// Check if arbitrage is enabled
	if !e.cfg.ArbEnabled {
		log.Debug().Msg("❌ Trade blocked: ARB_ENABLED=false")
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
	// Determine which token to buy (YES=Up, NO=Down)
	var tokenID string
	if opp.Direction == "UP" {
		tokenID = opp.Window.YesTokenID
	} else {
		tokenID = opp.Window.NoTokenID
	}

	// Safety check for zero odds
	if opp.MarketOdds.IsZero() || opp.MarketOdds.LessThanOrEqual(decimal.Zero) {
		log.Error().Msg("Cannot execute trade: MarketOdds is zero")
		return nil
	}

	// Calculate size: $amount / price = shares
	// E.g., $1 / 0.45 = 2.22 shares
	shares := e.positionSize.Div(opp.MarketOdds).Round(2)

	trade := &Trade{
		ID:             fmt.Sprintf("arb_%d", time.Now().UnixNano()),
		WindowID:       opp.Window.ID,
		Question:       opp.Window.Question,
		Direction:      opp.Direction,
		TokenID:        tokenID,
		EntryPrice:     opp.MarketOdds,
		Amount:         e.positionSize,
		Shares:         shares,
		BTCAtEntry:     opp.CurrentBTC,
		BTCAtStart:     opp.StartBTC,
		PriceChangePct: opp.PriceChangePct,
		Edge:           opp.Edge,
		Status:         "open",
		EnteredAt:      time.Now(),
	}

	log.Info().
		Str("direction", trade.Direction).
		Str("entry_price", trade.EntryPrice.String()).
		Str("amount", trade.Amount.String()).
		Str("shares", shares.String()).
		Str("btc_move", trade.PriceChangePct.String()+"%").
		Str("edge", trade.Edge.Mul(decimal.NewFromInt(100)).String()+"%").
		Str("window", truncate(trade.Question, 40)).
		Bool("live", !e.cfg.DryRun).
		Msg("⚡ ARBITRAGE TRADE EXECUTING...")

	// ⚡ Execute actual trade if CLOB client is available
	// Use PlaceMarketBuyAtPrice to avoid redundant odds fetch (SPEED CRITICAL!)
	if e.clobClient != nil && tokenID != "" {
		startOrder := time.Now()
		orderResp, err := e.clobClient.PlaceMarketBuyAtPrice(tokenID, shares, opp.MarketOdds)
		orderTime := time.Since(startOrder)
		
		if err != nil {
			log.Error().
				Err(err).
				Str("token", tokenID).
				Str("shares", shares.String()).
				Dur("order_time_ms", orderTime).
				Msg("❌ Order placement failed")
			trade.Status = "failed"
			return nil
		}

		log.Info().
			Str("order_id", orderResp.OrderID).
			Str("status", orderResp.Status).
			Dur("order_time_ms", orderTime).
			Msg("✅ Order filled")
		trade.ID = orderResp.OrderID
		trade.Status = "filled"
	} else if e.clobClient == nil {
		log.Warn().Msg("⚠️ CLOB client not connected - trade simulated only")
		trade.Status = "simulated"
	} else {
		log.Warn().Str("direction", opp.Direction).Msg("⚠️ No token ID for direction - trade skipped")
		trade.Status = "skipped"
		return nil
	}

	// Track trade
	e.tradesMu.Lock()
	e.trades = append(e.trades, *trade)
	e.totalTrades++
	e.tradesMu.Unlock()

	// Add to open positions for exit monitoring (dual exit strategy)
	if trade.Status == "filled" || trade.Status == "simulated" {
		e.positionsMu.Lock()
		e.openPositions[trade.ID] = &OpenPosition{
			Trade:          trade,
			TokenID:        tokenID,
			Direction:      opp.Direction,
			EntryPrice:     opp.MarketOdds,
			Shares:         shares,
			BTCMoveAtEntry: opp.PriceChangePct.Div(decimal.NewFromInt(100)), // Convert back to decimal
			WindowEndTime:  opp.Window.EndDate,
			EnteredAt:      time.Now(),
		}
		e.positionsMu.Unlock()

		log.Debug().
			Str("trade_id", trade.ID).
			Str("exit_target", e.exitOddsThreshold.String()).
			Msg("📊 Position tracked for exit monitoring")
	}

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
