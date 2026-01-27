package risk

import (
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/strategy"
	"github.com/web3guy0/polybot/types"
)

// ═══════════════════════════════════════════════════════════════════════════════
// RISK MANAGER - Gatekeeper for all trades
// ═══════════════════════════════════════════════════════════════════════════════
//
// Responsibilities:
// 1. Validate signals before execution
// 2. Calculate position sizes (% based compounding)
// 3. Enforce max positions, max daily drawdown
// 4. Circuit breaker on consecutive losses
//
// ═══════════════════════════════════════════════════════════════════════════════

type Manager struct {
	mu sync.RWMutex

	// Configuration
	riskPerTrade  decimal.Decimal // % of equity per trade (e.g., 0.02 = 2%)
	maxPositions  int             // Maximum concurrent positions
	maxDailyLoss  decimal.Decimal // Maximum daily loss as % of equity
	maxDrawdown   decimal.Decimal // Maximum drawdown from peak
	minRiskReward decimal.Decimal // Minimum R:R ratio required

	// State
	dailyPnL       decimal.Decimal
	dailyPeakEquity decimal.Decimal
	lastResetDay   int
	consecutiveLoss int
	circuitTripped bool

	// Circuit breaker settings
	maxConsecLoss   int
	circuitCooldown time.Duration
	circuitTrippedAt time.Time
}

// NewManager creates a new risk manager
func NewManager() *Manager {
	riskPct := envDecimalRM("RISK_PER_TRADE_PCT", 0.02)
	maxPos := envIntRM("MAX_POSITIONS", 3)
	maxDailyLoss := envDecimalRM("MAX_DAILY_LOSS_PCT", 0.05)
	maxDrawdown := envDecimalRM("MAX_DRAWDOWN_PCT", 0.15)
	minRR := envDecimalRM("MIN_RISK_REWARD", 1.5)
	maxConsecLoss := envIntRM("MAX_CONSECUTIVE_LOSSES", 3)

	mgr := &Manager{
		riskPerTrade:    riskPct,
		maxPositions:    maxPos,
		maxDailyLoss:    maxDailyLoss,
		maxDrawdown:     maxDrawdown,
		minRiskReward:   minRR,
		maxConsecLoss:   maxConsecLoss,
		circuitCooldown: 30 * time.Minute,
	}

	log.Info().
		Str("risk_per_trade", riskPct.Mul(decimal.NewFromInt(100)).String()+"%").
		Int("max_positions", maxPos).
		Str("max_daily_loss", maxDailyLoss.Mul(decimal.NewFromInt(100)).String()+"%").
		Msg("🛡️ Risk manager initialized")

	return mgr
}

// ValidateSignal checks if a signal passes risk rules
func (rm *Manager) ValidateSignal(
	signal *strategy.Signal,
	equity decimal.Decimal,
	positions map[string]*types.Position,
) bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	// Reset daily stats if new day
	rm.checkDayReset()

	// 1. Circuit breaker check
	if rm.circuitTripped {
		if time.Since(rm.circuitTrippedAt) < rm.circuitCooldown {
			log.Warn().Msg("🚨 Circuit breaker active - no trades")
			return false
		}
		rm.circuitTripped = false
		rm.consecutiveLoss = 0
		log.Info().Msg("✅ Circuit breaker reset")
	}

	// 2. Max positions check
	if len(positions) >= rm.maxPositions {
		log.Debug().
			Int("current", len(positions)).
			Int("max", rm.maxPositions).
			Msg("Max positions reached")
		return false
	}

	// 3. Already in this market?
	for _, pos := range positions {
		if pos.Market == signal.Market {
			log.Debug().Str("market", signal.Market).Msg("Already in market")
			return false
		}
	}

	// 4. Daily loss limit
	if rm.dailyPnL.LessThan(rm.maxDailyLoss.Neg().Mul(equity)) {
		log.Warn().
			Str("daily_pnl", rm.dailyPnL.StringFixed(2)).
			Msg("🚨 Daily loss limit hit")
		return false
	}

	// 5. Risk:Reward check
	rr := signal.RiskReward()
	if rr.LessThan(rm.minRiskReward) {
		log.Debug().
			Str("rr", rr.StringFixed(2)).
			Str("min", rm.minRiskReward.StringFixed(2)).
			Msg("R:R too low")
		return false
	}

	// 6. Basic signal validation
	if !signal.Validate() {
		log.Warn().Msg("Invalid signal structure")
		return false
	}

	return true
}

// CalculateSize determines position size using % risk model
// Formula: size = (equity * risk_pct) / (entry - stop)
func (rm *Manager) CalculateSize(signal *strategy.Signal, equity decimal.Decimal) decimal.Decimal {
	rm.mu.RLock()
	defer rm.mu.RUnlock()

	// Risk amount in dollars
	riskAmount := equity.Mul(rm.riskPerTrade)

	// Risk per share (distance from entry to stop)
	riskPerShare := signal.Entry.Sub(signal.StopLoss).Abs()

	if riskPerShare.IsZero() {
		return decimal.Zero
	}

	// Position size = risk amount / risk per share
	size := riskAmount.Div(riskPerShare)

	// Round down to 2 decimal places
	size = size.Truncate(2)

	// Minimum size check
	if size.LessThan(decimal.NewFromFloat(1)) {
		size = decimal.NewFromFloat(1)
	}

	// Maximum size check (never more than 50% of equity in one trade)
	maxSize := equity.Mul(decimal.NewFromFloat(0.5)).Div(signal.Entry)
	if size.GreaterThan(maxSize) {
		size = maxSize
	}

	log.Debug().
		Str("equity", "$"+equity.StringFixed(2)).
		Str("risk_amt", "$"+riskAmount.StringFixed(2)).
		Str("risk_per_share", riskPerShare.StringFixed(4)).
		Str("size", size.StringFixed(2)).
		Msg("Position sizing")

	return size
}

// RecordTrade updates stats after a trade closes
func (rm *Manager) RecordTrade(pnl decimal.Decimal) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	rm.dailyPnL = rm.dailyPnL.Add(pnl)

	if pnl.LessThan(decimal.Zero) {
		rm.consecutiveLoss++
		if rm.consecutiveLoss >= rm.maxConsecLoss {
			rm.circuitTripped = true
			rm.circuitTrippedAt = time.Now()
			log.Warn().
				Int("consecutive_losses", rm.consecutiveLoss).
				Msg("🚨 CIRCUIT BREAKER TRIPPED")
		}
	} else {
		rm.consecutiveLoss = 0
	}

	log.Info().
		Str("trade_pnl", pnl.StringFixed(2)).
		Str("daily_pnl", rm.dailyPnL.StringFixed(2)).
		Int("consecutive_loss", rm.consecutiveLoss).
		Msg("📊 Trade recorded")
}

// checkDayReset resets daily stats at midnight
func (rm *Manager) checkDayReset() {
	today := time.Now().YearDay()
	if rm.lastResetDay != today {
		rm.dailyPnL = decimal.Zero
		rm.lastResetDay = today
		rm.consecutiveLoss = 0
		rm.circuitTripped = false
		log.Info().Msg("📅 Daily stats reset")
	}
}

// GetStats returns current risk stats
func (rm *Manager) GetStats() (dailyPnL decimal.Decimal, consecLoss int, circuitTripped bool) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.dailyPnL, rm.consecutiveLoss, rm.circuitTripped
}

// ═══════════════════════════════════════════════════════════════════════════════
// HELPERS
// ═══════════════════════════════════════════════════════════════════════════════

func envDecimalRM(key string, fallback float64) decimal.Decimal {
	if val := os.Getenv(key); val != "" {
		if d, err := decimal.NewFromString(val); err == nil {
			return d
		}
	}
	return decimal.NewFromFloat(fallback)
}

func envIntRM(key string, fallback int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return fallback
}
