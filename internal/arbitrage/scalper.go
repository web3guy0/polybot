package arbitrage

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
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
	SendTradeAlert(asset, side string, price decimal.Decimal, size int64, action string)
}

type ScalperStrategy struct {
	windowScanner *polymarket.WindowScanner
	clobClient    *CLOBClient
	engine        *Engine // Reference to engine for price-to-beat data
	db            *database.Database // Database for trade logging
	notifier      TradeNotifier // For Telegram alerts
	
	// ML-powered dynamic thresholds
	dynamicThreshold *DynamicThreshold
	useML            bool // Enable intelligent mode
	
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
	ticker := time.NewTicker(500 * time.Millisecond) // Fast scanning
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
		s.mu.Unlock()

		// Check if we're on cooldown (failed order recently)
		if onCooldown && time.Now().Before(cooldownTime) {
			log.Debug().
				Str("asset", asset).
				Str("cooldown_until", cooldownTime.Format("15:04:05")).
				Msg("⏳ [SCALP] On cooldown - skipping")
			continue
		}

		if hasPos {
			// Manage existing position
			s.managePosition(pos, w)
		} else {
			// Look for new opportunity
			s.findScalpOpportunity(w)
		}
	}
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
	// ML-POWERED INTELLIGENT ANALYSIS (if enabled)
	// ═══════════════════════════════════════════════════════════════════════
	s.mu.RLock()
	useML := s.useML
	s.mu.RUnlock()

	var priceMovePct decimal.Decimal
	if s.engine != nil {
		state := s.engine.GetWindowState(w.ID)
		if state != nil && !state.StartPrice.IsZero() {
			currentPrice := s.engine.GetCurrentPrice()
			if !currentPrice.IsZero() {
				priceMove := currentPrice.Sub(state.StartPrice).Abs()
				priceMovePct = priceMove.Div(state.StartPrice)
				
				// Record price for volatility tracking
				s.dynamicThreshold.GetCollector(asset).RecordPrice(currentPrice)
			}
		}
	}

	if useML {
		// Use intelligent ML-style analysis
		features := s.dynamicThreshold.Analyze(
			asset,
			upPrice,
			downPrice,
			priceMovePct,
			timeRemaining,
			s.positionSize,
		)

		if !features.ShouldTrade {
			log.Debug().
				Str("asset", asset).
				Str("up", upPrice.String()).
				Str("down", downPrice.String()).
				Str("P(profit)", features.ProfitProbability.StringFixed(2)).
				Str("reason", features.Reason).
				Msg("🧠 [ML] No trade recommended")
			return
		}

		// ML says GO! Log the analysis
		log.Info().
			Str("asset", asset).
			Str("side", features.CheapSide).
			Str("price", features.CheapPrice.String()).
			Str("P(profit)", features.ProfitProbability.StringFixed(2)).
			Str("volatility", features.Volatility15m.StringFixed(3)+"%").
			Str("momentum_5m", features.PriceVelocity5m.StringFixed(3)+"%").
			Str("time_left", fmt.Sprintf("%.1f min", features.TimeRemainingMin)).
			Str("recommended_size", features.RecommendedSize.StringFixed(2)).
			Msg("🧠 [ML] TRADE RECOMMENDED!")

		// Get dynamic thresholds
		dynamicProfit := s.dynamicThreshold.CalculateDynamicProfit(asset, features.CheapPrice, timeRemaining, features.Volatility15m)
		dynamicStop := s.dynamicThreshold.CalculateDynamicStop(asset, features.CheapPrice, features.Volatility15m)

		log.Debug().
			Str("dynamic_profit", dynamicProfit.String()).
			Str("dynamic_stop", dynamicStop.String()).
			Msg("🧠 [ML] Dynamic thresholds calculated")

		// Execute with ML-recommended parameters + features for DB
		s.executeEntry(w, &features, dynamicProfit, dynamicStop)
		return
	}

	// ═══════════════════════════════════════════════════════════════════════
	// STATIC MODE (fallback if ML disabled)
	// ═══════════════════════════════════════════════════════════════════════
	
	// 1. Check window age - don't enter if less than 5 mins remaining
	if windowAge > s.maxWindowAge {
		log.Debug().
			Str("asset", asset).
			Str("window_age", windowAge.Round(time.Second).String()).
			Str("max_age", s.maxWindowAge.String()).
			Msg("📊 [SCALP] Window too old - not enough time for reversal")
		return
	}

	// 2. Check price-to-beat from engine (if available)
	if s.engine != nil {
		state := s.engine.GetWindowState(w.ID)
		if state != nil && !state.StartPrice.IsZero() {
			currentPrice := s.engine.GetCurrentPrice()
			if !currentPrice.IsZero() {
				// If price moved significantly, the cheap odds might be JUSTIFIED
				if priceMovePct.GreaterThan(s.maxPriceMovePct) {
					priceWentUp := currentPrice.GreaterThan(state.StartPrice)
					
					if (priceWentUp && downPrice.LessThanOrEqual(s.entryThreshold)) ||
					   (!priceWentUp && upPrice.LessThanOrEqual(s.entryThreshold)) {
						log.Warn().
							Str("asset", asset).
							Str("move_pct", priceMovePct.Mul(decimal.NewFromInt(100)).StringFixed(3)+"%").
							Bool("price_went_up", priceWentUp).
							Msg("⚠️ [SCALP] SKIP - Price moved significantly! Cheap odds are JUSTIFIED")
						return
					}
					
					log.Info().
						Str("asset", asset).
						Str("move_pct", priceMovePct.Mul(decimal.NewFromInt(100)).StringFixed(3)+"%").
						Bool("price_went_up", priceWentUp).
						Msg("✅ [SCALP] Price moved but we're betting WITH the move direction")
				}
			}
		}
	}

	// Check if either side is extremely cheap (static threshold)
	var cheapSide string
	var cheapPrice decimal.Decimal
	var tokenID string

	if upPrice.LessThanOrEqual(s.entryThreshold) {
		cheapSide = "UP"
		cheapPrice = upPrice
		tokenID = w.YesTokenID
	} else if downPrice.LessThanOrEqual(s.entryThreshold) {
		cheapSide = "DOWN"
		cheapPrice = downPrice
		tokenID = w.NoTokenID
	}

	if cheapSide == "" {
		log.Debug().
			Str("asset", asset).
			Str("up", upPrice.String()).
			Str("down", downPrice.String()).
			Msg("📊 [SCALP] No extreme prices - waiting")
		return
	}

	// SUPER CHEAP DETECTED! Time to scalp
	log.Info().
		Str("asset", asset).
		Str("side", cheapSide).
		Str("price", cheapPrice.String()).
		Str("window", truncateQuestion(w.Question)).
		Str("window_age", windowAge.Round(time.Second).String()).
		Msg("🎯 [SCALP] SUPER CHEAP DETECTED - Entry conditions met!")

	// Calculate position size
	tickSize := decimal.NewFromFloat(0.01)
	roundedPrice := cheapPrice.Div(tickSize).Floor().Mul(tickSize)
	if roundedPrice.LessThan(tickSize) {
		roundedPrice = tickSize
	}

	size := s.positionSize.Div(roundedPrice).Floor().IntPart()
	if size < 5 {
		size = 5 // Minimum 5 shares
	}

	// Create position
	pos := &ScalpPosition{
		Asset:       asset,
		Side:        cheapSide,
		EntryPrice:  roundedPrice,
		Size:        size,
		EntryTime:   time.Now(),
		TargetPrice: s.profitTarget,
		StopLoss:    s.stopLoss,
		ConditionID: w.ConditionID,
		TokenID:     tokenID,
		WindowID:    w.ID,
	}

	actualCost := roundedPrice.Mul(decimal.NewFromInt(size))
	potentialProfit := s.profitTarget.Sub(roundedPrice).Mul(decimal.NewFromInt(size))

	log.Info().
		Str("asset", asset).
		Str("side", cheapSide).
		Str("entry", roundedPrice.String()).
		Str("target", s.profitTarget.String()).
		Str("stop_loss", s.stopLoss.String()).
		Int64("size", size).
		Str("cost", actualCost.String()).
		Str("potential_profit", potentialProfit.String()).
		Msg("🚀 [SCALP] ENTERING TRADE")

	// Place the order
	s.placeOrder(pos, w, "BUY")
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
		log.Error().Err(err).Str("asset", pos.Asset).Msg("❌ [SCALP] Failed to place order")
		if side == "BUY" {
			// Failed to enter - SET COOLDOWN to prevent spam retries!
			cooldownDuration := 2 * time.Minute // Wait 2 minutes before retrying this asset
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
		pos.OrderID = orderID
		s.positions[pos.Asset] = pos
		
		// Save entry to database
		s.saveEntryToDB(pos, orderID)
		
		// Send Telegram alert
		if s.notifier != nil {
			s.notifier.SendTradeAlert(pos.Asset, pos.Side, orderPrice, pos.Size, "BUY")
		}
		
		log.Info().
			Str("order_id", orderID).
			Str("asset", pos.Asset).
			Msg("✅ [SCALP] BUY order placed!")
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
		s.saveExitToDB(pos, orderID, orderPrice, pnl, exitType)
		
		// Send Telegram alert with P&L
		if s.notifier != nil {
			action := "SELL"
			if exitType == "stop_loss" {
				action = "STOP"
			}
			s.notifier.SendTradeAlert(pos.Asset, pos.Side, orderPrice, pos.Size, action)
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
			Bool("ml_updated", true).
			Msg("✅ [SCALP] SELL order placed! (ML + DB updated)")
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
