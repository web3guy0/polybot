package arbitrage

import (
	"fmt"
	"math"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ProbabilityAnalysis calculates theoretical win rates for different strategies
// Based on BTC volatility and market behavior models

// VolatilityProfile represents BTC hourly volatility characteristics
type VolatilityProfile struct {
	AvgHourlyMove     float64 // Average hourly price move (%)
	StdDevHourlyMove  float64 // Standard deviation of hourly moves
	ReversalProb      float64 // Probability of reversal within window
	MeanReversionTime float64 // Average time for mean reversion (minutes)
}

// MarketConditions represents current market state
type MarketConditions struct {
	Profile           VolatilityProfile
	AvgSpread         float64 // Bid-ask spread on Polymarket
	OrderBookDepth    float64 // Typical liquidity at price levels
	CompetitorBots    int     // Estimated number of competing bots
	LatencyAdvantage  float64 // Our latency advantage in milliseconds
}

// StrategyProbabilities holds calculated probabilities for each strategy
type StrategyProbabilities struct {
	// Latency Arbitrage
	LatencyEntryProb     float64 // Prob of getting entry before odds update
	LatencyWinProb       float64 // Prob of winning given entry
	LatencyExpectedValue float64 // Expected $ return per trade

	// Smart Dual Side
	DualCheapSideProb     float64 // Prob one side gets cheap (<38¢)
	DualReversalProb      float64 // Prob of reversal after first entry
	DualBothFilledProb    float64 // Prob both sides fill
	DualSingleWinProb     float64 // Prob of win if only one side fills
	DualExpectedValue     float64 // Expected $ return per window

	// Combined Strategy
	CombinedExpectedValue float64
	CombinedWinRate       float64
}

// DefaultVolatilityProfiles for different market conditions
var (
	LowVolProfile = VolatilityProfile{
		AvgHourlyMove:     0.15,
		StdDevHourlyMove:  0.10,
		ReversalProb:      0.25,
		MeanReversionTime: 45,
	}

	MediumVolProfile = VolatilityProfile{
		AvgHourlyMove:     0.35,
		StdDevHourlyMove:  0.20,
		ReversalProb:      0.40,
		MeanReversionTime: 30,
	}

	HighVolProfile = VolatilityProfile{
		AvgHourlyMove:     0.80,
		StdDevHourlyMove:  0.40,
		ReversalProb:      0.55,
		MeanReversionTime: 20,
	}

	ExtremeVolProfile = VolatilityProfile{
		AvgHourlyMove:     1.50,
		StdDevHourlyMove:  0.80,
		ReversalProb:      0.65,
		MeanReversionTime: 15,
	}
)

// CalculateProbabilities computes theoretical win rates
func CalculateProbabilities(conditions MarketConditions) StrategyProbabilities {
	p := StrategyProbabilities{}

	// === LATENCY ARBITRAGE PROBABILITIES ===

	// Probability of getting entry before odds update
	// Depends on our latency advantage vs competitors
	// Model: Exponential decay based on number of competitors
	competitorFactor := math.Exp(-float64(conditions.CompetitorBots) * 0.1)
	latencyFactor := math.Min(1.0, conditions.LatencyAdvantage/100.0) // 100ms = full advantage

	// Base entry probability when price moves enough
	baseMoveProb := normalCDF(conditions.Profile.AvgHourlyMove, 0.10, conditions.Profile.StdDevHourlyMove)
	p.LatencyEntryProb = baseMoveProb * competitorFactor * latencyFactor

	// Win probability given entry
	// If we enter on a real price move, the move tends to persist
	// Model: 60% base + bonus for larger moves
	p.LatencyWinProb = 0.60 + (conditions.Profile.AvgHourlyMove * 0.10)
	if p.LatencyWinProb > 0.85 {
		p.LatencyWinProb = 0.85 // Cap at 85%
	}

	// Expected value per opportunity
	// Assume entry at 50¢, exit at 75¢ or resolution
	avgEntry := 0.50
	avgExitWin := 0.80    // Average exit when winning
	avgExitLoss := 0.20   // Average exit when losing
	takerFee := 0.001     // 0.1% taker fee

	grossWin := (avgExitWin - avgEntry) * (1 - takerFee)
	grossLoss := (avgEntry - avgExitLoss) * (1 + takerFee)

	p.LatencyExpectedValue = (p.LatencyWinProb * grossWin) - ((1 - p.LatencyWinProb) * grossLoss)

	// === SMART DUAL SIDE PROBABILITIES ===

	// Probability one side gets cheap (<38¢)
	// This happens when price moves significantly in one direction
	// Model: Need ~0.3% move for odds to shift to 38/62
	moveNeeded := 0.30
	p.DualCheapSideProb = normalCDF(conditions.Profile.AvgHourlyMove, moveNeeded, conditions.Profile.StdDevHourlyMove)

	// Probability of reversal after first entry
	// This is the key to dual-side profitability
	p.DualReversalProb = conditions.Profile.ReversalProb

	// Probability BOTH sides fill
	// P(both) = P(first cheap) × P(reversal | first cheap)
	// But reversal needs to be large enough (another 0.3% move back)
	reversalMagnitudeProb := normalCDF(conditions.Profile.AvgHourlyMove, moveNeeded, conditions.Profile.StdDevHourlyMove)
	p.DualBothFilledProb = p.DualCheapSideProb * p.DualReversalProb * reversalMagnitudeProb

	// Win probability if only one side fills
	// If we entered cheap side and no reversal, we're betting directionally
	// Cheap side typically means we're betting AGAINST the current trend
	// Trend continuation probability depends on volatility
	trendContinuationProb := 1 - conditions.Profile.ReversalProb
	p.DualSingleWinProb = 1 - trendContinuationProb // We win if trend reverses

	// Expected value for dual-side strategy
	// Case 1: Both fill - guaranteed profit
	bothFilledProfit := 1.0 - 0.76 // $0.38 + $0.38 = $0.76 cost, $1.00 payout
	bothFilledEV := p.DualBothFilledProb * bothFilledProfit

	// Case 2: Only first fills, wins
	singleWinProfit := 1.0 - 0.38 // $0.38 cost, $1.00 payout
	singleWinEV := (p.DualCheapSideProb - p.DualBothFilledProb) * p.DualSingleWinProb * singleWinProfit

	// Case 3: Only first fills, loses
	singleLossAmount := 0.38
	singleLossEV := (p.DualCheapSideProb - p.DualBothFilledProb) * (1 - p.DualSingleWinProb) * (-singleLossAmount)

	p.DualExpectedValue = bothFilledEV + singleWinEV + singleLossEV

	// === COMBINED METRICS ===
	p.CombinedExpectedValue = p.LatencyExpectedValue + p.DualExpectedValue
	
	// Overall win rate (weighted by entry probability)
	totalEntryProb := p.LatencyEntryProb + p.DualCheapSideProb
	if totalEntryProb > 0 {
		latencyWinContrib := p.LatencyEntryProb * p.LatencyWinProb
		dualWinContrib := p.DualBothFilledProb + (p.DualCheapSideProb-p.DualBothFilledProb)*p.DualSingleWinProb
		p.CombinedWinRate = (latencyWinContrib + dualWinContrib) / totalEntryProb
	}

	return p
}

// normalCDF calculates the cumulative distribution function for normal distribution
func normalCDF(x, mean, stdDev float64) float64 {
	return 0.5 * (1 + math.Erf((x-mean)/(stdDev*math.Sqrt2)))
}

// PrintProbabilityAnalysis outputs a detailed analysis
func PrintProbabilityAnalysis() {
	log.Info().Msg("")
	log.Info().Msg("╔══════════════════════════════════════════════════════════════════════════╗")
	log.Info().Msg("║                    POLYBOT WIN RATE PROBABILITY ANALYSIS                 ║")
	log.Info().Msg("╚══════════════════════════════════════════════════════════════════════════╝")
	log.Info().Msg("")

	// Analyze for each volatility condition
	profiles := map[string]VolatilityProfile{
		"LOW (quiet market)":    LowVolProfile,
		"MEDIUM (normal)":       MediumVolProfile,
		"HIGH (active)":         HighVolProfile,
		"EXTREME (news/events)": ExtremeVolProfile,
	}

	baseConditions := MarketConditions{
		AvgSpread:        0.02,  // 2% spread
		OrderBookDepth:   1000,  // $1000 typical depth
		CompetitorBots:   5,     // 5 competing bots
		LatencyAdvantage: 50,    // 50ms advantage
	}

	for name, profile := range profiles {
		conditions := baseConditions
		conditions.Profile = profile

		probs := CalculateProbabilities(conditions)

		log.Info().Msg("────────────────────────────────────────────────────────────────────────────")
		log.Info().Msgf("  📊 VOLATILITY: %s", name)
		log.Info().Msgf("     Avg Hourly Move: %.2f%%, Reversal Prob: %.0f%%", 
			profile.AvgHourlyMove, profile.ReversalProb*100)
		log.Info().Msg("────────────────────────────────────────────────────────────────────────────")
		log.Info().Msg("")
		log.Info().Msg("  🎯 LATENCY ARBITRAGE:")
		log.Info().Msgf("     Entry Probability:     %5.1f%%", probs.LatencyEntryProb*100)
		log.Info().Msgf("     Win Rate (if entered): %5.1f%%", probs.LatencyWinProb*100)
		log.Info().Msgf("     Expected Value/Trade:  $%+.3f", probs.LatencyExpectedValue)
		log.Info().Msg("")
		log.Info().Msg("  🎲 SMART DUAL-SIDE:")
		log.Info().Msgf("     Cheap Side Probability:  %5.1f%%", probs.DualCheapSideProb*100)
		log.Info().Msgf("     Reversal Probability:    %5.1f%%", probs.DualReversalProb*100)
		log.Info().Msgf("     Both Fill Probability:   %5.1f%%", probs.DualBothFilledProb*100)
		log.Info().Msgf("     Single Side Win Rate:    %5.1f%%", probs.DualSingleWinProb*100)
		log.Info().Msgf("     Expected Value/Window:   $%+.3f", probs.DualExpectedValue)
		log.Info().Msg("")
		log.Info().Msg("  📈 COMBINED STRATEGY:")
		log.Info().Msgf("     Overall Win Rate:        %5.1f%%", probs.CombinedWinRate*100)
		log.Info().Msgf("     Expected Value:          $%+.3f", probs.CombinedExpectedValue)
		log.Info().Msg("")
	}

	// Summary table
	log.Info().Msg("╔══════════════════════════════════════════════════════════════════════════╗")
	log.Info().Msg("║                         STRATEGY COMPARISON                              ║")
	log.Info().Msg("╠══════════════════════════════════════════════════════════════════════════╣")
	log.Info().Msg("║  Condition     │ Latency EV │ Dual EV  │ Combined │ Win Rate            ║")
	log.Info().Msg("╠══════════════════════════════════════════════════════════════════════════╣")

	for _, name := range []string{"LOW (quiet market)", "MEDIUM (normal)", "HIGH (active)", "EXTREME (news/events)"} {
		profile := profiles[name]
		conditions := baseConditions
		conditions.Profile = profile
		probs := CalculateProbabilities(conditions)

		shortName := name[:12]
		log.Info().Msgf("║  %-12s │   $%+.3f  │  $%+.3f │  $%+.3f │   %5.1f%%            ║",
			shortName, probs.LatencyExpectedValue, probs.DualExpectedValue,
			probs.CombinedExpectedValue, probs.CombinedWinRate*100)
	}
	log.Info().Msg("╚══════════════════════════════════════════════════════════════════════════╝")
	log.Info().Msg("")

	// Recommendations
	log.Info().Msg("💡 RECOMMENDATIONS:")
	log.Info().Msg("   • LOW volatility:  Focus on latency arb only, dual-side rarely fills")
	log.Info().Msg("   • MEDIUM volatility: Run both strategies, dual-side ~15% both-fill rate")
	log.Info().Msg("   • HIGH volatility: Dual-side becomes profitable, ~25% both-fill rate")
	log.Info().Msg("   • EXTREME volatility: Best for dual-side, but latency arb also great")
	log.Info().Msg("")
	log.Info().Msg("📊 KEY METRICS TO WATCH:")
	log.Info().Msg("   • Both-Fill Rate > 20% = Dual-side strategy is working")
	log.Info().Msg("   • Single-Side Win Rate > 40% = Mean reversion is happening")
	log.Info().Msg("   • Latency Win Rate > 60% = Speed advantage is real")
	log.Info().Msg("")
}

// MonteCarloSimulation runs many simulated windows to estimate outcomes
func MonteCarloSimulation(conditions MarketConditions, numWindows int) {
	log.Info().Msgf("🎲 Running Monte Carlo simulation (%d windows)...", numWindows)

	var (
		latencyWins, latencyLosses             int
		dualBothFilled, dualSingleWin, dualSingleLoss int
		dualNeverEntered                       int
		totalProfit                            float64
	)

	probs := CalculateProbabilities(conditions)

	for i := 0; i < numWindows; i++ {
		// Simulate latency arbitrage
		if randFloat() < probs.LatencyEntryProb {
			if randFloat() < probs.LatencyWinProb {
				latencyWins++
				totalProfit += 0.30 // Average win
			} else {
				latencyLosses++
				totalProfit -= 0.30 // Average loss
			}
		}

		// Simulate dual-side
		if randFloat() < probs.DualCheapSideProb {
			if randFloat() < probs.DualBothFilledProb/probs.DualCheapSideProb {
				dualBothFilled++
				totalProfit += 0.24 // Guaranteed profit (76¢ cost, $1 payout)
			} else {
				// Single side only
				if randFloat() < probs.DualSingleWinProb {
					dualSingleWin++
					totalProfit += 0.62 // Win on 38¢ entry
				} else {
					dualSingleLoss++
					totalProfit -= 0.38 // Lose 38¢
				}
			}
		} else {
			dualNeverEntered++
		}
	}

	log.Info().Msg("")
	log.Info().Msg("╔══════════════════════════════════════════════════════════════╗")
	log.Info().Msg("║              MONTE CARLO SIMULATION RESULTS                  ║")
	log.Info().Msg("╠══════════════════════════════════════════════════════════════╣")
	log.Info().Msgf("║  Windows Simulated:     %6d                               ║", numWindows)
	log.Info().Msg("╠══════════════════════════════════════════════════════════════╣")
	log.Info().Msg("║  LATENCY ARBITRAGE:                                          ║")
	log.Info().Msgf("║    Wins:   %5d  │  Losses: %5d  │  Win Rate: %5.1f%%       ║",
		latencyWins, latencyLosses,
		float64(latencyWins)/float64(max(latencyWins+latencyLosses, 1))*100)
	log.Info().Msg("╠══════════════════════════════════════════════════════════════╣")
	log.Info().Msg("║  SMART DUAL-SIDE:                                            ║")
	log.Info().Msgf("║    Both Filled:    %5d (%.1f%% of entries)                  ║",
		dualBothFilled,
		float64(dualBothFilled)/float64(max(dualBothFilled+dualSingleWin+dualSingleLoss, 1))*100)
	log.Info().Msgf("║    Single Win:     %5d                                     ║", dualSingleWin)
	log.Info().Msgf("║    Single Loss:    %5d                                     ║", dualSingleLoss)
	log.Info().Msgf("║    Never Entered:  %5d                                     ║", dualNeverEntered)
	log.Info().Msg("╠══════════════════════════════════════════════════════════════╣")
	log.Info().Msgf("║  TOTAL PROFIT:     $%+8.2f                                ║", totalProfit)
	log.Info().Msgf("║  PER WINDOW:       $%+8.4f                                ║", totalProfit/float64(numWindows))
	log.Info().Msg("╚══════════════════════════════════════════════════════════════╝")
	log.Info().Msg("")
}

// Simple random float [0,1)
var randState uint64 = 12345

func randFloat() float64 {
	randState = randState*6364136223846793005 + 1442695040888963407
	return float64(randState>>33) / float64(1<<31)
}

// GetWinRateSummary returns a formatted string with key win rates
func GetWinRateSummary(conditions MarketConditions) string {
	probs := CalculateProbabilities(conditions)
	return fmt.Sprintf(
		"Latency: %.0f%% win | Dual: %.0f%% both-fill, %.0f%% single-win | Combined EV: $%.3f",
		probs.LatencyWinProb*100,
		probs.DualBothFilledProb*100,
		probs.DualSingleWinProb*100,
		probs.CombinedExpectedValue,
	)
}

// EstimatedDailyProfit calculates expected daily profit
func EstimatedDailyProfit(windowsPerDay int, avgPositionSize decimal.Decimal, conditions MarketConditions) decimal.Decimal {
	probs := CalculateProbabilities(conditions)
	evPerWindow := decimal.NewFromFloat(probs.CombinedExpectedValue)
	return evPerWindow.Mul(decimal.NewFromInt(int64(windowsPerDay))).Mul(avgPositionSize)
}
