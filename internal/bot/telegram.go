// Package bot provides Telegram bot functionality
//
// telegram.go - Telegram bot for crypto prediction alerts and trading
// Monitors Polymarket prediction windows and sends alerts based on technical signals.
package bot

import (
	"fmt"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/internal/binance"
	"github.com/web3guy0/polybot/internal/config"
	"github.com/web3guy0/polybot/internal/database"
	"github.com/web3guy0/polybot/internal/polymarket"
	"github.com/web3guy0/polybot/internal/predictor"
	"github.com/web3guy0/polybot/internal/trading"
)

// Bot handles Telegram interactions for the prediction system
type Bot struct {
	api              *tgbotapi.BotAPI
	cfg              *config.Config
	db               *database.Database
	tradingEngine    *trading.Engine
	binanceClient    *binance.Client
	predictor        *predictor.Predictor
	windowScanner    *polymarket.BTCWindowScanner
	trader           *trading.BTCTrader
	stopCh           chan struct{}
}

// New creates a new prediction-focused Telegram bot
func New(cfg *config.Config, db *database.Database, engine *trading.Engine,
	binanceClient *binance.Client, pred *predictor.Predictor, 
	windowScanner *polymarket.BTCWindowScanner, trader *trading.BTCTrader) (*Bot, error) {
	
	api, err := tgbotapi.NewBotAPI(cfg.TelegramToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram bot: %w", err)
	}

	log.Info().Str("username", api.Self.UserName).Msg("🤖 Telegram bot connected")

	bot := &Bot{
		api:           api,
		cfg:           cfg,
		db:            db,
		tradingEngine: engine,
		binanceClient: binanceClient,
		predictor:     pred,
		windowScanner: windowScanner,
		trader:        trader,
		stopCh:        make(chan struct{}),
	}

	// Set up signal callback for alerts
	if pred != nil && cfg.TelegramChatID != 0 {
		pred.SetSignalCallback(func(signal predictor.Signal) {
			strength := "WEAK"
			absScore := absFloat(signal.TotalScore)
			if absScore >= 70 {
				strength = "STRONG"
			} else if absScore >= 40 {
				strength = "MODERATE"
			}
			if strength == "STRONG" || (cfg.BTCAlertOnly && strength != "WEAK") {
				bot.sendSignalAlert(cfg.TelegramChatID, signal)
			}
		})
	}

	// Set up trade callback
	if trader != nil && cfg.TelegramChatID != 0 {
		trader.SetTradeCallback(func(trade trading.BTCTrade) {
			bot.sendTradeAlert(cfg.TelegramChatID, trade)
		})
	}

	return bot, nil
}

// Start begins the bot's command listener
func (b *Bot) Start() {
	go b.listenForCommands()

	if b.cfg.TelegramChatID != 0 {
		b.sendStartupMessage()
	}
}

// Stop stops the bot
func (b *Bot) Stop() {
	close(b.stopCh)
}

func (b *Bot) listenForCommands() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)

	for {
		select {
		case update := <-updates:
			if update.Message != nil {
				go b.handleMessage(update.Message)
			}
			if update.CallbackQuery != nil {
				go b.handleCallback(update.CallbackQuery)
			}
		case <-b.stopCh:
			return
		}
	}
}

func (b *Bot) handleMessage(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	text := msg.Text

	log.Debug().
		Int64("chat_id", chatID).
		Str("text", text).
		Msg("Received message")

	if msg.IsCommand() {
		switch msg.Command() {
		case "start":
			b.cmdStart(chatID)
		case "help":
			b.cmdHelp(chatID)
		case "status":
			b.cmdStatus(chatID)
		case "signal":
			b.cmdSignal(chatID)
		case "windows":
			b.cmdWindows(chatID)
		case "trade":
			b.cmdTrade(chatID, msg.CommandArguments())
		case "stats":
			b.cmdStats(chatID)
		case "autotrade":
			b.cmdAutoTrade(chatID, msg.CommandArguments())
		case "settings":
			b.cmdSettings(chatID)
		case "subscribe":
			b.cmdSubscribe(chatID)
		case "unsubscribe":
			b.cmdUnsubscribe(chatID)
		default:
			b.sendText(chatID, "❓ Unknown command. Use /help for available commands.")
		}
	}
}

func (b *Bot) handleCallback(cb *tgbotapi.CallbackQuery) {
	chatID := cb.Message.Chat.ID
	data := cb.Data

	log.Debug().
		Int64("chat_id", chatID).
		Str("data", data).
		Msg("Received callback")

	b.api.Request(tgbotapi.NewCallback(cb.ID, ""))

	switch {
	case data == "refresh_signal":
		b.cmdSignal(chatID)
	case data == "refresh_windows":
		b.cmdWindows(chatID)
	case data == "refresh_stats":
		b.cmdStats(chatID)
	case data == "show_windows":
		b.cmdWindows(chatID)
	case data == "trade_up":
		b.cmdTrade(chatID, "UP")
	case data == "trade_down":
		b.cmdTrade(chatID, "DOWN")
	case data == "toggle_alerts":
		b.toggleAlerts(chatID)
	}
}

// Commands

func (b *Bot) cmdStart(chatID int64) {
	settings, _ := b.db.GetUserSettings(chatID)
	settings.ChatID = chatID
	settings.AlertsEnabled = true
	b.db.SaveUserSettings(settings)

	asset := b.cfg.TradingAsset
	if asset == "" {
		asset = "BTC"
	}

	text := fmt.Sprintf(`🚀 *Welcome to Polybot!*

Your %s Prediction Trading Bot

*What I do:*
• 📊 Analyze %s using 6 technical indicators
• 🎯 Generate buy/sell signals for Polymarket windows
• 📱 Send instant alerts on strong signals
• 💰 Execute trades automatically (optional)

*How It Works:*
Polymarket offers 15-minute prediction windows.
I analyze RSI, Momentum, Volume, Order Book,
Funding Rate & Buy/Sell Ratio to predict direction.

*Quick Start:*
1️⃣ Use /signal to see current prediction
2️⃣ Use /windows to view active markets
3️⃣ Use /trade UP or /trade DOWN to execute

*Commands:*
/help - All commands
/signal - Current prediction signal
/windows - Active prediction windows
/stats - Trading statistics

Let's trade! 💪`, asset, asset)

	b.sendMarkdown(chatID, text)
}

func (b *Bot) cmdHelp(chatID int64) {
	asset := b.cfg.TradingAsset
	if asset == "" {
		asset = "BTC"
	}

	text := fmt.Sprintf(`📚 *Polybot Commands*

*📊 Prediction:*
/signal - Get current %s prediction
/windows - View active prediction windows
/status - Bot & market status

*💰 Trading:*
/trade UP - Bet on price going up
/trade DOWN - Bet on price going down
/autotrade on/off - Toggle auto-trading
/stats - Trading statistics

*⚙️ Settings:*
/settings - View/change settings
/subscribe - Enable signal alerts
/unsubscribe - Disable signal alerts

*How Signals Work:*
Bot uses 6 technical indicators:
• RSI - Overbought/oversold
• Momentum - Price trend strength
• Volume - Trading activity
• Order Book - Buy/sell pressure
• Funding Rate - Market sentiment
• Buy/Sell Ratio - Taker activity

Strong signals (score > 70) = trade opportunity!`, asset)

	b.sendMarkdown(chatID, text)
}

func (b *Bot) cmdStatus(chatID int64) {
	settings, _ := b.db.GetUserSettings(chatID)
	alertStatus := "🟢 Enabled"
	if !settings.AlertsEnabled {
		alertStatus = "🔴 Disabled"
	}

	autoTradeStatus := "🔴 OFF"
	if b.cfg.BTCAutoTrade {
		autoTradeStatus = "🟢 ON"
	}

	asset := b.cfg.TradingAsset
	if asset == "" {
		asset = "BTC"
	}

	windowCount := 0
	if b.windowScanner != nil {
		windowCount = len(b.windowScanner.GetActiveWindows())
	}

	text := fmt.Sprintf(`📊 *Bot Status*

🤖 *Bot:* Online
📡 *Asset:* %s
🎯 *Auto-Trade:* %s
🔔 *Alerts:* %s

*Market:*
• Active Windows: %d
• Scan Interval: %s

*Risk Settings:*
• Max Bet: $%s
• Min Odds: %s
• Max Odds: %s`,
		asset,
		autoTradeStatus,
		alertStatus,
		windowCount,
		b.cfg.BTCCooldown.String(),
		b.cfg.BTCMaxBetSize.String(),
		b.cfg.BTCMinOdds.String(),
		b.cfg.BTCMaxOdds.String(),
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "refresh_signal"),
			tgbotapi.NewInlineKeyboardButtonData("📊 Windows", "show_windows"),
		),
	)

	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *Bot) cmdSignal(chatID int64) {
	if b.predictor == nil || b.binanceClient == nil {
		b.sendText(chatID, "❌ Prediction system not enabled. Set BTC_ENABLED=true in config.")
		return
	}

	signal := b.predictor.GetCurrentSignal()
	priceFloat, _ := signal.CurrentPrice.Float64()

	var dirEmoji, dirText string
	if signal.Direction == "UP" {
		dirEmoji = "🟢"
		dirText = "BULLISH"
	} else if signal.Direction == "DOWN" {
		dirEmoji = "🔴"
		dirText = "BEARISH"
	} else {
		dirEmoji = "⚪"
		dirText = "NEUTRAL"
	}

	strength := "WEAK"
	absScore := absFloat(signal.TotalScore)
	if absScore >= 70 {
		strength = "STRONG 🔥"
	} else if absScore >= 40 {
		strength = "MODERATE"
	}

	asset := b.cfg.TradingAsset
	if asset == "" {
		asset = "BTC"
	}

	text := fmt.Sprintf(`🔶 *%s Prediction Signal*

💰 *Price:* $%s

%s *Direction:* %s
📊 *Score:* %.0f/100
💪 *Strength:* %s
🎯 *Confidence:* %.0f%%

*Indicator Breakdown:*
├ RSI: %.0f
├ Momentum: %.0f
├ Volume: %.0f
├ Order Book: %.0f
├ Funding: %.0f
└ Buy/Sell: %.0f

⏱️ Updated: %s`,
		asset,
		formatPrice(priceFloat),
		dirEmoji, dirText,
		absScore,
		strength,
		signal.Confidence,
		signal.RSI,
		signal.Momentum,
		signal.VolumeSignal,
		signal.OrderBook,
		signal.FundingRate,
		signal.BuySellRatio,
		signal.Timestamp.Format("15:04:05"),
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "refresh_signal"),
			tgbotapi.NewInlineKeyboardButtonData("📊 Windows", "show_windows"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🟢 Trade UP", "trade_up"),
			tgbotapi.NewInlineKeyboardButtonData("🔴 Trade DOWN", "trade_down"),
		),
	)

	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *Bot) cmdWindows(chatID int64) {
	if b.windowScanner == nil {
		b.sendText(chatID, "❌ Window scanner not enabled.")
		return
	}

	windows := b.windowScanner.GetActiveWindows()
	if len(windows) == 0 {
		b.sendText(chatID, "📊 No active prediction windows found. Markets may be closed.")
		return
	}

	asset := b.cfg.TradingAsset
	if asset == "" {
		asset = "BTC"
	}

	text := fmt.Sprintf("📊 *Active %s Windows* (%d)\n\n", asset, len(windows))

	for i, w := range windows {
		if i >= 5 {
			text += fmt.Sprintf("\n_...and %d more_", len(windows)-5)
			break
		}

		yesPrice, _ := w.YesPrice.Float64()
		noPrice, _ := w.NoPrice.Float64()
		volume, _ := w.Volume.Float64()

		text += fmt.Sprintf(`*%s*
├ UP: $%.3f | DOWN: $%.3f
├ Volume: $%.0f
└ Ends: %s

`,
			escapeMarkdown(w.Question),
			yesPrice, noPrice,
			volume,
			w.EndDate.Format("15:04"),
		)
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "refresh_windows"),
		),
	)

	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *Bot) cmdTrade(chatID int64, args string) {
	if b.trader == nil {
		b.sendText(chatID, "❌ Trading not enabled.")
		return
	}

	direction := strings.ToUpper(strings.TrimSpace(args))
	if direction != "UP" && direction != "DOWN" {
		b.sendText(chatID, "⚠️ Usage: /trade UP or /trade DOWN")
		return
	}

	trade, err := b.trader.ManualTrade(direction, b.cfg.BTCMaxBetSize)
	if err != nil {
		b.sendText(chatID, fmt.Sprintf("❌ Trade failed: %s", err.Error()))
		return
	}

	var emoji string
	if direction == "UP" {
		emoji = "🟢"
	} else {
		emoji = "🔴"
	}

	entryF, _ := trade.EntryPrice.Float64()
	amountF, _ := trade.Amount.Float64()

	text := fmt.Sprintf(`%s *Trade Executed*

*Direction:* %s
*Amount:* $%.2f
*Price:* $%.3f
*Market:* %s

_Trade ID: %s_`,
		emoji,
		direction,
		amountF,
		entryF,
		escapeMarkdown(trade.WindowID),
		trade.ID,
	)

	b.sendMarkdown(chatID, text)
}

func (b *Bot) cmdStats(chatID int64) {
	if b.trader == nil {
		b.sendText(chatID, "❌ Trading not enabled.")
		return
	}

	total, won, lost, profitDec, winRate := b.trader.GetStats()
	profit, _ := profitDec.Float64()

	var pnlEmoji string
	if profit > 0 {
		pnlEmoji = "🟢"
	} else if profit < 0 {
		pnlEmoji = "🔴"
	} else {
		pnlEmoji = "⚪"
	}

	asset := b.cfg.TradingAsset
	if asset == "" {
		asset = "BTC"
	}

	text := fmt.Sprintf(`📈 *%s Trading Statistics*

*Performance:*
%s Total P/L: $%.2f
├ Win Rate: %.1f%%
├ Total Trades: %d
├ Wins: %d
└ Losses: %d`,
		asset,
		pnlEmoji,
		profit,
		winRate*100,
		total,
		won,
		lost,
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", "refresh_stats"),
		),
	)

	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *Bot) cmdAutoTrade(chatID int64, args string) {
	if b.trader == nil {
		b.sendText(chatID, "❌ Trading not enabled.")
		return
	}

	arg := strings.ToLower(strings.TrimSpace(args))

	switch arg {
	case "on", "enable", "start":
		b.cfg.BTCAutoTrade = true
		b.sendText(chatID, "🟢 Auto-trading ENABLED. Bot will execute trades on strong signals.")
	case "off", "disable", "stop":
		b.cfg.BTCAutoTrade = false
		b.sendText(chatID, "🔴 Auto-trading DISABLED. Use /trade to execute manually.")
	default:
		status := "OFF"
		if b.cfg.BTCAutoTrade {
			status = "ON"
		}
		b.sendText(chatID, fmt.Sprintf("⚙️ Auto-trading is currently %s\n\nUsage: /autotrade on or /autotrade off", status))
	}
}

func (b *Bot) cmdSettings(chatID int64) {
	settings, _ := b.db.GetUserSettings(chatID)

	alertStatus := "🟢 ON"
	alertBtn := "🔕 Turn OFF"
	if !settings.AlertsEnabled {
		alertStatus = "🔴 OFF"
		alertBtn = "🔔 Turn ON"
	}

	text := fmt.Sprintf(`⚙️ *Settings*

*Signal Alerts:* %s

Alerts are sent when strong signals are detected.`,
		alertStatus,
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(alertBtn, "toggle_alerts"),
		),
	)

	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *Bot) cmdSubscribe(chatID int64) {
	settings, _ := b.db.GetUserSettings(chatID)
	settings.AlertsEnabled = true
	b.db.SaveUserSettings(settings)

	b.sendText(chatID, "🔔 Alerts enabled! You'll receive notifications for strong trading signals.")
}

func (b *Bot) cmdUnsubscribe(chatID int64) {
	settings, _ := b.db.GetUserSettings(chatID)
	settings.AlertsEnabled = false
	b.db.SaveUserSettings(settings)

	b.sendText(chatID, "🔕 Alerts disabled. Use /subscribe to re-enable.")
}

func (b *Bot) toggleAlerts(chatID int64) {
	settings, _ := b.db.GetUserSettings(chatID)
	settings.AlertsEnabled = !settings.AlertsEnabled
	b.db.SaveUserSettings(settings)

	if settings.AlertsEnabled {
		b.sendText(chatID, "🔔 Alerts enabled!")
	} else {
		b.sendText(chatID, "🔕 Alerts disabled!")
	}
}

// Alerts

func (b *Bot) sendSignalAlert(chatID int64, signal predictor.Signal) error {
	var emoji, dirText string
	if signal.Direction == "UP" {
		emoji = "🟢"
		dirText = "BULLISH"
	} else {
		emoji = "🔴"
		dirText = "BEARISH"
	}

	strength := "WEAK"
	absScore := absFloat(signal.TotalScore)
	if absScore >= 70 {
		strength = "STRONG 🔥"
	} else if absScore >= 40 {
		strength = "MODERATE"
	}

	asset := b.cfg.TradingAsset
	if asset == "" {
		asset = "BTC"
	}

	text := fmt.Sprintf(`%s *%s SIGNAL ALERT*

*Direction:* %s
*Score:* %.0f/100
*Strength:* %s
*Confidence:* %.0f%%

_Timestamp: %s_`,
		emoji,
		asset,
		dirText,
		absScore,
		strength,
		signal.Confidence,
		signal.Timestamp.Format("15:04:05"),
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🟢 Trade UP", "trade_up"),
			tgbotapi.NewInlineKeyboardButtonData("🔴 Trade DOWN", "trade_down"),
		),
	)

	return b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *Bot) sendTradeAlert(chatID int64, trade trading.BTCTrade) error {
	var emoji string
	if trade.Direction == "UP" {
		emoji = "🟢"
	} else {
		emoji = "🔴"
	}

	profit, _ := trade.Profit.Float64()
	amount, _ := trade.Amount.Float64()
	entry, _ := trade.EntryPrice.Float64()

	var resultText string
	if trade.Status == "completed" || trade.Status == "won" || trade.Status == "lost" {
		if profit > 0 {
			resultText = fmt.Sprintf("✅ WIN: +$%.2f", profit)
		} else {
			resultText = fmt.Sprintf("❌ LOSS: -$%.2f", -profit)
		}
	} else {
		resultText = "⏳ Pending resolution"
	}

	text := fmt.Sprintf(`%s *TRADE EXECUTED*

*Direction:* %s
*Amount:* $%.2f
*Entry:* $%.3f
*Result:* %s

_ID: %s_`,
		emoji,
		trade.Direction,
		amount,
		entry,
		resultText,
		trade.ID,
	)

	return b.sendMarkdown(chatID, text)
}

func (b *Bot) sendStartupMessage() {
	asset := b.cfg.TradingAsset
	if asset == "" {
		asset = "BTC"
	}

	text := fmt.Sprintf(`🟢 *Polybot Online*

%s prediction system active.
Use /signal to check current prediction.`, asset)

	b.sendMarkdown(b.cfg.TelegramChatID, text)
}

// Helpers

func (b *Bot) sendText(chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	_, err := b.api.Send(msg)
	return err
}

func (b *Bot) sendMarkdown(chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.DisableWebPagePreview = true
	_, err := b.api.Send(msg)
	return err
}

func (b *Bot) sendMarkdownWithKeyboard(chatID int64, text string, keyboard tgbotapi.InlineKeyboardMarkup) error {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.DisableWebPagePreview = true
	msg.ReplyMarkup = keyboard
	_, err := b.api.Send(msg)
	return err
}

func escapeMarkdown(s string) string {
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"[", "\\[",
		"]", "\\]",
		"`", "\\`",
	)
	return replacer.Replace(s)
}

func formatPrice(price float64) string {
	return fmt.Sprintf("%.2f", price)
}

func absFloat(n float64) float64 {
	if n < 0 {
		return -n
	}
	return n
}

// Legacy compatibility - these are needed for database types
func (b *Bot) setMinSpread(chatID int64, spread string) {
	settings, _ := b.db.GetUserSettings(chatID)
	if s, err := decimal.NewFromString(spread); err == nil {
		settings.MinSpreadPct = s
		b.db.SaveUserSettings(settings)
	}
}
