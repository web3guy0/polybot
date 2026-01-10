// Package bot provides a clean, modern Telegram bot for the scalper
// Minimal features: Status, Positions, Trade Alerts
package bot

import (
	"fmt"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/internal/arbitrage"
	"github.com/web3guy0/polybot/internal/config"
)

// Bot is a minimal, modern Telegram bot for the scalper
type Bot struct {
	api    *tgbotapi.BotAPI
	cfg    *config.Config
	chatID int64

	// Scalpers for each asset
	scalpers map[string]*arbitrage.ScalperStrategy
	engines  map[string]*arbitrage.Engine

	// State
	mu       sync.RWMutex
	alertsOn bool

	stopCh chan struct{}
}

// New creates a new Telegram bot
func New(cfg *config.Config) (*Bot, error) {
	if cfg.TelegramToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN not set")
	}

	api, err := tgbotapi.NewBotAPI(cfg.TelegramToken)
	if err != nil {
		return nil, fmt.Errorf("telegram connect failed: %w", err)
	}

	log.Info().Str("username", api.Self.UserName).Msg("🤖 Telegram connected")

	return &Bot{
		api:      api,
		cfg:      cfg,
		chatID:   cfg.TelegramChatID,
		scalpers: make(map[string]*arbitrage.ScalperStrategy),
		engines:  make(map[string]*arbitrage.Engine),
		alertsOn: true,
		stopCh:   make(chan struct{}),
	}, nil
}

// AddScalper registers a scalper for an asset
func (b *Bot) AddScalper(asset string, s *arbitrage.ScalperStrategy) {
	b.scalpers[asset] = s
}

// AddEngine registers an engine for an asset
func (b *Bot) AddEngine(asset string, e *arbitrage.Engine) {
	b.engines[asset] = e
}

// Start begins listening for commands
func (b *Bot) Start() {
	go b.listen()
	b.sendStartup()
}

// Stop stops the bot
func (b *Bot) Stop() {
	close(b.stopCh)
}

// SendTradeAlert sends a trade notification
func (b *Bot) SendTradeAlert(asset, side string, price decimal.Decimal, size int64, action string, pnl decimal.Decimal) {
	b.mu.RLock()
	alertsOn := b.alertsOn
	b.mu.RUnlock()

	if !alertsOn || b.chatID == 0 {
		log.Debug().Str("action", action).Bool("alertsOn", alertsOn).Int64("chatID", b.chatID).Msg("📱 Alert skipped")
		return
	}

	emoji := "🟢"
	if action == "SELL" || action == "STOP" {
		emoji = "🔴"
	}

	// Base message
	msg := fmt.Sprintf(`%s *%s %s*

Asset: *%s*
Side: *%s*
Price: *%s¢*
Size: *%d shares*`,
		emoji, action, side,
		asset,
		side,
		price.Mul(decimal.NewFromInt(100)).StringFixed(1),
		size,
	)

	// Add P&L for sells
	if action == "SELL" || action == "STOP" {
		pnlIcon := "💰"
		if pnl.LessThan(decimal.Zero) {
			pnlIcon = "💸"
		}
		msg += fmt.Sprintf("\nP&L: *%s $%s*", pnlIcon, pnl.StringFixed(2))
	}

	msg += fmt.Sprintf("\nTime: %s", time.Now().Format("15:04:05"))

	log.Info().Str("action", action).Str("asset", asset).Msg("📱 Sending Telegram alert")
	b.sendMarkdown(msg)
}

// ═══════════════════════════════════════════════════════════════════════════
// INTERNAL
// ═══════════════════════════════════════════════════════════════════════════

func (b *Bot) listen() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := b.api.GetUpdatesChan(u)

	for {
		select {
		case update := <-updates:
			if update.Message != nil && update.Message.IsCommand() {
				go b.handleCommand(update.Message)
			}
			if update.CallbackQuery != nil {
				go b.handleCallback(update.CallbackQuery)
			}
		case <-b.stopCh:
			return
		}
	}
}

func (b *Bot) handleCommand(msg *tgbotapi.Message) {
	// Security check
	if b.chatID != 0 && msg.Chat.ID != b.chatID {
		return
	}

	switch msg.Command() {
	case "start", "menu":
		b.cmdMenu(msg.Chat.ID)
	case "status", "s":
		b.cmdStatus(msg.Chat.ID)
	case "pos", "positions":
		b.cmdPositions(msg.Chat.ID)
	case "stats":
		b.cmdStats(msg.Chat.ID)
	case "alerts":
		b.cmdToggleAlerts(msg.Chat.ID)
	case "help", "h":
		b.cmdHelp(msg.Chat.ID)
	default:
		b.send(msg.Chat.ID, "❓ Unknown. Try /help")
	}
}

func (b *Bot) handleCallback(cb *tgbotapi.CallbackQuery) {
	// Acknowledge
	b.api.Request(tgbotapi.NewCallback(cb.ID, ""))

	chatID := cb.Message.Chat.ID
	msgID := cb.Message.MessageID

	switch cb.Data {
	case "refresh_status":
		b.updateStatus(chatID, msgID)
	case "refresh_pos":
		b.updatePositions(chatID, msgID)
	case "toggle_alerts":
		b.toggleAlerts(chatID, msgID)
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// COMMANDS
// ═══════════════════════════════════════════════════════════════════════════

func (b *Bot) cmdMenu(chatID int64) {
	mode := "🟢 LIVE"
	if b.cfg.DryRun {
		mode = "📝 PAPER"
	}

	msg := fmt.Sprintf(`⚡ *POLYBOT SCALPER*
━━━━━━━━━━━━━━━━━━━━━
Mode: %s
Strategy: Buy ≤20¢ → Sell 33¢

/status - Market status
/pos - Open positions  
/stats - Performance
/alerts - Toggle alerts
━━━━━━━━━━━━━━━━━━━━━`, mode)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Status", "refresh_status"),
			tgbotapi.NewInlineKeyboardButtonData("💰 Positions", "refresh_pos"),
		),
	)

	b.sendWithKeyboard(chatID, msg, keyboard)
}

func (b *Bot) cmdStatus(chatID int64) {
	var sb strings.Builder
	sb.WriteString("📊 *MARKET STATUS*\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━\n\n")

	for asset, engine := range b.engines {
		price := engine.GetCurrentPrice()
		
		// Get window info
		windows := engine.GetWindowScanner().GetActiveWindows()
		if len(windows) > 0 {
			w := windows[0]
			sb.WriteString(fmt.Sprintf("*%s* | $%s\n", asset, price.StringFixed(2)))
			sb.WriteString(fmt.Sprintf("  UP: %s¢ | DOWN: %s¢\n",
				w.YesPrice.Mul(decimal.NewFromInt(100)).StringFixed(1),
				w.NoPrice.Mul(decimal.NewFromInt(100)).StringFixed(1)))
			
			// Window age
			age := time.Since(w.StartDate)
			remaining := 15*time.Minute - age
			if remaining > 0 {
				sb.WriteString(fmt.Sprintf("  ⏱ %s left\n", remaining.Round(time.Second)))
			}
			sb.WriteString("\n")
		} else {
			sb.WriteString(fmt.Sprintf("*%s* | $%s | No window\n\n", asset, price.StringFixed(2)))
		}
	}

	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━\n")
	sb.WriteString(fmt.Sprintf("Updated: %s", time.Now().Format("15:04:05")))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "refresh_status"),
		),
	)

	b.sendWithKeyboard(chatID, sb.String(), keyboard)
}

func (b *Bot) cmdPositions(chatID int64) {
	var sb strings.Builder
	sb.WriteString("💰 *OPEN POSITIONS*\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━\n\n")

	hasPositions := false
	for asset, scalper := range b.scalpers {
		positions := scalper.GetPositions()
		for _, pos := range positions {
			hasPositions = true
			held := time.Since(pos.EntryTime).Round(time.Second)
			sb.WriteString(fmt.Sprintf("*%s %s*\n", asset, pos.Side))
			sb.WriteString(fmt.Sprintf("  Entry: %s¢\n", pos.EntryPrice.Mul(decimal.NewFromInt(100)).StringFixed(1)))
			sb.WriteString(fmt.Sprintf("  Target: %s¢\n", pos.TargetPrice.Mul(decimal.NewFromInt(100)).StringFixed(1)))
			sb.WriteString(fmt.Sprintf("  Stop: %s¢\n", pos.StopLoss.Mul(decimal.NewFromInt(100)).StringFixed(1)))
			sb.WriteString(fmt.Sprintf("  Held: %s\n\n", held))
		}
	}

	if !hasPositions {
		sb.WriteString("_No open positions_\n\n")
	}

	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━\n")
	sb.WriteString("Entry: ≤20¢ | Target: 33¢ | Stop: 15¢")

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "refresh_pos"),
		),
	)

	b.sendWithKeyboard(chatID, sb.String(), keyboard)
}

func (b *Bot) cmdStats(chatID int64) {
	var sb strings.Builder
	sb.WriteString("📈 *SCALPER STATS*\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━\n\n")

	totalTrades := 0
	totalWins := 0
	totalProfit := decimal.Zero

	for asset, scalper := range b.scalpers {
		stats := scalper.GetStats()
		totalTrades += stats.TotalTrades
		totalWins += stats.WinningTrades
		totalProfit = totalProfit.Add(stats.TotalProfit)

		if stats.TotalTrades > 0 {
			winRate := float64(stats.WinningTrades) / float64(stats.TotalTrades) * 100
			sb.WriteString(fmt.Sprintf("*%s*: %d trades | %.0f%% WR | $%s\n",
				asset, stats.TotalTrades, winRate, stats.TotalProfit.StringFixed(2)))
		}
	}

	if totalTrades == 0 {
		sb.WriteString("_No trades yet_\n")
	}

	sb.WriteString("\n━━━━━━━━━━━━━━━━━━━━━\n")
	if totalTrades > 0 {
		winRate := float64(totalWins) / float64(totalTrades) * 100
		sb.WriteString(fmt.Sprintf("*Total:* %d | %.0f%% WR | $%s", totalTrades, winRate, totalProfit.StringFixed(2)))
	}

	b.sendMarkdown(sb.String())
}

func (b *Bot) cmdToggleAlerts(chatID int64) {
	b.mu.Lock()
	b.alertsOn = !b.alertsOn
	status := b.alertsOn
	b.mu.Unlock()

	if status {
		b.send(chatID, "🔔 Alerts ON")
	} else {
		b.send(chatID, "🔕 Alerts OFF")
	}
}

func (b *Bot) cmdHelp(chatID int64) {
	msg := `📖 *COMMANDS*
━━━━━━━━━━━━━━━━━━━━━

/status - Market prices & windows
/pos - Open scalp positions
/stats - Trading performance
/alerts - Toggle notifications

━━━━━━━━━━━━━━━━━━━━━
*Strategy:* Buy ≤20¢ → Sell 33¢
*Stop Loss:* 15¢
*Max Age:* 10 min (need time for reversal)`

	b.sendMarkdown(msg)
}

// ═══════════════════════════════════════════════════════════════════════════
// UPDATE HELPERS (for inline refresh buttons)
// ═══════════════════════════════════════════════════════════════════════════

func (b *Bot) updateStatus(chatID int64, msgID int) {
	var sb strings.Builder
	sb.WriteString("📊 *MARKET STATUS*\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━\n\n")

	for asset, engine := range b.engines {
		price := engine.GetCurrentPrice()
		windows := engine.GetWindowScanner().GetActiveWindows()
		if len(windows) > 0 {
			w := windows[0]
			sb.WriteString(fmt.Sprintf("*%s* | $%s\n", asset, price.StringFixed(2)))
			sb.WriteString(fmt.Sprintf("  UP: %s¢ | DOWN: %s¢\n",
				w.YesPrice.Mul(decimal.NewFromInt(100)).StringFixed(1),
				w.NoPrice.Mul(decimal.NewFromInt(100)).StringFixed(1)))
			age := time.Since(w.StartDate)
			remaining := 15*time.Minute - age
			if remaining > 0 {
				sb.WriteString(fmt.Sprintf("  ⏱ %s left\n", remaining.Round(time.Second)))
			}
			sb.WriteString("\n")
		} else {
			sb.WriteString(fmt.Sprintf("*%s* | $%s | No window\n\n", asset, price.StringFixed(2)))
		}
	}

	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━\n")
	sb.WriteString(fmt.Sprintf("Updated: %s", time.Now().Format("15:04:05")))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "refresh_status"),
		),
	)

	b.editWithKeyboard(chatID, msgID, sb.String(), keyboard)
}

func (b *Bot) updatePositions(chatID int64, msgID int) {
	var sb strings.Builder
	sb.WriteString("💰 *OPEN POSITIONS*\n")
	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━\n\n")

	hasPositions := false
	for asset, scalper := range b.scalpers {
		positions := scalper.GetPositions()
		for _, pos := range positions {
			hasPositions = true
			held := time.Since(pos.EntryTime).Round(time.Second)
			sb.WriteString(fmt.Sprintf("*%s %s*\n", asset, pos.Side))
			sb.WriteString(fmt.Sprintf("  Entry: %s¢\n", pos.EntryPrice.Mul(decimal.NewFromInt(100)).StringFixed(1)))
			sb.WriteString(fmt.Sprintf("  Target: %s¢\n", pos.TargetPrice.Mul(decimal.NewFromInt(100)).StringFixed(1)))
			sb.WriteString(fmt.Sprintf("  Stop: %s¢\n", pos.StopLoss.Mul(decimal.NewFromInt(100)).StringFixed(1)))
			sb.WriteString(fmt.Sprintf("  Held: %s\n\n", held))
		}
	}

	if !hasPositions {
		sb.WriteString("_No open positions_\n\n")
	}

	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━\n")
	sb.WriteString(fmt.Sprintf("Updated: %s", time.Now().Format("15:04:05")))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "refresh_pos"),
		),
	)

	b.editWithKeyboard(chatID, msgID, sb.String(), keyboard)
}

func (b *Bot) toggleAlerts(chatID int64, msgID int) {
	b.mu.Lock()
	b.alertsOn = !b.alertsOn
	status := b.alertsOn
	b.mu.Unlock()

	msg := "🔕 Alerts OFF"
	if status {
		msg = "🔔 Alerts ON"
	}

	b.edit(chatID, msgID, msg)
}

// ═══════════════════════════════════════════════════════════════════════════
// SEND HELPERS
// ═══════════════════════════════════════════════════════════════════════════

func (b *Bot) sendStartup() {
	if b.chatID == 0 {
		return
	}

	mode := "🟢 LIVE"
	if b.cfg.DryRun {
		mode = "📝 PAPER"
	}

	msg := fmt.Sprintf(`⚡ *POLYBOT STARTED*
━━━━━━━━━━━━━━━━━━━━━
Mode: %s
Assets: BTC, ETH, SOL
Strategy: Scalp ≤20¢ → 33¢

/status for market info`, mode)

	b.sendMarkdown(msg)
}

func (b *Bot) send(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	b.api.Send(msg)
}

func (b *Bot) sendMarkdown(text string) {
	if b.chatID == 0 {
		return
	}
	msg := tgbotapi.NewMessage(b.chatID, text)
	msg.ParseMode = "Markdown"
	b.api.Send(msg)
}

func (b *Bot) sendWithKeyboard(chatID int64, text string, keyboard tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = keyboard
	b.api.Send(msg)
}

func (b *Bot) edit(chatID int64, msgID int, text string) {
	edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
	b.api.Send(edit)
}

func (b *Bot) editWithKeyboard(chatID int64, msgID int, text string, keyboard tgbotapi.InlineKeyboardMarkup) {
	edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
	edit.ParseMode = "Markdown"
	edit.ReplyMarkup = &keyboard
	b.api.Send(edit)
}
