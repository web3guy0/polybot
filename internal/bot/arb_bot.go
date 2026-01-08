// Package bot provides Telegram bot functionality
//
// arb_bot.go - Modern Telegram bot for latency arbitrage trading
// Features: Inline keyboards, real-time stats, trade controls, settings management
// Supports: Multi-asset (BTC, ETH, SOL), CMC pricing, dynamic position sizing
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
	"github.com/web3guy0/polybot/internal/binance"
	"github.com/web3guy0/polybot/internal/cmc"
	"github.com/web3guy0/polybot/internal/config"
	"github.com/web3guy0/polybot/internal/database"
	"github.com/web3guy0/polybot/internal/polymarket"
)

var hundred = decimal.NewFromInt(100)

// Callback data prefixes
const (
	cbToggleLive     = "toggle_live"
	cbToggleAlerts   = "toggle_alerts"
	cbSetSize1       = "set_size_1"
	cbSetSize5       = "set_size_5"
	cbSetSize10      = "set_size_10"
	cbSetSize25      = "set_size_25"
	cbSetSize50      = "set_size_50"
	cbSetSize100     = "set_size_100"
	cbRefreshStatus  = "refresh_status"
	cbRefreshPrice   = "refresh_price"
	cbRefreshWindows = "refresh_windows"
	cbRefreshOpps    = "refresh_opps"
	cbRefreshStats   = "refresh_stats"
	cbRefreshAccount = "refresh_account"
	cbRefreshPositions = "refresh_positions"
	cbRefreshHistory = "refresh_history"
	cbShowSettings   = "show_settings"
	cbShowMain       = "show_main"
	cbConfirmLive    = "confirm_live"
	cbCancelLive     = "cancel_live"
)

// ArbBot handles Telegram interactions for the arbitrage system
type ArbBot struct {
	api           *tgbotapi.BotAPI
	cfg           *config.Config
	db            *database.Database
	binanceClient *binance.Client
	cmcClient     *cmc.Client // Multi-asset price feed
	windowScanner *polymarket.WindowScanner
	arbEngine     *arbitrage.Engine
	allEngines    []*arbitrage.Engine // All asset engines for multi-asset view
	clobClient    *arbitrage.CLOBClient

	// Runtime state (can be toggled via bot)
	isLive       bool
	alertsOn     bool
	positionSize decimal.Decimal
	stateMu      sync.RWMutex

	// Alert rate limiting
	lastOppAlert   time.Time
	lastTradeAlert time.Time

	stopCh chan struct{}
}

// NewArbBot creates a new arbitrage-focused Telegram bot
func NewArbBot(cfg *config.Config, db *database.Database,
	binanceClient *binance.Client, windowScanner *polymarket.WindowScanner,
	arbEngine *arbitrage.Engine, clobClient *arbitrage.CLOBClient) (*ArbBot, error) {

	api, err := tgbotapi.NewBotAPI(cfg.TelegramToken)
	if err != nil {
		return nil, fmt.Errorf("failed to create Telegram bot: %w", err)
	}

	log.Info().Str("username", api.Self.UserName).Msg("🤖 Telegram bot connected")

	bot := &ArbBot{
		api:           api,
		cfg:           cfg,
		db:            db,
		binanceClient: binanceClient,
		windowScanner: windowScanner,
		arbEngine:     arbEngine,
		clobClient:    clobClient,
		isLive:        !cfg.DryRun,
		alertsOn:      true,
		positionSize:  cfg.ArbPositionSize,
		stopCh:        make(chan struct{}),
	}

	// Set up callbacks
	if arbEngine != nil && cfg.TelegramChatID != 0 {
		arbEngine.SetOpportunityCallback(func(opp arbitrage.Opportunity) {
			bot.stateMu.RLock()
			alertsOn := bot.alertsOn
			bot.stateMu.RUnlock()

			if alertsOn && time.Since(bot.lastOppAlert) > 30*time.Second {
				bot.sendOpportunityAlert(cfg.TelegramChatID, opp)
				bot.lastOppAlert = time.Now()
			}
		})

		arbEngine.SetTradeCallback(func(trade arbitrage.Trade) {
			bot.sendTradeAlert(cfg.TelegramChatID, trade)
			bot.lastTradeAlert = time.Now()
		})
	}

	return bot, nil
}

// Start begins the bot's command listener
func (b *ArbBot) Start() {
	go b.listenForUpdates()

	if b.cfg.TelegramChatID != 0 {
		b.sendStartupMessage()
	}
}

// Stop stops the bot
func (b *ArbBot) Stop() {
	close(b.stopCh)
}

// SetCMCClient sets the CMC client for multi-asset pricing
func (b *ArbBot) SetCMCClient(client *cmc.Client) {
	b.cmcClient = client
}

// SetAllEngines sets all arbitrage engines and configures callbacks for each
func (b *ArbBot) SetAllEngines(engines []*arbitrage.Engine) {
	b.allEngines = engines
	
	// Set up callbacks for ALL engines (not just primary)
	for _, engine := range engines {
		asset := engine.GetAsset()
		
		// Opportunity callback (rate limited)
		engine.SetOpportunityCallback(func(opp arbitrage.Opportunity) {
			b.stateMu.RLock()
			alertsOn := b.alertsOn
			b.stateMu.RUnlock()

			if alertsOn && time.Since(b.lastOppAlert) > 30*time.Second {
				b.sendOpportunityAlert(b.cfg.TelegramChatID, opp)
				b.lastOppAlert = time.Now()
			}
		})
		
		// Trade callback - always fire AND save to database
		engine.SetTradeCallback(func(trade arbitrage.Trade) {
			// Send Telegram alert
			b.sendTradeAlert(b.cfg.TelegramChatID, trade)
			b.lastTradeAlert = time.Now()
			
			// Save to database
			b.saveArbTradeToDb(trade, asset)
		})
		
		log.Debug().Str("asset", asset).Msg("📊 Callbacks configured for engine")
	}
}

func (b *ArbBot) listenForUpdates() {
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

// ==================== MESSAGE HANDLERS ====================

func (b *ArbBot) handleMessage(msg *tgbotapi.Message) {
	chatID := msg.Chat.ID

	// Only respond to authorized user
	if b.cfg.TelegramChatID != 0 && chatID != b.cfg.TelegramChatID {
		b.sendText(chatID, "⛔ Unauthorized")
		return
	}

	if !msg.IsCommand() {
		return
	}

	switch msg.Command() {
	case "start":
		b.cmdStart(chatID)
	case "menu", "m":
		b.cmdMenu(chatID)
	case "status", "s":
		b.cmdStatus(chatID)
	case "price", "p":
		b.cmdPrice(chatID)
	case "windows", "w":
		b.cmdWindows(chatID)
	case "opps", "o":
		b.cmdOpportunities(chatID)
	case "stats":
		b.cmdStats(chatID)
	case "trades", "t":
		b.cmdTrades(chatID)
	case "account", "a":
		b.cmdAccount(chatID)
	case "positions", "pos":
		b.cmdPositions(chatID)
	case "history":
		b.cmdHistory(chatID)
	case "settings":
		b.cmdSettings(chatID)
	case "live":
		b.cmdToggleLive(chatID)
	case "all":
		b.cmdAllAssets(chatID)
	case "config", "cfg":
		b.cmdConfig(chatID)
	case "help", "h":
		b.cmdHelp(chatID)
	default:
		b.sendText(chatID, "❓ Unknown command. Use /menu")
	}
}

// ==================== CALLBACK HANDLERS ====================

func (b *ArbBot) handleCallback(cb *tgbotapi.CallbackQuery) {
	chatID := cb.Message.Chat.ID
	msgID := cb.Message.MessageID

	// Acknowledge callback
	callback := tgbotapi.NewCallback(cb.ID, "")
	b.api.Request(callback)

	switch cb.Data {
	case cbRefreshStatus:
		b.updateStatusMessage(chatID, msgID)
	case cbRefreshPrice:
		b.updatePriceMessage(chatID, msgID)
	case cbRefreshWindows:
		b.updateWindowsMessage(chatID, msgID)
	case cbRefreshOpps:
		b.updateOppsMessage(chatID, msgID)
	case cbRefreshStats:
		b.updateStatsMessage(chatID, msgID)
	case cbRefreshAccount:
		b.updateAccountMessage(chatID, msgID)
	case cbRefreshPositions:
		b.updatePositionsMessage(chatID, msgID)
	case cbRefreshHistory:
		b.updateHistoryMessage(chatID, msgID)
	case cbShowSettings:
		b.updateSettingsMessage(chatID, msgID)
	case cbShowMain:
		b.updateMainMenu(chatID, msgID)
	case cbToggleLive:
		b.handleToggleLive(chatID, msgID)
	case cbConfirmLive:
		b.confirmGoLive(chatID, msgID)
	case cbCancelLive:
		b.cancelGoLive(chatID, msgID)
	case cbToggleAlerts:
		b.toggleAlerts(chatID, msgID)
	case cbSetSize1:
		b.setPositionSize(chatID, msgID, 1)
	case cbSetSize5:
		b.setPositionSize(chatID, msgID, 5)
	case cbSetSize10:
		b.setPositionSize(chatID, msgID, 10)
	case cbSetSize25:
		b.setPositionSize(chatID, msgID, 25)
	case cbSetSize50:
		b.setPositionSize(chatID, msgID, 50)
	case cbSetSize100:
		b.setPositionSize(chatID, msgID, 100)
	case cbShowAll:
		b.updateAllAssetsMessage(chatID, msgID)
	case cbShowBTC:
		b.updateAssetMessage(chatID, msgID, "BTC")
	case cbShowETH:
		b.updateAssetMessage(chatID, msgID, "ETH")
	case cbShowSOL:
		b.updateAssetMessage(chatID, msgID, "SOL")
	case cbShowConfig:
		b.updateConfigMessage(chatID, msgID)
	}
}

// ==================== COMMANDS ====================

func (b *ArbBot) cmdStart(chatID int64) {
	msg := `⚡ *POLYBOT v4.0*
━━━━━━━━━━━━━━━━━━━━━

Multi-Asset Latency Arbitrage Bot

*Assets:* BTC • ETH • SOL
*Strategy:* Price moves → Odds stale → Buy winner → $1

*Features:*
• 🚀 Dynamic sizing (1x/2x/3x)
• 📊 CMC price feed (1s updates)
• 🛑 20% stop-loss protection

*Quick Commands:*
/menu - Main menu
/all - All assets status
/config - View settings

━━━━━━━━━━━━━━━━━━━━━
💡 Tap /menu to get started`

	b.sendMarkdown(chatID, msg)
}

func (b *ArbBot) cmdMenu(chatID int64) {
	b.stateMu.RLock()
	isLive := b.isLive
	b.stateMu.RUnlock()

	modeEmoji := "🧪"
	modeText := "DRY RUN"
	if isLive {
		modeEmoji = "💰"
		modeText = "LIVE"
	}

	btcPrice := b.binanceClient.GetCurrentPrice()
	windows := len(b.windowScanner.GetActiveWindows())
	opps := len(b.arbEngine.GetActiveOpportunities())

	text := fmt.Sprintf(`%s *POLYBOT* │ %s
━━━━━━━━━━━━━━━━━━━━━

💰 *BTC:* $%s
🎯 *Windows:* %d active
⚡ *Opportunities:* %d

━━━━━━━━━━━━━━━━━━━━━`,
		modeEmoji, modeText,
		btcPrice.StringFixed(0),
		windows,
		opps,
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Status", cbRefreshStatus),
			tgbotapi.NewInlineKeyboardButtonData("💰 Price", cbRefreshPrice),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🎯 Windows", cbRefreshWindows),
			tgbotapi.NewInlineKeyboardButtonData("⚡ Opps", cbRefreshOpps),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("👛 Account", cbRefreshAccount),
			tgbotapi.NewInlineKeyboardButtonData("📦 Positions", cbRefreshPositions),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🌐 All Assets", cbShowAll),
			tgbotapi.NewInlineKeyboardButtonData("📈 Stats", cbRefreshStats),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Settings", cbShowSettings),
		),
	)

	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *ArbBot) cmdStatus(chatID int64) {
	text, keyboard := b.buildStatusMessage()
	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *ArbBot) cmdPrice(chatID int64) {
	text, keyboard := b.buildPriceMessage()
	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *ArbBot) cmdWindows(chatID int64) {
	text, keyboard := b.buildWindowsMessage()
	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *ArbBot) cmdOpportunities(chatID int64) {
	text, keyboard := b.buildOppsMessage()
	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *ArbBot) cmdStats(chatID int64) {
	text, keyboard := b.buildStatsMessage()
	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *ArbBot) cmdTrades(chatID int64) {
	trades := b.arbEngine.GetRecentTrades(10)

	if len(trades) == 0 {
		b.sendMarkdown(chatID, "📭 *No trades yet*\n\nWaiting for opportunities...")
		return
	}

	var sb strings.Builder
	sb.WriteString("📜 *Recent Trades*\n━━━━━━━━━━━━━━━━━━━━━\n\n")

	for i, t := range trades {
		emoji := "📈"
		if t.Direction == "DOWN" {
			emoji = "📉"
		}

		statusEmoji := "⏳"
		if t.Status == "won" {
			statusEmoji = "✅"
		} else if t.Status == "lost" {
			statusEmoji = "❌"
		}

		sb.WriteString(fmt.Sprintf("*%d.* %s %s %s\n", i+1, emoji, t.Direction, statusEmoji))
		sb.WriteString(fmt.Sprintf("   💵 $%s @ %.0f¢\n", t.Amount.StringFixed(0), t.EntryPrice.Mul(hundred).InexactFloat64()))
		sb.WriteString(fmt.Sprintf("   📊 Edge: %s%%\n", t.Edge.Mul(hundred).StringFixed(1)))
		sb.WriteString(fmt.Sprintf("   🕐 %s\n\n", t.EnteredAt.Format("15:04:05")))
	}

	b.sendMarkdown(chatID, sb.String())
}

func (b *ArbBot) cmdAccount(chatID int64) {
	text, keyboard := b.buildAccountMessage()
	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *ArbBot) cmdPositions(chatID int64) {
	text, keyboard := b.buildPositionsMessage()
	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *ArbBot) cmdHistory(chatID int64) {
	text, keyboard := b.buildHistoryMessage()
	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *ArbBot) cmdSettings(chatID int64) {
	text, keyboard := b.buildSettingsMessage()
	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *ArbBot) cmdToggleLive(chatID int64) {
	b.stateMu.RLock()
	isLive := b.isLive
	b.stateMu.RUnlock()

	if isLive {
		// Going to dry run - no confirmation needed
		b.stateMu.Lock()
		b.isLive = false
		b.stateMu.Unlock()
		b.sendMarkdown(chatID, "🧪 *Switched to DRY RUN mode*\n\nNo real trades will be executed.")
	} else {
		// Going live - show confirmation
		b.showLiveConfirmation(chatID)
	}
}

func (b *ArbBot) cmdHelp(chatID int64) {
	msg := `📖 *Commands*
━━━━━━━━━━━━━━━━━━━━━

*Navigation:*
/menu - Main menu with buttons
/status - System health
/price - Current prices

*Multi-Asset:*
/all - All assets (BTC/ETH/SOL)
/config - View configuration

*Trading:*
/opps - Active opportunities
/windows - Prediction windows
/trades - Recent trade history
/stats - Performance stats

*Account:*
/account - Portfolio & balance
/positions - Open positions
/history - Last 10 trades

*Settings:*
/settings - Configure bot
/live - Toggle live/dry mode

*Shortcuts:*
/m - Menu  /s - Status  /p - Price
/o - Opps  /w - Windows /t - Trades
/a - Account  /pos - Positions

━━━━━━━━━━━━━━━━━━━━━
💡 All data refreshes in real-time`

	b.sendMarkdown(chatID, msg)
}

// ==================== MESSAGE BUILDERS ====================

func (b *ArbBot) buildStatusMessage() (string, tgbotapi.InlineKeyboardMarkup) {
	b.stateMu.RLock()
	isLive := b.isLive
	alertsOn := b.alertsOn
	posSize := b.positionSize
	b.stateMu.RUnlock()

	// Get BTC price from CMC first, fallback to Binance
	var btcPrice decimal.Decimal
	priceSource := "CMC"
	if b.cmcClient != nil {
		btcPrice = b.cmcClient.GetAssetPrice("BTC")
	}
	if btcPrice.IsZero() {
		btcPrice = b.binanceClient.GetCurrentPrice()
		priceSource = "Binance"
	}
	
	connected := !btcPrice.IsZero()
	
	// Count all active windows across engines
	totalWindows := 0
	for _, engine := range b.allEngines {
		if engine != nil {
			stats := engine.GetStats()
			if v, ok := stats["active_windows"]; ok {
				totalWindows += v.(int)
			}
		}
	}
	if totalWindows == 0 {
		totalWindows = len(b.windowScanner.GetActiveWindows())
	}
	
	stats := b.arbEngine.GetStats()

	modeEmoji := "🧪"
	modeText := "DRY RUN"
	if isLive {
		modeEmoji = "🔴"
		modeText := "LIVE"
		_ = modeText
	}

	connEmoji := "🟢"
	if !connected {
		connEmoji = "🔴"
	}

	alertEmoji := "🔔"
	if !alertsOn {
		alertEmoji = "🔕"
	}

	// Count active engines
	activeEngines := 0
	for _, engine := range b.allEngines {
		if engine != nil {
			activeEngines++
		}
	}

	text := fmt.Sprintf(`📊 *System Status*
━━━━━━━━━━━━━━━━━━━━━

*Mode:* %s %s
*Alerts:* %s %s
*Position Size:* $%s

*Price Feed:*
%s %s: $%s

*Markets:*
🌐 Active Engines: %d
🎯 Windows: %d
⚡ Opportunities: %v

*Performance:*
📈 Trades: %v
🏆 Win Rate: %v
💰 P/L: $%v

━━━━━━━━━━━━━━━━━━━━━
🕐 Updated: %s`,
		modeEmoji, modeText,
		alertEmoji, func() string {
			if alertsOn {
				return "ON"
			}
			return "OFF"
		}(),
		posSize.StringFixed(0),
		connEmoji, priceSource, btcPrice.StringFixed(0),
		activeEngines,
		totalWindows,
		stats["active_windows"],
		stats["total_trades"],
		stats["win_rate"],
		stats["total_profit"],
		time.Now().Format("15:04:05"),
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", cbRefreshStatus),
			tgbotapi.NewInlineKeyboardButtonData("🌐 All Assets", cbShowAll),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Settings", cbShowSettings),
			tgbotapi.NewInlineKeyboardButtonData("◀️ Menu", cbShowMain),
		),
	)

	return text, keyboard
}

func (b *ArbBot) buildPriceMessage() (string, tgbotapi.InlineKeyboardMarkup) {
	// Use CMC for all prices (more accurate than Binance)
	var btcPrice, ethPrice, solPrice decimal.Decimal
	priceSource := "CMC"
	
	if b.cmcClient != nil {
		btcPrice = b.cmcClient.GetAssetPrice("BTC")
		ethPrice = b.cmcClient.GetAssetPrice("ETH")
		solPrice = b.cmcClient.GetAssetPrice("SOL")
	}
	
	// Fallback to Binance for BTC only
	if btcPrice.IsZero() {
		btcPrice = b.binanceClient.GetCurrentPrice()
		priceSource = "Binance"
	}

	// Get change from Binance (still useful for trend)
	change1m, changePct1m := b.binanceClient.GetPriceChange(1 * time.Minute)
	change5m, changePct5m := b.binanceClient.GetPriceChange(5 * time.Minute)
	_, changePct15m := b.binanceClient.GetPriceChange(15 * time.Minute)

	getArrow := func(pct decimal.Decimal) string {
		if pct.IsPositive() {
			return "📈"
		} else if pct.IsNegative() {
			return "📉"
		}
		return "➡️"
	}

	var sb strings.Builder
	sb.WriteString("💰 *Crypto Prices*\n━━━━━━━━━━━━━━━━━━━━━\n\n")
	
	sb.WriteString(fmt.Sprintf("*₿ BTC:* $%s\n", btcPrice.StringFixed(0)))
	if !ethPrice.IsZero() {
		sb.WriteString(fmt.Sprintf("*Ξ ETH:* $%s\n", ethPrice.StringFixed(2)))
	}
	if !solPrice.IsZero() {
		sb.WriteString(fmt.Sprintf("*◎ SOL:* $%s\n", solPrice.StringFixed(2)))
	}
	
	sb.WriteString("\n*BTC Changes:*\n")
	sb.WriteString(fmt.Sprintf("%s 1m:  %s%% ($%s)\n", getArrow(changePct1m), changePct1m.StringFixed(3), change1m.Abs().StringFixed(0)))
	sb.WriteString(fmt.Sprintf("%s 5m:  %s%% ($%s)\n", getArrow(changePct5m), changePct5m.StringFixed(3), change5m.Abs().StringFixed(0)))
	sb.WriteString(fmt.Sprintf("%s 15m: %s%%\n", getArrow(changePct15m), changePct15m.StringFixed(3)))
	
	sb.WriteString("\n━━━━━━━━━━━━━━━━━━━━━\n")
	sb.WriteString(fmt.Sprintf("📊 Source: %s\n", priceSource))
	sb.WriteString(fmt.Sprintf("🕐 %s", time.Now().Format("15:04:05")))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", cbRefreshPrice),
			tgbotapi.NewInlineKeyboardButtonData("🌐 All Assets", cbShowAll),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Menu", cbShowMain),
		),
	)

	return sb.String(), keyboard
}

func (b *ArbBot) buildWindowsMessage() (string, tgbotapi.InlineKeyboardMarkup) {
	windows := b.windowScanner.GetActiveWindows()

	var sb strings.Builder
	sb.WriteString("🎯 *Active Windows*\n━━━━━━━━━━━━━━━━━━━━━\n\n")

	if len(windows) == 0 {
		sb.WriteString("📭 No active windows found\n")
	} else {
		for i, w := range windows {
			if i >= 5 {
				sb.WriteString(fmt.Sprintf("\n_...and %d more_", len(windows)-5))
				break
			}

			timeLeft := time.Until(w.EndDate)
			timeStr := fmt.Sprintf("%dm", int(timeLeft.Minutes()))
			if timeLeft < 0 {
				timeStr = "ended"
			}

			sb.WriteString(fmt.Sprintf("*%d.* %s\n", i+1, truncateStr(w.Question, 45)))
			sb.WriteString(fmt.Sprintf("   📈 Up: %.0f¢ │ 📉 Down: %.0f¢\n",
				w.YesPrice.Mul(hundred).InexactFloat64(),
				w.NoPrice.Mul(hundred).InexactFloat64(),
			))
			sb.WriteString(fmt.Sprintf("   ⏱ %s remaining\n\n", timeStr))
		}
	}

	sb.WriteString(fmt.Sprintf("━━━━━━━━━━━━━━━━━━━━━\n🕐 %s", time.Now().Format("15:04:05")))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", cbRefreshWindows),
			tgbotapi.NewInlineKeyboardButtonData("⚡ Opps", cbRefreshOpps),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Menu", cbShowMain),
		),
	)

	return sb.String(), keyboard
}

func (b *ArbBot) buildOppsMessage() (string, tgbotapi.InlineKeyboardMarkup) {
	opps := b.arbEngine.GetActiveOpportunities()
	btcPrice := b.binanceClient.GetCurrentPrice()

	var sb strings.Builder
	sb.WriteString("⚡ *Opportunities*\n━━━━━━━━━━━━━━━━━━━━━\n\n")

	if len(opps) == 0 {
		sb.WriteString(fmt.Sprintf("📭 No opportunities right now\n\n*BTC:* $%s\n\n", btcPrice.StringFixed(0)))
		sb.WriteString("_Waiting for:_\n")
		sb.WriteString("• BTC move >0.2%\n")
		sb.WriteString("• Stale Polymarket odds\n")
		sb.WriteString("• Edge >10%\n")
	} else {
		for i, opp := range opps {
			if i >= 3 {
				break
			}

			emoji := "📈"
			if opp.Direction == "DOWN" {
				emoji = "📉"
			}

			sb.WriteString(fmt.Sprintf("*%d. %s %s*\n", i+1, emoji, opp.Direction))
			sb.WriteString(fmt.Sprintf("   Move: %s%%\n", opp.PriceChangePct.StringFixed(2)))
			sb.WriteString(fmt.Sprintf("   Odds: %.0f¢ (fair: %.0f¢)\n",
				opp.MarketOdds.Mul(hundred).InexactFloat64(),
				opp.FairOdds.Mul(hundred).InexactFloat64(),
			))
			sb.WriteString(fmt.Sprintf("   *Edge: %s%%*\n\n", opp.Edge.Mul(hundred).StringFixed(1)))
		}
	}

	sb.WriteString(fmt.Sprintf("━━━━━━━━━━━━━━━━━━━━━\n🕐 %s", time.Now().Format("15:04:05")))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", cbRefreshOpps),
			tgbotapi.NewInlineKeyboardButtonData("💰 Price", cbRefreshPrice),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Menu", cbShowMain),
		),
	)

	return sb.String(), keyboard
}

func (b *ArbBot) buildStatsMessage() (string, tgbotapi.InlineKeyboardMarkup) {
	stats := b.arbEngine.GetStats()

	text := fmt.Sprintf(`📈 *Performance*
━━━━━━━━━━━━━━━━━━━━━

*Trades:*
📊 Total: %v
✅ Won: %v
❌ Lost: %v
🎯 Win Rate: %v

*Profit/Loss:*
💰 Total P/L: $%v
📅 Today: $%v

*Activity:*
🎯 Active Windows: %v

━━━━━━━━━━━━━━━━━━━━━
🕐 %s`,
		stats["total_trades"],
		stats["won"],
		stats["lost"],
		stats["win_rate"],
		stats["total_profit"],
		stats["daily_pl"],
		stats["active_windows"],
		time.Now().Format("15:04:05"),
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", cbRefreshStats),
			tgbotapi.NewInlineKeyboardButtonData("📜 Trades", "show_trades"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Menu", cbShowMain),
		),
	)

	return text, keyboard
}

func (b *ArbBot) buildSettingsMessage() (string, tgbotapi.InlineKeyboardMarkup) {
	b.stateMu.RLock()
	isLive := b.isLive
	alertsOn := b.alertsOn
	posSize := b.positionSize
	b.stateMu.RUnlock()

	modeEmoji := "🧪"
	modeText := "DRY RUN"
	modeBtn := "🔴 Go LIVE"
	if isLive {
		modeEmoji = "🔴"
		modeText = "LIVE"
		modeBtn = "🧪 Go DRY"
	}

	alertEmoji := "🔔"
	alertBtn := "🔕 Mute"
	if !alertsOn {
		alertEmoji = "🔕"
		alertBtn = "🔔 Unmute"
	}

	text := fmt.Sprintf(`⚙️ *Settings*
━━━━━━━━━━━━━━━━━━━━━

*Mode:* %s %s
*Alerts:* %s
*Position Size:* $%s

━━━━━━━━━━━━━━━━━━━━━

*Position Size:*
Tap to change trade size`,
		modeEmoji, modeText,
		alertEmoji,
		posSize.StringFixed(0),
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(modeBtn, cbToggleLive),
			tgbotapi.NewInlineKeyboardButtonData(alertBtn, cbToggleAlerts),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("$1", cbSetSize1),
			tgbotapi.NewInlineKeyboardButtonData("$5", cbSetSize5),
			tgbotapi.NewInlineKeyboardButtonData("$10", cbSetSize10),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("$25", cbSetSize25),
			tgbotapi.NewInlineKeyboardButtonData("$50", cbSetSize50),
			tgbotapi.NewInlineKeyboardButtonData("$100", cbSetSize100),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Menu", cbShowMain),
		),
	)

	return text, keyboard
}

func (b *ArbBot) buildAccountMessage() (string, tgbotapi.InlineKeyboardMarkup) {
	b.stateMu.RLock()
	isLive := b.isLive
	posSize := b.positionSize
	b.stateMu.RUnlock()

	// Get stats from engine
	stats := b.arbEngine.GetStats()
	dbStats, _ := b.db.GetStats()
	totalPnL, _ := b.db.GetTotalProfitLoss()

	// Try to get balance from CLOB
	var balanceStr string
	if b.clobClient != nil {
		balance, err := b.clobClient.GetBalance()
		if err != nil {
			log.Debug().Err(err).Msg("Failed to fetch balance")
			balanceStr = "⚠️ API Error"
		} else if !balance.IsZero() {
			balanceStr = fmt.Sprintf("$%s", balance.StringFixed(2))
		} else {
			balanceStr = "$0.00"
		}
	} else {
		balanceStr = "⚠️ Not configured"
	}

	modeEmoji := "🧪"
	modeText := "DRY RUN"
	if isLive {
		modeEmoji = "💰"
		modeText = "LIVE"
	}

	pnlEmoji := "📊"
	if totalPnL.IsPositive() {
		pnlEmoji = "📈"
	} else if totalPnL.IsNegative() {
		pnlEmoji = "📉"
	}

	// Extract values from stats map
	tradesTotal := int64(0)
	if v, ok := dbStats["total_trades"]; ok {
		tradesTotal = v.(int64)
	}
	totalTradesToday := 0
	if v, ok := stats["total_trades"]; ok {
		totalTradesToday = v.(int)
	}
	activeWindows := 0
	if v, ok := stats["active_windows"]; ok {
		activeWindows = v.(int)
	}

	text := fmt.Sprintf(`👛 *Account Overview*
━━━━━━━━━━━━━━━━━━━━━

*Mode:* %s %s
*Balance:* %s
*Position Size:* $%s

━━━━━━━━━━━━━━━━━━━━━

*Session Stats:*
🎯 Active Windows: %d
📝 Trades Today: %d
📊 Total Trades: %d

%s *P&L:* $%s

━━━━━━━━━━━━━━━━━━━━━
🕐 Updated: %s`,
		modeEmoji, modeText,
		balanceStr,
		posSize.StringFixed(0),
		activeWindows,
		totalTradesToday,
		tradesTotal,
		pnlEmoji, totalPnL.StringFixed(2),
		time.Now().Format("15:04:05"),
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📦 Positions", cbRefreshPositions),
			tgbotapi.NewInlineKeyboardButtonData("📜 History", cbRefreshHistory),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", cbRefreshAccount),
			tgbotapi.NewInlineKeyboardButtonData("◀️ Menu", cbShowMain),
		),
	)

	return text, keyboard
}

func (b *ArbBot) buildPositionsMessage() (string, tgbotapi.InlineKeyboardMarkup) {
	var sb strings.Builder
	sb.WriteString("📦 *Open Positions*\n━━━━━━━━━━━━━━━━━━━━━\n\n")

	// Try to get positions from CLOB
	if b.clobClient != nil {
		positions, err := b.clobClient.GetPositions()
		if err == nil && len(positions) > 0 {
			for i, pos := range positions {
				sideEmoji := "📈"
				if pos.Side == arbitrage.OrderSideSell {
					sideEmoji = "📉"
				}
				sb.WriteString(fmt.Sprintf("*%d.* %s %s\n", i+1, sideEmoji, string(pos.Side)))
				sb.WriteString(fmt.Sprintf("   📊 Size: %s\n", pos.Size.String()))
				sb.WriteString(fmt.Sprintf("   💰 Avg: %.2f¢\n", pos.AvgPrice.Mul(hundred).InexactFloat64()))
				sb.WriteString(fmt.Sprintf("   🔗 `%s...`\n\n", truncateStr(pos.TokenID, 12)))
			}
		} else if err != nil {
			sb.WriteString("⚠️ Failed to fetch positions\n\n")
			sb.WriteString(fmt.Sprintf("_Error: %s_\n", truncateStr(err.Error(), 50)))
		} else {
			sb.WriteString("📭 No open positions\n\n")
			sb.WriteString("_Positions will appear here when you have active trades_")
		}
	} else {
		sb.WriteString("⚠️ CLOB client not configured\n\n")
		sb.WriteString("_Set CLOB\\_API\\_KEY and CLOB\\_API\\_SECRET in .env_")
	}

	sb.WriteString("\n\n━━━━━━━━━━━━━━━━━━━━━")
	sb.WriteString(fmt.Sprintf("\n🕐 Updated: %s", time.Now().Format("15:04:05")))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("👛 Account", cbRefreshAccount),
			tgbotapi.NewInlineKeyboardButtonData("📜 History", cbRefreshHistory),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", cbRefreshPositions),
			tgbotapi.NewInlineKeyboardButtonData("◀️ Menu", cbShowMain),
		),
	)

	return sb.String(), keyboard
}

func (b *ArbBot) buildHistoryMessage() (string, tgbotapi.InlineKeyboardMarkup) {
	var sb strings.Builder
	sb.WriteString("📜 *Trade History* (Last 10)\n━━━━━━━━━━━━━━━━━━━━━\n\n")

	hasContent := false

	// Try to get Polymarket CLOB trades first
	if b.clobClient != nil {
		clobTrades, err := b.clobClient.GetTrades()
		if err != nil {
			log.Debug().Err(err).Msg("Failed to fetch CLOB trades")
		} else if len(clobTrades) > 0 {
			hasContent = true
			sb.WriteString("*🔗 Polymarket Trades:*\n\n")
			
			// Show last 10 trades
			limit := 10
			if len(clobTrades) < limit {
				limit = len(clobTrades)
			}
			
			for i := 0; i < limit; i++ {
				t := clobTrades[i]
				emoji := "📈"
				if t.Side == "SELL" {
					emoji = "📉"
				}
				
				statusEmoji := "✅"
				if t.Status != "MATCHED" && t.Status != "CONFIRMED" {
					statusEmoji = "⏳"
				}
				
				sb.WriteString(fmt.Sprintf("*%d.* %s %s %s\n", i+1, emoji, t.Side, statusEmoji))
				sb.WriteString(fmt.Sprintf("   💵 %s @ %s¢\n", t.Size, t.Price))
				if t.MatchTime != "" {
					sb.WriteString(fmt.Sprintf("   🕐 %s\n\n", t.MatchTime))
				} else {
					sb.WriteString("\n")
				}
			}
			
			sb.WriteString("━━━━━━━━━━━━━━━━━━━━━\n")
		}
	}

	// Get local session trades from arbitrage engine
	trades := b.arbEngine.GetRecentTrades(10)

	if len(trades) > 0 {
		hasContent = true
		sb.WriteString("*📊 Session Trades:*\n\n")
		wins := 0
		losses := 0

		for i, t := range trades {
			emoji := "📈"
			if t.Direction == "DOWN" {
				emoji = "📉"
			}

			statusEmoji := "⏳"
			if t.Status == "won" {
				statusEmoji = "✅"
				wins++
			} else if t.Status == "lost" {
				statusEmoji = "❌"
				losses++
			}

			sb.WriteString(fmt.Sprintf("*%d.* %s %s %s\n", i+1, emoji, t.Direction, statusEmoji))
			sb.WriteString(fmt.Sprintf("   💵 $%s @ %.0f¢", t.Amount.StringFixed(0), t.EntryPrice.Mul(hundred).InexactFloat64()))
			sb.WriteString(fmt.Sprintf(" │ Edge: %s%%\n", t.Edge.Mul(hundred).StringFixed(0)))
			sb.WriteString(fmt.Sprintf("   🕐 %s\n\n", t.EnteredAt.Format("15:04:05")))
		}

		sb.WriteString("━━━━━━━━━━━━━━━━━━━━━\n")
		sb.WriteString(fmt.Sprintf("*Summary:* ✅ %d wins │ ❌ %d losses\n", wins, losses))
		if wins+losses > 0 {
			winRate := float64(wins) / float64(wins+losses) * 100
			sb.WriteString(fmt.Sprintf("*Win Rate:* %.0f%%\n", winRate))
		}
	}
	
	if !hasContent {
		// No trades anywhere - show database stats
		dbStats, _ := b.db.GetStats()
		totalPnL, _ := b.db.GetTotalProfitLoss()
		
		totalTrades := int64(0)
		if v, ok := dbStats["total_trades"]; ok {
			totalTrades = v.(int64)
		}

		if totalTrades > 0 {
			sb.WriteString("📊 *All-Time Stats:*\n\n")
			sb.WriteString(fmt.Sprintf("📝 Total Trades: %d\n", totalTrades))
			
			pnlEmoji := "📊"
			if totalPnL.IsPositive() {
				pnlEmoji = "📈"
			} else if totalPnL.IsNegative() {
				pnlEmoji = "📉"
			}
			sb.WriteString(fmt.Sprintf("%s Total P&L: $%s\n", pnlEmoji, totalPnL.StringFixed(2)))
			sb.WriteString("\n_No trades in current session_\n")
			sb.WriteString("_Bot is running in DRY RUN mode_")
		} else {
			sb.WriteString("📭 *No trades yet*\n\n")
			sb.WriteString("_Waiting for trading activity..._\n")
			sb.WriteString("_Trades will appear here once executed_")
		}
	}

	sb.WriteString(fmt.Sprintf("\n\n━━━━━━━━━━━━━━━━━━━━━\n🕐 Updated: %s", time.Now().Format("15:04:05")))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("👛 Account", cbRefreshAccount),
			tgbotapi.NewInlineKeyboardButtonData("📦 Positions", cbRefreshPositions),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", cbRefreshHistory),
			tgbotapi.NewInlineKeyboardButtonData("◀️ Menu", cbShowMain),
		),
	)

	return sb.String(), keyboard
}

// ==================== UPDATE HANDLERS ====================

func (b *ArbBot) updateStatusMessage(chatID int64, msgID int) {
	text, keyboard := b.buildStatusMessage()
	b.editMessage(chatID, msgID, text, keyboard)
}

func (b *ArbBot) updatePriceMessage(chatID int64, msgID int) {
	text, keyboard := b.buildPriceMessage()
	b.editMessage(chatID, msgID, text, keyboard)
}

func (b *ArbBot) updateWindowsMessage(chatID int64, msgID int) {
	text, keyboard := b.buildWindowsMessage()
	b.editMessage(chatID, msgID, text, keyboard)
}

func (b *ArbBot) updateOppsMessage(chatID int64, msgID int) {
	text, keyboard := b.buildOppsMessage()
	b.editMessage(chatID, msgID, text, keyboard)
}

func (b *ArbBot) updateStatsMessage(chatID int64, msgID int) {
	text, keyboard := b.buildStatsMessage()
	b.editMessage(chatID, msgID, text, keyboard)
}

func (b *ArbBot) updateAccountMessage(chatID int64, msgID int) {
	text, keyboard := b.buildAccountMessage()
	b.editMessage(chatID, msgID, text, keyboard)
}

func (b *ArbBot) updatePositionsMessage(chatID int64, msgID int) {
	text, keyboard := b.buildPositionsMessage()
	b.editMessage(chatID, msgID, text, keyboard)
}

func (b *ArbBot) updateHistoryMessage(chatID int64, msgID int) {
	text, keyboard := b.buildHistoryMessage()
	b.editMessage(chatID, msgID, text, keyboard)
}

func (b *ArbBot) updateSettingsMessage(chatID int64, msgID int) {
	text, keyboard := b.buildSettingsMessage()
	b.editMessage(chatID, msgID, text, keyboard)
}

func (b *ArbBot) updateMainMenu(chatID int64, msgID int) {
	b.stateMu.RLock()
	isLive := b.isLive
	b.stateMu.RUnlock()

	modeEmoji := "🧪"
	modeText := "DRY RUN"
	if isLive {
		modeEmoji = "💰"
		modeText = "LIVE"
	}

	btcPrice := b.binanceClient.GetCurrentPrice()
	windows := len(b.windowScanner.GetActiveWindows())
	opps := len(b.arbEngine.GetActiveOpportunities())

	text := fmt.Sprintf(`%s *POLYBOT* │ %s
━━━━━━━━━━━━━━━━━━━━━

💰 *BTC:* $%s
🎯 *Windows:* %d active
⚡ *Opportunities:* %d

━━━━━━━━━━━━━━━━━━━━━`,
		modeEmoji, modeText,
		btcPrice.StringFixed(0),
		windows,
		opps,
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📊 Status", cbRefreshStatus),
			tgbotapi.NewInlineKeyboardButtonData("💰 Price", cbRefreshPrice),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🎯 Windows", cbRefreshWindows),
			tgbotapi.NewInlineKeyboardButtonData("⚡ Opps", cbRefreshOpps),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📈 Stats", cbRefreshStats),
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Settings", cbShowSettings),
		),
	)

	b.editMessage(chatID, msgID, text, keyboard)
}

// ==================== SETTINGS ACTIONS ====================

func (b *ArbBot) handleToggleLive(chatID int64, msgID int) {
	b.stateMu.RLock()
	isLive := b.isLive
	b.stateMu.RUnlock()

	if isLive {
		// Going to dry run
		b.stateMu.Lock()
		b.isLive = false
		b.stateMu.Unlock()
		b.updateSettingsMessage(chatID, msgID)
	} else {
		// Show confirmation
		b.showLiveConfirmationEdit(chatID, msgID)
	}
}

func (b *ArbBot) showLiveConfirmation(chatID int64) {
	b.stateMu.RLock()
	posSize := b.positionSize
	b.stateMu.RUnlock()

	text := fmt.Sprintf(`⚠️ *CONFIRM LIVE TRADING*
━━━━━━━━━━━━━━━━━━━━━

You are about to enable *LIVE TRADING*

*Position Size:* $%s per trade

This will use REAL MONEY.
Are you sure?

━━━━━━━━━━━━━━━━━━━━━`,
		posSize.StringFixed(0),
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Yes, Go LIVE", cbConfirmLive),
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", cbCancelLive),
		),
	)

	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

func (b *ArbBot) showLiveConfirmationEdit(chatID int64, msgID int) {
	b.stateMu.RLock()
	posSize := b.positionSize
	b.stateMu.RUnlock()

	text := fmt.Sprintf(`⚠️ *CONFIRM LIVE TRADING*
━━━━━━━━━━━━━━━━━━━━━

You are about to enable *LIVE TRADING*

*Position Size:* $%s per trade

This will use REAL MONEY.
Are you sure?

━━━━━━━━━━━━━━━━━━━━━`,
		posSize.StringFixed(0),
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Yes, Go LIVE", cbConfirmLive),
			tgbotapi.NewInlineKeyboardButtonData("❌ Cancel", cbCancelLive),
		),
	)

	b.editMessage(chatID, msgID, text, keyboard)
}

func (b *ArbBot) confirmGoLive(chatID int64, msgID int) {
	b.stateMu.Lock()
	b.isLive = true
	b.stateMu.Unlock()

	text := `🔴 *LIVE TRADING ENABLED*
━━━━━━━━━━━━━━━━━━━━━

Bot is now trading with REAL MONEY.

Stay safe! 🚀`

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Menu", cbShowMain),
		),
	)

	b.editMessage(chatID, msgID, text, keyboard)
	log.Warn().Msg("🔴 LIVE TRADING ENABLED via Telegram")
}

func (b *ArbBot) cancelGoLive(chatID int64, msgID int) {
	b.updateSettingsMessage(chatID, msgID)
}

func (b *ArbBot) toggleAlerts(chatID int64, msgID int) {
	b.stateMu.Lock()
	b.alertsOn = !b.alertsOn
	b.stateMu.Unlock()

	b.updateSettingsMessage(chatID, msgID)
}

func (b *ArbBot) setPositionSize(chatID int64, msgID int, size float64) {
	b.stateMu.Lock()
	b.positionSize = decimal.NewFromFloat(size)
	b.arbEngine.SetPositionSize(b.positionSize)
	b.stateMu.Unlock()

	b.updateSettingsMessage(chatID, msgID)
	log.Info().Float64("size", size).Msg("Position size updated via Telegram")
}

// ==================== ALERTS ====================

func (b *ArbBot) sendStartupMessage() {
	b.stateMu.RLock()
	isLive := b.isLive
	posSize := b.positionSize
	b.stateMu.RUnlock()

	modeEmoji := "🧪"
	modeText := "DRY RUN"
	if isLive {
		modeEmoji = "🔴"
		modeText = "LIVE"
	}

	// Get assets from engines
	var assets []string
	for _, engine := range b.allEngines {
		if engine != nil {
			assets = append(assets, engine.GetAsset())
		}
	}
	if len(assets) == 0 {
		assets = []string{b.cfg.TradingAsset}
	}

	text := fmt.Sprintf(`⚡ *Polybot v4.0 Started*
━━━━━━━━━━━━━━━━━━━━━

%s *Mode:* %s
💵 *Size:* $%s/trade
🌐 *Assets:* %s

*Dynamic Sizing:*
• 0.1-0.2%% → 1x
• 0.2-0.3%% → 2x
• >0.3%% → 3x

_Monitoring for opportunities..._

━━━━━━━━━━━━━━━━━━━━━
💡 /menu for controls`,
		modeEmoji, modeText,
		posSize.StringFixed(0),
		strings.Join(assets, ", "),
	)

	b.sendMarkdown(b.cfg.TelegramChatID, text)
}

func (b *ArbBot) sendOpportunityAlert(chatID int64, opp arbitrage.Opportunity) {
	emoji := "📈"
	if opp.Direction == "DOWN" {
		emoji = "📉"
	}

	text := fmt.Sprintf(`⚡ *OPPORTUNITY DETECTED*
━━━━━━━━━━━━━━━━━━━━━

%s *%s*

*Move:* %s%%
*Odds:* %.0f¢ → Fair: %.0f¢
*Edge:* %s%%

BTC: $%s → $%s

━━━━━━━━━━━━━━━━━━━━━`,
		emoji, opp.Direction,
		opp.PriceChangePct.StringFixed(2),
		opp.MarketOdds.Mul(hundred).InexactFloat64(),
		opp.FairOdds.Mul(hundred).InexactFloat64(),
		opp.Edge.Mul(hundred).StringFixed(1),
		opp.StartBTC.StringFixed(0),
		opp.CurrentBTC.StringFixed(0),
	)

	b.sendMarkdown(chatID, text)
}

// saveArbTradeToDb saves an arbitrage trade to the database
func (b *ArbBot) saveArbTradeToDb(trade arbitrage.Trade, asset string) {
	if b.db == nil {
		return
	}

	// Determine size multiplier from amount
	sizeMultiplier := "1x"
	baseSize := b.cfg.ArbPositionSize
	if !baseSize.IsZero() {
		ratio := trade.Amount.Div(baseSize)
		if ratio.GreaterThanOrEqual(decimal.NewFromFloat(2.5)) {
			sizeMultiplier = "3x"
		} else if ratio.GreaterThanOrEqual(decimal.NewFromFloat(1.5)) {
			sizeMultiplier = "2x"
		}
	}

	dbTrade := &database.ArbTrade{
		ID:             trade.ID,
		Asset:          asset,
		WindowID:       trade.WindowID,
		Question:       trade.Question,
		Direction:      trade.Direction,
		TokenID:        trade.TokenID,
		EntryPrice:     trade.EntryPrice,
		ExitPrice:      trade.ExitPrice,
		Amount:         trade.Amount,
		Shares:         trade.Shares,
		BTCAtEntry:     trade.BTCAtEntry,
		BTCAtStart:     trade.BTCAtStart,
		PriceChangePct: trade.PriceChangePct,
		Edge:           trade.Edge,
		SizeMultiplier: sizeMultiplier,
		Status:         trade.Status,
		ExitType:       trade.ExitType,
		Profit:         trade.Profit,
		EnteredAt:      trade.EnteredAt,
		ExitedAt:       trade.ExitedAt,
		ResolvedAt:     trade.ResolvedAt,
	}

	if err := b.db.SaveArbTrade(dbTrade); err != nil {
		log.Error().Err(err).Str("trade_id", trade.ID).Msg("Failed to save trade to database")
	} else {
		log.Debug().Str("trade_id", trade.ID).Str("asset", asset).Msg("💾 Trade saved to database")
	}
}

func (b *ArbBot) sendTradeAlert(chatID int64, trade arbitrage.Trade) {
	emoji := "📈"
	if trade.Direction == "DOWN" {
		emoji = "📉"
	}

	text := fmt.Sprintf(`🎯 *TRADE EXECUTED*
━━━━━━━━━━━━━━━━━━━━━

%s *%s* @ %.0f¢

*Size:* $%s
*Edge:* %s%%
*Move:* %s%%

━━━━━━━━━━━━━━━━━━━━━
⏳ Waiting for resolution...`,
		emoji, trade.Direction,
		trade.EntryPrice.Mul(hundred).InexactFloat64(),
		trade.Amount.StringFixed(0),
		trade.Edge.Mul(hundred).StringFixed(1),
		trade.PriceChangePct.StringFixed(2),
	)

	b.sendMarkdown(chatID, text)
}

// ==================== HELPERS ====================

func (b *ArbBot) sendText(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := b.api.Send(msg); err != nil {
		log.Error().Err(err).Msg("Failed to send message")
	}
}

func (b *ArbBot) sendMarkdown(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	if _, err := b.api.Send(msg); err != nil {
		log.Error().Err(err).Msg("Failed to send message")
	}
}

func (b *ArbBot) sendMarkdownWithKeyboard(chatID int64, text string, keyboard tgbotapi.InlineKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = keyboard
	if _, err := b.api.Send(msg); err != nil {
		log.Error().Err(err).Msg("Failed to send message")
	}
}

func (b *ArbBot) editMessage(chatID int64, msgID int, text string, keyboard tgbotapi.InlineKeyboardMarkup) {
	edit := tgbotapi.NewEditMessageText(chatID, msgID, text)
	edit.ParseMode = "Markdown"
	edit.ReplyMarkup = &keyboard
	if _, err := b.api.Send(edit); err != nil {
		log.Debug().Err(err).Msg("Failed to edit message")
	}
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// ==================== MULTI-ASSET UPDATE HANDLERS ====================

func (b *ArbBot) updateAllAssetsMessage(chatID int64, msgID int) {
	text, keyboard := b.buildAllAssetsMessage()
	b.editMessage(chatID, msgID, text, keyboard)
}

func (b *ArbBot) updateAssetMessage(chatID int64, msgID int, asset string) {
	text, keyboard := b.buildAssetDetailMessage(asset)
	b.editMessage(chatID, msgID, text, keyboard)
}

func (b *ArbBot) updateConfigMessage(chatID int64, msgID int) {
	text, keyboard := b.buildConfigMessage()
	b.editMessage(chatID, msgID, text, keyboard)
}
