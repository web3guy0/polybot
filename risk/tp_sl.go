package risk

import (
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/types"
)

// ═══════════════════════════════════════════════════════════════════════════════
// TP/SL MANAGER - Monitors positions for exit conditions
// ═══════════════════════════════════════════════════════════════════════════════

type TPSLManager struct {
	mu sync.RWMutex

	// Trailing stop configuration
	trailingEnabled  bool
	trailingStart    decimal.Decimal // Start trailing after X% profit
	trailingDistance decimal.Decimal // Trail by X%

	// Time-based stops
	maxHoldTime time.Duration // Maximum time to hold a position
}

// NewTPSLManager creates a new TP/SL manager
func NewTPSLManager() *TPSLManager {
	return &TPSLManager{
		trailingEnabled:  false, // Disabled for now
		trailingStart:    decimal.NewFromFloat(0.05), // Start after 5% profit
		trailingDistance: decimal.NewFromFloat(0.03), // 3% trailing distance
		maxHoldTime:      4 * time.Hour,
	}
}

// CheckExit determines if a position should be closed
func (tm *TPSLManager) CheckExit(pos *types.Position, currentPrice decimal.Decimal) (shouldExit bool, reason string, exitPrice decimal.Decimal) {
	// Check take profit
	if currentPrice.GreaterThanOrEqual(pos.TakeProfit) {
		return true, "TAKE_PROFIT", pos.TakeProfit
	}

	// Check stop loss
	if currentPrice.LessThanOrEqual(pos.StopLoss) {
		return true, "STOP_LOSS", pos.StopLoss
	}

	// Check trailing stop if enabled
	if tm.trailingEnabled {
		newSL := tm.calculateTrailingStop(pos, currentPrice)
		if newSL.GreaterThan(pos.StopLoss) {
			pos.StopLoss = newSL // Update stop loss
			log.Debug().
				Str("asset", pos.Asset).
				Str("new_sl", newSL.StringFixed(2)).
				Msg("Trailing stop updated")
		}
	}

	// Check max hold time
	if time.Since(pos.EntryTime) > tm.maxHoldTime {
		return true, "MAX_HOLD_TIME", currentPrice
	}

	return false, "", decimal.Zero
}

// calculateTrailingStop computes the trailing stop price
func (tm *TPSLManager) calculateTrailingStop(pos *types.Position, currentPrice decimal.Decimal) decimal.Decimal {
	// Calculate current profit %
	profitPct := currentPrice.Sub(pos.EntryPrice).Div(pos.EntryPrice)

	// Only trail if we've hit the start threshold
	if profitPct.LessThan(tm.trailingStart) {
		return pos.StopLoss
	}

	// Update high water mark
	if currentPrice.GreaterThan(pos.HighPrice) {
		pos.HighPrice = currentPrice
	}

	// Trail from high: new SL = high * (1 - trailing distance)
	newSL := pos.HighPrice.Mul(decimal.NewFromInt(1).Sub(tm.trailingDistance))

	return newSL
}

// EnableTrailing enables trailing stops
func (tm *TPSLManager) EnableTrailing(startPct, distancePct float64) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.trailingEnabled = true
	tm.trailingStart = decimal.NewFromFloat(startPct)
	tm.trailingDistance = decimal.NewFromFloat(distancePct)
}

// DisableTrailing disables trailing stops
func (tm *TPSLManager) DisableTrailing() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.trailingEnabled = false
}
