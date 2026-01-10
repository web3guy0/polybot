package arbitrage

import (
	"math"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ═══════════════════════════════════════════════════════════════════════════════
// ML FEATURE COLLECTOR & DYNAMIC THRESHOLD ENGINE
// ═══════════════════════════════════════════════════════════════════════════════
// This module provides intelligent, adaptive trading thresholds based on:
// 1. Real-time volatility measurement
// 2. Price momentum (velocity)
// 3. Time remaining in window
// 4. Historical win rate at each price level
// 5. Order book imbalance (future)
//
// Instead of static "buy at 20¢", we calculate:
// P(profit) = f(volatility, momentum, time_remaining, historical_winrate, ...)
// Trade if P(profit) > threshold
// ═══════════════════════════════════════════════════════════════════════════════

// FeatureCollector collects real-time features for ML-style decision making
type FeatureCollector struct {
	asset string

	mu sync.RWMutex

	// Price history for volatility/momentum calculation
	priceHistory   []PricePoint
	maxHistorySize int

	// Historical outcomes for learning
	tradeHistory []TradeOutcome

	// Cached calculations
	volatility15m decimal.Decimal
	momentum1m    decimal.Decimal
	momentum5m    decimal.Decimal
}

type PricePoint struct {
	Price     decimal.Decimal
	Timestamp time.Time
}

type TradeOutcome struct {
	EntryPrice    decimal.Decimal
	ExitPrice     decimal.Decimal
	Side          string // "UP" or "DOWN"
	PriceMoveAtEntry decimal.Decimal // Price move % from window start when entered
	TimeRemaining time.Duration
	Won           bool
	Profit        decimal.Decimal
	Timestamp     time.Time
}

// DynamicThreshold calculates intelligent entry/exit thresholds
type DynamicThreshold struct {
	mu sync.RWMutex

	// Base thresholds
	baseEntry  decimal.Decimal
	baseProfit decimal.Decimal
	baseStop   decimal.Decimal

	// Feature collector per asset
	collectors map[string]*FeatureCollector

	// Historical win rates by price bucket (e.g., 0.15-0.20 → 68% win rate)
	winRatesByBucket map[string]map[int]*BucketStats // asset → bucket → stats
}

type BucketStats struct {
	Bucket       int     // Price bucket (15 = 0.15-0.16, 20 = 0.20-0.21, etc)
	TotalTrades  int
	WinningTrades int
	TotalProfit  decimal.Decimal
}

// Features represents all features for a trading decision
type Features struct {
	// Current market state
	OddsUp       decimal.Decimal
	OddsDown     decimal.Decimal
	CheapPrice   decimal.Decimal
	CheapSide    string // "UP" or "DOWN"

	// Price dynamics
	PriceMovePct    decimal.Decimal // How much price moved from window start
	PriceVelocity1m decimal.Decimal // Price change rate last 1 min
	PriceVelocity5m decimal.Decimal // Price change rate last 5 min
	
	// Volatility
	Volatility15m decimal.Decimal // Realized volatility over 15 mins

	// Time
	TimeRemainingMin float64 // Minutes remaining in window
	WindowAgePct     float64 // 0-1, how much of window has elapsed

	// Historical
	HistoricalWinRate decimal.Decimal // Win rate at similar entry prices

	// Calculated
	ProfitProbability decimal.Decimal // ML model output or heuristic
	RecommendedSize   decimal.Decimal // Kelly-optimal position size
	ShouldTrade       bool
	Reason            string
}

// NewDynamicThreshold creates intelligent threshold calculator
func NewDynamicThreshold(baseEntry, baseProfit, baseStop decimal.Decimal) *DynamicThreshold {
	return &DynamicThreshold{
		baseEntry:        baseEntry,
		baseProfit:       baseProfit,
		baseStop:         baseStop,
		collectors:       make(map[string]*FeatureCollector),
		winRatesByBucket: make(map[string]map[int]*BucketStats),
	}
}

// GetCollector returns or creates a feature collector for an asset
func (dt *DynamicThreshold) GetCollector(asset string) *FeatureCollector {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	if fc, ok := dt.collectors[asset]; ok {
		return fc
	}

	fc := &FeatureCollector{
		asset:          asset,
		maxHistorySize: 1000, // ~15 mins at 1 second intervals
		priceHistory:   make([]PricePoint, 0, 1000),
		tradeHistory:   make([]TradeOutcome, 0, 100),
	}
	dt.collectors[asset] = fc
	return fc
}

// RecordPrice adds a price point for volatility/momentum calculation
func (fc *FeatureCollector) RecordPrice(price decimal.Decimal) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.priceHistory = append(fc.priceHistory, PricePoint{
		Price:     price,
		Timestamp: time.Now(),
	})

	// Trim old data
	if len(fc.priceHistory) > fc.maxHistorySize {
		fc.priceHistory = fc.priceHistory[len(fc.priceHistory)-fc.maxHistorySize:]
	}

	// Recalculate volatility and momentum
	fc.recalculate()
}

// recalculate updates volatility and momentum metrics
func (fc *FeatureCollector) recalculate() {
	now := time.Now()

	// Calculate 1-minute momentum
	fc.momentum1m = fc.calculateMomentum(now.Add(-1 * time.Minute))
	
	// Calculate 5-minute momentum
	fc.momentum5m = fc.calculateMomentum(now.Add(-5 * time.Minute))

	// Calculate 15-minute volatility
	fc.volatility15m = fc.calculateVolatility(now.Add(-15 * time.Minute))
}

// calculateMomentum calculates price change rate since given time
func (fc *FeatureCollector) calculateMomentum(since time.Time) decimal.Decimal {
	if len(fc.priceHistory) < 2 {
		return decimal.Zero
	}

	// Find price at 'since' time
	var startPrice, endPrice decimal.Decimal
	for _, p := range fc.priceHistory {
		if p.Timestamp.After(since) {
			if startPrice.IsZero() {
				startPrice = p.Price
			}
			endPrice = p.Price
		}
	}

	if startPrice.IsZero() || startPrice.Equal(decimal.Zero) {
		return decimal.Zero
	}

	return endPrice.Sub(startPrice).Div(startPrice).Mul(decimal.NewFromInt(100))
}

// calculateVolatility calculates realized volatility (std dev of returns)
func (fc *FeatureCollector) calculateVolatility(since time.Time) decimal.Decimal {
	var returns []float64

	for i := 1; i < len(fc.priceHistory); i++ {
		if fc.priceHistory[i].Timestamp.After(since) {
			prev := fc.priceHistory[i-1].Price.InexactFloat64()
			curr := fc.priceHistory[i].Price.InexactFloat64()
			if prev > 0 {
				ret := (curr - prev) / prev
				returns = append(returns, ret)
			}
		}
	}

	if len(returns) < 2 {
		return decimal.Zero
	}

	// Calculate standard deviation
	var sum float64
	for _, r := range returns {
		sum += r
	}
	mean := sum / float64(len(returns))

	var sumSq float64
	for _, r := range returns {
		sumSq += (r - mean) * (r - mean)
	}
	variance := sumSq / float64(len(returns))
	stdDev := math.Sqrt(variance)

	return decimal.NewFromFloat(stdDev * 100) // As percentage
}

// RecordOutcome records a trade outcome for learning
func (fc *FeatureCollector) RecordOutcome(outcome TradeOutcome) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	fc.tradeHistory = append(fc.tradeHistory, outcome)

	// Keep last 100 trades
	if len(fc.tradeHistory) > 100 {
		fc.tradeHistory = fc.tradeHistory[1:]
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// INTELLIGENT THRESHOLD CALCULATION
// ═══════════════════════════════════════════════════════════════════════════════

// CalculateDynamicEntry calculates entry threshold based on current conditions
func (dt *DynamicThreshold) CalculateDynamicEntry(asset string, timeRemaining time.Duration, volatility decimal.Decimal) decimal.Decimal {
	dt.mu.RLock()
	defer dt.mu.RUnlock()

	entry := dt.baseEntry

	// ═══════════════════════════════════════════════════════════════════════
	// ADJUSTMENT 1: Time remaining in window
	// ═══════════════════════════════════════════════════════════════════════
	// Less time = need cheaper entry (less time for reversal)
	// More time = can accept higher entry (more time for reversal)
	
	minRemaining := timeRemaining.Minutes()
	
	if minRemaining < 5 {
		// Very little time - need super cheap entry
		entry = entry.Mul(decimal.NewFromFloat(0.75)) // e.g., 20¢ → 15¢
	} else if minRemaining > 12 {
		// Lots of time - can accept slightly higher entry
		entry = entry.Mul(decimal.NewFromFloat(1.25)) // e.g., 20¢ → 25¢
	}

	// ═══════════════════════════════════════════════════════════════════════
	// ADJUSTMENT 2: Volatility
	// ═══════════════════════════════════════════════════════════════════════
	// High volatility = more likely to revert, can accept higher entry
	// Low volatility = less likely to revert, need cheaper entry
	
	volFloat := volatility.InexactFloat64()
	if volFloat > 0.3 {
		// High volatility - more generous entry
		entry = entry.Mul(decimal.NewFromFloat(1.15))
	} else if volFloat < 0.1 {
		// Low volatility - stricter entry
		entry = entry.Mul(decimal.NewFromFloat(0.90))
	}

	// ═══════════════════════════════════════════════════════════════════════
	// ADJUSTMENT 3: Historical win rate at this level
	// ═══════════════════════════════════════════════════════════════════════
	bucket := int(entry.InexactFloat64() * 100) // e.g., 0.18 → 18
	if assetBuckets, ok := dt.winRatesByBucket[asset]; ok {
		if stats, ok := assetBuckets[bucket]; ok && stats.TotalTrades >= 5 {
			winRate := float64(stats.WinningTrades) / float64(stats.TotalTrades)
			if winRate > 0.65 {
				// High win rate - can be more aggressive
				entry = entry.Mul(decimal.NewFromFloat(1.10))
			} else if winRate < 0.45 {
				// Low win rate - need cheaper entry
				entry = entry.Mul(decimal.NewFromFloat(0.85))
			}
		}
	}

	// Cap between 10¢ and 30¢
	minEntry := decimal.NewFromFloat(0.10)
	maxEntry := decimal.NewFromFloat(0.30)
	if entry.LessThan(minEntry) {
		entry = minEntry
	}
	if entry.GreaterThan(maxEntry) {
		entry = maxEntry
	}

	return entry
}

// CalculateDynamicProfit calculates profit target based on conditions
func (dt *DynamicThreshold) CalculateDynamicProfit(asset string, entryPrice decimal.Decimal, timeRemaining time.Duration, volatility decimal.Decimal) decimal.Decimal {
	dt.mu.RLock()
	defer dt.mu.RUnlock()

	target := dt.baseProfit

	minRemaining := timeRemaining.Minutes()

	// ═══════════════════════════════════════════════════════════════════════
	// ADJUSTMENT: Time pressure
	// ═══════════════════════════════════════════════════════════════════════
	// Less time = take profit faster (lower target)
	// More time = can wait for bigger profit
	
	if minRemaining < 3 {
		// Very little time - take any profit
		target = entryPrice.Mul(decimal.NewFromFloat(1.3)) // 30% above entry
	} else if minRemaining < 7 {
		// Medium time - moderate target
		target = entryPrice.Mul(decimal.NewFromFloat(1.5)) // 50% above entry
	} else {
		// Plenty of time - aim for 75% profit
		target = entryPrice.Mul(decimal.NewFromFloat(1.75))
	}

	// High volatility = expect bigger swings
	volFloat := volatility.InexactFloat64()
	if volFloat > 0.3 {
		target = target.Mul(decimal.NewFromFloat(1.15))
	}

	// Target should be percentage-based, not fixed!
	// If entry is 8¢, target should be ~12-14¢ (50-75% profit), not 25¢!
	// Cap between entry+30% and entry+100%
	minTarget := entryPrice.Mul(decimal.NewFromFloat(1.30)) // At least 30% profit
	maxTarget := entryPrice.Mul(decimal.NewFromFloat(2.00)) // At most 100% profit
	
	if target.LessThan(minTarget) {
		target = minTarget
	}
	if target.GreaterThan(maxTarget) {
		target = maxTarget
	}
	
	// Also cap absolute value - never set target above 40¢ 
	// (at 40¢+ market is uncertain, take what you can)
	absoluteMax := decimal.NewFromFloat(0.40)
	if target.GreaterThan(absoluteMax) {
		target = absoluteMax
	}

	return target
}

// CalculateDynamicStop calculates stop loss based on conditions
func (dt *DynamicThreshold) CalculateDynamicStop(asset string, entryPrice decimal.Decimal, volatility decimal.Decimal) decimal.Decimal {
	dt.mu.RLock()
	defer dt.mu.RUnlock()

	stop := dt.baseStop

	// High volatility = wider stops to avoid getting stopped out
	volFloat := volatility.InexactFloat64()
	if volFloat > 0.3 {
		stop = stop.Mul(decimal.NewFromFloat(0.85)) // Lower stop = wider range
	}

	// Never go below 8¢ (near-zero risk) or above 18¢
	minStop := decimal.NewFromFloat(0.08)
	maxStop := decimal.NewFromFloat(0.18)
	if stop.LessThan(minStop) {
		stop = minStop
	}
	if stop.GreaterThan(maxStop) {
		stop = maxStop
	}

	return stop
}

// ═══════════════════════════════════════════════════════════════════════════════
// PROBABILITY & POSITION SIZING
// ═══════════════════════════════════════════════════════════════════════════════

// EstimateProfitProbability estimates probability of profitable trade
// This is a heuristic model - can be replaced with ML model later
func (dt *DynamicThreshold) EstimateProfitProbability(f Features) decimal.Decimal {
	// Base probability
	prob := 0.50

	// ═══════════════════════════════════════════════════════════════════════
	// FACTOR 1: Entry price (cheaper = better)
	// ═══════════════════════════════════════════════════════════════════════
	entryFloat := f.CheapPrice.InexactFloat64()
	if entryFloat <= 0.15 {
		prob += 0.20 // Very cheap - high prob
	} else if entryFloat <= 0.20 {
		prob += 0.12
	} else if entryFloat <= 0.25 {
		prob += 0.05
	} else {
		prob -= 0.10 // Not cheap enough
	}

	// ═══════════════════════════════════════════════════════════════════════
	// FACTOR 2: Time remaining
	// ═══════════════════════════════════════════════════════════════════════
	if f.TimeRemainingMin > 10 {
		prob += 0.10 // Lots of time for reversal
	} else if f.TimeRemainingMin > 5 {
		prob += 0.05
	} else {
		prob -= 0.15 // Very little time
	}

	// ═══════════════════════════════════════════════════════════════════════
	// FACTOR 3: Momentum (betting against momentum is risky)
	// ═══════════════════════════════════════════════════════════════════════
	momFloat := f.PriceVelocity5m.InexactFloat64()
	
	// If buying DOWN and price is falling → good
	// If buying DOWN and price is rising → bad
	if f.CheapSide == "DOWN" && momFloat < -0.1 {
		prob += 0.08 // Momentum supports our bet
	} else if f.CheapSide == "DOWN" && momFloat > 0.2 {
		prob -= 0.12 // Momentum against us
	} else if f.CheapSide == "UP" && momFloat > 0.1 {
		prob += 0.08
	} else if f.CheapSide == "UP" && momFloat < -0.2 {
		prob -= 0.12
	}

	// ═══════════════════════════════════════════════════════════════════════
	// FACTOR 4: Historical win rate
	// ═══════════════════════════════════════════════════════════════════════
	histWR := f.HistoricalWinRate.InexactFloat64()
	if histWR > 0.65 {
		prob += 0.10
	} else if histWR < 0.45 && histWR > 0 {
		prob -= 0.10
	}

	// ═══════════════════════════════════════════════════════════════════════
	// FACTOR 5: Volatility (high vol = more reversions)
	// ═══════════════════════════════════════════════════════════════════════
	volFloat := f.Volatility15m.InexactFloat64()
	if volFloat > 0.25 {
		prob += 0.05
	}

	// Clamp to [0.1, 0.9]
	if prob < 0.1 {
		prob = 0.1
	}
	if prob > 0.9 {
		prob = 0.9
	}

	return decimal.NewFromFloat(prob)
}

// CalculateKellySize calculates optimal position size using Kelly Criterion
func (dt *DynamicThreshold) CalculateKellySize(prob decimal.Decimal, baseSize decimal.Decimal, odds decimal.Decimal) decimal.Decimal {
	// Kelly formula: f* = (bp - q) / b
	// Where:
	//   p = probability of winning
	//   q = probability of losing (1-p)
	//   b = odds received on a win (payout ratio)
	
	// For binary prediction markets at price p:
	// If we buy at 0.20 and win, we get 1.00, so payout = 1/0.20 = 5x
	// But we risk 0.20 to win 0.80, so b = 0.80/0.20 = 4
	
	p := prob.InexactFloat64()
	q := 1 - p
	
	entryPrice := odds.InexactFloat64()
	if entryPrice <= 0 || entryPrice >= 1 {
		return baseSize.Mul(decimal.NewFromFloat(0.5)) // Conservative default
	}
	
	winPayout := 1.0 - entryPrice // What we win if right
	b := winPayout / entryPrice    // Odds ratio

	kelly := (b*p - q) / b

	// Half-Kelly for safety
	kelly = kelly / 2

	if kelly < 0.1 {
		kelly = 0.1 // Minimum bet
	}
	if kelly > 0.5 {
		kelly = 0.5 // Max 50% of bankroll per trade
	}

	return baseSize.Mul(decimal.NewFromFloat(kelly / 0.25)) // Normalize to base size
}

// ═══════════════════════════════════════════════════════════════════════════════
// UNIFIED ANALYSIS
// ═══════════════════════════════════════════════════════════════════════════════

// Analyze returns full feature analysis and trading recommendation
func (dt *DynamicThreshold) Analyze(asset string, oddsUp, oddsDown decimal.Decimal, priceMovePct decimal.Decimal, timeRemaining time.Duration, positionSize decimal.Decimal) Features {
	fc := dt.GetCollector(asset)
	fc.mu.RLock()
	volatility := fc.volatility15m
	momentum1m := fc.momentum1m
	momentum5m := fc.momentum5m
	fc.mu.RUnlock()

	// Determine which side is cheap
	var cheapSide string
	var cheapPrice decimal.Decimal

	dynamicEntry := dt.CalculateDynamicEntry(asset, timeRemaining, volatility)

	if oddsUp.LessThanOrEqual(dynamicEntry) {
		cheapSide = "UP"
		cheapPrice = oddsUp
	} else if oddsDown.LessThanOrEqual(dynamicEntry) {
		cheapSide = "DOWN"
		cheapPrice = oddsDown
	}

	// Get historical win rate for this bucket
	bucket := int(cheapPrice.InexactFloat64() * 100)
	var histWinRate decimal.Decimal
	dt.mu.RLock()
	if assetBuckets, ok := dt.winRatesByBucket[asset]; ok {
		if stats, ok := assetBuckets[bucket]; ok && stats.TotalTrades > 0 {
			histWinRate = decimal.NewFromFloat(float64(stats.WinningTrades) / float64(stats.TotalTrades))
		}
	}
	dt.mu.RUnlock()

	f := Features{
		OddsUp:            oddsUp,
		OddsDown:          oddsDown,
		CheapPrice:        cheapPrice,
		CheapSide:         cheapSide,
		PriceMovePct:      priceMovePct,
		PriceVelocity1m:   momentum1m,
		PriceVelocity5m:   momentum5m,
		Volatility15m:     volatility,
		TimeRemainingMin:  timeRemaining.Minutes(),
		WindowAgePct:      1.0 - (timeRemaining.Minutes() / 15.0),
		HistoricalWinRate: histWinRate,
	}

	// Calculate probability
	f.ProfitProbability = dt.EstimateProfitProbability(f)

	// Calculate position size using Kelly
	if !cheapPrice.IsZero() {
		f.RecommendedSize = dt.CalculateKellySize(f.ProfitProbability, positionSize, cheapPrice)
	}

	// Make decision
	minProbability := decimal.NewFromFloat(0.55) // Require 55% probability
	if cheapSide != "" && f.ProfitProbability.GreaterThanOrEqual(minProbability) {
		f.ShouldTrade = true
		f.Reason = "P(profit) = " + f.ProfitProbability.StringFixed(2) + " ≥ 0.55"
	} else if cheapSide == "" {
		f.ShouldTrade = false
		f.Reason = "No cheap side (entry > " + dynamicEntry.StringFixed(2) + ")"
	} else {
		f.ShouldTrade = false
		f.Reason = "P(profit) = " + f.ProfitProbability.StringFixed(2) + " < 0.55"
	}

	return f
}

// RecordTradeOutcome updates historical stats after a trade completes
func (dt *DynamicThreshold) RecordTradeOutcome(asset string, entryPrice decimal.Decimal, won bool, profit decimal.Decimal) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	bucket := int(entryPrice.InexactFloat64() * 100)

	if _, ok := dt.winRatesByBucket[asset]; !ok {
		dt.winRatesByBucket[asset] = make(map[int]*BucketStats)
	}

	if _, ok := dt.winRatesByBucket[asset][bucket]; !ok {
		dt.winRatesByBucket[asset][bucket] = &BucketStats{Bucket: bucket}
	}

	stats := dt.winRatesByBucket[asset][bucket]
	stats.TotalTrades++
	if won {
		stats.WinningTrades++
	}
	stats.TotalProfit = stats.TotalProfit.Add(profit)

	log.Debug().
		Str("asset", asset).
		Int("bucket", bucket).
		Int("total", stats.TotalTrades).
		Int("wins", stats.WinningTrades).
		Str("win_rate", decimal.NewFromFloat(float64(stats.WinningTrades)/float64(stats.TotalTrades)*100).StringFixed(1)+"%").
		Msg("📊 Updated historical win rate")
}
