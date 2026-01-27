package bot

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/types"
)

// ═══════════════════════════════════════════════════════════════════════════════
// TELEGRAM BOT - Modern trading notifications & control
// ═══════════════════════════════════════════════════════════════════════════════
//
// Features:
//   📊 Real-time signal alerts
//   💰 Trade notifications (open/close/TP/SL)
//   📈 Daily P&L summaries
//   🎛️ Bot control commands (/status, /pause, /resume, /stats)
//   🔔 Configurable alert levels
//
// ═══════════════════════════════════════════════════════════════════════════════

// TelegramBot manages the Telegram interface
type TelegramBot struct {
	mu      sync.RWMutex
	api     *tgbotapi.BotAPI
	chatID  int64
	running bool
	stopCh  chan struct{}

	// Stats for reporting
	statsProvider StatsProvider

	// Control callbacks
	onPause  func()
	onResume func()
}

// StatsProvider provides trading statistics
type StatsProvider interface {
	GetStats() (trades, wins, losses int, pnl, equity decimal.Decimal)
	GetBalance() (decimal.Decimal, error)
	GetRecentTrades(limit int) ([]types.TradeRecord, error)
	GetOpenPositions() ([]types.PositionRecord, error)
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

// NewTelegramBot creates a new Telegram bot
func NewTelegramBot(statsProvider StatsProvider) (*TelegramBot, error) {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN not set")
	}

	chatIDStr := os.Getenv("TELEGRAM_CHAT_ID")
	if chatIDStr == "" {
		return nil, fmt.Errorf("TELEGRAM_CHAT_ID not set")
	}

	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid TELEGRAM_CHAT_ID: %w", err)
	}

	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("failed to create bot: %w", err)
	}

	bot := &TelegramBot{
		api:           api,
		chatID:        chatID,
		stopCh:        make(chan struct{}),
		statsProvider: statsProvider,
	}

	log.Info().Str("username", api.Self.UserName).Msg("🤖 Telegram bot initialized")

	return bot, nil
}

// SetControlCallbacks sets pause/resume handlers
func (b *TelegramBot) SetControlCallbacks(onPause, onResume func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onPause = onPause
	b.onResume = onResume
}

// Start begins listening for commands
func (b *TelegramBot) Start() {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return
	}
	b.running = true
	b.mu.Unlock()

	go b.commandLoop()
	log.Info().Msg("📱 Telegram bot started")
}

// Stop stops the bot
func (b *TelegramBot) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.running {
		return
	}

	b.running = false
	close(b.stopCh)
	log.Info().Msg("Telegram bot stopped")
}

// ═══════════════════════════════════════════════════════════════════════════════
// NOTIFICATIONS
// ═══════════════════════════════════════════════════════════════════════════════

// NotifySignal sends a signal alert
func (b *TelegramBot) NotifySignal(asset, side string, entry, tp, sl decimal.Decimal, reason string) {
	emoji := "🎯"
	if side == "YES" {
		emoji = "🟢"
	} else {
		emoji = "🔴"
	}

	msg := fmt.Sprintf(`%s *SIGNAL DETECTED*

📊 *%s* — %s
━━━━━━━━━━━━━━━━
💵 Entry: *%s¢*
🎯 TP: *%s¢* (+%s¢)
🛑 SL: *%s¢* (-%s¢)
━━━━━━━━━━━━━━━━
📝 %s`,
		emoji,
		asset, side,
		entry.Mul(decimal.NewFromInt(100)).StringFixed(1),
		tp.Mul(decimal.NewFromInt(100)).StringFixed(1),
		tp.Sub(entry).Mul(decimal.NewFromInt(100)).StringFixed(1),
		sl.Mul(decimal.NewFromInt(100)).StringFixed(1),
		entry.Sub(sl).Mul(decimal.NewFromInt(100)).StringFixed(1),
		reason,
	)

	b.sendMarkdown(msg)
}

// NotifyTrade sends a trade execution alert
func (b *TelegramBot) NotifyTrade(action, asset, side string, price, size decimal.Decimal) {
	var emoji string
	switch action {
	case "OPEN":
		emoji = "✅"
	case "CLOSE":
		emoji = "📊"
	case "TAKE_PROFIT":
		emoji = "💰"
	case "STOP_LOSS":
		emoji = "🛑"
	default:
		emoji = "📌"
	}

	msg := fmt.Sprintf(`%s *%s*

📊 %s %s
💵 Price: *%s¢*
📦 Size: *$%s*`,
		emoji, action,
		asset, side,
		price.Mul(decimal.NewFromInt(100)).StringFixed(1),
		size.StringFixed(2),
	)

	b.sendMarkdown(msg)
}

// NotifyPnL sends a P&L notification
func (b *TelegramBot) NotifyPnL(asset string, pnl decimal.Decimal, isWin bool) {
	emoji := "📈"
	if !isWin {
		emoji = "📉"
	}

	sign := "+"
	if pnl.IsNegative() {
		sign = ""
	}

	msg := fmt.Sprintf(`%s *TRADE CLOSED*

📊 %s
💵 P&L: *%s$%s*`,
		emoji, asset,
		sign, pnl.StringFixed(2),
	)

	b.sendMarkdown(msg)
}

// NotifyDailySummary sends end-of-day summary
func (b *TelegramBot) NotifyDailySummary() {
	if b.statsProvider == nil {
		return
	}

	trades, wins, losses, pnl, equity := b.statsProvider.GetStats()

	winRate := float64(0)
	if trades > 0 {
		winRate = float64(wins) / float64(trades) * 100
	}

	emoji := "📈"
	if pnl.IsNegative() {
		emoji = "📉"
	}

	sign := "+"
	if pnl.IsNegative() {
		sign = ""
	}

	msg := fmt.Sprintf(`%s *DAILY SUMMARY*
━━━━━━━━━━━━━━━━━━━━

📊 Trades: *%d*
✅ Wins: *%d*
❌ Losses: *%d*
📈 Win Rate: *%.1f%%*

━━━━━━━━━━━━━━━━━━━━
💵 P&L: *%s$%s*
💰 Equity: *$%s*`,
		emoji,
		trades, wins, losses, winRate,
		sign, pnl.StringFixed(2),
		equity.StringFixed(2),
	)

	b.sendMarkdown(msg)
}

// NotifyError sends an error alert
func (b *TelegramBot) NotifyError(err error) {
	msg := fmt.Sprintf("⚠️ *ERROR*\n\n`%s`", err.Error())
	b.sendMarkdown(msg)
}

// NotifyStartup sends startup notification
func (b *TelegramBot) NotifyStartup(mode string) {
	// Get balance if available
	balanceStr := "N/A"
	if b.statsProvider != nil {
		if bal, err := b.statsProvider.GetBalance(); err == nil {
			balanceStr = "$" + bal.StringFixed(2)
		}
	}

	msg := fmt.Sprintf(`🚀 *POLYBOT STARTED*
━━━━━━━━━━━━━━━━━━━━

🎯 Strategy: *Sniper*
📊 Mode: *%s*
💰 Balance: *%s*
⏱️ Detection: *100ms*

━━━━━━━━━━━━━━━━━━━━
Entry: 88-93¢ | TP: 99¢ | SL: 70¢
Window: Last 15-60 seconds

Use /help for commands`, mode, balanceStr)

	b.sendMarkdown(msg)
}

// ═══════════════════════════════════════════════════════════════════════════════
// COMMAND HANDLING
// ═══════════════════════════════════════════════════════════════════════════════

func (b *TelegramBot) commandLoop() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30

	updates := b.api.GetUpdatesChan(u)

	for {
		select {
		case <-b.stopCh:
			return
		case update := <-updates:
			if update.Message == nil || !update.Message.IsCommand() {
				continue
			}

			// Only respond to authorized chat
			if update.Message.Chat.ID != b.chatID {
				continue
			}

			b.handleCommand(update.Message)
		}
	}
}

func (b *TelegramBot) handleCommand(msg *tgbotapi.Message) {
	cmd := strings.ToLower(msg.Command())

	switch cmd {
	case "start", "help":
		b.cmdHelp()
	case "status":
		b.cmdStatus()
	case "balance":
		b.cmdBalance()
	case "stats":
		b.cmdStats()
	case "trades":
		b.cmdTrades()
	case "positions":
		b.cmdPositions()
	case "pause":
		b.cmdPause()
	case "resume":
		b.cmdResume()
	case "ping":
		b.send("🏓 Pong!")
	default:
		b.send("❓ Unknown command. Use /help")
	}
}

func (b *TelegramBot) cmdHelp() {
	msg := `🤖 *POLYBOT COMMANDS*
━━━━━━━━━━━━━━━━━━━━

📊 /status — Bot status
� /balance — Account balance
📈 /stats — Trading statistics
📜 /trades — Last 10 trades
💼 /positions — Open positions
⏸️ /pause — Pause trading
▶️ /resume — Resume trading
🏓 /ping — Test connection

━━━━━━━━━━━━━━━━━━━━
Polybot Sniper — 100ms detection`

	b.sendMarkdown(msg)
}

func (b *TelegramBot) cmdStatus() {
	mode := "LIVE"
	if os.Getenv("DRY_RUN") == "true" {
		mode = "PAPER"
	}

	status := "🟢 RUNNING"

	// Get balance if available
	balanceStr := "N/A"
	if b.statsProvider != nil {
		if bal, err := b.statsProvider.GetBalance(); err == nil {
			balanceStr = "$" + bal.StringFixed(2)
		}
	}

	msg := fmt.Sprintf(`📊 *BOT STATUS*
━━━━━━━━━━━━━━━━━━━━

%s
📊 Mode: *%s*
💰 Balance: *%s*
🎯 Strategy: *Sniper*
⏱️ Detection: *100ms*

Entry: 88-93¢ | TP: 99¢ | SL: 70¢`, status, mode, balanceStr)

	b.sendMarkdown(msg)
}

func (b *TelegramBot) cmdStats() {
	if b.statsProvider == nil {
		b.send("❌ Stats not available")
		return
	}

	trades, wins, losses, pnl, equity := b.statsProvider.GetStats()

	winRate := float64(0)
	if trades > 0 {
		winRate = float64(wins) / float64(trades) * 100
	}

	sign := "+"
	if pnl.IsNegative() {
		sign = ""
	}

	msg := fmt.Sprintf(`📈 *TRADING STATS*
━━━━━━━━━━━━━━━━━━━━

📊 Total Trades: *%d*
✅ Wins: *%d*
❌ Losses: *%d*
📈 Win Rate: *%.1f%%*

━━━━━━━━━━━━━━━━━━━━
💵 Total P&L: *%s$%s*
💰 Equity: *$%s*`,
		trades, wins, losses, winRate,
		sign, pnl.StringFixed(2),
		equity.StringFixed(2),
	)

	b.sendMarkdown(msg)
}

func (b *TelegramBot) cmdPositions() {
	if b.statsProvider == nil {
		b.send("❌ Positions not available")
		return
	}

	positions, err := b.statsProvider.GetOpenPositions()
	if err != nil {
		b.send("❌ Failed to fetch positions")
		return
	}

	if len(positions) == 0 {
		b.send("📭 No open positions")
		return
	}

	msg := "💼 *OPEN POSITIONS*\n━━━━━━━━━━━━━━━━━━━━\n\n"

	for i, pos := range positions {
		sideEmoji := "🟢"
		if pos.Side == "NO" {
			sideEmoji = "🔴"
		}
		duration := time.Since(pos.OpenedAt).Round(time.Second)

		msg += fmt.Sprintf(`%s *%s* — %s
💵 Entry: %s¢ | Size: $%s
🎯 TP: %s¢ | 🛑 SL: %s¢
⏱️ Duration: %v

`,
			sideEmoji, pos.Asset, pos.Side,
			pos.EntryPrice.Mul(decimal.NewFromInt(100)).StringFixed(1),
			pos.Size.StringFixed(2),
			pos.TakeProfit.Mul(decimal.NewFromInt(100)).StringFixed(1),
			pos.StopLoss.Mul(decimal.NewFromInt(100)).StringFixed(1),
			duration,
		)

		if i >= 4 {
			msg += fmt.Sprintf("_... and %d more_", len(positions)-5)
			break
		}
	}

	b.sendMarkdown(msg)
}

func (b *TelegramBot) cmdBalance() {
	if b.statsProvider == nil {
		b.send("❌ Balance not available")
		return
	}

	balance, err := b.statsProvider.GetBalance()
	if err != nil {
		b.send("❌ Failed to fetch balance")
		return
	}

	msg := fmt.Sprintf(`💰 *ACCOUNT BALANCE*
━━━━━━━━━━━━━━━━━━━━

💵 Available: *$%s*

Use /positions to see open trades`,
		balance.StringFixed(2),
	)

	b.sendMarkdown(msg)
}

func (b *TelegramBot) cmdTrades() {
	if b.statsProvider == nil {
		b.send("❌ Trades not available")
		return
	}

	trades, err := b.statsProvider.GetRecentTrades(10)
	if err != nil {
		b.send("❌ Failed to fetch trades")
		return
	}

	if len(trades) == 0 {
		b.send("📭 No trade history yet")
		return
	}

	msg := "📜 *LAST 10 TRADES*\n━━━━━━━━━━━━━━━━━━━━\n\n"

	for _, t := range trades {
		actionEmoji := "📌"
		switch t.Action {
		case "OPEN":
			actionEmoji = "✅"
		case "TAKE_PROFIT":
			actionEmoji = "💰"
		case "STOP_LOSS":
			actionEmoji = "🛑"
		case "CLOSE":
			actionEmoji = "📊"
		}

		pnlStr := ""
		if !t.PnL.IsZero() {
			sign := "+"
			if t.PnL.IsNegative() {
				sign = ""
			}
			pnlStr = fmt.Sprintf(" | P&L: %s$%s", sign, t.PnL.StringFixed(2))
		}

		timeStr := t.Timestamp.Format("Jan 2 15:04")

		msg += fmt.Sprintf("%s %s %s %s @ %s¢%s\n   _%s_\n\n",
			actionEmoji, t.Action, t.Asset, t.Side,
			t.Price.Mul(decimal.NewFromInt(100)).StringFixed(1),
			pnlStr, timeStr,
		)
	}

	b.sendMarkdown(msg)
}

func (b *TelegramBot) cmdPause() {
	b.mu.RLock()
	cb := b.onPause
	b.mu.RUnlock()

	if cb != nil {
		cb()
	}

	b.send("⏸️ Trading paused")
	log.Info().Msg("Trading paused via Telegram")
}

func (b *TelegramBot) cmdResume() {
	b.mu.RLock()
	cb := b.onResume
	b.mu.RUnlock()

	if cb != nil {
		cb()
	}

	b.send("▶️ Trading resumed")
	log.Info().Msg("Trading resumed via Telegram")
}

// ═══════════════════════════════════════════════════════════════════════════════
// HELPERS
// ═══════════════════════════════════════════════════════════════════════════════

func (b *TelegramBot) send(text string) {
	msg := tgbotapi.NewMessage(b.chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		log.Error().Err(err).Msg("Failed to send Telegram message")
	}
}

func (b *TelegramBot) sendMarkdown(text string) {
	msg := tgbotapi.NewMessage(b.chatID, text)
	msg.ParseMode = "Markdown"
	if _, err := b.api.Send(msg); err != nil {
		log.Error().Err(err).Msg("Failed to send Telegram message")
	}
}
