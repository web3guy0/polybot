package core

import (
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/exec"
	"github.com/web3guy0/polybot/feeds"
	"github.com/web3guy0/polybot/storage"
	"github.com/web3guy0/polybot/strategy"
	"github.com/web3guy0/polybot/types"
)

// ═══════════════════════════════════════════════════════════════════════════════
// ENGINE - Central orchestrator
// ═══════════════════════════════════════════════════════════════════════════════
//
// Flow:
//   Feed → Strategy → Risk → Sizing → Execution → TP/SL → Storage
//
// ═══════════════════════════════════════════════════════════════════════════════

// RiskValidator interface for risk manager to avoid import cycles
type RiskValidator interface {
	ValidateSignal(signal *strategy.Signal, equity decimal.Decimal, positions map[string]*types.Position) bool
	CalculateSize(signal *strategy.Signal, equity decimal.Decimal) decimal.Decimal
	RecordTrade(pnl decimal.Decimal)
}

// TradeNotifier interface for trade notifications (Telegram)
type TradeNotifier interface {
	NotifyTrade(action, asset, side string, price, size decimal.Decimal)
}

type Engine struct {
	mu sync.RWMutex

	// Components
	feed       *feeds.PolymarketFeed
	executor   *exec.Client
	riskMgr    RiskValidator
	strategies []strategy.Strategy
	db         *storage.Database
	router     *Router

	// State
	positions map[string]*types.Position
	equity    decimal.Decimal
	running   bool
	stopCh    chan struct{}

	// Stats
	totalTrades int
	winCount    int
	lossCount   int
	totalPnL    decimal.Decimal

	// Notifications
	tradeNotifier TradeNotifier
}

// NewEngine creates a new trading engine
func NewEngine(
	feed *feeds.PolymarketFeed,
	executor *exec.Client,
	riskMgr RiskValidator,
	strategies []strategy.Strategy,
	db *storage.Database,
) *Engine {
	return &Engine{
		feed:       feed,
		executor:   executor,
		riskMgr:    riskMgr,
		strategies: strategies,
		db:         db,
		router:     NewRouter(),
		positions:  make(map[string]*types.Position),
		equity:     decimal.NewFromFloat(100), // Initial equity
		stopCh:     make(chan struct{}),
		totalPnL:   decimal.Zero,
	}
}

// Start begins the engine loop
func (e *Engine) Start() {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return
	}
	e.running = true
	e.mu.Unlock()

	// Get initial balance
	if balance, err := e.executor.GetBalance(); err == nil {
		e.equity = balance
		log.Info().Str("balance", "$"+balance.StringFixed(2)).Msg("💰 Equity loaded")
	}

	// Start feed
	e.feed.Start()

	// Subscribe to ticks
	tickCh := e.feed.Subscribe()

	// Main loop
	go e.mainLoop(tickCh)

	// Position monitor loop
	go e.positionMonitorLoop()

	log.Info().Msg("⚡ Engine started")
}

// Stop stops the engine
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.running {
		return
	}

	e.running = false
	close(e.stopCh)
	e.feed.Stop()

	log.Info().Msg("Engine stopped")
}

// mainLoop processes incoming ticks
func (e *Engine) mainLoop(tickCh <-chan feeds.Tick) {
	for {
		select {
		case <-e.stopCh:
			return
		case tick := <-tickCh:
			e.processTick(tick)
		}
	}
}

// processTick handles a single tick event
func (e *Engine) processTick(tick feeds.Tick) {
	// Route tick to all strategies
	for _, strat := range e.strategies {
		signal := strat.OnTick(tick)
		if signal == nil {
			continue
		}

		// Validate signal with risk manager
		if !e.riskMgr.ValidateSignal(signal, e.equity, e.positions) {
			log.Debug().
				Str("strategy", strat.Name()).
				Str("reason", "risk rejected").
				Msg("Signal rejected")
			continue
		}

		// Calculate position size
		size := e.riskMgr.CalculateSize(signal, e.equity)
		if size.LessThanOrEqual(decimal.Zero) {
			continue
		}

		// Execute trade
		e.executeSignal(signal, size, strat.Name())
	}
}

// executeSignal places an order based on signal
func (e *Engine) executeSignal(signal *strategy.Signal, size decimal.Decimal, strategyName string) {
	log.Info().
		Str("asset", signal.Asset).
		Str("side", signal.Side).
		Str("entry", signal.Entry.StringFixed(2)).
		Str("tp", signal.TakeProfit.StringFixed(2)).
		Str("sl", signal.StopLoss.StringFixed(2)).
		Str("size", size.StringFixed(2)).
		Str("strategy", strategyName).
		Msg("🎯 SIGNAL DETECTED")

	// Place order
	orderID, err := e.executor.PlaceOrder(
		signal.TokenID,
		signal.Entry,
		size,
		"BUY",
	)

	if err != nil {
		log.Error().Err(err).Msg("Order failed")
		return
	}

	// Track position
	pos := &types.Position{
		ID:         orderID,
		Market:     signal.Market,
		Asset:      signal.Asset,
		Side:       signal.Side,
		TokenID:    signal.TokenID,
		EntryPrice: signal.Entry,
		Size:       size,
		EntryTime:  time.Now(),
		StopLoss:   signal.StopLoss,
		TakeProfit: signal.TakeProfit,
		Strategy:   strategyName,
		HighPrice:  signal.Entry,
	}

	e.mu.Lock()
	e.positions[orderID] = pos
	e.totalTrades++
	e.mu.Unlock()

	log.Info().
		Str("order_id", orderID).
		Str("asset", signal.Asset).
		Msg("✅ Position opened")

	// Log to database
	if e.db != nil {
		e.db.LogTrade(pos.ID, pos.Asset, pos.Side, pos.EntryPrice, pos.Size, "OPEN", strategyName)
	}

	// Notify via Telegram
	if e.tradeNotifier != nil {
		e.tradeNotifier.NotifyTrade("OPEN", signal.Asset, signal.Side, signal.Entry, size)
	}
}

// positionMonitorLoop monitors open positions for TP/SL
func (e *Engine) positionMonitorLoop() {
	// Use POSITION_MONITOR_MS from env, default 300ms
	intervalMs := 300
	if v := os.Getenv("POSITION_MONITOR_MS"); v != "" {
		if i, err := strconv.Atoi(v); err == nil && i > 0 {
			intervalMs = i
		}
	}
	
	ticker := time.NewTicker(time.Duration(intervalMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.checkPositions()
		}
	}
}

// checkPositions monitors all open positions
func (e *Engine) checkPositions() {
	e.mu.RLock()
	positions := make([]*types.Position, 0, len(e.positions))
	for _, pos := range e.positions {
		positions = append(positions, pos)
	}
	e.mu.RUnlock()

	for _, pos := range positions {
		e.checkPosition(pos)
	}
}

// checkPosition checks a single position for exit conditions
func (e *Engine) checkPosition(pos *types.Position) {
	// Get current price from feed
	currentPrice := e.feed.GetPrice(pos.Market, pos.Side)
	if currentPrice.IsZero() {
		return
	}

	// Update high water mark
	if currentPrice.GreaterThan(pos.HighPrice) {
		pos.HighPrice = currentPrice
	}

	// Check take profit
	if currentPrice.GreaterThanOrEqual(pos.TakeProfit) {
		e.exitPosition(pos, currentPrice, "TAKE_PROFIT")
		return
	}

	// Check stop loss
	if currentPrice.LessThanOrEqual(pos.StopLoss) {
		e.exitPosition(pos, currentPrice, "STOP_LOSS")
		return
	}
}

// exitPosition closes a position
func (e *Engine) exitPosition(pos *types.Position, exitPrice decimal.Decimal, reason string) {
	pnl := exitPrice.Sub(pos.EntryPrice).Mul(pos.Size)

	log.Info().
		Str("asset", pos.Asset).
		Str("entry", pos.EntryPrice.StringFixed(2)).
		Str("exit", exitPrice.StringFixed(2)).
		Str("pnl", pnl.StringFixed(2)).
		Str("reason", reason).
		Msg("📊 Position closed")

	// Place sell order
	_, err := e.executor.PlaceOrder(
		pos.TokenID,
		exitPrice,
		pos.Size,
		"SELL",
	)

	if err != nil {
		log.Error().Err(err).Msg("Exit order failed")
		return
	}

	// Update stats
	e.mu.Lock()
	delete(e.positions, pos.ID)
	e.totalPnL = e.totalPnL.Add(pnl)
	if pnl.GreaterThan(decimal.Zero) {
		e.winCount++
	} else {
		e.lossCount++
	}
	e.equity = e.equity.Add(pnl)
	e.mu.Unlock()

	// Log to database
	if e.db != nil {
		e.db.LogTrade(pos.ID, pos.Asset, pos.Side, exitPrice, pos.Size, reason, pos.Strategy)
	}

	// Notify risk manager
	e.riskMgr.RecordTrade(pnl)

	// Notify via Telegram
	if e.tradeNotifier != nil {
		e.tradeNotifier.NotifyTrade(reason, pos.Asset, pos.Side, exitPrice, pos.Size)
	}
}

// GetStats returns current engine statistics
func (e *Engine) GetStats() (trades, wins, losses int, pnl, equity decimal.Decimal) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.totalTrades, e.winCount, e.lossCount, e.totalPnL, e.equity
}

// GetPositions returns all open positions for display
func (e *Engine) GetPositions() []PositionInfo {
	e.mu.RLock()
	defer e.mu.RUnlock()

	result := make([]PositionInfo, 0, len(e.positions))
	for _, pos := range e.positions {
		// Get current price
		current := e.feed.GetPrice(pos.Market, pos.Side)
		if current.IsZero() {
			current = pos.EntryPrice
		}

		pnl := current.Sub(pos.EntryPrice).Mul(pos.Size)
		pnlPct := decimal.Zero
		if !pos.EntryPrice.IsZero() {
			pnlPct = current.Sub(pos.EntryPrice).Div(pos.EntryPrice).Mul(decimal.NewFromInt(100))
		}

		result = append(result, PositionInfo{
			Asset:      pos.Asset,
			Side:       pos.Side,
			Entry:      pos.EntryPrice,
			Current:    current,
			PnL:        pnl,
			PnLPercent: pnlPct,
			Duration:   time.Since(pos.EntryTime),
		})
	}
	return result
}

// PositionInfo represents a position for display
type PositionInfo struct {
	Asset      string
	Side       string
	Entry      decimal.Decimal
	Current    decimal.Decimal
	PnL        decimal.Decimal
	PnLPercent decimal.Decimal
	Duration   time.Duration
}

// ProcessSignal handles a signal from external sources (like Sniper's RunLoop)
func (e *Engine) ProcessSignal(signal *strategy.Signal, strategyName string) {
	if signal == nil {
		return
	}

	// Validate signal with risk manager
	if !e.riskMgr.ValidateSignal(signal, e.equity, e.positions) {
		log.Debug().
			Str("strategy", strategyName).
			Str("reason", "risk rejected").
			Msg("Signal rejected")
		return
	}

	// Calculate position size
	size := e.riskMgr.CalculateSize(signal, e.equity)
	if size.LessThanOrEqual(decimal.Zero) {
		return
	}

	// Execute trade
	e.executeSignal(signal, size, strategyName)
}

// ═══════════════════════════════════════════════════════════════════════════════
// TELEGRAM BOT INTERFACE
// ═══════════════════════════════════════════════════════════════════════════════

// SetTradeNotifier sets the callback for trade notifications
func (e *Engine) SetTradeNotifier(notifier TradeNotifier) {
	e.tradeNotifier = notifier
}

// GetBalance returns current USDC balance from exchange
func (e *Engine) GetBalance() (decimal.Decimal, error) {
	return e.executor.GetBalance()
}

// GetRecentTrades returns last N trades from database
func (e *Engine) GetRecentTrades(limit int) ([]types.TradeRecord, error) {
	if e.db == nil {
		return nil, nil
	}

	dbTrades, err := e.db.GetRecentTrades(limit)
	if err != nil {
		return nil, err
	}

	result := make([]types.TradeRecord, len(dbTrades))
	for i, t := range dbTrades {
		result[i] = types.TradeRecord{
			ID:        t.ID,
			Asset:     t.Asset,
			Side:      t.Side,
			Action:    t.Action,
			Price:     t.Price,
			Size:      t.Size,
			PnL:       t.PnL,
			Timestamp: t.Timestamp,
		}
	}

	return result, nil
}

// GetOpenPositions returns open positions for Telegram
func (e *Engine) GetOpenPositions() ([]types.PositionRecord, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	result := make([]types.PositionRecord, 0, len(e.positions))
	for _, pos := range e.positions {
		result = append(result, types.PositionRecord{
			Asset:      pos.Asset,
			Side:       pos.Side,
			EntryPrice: pos.EntryPrice,
			Size:       pos.Size,
			StopLoss:   pos.StopLoss,
			TakeProfit: pos.TakeProfit,
			OpenedAt:   pos.EntryTime,
		})
	}

	return result, nil
}
