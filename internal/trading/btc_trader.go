// Package trading provides trade execution functionality
//
// btc_trader.go - LEGACY COMPONENT
// This file handles BTC-specific Polymarket window trading.
// TODO: Generalize to support any asset via config.TradingAsset
// The new v3 architecture (engine.go + strategy + risk) is asset-agnostic.
// This file is kept for backward compatibility with Telegram bot commands.
package trading

import (
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/internal/config"
	"github.com/web3guy0/polybot/internal/database"
	"github.com/web3guy0/polybot/internal/polymarket"
	"github.com/web3guy0/polybot/internal/predictor"
)

// BTCTrade represents a BTC window trade
type BTCTrade struct {
	ID           string
	WindowID     string
	Question     string
	Direction    string // "UP" or "DOWN"
	Amount       decimal.Decimal
	EntryPrice   decimal.Decimal
	CurrentPrice decimal.Decimal
	Signal       predictor.Signal
	Status       string // "pending", "open", "won", "lost"
	Timestamp    time.Time
	ResolvedAt   *time.Time
	Profit       decimal.Decimal
}

// BTCTrader handles BTC window trading
type BTCTrader struct {
	cfg           *config.Config
	db            *database.Database
	windowScanner *polymarket.BTCWindowScanner
	pred          *predictor.Predictor
	
	// Trade tracking
	trades      []BTCTrade
	tradesMu    sync.RWMutex
	lastTrade   time.Time
	
	// Stats
	totalTrades  int
	wonTrades    int
	lostTrades   int
	totalProfit  decimal.Decimal
	
	// Callbacks
	onTrade func(BTCTrade)
	
	enabled bool
	running bool
	stopCh  chan struct{}
}

// NewBTCTrader creates a new BTC trader
func NewBTCTrader(cfg *config.Config, db *database.Database, windowScanner *polymarket.BTCWindowScanner, pred *predictor.Predictor) *BTCTrader {
	return &BTCTrader{
		cfg:           cfg,
		db:            db,
		windowScanner: windowScanner,
		pred:          pred,
		trades:        make([]BTCTrade, 0),
		totalProfit:   decimal.Zero,
		enabled:       cfg.BTCAutoTrade,
		stopCh:        make(chan struct{}),
	}
}

// SetTradeCallback sets callback for new trades
func (t *BTCTrader) SetTradeCallback(cb func(BTCTrade)) {
	t.onTrade = cb
}

// Start begins the trading loop
func (t *BTCTrader) Start() {
	t.running = true
	go t.tradingLoop()
	log.Info().
		Bool("auto_trade", t.cfg.BTCAutoTrade).
		Bool("alert_only", t.cfg.BTCAlertOnly).
		Msg("💰 BTC Trader started")
}

// Stop stops the trader
func (t *BTCTrader) Stop() {
	t.running = false
	close(t.stopCh)
}

// Enable enables auto-trading
func (t *BTCTrader) Enable() {
	t.enabled = true
	log.Info().Msg("🟢 BTC Auto-trading ENABLED")
}

// Disable disables auto-trading
func (t *BTCTrader) Disable() {
	t.enabled = false
	log.Info().Msg("🔴 BTC Auto-trading DISABLED")
}

// IsEnabled returns if auto-trading is enabled
func (t *BTCTrader) IsEnabled() bool {
	return t.enabled && !t.cfg.DryRun
}

func (t *BTCTrader) tradingLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			t.checkForTrades()
		case <-t.stopCh:
			return
		}
	}
}

func (t *BTCTrader) checkForTrades() {
	// Check cooldown
	if time.Since(t.lastTrade) < t.cfg.BTCCooldown {
		return
	}
	
	// Get current signal
	signal := t.pred.GetCurrentSignal()
	
	// Skip if no strong signal
	if signal.Direction == predictor.DirectionSkip {
		return
	}
	
	// Check confidence threshold
	if signal.Confidence < t.cfg.BTCMinConfidence {
		log.Debug().
			Float64("confidence", signal.Confidence).
			Float64("required", t.cfg.BTCMinConfidence).
			Msg("Signal confidence below threshold")
		return
	}
	
	// Find best window for the direction
	direction := string(signal.Direction)
	window := t.windowScanner.GetBestWindow(direction)
	
	if window == nil {
		log.Debug().Str("direction", direction).Msg("No suitable BTC window found")
		return
	}
	
	// Check odds
	var price decimal.Decimal
	if direction == "UP" {
		price = window.YesPrice
	} else {
		price = window.NoPrice
	}
	
	if price.LessThan(t.cfg.BTCMinOdds) || price.GreaterThan(t.cfg.BTCMaxOdds) {
		log.Debug().
			Str("price", price.String()).
			Str("min", t.cfg.BTCMinOdds.String()).
			Str("max", t.cfg.BTCMaxOdds.String()).
			Msg("Price outside acceptable range")
		return
	}
	
	// Create trade
	trade := BTCTrade{
		ID:         fmt.Sprintf("btc_%d", time.Now().UnixNano()),
		WindowID:   window.ID,
		Question:   window.Question,
		Direction:  direction,
		Amount:     t.cfg.BTCMaxBetSize,
		EntryPrice: price,
		Signal:     signal,
		Status:     "pending",
		Timestamp:  time.Now(),
	}
	
	// Execute or alert
	if t.enabled && t.cfg.BTCAutoTrade && !t.cfg.DryRun {
		t.executeTrade(&trade, window)
	} else if t.cfg.BTCAlertOnly {
		trade.Status = "alert"
		t.notifyTrade(trade)
	}
	
	// Record trade
	t.tradesMu.Lock()
	t.trades = append(t.trades, trade)
	t.lastTrade = time.Now()
	t.totalTrades++
	t.tradesMu.Unlock()
	
	// Save to database
	t.saveTrade(&trade)
}

func (t *BTCTrader) executeTrade(trade *BTCTrade, window *polymarket.BTCWindow) {
	log.Info().
		Str("direction", trade.Direction).
		Str("amount", trade.Amount.String()).
		Str("price", trade.EntryPrice.String()).
		Str("window", window.Question).
		Msg("🎯 Executing BTC trade")
	
	if t.cfg.DryRun {
		trade.Status = "dry_run"
		log.Info().Msg("🧪 DRY RUN: Trade not actually placed")
	} else {
		// TODO: Implement actual Polymarket CLOB order placement
		// This would use the py-clob-client or Go equivalent
		trade.Status = "open"
		log.Warn().Msg("⚠️ Real trading not yet implemented - would place order here")
	}
	
	t.notifyTrade(*trade)
}

func (t *BTCTrader) notifyTrade(trade BTCTrade) {
	if t.onTrade != nil {
		t.onTrade(trade)
	}
}

func (t *BTCTrader) saveTrade(trade *BTCTrade) {
	dbTrade := &database.Trade{
		MarketID:  trade.WindowID,
		Side:      trade.Direction,
		Amount:    trade.Amount,
		Price:     trade.EntryPrice,
		Status:    trade.Status,
		CreatedAt: trade.Timestamp,
	}
	t.db.SaveTrade(dbTrade)
}

// ManualTrade executes a manual trade
func (t *BTCTrader) ManualTrade(direction string, amount decimal.Decimal) (*BTCTrade, error) {
	// Get best window
	window := t.windowScanner.GetBestWindow(direction)
	if window == nil {
		return nil, fmt.Errorf("no suitable BTC window found for %s", direction)
	}
	
	var price decimal.Decimal
	if direction == "UP" {
		price = window.YesPrice
	} else {
		price = window.NoPrice
	}
	
	// Create trade
	trade := &BTCTrade{
		ID:         fmt.Sprintf("btc_%d", time.Now().UnixNano()),
		WindowID:   window.ID,
		Question:   window.Question,
		Direction:  direction,
		Amount:     amount,
		EntryPrice: price,
		Signal:     t.pred.GetCurrentSignal(),
		Status:     "manual",
		Timestamp:  time.Now(),
	}
	
	if !t.cfg.DryRun {
		// TODO: Execute actual trade
		trade.Status = "open"
	} else {
		trade.Status = "dry_run"
	}
	
	// Record
	t.tradesMu.Lock()
	t.trades = append(t.trades, *trade)
	t.lastTrade = time.Now()
	t.totalTrades++
	t.tradesMu.Unlock()
	
	t.saveTrade(trade)
	t.notifyTrade(*trade)
	
	return trade, nil
}

// GetOpenTrades returns currently open trades
func (t *BTCTrader) GetOpenTrades() []BTCTrade {
	t.tradesMu.RLock()
	defer t.tradesMu.RUnlock()
	
	open := make([]BTCTrade, 0)
	for _, trade := range t.trades {
		if trade.Status == "open" || trade.Status == "pending" {
			open = append(open, trade)
		}
	}
	return open
}

// GetRecentTrades returns recent trades
func (t *BTCTrader) GetRecentTrades(count int) []BTCTrade {
	t.tradesMu.RLock()
	defer t.tradesMu.RUnlock()
	
	if len(t.trades) <= count {
		result := make([]BTCTrade, len(t.trades))
		copy(result, t.trades)
		return result
	}
	
	result := make([]BTCTrade, count)
	copy(result, t.trades[len(t.trades)-count:])
	return result
}

// GetStats returns trading statistics
func (t *BTCTrader) GetStats() (total, won, lost int, profit decimal.Decimal, winRate float64) {
	t.tradesMu.RLock()
	defer t.tradesMu.RUnlock()
	
	total = t.totalTrades
	won = t.wonTrades
	lost = t.lostTrades
	profit = t.totalProfit
	
	if total > 0 {
		winRate = float64(won) / float64(total) * 100
	}
	
	return
}

// RecordResult records a trade result
func (t *BTCTrader) RecordResult(tradeID string, won bool, profit decimal.Decimal) {
	t.tradesMu.Lock()
	defer t.tradesMu.Unlock()
	
	for i := range t.trades {
		if t.trades[i].ID == tradeID {
			now := time.Now()
			t.trades[i].ResolvedAt = &now
			t.trades[i].Profit = profit
			
			if won {
				t.trades[i].Status = "won"
				t.wonTrades++
			} else {
				t.trades[i].Status = "lost"
				t.lostTrades++
			}
			
			t.totalProfit = t.totalProfit.Add(profit)
			break
		}
	}
}

// FormatTrade formats a trade for display
func FormatTrade(trade BTCTrade) string {
	dirEmoji := "🟢"
	if trade.Direction == "DOWN" {
		dirEmoji = "🔴"
	}
	
	statusEmoji := "⏳"
	switch trade.Status {
	case "open":
		statusEmoji = "📊"
	case "won":
		statusEmoji = "✅"
	case "lost":
		statusEmoji = "❌"
	case "dry_run":
		statusEmoji = "🧪"
	case "alert":
		statusEmoji = "🔔"
	}
	
	potentialProfit := decimal.NewFromInt(1).Div(trade.EntryPrice).Sub(decimal.NewFromInt(1)).Mul(decimal.NewFromInt(100))
	
	return fmt.Sprintf(`%s *BTC Trade: %s %s*

%s Status: %s

💵 Amount: $%s
💰 Entry: $%s (%s%% potential)
📊 Signal Score: %.1f

📋 *%s*

⏰ %s`,
		dirEmoji, trade.Direction, statusEmoji,
		statusEmoji, trade.Status,
		trade.Amount.StringFixed(2),
		trade.EntryPrice.StringFixed(2), potentialProfit.StringFixed(0),
		trade.Signal.TotalScore,
		trade.Question,
		trade.Timestamp.Format("15:04:05 MST"),
	)
}

// FormatStats formats trading stats for display
func (t *BTCTrader) FormatStats() string {
	total, won, lost, profit, winRate := t.GetStats()
	
	return fmt.Sprintf(`📊 *BTC Trading Stats*

📈 Total Trades: %d
✅ Won: %d
❌ Lost: %d
🎯 Win Rate: %.1f%%

💰 Total P/L: $%s

⚙️ Auto-Trade: %v
🧪 Dry Run: %v`,
		total, won, lost, winRate,
		profit.StringFixed(2),
		t.enabled,
		t.cfg.DryRun,
	)
}
