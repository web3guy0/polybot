package feeds

import (
	"sync"

	"github.com/shopspring/decimal"
)

// ═══════════════════════════════════════════════════════════════════════════════
// INDICATORS - Technical indicators for strategy decisions
// ═══════════════════════════════════════════════════════════════════════════════

// VolatilityTracker tracks price volatility using ATR-like calculation
type VolatilityTracker struct {
	mu      sync.RWMutex
	period  int
	prices  []decimal.Decimal
	highs   []decimal.Decimal
	lows    []decimal.Decimal
	atr     decimal.Decimal
	stdDev  decimal.Decimal
}

// NewVolatilityTracker creates a new volatility tracker
func NewVolatilityTracker(period int) *VolatilityTracker {
	return &VolatilityTracker{
		period: period,
		prices: make([]decimal.Decimal, 0, period),
		highs:  make([]decimal.Decimal, 0, period),
		lows:   make([]decimal.Decimal, 0, period),
	}
}

// Update adds a new price observation
func (vt *VolatilityTracker) Update(price, high, low decimal.Decimal) {
	vt.mu.Lock()
	defer vt.mu.Unlock()

	vt.prices = append(vt.prices, price)
	vt.highs = append(vt.highs, high)
	vt.lows = append(vt.lows, low)

	// Keep only last N periods
	if len(vt.prices) > vt.period {
		vt.prices = vt.prices[1:]
		vt.highs = vt.highs[1:]
		vt.lows = vt.lows[1:]
	}

	// Calculate ATR
	vt.calculateATR()
	vt.calculateStdDev()
}

// calculateATR computes Average True Range
func (vt *VolatilityTracker) calculateATR() {
	if len(vt.prices) < 2 {
		return
	}

	sum := decimal.Zero
	for i := 1; i < len(vt.prices); i++ {
		// True Range = max(high-low, |high-prevClose|, |low-prevClose|)
		hl := vt.highs[i].Sub(vt.lows[i])
		hpc := vt.highs[i].Sub(vt.prices[i-1]).Abs()
		lpc := vt.lows[i].Sub(vt.prices[i-1]).Abs()

		tr := hl
		if hpc.GreaterThan(tr) {
			tr = hpc
		}
		if lpc.GreaterThan(tr) {
			tr = lpc
		}
		sum = sum.Add(tr)
	}

	vt.atr = sum.Div(decimal.NewFromInt(int64(len(vt.prices) - 1)))
}

// calculateStdDev computes standard deviation of prices
func (vt *VolatilityTracker) calculateStdDev() {
	if len(vt.prices) < 2 {
		return
	}

	// Calculate mean
	sum := decimal.Zero
	for _, p := range vt.prices {
		sum = sum.Add(p)
	}
	mean := sum.Div(decimal.NewFromInt(int64(len(vt.prices))))

	// Calculate variance
	variance := decimal.Zero
	for _, p := range vt.prices {
		diff := p.Sub(mean)
		variance = variance.Add(diff.Mul(diff))
	}
	variance = variance.Div(decimal.NewFromInt(int64(len(vt.prices))))

	// Square root approximation for stddev
	vt.stdDev = sqrt(variance)
}

// ATR returns current Average True Range
func (vt *VolatilityTracker) ATR() decimal.Decimal {
	vt.mu.RLock()
	defer vt.mu.RUnlock()
	return vt.atr
}

// StdDev returns current standard deviation
func (vt *VolatilityTracker) StdDev() decimal.Decimal {
	vt.mu.RLock()
	defer vt.mu.RUnlock()
	return vt.stdDev
}

// IsHighVolatility returns true if volatility exceeds threshold
func (vt *VolatilityTracker) IsHighVolatility(threshold decimal.Decimal) bool {
	return vt.ATR().GreaterThan(threshold)
}

// IsLowVolatility returns true if volatility is below threshold
func (vt *VolatilityTracker) IsLowVolatility(threshold decimal.Decimal) bool {
	return vt.ATR().LessThan(threshold)
}

// ═══════════════════════════════════════════════════════════════════════════════
// MOMENTUM - Tracks price momentum
// ═══════════════════════════════════════════════════════════════════════════════

// MomentumTracker tracks price momentum
type MomentumTracker struct {
	mu       sync.RWMutex
	period   int
	prices   []decimal.Decimal
	momentum decimal.Decimal
	roc      decimal.Decimal // Rate of Change
}

// NewMomentumTracker creates a new momentum tracker
func NewMomentumTracker(period int) *MomentumTracker {
	return &MomentumTracker{
		period: period,
		prices: make([]decimal.Decimal, 0, period),
	}
}

// Update adds a new price
func (mt *MomentumTracker) Update(price decimal.Decimal) {
	mt.mu.Lock()
	defer mt.mu.Unlock()

	mt.prices = append(mt.prices, price)
	if len(mt.prices) > mt.period {
		mt.prices = mt.prices[1:]
	}

	if len(mt.prices) >= 2 {
		// Simple momentum: current - first
		mt.momentum = price.Sub(mt.prices[0])

		// Rate of Change: (current - first) / first * 100
		if !mt.prices[0].IsZero() {
			mt.roc = price.Sub(mt.prices[0]).Div(mt.prices[0]).Mul(decimal.NewFromInt(100))
		}
	}
}

// Momentum returns current momentum value
func (mt *MomentumTracker) Momentum() decimal.Decimal {
	mt.mu.RLock()
	defer mt.mu.RUnlock()
	return mt.momentum
}

// ROC returns Rate of Change
func (mt *MomentumTracker) ROC() decimal.Decimal {
	mt.mu.RLock()
	defer mt.mu.RUnlock()
	return mt.roc
}

// IsPositive returns true if momentum is positive
func (mt *MomentumTracker) IsPositive() bool {
	return mt.Momentum().GreaterThan(decimal.Zero)
}

// IsNegative returns true if momentum is negative
func (mt *MomentumTracker) IsNegative() bool {
	return mt.Momentum().LessThan(decimal.Zero)
}

// ═══════════════════════════════════════════════════════════════════════════════
// EMA - Exponential Moving Average
// ═══════════════════════════════════════════════════════════════════════════════

// EMA calculates exponential moving average
type EMA struct {
	mu         sync.RWMutex
	period     int
	multiplier decimal.Decimal
	value      decimal.Decimal
	initialized bool
}

// NewEMA creates a new EMA calculator
func NewEMA(period int) *EMA {
	// Multiplier = 2 / (period + 1)
	mult := decimal.NewFromInt(2).Div(decimal.NewFromInt(int64(period + 1)))
	return &EMA{
		period:     period,
		multiplier: mult,
	}
}

// Update adds a new price and recalculates EMA
func (e *EMA) Update(price decimal.Decimal) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if !e.initialized {
		e.value = price
		e.initialized = true
		return
	}

	// EMA = (price - prevEMA) * multiplier + prevEMA
	e.value = price.Sub(e.value).Mul(e.multiplier).Add(e.value)
}

// Value returns current EMA value
func (e *EMA) Value() decimal.Decimal {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.value
}

// ═══════════════════════════════════════════════════════════════════════════════
// HELPERS
// ═══════════════════════════════════════════════════════════════════════════════

// sqrt calculates square root using Newton's method
func sqrt(d decimal.Decimal) decimal.Decimal {
	if d.IsZero() || d.IsNegative() {
		return decimal.Zero
	}

	// Initial guess
	x := d
	for i := 0; i < 20; i++ {
		// x = (x + d/x) / 2
		x = x.Add(d.Div(x)).Div(decimal.NewFromInt(2))
	}
	return x
}
