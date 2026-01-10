package arbitrage

import (
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
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
type ScalperStrategy struct {
	windowScanner *polymarket.WindowScanner
	clobClient    *CLOBClient
	engine        *Engine // Reference to engine for price-to-beat data
	
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
	
	// Stats
	totalTrades   int
	winningTrades int
	totalProfit   decimal.Decimal
	
	stopCh chan struct{}
}

type ScalpPosition struct {
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
		s.mu.Unlock()

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

		// Execute with ML-recommended parameters
		s.executeEntry(w, features.CheapSide, features.CheapPrice, dynamicProfit, dynamicStop, features.RecommendedSize)
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
func (s *ScalperStrategy) executeEntry(w *polymarket.PredictionWindow, cheapSide string, cheapPrice, targetPrice, stopPrice, recommendedSize decimal.Decimal) {
	asset := w.Asset
	
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

	// Create position with dynamic targets
	pos := &ScalpPosition{
		Asset:       asset,
		Side:        cheapSide,
		EntryPrice:  roundedPrice,
		Size:        size,
		EntryTime:   time.Now(),
		TargetPrice: targetPrice,
		StopLoss:    stopPrice,
		ConditionID: w.ConditionID,
		TokenID:     tokenID,
		WindowID:    w.ID,
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
			
			delete(s.positions, pos.Asset)
			log.Info().
				Str("asset", pos.Asset).
				Str("pnl", pnl.String()).
				Str("total_profit", s.totalProfit.String()).
				Bool("won", won).
				Msg("📝 [SCALP] Paper SELL recorded (ML updated)")
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
			// Failed to enter - don't track position
			return
		}
		// Failed to exit - keep position for retry
		return
	}

	if side == "BUY" {
		pos.OrderID = orderID
		s.positions[pos.Asset] = pos
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
		
		delete(s.positions, pos.Asset)
		log.Info().
			Str("order_id", orderID).
			Str("asset", pos.Asset).
			Str("pnl", pnl.String()).
			Str("total_profit", s.totalProfit.String()).
			Int("wins", s.winningTrades).
			Int("total", s.totalTrades).
			Bool("ml_updated", true).
			Msg("✅ [SCALP] SELL order placed! (ML learning updated)")
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
