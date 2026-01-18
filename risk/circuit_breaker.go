package risk

import (
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ═══════════════════════════════════════════════════════════════════════════════
// CIRCUIT BREAKER - Protection against consecutive losses
// ═══════════════════════════════════════════════════════════════════════════════

type CircuitBreaker struct {
	mu sync.RWMutex

	// Configuration
	maxConsecutiveLosses int
	maxDailyLossPct      decimal.Decimal
	cooldownDuration     time.Duration

	// State
	consecutiveLosses    int
	dailyLoss            decimal.Decimal
	peakEquity           decimal.Decimal
	tripped              bool
	trippedAt            time.Time
	reason               string

	// Tracking
	lastResetDate string
}

// NewCircuitBreaker creates a new circuit breaker
func NewCircuitBreaker(maxLosses int, maxDailyLossPct float64, cooldown time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		maxConsecutiveLosses: maxLosses,
		maxDailyLossPct:      decimal.NewFromFloat(maxDailyLossPct),
		cooldownDuration:     cooldown,
	}
}

// Check returns true if trading should be halted
func (cb *CircuitBreaker) Check(equity decimal.Decimal) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// Check daily reset
	today := time.Now().Format("2006-01-02")
	if cb.lastResetDate != today {
		cb.reset()
		cb.lastResetDate = today
	}

	// Update peak equity
	if equity.GreaterThan(cb.peakEquity) {
		cb.peakEquity = equity
	}

	// If already tripped, check cooldown
	if cb.tripped {
		if time.Since(cb.trippedAt) > cb.cooldownDuration {
			cb.tripped = false
			cb.consecutiveLosses = 0
			cb.dailyLoss = decimal.Zero
			log.Info().Msg("✅ Circuit breaker reset after cooldown")
			return false
		}
		return true
	}

	// Check daily loss limit
	if !cb.peakEquity.IsZero() {
		drawdownPct := cb.dailyLoss.Abs().Div(cb.peakEquity)
		if drawdownPct.GreaterThan(cb.maxDailyLossPct) {
			cb.trip("Max daily loss exceeded")
			return true
		}
	}

	return false
}

// RecordLoss records a losing trade
func (cb *CircuitBreaker) RecordLoss(amount decimal.Decimal) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveLosses++
	cb.dailyLoss = cb.dailyLoss.Add(amount)

	if cb.consecutiveLosses >= cb.maxConsecutiveLosses {
		cb.trip("Max consecutive losses")
	}
}

// RecordWin records a winning trade
func (cb *CircuitBreaker) RecordWin(amount decimal.Decimal) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.consecutiveLosses = 0
	cb.dailyLoss = cb.dailyLoss.Add(amount)
}

// trip activates the circuit breaker
func (cb *CircuitBreaker) trip(reason string) {
	cb.tripped = true
	cb.trippedAt = time.Now()
	cb.reason = reason
	log.Warn().
		Str("reason", reason).
		Int("consecutive_losses", cb.consecutiveLosses).
		Str("daily_loss", cb.dailyLoss.StringFixed(2)).
		Dur("cooldown", cb.cooldownDuration).
		Msg("🚨 CIRCUIT BREAKER TRIPPED")
}

// reset clears the circuit breaker state
func (cb *CircuitBreaker) reset() {
	cb.consecutiveLosses = 0
	cb.dailyLoss = decimal.Zero
	cb.tripped = false
}

// IsTripped returns current trip state
func (cb *CircuitBreaker) IsTripped() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.tripped
}

// GetStats returns circuit breaker statistics
func (cb *CircuitBreaker) GetStats() (consecutiveLosses int, dailyLoss decimal.Decimal, tripped bool, reason string) {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.consecutiveLosses, cb.dailyLoss, cb.tripped, cb.reason
}

// ForceReset manually resets the circuit breaker
func (cb *CircuitBreaker) ForceReset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.reset()
	log.Info().Msg("Circuit breaker manually reset")
}
