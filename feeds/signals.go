package feeds

import (
	"github.com/shopspring/decimal"
)

// ═══════════════════════════════════════════════════════════════════════════════
// SIGNALS - Signal building helpers
// ═══════════════════════════════════════════════════════════════════════════════

// PriceWindow tracks prices over a time window
type PriceWindow struct {
	prices    []decimal.Decimal
	maxSize   int
	high      decimal.Decimal
	low       decimal.Decimal
	open      decimal.Decimal
	close     decimal.Decimal
}

// NewPriceWindow creates a new price window
func NewPriceWindow(maxSize int) *PriceWindow {
	return &PriceWindow{
		prices:  make([]decimal.Decimal, 0, maxSize),
		maxSize: maxSize,
	}
}

// Add adds a price to the window
func (pw *PriceWindow) Add(price decimal.Decimal) {
	if len(pw.prices) == 0 {
		pw.open = price
		pw.high = price
		pw.low = price
	}

	pw.prices = append(pw.prices, price)
	if len(pw.prices) > pw.maxSize {
		pw.prices = pw.prices[1:]
		// Recalculate high/low
		pw.recalculate()
	}

	pw.close = price
	if price.GreaterThan(pw.high) {
		pw.high = price
	}
	if price.LessThan(pw.low) {
		pw.low = price
	}
}

// recalculate recomputes high/low after trimming
func (pw *PriceWindow) recalculate() {
	if len(pw.prices) == 0 {
		return
	}

	pw.open = pw.prices[0]
	pw.high = pw.prices[0]
	pw.low = pw.prices[0]

	for _, p := range pw.prices[1:] {
		if p.GreaterThan(pw.high) {
			pw.high = p
		}
		if p.LessThan(pw.low) {
			pw.low = p
		}
	}
}

// High returns the highest price in window
func (pw *PriceWindow) High() decimal.Decimal {
	return pw.high
}

// Low returns the lowest price in window
func (pw *PriceWindow) Low() decimal.Decimal {
	return pw.low
}

// Open returns the first price in window
func (pw *PriceWindow) Open() decimal.Decimal {
	return pw.open
}

// Close returns the last price in window
func (pw *PriceWindow) Close() decimal.Decimal {
	return pw.close
}

// Range returns high - low
func (pw *PriceWindow) Range() decimal.Decimal {
	return pw.high.Sub(pw.low)
}

// IsBreakingUp returns true if price broke above resistance
func (pw *PriceWindow) IsBreakingUp(threshold decimal.Decimal) bool {
	if len(pw.prices) < 2 {
		return false
	}
	return pw.close.GreaterThanOrEqual(threshold)
}

// IsBreakingDown returns true if price broke below support
func (pw *PriceWindow) IsBreakingDown(threshold decimal.Decimal) bool {
	if len(pw.prices) < 2 {
		return false
	}
	return pw.close.LessThanOrEqual(threshold)
}

// Size returns number of prices in window
func (pw *PriceWindow) Size() int {
	return len(pw.prices)
}

// IsFull returns true if window is at capacity
func (pw *PriceWindow) IsFull() bool {
	return len(pw.prices) >= pw.maxSize
}

// Clear resets the window
func (pw *PriceWindow) Clear() {
	pw.prices = pw.prices[:0]
	pw.high = decimal.Zero
	pw.low = decimal.Zero
	pw.open = decimal.Zero
	pw.close = decimal.Zero
}

// ═══════════════════════════════════════════════════════════════════════════════
// BREAKOUT DETECTION
// ═══════════════════════════════════════════════════════════════════════════════

// BreakoutDetector detects price breakouts
type BreakoutDetector struct {
	window      *PriceWindow
	threshold   decimal.Decimal
	minRange    decimal.Decimal
}

// NewBreakoutDetector creates a new breakout detector
func NewBreakoutDetector(windowSize int, threshold, minRange decimal.Decimal) *BreakoutDetector {
	return &BreakoutDetector{
		window:    NewPriceWindow(windowSize),
		threshold: threshold,
		minRange:  minRange,
	}
}

// Update adds a new price
func (bd *BreakoutDetector) Update(price decimal.Decimal) {
	bd.window.Add(price)
}

// IsBreakoutUp detects upward breakout
func (bd *BreakoutDetector) IsBreakoutUp() bool {
	if !bd.window.IsFull() {
		return false
	}

	// Check range is sufficient
	if bd.window.Range().LessThan(bd.minRange) {
		return false
	}

	// Check if current price breaks above threshold percentage of range
	rangeTop := bd.window.Low().Add(bd.window.Range().Mul(bd.threshold))
	return bd.window.Close().GreaterThanOrEqual(rangeTop)
}

// IsBreakoutDown detects downward breakout
func (bd *BreakoutDetector) IsBreakoutDown() bool {
	if !bd.window.IsFull() {
		return false
	}

	if bd.window.Range().LessThan(bd.minRange) {
		return false
	}

	rangeBottom := bd.window.High().Sub(bd.window.Range().Mul(bd.threshold))
	return bd.window.Close().LessThanOrEqual(rangeBottom)
}
