// Package bot provides Telegram bot functionality
//
// multi_arb_bot.go - Multi-asset Telegram bot for latency arbitrage trading
// Features: BTC/ETH/SOL support, CMC prices, inline keyboards, dynamic sizing
package bot

import (
	"fmt"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/shopspring/decimal"
)

// Callback data for multi-asset
const (
	cbShowBTC    = "show_btc"
	cbShowETH    = "show_eth"
	cbShowSOL    = "show_sol"
	cbShowAll    = "show_all"
	cbShowConfig = "show_config"
)

// cmdAllAssets shows status for all assets
func (b *ArbBot) cmdAllAssets(chatID int64) {
	text, keyboard := b.buildAllAssetsMessage()
	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

// cmdConfig shows current configuration
func (b *ArbBot) cmdConfig(chatID int64) {
	text, keyboard := b.buildConfigMessage()
	b.sendMarkdownWithKeyboard(chatID, text, keyboard)
}

// buildAllAssetsMessage creates status for all assets
func (b *ArbBot) buildAllAssetsMessage() (string, tgbotapi.InlineKeyboardMarkup) {
	var sb strings.Builder
	sb.WriteString("🌐 *Multi-Asset Status*\n━━━━━━━━━━━━━━━━━━━━━\n\n")

	assets := []struct {
		name  string
		emoji string
	}{
		{"BTC", "₿"},
		{"ETH", "Ξ"},
		{"SOL", "◎"},
	}

	// Get prices from CMC
	if b.cmcClient != nil {
		for _, asset := range assets {
			price := b.cmcClient.GetAssetPrice(asset.name)
			if price.IsZero() {
				continue
			}

			// Find windows for this asset
			windowCount := 0
			var currentOdds string
			
			for _, engine := range b.allEngines {
				if engine != nil && engine.GetAsset() == asset.name {
					stats := engine.GetStats()
					if v, ok := stats["active_windows"]; ok {
						windowCount = v.(int)
					}
					// Get current odds from engine
					opps := engine.GetActiveOpportunities()
					if len(opps) > 0 {
						currentOdds = fmt.Sprintf("UP %.0f¢ / DOWN %.0f¢",
							opps[0].MarketOdds.Mul(hundred).InexactFloat64(),
							decimal.NewFromInt(1).Sub(opps[0].MarketOdds).Mul(hundred).InexactFloat64())
					}
				}
			}

			priceStr := price.StringFixed(2)
			if asset.name == "BTC" {
				priceStr = price.StringFixed(0)
			}

			sb.WriteString(fmt.Sprintf("*%s %s*\n", asset.emoji, asset.name))
			sb.WriteString(fmt.Sprintf("   💰 $%s\n", priceStr))
			sb.WriteString(fmt.Sprintf("   🎯 Windows: %d\n", windowCount))
			if currentOdds != "" {
				sb.WriteString(fmt.Sprintf("   📊 %s\n", currentOdds))
			}
			sb.WriteString("\n")
		}
	} else {
		// Fallback to Binance for BTC only
		btcPrice := b.binanceClient.GetCurrentPrice()
		sb.WriteString(fmt.Sprintf("*₿ BTC*: $%s\n", btcPrice.StringFixed(0)))
		sb.WriteString("_CMC client not available for ETH/SOL_\n")
	}

	sb.WriteString("━━━━━━━━━━━━━━━━━━━━━\n")
	sb.WriteString("📊 Source: CoinMarketCap\n")
	sb.WriteString(fmt.Sprintf("🕐 %s", time.Now().Format("15:04:05")))

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("₿ BTC", cbShowBTC),
			tgbotapi.NewInlineKeyboardButtonData("Ξ ETH", cbShowETH),
			tgbotapi.NewInlineKeyboardButtonData("◎ SOL", cbShowSOL),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", cbShowAll),
			tgbotapi.NewInlineKeyboardButtonData("◀️ Menu", cbShowMain),
		),
	)

	return sb.String(), keyboard
}

// buildConfigMessage shows current bot configuration
func (b *ArbBot) buildConfigMessage() (string, tgbotapi.InlineKeyboardMarkup) {
	b.stateMu.RLock()
	isLive := b.isLive
	alertsOn := b.alertsOn
	posSize := b.positionSize
	b.stateMu.RUnlock()

	modeEmoji := "🧪"
	modeText := "DRY RUN"
	if isLive {
		modeEmoji = "🔴"
		modeText := "LIVE"
		_ = modeText
	}

	// Get engine config
	var minMove, entryMin, entryMax, exitTarget, stopLoss string
	if b.arbEngine != nil {
		stats := b.arbEngine.GetStats()
		if v, ok := stats["min_move"]; ok {
			minMove = fmt.Sprintf("%v", v)
		}
		if v, ok := stats["entry_min"]; ok {
			entryMin = fmt.Sprintf("%.0f¢", v.(decimal.Decimal).Mul(hundred).InexactFloat64())
		}
		if v, ok := stats["entry_max"]; ok {
			entryMax = fmt.Sprintf("%.0f¢", v.(decimal.Decimal).Mul(hundred).InexactFloat64())
		}
		if v, ok := stats["exit_target"]; ok {
			exitTarget = fmt.Sprintf("%.0f¢", v.(decimal.Decimal).Mul(hundred).InexactFloat64())
		}
		if v, ok := stats["stop_loss"]; ok {
			stopLoss = fmt.Sprintf("%.0f%%", v.(decimal.Decimal).Mul(hundred).InexactFloat64())
		}
	}

	// Count active engines
	activeEngines := 0
	var assetList []string
	for _, engine := range b.allEngines {
		if engine != nil {
			activeEngines++
			assetList = append(assetList, engine.GetAsset())
		}
	}

	text := fmt.Sprintf(`⚙️ *Configuration*
━━━━━━━━━━━━━━━━━━━━━

*Mode:* %s %s
*Alerts:* %s
*Position Size:* $%s

*Assets:* %s
*Active Engines:* %d

━━━━━━━━━━━━━━━━━━━━━

*Entry Strategy:*
• Min Move: %s
• Entry Range: %s - %s
• Exit Target: %s
• Stop-Loss: %s

*Dynamic Sizing:*
• 0.1-0.2%% → 1x ($%s)
• 0.2-0.3%% → 2x ($%s)
• >0.3%% → 3x ($%s)

━━━━━━━━━━━━━━━━━━━━━
🕐 %s`,
		modeEmoji, modeText,
		func() string {
			if alertsOn {
				return "🔔 ON"
			}
			return "🔕 OFF"
		}(),
		posSize.StringFixed(0),
		strings.Join(assetList, ", "),
		activeEngines,
		minMove,
		entryMin, entryMax,
		exitTarget,
		stopLoss,
		posSize.StringFixed(0),
		posSize.Mul(decimal.NewFromInt(2)).StringFixed(0),
		posSize.Mul(decimal.NewFromInt(3)).StringFixed(0),
		time.Now().Format("15:04:05"),
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⚙️ Settings", cbShowSettings),
			tgbotapi.NewInlineKeyboardButtonData("🌐 Assets", cbShowAll),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("◀️ Menu", cbShowMain),
		),
	)

	return text, keyboard
}

// buildAssetDetailMessage shows detailed view for one asset
func (b *ArbBot) buildAssetDetailMessage(asset string) (string, tgbotapi.InlineKeyboardMarkup) {
	var sb strings.Builder
	
	emoji := "₿"
	switch asset {
	case "ETH":
		emoji = "Ξ"
	case "SOL":
		emoji = "◎"
	}

	sb.WriteString(fmt.Sprintf("%s *%s Status*\n━━━━━━━━━━━━━━━━━━━━━\n\n", emoji, asset))

	// Get price from CMC
	var price decimal.Decimal
	if b.cmcClient != nil {
		price = b.cmcClient.GetAssetPrice(asset)
	}
	if price.IsZero() && asset == "BTC" {
		price = b.binanceClient.GetCurrentPrice()
	}

	priceStr := price.StringFixed(2)
	if asset == "BTC" {
		priceStr = price.StringFixed(0)
	}

	sb.WriteString(fmt.Sprintf("*Price:* $%s\n", priceStr))
	sb.WriteString(fmt.Sprintf("*Source:* CMC\n\n"))

	// Find engine for this asset
	for _, engine := range b.allEngines {
		if engine != nil && engine.GetAsset() == asset {
			stats := engine.GetStats()
			
			windows := 0
			if v, ok := stats["active_windows"]; ok {
				windows = v.(int)
			}
			trades := 0
			if v, ok := stats["total_trades"]; ok {
				trades = v.(int)
			}
			winRate := "0%"
			if v, ok := stats["win_rate"]; ok {
				winRate = fmt.Sprintf("%v", v)
			}

			sb.WriteString("*Trading:*\n")
			sb.WriteString(fmt.Sprintf("🎯 Active Windows: %d\n", windows))
			sb.WriteString(fmt.Sprintf("📊 Trades Today: %d\n", trades))
			sb.WriteString(fmt.Sprintf("🏆 Win Rate: %s\n", winRate))

			// Get opportunities
			opps := engine.GetActiveOpportunities()
			if len(opps) > 0 {
				sb.WriteString("\n*Opportunities:*\n")
				for i, opp := range opps {
					if i >= 3 {
						break
					}
					oppEmoji := "📈"
					if opp.Direction == "DOWN" {
						oppEmoji = "📉"
					}
					sb.WriteString(fmt.Sprintf("%s %s: %.1f%% edge\n",
						oppEmoji, opp.Direction,
						opp.Edge.Mul(hundred).InexactFloat64()))
				}
			}
			break
		}
	}

	sb.WriteString("\n━━━━━━━━━━━━━━━━━━━━━\n")
	sb.WriteString(fmt.Sprintf("🕐 %s", time.Now().Format("15:04:05")))

	// Create callback for refresh
	refreshCb := cbShowBTC
	switch asset {
	case "ETH":
		refreshCb = cbShowETH
	case "SOL":
		refreshCb = cbShowSOL
	}

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("₿ BTC", cbShowBTC),
			tgbotapi.NewInlineKeyboardButtonData("Ξ ETH", cbShowETH),
			tgbotapi.NewInlineKeyboardButtonData("◎ SOL", cbShowSOL),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Refresh", refreshCb),
			tgbotapi.NewInlineKeyboardButtonData("◀️ Menu", cbShowMain),
		),
	)

	return sb.String(), keyboard
}
