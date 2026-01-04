package strategy

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/web3guy0/polybot/internal/indicators"
)

// Crypto15mStrategy implements a 15-minute crypto prediction strategy
// This strategy combines multiple technical indicators to predict
// short-term price movements for Polymarket windows
// Asset is configurable - works for BTC, ETH, SOL, etc.
type Crypto15mStrategy struct {
	BaseStrategy
	
	// Indicator weights (sum to 1.0)
	weights IndicatorWeights
}

// IndicatorWeights defines how much each indicator contributes to the final score
type IndicatorWeights struct {
	RSI           float64
	Momentum      float64
	Volume        float64
	OrderBook     float64
	FundingRate   float64
	BuySellRatio  float64
}

// DefaultCrypto15mWeights returns battle-tested weights for crypto 15m
func DefaultCrypto15mWeights() IndicatorWeights {
	return IndicatorWeights{
		RSI:          0.20, // 20% - Overbought/oversold
		Momentum:     0.25, // 25% - Price velocity (most important for short-term)
		Volume:       0.15, // 15% - Volume confirmation
		OrderBook:    0.20, // 20% - Order flow imbalance
		FundingRate:  0.10, // 10% - Market sentiment
		BuySellRatio: 0.10, // 10% - Taker buy/sell pressure
	}
}

// NewCrypto15mStrategy creates a new crypto 15-minute strategy for any asset
func NewCrypto15mStrategy(asset string) *Crypto15mStrategy {
	return &Crypto15mStrategy{
		BaseStrategy: NewBaseStrategy(
			fmt.Sprintf("%s_15m", asset),
			fmt.Sprintf("%s 15-minute prediction using technical indicators", asset),
			asset,
			15*time.Minute,
			0.60, // Minimum 60% confidence to trade
			20,   // Need at least 20 data points for warmup
		),
		weights: DefaultCrypto15mWeights(),
	}
}

// NewCrypto15mStrategyWithWeights creates a strategy with custom weights
func NewCrypto15mStrategyWithWeights(asset string, weights IndicatorWeights) *Crypto15mStrategy {
	s := NewCrypto15mStrategy(asset)
	s.weights = weights
	return s
}

// Evaluate implements the Strategy interface
// This is where ALL trading decisions are made for crypto assets
func (s *Crypto15mStrategy) Evaluate(ctx context.Context, market MarketContext) (Signal, error) {
	signal := Signal{
		Direction:   DirectionNoTrade,
		Indicators:  make(map[string]float64),
		GeneratedAt: time.Now(),
		ExpiresAt:   time.Now().Add(s.timeframe),
		Reason:      "Initializing",
		Reasons:     []string{},
	}

	// Check warmup
	if len(market.Prices) < s.warmupPeriods {
		signal.Reasons = append(signal.Reasons, "Insufficient data for warmup")
		return signal, nil
	}

	// Calculate individual indicator scores
	scores := s.calculateIndicatorScores(market)
	
	// Store raw indicator values
	signal.Indicators = scores
	
	// Calculate weighted composite score
	compositeScore := s.calculateCompositeScore(scores)
	signal.Score = compositeScore
	
	// Determine direction and strength
	absScore := math.Abs(compositeScore)
	signal.Strength = CalculateStrength(absScore)
	signal.Confidence = CalculateConfidence(absScore)
	
	if compositeScore > 20 {
		signal.Direction = DirectionUp
		signal.Reason = fmt.Sprintf("Bullish signal: composite score %.1f", compositeScore)
	} else if compositeScore < -20 {
		signal.Direction = DirectionDown
		signal.Reason = fmt.Sprintf("Bearish signal: composite score %.1f", compositeScore)
	} else {
		signal.Direction = DirectionNoTrade
		signal.Reason = fmt.Sprintf("Score too weak (%.1f) for directional signal", compositeScore)
	}
	signal.Reasons = append(signal.Reasons, signal.Reason)
	
	// Add detailed reasoning
	signal.Reasons = append(signal.Reasons, s.generateReasons(scores, compositeScore)...)
	
	return signal, nil
}

// calculateIndicatorScores computes individual indicator scores (-100 to +100)
func (s *Crypto15mStrategy) calculateIndicatorScores(market MarketContext) map[string]float64 {
	scores := make(map[string]float64)
	
	// 1. RSI Score
	if len(market.Prices) >= 14 {
		rsi := indicators.RSI(market.Prices, 14)
		scores["rsi"] = indicators.RSIScore(rsi)
		scores["rsi_raw"] = rsi
	}
	
	// 2. Momentum Score
	if len(market.Prices) >= 10 {
		scores["momentum"] = indicators.MomentumScore(market.Prices, 10)
		scores["momentum_raw"] = indicators.Momentum(market.Prices, 10)
	}
	
	// 3. Volume Score
	if len(market.Volumes) >= 20 {
		currentVol := market.Volumes[len(market.Volumes)-1]
		avgVol := average(market.Volumes[:len(market.Volumes)-1])
		
		// Determine price direction for volume analysis
		priceDir := 0.0
		if len(market.Prices) >= 2 {
			priceDir = market.Prices[len(market.Prices)-1] - market.Prices[len(market.Prices)-2]
		}
		
		scores["volume"] = indicators.VolumeScore(currentVol, avgVol, priceDir)
		if avgVol > 0 {
			scores["volume_ratio"] = currentVol / avgVol
		}
	}
	
	// 4. Order Book Imbalance Score
	if market.OrderBook != nil {
		scores["orderbook"] = indicators.OrderBookImbalanceScore(
			market.OrderBook.BidVolume,
			market.OrderBook.AskVolume,
		)
		scores["orderbook_imbalance"] = market.OrderBook.Imbalance
	}
	
	// 5. Funding Rate Score
	scores["funding"] = indicators.FundingRateScore(market.FundingRate)
	scores["funding_raw"] = market.FundingRate
	
	// 6. Buy/Sell Ratio Score (derived from order book if available)
	if market.OrderBook != nil {
		scores["buysell"] = indicators.BuySellRatioScore(
			market.OrderBook.BidVolume,
			market.OrderBook.AskVolume,
		)
		totalVol := market.OrderBook.BidVolume + market.OrderBook.AskVolume
		if totalVol > 0 {
			scores["buysell_ratio"] = market.OrderBook.BidVolume / totalVol
		}
	}
	
	return scores
}

// calculateCompositeScore combines indicator scores using weights
func (s *Crypto15mStrategy) calculateCompositeScore(scores map[string]float64) float64 {
	var composite float64
	var totalWeight float64
	
	if score, ok := scores["rsi"]; ok {
		composite += score * s.weights.RSI
		totalWeight += s.weights.RSI
	}
	
	if score, ok := scores["momentum"]; ok {
		composite += score * s.weights.Momentum
		totalWeight += s.weights.Momentum
	}
	
	if score, ok := scores["volume"]; ok {
		composite += score * s.weights.Volume
		totalWeight += s.weights.Volume
	}
	
	if score, ok := scores["orderbook"]; ok {
		composite += score * s.weights.OrderBook
		totalWeight += s.weights.OrderBook
	}
	
	if score, ok := scores["funding"]; ok {
		composite += score * s.weights.FundingRate
		totalWeight += s.weights.FundingRate
	}
	
	if score, ok := scores["buysell"]; ok {
		composite += score * s.weights.BuySellRatio
		totalWeight += s.weights.BuySellRatio
	}
	
	// Normalize if we didn't use all indicators
	if totalWeight > 0 && totalWeight < 1.0 {
		composite = composite / totalWeight
	}
	
	return composite
}

// generateReasons creates human-readable explanations
func (s *Crypto15mStrategy) generateReasons(scores map[string]float64, composite float64) []string {
	var reasons []string
	
	// RSI reasoning
	if rsiRaw, ok := scores["rsi_raw"]; ok {
		if rsiRaw < 30 {
			reasons = append(reasons, "RSI oversold (bullish)")
		} else if rsiRaw > 70 {
			reasons = append(reasons, "RSI overbought (bearish)")
		}
	}
	
	// Momentum reasoning
	if mom, ok := scores["momentum"]; ok {
		if mom > 50 {
			reasons = append(reasons, "Strong upward momentum")
		} else if mom < -50 {
			reasons = append(reasons, "Strong downward momentum")
		}
	}
	
	// Order book reasoning
	if ob, ok := scores["orderbook"]; ok {
		if ob > 40 {
			reasons = append(reasons, "Heavy bid-side pressure")
		} else if ob < -40 {
			reasons = append(reasons, "Heavy ask-side pressure")
		}
	}
	
	// Funding reasoning
	if fr, ok := scores["funding_raw"]; ok {
		if fr > 0.01 {
			reasons = append(reasons, "High positive funding (crowded long)")
		} else if fr < -0.01 {
			reasons = append(reasons, "Negative funding (crowded short)")
		}
	}
	
	// Volume reasoning
	if vr, ok := scores["volume_ratio"]; ok {
		if vr > 1.5 {
			reasons = append(reasons, "Volume surge detected")
		}
	}
	
	return reasons
}

// Helper function
func average(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}
