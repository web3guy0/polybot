// Package arbitrage - Mean Reversion Scanner
// Tracks odds movement to detect oversold/overbought conditions
package arbitrage

import (
	"fmt"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// OddsPoint represents a single odds reading for swing trading
type OddsPoint struct {
	UpOdds   decimal.Decimal
	DownOdds decimal.Decimal
	Time     time.Time
}

// SwingSignal represents a trading opportunity
type SwingSignal struct {
	Asset         string
	Side          string          // "UP" or "DOWN" - which side to BUY
	CurrentOdds   decimal.Decimal // Current odds of the side to buy
	DropSize      decimal.Decimal // How much it dropped (positive = oversold)
	DropSpeed     float64         // Cents per second
	BounceTarget  decimal.Decimal // Target exit price
	StopLoss      decimal.Decimal // Stop loss price
	Confidence    float64         // 0-1 signal strength
	TimeRemaining time.Duration   // Time left in window
	Reason        string          // Human readable reason
}

// MeanReversionScanner tracks odds history and detects swing opportunities
type MeanReversionScanner struct {
	mu sync.RWMutex

	// Price history per asset (last 60 seconds of readings)
	history map[string][]OddsPoint

	// Config
	config MeanReversionConfig
}

// MeanReversionConfig holds tunable parameters
type MeanReversionConfig struct {
	// Minimum drop to trigger buy signal (in cents, e.g., 0.12 = 12¢)
	MinDropCents float64

	// Lookback window for detecting drops
	LookbackSeconds int

	// Minimum drop speed (cents/second) - faster = stronger signal
	MinDropSpeed float64

	// Expected bounce size as % of drop (e.g., 0.5 = expect 50% retracement)
	BounceRatio float64

	// Stop loss as % of entry price (e.g., 0.15 = stop at 15% below entry)
	StopLossPct float64

	// Maximum odds to buy (don't buy if already expensive)
	MaxOddsCents float64

	// Minimum odds to buy (don't buy if already too cheap - no bounce room)
	MinOddsCents float64

	// Minimum time remaining to trade (seconds)
	MinTimeRemaining int
}

// NewMeanReversionScanner creates a scanner with default config
func NewMeanReversionScanner() *MeanReversionScanner {
	return &MeanReversionScanner{
		history: make(map[string][]OddsPoint),
		config: MeanReversionConfig{
			MinDropCents:     0.12,  // 12¢ drop triggers signal
			LookbackSeconds:  30,    // Look at last 30 seconds
			MinDropSpeed:     0.004, // 0.4¢/sec minimum (12¢ in 30s)
			BounceRatio:      0.50,  // Expect 50% retracement
			StopLossPct:      0.20,  // Stop at 20% below entry
			MaxOddsCents:     0.65,  // Don't buy above 65¢
			MinOddsCents:     0.08,  // Don't buy below 8¢ (no bounce room)
			MinTimeRemaining: 60,    // Need at least 1 minute
		},
	}
}

// RecordPrice adds a new price point to history
func (m *MeanReversionScanner) RecordPrice(asset string, upOdds, downOdds decimal.Decimal) {
	m.mu.Lock()
	defer m.mu.Unlock()

	point := OddsPoint{
		UpOdds:   upOdds,
		DownOdds: downOdds,
		Time:     time.Now(),
	}

	// Initialize if needed
	if _, exists := m.history[asset]; !exists {
		m.history[asset] = make([]OddsPoint, 0, 300) // ~5 minutes at 1/sec
	}

	// Add new point
	m.history[asset] = append(m.history[asset], point)

	// Trim old points (keep last 60 seconds)
	cutoff := time.Now().Add(-60 * time.Second)
	trimmed := make([]OddsPoint, 0, len(m.history[asset]))
	for _, p := range m.history[asset] {
		if p.Time.After(cutoff) {
			trimmed = append(trimmed, p)
		}
	}
	m.history[asset] = trimmed
}

// GetSignal analyzes price history and returns a swing signal if conditions are met
func (m *MeanReversionScanner) GetSignal(asset string, timeRemaining time.Duration) *SwingSignal {
	m.mu.RLock()
	defer m.mu.RUnlock()

	history, exists := m.history[asset]
	if !exists || len(history) < 3 {
		return nil // Need at least 3 data points
	}

	// Check time remaining
	if timeRemaining.Seconds() < float64(m.config.MinTimeRemaining) {
		return nil
	}

	// Get current and lookback prices
	current := history[len(history)-1]
	lookbackTime := time.Now().Add(-time.Duration(m.config.LookbackSeconds) * time.Second)

	// Find the highest point in lookback window for each side
	var upHigh, downHigh decimal.Decimal
	var upHighTime, downHighTime time.Time

	for _, p := range history {
		if p.Time.Before(lookbackTime) {
			continue
		}
		if p.UpOdds.GreaterThan(upHigh) {
			upHigh = p.UpOdds
			upHighTime = p.Time
		}
		if p.DownOdds.GreaterThan(downHigh) {
			downHigh = p.DownOdds
			downHighTime = p.Time
		}
	}

	// Calculate drops from high
	upDrop := upHigh.Sub(current.UpOdds)
	downDrop := downHigh.Sub(current.DownOdds)

	minDrop := decimal.NewFromFloat(m.config.MinDropCents)

	// Check UP side for oversold
	if upDrop.GreaterThanOrEqual(minDrop) {
		signal := m.evaluateSide("UP", current.UpOdds, upDrop, upHighTime, timeRemaining)
		if signal != nil {
			signal.Asset = asset
			return signal
		}
	}

	// Check DOWN side for oversold
	if downDrop.GreaterThanOrEqual(minDrop) {
		signal := m.evaluateSide("DOWN", current.DownOdds, downDrop, downHighTime, timeRemaining)
		if signal != nil {
			signal.Asset = asset
			return signal
		}
	}

	return nil
}

// evaluateSide checks if a specific side has a valid swing opportunity
func (m *MeanReversionScanner) evaluateSide(
	side string,
	currentOdds decimal.Decimal,
	dropSize decimal.Decimal,
	dropStartTime time.Time,
	timeRemaining time.Duration,
) *SwingSignal {
	// Check price bounds
	maxOdds := decimal.NewFromFloat(m.config.MaxOddsCents)
	minOdds := decimal.NewFromFloat(m.config.MinOddsCents)

	if currentOdds.GreaterThan(maxOdds) {
		return nil // Too expensive
	}
	if currentOdds.LessThan(minOdds) {
		return nil // Too cheap, no bounce room
	}

	// Calculate drop speed
	timeSinceDrop := time.Since(dropStartTime).Seconds()
	if timeSinceDrop < 1 {
		timeSinceDrop = 1
	}
	dropSpeed := dropSize.InexactFloat64() / timeSinceDrop

	if dropSpeed < m.config.MinDropSpeed {
		return nil // Drop too slow
	}

	// Calculate targets
	bounceAmount := dropSize.Mul(decimal.NewFromFloat(m.config.BounceRatio))
	bounceTarget := currentOdds.Add(bounceAmount)

	stopLossAmount := currentOdds.Mul(decimal.NewFromFloat(m.config.StopLossPct))
	stopLoss := currentOdds.Sub(stopLossAmount)

	// Calculate confidence based on drop size and speed
	dropCents := dropSize.InexactFloat64() * 100 // Convert to cents
	speedFactor := dropSpeed / m.config.MinDropSpeed
	confidence := min(1.0, (dropCents/20.0)*0.5+(speedFactor)*0.5)

	return &SwingSignal{
		Side:          side,
		CurrentOdds:   currentOdds,
		DropSize:      dropSize,
		DropSpeed:     dropSpeed,
		BounceTarget:  bounceTarget,
		StopLoss:      stopLoss,
		Confidence:    confidence,
		TimeRemaining: timeRemaining,
		Reason: fmt.Sprintf("%s oversold: dropped %.0f¢ in %.0fs (%.1f¢/s), target +%.0f¢",
			side,
			dropSize.Mul(decimal.NewFromInt(100)).InexactFloat64(),
			timeSinceDrop,
			dropSpeed*100,
			bounceAmount.Mul(decimal.NewFromInt(100)).InexactFloat64(),
		),
	}
}

// GetStats returns current tracking stats
func (m *MeanReversionScanner) GetStats() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := make(map[string]interface{})
	for asset, history := range m.history {
		if len(history) > 0 {
			latest := history[len(history)-1]
			stats[asset] = map[string]interface{}{
				"points":    len(history),
				"up_odds":   latest.UpOdds.StringFixed(2),
				"down_odds": latest.DownOdds.StringFixed(2),
				"span_secs": time.Since(history[0].Time).Seconds(),
			}
		}
	}
	return stats
}

// GetPriceHistory returns recent price history for an asset
func (m *MeanReversionScanner) GetPriceHistory(asset string) []OddsPoint {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if history, exists := m.history[asset]; exists {
		// Return a copy
		result := make([]OddsPoint, len(history))
		copy(result, history)
		return result
	}
	return nil
}

// min returns the smaller of two float64 values
func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
