package arbitrage

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	"github.com/web3guy0/polybot/internal/dashboard"
	"github.com/web3guy0/polybot/internal/database"
	"github.com/web3guy0/polybot/internal/polymarket"
)

// ScalperStrategy - Quick in-and-out trades exploiting temporary mispricings
// When one side drops to extreme low (<25¢) without matching price movement,
// buy it and sell when it bounces back to 30-35¢
//
// CRITICAL: Must consider "price to beat" - if price already moved significantly
// from window start, there's no time for reversal in 15-minute windows!
//
// Now with DYNAMIC THRESHOLDS powered by ML-style feature analysis:
// - Volatility-adjusted entry thresholds
// - Time-decay consideration
// - Momentum detection
// - Historical win rate learning
// - Kelly criterion position sizing

// TradeNotifier interface for sending trade alerts
type TradeNotifier interface {
	SendTradeAlert(asset, side string, price decimal.Decimal, size int64, action string, pnl decimal.Decimal)
}

type ScalperStrategy struct {
	windowScanner *polymarket.WindowScanner
	clobClient    *CLOBClient
	engine        *Engine // Reference to engine for price-to-beat data
	db            *database.Database // Database for trade logging
	notifier      TradeNotifier // For Telegram alerts
	dash          *dashboard.ResponsiveDash // Terminal dashboard
	
	// ML-powered dynamic thresholds (legacy)
	dynamicThreshold *DynamicThreshold
	useML            bool // Enable intelligent mode
	
	// NEW: Smart probability model
	probModel *ProbabilityModel
	
	// Configuration (fallback if ML disabled)
	entryThreshold decimal.Decimal // Buy when price drops to this (e.g., 0.20)
	profitTarget   decimal.Decimal // Sell when price rises to this (e.g., 0.33)
	stopLoss       decimal.Decimal // Cut losses if drops to this (e.g., 0.15)
	positionSize   decimal.Decimal // USDC per trade
	
	// Late entry protection
	maxWindowAge     time.Duration   // Don't enter if window is older than this
	maxPriceMovePct  decimal.Decimal // Don't enter if price moved more than this from start
	
	// Paper trading mode
	paperTrade bool

	mu sync.RWMutex

	// Active positions we're scalping
	positions map[string]*ScalpPosition
	
	// Cooldowns to prevent spam after failed orders
	orderCooldowns map[string]time.Time // asset -> next allowed order time
	
	// Stats
	totalTrades   int
	winningTrades int
	totalProfit   decimal.Decimal
	cachedBalance decimal.Decimal // Cached account balance
	lastBalanceFetch time.Time    // When we last fetched balance
	
	stopCh chan struct{}
}

type ScalpPosition struct {
	TradeID     string // UUID for DB
	Asset       string
	Side        string // "UP" or "DOWN"
	EntryPrice  decimal.Decimal
	Size        int64
	EntryTime   time.Time
	TargetPrice decimal.Decimal // Sell target (e.g., 0.33)
	StopLoss    decimal.Decimal // Cut losses if drops further
	OrderID     string
	ConditionID string
	TokenID     string
	WindowID    string
	WindowTitle string
	
	// ML features at entry for DB logging
	MLProbability    decimal.Decimal
	MLEntryThreshold decimal.Decimal
	Volatility15m    decimal.Decimal
	Momentum1m       decimal.Decimal
	Momentum5m       decimal.Decimal
	PriceAtEntry     decimal.Decimal
	TimeRemainingMin int
}

// Scalping parameters - defaults
const (
	DefaultScalpEntry      = 0.20 // Buy if <= 20¢ (super cheap)
	DefaultScalpTarget     = 0.33 // Sell at 33¢
	DefaultScalpStop       = 0.15 // Stop loss at 15¢
	DefaultMaxWindowAge    = 10 * time.Minute // Don't enter after 10 mins (only 5 mins left for reversal)
	DefaultMaxPriceMovePct = 0.005 // 0.5% - if price moved more than this, odds are justified
)

func NewScalperStrategy(scanner *polymarket.WindowScanner, clobClient *CLOBClient, paperTrade bool, positionSize decimal.Decimal) *ScalperStrategy {
	baseEntry := decimal.NewFromFloat(DefaultScalpEntry)
	baseProfit := decimal.NewFromFloat(DefaultScalpTarget)
	baseStop := decimal.NewFromFloat(DefaultScalpStop)
	
	return &ScalperStrategy{
		windowScanner:    scanner,
		clobClient:       clobClient,
		paperTrade:       paperTrade,
		entryThreshold:   baseEntry,
		profitTarget:     baseProfit,
		stopLoss:         baseStop,
		positionSize:     positionSize,
		maxWindowAge:     DefaultMaxWindowAge,
		maxPriceMovePct:  decimal.NewFromFloat(DefaultMaxPriceMovePct),
		positions:        make(map[string]*ScalpPosition),
		orderCooldowns:   make(map[string]time.Time),
		totalProfit:      decimal.Zero,
		stopCh:           make(chan struct{}),
		// Initialize ML-powered dynamic thresholds
		dynamicThreshold: NewDynamicThreshold(baseEntry, baseProfit, baseStop),
		useML:            true, // Enable intelligent mode by default
		// NEW: Smart probability model
		probModel:        NewProbabilityModel(),
	}
}

// SetEngine sets the arbitrage engine reference for price-to-beat data
func (s *ScalperStrategy) SetEngine(engine *Engine) {
	s.engine = engine
}

// SetDatabase sets the database for trade logging
func (s *ScalperStrategy) SetDatabase(db *database.Database) {
	s.db = db
	log.Info().Msg("📊 [SCALP] Database connected for trade logging")
}

// SetNotifier sets the notifier for trade alerts (Telegram)
func (s *ScalperStrategy) SetNotifier(n TradeNotifier) {
	s.notifier = n
	log.Info().Msg("📱 [SCALP] Notifier connected for trade alerts")
}

// SetDashboard sets the terminal dashboard
func (s *ScalperStrategy) SetDashboard(d *dashboard.ResponsiveDash) {
	s.dash = d
	log.Info().Msg("📺 [SCALP] Dashboard connected")
}

// EnableML enables/disables ML-powered dynamic thresholds
func (s *ScalperStrategy) EnableML(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.useML = enabled
	log.Info().Bool("ml_enabled", enabled).Msg("🧠 Scalper ML mode changed")
}

func (s *ScalperStrategy) Start() {
	log.Info().
		Str("entry_threshold", s.entryThreshold.String()).
		Str("profit_target", s.profitTarget.String()).
		Str("stop_loss", s.stopLoss.String()).
		Str("position_size", s.positionSize.String()).
		Bool("paper", s.paperTrade).
		Bool("ml_enabled", s.useML).
		Msg("🧠 Intelligent Scalper starting...")

	go s.monitorLoop()
}

func (s *ScalperStrategy) monitorLoop() {
	ticker := time.NewTicker(100 * time.Millisecond) // ULTRA-FAST 100ms scanning!
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			log.Info().Msg("🛑 Scalper strategy stopped")
			return
		case <-ticker.C:
			s.checkWindows()
		}
	}
}

func (s *ScalperStrategy) checkWindows() {
	windows := s.windowScanner.GetActiveWindows()
	if len(windows) == 0 {
		return
	}

	for i := range windows {
		w := &windows[i]
		asset := w.Asset
		
		s.mu.Lock()
		pos, hasPos := s.positions[asset]  // Use ASSET as key, not window ID!
		cooldownTime, onCooldown := s.orderCooldowns[asset]
		numPositions := len(s.positions)
		s.mu.Unlock()

		// Update dashboard with prices
		// Use pre-captured Price to Beat from engine (not from Polymarket API - they don't expose it!)
		binPrice := decimal.Zero
		priceToBeat := decimal.Zero
		if s.engine != nil {
			binPrice = s.engine.GetCurrentPrice()
			// Get price to beat from our captured window state
			if state := s.engine.GetWindowState(w.ID); state != nil && !state.StartPrice.IsZero() {
				priceToBeat = state.StartPrice
			}
		}
		s.dashUpdatePrices(asset, binPrice, priceToBeat, w.YesPrice, w.NoPrice)

		// Log position state periodically (every ~5 seconds)
		if time.Now().Second()%5 == 0 {
			log.Debug().
				Str("asset", asset).
				Bool("has_position", hasPos).
				Int("total_positions", numPositions).
				Str("price_to_beat", priceToBeat.StringFixed(2)).
				Str("window_id", w.ID).
				Msg("📊 [SCALP] Position check")
		}

		// Check if we're on cooldown (failed order recently)
		if onCooldown && time.Now().Before(cooldownTime) {
			log.Info().
				Str("asset", asset).
				Str("cooldown_until", cooldownTime.Format("15:04:05")).
				Msg("⏳ [SCALP] On cooldown - skipping")
			continue
		}

		if hasPos {
			// Update dashboard with position
			var currentPrice decimal.Decimal
			if pos.Side == "UP" {
				currentPrice = w.YesPrice
			} else {
				currentPrice = w.NoPrice
			}
			s.dashUpdatePosition(pos, currentPrice, "OPEN")
			
			// Manage existing position
			s.managePosition(pos, w)
		} else {
			// Look for new opportunity
			s.findScalpOpportunity(w)
		}
	}
	
	// Update stats on dashboard
	s.dashUpdateStats()
}

func (s *ScalperStrategy) findScalpOpportunity(w *polymarket.PredictionWindow) {
	asset := w.Asset

	// Get current odds (YesPrice = UP, NoPrice = DOWN)
	upPrice := w.YesPrice
	downPrice := w.NoPrice

	// Calculate time remaining
	windowAge := time.Since(w.StartDate)
	windowDuration := 15 * time.Minute
	timeRemaining := windowDuration - windowAge
	if timeRemaining < 0 {
		timeRemaining = 0
	}

	// ═══════════════════════════════════════════════════════════════════════
	// SMART PROBABILITY MODEL - The brain of our trading!
	// ═══════════════════════════════════════════════════════════════════════
	
	// Get price data - CRITICAL: Must have accurate Price to Beat!
	priceToBeat := decimal.Zero
	currentPrice := decimal.Zero
	momentum := 0.0
	
	// PRIORITY 1: Get from engine's tracked state (most reliable!)
	if s.engine != nil {
		currentPrice = s.engine.GetCurrentPrice()
		
		// Get start price from engine's window state
		if state := s.engine.GetWindowState(w.ID); state != nil && !state.StartPrice.IsZero() {
			priceToBeat = state.StartPrice
		}
	}
	
	// PRIORITY 2: Use window's PriceToBeat if engine doesn't have it
	if priceToBeat.IsZero() && !w.PriceToBeat.IsZero() {
		priceToBeat = w.PriceToBeat
	}
	
	// Log if we're missing critical data
	if priceToBeat.IsZero() {
		log.Debug().Str("asset", asset).Msg("⚠️ [SCALP] No Price to Beat - waiting for next window")
		return
	}
	
	if currentPrice.IsZero() {
		log.Debug().Str("asset", asset).Msg("⚠️ [SCALP] No current price - waiting for feed")
		return
	}
	
	// Record price for volatility tracking
	s.dynamicThreshold.GetCollector(asset).RecordPrice(currentPrice)
	
	// Use the probability model to decide
	if s.probModel != nil && !priceToBeat.IsZero() && !currentPrice.IsZero() {
		decision := s.probModel.Analyze(
			asset,
			priceToBeat,
			currentPrice,
			upPrice,
			downPrice,
			timeRemaining,
			momentum,
		)
		
		// Always update ML signal display with latest analysis
		signalStr := "HOLD"
		if decision.ShouldTrade {
			if decision.WinProb >= 0.7 {
				signalStr = "STRONG_BUY"
			} else {
				signalStr = "BUY"
			}
		} else if decision.WinProb < 0.3 {
			signalStr = "SKIP"
		}
		s.dashUpdateMLSignal(
			asset,
			decision.Side,
			decision.MarketPrice,
			decision.WinProb,
			decision.Edge.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢",
			decision.ExpectedValue.Mul(decimal.NewFromInt(100)).StringFixed(1)+"%",
			signalStr,
		)
		
		if decision.ShouldTrade {
			// Model says GO!
			s.dashAddOpportunity(asset, decision.Side, decision.MarketPrice, decimal.NewFromFloat(decision.WinProb), "🧠 BUY", decision.Reason)
			
			log.Info().
				Str("asset", asset).
				Str("side", decision.Side).
				Str("market_price", decision.MarketPrice.StringFixed(2)).
				Str("fair_price", decision.FairPrice.StringFixed(2)).
				Str("edge", decision.Edge.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
				Float64("win_prob", decision.WinProb).
				Str("ev", decision.ExpectedValue.Mul(decimal.NewFromInt(100)).StringFixed(1)+"%").
				Str("risk", decision.RiskLevel).
				Msg("🧠 [PROB] SMART ENTRY!")
			
			// Execute the trade with model's recommendation
			s.executeSmartEntry(w, &decision)
			return
		} else {
			// Model says NO
			log.Debug().
				Str("asset", asset).
				Str("up", upPrice.String()).
				Str("down", downPrice.String()).
				Str("reason", decision.Reason).
				Msg("🧠 [PROB] No opportunity")
			return
		}
	}
	
	// If probability model couldn't run (missing price data), DON'T TRADE
	// The fallback ML path caused losses by betting against price direction
	log.Debug().
		Str("asset", asset).
		Str("price_to_beat", priceToBeat.String()).
		Str("current_price", currentPrice.String()).
		Msg("⚠️ [SCALP] No trade - probability model requires price data")
	return
}

// executeEntry executes an entry trade with ML-recommended parameters
func (s *ScalperStrategy) executeEntry(w *polymarket.PredictionWindow, features *Features, targetPrice, stopPrice decimal.Decimal) {
	asset := w.Asset
	cheapSide := features.CheapSide
	cheapPrice := features.CheapPrice
	recommendedSize := features.RecommendedSize
	
	var tokenID string
	if cheapSide == "UP" {
		tokenID = w.YesTokenID
	} else {
		tokenID = w.NoTokenID
	}

	// Calculate position size
	tickSize := decimal.NewFromFloat(0.01)
	roundedPrice := cheapPrice.Div(tickSize).Floor().Mul(tickSize)
	if roundedPrice.LessThan(tickSize) {
		roundedPrice = tickSize
	}

	// Use ML-recommended size or fallback
	posSize := recommendedSize
	if posSize.IsZero() || posSize.LessThan(decimal.NewFromInt(1)) {
		posSize = s.positionSize
	}

	size := posSize.Div(roundedPrice).Floor().IntPart()
	if size < 5 {
		size = 5 // Minimum 5 shares
	}

	// Calculate time remaining
	windowAge := time.Since(w.StartDate)
	timeRemainingMin := int((15*time.Minute - windowAge).Minutes())
	if timeRemainingMin < 0 {
		timeRemainingMin = 0
	}
	
	// Get current BTC/ETH/SOL price for reference
	var priceAtEntry decimal.Decimal
	if s.engine != nil {
		priceAtEntry = s.engine.GetCurrentPrice()
	}

	// Create position with dynamic targets and ML features
	pos := &ScalpPosition{
		TradeID:          uuid.New().String(),
		Asset:            asset,
		Side:             cheapSide,
		EntryPrice:       roundedPrice,
		Size:             size,
		EntryTime:        time.Now(),
		TargetPrice:      targetPrice,
		StopLoss:         stopPrice,
		ConditionID:      w.ConditionID,
		TokenID:          tokenID,
		WindowID:         w.ID,
		WindowTitle:      w.Question,
		MLProbability:    features.ProfitProbability,
		MLEntryThreshold: features.CheapPrice, // Entry price = threshold used
		Volatility15m:    features.Volatility15m,
		Momentum1m:       features.PriceVelocity1m,
		Momentum5m:       features.PriceVelocity5m,
		PriceAtEntry:     priceAtEntry,
		TimeRemainingMin: timeRemainingMin,
	}

	actualCost := roundedPrice.Mul(decimal.NewFromInt(size))
	potentialProfit := targetPrice.Sub(roundedPrice).Mul(decimal.NewFromInt(size))

	log.Info().
		Str("asset", asset).
		Str("side", cheapSide).
		Str("entry", roundedPrice.String()).
		Str("target", targetPrice.String()).
		Str("stop_loss", stopPrice.String()).
		Int64("size", size).
		Str("cost", actualCost.String()).
		Str("potential_profit", potentialProfit.String()).
		Str("P(profit)", features.ProfitProbability.StringFixed(2)).
		Bool("ml_mode", true).
		Msg("🧠 [ML] INTELLIGENT ENTRY")

	// Place the order
	s.placeOrder(pos, w, "BUY")
}

// executeSmartEntry executes a trade based on the probability model's decision
func (s *ScalperStrategy) executeSmartEntry(w *polymarket.PredictionWindow, decision *TradeDecision) {
	asset := w.Asset
	side := decision.Side
	marketPrice := decision.MarketPrice
	
	var tokenID string
	if side == "UP" {
		tokenID = w.YesTokenID
	} else {
		tokenID = w.NoTokenID
	}

	// Calculate position size based on model's recommendation
	tickSize := decimal.NewFromFloat(0.01)
	roundedPrice := marketPrice.Div(tickSize).Floor().Mul(tickSize)
	if roundedPrice.LessThan(tickSize) {
		roundedPrice = tickSize
	}

	// Use Kelly-based size from model, scaled to our position size
	kellyFraction := decision.RecommendedSize.InexactFloat64()
	posValue := s.positionSize.Mul(decimal.NewFromFloat(kellyFraction))
	if posValue.LessThan(decimal.NewFromFloat(0.50)) {
		posValue = decimal.NewFromFloat(0.50) // Minimum $0.50
	}

	size := posValue.Div(roundedPrice).Floor().IntPart()
	if size < 5 {
		size = 5 // Minimum 5 shares
	}

	// Calculate dynamic stop loss based on edge
	// Tighter stops for trades with high confidence
	stopPct := decimal.NewFromFloat(0.75) // Default 25% loss
	if decision.Confidence > 0.7 {
		stopPct = decimal.NewFromFloat(0.80) // 20% loss for high confidence
	}
	stopPrice := roundedPrice.Mul(stopPct)
	if stopPrice.LessThan(decimal.NewFromFloat(0.02)) {
		stopPrice = decimal.NewFromFloat(0.02)
	}

	// Target = fair price (model's estimate of true value)
	targetPrice := decision.FairPrice
	if targetPrice.LessThanOrEqual(roundedPrice) {
		// If fair is below market (shouldn't happen), use 50% profit target
		targetPrice = roundedPrice.Mul(decimal.NewFromFloat(1.5))
	}

	// Get current asset price for logging
	priceAtEntry := decimal.Zero
	if s.engine != nil {
		priceAtEntry = s.engine.GetCurrentPrice()
	}

	pos := &ScalpPosition{
		TradeID:          uuid.New().String(),
		Asset:            asset,
		Side:             side,
		EntryPrice:       roundedPrice,
		Size:             size,
		EntryTime:        time.Now(),
		TargetPrice:      targetPrice,
		StopLoss:         stopPrice,
		ConditionID:      w.ConditionID,
		TokenID:          tokenID,
		WindowID:         w.ID,
		WindowTitle:      w.Question,
		MLProbability:    decimal.NewFromFloat(decision.WinProb),
		MLEntryThreshold: roundedPrice,
		PriceAtEntry:     priceAtEntry,
	}

	actualCost := roundedPrice.Mul(decimal.NewFromInt(size))
	potentialProfit := targetPrice.Sub(roundedPrice).Mul(decimal.NewFromInt(size))

	log.Info().
		Str("asset", asset).
		Str("side", side).
		Str("entry", roundedPrice.String()).
		Str("fair", decision.FairPrice.String()).
		Str("target", targetPrice.String()).
		Str("stop_loss", stopPrice.String()).
		Int64("size", size).
		Str("cost", actualCost.String()).
		Str("potential", potentialProfit.String()).
		Float64("win_prob", decision.WinProb).
		Str("edge", decision.Edge.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
		Str("ev", decision.ExpectedValue.Mul(decimal.NewFromInt(100)).StringFixed(1)+"%").
		Str("risk", decision.RiskLevel).
		Msg("🧠 [SMART] PROBABILITY-BASED ENTRY!")

	// Place the order
	s.placeOrder(pos, w, "BUY")
}

func (s *ScalperStrategy) managePosition(pos *ScalpPosition, w *polymarket.PredictionWindow) {
	// Get current price of our position's side (YesPrice = UP, NoPrice = DOWN)
	var currentPrice decimal.Decimal

	if pos.Side == "UP" {
		currentPrice = w.YesPrice
	} else {
		currentPrice = w.NoPrice
	}

	holdTime := time.Since(pos.EntryTime)

	// Check profit target - SELL!
	// Also sell if we've made 50%+ profit on entry (e.g., bought at 8¢, now 12¢+)
	profitPct := currentPrice.Sub(pos.EntryPrice).Div(pos.EntryPrice)
	minProfitPct := decimal.NewFromFloat(0.50) // Take profit at 50% gain
	
	if currentPrice.GreaterThanOrEqual(pos.TargetPrice) || profitPct.GreaterThanOrEqual(minProfitPct) {
		profit := currentPrice.Sub(pos.EntryPrice).Mul(decimal.NewFromInt(pos.Size))
		log.Info().
			Str("asset", pos.Asset).
			Str("side", pos.Side).
			Str("entry", pos.EntryPrice.String()).
			Str("current", currentPrice.String()).
			Str("profit_pct", profitPct.Mul(decimal.NewFromInt(100)).StringFixed(1)+"%").
			Str("profit", profit.String()).
			Str("hold_time", holdTime.Round(time.Second).String()).
			Msg("💰 [SCALP] PROFIT TARGET HIT - SELLING!")

		s.placeOrder(pos, w, "SELL")
		return
	}

	// Check stop loss - CUT LOSSES!
	if currentPrice.LessThanOrEqual(pos.StopLoss) {
		loss := pos.EntryPrice.Sub(currentPrice).Mul(decimal.NewFromInt(pos.Size))
		log.Warn().
			Str("asset", pos.Asset).
			Str("side", pos.Side).
			Str("entry", pos.EntryPrice.String()).
			Str("current", currentPrice.String()).
			Str("loss", loss.String()).
			Msg("🛑 [SCALP] STOP LOSS HIT!")

		s.placeOrder(pos, w, "SELL")
		return
	}

	// Check time-based exit - if held too long without hitting target, exit
	if holdTime > 3*time.Minute {
		// Force exit after 3 minutes - don't baghold
		pnl := currentPrice.Sub(pos.EntryPrice).Mul(decimal.NewFromInt(pos.Size))
		log.Warn().
			Str("asset", pos.Asset).
			Str("side", pos.Side).
			Str("hold_time", holdTime.String()).
			Str("pnl", pnl.String()).
			Msg("⏰ [SCALP] TIME EXIT - exiting position")

		s.placeOrder(pos, w, "SELL")
		return
	}

	// Still waiting for target
	pnl := currentPrice.Sub(pos.EntryPrice).Mul(decimal.NewFromInt(pos.Size))
	log.Debug().
		Str("asset", pos.Asset).
		Str("side", pos.Side).
		Str("entry", pos.EntryPrice.String()).
		Str("current", currentPrice.String()).
		Str("target", pos.TargetPrice.String()).
		Str("pnl", pnl.String()).
		Str("hold_time", holdTime.Round(time.Second).String()).
		Msg("⏳ [SCALP] Holding - waiting for target")
}

func (s *ScalperStrategy) placeOrder(pos *ScalpPosition, w *polymarket.PredictionWindow, side string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var tokenID string
	var currentPrice decimal.Decimal
	
	if pos.Side == "UP" {
		tokenID = w.YesTokenID
		currentPrice = w.YesPrice
	} else {
		tokenID = w.NoTokenID
		currentPrice = w.NoPrice
	}

	tickSize := decimal.NewFromFloat(0.01)
	var orderPrice decimal.Decimal
	
	if side == "BUY" {
		orderPrice = pos.EntryPrice // Already rounded
	} else {
		// SELL - use current market price (floored)
		orderPrice = currentPrice.Div(tickSize).Floor().Mul(tickSize)
	}

	modeStr := "LIVE"
	if s.paperTrade {
		modeStr = "PAPER"
	}

	log.Info().
		Str("mode", modeStr).
		Str("action", side).
		Str("asset", pos.Asset).
		Str("side", pos.Side).
		Str("price", orderPrice.String()).
		Int64("size", pos.Size).
		Msg(fmt.Sprintf("📤 [SCALP] %s order", side))

	if s.paperTrade {
		// Paper trade - just track it
		if side == "BUY" {
			s.positions[pos.Asset] = pos
			
			// Save entry to database
			s.saveEntryToDB(pos, "")
			
			// Dashboard updates
			s.dashUpdatePosition(pos, orderPrice, "OPEN")
			s.dashAddTrade(pos.Asset, "BUY", orderPrice, pos.Size, decimal.Zero, "✅")
			
			log.Info().Str("asset", pos.Asset).Msg("📝 [SCALP] Paper BUY recorded")
		} else {
			// Calculate P&L
			pnl := orderPrice.Sub(pos.EntryPrice).Mul(decimal.NewFromInt(pos.Size))
			s.totalProfit = s.totalProfit.Add(pnl)
			s.totalTrades++
			won := pnl.GreaterThan(decimal.Zero)
			if won {
				s.winningTrades++
			}
			
			// Record outcome for ML learning
			s.dynamicThreshold.RecordTradeOutcome(pos.Asset, pos.EntryPrice, won, pnl)
			
			// Determine exit type
			exitType := s.determineExitType(pos, currentPrice)
			
			// Update database with exit
			s.saveExitToDB(pos, "", orderPrice, pnl, exitType)
			
			// Dashboard updates
			s.dashRemovePosition(pos.Asset)
			resultIcon := "✅"
			if !won {
				resultIcon = "❌"
			}
			s.dashAddTrade(pos.Asset, "SELL", orderPrice, pos.Size, pnl, resultIcon)
			s.dashUpdateStats()
			
			delete(s.positions, pos.Asset)
			log.Info().
				Str("asset", pos.Asset).
				Str("pnl", pnl.String()).
				Str("total_profit", s.totalProfit.String()).
				Bool("won", won).
				Str("exit_type", exitType).
				Msg("📝 [SCALP] Paper SELL recorded (ML + DB updated)")
		}
		return
	}

	// LIVE TRADE
	if s.clobClient == nil {
		log.Error().Msg("❌ [SCALP] CLOB client not available")
		return
	}

	// Place the order - PlaceLimitOrder(tokenID, price, size, side)
	sizeDecimal := decimal.NewFromInt(pos.Size)
	orderID, err := s.clobClient.PlaceLimitOrder(tokenID, orderPrice, sizeDecimal, side)
	if err != nil {
		errStr := err.Error()
		
		// Check if it's a balance issue
		isBalanceError := strings.Contains(errStr, "balance") || strings.Contains(errStr, "allowance")
		
		if isBalanceError {
			log.Error().
				Str("asset", pos.Asset).
				Str("price", orderPrice.String()).
				Int64("size", pos.Size).
				Msg("💸 [SCALP] NOT ENOUGH BALANCE - Need to deposit more USDC!")
		} else {
			log.Error().Err(err).Str("asset", pos.Asset).Msg("❌ [SCALP] Failed to place order")
		}
		
		if side == "BUY" {
			// Failed to enter - SET COOLDOWN to prevent spam retries!
			// Longer cooldown for balance errors since depositing takes time
			cooldownDuration := 5 * time.Minute
			if !isBalanceError {
				cooldownDuration = 2 * time.Minute
			}
			s.orderCooldowns[pos.Asset] = time.Now().Add(cooldownDuration)
			log.Warn().
				Str("asset", pos.Asset).
				Str("cooldown", cooldownDuration.String()).
				Msg("⏳ [SCALP] Order failed - cooling down")
			return
		}
		// Failed to exit - keep position for retry (shorter cooldown)
		s.orderCooldowns[pos.Asset] = time.Now().Add(30 * time.Second)
		return
	}
	
	// Clear any cooldown on success
	delete(s.orderCooldowns, pos.Asset)

	if side == "BUY" {
		// CRITICAL: Verify the order was actually filled!
		// FAK orders may partially fill - we need to know how much
		time.Sleep(500 * time.Millisecond) // Brief wait for order to process
		
		status, filledSize, _, err := s.clobClient.GetOrderStatus(orderID)
		if err != nil {
			log.Warn().Err(err).Str("order_id", orderID).Msg("⚠️ [SCALP] Could not verify order status")
			// Assume filled if we got an order ID back
		} else if status != "matched" && status != "filled" && filledSize.IsZero() {
			log.Warn().
				Str("order_id", orderID).
				Str("status", status).
				Str("filled", filledSize.String()).
				Msg("❌ [SCALP] Order NOT filled - not tracking position!")
			// Set cooldown since the trade opportunity passed
			s.orderCooldowns[pos.Asset] = time.Now().Add(1 * time.Minute)
			return
		} else if !filledSize.IsZero() {
			// Update position size to ACTUAL filled amount (handles partial fills)
			actualSize := filledSize.IntPart()
			if actualSize > 0 && actualSize != pos.Size {
				log.Info().
					Int64("ordered", pos.Size).
					Int64("filled", actualSize).
					Msg("📊 [SCALP] Partial fill - adjusting position size")
				pos.Size = actualSize
			}
		}
		
		pos.OrderID = orderID
		s.positions[pos.Asset] = pos
		
		// Save entry to database
		s.saveEntryToDB(pos, orderID)
		
		// Dashboard updates
		s.dashUpdatePosition(pos, orderPrice, "OPEN")
		s.dashAddTrade(pos.Asset, "BUY", orderPrice, pos.Size, decimal.Zero, "✅")
		
		// Send Telegram alert
		if s.notifier != nil {
			s.notifier.SendTradeAlert(pos.Asset, pos.Side, orderPrice, pos.Size, "BUY", decimal.Zero)
		}
		
		log.Info().
			Str("order_id", orderID).
			Str("asset", pos.Asset).
			Int64("size", pos.Size).
			Str("status", "FILLED").
			Msg("✅ [SCALP] BUY order FILLED - tracking position!")
	} else {
		// SELL order - verify it filled before updating state
		time.Sleep(500 * time.Millisecond) // Brief wait for order to process
		
		status, filledSize, _, err := s.clobClient.GetOrderStatus(orderID)
		if err != nil {
			log.Warn().Err(err).Str("order_id", orderID).Msg("⚠️ [SCALP] Could not verify SELL status")
			// Keep position for retry
			s.orderCooldowns[pos.Asset] = time.Now().Add(30 * time.Second)
			return
		}
		
		if filledSize.IsZero() && status != "matched" && status != "filled" {
			log.Warn().
				Str("order_id", orderID).
				Str("status", status).
				Msg("❌ [SCALP] SELL NOT filled - keeping position for retry!")
			s.orderCooldowns[pos.Asset] = time.Now().Add(30 * time.Second)
			return
		}
		
		// Check for partial SELL
		actualSold := filledSize.IntPart()
		if actualSold > 0 && actualSold < pos.Size {
			// Partial sell - update remaining position
			remaining := pos.Size - actualSold
			log.Warn().
				Int64("ordered", pos.Size).
				Int64("sold", actualSold).
				Int64("remaining", remaining).
				Msg("⚠️ [SCALP] Partial SELL - still have shares!")
			pos.Size = remaining
			// Don't delete position, keep managing it
			return
		}
		
		// Calculate P&L
		pnl := orderPrice.Sub(pos.EntryPrice).Mul(decimal.NewFromInt(pos.Size))
		s.totalProfit = s.totalProfit.Add(pnl)
		s.totalTrades++
		won := pnl.GreaterThan(decimal.Zero)
		if won {
			s.winningTrades++
		}
		
		// Record outcome for ML learning
		s.dynamicThreshold.RecordTradeOutcome(pos.Asset, pos.EntryPrice, won, pnl)
		
		// Determine exit type
		exitType := s.determineExitType(pos, currentPrice)
		
		// Update database with exit
		s.saveExitToDB(pos, orderID, orderPrice, pnl, exitType)
		
		// Dashboard updates
		s.dashRemovePosition(pos.Asset)
		resultIcon := "✅"
		action := "SELL"
		if exitType == "stop_loss" {
			resultIcon = "❌"
			action = "STOP"
		}
		s.dashAddTrade(pos.Asset, action, orderPrice, pos.Size, pnl, resultIcon)
		s.dashUpdateStats()
		
		// Send Telegram alert with P&L
		if s.notifier != nil {
			s.notifier.SendTradeAlert(pos.Asset, pos.Side, orderPrice, pos.Size, action, pnl)
		}
		
		delete(s.positions, pos.Asset)
		log.Info().
			Str("order_id", orderID).
			Str("asset", pos.Asset).
			Str("pnl", pnl.String()).
			Str("total_profit", s.totalProfit.String()).
			Int("wins", s.winningTrades).
			Int("total", s.totalTrades).
			Str("exit_type", exitType).
			Bool("verified", true).
			Msg("✅ [SCALP] SELL VERIFIED & FILLED!")
	}
}

// determineExitType figures out why we exited
func (s *ScalperStrategy) determineExitType(pos *ScalpPosition, currentPrice decimal.Decimal) string {
	holdTime := time.Since(pos.EntryTime)
	profitPct := currentPrice.Sub(pos.EntryPrice).Div(pos.EntryPrice)
	
	if currentPrice.GreaterThanOrEqual(pos.TargetPrice) {
		return "profit_target"
	}
	if profitPct.GreaterThanOrEqual(decimal.NewFromFloat(0.50)) {
		return "early_exit"
	}
	if currentPrice.LessThanOrEqual(pos.StopLoss) {
		return "stop_loss"
	}
	if holdTime > 3*time.Minute {
		return "timeout"
	}
	return "manual"
}

// saveEntryToDB saves an entry trade to the database
func (s *ScalperStrategy) saveEntryToDB(pos *ScalpPosition, orderID string) {
	if s.db == nil {
		return
	}
	
	entryCost := pos.EntryPrice.Mul(decimal.NewFromInt(pos.Size))
	
	trade := &database.ScalpTrade{
		TradeID:          pos.TradeID,
		Asset:            pos.Asset,
		WindowID:         pos.WindowID,
		WindowTitle:      pos.WindowTitle,
		Side:             pos.Side,
		TokenID:          pos.TokenID,
		EntryPrice:       pos.EntryPrice,
		EntrySize:        pos.Size,
		EntryCost:        entryCost,
		EntryTime:        pos.EntryTime,
		EntryOrderID:     orderID,
		MLProbability:    pos.MLProbability,
		MLEntryThreshold: pos.MLEntryThreshold,
		MLProfitTarget:   pos.TargetPrice,
		MLStopLoss:       pos.StopLoss,
		Volatility15m:    pos.Volatility15m,
		Momentum1m:       pos.Momentum1m,
		Momentum5m:       pos.Momentum5m,
		PriceAtEntry:     pos.PriceAtEntry,
		TimeRemainingMin: pos.TimeRemainingMin,
		Status:           "OPEN",
	}
	
	if err := s.db.SaveScalpTrade(trade); err != nil {
		log.Error().Err(err).Str("asset", pos.Asset).Msg("❌ Failed to save entry to DB")
	} else {
		log.Debug().Str("trade_id", pos.TradeID).Msg("📊 Entry saved to DB")
	}
}

// saveExitToDB updates the trade with exit info
func (s *ScalperStrategy) saveExitToDB(pos *ScalpPosition, orderID string, exitPrice, pnl decimal.Decimal, exitType string) {
	if s.db == nil {
		return
	}
	
	// Fetch the existing trade
	trade, err := s.db.GetScalpTrade(pos.TradeID)
	if err != nil {
		log.Error().Err(err).Str("trade_id", pos.TradeID).Msg("❌ Failed to find trade in DB")
		return
	}
	
	// Update exit fields
	exitTime := time.Now()
	exitValue := exitPrice.Mul(decimal.NewFromInt(pos.Size))
	profitPct := decimal.Zero
	if !pos.EntryPrice.IsZero() {
		profitPct = pnl.Div(pos.EntryPrice.Mul(decimal.NewFromInt(pos.Size))).Mul(decimal.NewFromInt(100))
	}
	
	trade.ExitPrice = exitPrice
	trade.ExitSize = pos.Size
	trade.ExitValue = exitValue
	trade.ExitTime = &exitTime
	trade.ExitOrderID = orderID
	trade.ExitType = exitType
	trade.ProfitLoss = pnl
	trade.ProfitPct = profitPct
	trade.Status = "CLOSED"
	
	if err := s.db.UpdateScalpTrade(trade); err != nil {
		log.Error().Err(err).Str("trade_id", pos.TradeID).Msg("❌ Failed to update exit in DB")
	} else {
		log.Debug().
			Str("trade_id", pos.TradeID).
			Str("pnl", pnl.String()).
			Str("exit_type", exitType).
			Msg("📊 Exit saved to DB")
	}
	
	// Update ML learning and daily stats
	won := pnl.GreaterThan(decimal.Zero)
	priceBucket := int(pos.EntryPrice.Mul(decimal.NewFromInt(100)).IntPart()) // 0.08 -> 8
	
	if err := s.db.UpdateMLLearning(pos.Asset, priceBucket, pnl, won); err != nil {
		log.Warn().Err(err).Msg("⚠️ Failed to update ML learning")
	}
	
	if err := s.db.UpdateDailyStats(pnl, pos.Asset, won); err != nil {
		log.Warn().Err(err).Msg("⚠️ Failed to update daily stats")
	}
}

func (s *ScalperStrategy) Stop() {
	close(s.stopCh)
	log.Info().
		Int("total_trades", s.totalTrades).
		Int("winning_trades", s.winningTrades).
		Str("total_profit", s.totalProfit.String()).
		Msg("🛑 [SCALP] Strategy stopped")
}

// ScalperStats holds scalper performance statistics
type ScalperStats struct {
	TotalTrades   int
	WinningTrades int
	TotalProfit   decimal.Decimal
}

// GetStats returns scalper statistics
func (s *ScalperStrategy) GetStats() ScalperStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return ScalperStats{
		TotalTrades:   s.totalTrades,
		WinningTrades: s.winningTrades,
		TotalProfit:   s.totalProfit,
	}
}

// GetPositions returns all open positions
func (s *ScalperStrategy) GetPositions() []*ScalpPosition {
	s.mu.RLock()
	defer s.mu.RUnlock()
	
	positions := make([]*ScalpPosition, 0, len(s.positions))
	for _, pos := range s.positions {
		positions = append(positions, pos)
	}
	return positions
}

func (s *ScalperStrategy) modeStr() string {
	if s.paperTrade {
		return "PAPER"
	}
	return "LIVE"
}

// Helper to truncate question for logs
func truncateQuestion(q string) string {
	if len(q) > 40 {
		return q[:40] + "..."
	}
	return q
}

// ═══════════════════════════════════════════════════════════════════════
// DASHBOARD HELPERS
// ═══════════════════════════════════════════════════════════════════════

// dashUpdatePosition updates the dashboard with current position state
func (s *ScalperStrategy) dashUpdatePosition(pos *ScalpPosition, currentPrice decimal.Decimal, status string) {
	if s.dash != nil {
		s.dash.UpdatePosition(pos.Asset, pos.Side, pos.EntryPrice, currentPrice, pos.Size, status)
	}
}

// dashRemovePosition removes a position from dashboard
func (s *ScalperStrategy) dashRemovePosition(asset string) {
	if s.dash != nil {
		s.dash.RemovePosition(asset)
	}
}

// dashAddTrade logs a trade to dashboard
func (s *ScalperStrategy) dashAddTrade(asset, action string, price decimal.Decimal, size int64, pnl decimal.Decimal, result string) {
	if s.dash != nil {
		s.dash.AddLog(fmt.Sprintf("%s %s %s @ %s¢ x%d %s", result, action, asset, price.Mul(decimal.NewFromInt(100)).StringFixed(0), size, pnl.StringFixed(2)))
	}
}

// dashUpdateStats updates overall stats on dashboard
func (s *ScalperStrategy) dashUpdateStats() {
	s.mu.RLock()
	totalTrades := s.totalTrades
	winningTrades := s.winningTrades
	totalProfit := s.totalProfit
	cachedBalance := s.cachedBalance
	lastFetch := s.lastBalanceFetch
	s.mu.RUnlock()
	
	// Fetch balance every 30 seconds
	if time.Since(lastFetch) > 30*time.Second && s.clobClient != nil {
		go func() {
			balance, err := s.clobClient.GetBalance()
			if err == nil {
				s.mu.Lock()
				s.cachedBalance = balance
				s.lastBalanceFetch = time.Now()
				s.mu.Unlock()
			}
		}()
	}
	
	if s.dash != nil {
		s.dash.UpdateStats(totalTrades, winningTrades, totalProfit, cachedBalance)
	}
}

// dashUpdatePrices updates price display on dashboard
func (s *ScalperStrategy) dashUpdatePrices(asset string, binPrice, priceToBeat, upOdds, downOdds decimal.Decimal) {
	if s.dash != nil {
		s.dash.UpdateMarket(asset, binPrice, priceToBeat, upOdds, downOdds)
	}
}

// dashLog logs a message to dashboard
func (s *ScalperStrategy) dashLog(msg string) {
	if s.dash != nil {
		s.dash.AddLog(msg)
	}
}

// dashAddOpportunity logs an opportunity/signal to dashboard
func (s *ScalperStrategy) dashAddOpportunity(asset, side string, price, probability decimal.Decimal, signal, reason string) {
	if s.dash != nil {
		s.dash.AddSignal(asset, side, signal, price, reason, probability.InexactFloat64())
	}
}

// dashUpdateMLSignal updates ML signal display on dashboard
func (s *ScalperStrategy) dashUpdateMLSignal(asset, side string, price decimal.Decimal, probRev float64, edge, ev, signal string) {
	if s.dash != nil {
		s.dash.UpdateMLSignal(asset, side, price, probRev, edge, ev, signal)
	}
}