package strategy

import (
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/feeds"
)

// ═══════════════════════════════════════════════════════════════════════════════
// BREAKOUT 15M STRATEGY - V1 Strategy
// ═══════════════════════════════════════════════════════════════════════════════
//
// Entry: Price between 88-93¢ with upward breakout momentum
// TP: 99¢ (high probability of resolving YES)
// SL: 70¢ (20¢ risk for 6-11¢ reward = ~1:3 R:R at worst)
//
// Filters:
// - Orderbook imbalance > 0.3 (more bids than asks)
// - Volatility within acceptable range
// - Not already in position on this market
// - Minimum 15min candle confirms direction
//
// ═══════════════════════════════════════════════════════════════════════════════

type Breakout15M struct {
	mu      sync.RWMutex
	enabled bool

	// Configuration
	entryMin   decimal.Decimal // 88¢
	entryMax   decimal.Decimal // 93¢
	takeProfit decimal.Decimal // 99¢
	stopLoss   decimal.Decimal // 70¢
	minVolume  decimal.Decimal // Minimum 24h volume
	minImbal   decimal.Decimal // Minimum orderbook imbalance

	// State per market
	windows    map[string]*feeds.PriceWindow
	lastSignal map[string]time.Time // Cooldown tracking
	cooldown   time.Duration

	// Volatility tracking
	volatility map[string]*feeds.VolatilityTracker
	maxATR     decimal.Decimal // Max volatility threshold

	// Stats
	signalCount int
}

// NewBreakout15M creates a new 15-minute breakout strategy
func NewBreakout15M() *Breakout15M {
	// Load config from environment
	entryMin := envDecimal("BREAKOUT_ENTRY_MIN", 0.88)
	entryMax := envDecimal("BREAKOUT_ENTRY_MAX", 0.93)
	takeProfit := envDecimal("BREAKOUT_TP", 0.99)
	stopLoss := envDecimal("BREAKOUT_SL", 0.70)
	minVolume := envDecimal("BREAKOUT_MIN_VOLUME", 10000)
	minImbal := envDecimal("BREAKOUT_MIN_IMBALANCE", 0.3)
	cooldownMin := envInt("BREAKOUT_COOLDOWN_MIN", 15)

	strat := &Breakout15M{
		enabled:    true,
		entryMin:   entryMin,
		entryMax:   entryMax,
		takeProfit: takeProfit,
		stopLoss:   stopLoss,
		minVolume:  minVolume,
		minImbal:   minImbal,
		windows:    make(map[string]*feeds.PriceWindow),
		lastSignal: make(map[string]time.Time),
		cooldown:   time.Duration(cooldownMin) * time.Minute,
		volatility: make(map[string]*feeds.VolatilityTracker),
		maxATR:     decimal.NewFromFloat(0.15), // Max 15% ATR
	}

	log.Info().
		Str("entry", entryMin.StringFixed(2)+"-"+entryMax.StringFixed(2)).
		Str("tp", takeProfit.StringFixed(2)).
		Str("sl", stopLoss.StringFixed(2)).
		Msg("📊 Breakout15M strategy initialized")

	return strat
}

// Name returns the strategy name
func (b *Breakout15M) Name() string {
	return "Breakout15M"
}

// Enabled returns if strategy is active
func (b *Breakout15M) Enabled() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.enabled
}

// Config returns configuration
func (b *Breakout15M) Config() map[string]interface{} {
	return map[string]interface{}{
		"entry_min":   b.entryMin.String(),
		"entry_max":   b.entryMax.String(),
		"take_profit": b.takeProfit.String(),
		"stop_loss":   b.stopLoss.String(),
		"min_volume":  b.minVolume.String(),
		"min_imbal":   b.minImbal.String(),
		"cooldown":    b.cooldown.String(),
	}
}

// OnTick processes a price tick and returns a signal or nil
func (b *Breakout15M) OnTick(tick feeds.Tick) *Signal {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !b.enabled {
		return nil
	}

	// Get or create price window for this market
	window, exists := b.windows[tick.Asset]
	if !exists {
		window = feeds.NewPriceWindow(90) // 15min of 10s ticks
		b.windows[tick.Asset] = window
	}

	// Get or create volatility tracker
	vol, exists := b.volatility[tick.Asset]
	if !exists {
		vol = feeds.NewVolatilityTracker(20)
		b.volatility[tick.Asset] = vol
	}

	// Update trackers
	window.Add(tick.Mid)
	vol.Update(tick.Mid, tick.BestAsk, tick.BestBid)

	// Check cooldown
	if lastSig, ok := b.lastSignal[tick.Asset]; ok {
		if time.Since(lastSig) < b.cooldown {
			return nil
		}
	}

	// === ENTRY FILTERS ===

	// 1. Price in entry range (88-93¢)
	if tick.Mid.LessThan(b.entryMin) || tick.Mid.GreaterThan(b.entryMax) {
		return nil
	}

	// 2. Need full window for momentum confirmation
	if !window.IsFull() {
		return nil
	}

	// 3. Volatility filter - reject if too volatile
	if vol.ATR().GreaterThan(b.maxATR) {
		log.Debug().
			Str("asset", tick.Asset).
			Str("atr", vol.ATR().StringFixed(4)).
			Msg("Rejected: High volatility")
		return nil
	}

	// 4. Momentum check - price must be at upper end of window range
	windowRange := window.High().Sub(window.Low())
	if windowRange.LessThan(decimal.NewFromFloat(0.03)) {
		return nil // Need at least 3¢ range in window
	}

	pricePosition := tick.Mid.Sub(window.Low()).Div(windowRange)
	if pricePosition.LessThan(decimal.NewFromFloat(0.7)) {
		return nil // Price not at top 30% of range = no breakout
	}

	// 5. Upward momentum - close must be above open
	if window.Close().LessThanOrEqual(window.Open()) {
		return nil
	}

	// 6. Orderbook imbalance check (if available)
	// Positive imbalance = more bids = bullish
	// We infer from bid/ask sizes in tick
	if !tick.BidSize.IsZero() && !tick.AskSize.IsZero() {
		total := tick.BidSize.Add(tick.AskSize)
		imbalance := tick.BidSize.Sub(tick.AskSize).Div(total)
		if imbalance.LessThan(b.minImbal) {
			log.Debug().
				Str("asset", tick.Asset).
				Str("imbalance", imbalance.StringFixed(2)).
				Msg("Rejected: Weak orderbook imbalance")
			return nil
		}
	}

	// === ALL FILTERS PASSED - GENERATE SIGNAL ===

	b.signalCount++
	b.lastSignal[tick.Asset] = time.Now()

	signal := NewSignal().
		Market(tick.Market).
		Asset(tick.Asset).
		TokenID(tick.Asset).
		Side(tick.Side).
		Entry(tick.Mid).
		TakeProfit(b.takeProfit).
		StopLoss(b.stopLoss).
		Confidence(decimal.NewFromFloat(0.7)).
		Reason("15m breakout: price in zone, momentum up, imbalance bullish").
		Strategy(b.Name()).
		Build()

	log.Info().
		Str("asset", tick.Asset).
		Str("price", tick.Mid.StringFixed(2)).
		Str("momentum", pricePosition.StringFixed(2)).
		Msg("🎯 Breakout signal generated")

	return signal
}

// ═══════════════════════════════════════════════════════════════════════════════
// HELPER FUNCTIONS
// ═══════════════════════════════════════════════════════════════════════════════

func envDecimal(key string, fallback float64) decimal.Decimal {
	if val := os.Getenv(key); val != "" {
		if d, err := decimal.NewFromString(val); err == nil {
			return d
		}
	}
	return decimal.NewFromFloat(fallback)
}

func envInt(key string, fallback int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return fallback
}
