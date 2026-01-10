// Package arbitrage - Swing Trading Strategy
// Trades mean reversion on odds swings - buy the dip, sell the bounce
package arbitrage

import (
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/internal/dashboard"
	"github.com/web3guy0/polybot/internal/database"
	"github.com/web3guy0/polybot/internal/polymarket"
)

// SwingPosition represents an active swing trade
type SwingPosition struct {
	Asset        string
	Side         string          // "UP" or "DOWN"
	EntryPrice   decimal.Decimal // Price we bought at
	EntryTime    time.Time
	Size         int64
	BounceTarget decimal.Decimal // Target exit (take profit)
	StopLoss     decimal.Decimal // Stop loss exit
	OrderID      string
	Signal       *SwingSignal // Original signal that triggered entry
}

// SwingStrategy implements mean-reversion swing trading
type SwingStrategy struct {
	mu sync.RWMutex

	// Components
	scanner       *MeanReversionScanner
	windowScanner *polymarket.WindowScanner
	clobClient    *CLOBClient
	db            *database.Database
	notifier      TradeNotifier
	proDash       *dashboard.ProDashboard

	// State
	positions   map[string]*SwingPosition // Asset -> position
	cooldowns   map[string]time.Time      // Asset -> cooldown expiry
	running     bool
	stopCh      chan struct{}

	// Stats
	totalTrades   int
	winningTrades int
	totalProfit   decimal.Decimal

	// Config
	config SwingConfig
}

// SwingConfig holds strategy configuration
type SwingConfig struct {
	// Position sizing
	MaxPositionUSD float64 // Max $ per position
	MaxPositions   int     // Max concurrent positions

	// Timing
	ScanInterval   time.Duration // How often to scan for signals
	VerifyDelay    time.Duration // Delay before verifying order status

	// Exit timing
	MinHoldTime    time.Duration // Minimum hold time before exit
	MaxHoldTime    time.Duration // Force exit after this time

	// Slippage for FAK orders
	BuySlippage  decimal.Decimal // Extra cents for buy orders
	SellSlippage decimal.Decimal // Extra cents for sell orders

	// Cooldown after trade
	CooldownTime time.Duration
}

// NewSwingStrategy creates a new swing trading strategy
func NewSwingStrategy(
	windowScanner *polymarket.WindowScanner,
	clobClient *CLOBClient,
	db *database.Database,
) *SwingStrategy {
	return &SwingStrategy{
		scanner:       NewMeanReversionScanner(),
		windowScanner: windowScanner,
		clobClient:    clobClient,
		db:            db,
		positions:     make(map[string]*SwingPosition),
		cooldowns:     make(map[string]time.Time),
		stopCh:        make(chan struct{}),
		totalProfit:   decimal.Zero,
		config: SwingConfig{
			MaxPositionUSD: 2.0,                        // $2 max per trade
			MaxPositions:   3,                          // Max 3 concurrent
			ScanInterval:   200 * time.Millisecond,     // 5x per second
			VerifyDelay:    300 * time.Millisecond,     // Verify after 300ms
			MinHoldTime:    2 * time.Second,            // Hold at least 2s
			MaxHoldTime:    2 * time.Minute,            // Force exit after 2m
			BuySlippage:    decimal.NewFromFloat(0.02), // 2¢ slippage
			SellSlippage:   decimal.NewFromFloat(0.03), // 3¢ slippage
			CooldownTime:   30 * time.Second,           // 30s cooldown
		},
	}
}

// SetNotifier sets the Telegram notifier
func (s *SwingStrategy) SetNotifier(n TradeNotifier) {
	s.notifier = n
	log.Info().Msg("📱 [SWING] Notifier connected")
}

// SetDashboard sets the dashboard updater
func (s *SwingStrategy) SetDashboard(d *dashboard.ProDashboard) {
	s.proDash = d
	log.Info().Msg("📊 [SWING] Dashboard connected")
}

// Start begins the swing trading loop
func (s *SwingStrategy) Start() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()

	log.Info().Msg("🎢 [SWING] Starting mean-reversion swing strategy...")

	go s.runLoop()
	go s.monitorPositions()
}

// Stop halts the strategy
func (s *SwingStrategy) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	s.mu.Unlock()

	close(s.stopCh)
	log.Info().Msg("🛑 [SWING] Strategy stopped")
}

// runLoop is the main scanning loop
func (s *SwingStrategy) runLoop() {
	ticker := time.NewTicker(s.config.ScanInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.scan()
		}
	}
}

// scan looks for swing opportunities
func (s *SwingStrategy) scan() {
	// Get active windows
	windows := s.windowScanner.GetActiveWindows()

	for _, w := range windows {
		// Record price for history tracking
		s.scanner.RecordPrice(w.Asset, w.YesPrice, w.NoPrice)

		// Skip if we have position or cooldown
		if s.hasPosition(w.Asset) || s.onCooldown(w.Asset) {
			continue
		}

		// Skip if at max positions
		if s.positionCount() >= s.config.MaxPositions {
			continue
		}

		// Check for signal
		timeRemaining := time.Until(w.EndDate)
		signal := s.scanner.GetSignal(w.Asset, timeRemaining)

		if signal != nil {
			s.executeSignal(w, signal)
		}
	}

	// Update dashboard
	s.updateDashboard()
}

// executeSignal attempts to enter a swing trade
func (s *SwingStrategy) executeSignal(window polymarket.PredictionWindow, signal *SwingSignal) {
	log.Info().
		Str("asset", signal.Asset).
		Str("side", signal.Side).
		Str("odds", signal.CurrentOdds.StringFixed(2)).
		Str("drop", signal.DropSize.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Float64("confidence", signal.Confidence).
		Str("target", signal.BounceTarget.StringFixed(2)).
		Msg("🎢 [SWING] Signal detected!")

	// Calculate position size
	size := s.calculateSize(signal.CurrentOdds)
	if size <= 0 {
		log.Warn().Msg("⚠️ [SWING] Size too small, skipping")
		return
	}

	// Get token ID
	var tokenID string
	if signal.Side == "UP" {
		tokenID = window.YesTokenID
	} else {
		tokenID = window.NoTokenID
	}

	// Calculate order price with slippage
	orderPrice := signal.CurrentOdds.Add(s.config.BuySlippage)

	// Place FOK order (Fill Or Kill for immediate execution)
	order := Order{
		TokenID:   tokenID,
		Price:     orderPrice,
		Size:      decimal.NewFromInt(size),
		Side:      OrderSideBuy,
		OrderType: OrderTypeFOK,
	}
	orderResp, err := s.clobClient.PlaceOrder(order)

	if err != nil {
		log.Error().Err(err).Str("asset", signal.Asset).Msg("❌ [SWING] Order failed")
		s.cooldowns[signal.Asset] = time.Now().Add(s.config.CooldownTime)
		return
	}

	// Verify fill
	time.Sleep(s.config.VerifyDelay)
	status, filledSize, _, err := s.clobClient.GetOrderStatus(orderResp.OrderID)

	if err != nil || filledSize.IsZero() {
		log.Warn().
			Str("order_id", orderResp.OrderID).
			Str("status", status).
			Msg("⚠️ [SWING] Order not filled")
		s.cooldowns[signal.Asset] = time.Now().Add(s.config.CooldownTime)
		return
	}

	// Create position
	actualSize := filledSize.IntPart()
	pos := &SwingPosition{
		Asset:        signal.Asset,
		Side:         signal.Side,
		EntryPrice:   orderPrice,
		EntryTime:    time.Now(),
		Size:         actualSize,
		BounceTarget: signal.BounceTarget,
		StopLoss:     signal.StopLoss,
		OrderID:      orderResp.OrderID,
		Signal:       signal,
	}

	s.mu.Lock()
	s.positions[signal.Asset] = pos
	s.mu.Unlock()

	log.Info().
		Str("order_id", orderResp.OrderID).
		Str("asset", signal.Asset).
		Str("side", signal.Side).
		Int64("size", actualSize).
		Str("entry", orderPrice.StringFixed(2)).
		Str("target", signal.BounceTarget.StringFixed(2)).
		Str("stop", signal.StopLoss.StringFixed(2)).
		Msg("✅ [SWING] Position opened!")

	// Notify
	if s.notifier != nil {
		s.notifier.SendTradeAlert(signal.Asset, signal.Side, orderPrice, actualSize, "BUY", decimal.Zero)
	}

	// Save to DB
	s.saveEntryToDB(pos)
}

// monitorPositions watches open positions for exit conditions
func (s *SwingStrategy) monitorPositions() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.checkExits()
		}
	}
}

// checkExits evaluates all positions for exit
func (s *SwingStrategy) checkExits() {
	s.mu.RLock()
	positions := make([]*SwingPosition, 0, len(s.positions))
	for _, pos := range s.positions {
		positions = append(positions, pos)
	}
	s.mu.RUnlock()

	windows := s.windowScanner.GetActiveWindows()
	windowMap := make(map[string]polymarket.PredictionWindow)
	for _, w := range windows {
		windowMap[w.Asset] = w
	}

	for _, pos := range positions {
		window, exists := windowMap[pos.Asset]
		if !exists {
			// Window expired, position should have been settled
			log.Warn().Str("asset", pos.Asset).Msg("⚠️ [SWING] Window expired with position!")
			s.closePosition(pos, pos.EntryPrice, "EXPIRED")
			continue
		}

		// Get current odds
		var currentOdds decimal.Decimal
		if pos.Side == "UP" {
			currentOdds = window.YesPrice
		} else {
			currentOdds = window.NoPrice
		}

		holdTime := time.Since(pos.EntryTime)

		// Check exits (in priority order)
		exitReason := ""
		shouldExit := false

		// 1. Hit bounce target (profit)
		if currentOdds.GreaterThanOrEqual(pos.BounceTarget) {
			exitReason = "TARGET"
			shouldExit = true
		}

		// 2. Hit stop loss
		if currentOdds.LessThanOrEqual(pos.StopLoss) && holdTime > s.config.MinHoldTime {
			exitReason = "STOP"
			shouldExit = true
		}

		// 3. Max hold time exceeded
		if holdTime > s.config.MaxHoldTime {
			exitReason = "TIMEOUT"
			shouldExit = true
		}

		// 4. Window about to expire (30 seconds left)
		timeRemaining := time.Until(window.EndDate)
		if timeRemaining < 30*time.Second {
			exitReason = "EXPIRY"
			shouldExit = true
		}

		// 5. Good profit (>10¢) even if not at target
		profit := currentOdds.Sub(pos.EntryPrice)
		if profit.GreaterThanOrEqual(decimal.NewFromFloat(0.10)) && holdTime > s.config.MinHoldTime {
			exitReason = "PROFIT"
			shouldExit = true
		}

		if shouldExit {
			s.exitPosition(pos, window, currentOdds, exitReason)
		}
	}
}

// exitPosition closes a position by selling
func (s *SwingStrategy) exitPosition(pos *SwingPosition, window polymarket.PredictionWindow, currentOdds decimal.Decimal, reason string) {
	// Get token ID
	var tokenID string
	if pos.Side == "UP" {
		tokenID = window.YesTokenID
	} else {
		tokenID = window.NoTokenID
	}

	// Calculate sell price with slippage (lower to ensure fill)
	sellPrice := currentOdds.Sub(s.config.SellSlippage)
	if sellPrice.LessThan(decimal.NewFromFloat(0.01)) {
		sellPrice = decimal.NewFromFloat(0.01)
	}

	// Place FOK sell order
	order := Order{
		TokenID:   tokenID,
		Price:     sellPrice,
		Size:      decimal.NewFromInt(pos.Size),
		Side:      OrderSideSell,
		OrderType: OrderTypeFOK,
	}
	orderResp, err := s.clobClient.PlaceOrder(order)

	if err != nil {
		log.Error().Err(err).Str("asset", pos.Asset).Msg("❌ [SWING] SELL order failed")
		return
	}

	// Verify fill
	time.Sleep(s.config.VerifyDelay)
	status, filledSize, _, err := s.clobClient.GetOrderStatus(orderResp.OrderID)

	if err != nil || filledSize.IsZero() {
		log.Warn().
			Str("order_id", orderResp.OrderID).
			Str("status", status).
			Msg("⚠️ [SWING] SELL not filled, retrying...")
		return // Will retry next tick
	}

	// Calculate P&L
	pnl := sellPrice.Sub(pos.EntryPrice).Mul(decimal.NewFromInt(pos.Size))

	s.mu.Lock()
	s.totalProfit = s.totalProfit.Add(pnl)
	s.totalTrades++
	if pnl.GreaterThan(decimal.Zero) {
		s.winningTrades++
	}
	delete(s.positions, pos.Asset)
	s.cooldowns[pos.Asset] = time.Now().Add(s.config.CooldownTime)
	s.mu.Unlock()

	action := "SELL"
	emoji := "✅"
	if reason == "STOP" {
		action = "STOP"
		emoji = "❌"
	}

	log.Info().
		Str("order_id", orderResp.OrderID).
		Str("asset", pos.Asset).
		Str("side", pos.Side).
		Str("reason", reason).
		Str("entry", pos.EntryPrice.StringFixed(2)).
		Str("exit", sellPrice.StringFixed(2)).
		Str("pnl", pnl.StringFixed(4)).
		Str("total_pnl", s.totalProfit.StringFixed(4)).
		Msg(emoji + " [SWING] Position closed!")

	// Notify
	if s.notifier != nil {
		s.notifier.SendTradeAlert(pos.Asset, pos.Side, sellPrice, pos.Size, action, pnl)
	}

	// Save to DB
	s.saveExitToDB(pos, orderResp.OrderID, sellPrice, pnl, reason)
}

// closePosition removes position without selling (e.g., window expired)
func (s *SwingStrategy) closePosition(pos *SwingPosition, exitPrice decimal.Decimal, reason string) {
	pnl := exitPrice.Sub(pos.EntryPrice).Mul(decimal.NewFromInt(pos.Size))

	s.mu.Lock()
	s.totalProfit = s.totalProfit.Add(pnl)
	s.totalTrades++
	if pnl.GreaterThan(decimal.Zero) {
		s.winningTrades++
	}
	delete(s.positions, pos.Asset)
	s.mu.Unlock()

	log.Warn().
		Str("asset", pos.Asset).
		Str("reason", reason).
		Str("pnl", pnl.StringFixed(4)).
		Msg("⚠️ [SWING] Position closed (no sell)")
}

// Helper methods

func (s *SwingStrategy) hasPosition(asset string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, exists := s.positions[asset]
	return exists
}

func (s *SwingStrategy) onCooldown(asset string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	expiry, exists := s.cooldowns[asset]
	return exists && time.Now().Before(expiry)
}

func (s *SwingStrategy) positionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.positions)
}

func (s *SwingStrategy) calculateSize(odds decimal.Decimal) int64 {
	// Size = MaxUSD / odds
	maxUSD := decimal.NewFromFloat(s.config.MaxPositionUSD)
	size := maxUSD.Div(odds).IntPart()
	if size < 1 {
		return 0
	}
	if size > 100 {
		size = 100 // Cap at 100 shares
	}
	return size
}

func (s *SwingStrategy) updateDashboard() {
	if s.proDash == nil {
		return
	}

	// Update stats - pass zero for balance since we don't track it here
	s.mu.RLock()
	s.proDash.UpdateStats(s.totalTrades, s.winningTrades, s.totalProfit, decimal.Zero)
	s.mu.RUnlock()
}

func (s *SwingStrategy) saveEntryToDB(pos *SwingPosition) {
	if s.db == nil {
		return
	}
	// TODO: Save entry to database
}

func (s *SwingStrategy) saveExitToDB(pos *SwingPosition, orderID string, exitPrice, pnl decimal.Decimal, reason string) {
	if s.db == nil {
		return
	}
	// TODO: Save exit to database
}

// GetPositions returns current positions (for dashboard)
func (s *SwingStrategy) GetPositions() []*SwingPosition {
	s.mu.RLock()
	defer s.mu.RUnlock()

	positions := make([]*SwingPosition, 0, len(s.positions))
	for _, pos := range s.positions {
		positions = append(positions, pos)
	}
	return positions
}

// GetStats returns trading statistics
func (s *SwingStrategy) GetStats() (int, int, decimal.Decimal) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.totalTrades, s.winningTrades, s.totalProfit
}
