package risk

import (
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/strategy"
)

// ═══════════════════════════════════════════════════════════════════════════════
// POSITION SIZING - % based compounding position sizing
// ═══════════════════════════════════════════════════════════════════════════════
//
// Formula: size = (equity * risk_pct) / (entry - stop)
//
// This ensures:
// - Fixed % of equity risked per trade (compounding)
// - Wider stops = smaller positions
// - Tighter stops = larger positions
// - Account grows proportionally with wins
//
// ═══════════════════════════════════════════════════════════════════════════════

type Sizer struct {
	riskPct     decimal.Decimal // % of equity to risk per trade
	minPosition decimal.Decimal // Minimum position size
	maxPct      decimal.Decimal // Maximum % of equity in single trade
}

// NewSizer creates a new position sizer
func NewSizer(riskPct float64) *Sizer {
	return &Sizer{
		riskPct:     decimal.NewFromFloat(riskPct),
		minPosition: decimal.NewFromFloat(1),
		maxPct:      decimal.NewFromFloat(0.25), // Never more than 25% in one trade
	}
}

// Calculate computes position size for a signal
func (s *Sizer) Calculate(signal *strategy.Signal, equity decimal.Decimal) decimal.Decimal {
	// Risk amount = equity * risk%
	riskAmount := equity.Mul(s.riskPct)

	// Risk per unit = entry - stop (for longs)
	riskPerUnit := signal.Entry.Sub(signal.StopLoss).Abs()

	if riskPerUnit.IsZero() {
		return s.minPosition
	}

	// Position size = risk amount / risk per unit
	size := riskAmount.Div(riskPerUnit)

	// Apply constraints
	size = s.applyConstraints(size, signal.Entry, equity)

	return size.Truncate(2)
}

// applyConstraints enforces min/max position limits
func (s *Sizer) applyConstraints(size, entryPrice, equity decimal.Decimal) decimal.Decimal {
	// Minimum position
	if size.LessThan(s.minPosition) {
		return s.minPosition
	}

	// Maximum position (% of equity)
	maxDollarAmount := equity.Mul(s.maxPct)
	maxUnits := maxDollarAmount.Div(entryPrice)
	if size.GreaterThan(maxUnits) {
		return maxUnits
	}

	return size
}

// CalculateWithKelly uses Kelly Criterion for sizing (optional)
func (s *Sizer) CalculateWithKelly(signal *strategy.Signal, equity decimal.Decimal, winRate, avgWinLoss decimal.Decimal) decimal.Decimal {
	// Kelly % = W - (1-W)/R
	// W = win rate
	// R = avg win / avg loss ratio

	if avgWinLoss.IsZero() {
		return s.Calculate(signal, equity)
	}

	one := decimal.NewFromInt(1)
	kellyPct := winRate.Sub(one.Sub(winRate).Div(avgWinLoss))

	// Use half Kelly for safety
	halfKelly := kellyPct.Div(decimal.NewFromInt(2))

	// Clamp to max risk percentage
	if halfKelly.GreaterThan(s.riskPct) {
		halfKelly = s.riskPct
	}
	if halfKelly.LessThan(decimal.Zero) {
		return s.minPosition
	}

	// Apply to base calculation
	riskAmount := equity.Mul(halfKelly)
	riskPerUnit := signal.Entry.Sub(signal.StopLoss).Abs()

	if riskPerUnit.IsZero() {
		return s.minPosition
	}

	size := riskAmount.Div(riskPerUnit)
	return s.applyConstraints(size, signal.Entry, equity).Truncate(2)
}

// RiskAmount returns the dollar amount at risk for a position
func (s *Sizer) RiskAmount(size, entry, stop decimal.Decimal) decimal.Decimal {
	return size.Mul(entry.Sub(stop).Abs())
}

// RiskPercentage returns the % of equity at risk
func (s *Sizer) RiskPercentage(size, entry, stop, equity decimal.Decimal) decimal.Decimal {
	riskDollars := s.RiskAmount(size, entry, stop)
	if equity.IsZero() {
		return decimal.Zero
	}
	return riskDollars.Div(equity).Mul(decimal.NewFromInt(100))
}
