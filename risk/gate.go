package risk

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ═══════════════════════════════════════════════════════════════════════════════
// TRADE REQUEST - Central approval system
// ═══════════════════════════════════════════════════════════════════════════════
//
// Strategy asks → Risk approves/rejects → Executor executes
//
// This enforces ALL capital protection rules in ONE place
//
// ═══════════════════════════════════════════════════════════════════════════════

// TradeRequest represents a request to enter a trade
type TradeRequest struct {
	Asset      string          // BTC, ETH, SOL
	Side       string          // YES or NO
	Action     string          // BUY or SELL
	Price      decimal.Decimal // Entry/exit price
	Size       decimal.Decimal // Requested size (shares)
	Strategy   string          // Which strategy is requesting
	Phase      string          // Market phase (OPENING, CLOSING, etc.)
	Reason     string          // Why this trade (for logging)
	Metadata   map[string]any  // Extra context
}

// TradeApproval is the response to a trade request
type TradeApproval struct {
	Approved     bool
	AdjustedSize decimal.Decimal // Risk may reduce size
	RejectionMsg string
	RiskScore    float64 // 0-100, higher = more risky
}

// RiskGate is the centralized risk approval system
type RiskGate struct {
	mu sync.RWMutex

	// Configuration
	maxPositionPct    decimal.Decimal // Max % of balance in one position (25%)
	maxTotalExposure  decimal.Decimal // Max total exposure (50%)
	dailyLossLimitPct decimal.Decimal // Max daily loss as % of balance (3%)
	maxConsecLosses   int             // Circuit breaker threshold (3)
	positionCooldown  time.Duration   // Cooldown after exit (30s)
	maxPositionsPerAsset int          // Max positions per asset (1)

	// State
	currentBalance   decimal.Decimal
	dailyPnL         decimal.Decimal
	dailyStartBalance decimal.Decimal
	consecutiveLosses int
	circuitTripped   bool
	circuitTrippedAt time.Time
	lastResetDay     int

	// Per-asset tracking
	assetLosses      map[string]int       // Loss count per asset
	assetDisabled    map[string]bool      // Disabled assets
	assetLastExit    map[string]time.Time // Last exit time per asset
	assetPositions   map[string]int       // Open position count per asset

	// Callbacks
	onCircuitTrip func(reason string)
}

// NewRiskGate creates the centralized risk approval system
func NewRiskGate(initialBalance decimal.Decimal) *RiskGate {
	// Read config from env with defaults
	maxPositionPct := 0.25 // 25% default
	if v := os.Getenv("MAX_POSITION_PCT"); v != "" {
		if val, err := strconv.ParseFloat(v, 64); err == nil {
			maxPositionPct = val / 100 // Convert from percentage
		}
	}
	
	dailyLossLimitPct := 0.03 // 3% default
	if v := os.Getenv("MAX_DAILY_LOSS_PCT"); v != "" {
		if val, err := strconv.ParseFloat(v, 64); err == nil {
			dailyLossLimitPct = val / 100 // Convert from percentage
		}
	}
	
	maxConsecLosses := 3
	if v := os.Getenv("MAX_CONSECUTIVE_LOSSES"); v != "" {
		if val, err := strconv.Atoi(v); err == nil {
			maxConsecLosses = val
		}
	}
	
	positionCooldown := 30 * time.Second
	if v := os.Getenv("POSITION_COOLDOWN_SEC"); v != "" {
		if val, err := strconv.Atoi(v); err == nil {
			positionCooldown = time.Duration(val) * time.Second
		}
	}

	rg := &RiskGate{
		maxPositionPct:    decimal.NewFromFloat(maxPositionPct),
		maxTotalExposure:  decimal.NewFromFloat(0.50), // 50% max total
		dailyLossLimitPct: decimal.NewFromFloat(dailyLossLimitPct),
		maxConsecLosses:   maxConsecLosses,
		positionCooldown:  positionCooldown,
		maxPositionsPerAsset: 1,

		currentBalance:    initialBalance,
		dailyStartBalance: initialBalance,
		dailyPnL:          decimal.Zero,

		assetLosses:    make(map[string]int),
		assetDisabled:  make(map[string]bool),
		assetLastExit:  make(map[string]time.Time),
		assetPositions: make(map[string]int),
	}

	log.Info().
		Str("balance", initialBalance.StringFixed(2)).
		Str("max_position", fmt.Sprintf("%.0f%%", maxPositionPct*100)).
		Str("daily_loss_limit", fmt.Sprintf("%.0f%%", dailyLossLimitPct*100)).
		Int("max_consec_losses", rg.maxConsecLosses).
		Dur("cooldown", rg.positionCooldown).
		Msg("🛡️ Risk Gate initialized")

	return rg
}

// ═══════════════════════════════════════════════════════════════════════════════
// ENTRY APPROVAL
// ═══════════════════════════════════════════════════════════════════════════════

// CanEnter checks if a trade entry is allowed
func (rg *RiskGate) CanEnter(req TradeRequest) TradeApproval {
	rg.mu.Lock()
	defer rg.mu.Unlock()

	// Reset daily stats if new day
	rg.checkDayReset()

	// Build rejection helper
	reject := func(msg string) TradeApproval {
		log.Debug().
			Str("asset", req.Asset).
			Str("reason", msg).
			Msg("🚫 Trade rejected")
		return TradeApproval{
			Approved:     false,
			RejectionMsg: msg,
		}
	}

	// ══════════════════════════════════════════════════════════════════════════
	// HARD BLOCKS (cannot override)
	// ══════════════════════════════════════════════════════════════════════════

	// 1. Circuit breaker
	if rg.circuitTripped {
		if time.Since(rg.circuitTrippedAt) < 30*time.Minute {
			return reject("circuit breaker active")
		}
		// Reset circuit breaker after cooldown
		rg.circuitTripped = false
		rg.consecutiveLosses = 0
		log.Info().Msg("✅ Circuit breaker reset after cooldown")
	}

	// 2. Daily loss limit
	dailyLossLimit := rg.dailyStartBalance.Mul(rg.dailyLossLimitPct)
	if rg.dailyPnL.LessThan(dailyLossLimit.Neg()) {
		return reject("daily loss limit hit")
	}

	// 3. Asset disabled (2+ losses on this asset)
	if rg.assetDisabled[req.Asset] {
		return reject("asset disabled after 2 losses")
	}

	// 4. Already have position on this asset
	if rg.assetPositions[req.Asset] >= rg.maxPositionsPerAsset {
		return reject("already have position on this asset")
	}

	// 5. Cooldown after last exit
	if lastExit, ok := rg.assetLastExit[req.Asset]; ok {
		if time.Since(lastExit) < rg.positionCooldown {
			remaining := rg.positionCooldown - time.Since(lastExit)
			return reject(fmt.Sprintf("cooldown active (%.0fs remaining)", remaining.Seconds()))
		}
	}

	// ══════════════════════════════════════════════════════════════════════════
	// SIZE ADJUSTMENTS
	// ══════════════════════════════════════════════════════════════════════════

	adjustedSize := req.Size

	// 6. Max position size (25% of balance)
	maxPositionValue := rg.currentBalance.Mul(rg.maxPositionPct)
	positionValue := req.Price.Mul(req.Size)
	if positionValue.GreaterThan(maxPositionValue) {
		adjustedSize = maxPositionValue.Div(req.Price)
		log.Debug().
			Str("asset", req.Asset).
			Str("original", req.Size.StringFixed(2)).
			Str("adjusted", adjustedSize.StringFixed(2)).
			Msg("📉 Size reduced to max position limit")
	}

	// 7. Reduce size in Closing phase (70%)
	if req.Phase == "CLOSING" {
		adjustedSize = adjustedSize.Mul(decimal.NewFromFloat(0.7))
		log.Debug().
			Str("asset", req.Asset).
			Str("phase", req.Phase).
			Msg("📉 Size reduced 30% for Closing phase")
	}

	// 8. Minimum size check
	minSize := decimal.NewFromFloat(1)
	if adjustedSize.LessThan(minSize) {
		return reject("position size too small after adjustments")
	}

	// ══════════════════════════════════════════════════════════════════════════
	// CALCULATE RISK SCORE
	// ══════════════════════════════════════════════════════════════════════════

	riskScore := rg.calculateRiskScore(req, adjustedSize)

	// ══════════════════════════════════════════════════════════════════════════
	// APPROVED
	// ══════════════════════════════════════════════════════════════════════════

	// Mark position as open
	rg.assetPositions[req.Asset]++

	log.Info().
		Str("asset", req.Asset).
		Str("side", req.Side).
		Str("size", adjustedSize.StringFixed(2)).
		Str("phase", req.Phase).
		Float64("risk_score", riskScore).
		Msg("✅ Trade approved by Risk Gate")

	return TradeApproval{
		Approved:     true,
		AdjustedSize: adjustedSize,
		RiskScore:    riskScore,
	}
}

// calculateRiskScore returns 0-100 risk score
func (rg *RiskGate) calculateRiskScore(req TradeRequest, size decimal.Decimal) float64 {
	score := 0.0

	// Consecutive losses add risk
	score += float64(rg.consecutiveLosses) * 15

	// Asset losses add risk
	score += float64(rg.assetLosses[req.Asset]) * 20

	// Daily PnL impact
	dailyLossLimit := rg.dailyStartBalance.Mul(rg.dailyLossLimitPct)
	if rg.dailyPnL.IsNegative() {
		pctOfLimit := rg.dailyPnL.Abs().Div(dailyLossLimit).InexactFloat64() * 100
		score += pctOfLimit * 0.3
	}

	// Phase risk
	if req.Phase == "CLOSING" {
		score += 10 // Closing phase is riskier
	}

	// Clamp to 0-100
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}

	return score
}

// ═══════════════════════════════════════════════════════════════════════════════
// EXIT HANDLING
// ═══════════════════════════════════════════════════════════════════════════════

// CanExit checks if a trade exit is allowed (almost always yes, but for logging)
func (rg *RiskGate) CanExit(req TradeRequest) TradeApproval {
	// Exits are almost always allowed - we don't want to trap in bad positions
	log.Debug().
		Str("asset", req.Asset).
		Str("reason", req.Reason).
		Msg("🔓 Exit approved")

	return TradeApproval{
		Approved:     true,
		AdjustedSize: req.Size,
	}
}

// RecordExit updates state after position close
func (rg *RiskGate) RecordExit(asset string, pnl decimal.Decimal) {
	rg.mu.Lock()
	defer rg.mu.Unlock()

	// Update balance and daily PnL
	rg.currentBalance = rg.currentBalance.Add(pnl)
	rg.dailyPnL = rg.dailyPnL.Add(pnl)

	// Update position count
	if rg.assetPositions[asset] > 0 {
		rg.assetPositions[asset]--
	}

	// Record cooldown
	rg.assetLastExit[asset] = time.Now()

	if pnl.LessThan(decimal.Zero) {
		// Loss
		rg.consecutiveLosses++
		rg.assetLosses[asset]++

		// Check circuit breaker
		if rg.consecutiveLosses >= rg.maxConsecLosses {
			rg.circuitTripped = true
			rg.circuitTrippedAt = time.Now()
			log.Error().
				Int("consecutive_losses", rg.consecutiveLosses).
				Msg("🚨 CIRCUIT BREAKER TRIPPED")

			if rg.onCircuitTrip != nil {
				rg.onCircuitTrip("consecutive losses")
			}
		}

		// Check asset disable (after 2 losses)
		if rg.assetLosses[asset] >= 2 && !rg.assetDisabled[asset] {
			rg.assetDisabled[asset] = true
			log.Error().
				Str("asset", asset).
				Int("losses", rg.assetLosses[asset]).
				Msg("🛑 Asset disabled after 2 losses")
		}

		log.Warn().
			Str("asset", asset).
			Str("pnl", pnl.StringFixed(2)).
			Int("consec_losses", rg.consecutiveLosses).
			Int("asset_losses", rg.assetLosses[asset]).
			Msg("📉 Loss recorded")

	} else {
		// Win - reset consecutive losses
		rg.consecutiveLosses = 0

		log.Info().
			Str("asset", asset).
			Str("pnl", pnl.StringFixed(2)).
			Msg("📈 Win recorded")
	}
}

// ═══════════════════════════════════════════════════════════════════════════════
// STATE MANAGEMENT
// ═══════════════════════════════════════════════════════════════════════════════

// checkDayReset resets daily stats at midnight
func (rg *RiskGate) checkDayReset() {
	today := time.Now().YearDay()
	if rg.lastResetDay != today {
		rg.dailyPnL = decimal.Zero
		rg.dailyStartBalance = rg.currentBalance
		rg.lastResetDay = today
		rg.consecutiveLosses = 0
		rg.circuitTripped = false
		
		// Reset per-asset tracking
		rg.assetLosses = make(map[string]int)
		rg.assetDisabled = make(map[string]bool)
		
		log.Info().
			Str("balance", rg.currentBalance.StringFixed(2)).
			Msg("📅 Daily risk stats reset")
	}
}

// SetBalance updates current balance (for initial load or external update)
func (rg *RiskGate) SetBalance(balance decimal.Decimal) {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	rg.currentBalance = balance
	if rg.dailyStartBalance.IsZero() {
		rg.dailyStartBalance = balance
	}
}

// GetStats returns current risk state
func (rg *RiskGate) GetStats() map[string]interface{} {
	rg.mu.RLock()
	defer rg.mu.RUnlock()

	dailyLossLimit := rg.dailyStartBalance.Mul(rg.dailyLossLimitPct)
	dailyLossUsed := decimal.Zero
	if rg.dailyPnL.IsNegative() {
		dailyLossUsed = rg.dailyPnL.Abs().Div(dailyLossLimit).Mul(decimal.NewFromInt(100))
	}

	return map[string]interface{}{
		"balance":           rg.currentBalance.StringFixed(2),
		"daily_pnl":         rg.dailyPnL.StringFixed(2),
		"daily_loss_limit":  dailyLossLimit.StringFixed(2),
		"daily_loss_used":   dailyLossUsed.StringFixed(1) + "%",
		"consecutive_losses": rg.consecutiveLosses,
		"circuit_tripped":   rg.circuitTripped,
		"disabled_assets":   rg.assetDisabled,
	}
}

// OnCircuitTrip sets callback for circuit breaker events
func (rg *RiskGate) OnCircuitTrip(fn func(reason string)) {
	rg.onCircuitTrip = fn
}

// IsAssetDisabled checks if an asset is disabled
func (rg *RiskGate) IsAssetDisabled(asset string) bool {
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	return rg.assetDisabled[asset]
}

// IsDailyLimitHit checks if daily loss limit is hit
func (rg *RiskGate) IsDailyLimitHit() bool {
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	dailyLossLimit := rg.dailyStartBalance.Mul(rg.dailyLossLimitPct)
	return rg.dailyPnL.LessThan(dailyLossLimit.Neg())
}

// IsCircuitTripped returns circuit breaker state
func (rg *RiskGate) IsCircuitTripped() bool {
	rg.mu.RLock()
	defer rg.mu.RUnlock()
	return rg.circuitTripped
}
