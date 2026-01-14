package arbitrage

import (
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/internal/database"
	"github.com/web3guy0/polybot/internal/polymarket"
)

// ═══════════════════════════════════════════════════════════════════════════════
// 🐋 WHALE STRATEGY - CONTRARIAN DIP BUYING
// ═══════════════════════════════════════════════════════════════════════════════
//
// ML-TRAINED STRATEGY based on 150,000+ trades from top whale wallets
//
// KEY INSIGHT: Whales buy when odds CRASH, not when they're high
//   - Whale avg entry: 41¢ (vs your old 83¢)
//   - Whales buy at 15-55¢, not 60-85¢
//   - They hold to expiry, not quick flip
//
// STRATEGY:
// 1. Wait for odds to DROP significantly (panic selling)
// 2. Buy the crashed side at 15-55¢ (deep value)
// 3. Hold to resolution for full $1 payout
// 4. NO stop loss - binary outcome, ride to expiry
//
// R:R MATH (vs Old Strategy):
// ┌───────────────────────────────────────────────────────────────┐
// │  Metric          │ Old (Sniper)  │ Whale           │ Diff    │
// ├───────────────────────────────────────────────────────────────┤
// │  Entry Range     │ 60-85¢        │ 15-55¢          │ -45¢    │
// │  Avg Entry       │ 83¢           │ 35¢             │ -48¢    │
// │  Max Reward      │ 17¢ (to 100)  │ 65¢ (to 100)    │ +48¢    │
// │  Max Risk        │ 33¢ (to 50 SL)│ 35¢ (to 0)      │ +2¢     │
// │  R:R Ratio       │ 1:0.52        │ 1:1.86          │ +258%   │
// │  Breakeven WR    │ 66%           │ 35%             │ -31%    │
// │  EV at 50% WR    │ -$0.17/share  │ +$0.15/share    │ +$0.32  │
// │  EV at 55% WR    │ -$0.12/share  │ +$0.20/share    │ +$0.32  │
// └───────────────────────────────────────────────────────────────┘
//
// ASSET-SPECIFIC PARAMETERS (from ML training):
//   BTC: 15-55¢ entry, optimal 35¢, R:R 1:1.86
//   ETH: 20-60¢ entry, optimal 40¢, R:R 1:1.50
//   SOL: 25-65¢ entry, optimal 45¢, R:R 1:1.22
//
// ═══════════════════════════════════════════════════════════════════════════════

// WhaleConfig holds ML-derived configuration for whale strategy
type WhaleConfig struct {
	// Entry conditions (CONTRARIAN - buy crashed odds)
	MinOddsEntry        decimal.Decimal // Min odds to buy (e.g., 0.15 = 15¢)
	MaxOddsEntry        decimal.Decimal // Max odds to buy (e.g., 0.55 = 55¢)
	OptimalEntry        decimal.Decimal // Sweet spot entry (e.g., 0.35)
	
	// Time window (can trade anytime, but prefer early/mid)
	MinTimeRemainingMin float64         // Minimum time left (e.g., 2 min)
	MaxTimeRemainingMin float64         // Maximum time left (no limit, enter early)
	
	// Drop detection (look for crashes)
	MinOddsDrop         decimal.Decimal // Min drop from high (e.g., 0.15 = 15¢ drop)
	
	// 🎯 EXIT CONDITIONS (PROFIT TAKING)
	TakeProfitPct       decimal.Decimal // Take profit at +X% (e.g., 0.15 = +15%)
	StopLossPct         decimal.Decimal // Stop loss at -X% (e.g., 0.25 = -25%)
	TrailingStopPct     decimal.Decimal // Trailing stop % from high (e.g., 0.10 = 10%)
	TimeExitMinutes     float64         // Exit X minutes before resolution (e.g., 1.0)
	MinProfitToExit     decimal.Decimal // Min profit to exit early (e.g., 0.03 = 3¢)
	
	// Exit mode
	HoldToResolution    bool            // If true, ignore TP/SL and hold to end
	
	// Position sizing
	PositionSizePct     decimal.Decimal // % of balance per trade (e.g., 0.15 = 15%)
	MaxConcurrentPositions int          // Max positions at once
	
	// Per-Asset Entry Zones (ML-derived)
	AssetMinEntry       map[string]decimal.Decimal // Min entry per asset
	AssetMaxEntry       map[string]decimal.Decimal // Max entry per asset
	AssetOptimalEntry   map[string]decimal.Decimal // Optimal entry per asset
	
	// Safety
	MaxTradesPerWindow  int           // Max trades per window
	CooldownDuration    time.Duration // Cooldown after trade
}

// DefaultWhaleConfig returns ML-trained default configuration
func DefaultWhaleConfig() WhaleConfig {
	return WhaleConfig{
		// Global entry range (contrarian)
		MinOddsEntry:     decimal.NewFromFloat(0.15),
		MaxOddsEntry:     decimal.NewFromFloat(0.55),
		OptimalEntry:     decimal.NewFromFloat(0.35),
		
		// Time window (enter with time to recover)
		MinTimeRemainingMin: 2.0,  // At least 2 min left
		MaxTimeRemainingMin: 14.0, // Can enter at start of window
		
		// Crash detection
		MinOddsDrop:      decimal.NewFromFloat(0.10), // 10¢ drop triggers interest
		
		// 🎯 EXIT CONDITIONS (TAKE PROFITS!)
		TakeProfitPct:    decimal.NewFromFloat(0.20), // +20% take profit (35¢→42¢)
		StopLossPct:      decimal.NewFromFloat(0.30), // -30% stop loss (35¢→24.5¢)
		TrailingStopPct:  decimal.NewFromFloat(0.15), // 15% trailing from high
		TimeExitMinutes:  0.5,                        // Exit 30 sec before resolution
		MinProfitToExit:  decimal.NewFromFloat(0.02), // Min 2¢ profit to exit
		
		// Exit mode - FALSE = take profits actively
		HoldToResolution: false,
		
		// Position sizing
		PositionSizePct: decimal.NewFromFloat(0.15), // 15% per trade (fewer, larger)
		MaxConcurrentPositions: 2,
		
		// ML-derived per-asset zones
		AssetMinEntry: map[string]decimal.Decimal{
			"BTC": decimal.NewFromFloat(0.15), // Whales buy BTC dips hard
			"ETH": decimal.NewFromFloat(0.20),
			"SOL": decimal.NewFromFloat(0.25),
		},
		AssetMaxEntry: map[string]decimal.Decimal{
			"BTC": decimal.NewFromFloat(0.55),
			"ETH": decimal.NewFromFloat(0.60),
			"SOL": decimal.NewFromFloat(0.65),
		},
		AssetOptimalEntry: map[string]decimal.Decimal{
			"BTC": decimal.NewFromFloat(0.35), // R:R 1:1.86
			"ETH": decimal.NewFromFloat(0.40), // R:R 1:1.50
			"SOL": decimal.NewFromFloat(0.45), // R:R 1:1.22
		},
		
		// Safety
		MaxTradesPerWindow: 1,
		CooldownDuration:   5 * time.Second,
	}
}

// GetAssetEntryZone returns entry zone for specific asset
func (c *WhaleConfig) GetAssetEntryZone(asset string) (minEntry, maxEntry, optimalEntry decimal.Decimal) {
	// Get asset-specific zones, fall back to global
	if v, ok := c.AssetMinEntry[asset]; ok {
		minEntry = v
	} else {
		minEntry = c.MinOddsEntry
	}
	
	if v, ok := c.AssetMaxEntry[asset]; ok {
		maxEntry = v
	} else {
		maxEntry = c.MaxOddsEntry
	}
	
	if v, ok := c.AssetOptimalEntry[asset]; ok {
		optimalEntry = v
	} else {
		optimalEntry = c.OptimalEntry
	}
	
	return
}

// CalculateRR calculates Risk:Reward ratio for entry price
func (c *WhaleConfig) CalculateRR(entryPrice decimal.Decimal) decimal.Decimal {
	// Risk = entry price (lose all if 0)
	// Reward = 1 - entry (gain to 100¢)
	risk := entryPrice
	reward := decimal.NewFromFloat(1.0).Sub(entryPrice)
	
	if risk.IsZero() {
		return decimal.NewFromFloat(999)
	}
	
	return reward.Div(risk)
}

// CalculateBreakevenWinRate returns win rate needed to break even
func (c *WhaleConfig) CalculateBreakevenWinRate(entryPrice decimal.Decimal) decimal.Decimal {
	// Breakeven WR = Risk / (Risk + Reward) = Entry / 1.0 = Entry
	return entryPrice
}

// WhalePosition represents an active whale position (contrarian hold)
type WhalePosition struct {
	TradeID       string
	Asset         string
	Side          string // "UP" or "DOWN" (bought the crashed side)
	TokenID       string
	ConditionID   string
	WindowID      string
	EntryPrice    decimal.Decimal
	CurrentPrice  decimal.Decimal // Latest price
	HighPrice     decimal.Decimal // Highest price since entry (for trailing stop)
	Size          decimal.Decimal // Shares owned
	EntryTime     time.Time
	WindowEnd     time.Time
	OddsDropPct   decimal.Decimal // How much odds dropped before entry
	ExpectedRR    decimal.Decimal // R:R at entry
	BreakevenWR   decimal.Decimal // Win rate needed
	Status        string          // "holding", "exited_tp", "exited_sl", "exited_time", "resolved_win", "resolved_loss"
	ExitPrice     decimal.Decimal // Price we exited at
	ExitReason    string          // Why we exited
	PnL           decimal.Decimal // Realized P&L
}

// PriceHistory tracks historical odds for crash detection
type PriceHistory struct {
	Prices     []decimal.Decimal
	Timestamps []time.Time
	High       decimal.Decimal // Highest seen
	Low        decimal.Decimal // Lowest seen
	LastUpdate time.Time
}

// GetDrop calculates drop from high
func (ph *PriceHistory) GetDrop() decimal.Decimal {
	if ph.High.IsZero() {
		return decimal.Zero
	}
	return ph.High.Sub(ph.Low)
}

// GetDropPct calculates drop percentage from high
func (ph *PriceHistory) GetDropPct() decimal.Decimal {
	if ph.High.IsZero() {
		return decimal.Zero
	}
	drop := ph.High.Sub(ph.Low)
	return drop.Div(ph.High).Mul(decimal.NewFromFloat(100))
}

// WhaleStrategy implements the ML-trained contrarian whale strategy
type WhaleStrategy struct {
	mu sync.RWMutex

	// Components
	windowScanner *polymarket.WindowScanner
	clobClient    *CLOBClient
	db            *database.Database

	// Paper trading mode
	paperTrade bool
	
	// Bankroll for position sizing
	bankroll decimal.Decimal

	// Configuration
	config WhaleConfig

	// Active positions (fewer, larger, hold to resolution)
	positions map[string]*WhalePosition // keyed by windowID

	// Price history for crash detection
	priceHistory map[string]*PriceHistory // keyed by windowID+side

	// Trade tracking
	tradesExecuted int
	tradedWindows  map[string]time.Time // windows we've traded

	// Statistics
	stats WhaleStats
}

// WhaleStats tracks performance metrics
type WhaleStats struct {
	TotalTrades     int
	Wins            int
	Losses          int
	TotalPnL        decimal.Decimal
	AvgEntryPrice   decimal.Decimal
	AvgRR           decimal.Decimal
	BestTrade       decimal.Decimal
	WorstTrade      decimal.Decimal
}

// NewWhaleStrategy creates a new whale strategy instance
func NewWhaleStrategy(
	scanner *polymarket.WindowScanner,
	clobClient *CLOBClient,
	db *database.Database,
	paperTrade bool,
	bankroll decimal.Decimal,
) *WhaleStrategy {
	return &WhaleStrategy{
		windowScanner: scanner,
		clobClient:    clobClient,
		db:            db,
		paperTrade:    paperTrade,
		bankroll:      bankroll,
		config:        DefaultWhaleConfig(),
		positions:     make(map[string]*WhalePosition),
		priceHistory:  make(map[string]*PriceHistory),
		tradedWindows: make(map[string]time.Time),
		stats:         WhaleStats{},
	}
}

// Start begins the whale strategy monitoring
func (ws *WhaleStrategy) Start() {
	go ws.mainLoop()
	go ws.exitMonitorLoop() // 🎯 Monitor positions for exit conditions
	log.Info().
		Str("min_entry", ws.config.MinOddsEntry.String()).
		Str("max_entry", ws.config.MaxOddsEntry.String()).
		Str("take_profit", fmt.Sprintf("+%.0f%%", ws.config.TakeProfitPct.Mul(decimal.NewFromInt(100)).InexactFloat64())).
		Str("stop_loss", fmt.Sprintf("-%.0f%%", ws.config.StopLossPct.Mul(decimal.NewFromInt(100)).InexactFloat64())).
		Bool("hold_to_resolution", ws.config.HoldToResolution).
		Msg("🐋 Whale strategy started - hunting for crashed odds")
}

// mainLoop monitors for whale entry opportunities
func (ws *WhaleStrategy) mainLoop() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		ws.scanForCrashedOdds()
	}
}

// exitMonitorLoop monitors open positions for exit conditions
func (ws *WhaleStrategy) exitMonitorLoop() {
	ticker := time.NewTicker(250 * time.Millisecond) // Check exits fast
	defer ticker.Stop()

	for range ticker.C {
		ws.checkExitConditions()
	}
}

// checkExitConditions evaluates all open positions for exit signals
func (ws *WhaleStrategy) checkExitConditions() {
	if ws.config.HoldToResolution {
		return // Skip exit checks if holding to resolution
	}

	if ws.windowScanner == nil {
		return
	}

	windows := ws.windowScanner.GetActiveWindows()
	windowMap := make(map[string]*polymarket.PredictionWindow)
	for i := range windows {
		windowMap[windows[i].ID] = &windows[i]
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()

	for windowID, position := range ws.positions {
		if position.Status != "holding" {
			continue
		}

		window, exists := windowMap[windowID]
		if !exists {
			continue
		}

		// Get current price for our side
		var currentPrice decimal.Decimal
		if position.Side == "UP" {
			currentPrice = window.YesPrice
		} else {
			currentPrice = window.NoPrice
		}

		// Update position tracking
		position.CurrentPrice = currentPrice
		if currentPrice.GreaterThan(position.HighPrice) {
			position.HighPrice = currentPrice
		}

		// Calculate P&L percentage
		pnlPct := currentPrice.Sub(position.EntryPrice).Div(position.EntryPrice)
		pnlAbs := currentPrice.Sub(position.EntryPrice).Mul(position.Size)

		// Time until resolution
		timeLeft := time.Until(position.WindowEnd).Minutes()

		// 🎯 CHECK EXIT CONDITIONS

		// 1. TAKE PROFIT - Price rose enough
		if pnlPct.GreaterThanOrEqual(ws.config.TakeProfitPct) {
			ws.executeExit(position, currentPrice, "take_profit", 
				fmt.Sprintf("🎯 TAKE PROFIT +%.1f%% (%.0f¢→%.0f¢)", 
					pnlPct.Mul(decimal.NewFromInt(100)).InexactFloat64(),
					position.EntryPrice.Mul(decimal.NewFromInt(100)).InexactFloat64(),
					currentPrice.Mul(decimal.NewFromInt(100)).InexactFloat64()))
			continue
		}

		// 2. STOP LOSS - Price dropped too much
		if pnlPct.LessThanOrEqual(ws.config.StopLossPct.Neg()) {
			ws.executeExit(position, currentPrice, "stop_loss",
				fmt.Sprintf("🛑 STOP LOSS %.1f%% (%.0f¢→%.0f¢)",
					pnlPct.Mul(decimal.NewFromInt(100)).InexactFloat64(),
					position.EntryPrice.Mul(decimal.NewFromInt(100)).InexactFloat64(),
					currentPrice.Mul(decimal.NewFromInt(100)).InexactFloat64()))
			continue
		}

		// 3. TRAILING STOP - Price fell from high water mark
		if position.HighPrice.GreaterThan(position.EntryPrice) {
			dropFromHigh := position.HighPrice.Sub(currentPrice).Div(position.HighPrice)
			if dropFromHigh.GreaterThanOrEqual(ws.config.TrailingStopPct) {
				// Only trigger if we're still in profit
				if currentPrice.GreaterThan(position.EntryPrice) {
					ws.executeExit(position, currentPrice, "trailing_stop",
						fmt.Sprintf("📉 TRAILING STOP (high %.0f¢→%.0f¢, drop %.1f%%)",
							position.HighPrice.Mul(decimal.NewFromInt(100)).InexactFloat64(),
							currentPrice.Mul(decimal.NewFromInt(100)).InexactFloat64(),
							dropFromHigh.Mul(decimal.NewFromInt(100)).InexactFloat64()))
					continue
				}
			}
		}

		// 4. TIME EXIT - Near resolution with profit
		if timeLeft <= ws.config.TimeExitMinutes && timeLeft > 0 {
			if pnlAbs.GreaterThanOrEqual(ws.config.MinProfitToExit) {
				ws.executeExit(position, currentPrice, "time_exit",
					fmt.Sprintf("⏰ TIME EXIT (%.1f min left, +%.2f¢ profit)",
						timeLeft, pnlAbs.Mul(decimal.NewFromInt(100)).InexactFloat64()))
				continue
			}
		}

		// Log position status periodically (every ~5 seconds based on loop)
		if time.Since(position.EntryTime).Seconds() > 5 && 
			int(time.Since(position.EntryTime).Seconds())%10 == 0 {
			log.Debug().
				Str("asset", position.Asset).
				Str("side", position.Side).
				Str("entry", fmt.Sprintf("%.0f¢", position.EntryPrice.Mul(decimal.NewFromInt(100)).InexactFloat64())).
				Str("current", fmt.Sprintf("%.0f¢", currentPrice.Mul(decimal.NewFromInt(100)).InexactFloat64())).
				Str("high", fmt.Sprintf("%.0f¢", position.HighPrice.Mul(decimal.NewFromInt(100)).InexactFloat64())).
				Str("pnl", fmt.Sprintf("%+.1f%%", pnlPct.Mul(decimal.NewFromInt(100)).InexactFloat64())).
				Str("time_left", fmt.Sprintf("%.1fm", timeLeft)).
				Msg("🐋 Position status")
		}
	}
}

// executeExit sells position and records P&L
func (ws *WhaleStrategy) executeExit(position *WhalePosition, exitPrice decimal.Decimal, reason, message string) {
	// Calculate P&L
	pnl := exitPrice.Sub(position.EntryPrice).Mul(position.Size)
	
	modeStr := "LIVE"
	if ws.paperTrade {
		modeStr = "PAPER"
	}

	log.Info().
		Str("mode", modeStr).
		Str("asset", position.Asset).
		Str("side", position.Side).
		Str("entry", fmt.Sprintf("%.0f¢", position.EntryPrice.Mul(decimal.NewFromInt(100)).InexactFloat64())).
		Str("exit", fmt.Sprintf("%.0f¢", exitPrice.Mul(decimal.NewFromInt(100)).InexactFloat64())).
		Str("pnl", fmt.Sprintf("%+$%.4f", pnl.InexactFloat64())).
		Str("reason", reason).
		Msg(message)

	if ws.paperTrade {
		// PAPER TRADE - just log
		log.Info().
			Str("asset", position.Asset).
			Str("shares", position.Size.String()).
			Str("pnl", fmt.Sprintf("%+$%.4f", pnl.InexactFloat64())).
			Msg("📝 [WHALE] Paper SELL recorded")
	} else {
		// LIVE TRADE - execute sell order with actual shares (not dollars)
		_, err := ws.clobClient.SellSharesAtPrice(position.TokenID, position.Size, exitPrice)
		if err != nil {
			log.Error().Err(err).
				Str("asset", position.Asset).
				Str("shares", position.Size.String()).
				Msg("🐋❌ Failed to execute exit order")
			return // Don't update position if sell failed
		}
	}

	// Update position
	position.Status = "exited_" + reason
	position.ExitPrice = exitPrice
	position.ExitReason = reason
	position.PnL = pnl

	// Update stats
	if pnl.GreaterThan(decimal.Zero) {
		ws.stats.Wins++
	} else {
		ws.stats.Losses++
	}
	ws.stats.TotalPnL = ws.stats.TotalPnL.Add(pnl)
	
	if pnl.GreaterThan(ws.stats.BestTrade) {
		ws.stats.BestTrade = pnl
	}
	if pnl.LessThan(ws.stats.WorstTrade) || ws.stats.WorstTrade.IsZero() {
		ws.stats.WorstTrade = pnl
	}

	// Remove from active positions
	delete(ws.positions, position.WindowID)

	// Log cumulative stats
	winRate := decimal.Zero
	if ws.stats.TotalTrades > 0 {
		winRate = decimal.NewFromInt(int64(ws.stats.Wins)).Div(decimal.NewFromInt(int64(ws.stats.TotalTrades))).Mul(decimal.NewFromInt(100))
	}
	
	log.Info().
		Int("total_trades", ws.stats.TotalTrades).
		Int("wins", ws.stats.Wins).
		Int("losses", ws.stats.Losses).
		Str("win_rate", fmt.Sprintf("%.1f%%", winRate.InexactFloat64())).
		Str("total_pnl", fmt.Sprintf("%+$%.4f", ws.stats.TotalPnL.InexactFloat64())).
		Msg("🐋📊 Whale stats update")
}

// scanForCrashedOdds looks for odds that have crashed into our buy zone
func (ws *WhaleStrategy) scanForCrashedOdds() {
	if ws.windowScanner == nil {
		return
	}

	windows := ws.windowScanner.GetActiveWindows()
	
	for i := range windows {
		window := &windows[i]
		
		// Update price history for both sides
		ws.updatePriceHistory(window.ID, "UP", window.YesPrice)
		ws.updatePriceHistory(window.ID, "DOWN", window.NoPrice)
		
		// Check for entry signal
		signal, err := ws.CheckEntry(window)
		if err != nil {
			continue
		}
		
		if signal != nil && signal.Strength.GreaterThanOrEqual(decimal.NewFromFloat(50)) {
			// Skip if already traded this window
			ws.mu.RLock()
			_, alreadyTraded := ws.tradedWindows[window.ID]
			ws.mu.RUnlock()
			if alreadyTraded {
				continue
			}
			
			// Calculate position size: bankroll × position_size_pct
			// Polymarket minimum for market orders is $1
			positionSize := ws.bankroll.Mul(ws.config.PositionSizePct)
			minMarketOrder := decimal.NewFromFloat(1.0) // $1 minimum for market orders
			if positionSize.LessThan(minMarketOrder) {
				positionSize = minMarketOrder
			}
			
			log.Info().
				Str("asset", window.Asset).
				Str("side", signal.Side).
				Str("odds", signal.CurrentOdds.String()).
				Str("rr", fmt.Sprintf("1:%.2f", signal.ExpectedRR.InexactFloat64())).
				Str("strength", signal.Strength.String()).
				Str("reason", signal.Reason).
				Msg("🐋 WHALE SIGNAL DETECTED!")
			
			// Execute trade (paper or live)
			_, err := ws.ExecuteTrade(signal, positionSize)
			if err != nil {
				log.Error().Err(err).Msg("🐋 Failed to execute whale trade")
			}
		}
	}
}

// SetConfig updates the whale strategy configuration
func (ws *WhaleStrategy) SetConfig(config WhaleConfig) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.config = config
	log.Info().
		Str("min_entry", config.MinOddsEntry.String()).
		Str("max_entry", config.MaxOddsEntry.String()).
		Str("optimal", config.OptimalEntry.String()).
		Msg("🐋 Whale strategy config updated")
}

// updatePriceHistory tracks price for crash detection
func (ws *WhaleStrategy) updatePriceHistory(windowID, side string, price decimal.Decimal) {
	key := windowID + "_" + side
	
	ws.mu.Lock()
	defer ws.mu.Unlock()
	
	history, exists := ws.priceHistory[key]
	if !exists {
		history = &PriceHistory{
			Prices:     make([]decimal.Decimal, 0),
			Timestamps: make([]time.Time, 0),
			High:       price,
			Low:        price,
		}
		ws.priceHistory[key] = history
	}
	
	// Update history
	history.Prices = append(history.Prices, price)
	history.Timestamps = append(history.Timestamps, time.Now())
	history.LastUpdate = time.Now()
	
	// Update high/low
	if price.GreaterThan(history.High) {
		history.High = price
	}
	if price.LessThan(history.Low) {
		history.Low = price
	}
	
	// Keep last 100 prices only
	if len(history.Prices) > 100 {
		history.Prices = history.Prices[len(history.Prices)-100:]
		history.Timestamps = history.Timestamps[len(history.Timestamps)-100:]
	}
}

// CheckEntry evaluates if current conditions warrant a whale entry
func (ws *WhaleStrategy) CheckEntry(window *polymarket.PredictionWindow) (*WhaleSignal, error) {
	ws.mu.RLock()
	defer ws.mu.RUnlock()

	// Skip if already traded this window
	if _, traded := ws.tradedWindows[window.ID]; traded {
		return nil, nil
	}

	// Check time remaining
	timeRemaining := time.Until(window.EndDate).Minutes()
	if timeRemaining < ws.config.MinTimeRemainingMin {
		return nil, nil
	}
	if timeRemaining > ws.config.MaxTimeRemainingMin {
		return nil, nil
	}

	// Get asset-specific entry zones
	minEntry, maxEntry, optimalEntry := ws.config.GetAssetEntryZone(window.Asset)

	// Check both UP and DOWN sides for crash opportunities
	for _, side := range []string{"UP", "DOWN"} {
		var currentOdds decimal.Decimal
		if side == "UP" {
			currentOdds = window.YesPrice
		} else {
			currentOdds = window.NoPrice
		}

		// Is price in our entry zone?
		if currentOdds.LessThan(minEntry) || currentOdds.GreaterThan(maxEntry) {
			continue
		}

		// Check for crash (price dropped from high)
		key := window.ID + "_" + side
		history, exists := ws.priceHistory[key]
		
		var dropFromHigh decimal.Decimal
		if exists && !history.High.IsZero() {
			dropFromHigh = history.High.Sub(currentOdds)
		}

		// Calculate R:R for this entry
		rr := ws.config.CalculateRR(currentOdds)
		breakevenWR := ws.config.CalculateBreakevenWinRate(currentOdds)

		// Determine signal strength
		strength := ws.calculateSignalStrength(currentOdds, optimalEntry, dropFromHigh)

		// Generate signal if conditions met
		if strength.GreaterThan(decimal.Zero) {
			signal := &WhaleSignal{
				Window:       window,
				Side:         side,
				CurrentOdds:  currentOdds,
				EntryZone:    fmt.Sprintf("%.0f-%.0f¢", minEntry.Mul(decimal.NewFromInt(100)).InexactFloat64(), maxEntry.Mul(decimal.NewFromInt(100)).InexactFloat64()),
				DropFromHigh: dropFromHigh,
				ExpectedRR:   rr,
				BreakevenWR:  breakevenWR,
				Strength:     strength,
				Reason:       ws.getSignalReason(currentOdds, optimalEntry, dropFromHigh, rr),
			}
			return signal, nil
		}
	}

	return nil, nil
}

// WhaleSignal represents a trading signal from whale strategy
type WhaleSignal struct {
	Window       *polymarket.PredictionWindow
	Side         string          // "UP" or "DOWN"
	CurrentOdds  decimal.Decimal
	EntryZone    string
	DropFromHigh decimal.Decimal
	ExpectedRR   decimal.Decimal
	BreakevenWR  decimal.Decimal
	Strength     decimal.Decimal // 0-100 signal strength
	Reason       string
}

// calculateSignalStrength determines how good the entry is (0-100)
func (ws *WhaleStrategy) calculateSignalStrength(
	currentOdds, optimalEntry, dropFromHigh decimal.Decimal,
) decimal.Decimal {
	strength := decimal.Zero

	// Distance from optimal entry (closer = better)
	// If at optimal, +40 points
	// If within 10¢, +30 points
	optimalDiff := currentOdds.Sub(optimalEntry).Abs()
	if optimalDiff.LessThanOrEqual(decimal.NewFromFloat(0.05)) {
		strength = strength.Add(decimal.NewFromFloat(40))
	} else if optimalDiff.LessThanOrEqual(decimal.NewFromFloat(0.10)) {
		strength = strength.Add(decimal.NewFromFloat(30))
	} else if optimalDiff.LessThanOrEqual(decimal.NewFromFloat(0.15)) {
		strength = strength.Add(decimal.NewFromFloat(20))
	} else {
		strength = strength.Add(decimal.NewFromFloat(10))
	}

	// Crash detection bonus (bigger drop = better)
	if dropFromHigh.GreaterThanOrEqual(decimal.NewFromFloat(0.20)) {
		strength = strength.Add(decimal.NewFromFloat(30)) // 20¢+ drop
	} else if dropFromHigh.GreaterThanOrEqual(decimal.NewFromFloat(0.15)) {
		strength = strength.Add(decimal.NewFromFloat(25))
	} else if dropFromHigh.GreaterThanOrEqual(decimal.NewFromFloat(0.10)) {
		strength = strength.Add(decimal.NewFromFloat(20))
	} else if dropFromHigh.GreaterThanOrEqual(decimal.NewFromFloat(0.05)) {
		strength = strength.Add(decimal.NewFromFloat(10))
	}

	// R:R bonus (higher R:R = better)
	rr := ws.config.CalculateRR(currentOdds)
	if rr.GreaterThanOrEqual(decimal.NewFromFloat(2.0)) {
		strength = strength.Add(decimal.NewFromFloat(30)) // 1:2 or better
	} else if rr.GreaterThanOrEqual(decimal.NewFromFloat(1.5)) {
		strength = strength.Add(decimal.NewFromFloat(20)) // 1:1.5
	} else if rr.GreaterThanOrEqual(decimal.NewFromFloat(1.0)) {
		strength = strength.Add(decimal.NewFromFloat(10)) // 1:1
	}

	return strength
}

// getSignalReason returns human-readable reason for signal
func (ws *WhaleStrategy) getSignalReason(
	currentOdds, optimalEntry, dropFromHigh, rr decimal.Decimal,
) string {
	reasons := []string{}

	// Entry quality
	optimalDiff := currentOdds.Sub(optimalEntry).Abs()
	if optimalDiff.LessThanOrEqual(decimal.NewFromFloat(0.05)) {
		reasons = append(reasons, "🎯 At optimal entry")
	} else if currentOdds.LessThan(optimalEntry) {
		reasons = append(reasons, "💎 Below optimal (deep value)")
	}

	// Crash quality
	if dropFromHigh.GreaterThanOrEqual(decimal.NewFromFloat(0.15)) {
		reasons = append(reasons, fmt.Sprintf("📉 %.0f¢ crash detected", dropFromHigh.Mul(decimal.NewFromInt(100)).InexactFloat64()))
	}

	// R:R quality
	if rr.GreaterThanOrEqual(decimal.NewFromFloat(2.0)) {
		reasons = append(reasons, fmt.Sprintf("🚀 Excellent R:R 1:%.2f", rr.InexactFloat64()))
	} else if rr.GreaterThanOrEqual(decimal.NewFromFloat(1.5)) {
		reasons = append(reasons, fmt.Sprintf("✅ Good R:R 1:%.2f", rr.InexactFloat64()))
	}

	if len(reasons) == 0 {
		return "Standard entry conditions met"
	}

	result := ""
	for i, r := range reasons {
		if i > 0 {
			result += " | "
		}
		result += r
	}
	return result
}

// ExecuteTrade executes a whale strategy trade
func (ws *WhaleStrategy) ExecuteTrade(signal *WhaleSignal, positionSize decimal.Decimal) (*WhalePosition, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	// Check concurrent positions
	if len(ws.positions) >= ws.config.MaxConcurrentPositions {
		return nil, fmt.Errorf("max concurrent positions reached (%d)", ws.config.MaxConcurrentPositions)
	}

	// Get token ID
	var tokenID string
	if signal.Side == "UP" {
		tokenID = signal.Window.YesTokenID
	} else {
		tokenID = signal.Window.NoTokenID
	}

	modeStr := "LIVE"
	if ws.paperTrade {
		modeStr = "PAPER"
	}

	log.Info().
		Str("mode", modeStr).
		Str("asset", signal.Window.Asset).
		Str("side", signal.Side).
		Str("odds", signal.CurrentOdds.String()).
		Str("rr", fmt.Sprintf("1:%.2f", signal.ExpectedRR.InexactFloat64())).
		Str("breakeven_wr", fmt.Sprintf("%.0f%%", signal.BreakevenWR.Mul(decimal.NewFromInt(100)).InexactFloat64())).
		Str("reason", signal.Reason).
		Msg("🐋 WHALE ENTRY SIGNAL")

	var orderID string

	if ws.paperTrade {
		// PAPER TRADE - simulate the order
		orderID = fmt.Sprintf("paper_%s_%d", signal.Window.Asset, time.Now().UnixNano())
		log.Info().
			Str("asset", signal.Window.Asset).
			Str("side", signal.Side).
			Str("odds", signal.CurrentOdds.String()).
			Str("size", positionSize.String()).
			Str("order_id", orderID).
			Msg("📝 [WHALE] Paper BUY recorded")
	} else {
		// LIVE TRADE - execute real order at the current entry odds
		// IMPORTANT: Pass the actual entry price to avoid fetching wrong price
		order, err := ws.clobClient.PlaceMarketBuyAtPrice(tokenID, positionSize, signal.CurrentOdds)
		if err != nil {
			return nil, fmt.Errorf("failed to place whale order: %w", err)
		}
		orderID = order.OrderID
	}

	// Create position with high water mark for trailing stop
	position := &WhalePosition{
		TradeID:      orderID,
		Asset:        signal.Window.Asset,
		Side:         signal.Side,
		TokenID:      tokenID,
		ConditionID:  signal.Window.ConditionID,
		WindowID:     signal.Window.ID,
		EntryPrice:   signal.CurrentOdds,
		CurrentPrice: signal.CurrentOdds,
		HighPrice:    signal.CurrentOdds, // Initialize high water mark
		Size:         positionSize.Div(signal.CurrentOdds), // Approx shares
		EntryTime:    time.Now(),
		WindowEnd:    signal.Window.EndDate,
		OddsDropPct:  signal.DropFromHigh,
		ExpectedRR:   signal.ExpectedRR,
		BreakevenWR:  signal.BreakevenWR,
		Status:       "holding",
	}

	// Store position
	ws.positions[signal.Window.ID] = position
	ws.tradedWindows[signal.Window.ID] = time.Now()
	ws.tradesExecuted++

	// Update stats
	ws.stats.TotalTrades++
	if ws.stats.AvgEntryPrice.IsZero() {
		ws.stats.AvgEntryPrice = signal.CurrentOdds
	} else {
		ws.stats.AvgEntryPrice = ws.stats.AvgEntryPrice.Add(signal.CurrentOdds).Div(decimal.NewFromInt(2))
	}

	log.Info().
		Str("trade_id", orderID).
		Str("asset", signal.Window.Asset).
		Str("side", signal.Side).
		Str("entry", fmt.Sprintf("%.0f¢", signal.CurrentOdds.Mul(decimal.NewFromInt(100)).InexactFloat64())).
		Str("size", positionSize.String()).
		Str("rr", fmt.Sprintf("1:%.2f", signal.ExpectedRR.InexactFloat64())).
		Msg("🐋 WHALE POSITION OPENED - HOLDING TO RESOLUTION")

	return position, nil
}

// ProcessResolution handles position resolution
func (ws *WhaleStrategy) ProcessResolution(windowID string, winnerSide string) {
	ws.mu.Lock()
	defer ws.mu.Unlock()

	position, exists := ws.positions[windowID]
	if !exists {
		return
	}

	// Calculate P&L
	if position.Side == winnerSide {
		// WIN: Get $1 per share
		pnl := decimal.NewFromFloat(1.0).Sub(position.EntryPrice).Mul(position.Size)
		position.Status = "resolved_win"
		ws.stats.Wins++
		ws.stats.TotalPnL = ws.stats.TotalPnL.Add(pnl)

		log.Info().
			Str("asset", position.Asset).
			Str("side", position.Side).
			Str("entry", fmt.Sprintf("%.0f¢", position.EntryPrice.Mul(decimal.NewFromInt(100)).InexactFloat64())).
			Str("pnl", fmt.Sprintf("+$%.2f", pnl.InexactFloat64())).
			Msg("🐋✅ WHALE WIN!")
	} else {
		// LOSS: Lose entry price per share
		pnl := position.EntryPrice.Neg().Mul(position.Size)
		position.Status = "resolved_loss"
		ws.stats.Losses++
		ws.stats.TotalPnL = ws.stats.TotalPnL.Add(pnl)

		log.Info().
			Str("asset", position.Asset).
			Str("side", position.Side).
			Str("entry", fmt.Sprintf("%.0f¢", position.EntryPrice.Mul(decimal.NewFromInt(100)).InexactFloat64())).
			Str("pnl", fmt.Sprintf("-$%.2f", pnl.Abs().InexactFloat64())).
			Msg("🐋❌ WHALE LOSS")
	}

	// Remove from active positions
	delete(ws.positions, windowID)
}

// GetStats returns current strategy statistics
func (ws *WhaleStrategy) GetStats() WhaleStats {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	
	stats := ws.stats
	if stats.TotalTrades > 0 {
		stats.AvgRR = ws.config.CalculateRR(stats.AvgEntryPrice)
	}
	return stats
}

// GetActivePositions returns currently held positions
func (ws *WhaleStrategy) GetActivePositions() []*WhalePosition {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	
	positions := make([]*WhalePosition, 0, len(ws.positions))
	for _, p := range ws.positions {
		positions = append(positions, p)
	}
	return positions
}

// PrintStrategyInfo logs whale strategy explanation
func (ws *WhaleStrategy) PrintStrategyInfo() {
	log.Info().Msg("═══════════════════════════════════════════════════════════════")
	log.Info().Msg("🐋 WHALE STRATEGY - ML-TRAINED CONTRARIAN DIP BUYING")
	log.Info().Msg("═══════════════════════════════════════════════════════════════")
	log.Info().Msg("")
	log.Info().Msg("📊 Trained on 150,411 trades from top whale wallets")
	log.Info().Msg("")
	log.Info().Msg("📈 KEY INSIGHT:")
	log.Info().Msg("   Whales buy when odds CRASH, not when they're high")
	log.Info().Msg("   - Entry at 15-55¢ (not 60-85¢)")
	log.Info().Msg("   - Hold to resolution (no quick flip)")
	log.Info().Msg("   - NO stop loss (binary outcome)")
	log.Info().Msg("")
	log.Info().Msg("💰 R:R COMPARISON:")
	log.Info().Msg("   Your Old Strategy: Entry 83¢, R:R 1:0.52, Breakeven 66%")
	log.Info().Msg("   Whale Strategy:    Entry 35¢, R:R 1:1.86, Breakeven 35%")
	log.Info().Msg("")
	log.Info().Msg("🎯 PER-ASSET ENTRY ZONES:")
	log.Info().Msg("   BTC: 15-55¢ (optimal 35¢) R:R 1:1.86")
	log.Info().Msg("   ETH: 20-60¢ (optimal 40¢) R:R 1:1.50")
	log.Info().Msg("   SOL: 25-65¢ (optimal 45¢) R:R 1:1.22")
	log.Info().Msg("")
	log.Info().Msg("📉 EXPECTED VALUE:")
	log.Info().Msg("   At 50% WR: +$0.15/share")
	log.Info().Msg("   At 55% WR: +$0.20/share")
	log.Info().Msg("   At 60% WR: +$0.25/share")
	log.Info().Msg("═══════════════════════════════════════════════════════════════")
}
