package arbitrage

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ═══════════════════════════════════════════════════════════════════════════════
// SMART PROBABILITY MODEL
// ═══════════════════════════════════════════════════════════════════════════════
//
// Core insight: The probability of price reversal depends on:
// 1. HOW FAR price moved (bigger move = less likely to reverse)
// 2. HOW MUCH TIME remains (more time = more chance to reverse)
// 3. MOMENTUM (is price accelerating or slowing down?)
// 4. VOLATILITY (high vol = reversals more common)
//
// Example scenarios for BTC with $90,000 price to beat:
//
// | Current Price | Move    | Time Left | P(Reversal) | Fair DOWN | Strategy      |
// |---------------|---------|-----------|-------------|-----------|---------------|
// | $89,800       | -0.22%  | 10 min    | 15%         | 85¢       | DON'T BUY     |
// | $89,900       | -0.11%  | 10 min    | 40%         | 60¢       | MAYBE (if <50¢)|
// | $89,950       | -0.055% | 10 min    | 60%         | 40¢       | BUY (if <30¢) |
// | $89,980       | -0.022% | 10 min    | 75%         | 25¢       | BUY (if <15¢) |
//
// The key formula:
// Fair Price = 1 - P(Reversal)
// Edge = Fair Price - Market Price
// Only trade if Edge > minimum threshold (e.g., 15¢)
//
// ═══════════════════════════════════════════════════════════════════════════════

// ProbabilityModel calculates true probabilities for crypto price reversals
type ProbabilityModel struct {
	mu sync.RWMutex

	// Historical data for learning
	tradeHistory []ProbTradeOutcome

	// Calibrated parameters (can be tuned with more data)
	// These are based on typical BTC 15-minute window behavior
	params ModelParams

	// Per-asset volatility tracking
	volatility map[string]*VolatilityTracker
}

// ModelParams holds the calibrated model parameters
type ModelParams struct {
	// Base reversal probability at zero move
	BaseReversalProb float64 // ~0.50 (50/50 at no move)

	// Decay rate - how fast probability drops with move size
	// Higher = faster decay = less reversal chance for big moves
	MoveDecayRate float64 // ~150 for BTC (tuned empirically)

	// Time factor - how time affects reversal probability
	// More time = more chance to reverse
	TimeBoostFactor float64 // ~0.02 per minute remaining

	// Momentum dampening - strong momentum reduces reversal chance
	MomentumFactor float64 // ~0.5

	// Minimum edge required to trade (in cents)
	MinEdgeCents float64 // 15¢ minimum edge

	// Maximum acceptable loss probability
	MaxLossProb float64 // 0.60 (don't trade if >60% chance of loss)
}

// ProbTradeOutcome records historical trade for learning
type ProbTradeOutcome struct {
	Asset         string
	MovePct       float64 // Price move % at entry
	TimeRemaining float64 // Minutes remaining
	Volatility    float64 // 15m volatility at entry
	Momentum      float64 // Price momentum at entry
	Side          string  // UP or DOWN
	EntryPrice    float64 // Odds we paid
	Won           bool    // Did we win?
	PnL           float64 // Profit/loss
	Timestamp     time.Time
}

// VolatilityTracker tracks recent volatility for an asset
type VolatilityTracker struct {
	prices    []float64
	times     []time.Time
	maxLength int
}

// TradeDecision is the model's recommendation
type TradeDecision struct {
	ShouldTrade      bool
	Side             string          // UP or DOWN
	MarketPrice      decimal.Decimal // Current market price for this side
	FairPrice        decimal.Decimal // Model's fair value
	Edge             decimal.Decimal // FairPrice - MarketPrice (positive = underpriced)
	ReversalProb     float64         // P(price reverses back)
	WinProb          float64         // P(we win this trade)
	ExpectedValue    decimal.Decimal // Expected profit per dollar
	Confidence       float64         // Model confidence (0-1)
	Reason           string          // Human-readable explanation
	RiskLevel        string          // LOW, MEDIUM, HIGH
	RecommendedSize  decimal.Decimal // Suggested position size
}

// NewProbabilityModel creates a new model with default parameters
func NewProbabilityModel() *ProbabilityModel {
	return &ProbabilityModel{
		tradeHistory: make([]ProbTradeOutcome, 0),
		params: ModelParams{
			BaseReversalProb: 0.50,  // 50% at no move
			MoveDecayRate:    3.0,   // FIXED: Was 200 (way too aggressive!)
			                         // Now: 0.1% move → 37% reversal, 0.5% → 11%, 1% → 2.5%
			TimeBoostFactor:  0.03,  // More time boost for small moves
			MomentumFactor:   0.5,   // Momentum adjustment
			MinEdgeCents:     0.25,  // 25¢ minimum edge (even stricter)
			MaxLossProb:      0.35,  // Max 35% loss = need >65% win!
		},
		volatility: make(map[string]*VolatilityTracker),
	}
}

// Analyze calculates whether to trade and what the true probabilities are
func (pm *ProbabilityModel) Analyze(
	asset string,
	priceToBeat decimal.Decimal,
	currentPrice decimal.Decimal,
	upOdds decimal.Decimal,
	downOdds decimal.Decimal,
	timeRemaining time.Duration,
	momentum float64, // Price velocity (positive = going up)
) TradeDecision {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	// HARD CEILING: NEVER buy odds above 15¢ - learned this the hard way!
	maxOdds := decimal.NewFromFloat(0.15)
	upTooExpensive := upOdds.GreaterThan(maxOdds)
	downTooExpensive := downOdds.GreaterThan(maxOdds)
	
	if upTooExpensive && downTooExpensive {
		return TradeDecision{
			ShouldTrade: false,
			Reason:      fmt.Sprintf("Both sides too expensive: UP=%.0f¢ DOWN=%.0f¢ (max 15¢)", 
				upOdds.Mul(decimal.NewFromInt(100)).InexactFloat64(),
				downOdds.Mul(decimal.NewFromInt(100)).InexactFloat64()),
		}
	}

	// Calculate price move
	if priceToBeat.IsZero() || currentPrice.IsZero() {
		return TradeDecision{
			ShouldTrade: false,
			Reason:      "Missing price data",
		}
	}

	priceMove := currentPrice.Sub(priceToBeat)
	movePct := priceMove.Div(priceToBeat).InexactFloat64() * 100 // As percentage
	timeMin := timeRemaining.Minutes()

	// Determine which side is "winning" based on price move
	priceWentUp := currentPrice.GreaterThan(priceToBeat)
	absMovePct := math.Abs(movePct)

	// ═══════════════════════════════════════════════════════════════════════════
	// HARD BLOCK: If price moved significantly, NEVER bet against the move!
	// This is the #1 cause of losses - betting DOWN when price went UP
	// ═══════════════════════════════════════════════════════════════════════════
	if absMovePct > 0.05 { // More than 0.05% move
		// Only allow betting WITH the direction
		if priceWentUp && downOdds.LessThan(decimal.NewFromFloat(0.15)) {
			// Price went UP but DOWN is cheap - this is a TRAP!
			return TradeDecision{
				ShouldTrade: false,
				Reason:      fmt.Sprintf("BLOCKED: Price UP %.3f%% - DOWN at %.0f¢ is a trap!", 
					movePct, downOdds.Mul(decimal.NewFromInt(100)).InexactFloat64()),
			}
		}
		if !priceWentUp && upOdds.LessThan(decimal.NewFromFloat(0.15)) {
			// Price went DOWN but UP is cheap - this is a TRAP!
			return TradeDecision{
				ShouldTrade: false,
				Reason:      fmt.Sprintf("BLOCKED: Price DOWN %.3f%% - UP at %.0f¢ is a trap!", 
					movePct, upOdds.Mul(decimal.NewFromInt(100)).InexactFloat64()),
			}
		}
	}

	// Calculate reversal probability
	reversalProb := pm.calculateReversalProbability(movePct, timeMin, momentum)

	// Determine fair prices
	// If price went UP: UP should win, so UP is worth (1-reversalProb), DOWN is worth reversalProb
	// If price went DOWN: DOWN should win, so DOWN is worth (1-reversalProb), UP is worth reversalProb
	var fairUp, fairDown float64
	if priceWentUp {
		fairUp = 1.0 - reversalProb   // UP likely to win
		fairDown = reversalProb       // DOWN only wins if reversal
	} else {
		fairUp = reversalProb         // UP only wins if reversal
		fairDown = 1.0 - reversalProb // DOWN likely to win
	}

	fairUpDec := decimal.NewFromFloat(fairUp)
	fairDownDec := decimal.NewFromFloat(fairDown)

	// Calculate edge for each side
	edgeUp := fairUpDec.Sub(upOdds)     // Positive = UP is underpriced
	edgeDown := fairDownDec.Sub(downOdds) // Positive = DOWN is underpriced

	// Find the best opportunity
	var decision TradeDecision

	// Check UP side (skip if too expensive)
	if !upTooExpensive && edgeUp.GreaterThan(decimal.NewFromFloat(pm.params.MinEdgeCents)) {
		winProb := fairUp
		ev := pm.calculateExpectedValue(upOdds.InexactFloat64(), winProb)

		decision = TradeDecision{
			ShouldTrade:   true,
			Side:          "UP",
			MarketPrice:   upOdds,
			FairPrice:     fairUpDec,
			Edge:          edgeUp,
			ReversalProb:  reversalProb,
			WinProb:       winProb,
			ExpectedValue: decimal.NewFromFloat(ev),
			Confidence:    pm.calculateConfidence(math.Abs(movePct), timeMin),
			RiskLevel:     pm.assessRisk(winProb, math.Abs(movePct)),
		}
	}

	// Check DOWN side (prefer higher edge, skip if too expensive)
	if !downTooExpensive && edgeDown.GreaterThan(decimal.NewFromFloat(pm.params.MinEdgeCents)) {
		winProb := fairDown
		ev := pm.calculateExpectedValue(downOdds.InexactFloat64(), winProb)

		if !decision.ShouldTrade || edgeDown.GreaterThan(decision.Edge) {
			decision = TradeDecision{
				ShouldTrade:   true,
				Side:          "DOWN",
				MarketPrice:   downOdds,
				FairPrice:     fairDownDec,
				Edge:          edgeDown,
				ReversalProb:  reversalProb,
				WinProb:       winProb,
				ExpectedValue: decimal.NewFromFloat(ev),
				Confidence:    pm.calculateConfidence(math.Abs(movePct), timeMin),
				RiskLevel:     pm.assessRisk(winProb, math.Abs(movePct)),
			}
		}
	}

	// Final validation
	if decision.ShouldTrade {
		// Don't trade if win probability too low
		if decision.WinProb < (1 - pm.params.MaxLossProb) {
			decision.ShouldTrade = false
			decision.Reason = fmt.Sprintf("Win probability too low: %.1f%% (need >%.0f%%)",
				decision.WinProb*100, (1-pm.params.MaxLossProb)*100)
			return decision
		}

		// Don't trade if expected value is negative
		if decision.ExpectedValue.LessThanOrEqual(decimal.Zero) {
			decision.ShouldTrade = false
			decision.Reason = "Negative expected value"
			return decision
		}

		// Calculate recommended size using Kelly Criterion
		decision.RecommendedSize = pm.kellySize(decision.WinProb, decision.MarketPrice.InexactFloat64())

		// Build reason
		decision.Reason = fmt.Sprintf(
			"%s: Edge=%.0f¢ (Fair=%.0f¢ vs Market=%.0f¢), P(win)=%.0f%%, EV=%.1f%%",
			decision.Side,
			decision.Edge.Mul(decimal.NewFromInt(100)).InexactFloat64(),
			decision.FairPrice.Mul(decimal.NewFromInt(100)).InexactFloat64(),
			decision.MarketPrice.Mul(decimal.NewFromInt(100)).InexactFloat64(),
			decision.WinProb*100,
			decision.ExpectedValue.Mul(decimal.NewFromInt(100)).InexactFloat64(),
		)

		log.Info().
			Str("asset", asset).
			Str("side", decision.Side).
			Str("market_price", decision.MarketPrice.StringFixed(2)).
			Str("fair_price", decision.FairPrice.StringFixed(2)).
			Str("edge", decision.Edge.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢").
			Float64("win_prob", decision.WinProb).
			Float64("reversal_prob", reversalProb).
			Str("move_pct", fmt.Sprintf("%.3f%%", movePct)).
			Float64("time_min", timeMin).
			Str("risk", decision.RiskLevel).
			Msg("🧠 [PROB] Trade opportunity found!")
	} else {
		if decision.Reason == "" {
			decision.Reason = fmt.Sprintf(
				"No edge: UP fair=%.0f¢ mkt=%.0f¢, DOWN fair=%.0f¢ mkt=%.0f¢ (need %.0f¢ edge)",
				fairUp*100, upOdds.Mul(decimal.NewFromInt(100)).InexactFloat64(),
				fairDown*100, downOdds.Mul(decimal.NewFromInt(100)).InexactFloat64(),
				pm.params.MinEdgeCents*100,
			)
		}
	}

	return decision
}

// calculateReversalProbability estimates P(price reverses back to start)
// Uses exponential decay based on move size, boosted by time remaining
func (pm *ProbabilityModel) calculateReversalProbability(movePct, timeMin, momentum float64) float64 {
	// Base probability decays exponentially with move size
	// P = base * exp(-decay * |move|)
	absMove := math.Abs(movePct)

	// Exponential decay: bigger moves = lower reversal probability
	baseProb := pm.params.BaseReversalProb * math.Exp(-pm.params.MoveDecayRate*absMove)

	// Time boost: more time = more chance to reverse
	// But with diminishing returns
	timeBoost := 1.0 + pm.params.TimeBoostFactor*math.Sqrt(timeMin)

	// Momentum penalty: strong momentum in one direction = less likely to reverse
	// If momentum is in same direction as move, reduce reversal probability
	momentumPenalty := 1.0
	if (movePct > 0 && momentum > 0) || (movePct < 0 && momentum < 0) {
		// Momentum confirms the move - less likely to reverse
		momentumPenalty = 1.0 - pm.params.MomentumFactor*math.Min(math.Abs(momentum), 1.0)
	} else if (movePct > 0 && momentum < 0) || (movePct < 0 && momentum > 0) {
		// Momentum against the move - more likely to reverse!
		momentumPenalty = 1.0 + pm.params.MomentumFactor*math.Min(math.Abs(momentum), 1.0)*0.5
	}

	prob := baseProb * timeBoost * momentumPenalty

	// Clamp to valid probability range
	if prob < 0.05 {
		prob = 0.05 // Minimum 5% chance
	}
	if prob > 0.95 {
		prob = 0.95 // Maximum 95% chance
	}

	return prob
}

// calculateExpectedValue computes EV for a trade
// EV = P(win) * profit - P(lose) * cost
// For binary options: profit = (1 - price), cost = price
func (pm *ProbabilityModel) calculateExpectedValue(price, winProb float64) float64 {
	profit := 1.0 - price // If we win, we get $1 and paid price
	loss := price         // If we lose, we lose our cost

	ev := winProb*profit - (1-winProb)*loss

	return ev
}

// calculateConfidence estimates model confidence based on data quality
func (pm *ProbabilityModel) calculateConfidence(absMovePct, timeMin float64) float64 {
	// Higher confidence when:
	// 1. Move is clear (not near zero)
	// 2. Reasonable time remaining
	// 3. We have historical data

	conf := 0.5 // Base confidence

	// Clear moves are easier to evaluate
	if absMovePct > 0.05 {
		conf += 0.2
	}
	if absMovePct > 0.1 {
		conf += 0.1
	}

	// Sweet spot for time is 5-12 minutes
	if timeMin >= 5 && timeMin <= 12 {
		conf += 0.15
	}

	// Historical data improves confidence
	historyCount := len(pm.tradeHistory)
	if historyCount > 10 {
		conf += 0.05
	}
	if historyCount > 50 {
		conf += 0.05
	}
	if historyCount > 100 {
		conf += 0.05
	}

	if conf > 0.95 {
		conf = 0.95
	}

	return conf
}

// assessRisk categorizes the risk level
func (pm *ProbabilityModel) assessRisk(winProb, absMovePct float64) string {
	// High risk: low win prob or extreme moves
	if winProb < 0.45 || absMovePct > 0.3 {
		return "HIGH"
	}

	// Low risk: high win prob and small moves
	if winProb > 0.65 && absMovePct < 0.1 {
		return "LOW"
	}

	return "MEDIUM"
}

// kellySize calculates optimal position size using Kelly Criterion
// Kelly% = (bp - q) / b where b=odds, p=win prob, q=lose prob
func (pm *ProbabilityModel) kellySize(winProb, price float64) decimal.Decimal {
	if price <= 0 || price >= 1 {
		return decimal.NewFromFloat(0.25) // Default to 25%
	}

	// For binary options: b = (1/price - 1), p = winProb, q = 1-winProb
	b := (1.0 / price) - 1.0 // Decimal odds - 1
	p := winProb
	q := 1.0 - winProb

	kelly := (b*p - q) / b

	// Apply fractional Kelly (half Kelly for safety)
	kelly = kelly * 0.5

	// Clamp to reasonable range
	if kelly < 0.1 {
		kelly = 0.1 // Minimum 10% of bankroll
	}
	if kelly > 0.5 {
		kelly = 0.5 // Maximum 50% of bankroll
	}

	return decimal.NewFromFloat(kelly)
}

// RecordOutcome records a trade outcome for model improvement
func (pm *ProbabilityModel) RecordOutcome(outcome ProbTradeOutcome) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	outcome.Timestamp = time.Now()
	pm.tradeHistory = append(pm.tradeHistory, outcome)

	// Keep last 1000 trades
	if len(pm.tradeHistory) > 1000 {
		pm.tradeHistory = pm.tradeHistory[len(pm.tradeHistory)-1000:]
	}

	// TODO: Could recalibrate model parameters based on outcomes
	log.Debug().
		Str("asset", outcome.Asset).
		Str("side", outcome.Side).
		Bool("won", outcome.Won).
		Float64("pnl", outcome.PnL).
		Float64("move_pct", outcome.MovePct).
		Msg("📊 [PROB] Recorded trade outcome")
}

// GetStats returns model performance statistics
func (pm *ProbabilityModel) GetStats() map[string]interface{} {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	if len(pm.tradeHistory) == 0 {
		return map[string]interface{}{
			"total_trades": 0,
		}
	}

	var wins, losses int
	var totalPnL float64

	for _, t := range pm.tradeHistory {
		if t.Won {
			wins++
		} else {
			losses++
		}
		totalPnL += t.PnL
	}

	return map[string]interface{}{
		"total_trades": len(pm.tradeHistory),
		"wins":         wins,
		"losses":       losses,
		"win_rate":     float64(wins) / float64(len(pm.tradeHistory)),
		"total_pnl":    totalPnL,
	}
}
