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
	binanceMulti    *binance.MultiClient // Multi-asset Binance (BTC, ETH, SOL)
	chainlinkClient *chainlink.Client // Chainlink price feed (what Polymarket uses for resolution)
	multiChainlink  *chainlink.MultiClient // Multi-asset Chainlink feeds (BTC, ETH, SOL)
	cmcClient       *cmc.Client       // CMC price feed (fast updates, same source as Data Streams!)
	windowScanner   *polymarket.WindowScanner
	wsClient        *polymarket.WSClient // Real-time WebSocket for odds
	clobClient      *CLOBClient // CLOB trading client
	asset           string      // Asset this engine tracks (BTC, ETH, SOL)

	// Window tracking
	windowStates map[string]*WindowState
	windowsMu    sync.RWMutex
	
	// Pre-captured prices for upcoming windows (captured at exact T=0)
	scheduledCaptures map[string]time.Time   // windowID -> scheduled capture time
	capturedPrices    map[string]decimal.Decimal // windowID -> captured price at T=0
	capturesMu        sync.RWMutex

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
	positionSize     decimal.Decimal // Base per-trade size in USD
	maxDailyTrades   int             // Max trades per day
	maxTradesPerWindow int           // Max trades per window
	cooldownSeconds  int             // Seconds between trades

	// Dynamic Position Sizing (trade bigger when more confident!)
	// Big move (>0.3%) → 3x base position
	// Medium move (0.2-0.3%) → 2x base position
	// Small move (0.1-0.2%) → 1x base position
	dynamicSizingEnabled bool
	smallMoveMultiplier  decimal.Decimal // 1x for 0.1-0.2% moves
	mediumMoveMultiplier decimal.Decimal // 2x for 0.2-0.3% moves
	largeMoveMultiplier  decimal.Decimal // 3x for >0.3% moves

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
		scheduledCaptures: make(map[string]time.Time),
		capturedPrices:   make(map[string]decimal.Decimal),
		openPositions:    make(map[string]*OpenPosition),
		trades:           make([]Trade, 0),
		totalProfit:      decimal.Zero,
		dailyPL:          decimal.Zero,
		lastDailyReset:   time.Now(),
		stopCh:           make(chan struct{}),

		// Default config - FEE-ADJUSTED strategy (with 3.15% Polymarket fee)
		// Buy at 25-65¢, exit at 75¢ OR stop-loss at -20% OR hold to resolution
		minPriceMove:       decimal.NewFromFloat(0.001),  // 0.10% min move (~$90 on BTC) - with accurate parallel snapshot
		minOddsForEntry:    decimal.NewFromFloat(0.25),   // 25 cents min (aggressive!)
		maxOddsForEntry:    decimal.NewFromFloat(0.65),   // 65 cents max
		minEdge:            decimal.NewFromFloat(0.10),   // 10% min edge
		positionSize:       decimal.NewFromFloat(100),    // $100 base per trade
		maxDailyTrades:     200,
		maxTradesPerWindow: 3,
		cooldownSeconds:    10,

		// Dynamic Position Sizing - trade BIGGER when more confident!
		dynamicSizingEnabled: true,
		smallMoveMultiplier:  decimal.NewFromFloat(1),    // 0.1-0.2% → 1x ($1)
		mediumMoveMultiplier: decimal.NewFromFloat(2),    // 0.2-0.3% → 2x ($2)
		largeMoveMultiplier:  decimal.NewFromFloat(3),    // >0.3%   → 3x ($3)

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
	if cfg.ArbMaxTradesPerWindow > 0 {
		e.maxTradesPerWindow = cfg.ArbMaxTradesPerWindow
	}
	if cfg.ArbMaxDailyTrades > 0 {
		e.maxDailyTrades = cfg.ArbMaxDailyTrades
	}
	if cfg.ArbCooldownSeconds > 0 {
		e.cooldownSeconds = cfg.ArbCooldownSeconds
	}
	if cfg.ArbMinPriceMove.GreaterThan(decimal.Zero) {
		e.minPriceMove = cfg.ArbMinPriceMove
	}
	if cfg.ArbMinEdge.GreaterThan(decimal.Zero) {
		e.minEdge = cfg.ArbMinEdge
	}

	return e
}

// SetCLOBClient sets the CLOB client for order execution
func (e *Engine) SetCLOBClient(client *CLOBClient) {
	e.clobClient = client
	log.Info().Str("asset", e.asset).Msg("🔗 CLOB client connected to engine")
}

// SetMultiChainlink sets the multi-asset Chainlink client
func (e *Engine) SetMultiChainlink(client *chainlink.MultiClient) {
	e.multiChainlink = client
	log.Info().Str("asset", e.asset).Msg("⛓️ Multi-asset Chainlink connected to engine")
}

// SetBinanceMulti sets the multi-asset Binance client
func (e *Engine) SetBinanceMulti(client *binance.MultiClient) {
	e.binanceMulti = client
	log.Info().Str("asset", e.asset).Msg("📈 Multi-asset Binance connected to engine")
}

// SetWSClient sets the Polymarket WebSocket client for real-time odds
func (e *Engine) SetWSClient(client *polymarket.WSClient) {
	e.wsClient = client
	log.Info().Str("asset", e.asset).Msg("📡 Polymarket WebSocket connected to engine")
}

// SetAsset sets the asset this engine tracks (BTC, ETH, SOL)
func (e *Engine) SetAsset(asset string) {
	e.asset = asset
}

// GetAsset returns the asset this engine tracks
func (e *Engine) GetAsset() string {
	if e.asset == "" {
		return "BTC" // Default
	}
	return e.asset
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
	
	// Scheduled price capture - capture Price to Beat at exact T=0
	go e.scheduledCaptureLoop()

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
	statusTicker := time.NewTicker(10 * time.Second) // Status update every 10s
	defer ticker.Stop()
	defer statusTicker.Stop()

	// Initial status after 2s startup
	go func() {
		time.Sleep(2 * time.Second)
		e.logStatus()
	}()

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

	asset := e.GetAsset()
	
	// Use CMC price for this asset (fastest, same source as Data Streams!)
	var currentPrice decimal.Decimal
	var priceSource string
	
	if e.cmcClient != nil && !e.cmcClient.IsAssetStale(asset) {
		currentPrice = e.cmcClient.GetAssetPrice(asset)
		priceSource = "CMC"
	}
	if currentPrice.IsZero() && e.chainlinkClient != nil && asset == "BTC" {
		currentPrice = e.chainlinkClient.GetCurrentPrice()
		priceSource = "CL"
	}
	if currentPrice.IsZero() && asset == "BTC" {
		currentPrice = e.binanceClient.GetCurrentPrice()
		priceSource = "BN"
	}

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

		// Determine status
		minMovePct := e.minPriceMove.Mul(decimal.NewFromInt(100))
		var status string
		
		if absChangePct.LessThan(minMovePct) {
			status = fmt.Sprintf("⏳ %.2f%% < %.1f%%", absChangePct.InexactFloat64(), minMovePct.InexactFloat64())
		} else if targetOdds.GreaterThan(e.maxOddsForEntry) {
			status = fmt.Sprintf("📈 %.0f¢>%.0f¢", targetOdds.Mul(decimal.NewFromInt(100)).InexactFloat64(), e.maxOddsForEntry.Mul(decimal.NewFromInt(100)).InexactFloat64())
		} else if targetOdds.LessThan(e.minOddsForEntry) {
			status = fmt.Sprintf("📉 %.0f¢<%.0f¢", targetOdds.Mul(decimal.NewFromInt(100)).InexactFloat64(), e.minOddsForEntry.Mul(decimal.NewFromInt(100)).InexactFloat64())
		} else {
			status = "🚀 READY!"
		}

		// Clean, compact log format
		log.Info().
			Str("asset", asset).
			Str("price", currentPrice.StringFixed(2)).
			Str("move", fmt.Sprintf("%s%.2f%%", direction[:1], absChangePct.InexactFloat64())).
			Str("odds", fmt.Sprintf("%.0f/%.0f", state.CurrentUpOdds.Mul(decimal.NewFromInt(100)).InexactFloat64(), state.CurrentDownOdds.Mul(decimal.NewFromInt(100)).InexactFloat64())).
			Str("src", priceSource).
			Str("status", status).
			Msg("📊 " + asset)
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

// scheduledCaptureLoop watches for upcoming windows and captures Price to Beat at EXACT T=0
// This eliminates the ~$100-150 error from late detection
func (e *Engine) scheduledCaptureLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			e.checkUpcomingWindows()
			e.executePendingCaptures()
		case <-e.stopCh:
			return
		}
	}
}

// checkUpcomingWindows looks for windows about to start and schedules price capture
func (e *Engine) checkUpcomingWindows() {
	windows := e.windowScanner.GetActiveWindows()
	now := time.Now()
	
	for _, w := range windows {
		// Look for windows starting in the next 30 seconds
		timeUntilStart := w.StartDate.Sub(now)
		
		if timeUntilStart > 0 && timeUntilStart <= 30*time.Second {
			e.capturesMu.Lock()
			if _, scheduled := e.scheduledCaptures[w.ID]; !scheduled {
				e.scheduledCaptures[w.ID] = w.StartDate
				log.Info().
					Str("asset", e.GetAsset()).
					Str("window", truncate(w.Question, 40)).
					Str("starts_in", timeUntilStart.Round(time.Second).String()).
					Msg("⏰ Scheduled price capture for upcoming window")
			}
			e.capturesMu.Unlock()
		}
	}
}

// executePendingCaptures checks if any scheduled captures should fire
func (e *Engine) executePendingCaptures() {
	now := time.Now()
	asset := e.GetAsset()
	
	e.capturesMu.Lock()
	defer e.capturesMu.Unlock()
	
	for windowID, captureTime := range e.scheduledCaptures {
		// Fire capture when we're within 1 second of the scheduled time
		timeDiff := now.Sub(captureTime)
		if timeDiff >= -500*time.Millisecond && timeDiff <= 2*time.Second {
			// CAPTURE NOW! Get price from all sources
			var cmcPrice, chainlinkPrice, binancePrice decimal.Decimal
			
			if e.cmcClient != nil {
				cmcPrice = e.cmcClient.GetAssetPrice(asset)
			}
			if e.multiChainlink != nil {
				chainlinkPrice = e.multiChainlink.GetPrice(asset)
			}
			if e.binanceMulti != nil {
				binancePrice = e.binanceMulti.GetPrice(asset)
			}
			
			// Use CMC as primary, average with Chainlink if both available
			var capturedPrice decimal.Decimal
			if !cmcPrice.IsZero() && !chainlinkPrice.IsZero() {
				// Average for accuracy
				capturedPrice = cmcPrice.Add(chainlinkPrice).Div(decimal.NewFromInt(2))
			} else if !cmcPrice.IsZero() {
				capturedPrice = cmcPrice
			} else if !chainlinkPrice.IsZero() {
				capturedPrice = chainlinkPrice
			} else if !binancePrice.IsZero() {
				capturedPrice = binancePrice
			}
			
			if !capturedPrice.IsZero() {
				e.capturedPrices[windowID] = capturedPrice
				log.Info().
					Str("asset", asset).
					Str("window_id", windowID).
					Str("price_to_beat", capturedPrice.StringFixed(2)).
					Str("cmc", cmcPrice.StringFixed(2)).
					Str("chainlink", chainlinkPrice.StringFixed(2)).
					Str("binance", binancePrice.StringFixed(2)).
					Msg("🎯 CAPTURED Price to Beat at T=0!")
			}
			
			// Remove from scheduled
			delete(e.scheduledCaptures, windowID)
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
// DYNAMIC EXIT STRATEGY based on current price movement:
//   A) DANGER ZONE: Force exit 90s before window end (HIGHEST PRIORITY!)
//   B) STOP-LOSS at -20% (risk management - CRITICAL!)
//   C) REVERSAL EXIT: Price reversed against us - exit immediately
//   D) MOMENTUM EXIT: Dynamic target based on current vs entry movement
//   E) MIN PROFIT EXIT: Take any profit if move is weakening
func (e *Engine) shouldExitPosition(pos *OpenPosition, odds *LiveOdds) (string, bool) {
	timeRemaining := time.Until(pos.WindowEndTime)

	// ⚠️ EXIT PATH A: DANGER ZONE - Force exit 90 seconds before end!
	dangerZoneSeconds := 90
	if timeRemaining.Seconds() > 0 && int(timeRemaining.Seconds()) <= dangerZoneSeconds {
		log.Warn().
			Str("position", pos.Trade.ID).
			Str("time_remaining", timeRemaining.Round(time.Second).String()).
			Msg("⚠️ DANGER ZONE - Force exit before resolution chaos!")
		return "danger_zone", true
	}

	// ⚠️ EXIT PATH B: STOP-LOSS at -20%
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

	// 📊 Get CURRENT price movement to compare with entry movement
	currentMove := e.getCurrentPriceMove(pos)
	entryMove := pos.BTCMoveAtEntry
	
	// ⚠️ EXIT PATH C: REVERSAL EXIT - Price reversed against our position!
	// If we bought UP but price is now DOWN (or vice versa), EXIT IMMEDIATELY
	if e.isMovementReversed(pos.Direction, currentMove) {
		log.Warn().
			Str("position", pos.Trade.ID).
			Str("direction", pos.Direction).
			Str("entry_move", entryMove.Mul(decimal.NewFromInt(100)).StringFixed(2)+"%").
			Str("current_move", currentMove.Mul(decimal.NewFromInt(100)).StringFixed(2)+"%").
			Msg("🔄 REVERSAL DETECTED - Exiting to avoid resolution loss!")
		return "reversal", true
	}

	// 📈 EXIT PATH D: DYNAMIC MOMENTUM-BASED EXIT
	// Calculate dynamic exit target based on move strength
	dynamicTarget := e.calculateDynamicExitTarget(pos, currentMove, entryMove)
	
	if odds.BestBid.GreaterThanOrEqual(dynamicTarget) {
		profit := odds.BestBid.Sub(pos.EntryPrice).Mul(pos.Shares)
		profitPct := odds.BestBid.Sub(pos.EntryPrice).Div(pos.EntryPrice).Mul(decimal.NewFromInt(100))

		log.Info().
			Str("position", pos.Trade.ID).
			Str("entry", pos.EntryPrice.String()).
			Str("exit", odds.BestBid.String()).
			Str("dynamic_target", dynamicTarget.String()).
			Str("profit", profit.String()).
			Str("profit_pct", profitPct.StringFixed(1)+"%").
			Str("current_move", currentMove.Mul(decimal.NewFromInt(100)).StringFixed(2)+"%").
			Msg("🎯 DYNAMIC EXIT - Target reached!")
		return "momentum_exit", true
	}

	// 💰 EXIT PATH E: TAKE ANY PROFIT if move is weakening significantly
	// If current move is less than 50% of entry move, take whatever profit we can
	moveRatio := decimal.Zero
	if !entryMove.IsZero() {
		moveRatio = currentMove.Div(entryMove)
	}
	
	minProfitThreshold := decimal.NewFromFloat(0.05) // 5% profit minimum
	profitPct := decimal.Zero
	if !pos.EntryPrice.IsZero() {
		profitPct = odds.BestBid.Sub(pos.EntryPrice).Div(pos.EntryPrice)
	}
	
	// Move weakened to <50% of entry AND we have at least 5% profit
	if moveRatio.LessThan(decimal.NewFromFloat(0.5)) && profitPct.GreaterThanOrEqual(minProfitThreshold) {
		profit := odds.BestBid.Sub(pos.EntryPrice).Mul(pos.Shares)
		log.Info().
			Str("position", pos.Trade.ID).
			Str("move_ratio", moveRatio.Mul(decimal.NewFromInt(100)).StringFixed(0)+"%").
			Str("profit", profit.String()).
			Str("profit_pct", profitPct.Mul(decimal.NewFromInt(100)).StringFixed(1)+"%").
			Msg("📉 WEAKENING MOVE - Taking available profit!")
		return "weak_move_exit", true
	}

	// Fallback: If less than 30 seconds remain and we're in profit, exit
	if timeRemaining < 30*time.Second && odds.BestBid.GreaterThan(pos.EntryPrice) {
		return "quick_flip", true
	}

	return "", false
}

// getCurrentPriceMove calculates current price movement from window start
func (e *Engine) getCurrentPriceMove(pos *OpenPosition) decimal.Decimal {
	// Get current price from CMC (fast source)
	currentPrice := decimal.Zero
	if e.cmcClient != nil {
		currentPrice = e.cmcClient.GetAssetPrice(e.asset)
	}
	if currentPrice.IsZero() && e.binanceClient != nil {
		currentPrice = e.binanceClient.GetCurrentPrice()
	}
	if currentPrice.IsZero() {
		return pos.BTCMoveAtEntry // Fallback to entry move
	}

	// Get window start price
	startPrice := decimal.Zero
	if pos.Trade != nil && !pos.Trade.BTCAtStart.IsZero() {
		startPrice = pos.Trade.BTCAtStart
	}
	if startPrice.IsZero() {
		return pos.BTCMoveAtEntry
	}

	// Calculate % change
	return currentPrice.Sub(startPrice).Div(startPrice)
}

// isMovementReversed checks if price has moved against our position
func (e *Engine) isMovementReversed(direction string, currentMove decimal.Decimal) bool {
	// Reversal threshold: 0.05% in wrong direction
	reversalThreshold := decimal.NewFromFloat(0.0005)
	
	if direction == "UP" {
		// We bet UP but price is now DOWN
		return currentMove.LessThan(reversalThreshold.Neg())
	} else {
		// We bet DOWN but price is now UP
		return currentMove.GreaterThan(reversalThreshold)
	}
}

// calculateDynamicExitTarget adjusts exit target based on momentum
// Strong move → higher target (wait for 80¢+)
// Weak move → lower target (take 55¢)
// Base: entry price + scaled profit based on move strength
func (e *Engine) calculateDynamicExitTarget(pos *OpenPosition, currentMove, entryMove decimal.Decimal) decimal.Decimal {
	// Base minimum: at least break even + 5%
	minTarget := pos.EntryPrice.Mul(decimal.NewFromFloat(1.05))
	
	// Maximum target: never wait for more than 85¢
	maxTarget := decimal.NewFromFloat(0.85)
	
	// Calculate move strength ratio
	moveRatio := decimal.NewFromFloat(1.0)
	if !entryMove.IsZero() {
		moveRatio = currentMove.Abs().Div(entryMove.Abs())
	}
	
	// Dynamic scaling:
	// - Move 2x stronger → target 80¢
	// - Move same → target 65¢  
	// - Move 50% weaker → target 55¢
	// - Move reversed → handled by reversal exit
	
	var target decimal.Decimal
	
	if moveRatio.GreaterThanOrEqual(decimal.NewFromFloat(2.0)) {
		// Move strengthened 2x+ → wait for 80¢
		target = decimal.NewFromFloat(0.80)
	} else if moveRatio.GreaterThanOrEqual(decimal.NewFromFloat(1.5)) {
		// Move strengthened 1.5x → wait for 75¢
		target = decimal.NewFromFloat(0.75)
	} else if moveRatio.GreaterThanOrEqual(decimal.NewFromFloat(1.0)) {
		// Move same or slightly stronger → 65¢
		target = decimal.NewFromFloat(0.65)
	} else if moveRatio.GreaterThanOrEqual(decimal.NewFromFloat(0.7)) {
		// Move slightly weaker → 58¢
		target = decimal.NewFromFloat(0.58)
	} else if moveRatio.GreaterThanOrEqual(decimal.NewFromFloat(0.5)) {
		// Move weakened to 50-70% → 52¢
		target = decimal.NewFromFloat(0.52)
	} else {
		// Move very weak → take any profit above entry+5%
		target = minTarget
	}
	
	// Ensure within bounds
	if target.LessThan(minTarget) {
		target = minTarget
	}
	if target.GreaterThan(maxTarget) {
		target = maxTarget
	}
	
	log.Debug().
		Str("position", pos.Trade.ID).
		Str("entry_move", entryMove.Mul(decimal.NewFromInt(100)).StringFixed(2)+"%").
		Str("current_move", currentMove.Mul(decimal.NewFromInt(100)).StringFixed(2)+"%").
		Str("move_ratio", moveRatio.StringFixed(2)+"x").
		Str("dynamic_target", target.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Msg("📊 Dynamic exit target calculated")
	
	return target
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
			// NEW WINDOW DETECTED - Get "Price to Beat"
			asset := e.GetAsset()
			windowAge := time.Since(w.StartDate)
			var startPrice decimal.Decimal
			var priceSource string
			var cmcPrice, chainlinkPrice, binancePrice decimal.Decimal
			
			// PRIORITY 0: Check if we pre-captured this price at EXACT T=0
			e.capturesMu.RLock()
			if capturedPrice, hasCaptured := e.capturedPrices[w.ID]; hasCaptured {
				startPrice = capturedPrice
				priceSource = "Pre-captured at T=0"
				log.Info().
					Str("asset", asset).
					Str("price_to_beat", startPrice.StringFixed(2)).
					Msg("✅ Using pre-captured Price to Beat (exact T=0)")
			}
			e.capturesMu.RUnlock()
			
			// If not pre-captured, use parallel snapshot
			if startPrice.IsZero() {
				// Collect parallel snapshots from ALL sources
				if e.cmcClient != nil {
					cmcPrice = e.cmcClient.GetAssetPrice(asset)
				}
				if e.multiChainlink != nil {
					chainlinkPrice = e.multiChainlink.GetPrice(asset)
				}
				// Use multi-asset Binance for all assets
				if e.binanceMulti != nil {
					binancePrice = e.binanceMulti.GetPrice(asset)
				} else if e.binanceClient != nil && asset == "BTC" {
					binancePrice = e.binanceClient.GetCurrentPrice()
				}
				
				// FRESH WINDOWS (<15s): Use CMC as primary, cross-validate with others
				if windowAge <= 15*time.Second {
					if !cmcPrice.IsZero() {
						startPrice = cmcPrice
						priceSource = "CMC (parallel snapshot)"
						
						// Log cross-validation for debugging
						log.Info().
							Str("asset", asset).
							Str("window_age", windowAge.Round(time.Second).String()).
							Str("cmc", cmcPrice.StringFixed(2)).
							Str("chainlink", chainlinkPrice.StringFixed(2)).
							Str("binance", binancePrice.StringFixed(2)).
							Str("price_to_beat", startPrice.StringFixed(2)).
							Msg("🎯 FRESH WINDOW - Parallel snapshot captured!")
					}
				} else if windowAge <= 30*time.Second {
					// SLIGHTLY STALE (15-30s): Still usable, use average for better accuracy
					if !cmcPrice.IsZero() && !chainlinkPrice.IsZero() {
						// Average of CMC and Chainlink for stability
						startPrice = cmcPrice.Add(chainlinkPrice).Div(decimal.NewFromInt(2))
						priceSource = "Avg(CMC+Chainlink)"
						log.Info().
							Str("asset", asset).
							Str("window_age", windowAge.Round(time.Second).String()).
							Str("price_to_beat", startPrice.StringFixed(2)).
							Msg("📊 Near-fresh window - using averaged price")
					} else if !cmcPrice.IsZero() {
						startPrice = cmcPrice
						priceSource = "CMC (near-fresh)"
					}
				} else {
					// TOO STALE (>30s): Skip - can't reliably know Price to Beat
					log.Debug().
						Str("asset", asset).
						Str("window_age", windowAge.Round(time.Second).String()).
						Str("window", truncate(w.Question, 40)).
						Msg("⏭️ Skipping stale window - Price to Beat unknown")
					continue
				}
			}
			
			// Fallback to Chainlink current if nothing else
			if startPrice.IsZero() && !chainlinkPrice.IsZero() {
				startPrice = chainlinkPrice
				priceSource = "Chainlink Current"
			}
			
			// Skip if we still don't have a start price
			if startPrice.IsZero() {
				log.Warn().
					Str("asset", asset).
					Str("window", truncate(w.Question, 40)).
					Msg("⚠️ Could not determine start price - skipping window")
				continue
			}
			
			e.windowStates[w.ID] = &WindowState{
				Window:     w,
				StartPrice: startPrice,
				StartTime:  w.StartDate,
			}
			
			if e.windowStates[w.ID].StartTime.IsZero() {
				e.windowStates[w.ID].StartTime = time.Now()
			}
			
			log.Info().
				Str("window", truncate(w.Question, 50)).
				Str("price_to_beat", startPrice.StringFixed(2)).
				Str("source", priceSource).
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
// Priority: 1) WebSocket (real-time), 2) CLOB API, 3) Scanner cache
func (e *Engine) refreshOdds() {
	e.windowsMu.Lock()
	defer e.windowsMu.Unlock()

	for _, state := range e.windowStates {
		// PRIORITY 1: WebSocket for real-time odds (sub-100ms updates!)
		if e.wsClient != nil && e.wsClient.IsConnected() && state.Window.YesTokenID != "" {
			upPrice, downPrice, ok := e.wsClient.GetMarketPrices(state.Window.YesTokenID, state.Window.NoTokenID)
			if ok && !upPrice.IsZero() {
				state.CurrentUpOdds = upPrice
				state.CurrentDownOdds = downPrice
				state.LastOddsCheck = time.Now()
				continue
			}
		}
		
		// PRIORITY 2: CLOB API direct fetch
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
		
		// PRIORITY 3: Scanner cache (HTTP polling)
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
	// 2. Chainlink on-chain - Updates every 20-60s, ~$1 diff from Data Streams (BTC only)
	// 3. Binance - Fast but ~$80 diff from Data Streams (direction mismatch!)
	var currentPrice decimal.Decimal
	asset := e.GetAsset()
	
	// Try CMC first (fastest + most accurate) - works for BTC, ETH, SOL
	if e.cmcClient != nil && !e.cmcClient.IsAssetStale(asset) {
		currentPrice = e.cmcClient.GetAssetPrice(asset)
	}
	// Fallback to Chainlink on-chain (BTC only)
	if currentPrice.IsZero() && e.chainlinkClient != nil && asset == "BTC" {
		currentPrice = e.chainlinkClient.GetCurrentPrice()
	}
	// Last resort: Binance (BTC only, less accurate)
	if currentPrice.IsZero() && asset == "BTC" {
		currentPrice = e.binanceClient.GetCurrentPrice()
	}
	if currentPrice.IsZero() {
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
		opp := e.analyzeWindow(state, currentPrice)
		if opp != nil {
			e.handleOpportunity(*opp, state)
		}
	}
}

// analyzeWindow checks if a window has a profitable opportunity
func (e *Engine) analyzeWindow(state *WindowState, currentPrice decimal.Decimal) *Opportunity {
	if state.StartPrice.IsZero() {
		return nil
	}

	// Calculate price change from window start
	priceChange := currentPrice.Sub(state.StartPrice)
	priceChangePct := priceChange.Div(state.StartPrice)

	// Check if move is significant enough
	absChangePct := priceChangePct.Abs()
	if absChangePct.LessThan(e.minPriceMove) {
		return nil // Move too small, no opportunity
	}
	
	// 🧠 PARALLEL MOMENTUM VALIDATION
	// Check if multiple sources agree on direction (reduces false signals)
	asset := e.GetAsset()
	var cmcPrice, chainlinkPrice decimal.Decimal
	if e.cmcClient != nil {
		cmcPrice = e.cmcClient.GetAssetPrice(asset)
	}
	if e.multiChainlink != nil {
		chainlinkPrice = e.multiChainlink.GetPrice(asset)
	}
	
	// Both sources must show same direction as our signal
	if !cmcPrice.IsZero() && !chainlinkPrice.IsZero() {
		cmcDirection := cmcPrice.Sub(state.StartPrice).Sign()
		chainlinkDirection := chainlinkPrice.Sub(state.StartPrice).Sign()
		ourDirection := priceChange.Sign()
		
		// If CMC and Chainlink disagree, skip (noise/lag, not real move)
		if cmcDirection != chainlinkDirection {
			log.Debug().
				Str("cmc_dir", directionStr(cmcDirection)).
				Str("chainlink_dir", directionStr(chainlinkDirection)).
				Msg("⏭️ Sources disagree - skipping noisy signal")
			return nil
		}
		
		// If our signal doesn't match confirmed direction, skip
		if cmcDirection != ourDirection {
			log.Debug().
				Str("signal_dir", directionStr(ourDirection)).
				Str("confirmed_dir", directionStr(cmcDirection)).
				Msg("⏭️ Signal doesn't match confirmed momentum")
			return nil
		}
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
		CurrentBTC:     currentPrice,
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
		log.Debug().
			Int("total", e.totalTrades).
			Int("max", e.maxDailyTrades).
			Msg("❌ Trade blocked: daily limit reached")
		return false
	}

	// Check per-window limit
	if state.TradesThisWindow >= e.maxTradesPerWindow {
		log.Debug().
			Int("trades", state.TradesThisWindow).
			Int("max", e.maxTradesPerWindow).
			Msg("❌ Trade blocked: per-window limit")
		return false
	}

	// Check cooldown
	timeSince := time.Since(state.LastTradeTime)
	cooldown := time.Duration(e.cooldownSeconds) * time.Second
	if timeSince < cooldown {
		log.Debug().
			Dur("since", timeSince).
			Dur("cooldown", cooldown).
			Msg("❌ Trade blocked: cooldown active")
		return false
	}

	log.Info().
		Str("asset", e.GetAsset()).
		Int("daily_trades", e.totalTrades).
		Int("window_trades", state.TradesThisWindow).
		Msg("✅ Trade checks passed - proceeding to execute")

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

	// Check if tokenID is empty
	if tokenID == "" {
		log.Error().
			Str("direction", opp.Direction).
			Str("yes_token", opp.Window.YesTokenID).
			Str("no_token", opp.Window.NoTokenID).
			Str("window", opp.Window.Question).
			Msg("❌ Token ID is empty - cannot trade")
		return nil
	}

	// Safety check for zero odds
	if opp.MarketOdds.IsZero() || opp.MarketOdds.LessThanOrEqual(decimal.Zero) {
		log.Error().Msg("Cannot execute trade: MarketOdds is zero")
		return nil
	}

	// 🚀 DYNAMIC POSITION SIZING - Trade bigger when more confident!
	// Based on BTC move size:
	//   >0.3% move → 3x base (high confidence)
	//   0.2-0.3%   → 2x base (medium confidence)
	//   0.1-0.2%   → 1x base (low confidence)
	tradeAmount := e.positionSize
	moveAbs := opp.PriceChangePct.Abs() // Already in % form (e.g., 0.25 = 0.25%)
	sizeMultiplier := "1x"
	
	if e.dynamicSizingEnabled {
		if moveAbs.GreaterThanOrEqual(decimal.NewFromFloat(0.3)) {
			// Large move (>0.3%) → 3x
			tradeAmount = e.positionSize.Mul(e.largeMoveMultiplier)
			sizeMultiplier = "3x"
		} else if moveAbs.GreaterThanOrEqual(decimal.NewFromFloat(0.2)) {
			// Medium move (0.2-0.3%) → 2x
			tradeAmount = e.positionSize.Mul(e.mediumMoveMultiplier)
			sizeMultiplier = "2x"
		} else {
			// Small move (0.1-0.2%) → 1x
			tradeAmount = e.positionSize.Mul(e.smallMoveMultiplier)
			sizeMultiplier = "1x"
		}
	}

	// Calculate size: $amount / price = shares
	// E.g., $3 / 0.45 = 6.67 shares
	shares := tradeAmount.Div(opp.MarketOdds).Round(2)

	trade := &Trade{
		ID:             fmt.Sprintf("arb_%d", time.Now().UnixNano()),
		WindowID:       opp.Window.ID,
		Question:       opp.Window.Question,
		Direction:      opp.Direction,
		TokenID:        tokenID,
		EntryPrice:     opp.MarketOdds,
		Amount:         tradeAmount,
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
		Str("sizing", sizeMultiplier).
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

// directionStr converts sign to readable string
func directionStr(sign int) string {
	switch sign {
	case 1:
		return "UP"
	case -1:
		return "DOWN"
	default:
		return "FLAT"
	}
}
