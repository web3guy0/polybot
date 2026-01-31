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
// PHASE-BASED FADE SCALPER - 15-Minute Market Specialist
// ═══════════════════════════════════════════════════════════════════════════════
//
// CORE INSIGHT: Momentum chasing = adverse selection on Polymarket
//               Instead, we FADE overreactions during specific phases
//
// PHASES:
//   0-3 min   → OPENING: Fade initial overreactions (humans panic in)
//   3-12 min  → DEAD ZONE: NO TRADING (bots dominate, spread tight)
//   12-14 min → CLOSING: Fade panic moves (humans panic out)
//   14-15 min → FLAT: Force close everything, NO NEW TRADES
//
// STRATEGY:
//   - Detect sharp moves (≥5¢ in 30 seconds)
//   - FADE the move (buy opposite side)
//   - Exit on: TP (+2-3¢), Timeout (15s), or Phase End
//   - NO stop loss (gets hunted in thin markets)
//
// RULES (NON-NEGOTIABLE):
//   ❌ NO trading after 14:00 remaining
//   ❌ NO holding into resolution  
//   ❌ NO averaging down
//   ❌ NO momentum following
//   ❌ NO trading in dead zone
//
// ═══════════════════════════════════════════════════════════════════════════════

// ═══════════════════════════════════════════════════════════════════════════════
// INTERFACES (to break circular dependencies)
// ═══════════════════════════════════════════════════════════════════════════════

// TradeApprover interface for risk gate (breaks risk → strategy cycle)
type TradeApprover interface {
	CanEnter(req TradeApprovalRequest) TradeApprovalResponse
	RecordExit(asset string, pnl decimal.Decimal)
	IsDailyLimitHit() bool
	IsCircuitTripped() bool
	IsAssetDisabled(asset string) bool
}

// TradeApprovalRequest for risk gate
type TradeApprovalRequest struct {
	Asset    string
	Side     string
	Action   string
	Price    decimal.Decimal
	Size     decimal.Decimal
	Strategy string
	Phase    string
	Reason   string
	Metadata map[string]any
}

// TradeApprovalResponse from risk gate
type TradeApprovalResponse struct {
	Approved     bool
	AdjustedSize decimal.Decimal
	RejectionMsg string
	RiskScore    float64
}

// PositionPersister interface for reconciler (breaks execution → strategy cycle)
type PositionPersister interface {
	PersistPosition(pos *PersistablePosition) error
	RemovePosition(id string) error
}

// PersistablePosition for reconciler
type PersistablePosition struct {
	ID       string
	MarketID string
	TokenID  string
	Asset    string
	Side     string
	Size     decimal.Decimal
	AvgEntry decimal.Decimal
	Strategy string
	OpenedAt time.Time
	Metadata map[string]any
}

// ═══════════════════════════════════════════════════════════════════════════════

// MarketPhase represents the current phase of a 15-minute window
type MarketPhase int

const (
	PhaseOpening    MarketPhase = iota // 0-3 min: Trade opening overreactions
	PhaseDeadZone                      // 3-12 min: NO TRADING
	PhaseClosing                       // 12-14 min: Trade closing panic
	PhaseFlat                          // 14-15 min: FORCE FLAT, NO TRADES
	PhaseResolution                    // <1 min: DANGER - should never be here
)

func (p MarketPhase) String() string {
	switch p {
	case PhaseOpening:
		return "OPENING"
	case PhaseDeadZone:
		return "DEAD_ZONE"
	case PhaseClosing:
		return "CLOSING"
	case PhaseFlat:
		return "FLAT"
	case PhaseResolution:
		return "RESOLUTION"
	default:
		return "UNKNOWN"
	}
}

// PhaseScalper implements phase-aware fade scalping
type PhaseScalper struct {
	mu      sync.RWMutex
	enabled bool

	// Dependencies
	windowScanner *feeds.WindowScanner
	polyFeed      *feeds.PolymarketFeed
	
	// NEW: Professional execution & risk layers (optional, use interfaces)
	riskGate   TradeApprover     // Risk approval interface
	persister  PositionPersister // Position persistence interface

	// Config
	scanIntervalMs int // 50ms scanning

	// Phase boundaries (seconds remaining)
	openingEnd  int // 720 = 12 min remaining (first 3 min)
	deadZoneEnd int // 180 = 3 min remaining (3-12 min is dead)
	closingEnd  int // 60 = 1 min remaining (12-14 min is closing)
	flatEnd     int // 60 = absolute cutoff

	// Fade thresholds
	openingFadeThreshold decimal.Decimal // Min move to fade in opening (6¢)
	closingFadeThreshold decimal.Decimal // Min move to fade in closing (4¢)
	takeProfitCents      decimal.Decimal // TP target (+2-3¢)
	maxTradeTimeout      time.Duration   // Max hold time (15s)

	// Position tracking
	activePositions map[string]*FadePosition
	priceHistoryYES map[string][]PriceTick // Track YES prices per asset
	priceHistoryNO  map[string][]PriceTick // Track NO prices per asset
	windowEndTime   map[string]time.Time   // Track window boundary per asset

	// Impulse quality tracking PER SIDE (YES and NO independently)
	consecutiveMovesYES map[string]int    // Consecutive moves on YES side
	consecutiveMovesNO  map[string]int    // Consecutive moves on NO side
	lastMoveDirYES      map[string]string // Last move direction for YES
	lastMoveDirNO       map[string]string // Last move direction for NO

	// Entry lock to prevent double-trigger in same scan tick
	entryInProgress map[string]bool      // Per-asset entry lock
	entryLockTime   map[string]time.Time // When lock was set

	// Paper trading
	paperMode    bool
	paperBalance decimal.Decimal
	paperTrades  []PaperTrade

	// Stats
	totalTrades   int
	winningTrades int
	losingTrades  int
	totalProfit   decimal.Decimal
	
	// Kill switches
	firstTradeLost    map[string]bool // Per-market kill switch
	marketDisabled    map[string]bool // Disabled markets
	marketLossCount   map[string]int  // Loss count per market (disable at 2)
	dailyLossHit      bool
	dailyPnL          decimal.Decimal
	startingBalance   decimal.Decimal // For dynamic loss limit calculation

	// Cooldowns
	lastTradeExit map[string]time.Time // Per-asset cooldown after exit
	tradeCooldown time.Duration        // Wait time after exit before new entry

	// Execution state machine (for realistic fill simulation)
	pendingOrders    map[string]*PendingOrder // Orders waiting for fill
	fillTimeoutMs    int                      // Max wait for fill (500ms)
	maxRetries       int                      // Max retry attempts (1)

	// ORDER SIZING - CRITICAL FOR LIVE TRADING
	marketOrderValue decimal.Decimal // Value in $ for market orders (default $1.1)
	limitOrderShares decimal.Decimal // Number of shares for limit orders (default 5)

	// Debug
	lastPhaseLog map[string]time.Time
}

// PendingOrder represents an order waiting for fill (execution state machine)
type PendingOrder struct {
	Asset       string
	Side        string          // "YES" or "NO"
	TokenID     string
	Price       decimal.Decimal // Limit price
	Shares      decimal.Decimal
	SubmitTime  time.Time
	State       string // "PENDING", "FILLED", "CANCELLED", "TIMEOUT"
	RetryCount  int
	Phase       MarketPhase
	MarketID    string
	FadedMove   string // Direction we're fading
}

// FadePosition represents an open fade position
type FadePosition struct {
	MarketID    string
	TokenID     string
	Asset       string
	Side        string          // "YES" or "NO" - we bought this side
	FadedMove   string          // "UP" or "DOWN" - the move we faded
	EntryPrice  decimal.Decimal
	EntryTime   time.Time
	Shares      decimal.Decimal // Actual shares bought
	TargetPrice decimal.Decimal
	TimeoutAt   time.Time       // Hard timeout
	IsPaper     bool
	Phase       MarketPhase     // Phase when entered
}

// PriceTick represents a single price observation
type PriceTick struct {
	Price     decimal.Decimal
	Timestamp time.Time
}

// PaperTrade represents a simulated trade for backtesting/paper mode
type PaperTrade struct {
	MarketID   string
	Asset      string
	Side       string          // "YES" or "NO"
	Direction  string          // "BUY" or "SELL"
	EntryPrice decimal.Decimal
	ExitPrice  decimal.Decimal
	Shares     decimal.Decimal
	PnL        decimal.Decimal
	EntryTime  time.Time
	ExitTime   time.Time
	Reason     string          // "TP", "TIMEOUT", "PHASE_END", etc.
	Phase      string          // Phase when trade was taken
}

// NewPhaseScalper creates the phase-based fade scalper
func NewPhaseScalper(polyFeed *feeds.PolymarketFeed, scanner *feeds.WindowScanner, paperMode bool) *PhaseScalper {
	// Load order sizes from env (CRITICAL FOR LIVE TRADING)
	marketOrderValue := decimal.NewFromFloat(1.1) // Default $1.1 for market orders
	if v := os.Getenv("MARKET_ORDER_VALUE"); v != "" {
		if val, err := strconv.ParseFloat(v, 64); err == nil {
			marketOrderValue = decimal.NewFromFloat(val)
		}
	}
	
	limitOrderShares := decimal.NewFromFloat(5) // Default 5 shares for limit orders
	if v := os.Getenv("LIMIT_ORDER_SHARES"); v != "" {
		if val, err := strconv.ParseFloat(v, 64); err == nil {
			limitOrderShares = decimal.NewFromFloat(val)
		}
	}
	
	initialBalance := decimal.NewFromFloat(100) // Default $100
	if v := os.Getenv("INITIAL_BALANCE"); v != "" {
		if val, err := strconv.ParseFloat(v, 64); err == nil {
			initialBalance = decimal.NewFromFloat(val)
		}
	}

	s := &PhaseScalper{
		enabled:        true,
		windowScanner:  scanner,
		polyFeed:       polyFeed,
		scanIntervalMs: 50,

		// Phase boundaries (seconds remaining in 15-min window)
		openingEnd:  720, // After 3 min, 12 min remain
		deadZoneEnd: 180, // After 12 min, 3 min remain  
		closingEnd:  60,  // After 14 min, 1 min remain
		flatEnd:     60,  // Absolute cutoff

		// Thresholds
		openingFadeThreshold: decimal.NewFromFloat(0.06), // 6¢ move to fade
		closingFadeThreshold: decimal.NewFromFloat(0.04), // 4¢ move to fade
		takeProfitCents:      decimal.NewFromFloat(0.025), // +2.5¢ target
		maxTradeTimeout:      15 * time.Second,

		// State
		activePositions:     make(map[string]*FadePosition),
		priceHistoryYES:     make(map[string][]PriceTick),
		priceHistoryNO:      make(map[string][]PriceTick),
		windowEndTime:       make(map[string]time.Time),
		consecutiveMovesYES: make(map[string]int),
		consecutiveMovesNO:  make(map[string]int),
		lastMoveDirYES:      make(map[string]string),
		lastMoveDirNO:       make(map[string]string),
		entryInProgress:     make(map[string]bool),
		entryLockTime:       make(map[string]time.Time),
		
		// Kill switches
		firstTradeLost:  make(map[string]bool),
		marketDisabled:  make(map[string]bool),
		marketLossCount: make(map[string]int),
		startingBalance: initialBalance,

		// Cooldowns
		lastTradeExit: make(map[string]time.Time),
		tradeCooldown: 30 * time.Second, // Wait 30s after exit before re-entry

		// Execution state machine
		pendingOrders: make(map[string]*PendingOrder),
		fillTimeoutMs: 500, // 500ms fill timeout
		maxRetries:    1,   // Retry once max
		
		// ORDER SIZING - CRITICAL FOR LIVE TRADING
		marketOrderValue: marketOrderValue, // $1.1 for market orders
		limitOrderShares: limitOrderShares, // 5 shares for limit orders

		// Paper trading
		paperMode:    paperMode,
		paperBalance: initialBalance,
		paperTrades:  make([]PaperTrade, 0),
		
		// Debug
		lastPhaseLog: make(map[string]time.Time),
	}

	mode := "PAPER"
	if !paperMode {
		mode = "LIVE"
	}

	log.Info().
		Str("mode", mode).
		Int("scan_ms", s.scanIntervalMs).
		Str("market_order_value", marketOrderValue.StringFixed(2)).
		Str("limit_order_shares", limitOrderShares.StringFixed(0)).
		Str("opening_fade", s.openingFadeThreshold.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Str("closing_fade", s.closingFadeThreshold.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Str("take_profit", s.takeProfitCents.Mul(decimal.NewFromInt(100)).StringFixed(1)+"¢").
		Dur("timeout", s.maxTradeTimeout).
		Msg("🎯 Phase-Based Fade Scalper initialized")

	return s
}

func (s *PhaseScalper) Name() string  { return "PhaseScalper" }
func (s *PhaseScalper) Enabled() bool { return s.enabled }

// Start begins the scalper loops
func (s *PhaseScalper) Start() {
	go s.scanLoop()
	go s.exitLoop()
	log.Info().Msg("🚀 Phase Scalper started")
}

// Stop gracefully shuts down the scalper
func (s *PhaseScalper) Stop() {
	s.mu.Lock()
	s.enabled = false
	s.mu.Unlock()
	
	// Force close any open positions on shutdown
	s.forceCloseAllPositions()
	
	log.Info().Msg("🛑 Phase Scalper stopped")
}

// SetRiskGate sets the centralized risk approval (optional, for pro mode)
func (s *PhaseScalper) SetRiskGate(rg TradeApprover) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.riskGate = rg
	log.Info().Msg("🛡️ Risk Gate attached to Phase Scalper")
}

// SetPersister sets the position persister (optional, for persistence)
func (s *PhaseScalper) SetPersister(p PositionPersister) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persister = p
	log.Info().Msg("📦 Persister attached to Phase Scalper")
}

// SetBalance updates the balance (used for LIVE mode to sync with exchange)
func (s *PhaseScalper) SetBalance(balance decimal.Decimal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paperBalance = balance
	s.startingBalance = balance
	log.Info().Str("balance", balance.StringFixed(2)).Msg("💰 Phase Scalper balance synced")
}

// forceCloseAllPositions closes all open positions (for shutdown)
func (s *PhaseScalper) forceCloseAllPositions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	if len(s.activePositions) == 0 {
		return
	}
	
	log.Warn().Int("count", len(s.activePositions)).Msg("🚨 Force closing positions on shutdown")
	
	windows := s.windowScanner.GetActiveWindows()
	
	for key, pos := range s.activePositions {
		var exitPrice decimal.Decimal
		
		// Try to get current price
		for _, w := range windows {
			if w.Asset == pos.Asset {
				if pos.Side == "YES" {
					exitPrice = w.YesPrice
				} else {
					exitPrice = w.NoPrice
				}
				break
			}
		}
		
		if exitPrice.IsZero() {
			exitPrice = pos.EntryPrice // Fallback
		}
		
		// Record the exit
		pnl := exitPrice.Sub(pos.EntryPrice).Mul(pos.Shares)
		
		log.Warn().
			Str("asset", pos.Asset).
			Str("side", pos.Side).
			Str("pnl", pnl.StringFixed(2)).
			Msg("🚨 Position force closed on shutdown")
		
		// Update risk gate if attached
		if s.riskGate != nil {
			s.riskGate.RecordExit(pos.Asset, pnl)
		}
		
		// Remove from persistence if attached
		if s.persister != nil {
			_ = s.persister.RemovePosition(pos.MarketID)
		}
		
		delete(s.activePositions, key)
	}
}

// GetOpenPositionCount returns count of open positions
func (s *PhaseScalper) GetOpenPositionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.activePositions)
}

// scanLoop monitors for fade opportunities
func (s *PhaseScalper) scanLoop() {
	ticker := time.NewTicker(time.Duration(s.scanIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		if !s.Enabled() {
			return
		}
		s.scan()
	}
}

// exitLoop monitors positions for exits
func (s *PhaseScalper) exitLoop() {
	ticker := time.NewTicker(time.Duration(s.scanIntervalMs) * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		if !s.Enabled() {
			return
		}
		s.checkExits()
	}
}

// getPhase determines current market phase based on time remaining
func (s *PhaseScalper) getPhase(secondsRemaining float64) MarketPhase {
	switch {
	case secondsRemaining > float64(s.openingEnd):
		return PhaseOpening // First 3 minutes
	case secondsRemaining > float64(s.deadZoneEnd):
		return PhaseDeadZone // Minutes 3-12
	case secondsRemaining > float64(s.closingEnd):
		return PhaseClosing // Minutes 12-14
	case secondsRemaining > 0:
		return PhaseFlat // Last minute
	default:
		return PhaseResolution
	}
}

// scan looks for fade opportunities
func (s *PhaseScalper) scan() {
	// Check daily loss limit - use RiskGate if available
	if s.riskGate != nil {
		if s.riskGate.IsDailyLimitHit() || s.riskGate.IsCircuitTripped() {
			return
		}
	} else if s.dailyLossHit {
		return
	}

	windows := s.windowScanner.GetActiveWindows()

	for _, w := range windows {
		// Skip disabled markets - check RiskGate first
		if s.riskGate != nil {
			if s.riskGate.IsAssetDisabled(w.Asset) {
				continue
			}
		} else if s.marketDisabled[w.Asset] {
			continue
		}

		timeRemaining := w.TimeRemainingSeconds()
		phase := s.getPhase(timeRemaining)

		// Log phase changes (once per 30 seconds per market)
		s.logPhaseIfChanged(w.Asset, phase, timeRemaining)

		// CRITICAL: Only trade in specific phases
		switch phase {
		case PhaseOpening:
			s.scanOpeningPhase(w)
		case PhaseDeadZone:
			// ❌ NO TRADING - this is where most bots die
			continue
		case PhaseClosing:
			s.scanClosingPhase(w)
		case PhaseFlat, PhaseResolution:
			// ❌ NO NEW TRADES - force exit handled in exitLoop
			continue
		}
	}
}

// logPhaseIfChanged logs phase transitions
func (s *PhaseScalper) logPhaseIfChanged(asset string, phase MarketPhase, timeRemaining float64) {
	now := time.Now()
	lastLog, exists := s.lastPhaseLog[asset]
	
	if !exists || now.Sub(lastLog) > 30*time.Second {
		s.lastPhaseLog[asset] = now
		
		emoji := "⏳"
		switch phase {
		case PhaseOpening:
			emoji = "🟢"
		case PhaseDeadZone:
			emoji = "🔴"
		case PhaseClosing:
			emoji = "🟡"
		case PhaseFlat:
			emoji = "⚫"
		}
		
		log.Info().
			Str("asset", asset).
			Str("phase", phase.String()).
			Float64("time_remaining", timeRemaining).
			Msgf("%s Phase: %s (%.0fs left)", emoji, phase.String(), timeRemaining)
	}
}

// scanOpeningPhase looks for opening overreaction fades
func (s *PhaseScalper) scanOpeningPhase(w *feeds.Window) {
	// Track price history (both YES and NO)
	s.recordPrice(w)

	// Check if market is disabled
	s.mu.RLock()
	disabled := s.marketDisabled[w.Asset]
	entryLocked := s.entryInProgress[w.Asset]
	s.mu.RUnlock()
	if disabled {
		return
	}
	
	// Check entry lock (prevents double-trigger in same scan tick)
	if entryLocked {
		return
	}

	// Already have position on this market?
	if s.hasPosition(w.Asset) {
		return
	}

	// Check BOTH sides for moves and fade the one that moved
	// This ensures we trade the correct instrument
	yesMove := s.detectSharpMove(w.Asset, "YES", 30*time.Second)
	noMove := s.detectSharpMove(w.Asset, "NO", 30*time.Second)
	
	// Pick the larger move (if any meet threshold)
	var move *SharpMove
	if yesMove != nil && yesMove.magnitude.GreaterThanOrEqual(s.openingFadeThreshold) {
		move = yesMove
	}
	if noMove != nil && noMove.magnitude.GreaterThanOrEqual(s.openingFadeThreshold) {
		if move == nil || noMove.magnitude.GreaterThan(move.magnitude) {
			move = noMove
		}
	}
	
	if move == nil {
		return
	}

	// Validate: move must be recent (within lookback window)
	if move.duration > 35*time.Second {
		log.Debug().Str("asset", w.Asset).Dur("duration", move.duration).Msg("⏰ Move too old")
		return
	}

	log.Info().
		Str("asset", w.Asset).
		Str("side", move.side).
		Str("direction", move.direction).
		Str("magnitude", move.magnitude.Mul(decimal.NewFromInt(100)).StringFixed(1)+"¢").
		Dur("duration", move.duration).
		Msg("📊 Opening overreaction detected")

	// FADE the move
	s.executeFade(w, move, PhaseOpening)
}

// scanClosingPhase looks for closing panic fades
func (s *PhaseScalper) scanClosingPhase(w *feeds.Window) {
	// Track price history (both YES and NO)
	s.recordPrice(w)

	// Check if market is disabled
	s.mu.RLock()
	disabled := s.marketDisabled[w.Asset]
	entryLocked := s.entryInProgress[w.Asset]
	s.mu.RUnlock()
	if disabled {
		return
	}
	
	// Check entry lock (prevents double-trigger in same scan tick)
	if entryLocked {
		return
	}

	// Already have position?
	if s.hasPosition(w.Asset) {
		return
	}

	// Check BOTH sides for panic moves
	yesMove := s.detectSharpMove(w.Asset, "YES", 20*time.Second)
	noMove := s.detectSharpMove(w.Asset, "NO", 20*time.Second)
	
	// Pick the larger move (if any meet threshold)
	var move *SharpMove
	if yesMove != nil && yesMove.magnitude.GreaterThanOrEqual(s.closingFadeThreshold) {
		move = yesMove
	}
	if noMove != nil && noMove.magnitude.GreaterThanOrEqual(s.closingFadeThreshold) {
		if move == nil || noMove.magnitude.GreaterThan(move.magnitude) {
			move = noMove
		}
	}
	
	if move == nil {
		return
	}

	// Validate: move must be recent
	if move.duration > 25*time.Second {
		log.Debug().Str("asset", w.Asset).Dur("duration", move.duration).Msg("⏰ Move too old")
		return
	}

	log.Info().
		Str("asset", w.Asset).
		Str("side", move.side).
		Str("direction", move.direction).
		Str("magnitude", move.magnitude.Mul(decimal.NewFromInt(100)).StringFixed(1)+"¢").
		Dur("duration", move.duration).
		Msg("🔥 Closing panic detected")

	// FADE the panic
	s.executeFade(w, move, PhaseClosing)
}

// SharpMove represents a detected sharp price move
type SharpMove struct {
	direction string          // "UP" or "DOWN"
	magnitude decimal.Decimal // Absolute change in price (e.g., 0.06 = 6¢)
	duration  time.Duration
	startPrice decimal.Decimal
	endPrice   decimal.Decimal
	side       string // "YES" or "NO" - which side had the move
}

// detectSharpMove finds sharp moves in recent price history for a specific side
func (s *PhaseScalper) detectSharpMove(asset string, side string, lookback time.Duration) *SharpMove {
	s.mu.RLock()
	var history []PriceTick
	var consecutiveMoves int
	if side == "YES" {
		history = s.priceHistoryYES[asset]
		consecutiveMoves = s.consecutiveMovesYES[asset] // Use YES-specific impulse
	} else {
		history = s.priceHistoryNO[asset]
		consecutiveMoves = s.consecutiveMovesNO[asset] // Use NO-specific impulse
	}
	s.mu.RUnlock()

	if len(history) < 2 {
		return nil // Need at least 2 ticks to measure a move
	}

	now := time.Now()
	cutoff := now.Add(-lookback)

	// Find oldest price within lookback window
	var oldestInWindow *PriceTick
	var oldestIdx int
	for i := 0; i < len(history); i++ {
		if history[i].Timestamp.After(cutoff) {
			oldestInWindow = &history[i]
			oldestIdx = i
			break
		}
	}

	// If no ticks within lookback, no recent data
	if oldestInWindow == nil {
		return nil
	}

	// Get current price (last in history)
	current := history[len(history)-1]
	
	// Calculate move within the lookback window only
	change := current.Price.Sub(oldestInWindow.Price)
	magnitude := change.Abs()
	
	direction := "UP"
	if change.IsNegative() {
		direction = "DOWN"
	}
	
	duration := current.Timestamp.Sub(oldestInWindow.Timestamp)

	// IMPULSE QUALITY FILTER: Require at least 2 consecutive moves in same direction
	// This filters out single-tick noise - USING SIDE-SPECIFIC COUNTER
	if consecutiveMoves < 2 {
		return nil // Not enough impulse quality
	}

	// Debug: log tick details when there's movement
	if magnitude.GreaterThan(decimal.Zero) {
		log.Debug().
			Str("asset", asset).
			Str("side", side).
			Int("oldest_idx", oldestIdx).
			Int("history_len", len(history)).
			Str("oldest_price", oldestInWindow.Price.StringFixed(4)).
			Str("current_price", current.Price.StringFixed(4)).
			Str("magnitude", magnitude.Mul(decimal.NewFromInt(100)).StringFixed(2)+"¢").
			Int("consecutive_moves", consecutiveMoves).
			Dur("lookback", lookback).
			Dur("actual_duration", duration).
			Msg("📈 Move within window")
	}

	return &SharpMove{
		direction:  direction,
		magnitude:  magnitude,
		duration:   duration,
		startPrice: oldestInWindow.Price,
		endPrice:   current.Price,
		side:       side,
	}
}

// recordPrice records current prices for an asset (both YES and NO)
func (s *PhaseScalper) recordPrice(w *feeds.Window) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	
	// Create ticks for BOTH sides
	yesTick := PriceTick{Price: w.YesPrice, Timestamp: now}
	noTick := PriceTick{Price: w.NoPrice, Timestamp: now}

	historyYES := s.priceHistoryYES[w.Asset]
	historyNO := s.priceHistoryNO[w.Asset]

	// Check if window changed - if so, clear BOTH histories
	currentWindowEnd := w.EndTime
	if lastWindowEnd, exists := s.windowEndTime[w.Asset]; exists {
		if !currentWindowEnd.Equal(lastWindowEnd) {
			// New window! Clear old history and reset ALL impulse tracking
			log.Info().
				Str("asset", w.Asset).
				Time("old_window", lastWindowEnd).
				Time("new_window", currentWindowEnd).
				Msg("🔄 Window changed - clearing price history")
			historyYES = nil
			historyNO = nil
			// Reset impulse tracking for BOTH sides
			s.consecutiveMovesYES[w.Asset] = 0
			s.consecutiveMovesNO[w.Asset] = 0
			s.lastMoveDirYES[w.Asset] = ""
			s.lastMoveDirNO[w.Asset] = ""
			// Clear entry lock on window change
			s.entryInProgress[w.Asset] = false
		}
	}
	s.windowEndTime[w.Asset] = currentWindowEnd

	// Track consecutive moves for YES side
	if len(historyYES) > 0 {
		lastPrice := historyYES[len(historyYES)-1].Price
		if !yesTick.Price.Equal(lastPrice) {
			change := yesTick.Price.Sub(lastPrice)
			changeDir := "UP"
			if change.IsNegative() {
				changeDir = "DOWN"
			}
			
			// Track consecutive moves in same direction for YES
			if s.lastMoveDirYES[w.Asset] == changeDir {
				s.consecutiveMovesYES[w.Asset]++
			} else {
				s.consecutiveMovesYES[w.Asset] = 1
				s.lastMoveDirYES[w.Asset] = changeDir
			}
			
			changeCents := change.Mul(decimal.NewFromInt(100))
			log.Debug().
				Str("asset", w.Asset).
				Str("side", "YES").
				Str("old", lastPrice.StringFixed(4)).
				Str("new", yesTick.Price.StringFixed(4)).
				Str("change", changeCents.StringFixed(2)+"¢").
				Int("consecutive", s.consecutiveMovesYES[w.Asset]).
				Str("direction", changeDir).
				Msg("💱 Odds tick")
		}
	}

	// Track consecutive moves for NO side
	if len(historyNO) > 0 {
		lastPrice := historyNO[len(historyNO)-1].Price
		if !noTick.Price.Equal(lastPrice) {
			change := noTick.Price.Sub(lastPrice)
			changeDir := "UP"
			if change.IsNegative() {
				changeDir = "DOWN"
			}
			
			// Track consecutive moves in same direction for NO
			if s.lastMoveDirNO[w.Asset] == changeDir {
				s.consecutiveMovesNO[w.Asset]++
			} else {
				s.consecutiveMovesNO[w.Asset] = 1
				s.lastMoveDirNO[w.Asset] = changeDir
			}
		}
	}

	historyYES = append(historyYES, yesTick)
	historyNO = append(historyNO, noTick)

	// Keep 60 seconds of history at 50ms = 1200 ticks, keep last 600
	if len(historyYES) > 600 {
		historyYES = historyYES[len(historyYES)-600:]
	}
	if len(historyNO) > 600 {
		historyNO = historyNO[len(historyNO)-600:]
	}

	s.priceHistoryYES[w.Asset] = historyYES
	s.priceHistoryNO[w.Asset] = historyNO
}

// hasPosition checks if we have an open position OR are in cooldown on this asset
func (s *PhaseScalper) hasPosition(asset string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Check for open position
	for _, pos := range s.activePositions {
		if pos.Asset == asset {
			return true
		}
	}
	
	// Check cooldown after last exit
	if lastExit, ok := s.lastTradeExit[asset]; ok {
		if time.Since(lastExit) < s.tradeCooldown {
			return true // In cooldown, treat as "has position"
		}
	}
	
	return false
}

// executeFade enters a fade position
func (s *PhaseScalper) executeFade(w *feeds.Window, move *SharpMove, phase MarketPhase) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// SET ENTRY LOCK immediately to prevent double-trigger
	s.entryInProgress[w.Asset] = true
	s.entryLockTime[w.Asset] = time.Now()

	// FADE logic: buy opposite of the move direction
	var side, tokenID string
	var entryPrice decimal.Decimal

	if move.direction == "UP" {
		// Price went UP → we think it's overshot → buy NO (bet it comes down)
		side = "NO"
		tokenID = w.NoTokenID
		entryPrice = w.NoPrice
	} else {
		// Price went DOWN → we think it's overshot → buy YES (bet it comes up)
		side = "YES"
		tokenID = w.YesTokenID
		entryPrice = w.YesPrice
	}

	// Sanity check: don't buy if price is extreme
	if entryPrice.LessThan(decimal.NewFromFloat(0.10)) || entryPrice.GreaterThan(decimal.NewFromFloat(0.90)) {
		log.Debug().
			Str("asset", w.Asset).
			Str("price", entryPrice.StringFixed(3)).
			Msg("⚠️ Skipping fade - price too extreme")
		s.entryInProgress[w.Asset] = false // Clear lock on skip
		return
	}

	// ═══════════════════════════════════════════════════════════════════════════
	// POSITION SIZING - CRITICAL FOR LIVE TRADING
	// Market orders: use $1.1 value (calculate shares from price)
	// Limit orders: use 5 shares (fixed shares)
	// For paper mode fade scalper, use market order value ($1.1)
	// ═══════════════════════════════════════════════════════════════════════════
	positionValue := s.marketOrderValue // $1.1 default for market orders
	if phase == PhaseClosing {
		positionValue = positionValue.Mul(decimal.NewFromFloat(0.7)) // 30% smaller in closing
		log.Debug().
			Str("asset", w.Asset).
			Str("size", positionValue.StringFixed(2)).
			Msg("📉 Reduced position size for Closing phase")
	}
	shares := positionValue.Div(entryPrice) // shares = $value / price

	log.Debug().
		Str("asset", w.Asset).
		Str("position_value", positionValue.StringFixed(2)).
		Str("entry_price", entryPrice.StringFixed(3)).
		Str("shares", shares.StringFixed(2)).
		Msg("📊 Position sizing calculated")

	// ═══════════════════════════════════════════════════════════════════════════
	// RISK GATE APPROVAL (if attached)
	// ═══════════════════════════════════════════════════════════════════════════
	if s.riskGate != nil {
		req := TradeApprovalRequest{
			Asset:    w.Asset,
			Side:     side,
			Action:   "BUY",
			Price:    entryPrice,
			Size:     shares,
			Strategy: "PhaseScalper",
			Phase:    phase.String(),
			Reason:   "Fade " + move.direction + " move",
			Metadata: map[string]any{
				"faded_move": move.direction,
				"magnitude":  move.magnitude.StringFixed(4),
			},
		}
		
		approval := s.riskGate.CanEnter(req)
		if !approval.Approved {
			log.Warn().
				Str("asset", w.Asset).
				Str("reason", approval.RejectionMsg).
				Msg("🚫 Trade rejected by Risk Gate")
			s.entryInProgress[w.Asset] = false
			return
		}
		
		// Use adjusted size from risk gate
		shares = approval.AdjustedSize
	}

	// Target price: entry + take profit
	targetPrice := entryPrice.Add(s.takeProfitCents)

	position := &FadePosition{
		MarketID:    w.ID,
		TokenID:     tokenID,
		Asset:       w.Asset,
		Side:        side,
		FadedMove:   move.direction,
		EntryPrice:  entryPrice,
		EntryTime:   time.Now(),
		Shares:      shares,
		TargetPrice: targetPrice,
		TimeoutAt:   time.Now().Add(s.maxTradeTimeout),
		IsPaper:     s.paperMode,
		Phase:       phase,
	}

	posKey := w.Asset + "_" + side
	s.activePositions[posKey] = position
	
	// ═══════════════════════════════════════════════════════════════════════════
	// PERSIST POSITION (if persister attached)
	// ═══════════════════════════════════════════════════════════════════════════
	if s.persister != nil {
		persistPos := &PersistablePosition{
			ID:       position.MarketID,
			MarketID: position.MarketID,
			TokenID:  tokenID,
			Asset:    w.Asset,
			Side:     side,
			Size:     shares,
			AvgEntry: entryPrice,
			Strategy: "PhaseScalper",
			OpenedAt: time.Now(),
			Metadata: map[string]any{
				"faded_move": move.direction,
				"phase":      phase.String(),
				"target":     targetPrice.String(),
			},
		}
		if err := s.persister.PersistPosition(persistPos); err != nil {
			log.Warn().Err(err).Str("asset", w.Asset).Msg("⚠️ Failed to persist position")
		}
	}

	mode := "PAPER"
	if !s.paperMode {
		mode = "LIVE"
	}

	log.Info().
		Str("asset", w.Asset).
		Str("phase", phase.String()).
		Str("faded", move.direction).
		Str("bought", side).
		Str("entry", entryPrice.Mul(decimal.NewFromInt(100)).StringFixed(1)+"¢").
		Str("target", targetPrice.Mul(decimal.NewFromInt(100)).StringFixed(1)+"¢").
		Str("shares", shares.StringFixed(2)).
		Dur("timeout", s.maxTradeTimeout).
		Msgf("⚡ FADE ENTRY (%s)", mode)
}

// checkExits monitors positions for exit signals
func (s *PhaseScalper) checkExits() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.activePositions) == 0 {
		return
	}

	now := time.Now()
	windows := s.windowScanner.GetActiveWindows()

	for key, pos := range s.activePositions {
		var currentPrice decimal.Decimal
		var timeRemaining float64
		var found bool

		// Find matching window
		for _, w := range windows {
			if w.Asset == pos.Asset {
				if pos.Side == "YES" {
					currentPrice = w.YesPrice
				} else {
					currentPrice = w.NoPrice
				}
				timeRemaining = w.TimeRemainingSeconds()
				found = true
				break
			}
		}

		if !found || currentPrice.IsZero() {
			continue
		}

		var exitReason string
		var exitPrice decimal.Decimal = currentPrice

		// Exit Priority:
		// 1. Phase cutoff (FLAT zone = force exit)
		phase := s.getPhase(timeRemaining)
		if phase == PhaseFlat || phase == PhaseResolution {
			exitReason = "PHASE_CUTOFF"
			log.Warn().
				Str("asset", pos.Asset).
				Float64("time_left", timeRemaining).
				Msg("🚨 Force exit - entering FLAT phase")
		}

		// 2. Take profit hit
		if exitReason == "" && currentPrice.GreaterThanOrEqual(pos.TargetPrice) {
			exitReason = "TAKE_PROFIT"
		}

		// 3. Timeout (NO stop loss - it gets hunted)
		if exitReason == "" && now.After(pos.TimeoutAt) {
			exitReason = "TIMEOUT"
		}

		// 4. Phase change (if entered in Opening, exit before Dead Zone)
		if exitReason == "" && pos.Phase == PhaseOpening && phase == PhaseDeadZone {
			exitReason = "PHASE_CHANGE"
		}

		if exitReason != "" {
			s.exitPosition(pos, exitPrice, exitReason, key)
		}
	}
}

// exitPosition closes a position
func (s *PhaseScalper) exitPosition(pos *FadePosition, exitPrice decimal.Decimal, reason string, key string) {
	// CLEAR ENTRY LOCK on exit
	s.entryInProgress[pos.Asset] = false
	
	// Correct PnL calculation: (exitPrice - entryPrice) * shares
	pnl := exitPrice.Sub(pos.EntryPrice).Mul(pos.Shares)

	s.totalTrades++
	s.totalProfit = s.totalProfit.Add(pnl)
	s.dailyPnL = s.dailyPnL.Add(pnl)

	isWin := pnl.GreaterThan(decimal.Zero)
	if isWin {
		s.winningTrades++
	} else {
		s.losingTrades++
		
		// Track loss count for this market (only if RiskGate not handling)
		if s.riskGate == nil {
			s.marketLossCount[pos.Asset]++
			lossCount := s.marketLossCount[pos.Asset]
			
			// Kill switch: first trade lost on this market
			if !s.firstTradeLost[pos.Asset] {
				s.firstTradeLost[pos.Asset] = true
				log.Warn().
					Str("asset", pos.Asset).
					Int("loss_count", lossCount).
					Msg("⚠️ First trade lost - monitoring market closely")
			}
			
			// MARKET DISABLE: After 2 losses, disable this market for the session
			if lossCount >= 2 && !s.marketDisabled[pos.Asset] {
				s.marketDisabled[pos.Asset] = true
				log.Error().
					Str("asset", pos.Asset).
					Int("loss_count", lossCount).
					Msg("🛑 MARKET DISABLED - 2 losses reached, avoiding revenge trading")
			}
		}
	}

	// ═══════════════════════════════════════════════════════════════════════════
	// RISK GATE EXIT RECORDING (if attached)
	// ═══════════════════════════════════════════════════════════════════════════
	if s.riskGate != nil {
		s.riskGate.RecordExit(pos.Asset, pnl)
	} else {
		// Fallback to internal daily loss limit check
		currentBalance := s.paperBalance
		if !s.paperMode {
			currentBalance = s.startingBalance
		}
		dynamicLossLimit := currentBalance.Mul(decimal.NewFromFloat(0.03))
		
		if s.dailyPnL.LessThan(dynamicLossLimit.Neg()) {
			s.dailyLossHit = true
			log.Error().
				Str("daily_pnl", s.dailyPnL.StringFixed(2)).
				Str("limit", dynamicLossLimit.StringFixed(2)).
				Msg("🛑 DAILY LOSS LIMIT HIT - Bot disabled for today")
		}
	}

	// Record cooldown for this asset (even if RiskGate handles it, local fallback)
	s.lastTradeExit[pos.Asset] = time.Now()

	// ═══════════════════════════════════════════════════════════════════════════
	// REMOVE FROM PERSISTENCE (if persister attached)
	// ═══════════════════════════════════════════════════════════════════════════
	if s.persister != nil {
		if err := s.persister.RemovePosition(pos.MarketID); err != nil {
			log.Warn().Err(err).Str("asset", pos.Asset).Msg("⚠️ Failed to remove persisted position")
		}
	}

	// Record paper trade
	if pos.IsPaper {
		s.paperTrades = append(s.paperTrades, PaperTrade{
			MarketID:   pos.MarketID,
			Asset:      pos.Asset,
			Side:       pos.Side,
			EntryPrice: pos.EntryPrice,
			ExitPrice:  exitPrice,
			EntryTime:  pos.EntryTime,
			ExitTime:   time.Now(),
			PnL:        pnl,
			Reason:     reason,
		})
		s.paperBalance = s.paperBalance.Add(pnl)
	}

	holdTime := time.Since(pos.EntryTime)

	emoji := "✅"
	if !isWin {
		emoji = "❌"
	}

	log.Info().
		Str("asset", pos.Asset).
		Str("side", pos.Side).
		Str("faded", pos.FadedMove).
		Str("entry", pos.EntryPrice.Mul(decimal.NewFromInt(100)).StringFixed(1)+"¢").
		Str("exit", exitPrice.Mul(decimal.NewFromInt(100)).StringFixed(1)+"¢").
		Str("pnl", "$"+pnl.StringFixed(2)).
		Str("reason", reason).
		Dur("hold", holdTime).
		Msgf("%s FADE EXIT (%s)", emoji, reason)

	delete(s.activePositions, key)
}

// GetPaperStats returns paper trading statistics
func (s *PhaseScalper) GetPaperStats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	winRate := 0.0
	if s.totalTrades > 0 {
		winRate = float64(s.winningTrades) / float64(s.totalTrades) * 100
	}

	avgProfit := decimal.Zero
	if s.totalTrades > 0 {
		avgProfit = s.totalProfit.Div(decimal.NewFromInt(int64(s.totalTrades)))
	}

	mode := "LIVE"
	if s.paperMode {
		mode = "PAPER"
	}

	return map[string]interface{}{
		"mode":           mode,
		"balance":        s.paperBalance.StringFixed(2),
		"total_trades":   s.totalTrades,
		"winning":        s.winningTrades,
		"losing":         s.losingTrades,
		"win_rate":       winRate,
		"total_pnl":      "$" + s.totalProfit.StringFixed(2),
		"avg_pnl":        "$" + avgProfit.StringFixed(2),
		"daily_pnl":      "$" + s.dailyPnL.StringFixed(2),
		"open_positions": len(s.activePositions),
		"daily_limit_hit": s.dailyLossHit,
	}
}

// GetRecentTrades returns last N paper trades
func (s *PhaseScalper) GetRecentTrades(limit int) []PaperTrade {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.paperTrades) <= limit {
		return s.paperTrades
	}

	return s.paperTrades[len(s.paperTrades)-limit:]
}
