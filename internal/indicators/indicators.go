package indicators

import (
	"math"

	"github.com/shopspring/decimal"
)

// RSI calculates Relative Strength Index
func RSI(prices []float64, period int) float64 {
	if len(prices) < period+1 {
		return 50 // Neutral if not enough data
	}

	gains := make([]float64, 0)
	losses := make([]float64, 0)

	for i := 1; i < len(prices); i++ {
		change := prices[i] - prices[i-1]
		if change > 0 {
			gains = append(gains, change)
			losses = append(losses, 0)
		} else {
			gains = append(gains, 0)
			losses = append(losses, -change)
		}
	}

	if len(gains) < period {
		return 50
	}

	// Calculate initial average gain/loss
	avgGain := average(gains[:period])
	avgLoss := average(losses[:period])

	// Smooth with remaining data
	for i := period; i < len(gains); i++ {
		avgGain = (avgGain*float64(period-1) + gains[i]) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + losses[i]) / float64(period)
	}

	if avgLoss == 0 {
		return 100
	}

	rs := avgGain / avgLoss
	rsi := 100 - (100 / (1 + rs))

	return rsi
}

// EMA calculates Exponential Moving Average
func EMA(prices []float64, period int) float64 {
	if len(prices) == 0 {
		return 0
	}
	if len(prices) < period {
		return average(prices)
	}

	multiplier := 2.0 / float64(period+1)
	ema := average(prices[:period])

	for i := period; i < len(prices); i++ {
		ema = (prices[i]-ema)*multiplier + ema
	}

	return ema
}

// SMA calculates Simple Moving Average
func SMA(prices []float64, period int) float64 {
	if len(prices) == 0 {
		return 0
	}
	if len(prices) < period {
		return average(prices)
	}

	return average(prices[len(prices)-period:])
}

// MACD calculates MACD line and signal line
func MACD(prices []float64, fastPeriod, slowPeriod, signalPeriod int) (float64, float64, float64) {
	if len(prices) < slowPeriod {
		return 0, 0, 0
	}

	fastEMA := EMA(prices, fastPeriod)
	slowEMA := EMA(prices, slowPeriod)
	macdLine := fastEMA - slowEMA

	// Calculate signal line (EMA of MACD)
	// For simplicity, we'll use the current MACD value
	// In production, you'd track MACD history
	signalLine := macdLine * 0.9 // Simplified

	histogram := macdLine - signalLine

	return macdLine, signalLine, histogram
}

// Momentum calculates price momentum over a period
func Momentum(prices []float64, period int) float64 {
	if len(prices) <= period {
		return 0
	}

	current := prices[len(prices)-1]
	previous := prices[len(prices)-1-period]

	if previous == 0 {
		return 0
	}

	return ((current - previous) / previous) * 100
}

// MomentumScore returns a normalized momentum score (-30 to +30)
func MomentumScore(prices []float64, period int) float64 {
	mom := Momentum(prices, period)
	
	// Normalize: ±1% momentum = ±30 score
	score := mom * 30
	
	// Clamp to range
	if score > 30 {
		score = 30
	} else if score < -30 {
		score = -30
	}
	
	return score
}

// RSIScore converts RSI to trading signal (-20 to +20)
func RSIScore(rsi float64) float64 {
	// RSI < 30: Oversold, bullish signal
	// RSI > 70: Overbought, bearish signal
	// RSI 40-60: Neutral
	
	if rsi < 30 {
		// Strong bullish: 0-30 RSI maps to +10 to +20
		return 10 + ((30-rsi)/30)*10
	} else if rsi < 40 {
		// Mild bullish: 30-40 RSI maps to 0 to +10
		return ((40 - rsi) / 10) * 10
	} else if rsi > 70 {
		// Strong bearish: 70-100 RSI maps to -10 to -20
		return -10 - ((rsi-70)/30)*10
	} else if rsi > 60 {
		// Mild bearish: 60-70 RSI maps to 0 to -10
		return -((rsi - 60) / 10) * 10
	}
	
	// Neutral zone
	return 0
}

// VolumeScore analyzes volume relative to average (-15 to +15)
func VolumeScore(currentVolume, avgVolume float64, priceDirection float64) float64 {
	if avgVolume == 0 {
		return 0
	}
	
	volumeRatio := currentVolume / avgVolume
	
	// High volume confirms trend, low volume suggests reversal
	if volumeRatio > 2.0 {
		// Very high volume - strong confirmation
		if priceDirection > 0 {
			return 15
		}
		return -15
	} else if volumeRatio > 1.5 {
		// High volume - moderate confirmation
		if priceDirection > 0 {
			return 10
		}
		return -10
	} else if volumeRatio < 0.5 {
		// Low volume - weak move, might reverse
		if priceDirection > 0 {
			return -5 // Price up on low volume = bearish
		}
		return 5 // Price down on low volume = bullish
	}
	
	return 0
}

// OrderBookImbalanceScore calculates order book imbalance signal (-20 to +20)
func OrderBookImbalanceScore(bidVolume, askVolume float64) float64 {
	if askVolume == 0 {
		return 20
	}
	if bidVolume == 0 {
		return -20
	}
	
	ratio := bidVolume / askVolume
	
	// Normalize: ratio 1.5 = +20, ratio 0.67 = -20
	if ratio > 1 {
		score := (ratio - 1) * 40
		if score > 20 {
			score = 20
		}
		return score
	} else {
		score := (1 - ratio) * 40
		if score > 20 {
			score = 20
		}
		return -score
	}
}

// FundingRateScore analyzes funding rate for contrarian signals (-15 to +15)
func FundingRateScore(fundingRate float64) float64 {
	// fundingRate is typically -0.001 to +0.001 (0.1%)
	// High positive = longs pay shorts = overleveraged longs = bearish
	// High negative = shorts pay longs = overleveraged shorts = bullish
	
	rate := fundingRate * 100 // Convert to percentage
	
	if rate > 0.05 {
		// High positive funding = bearish
		return -15
	} else if rate > 0.02 {
		return -10
	} else if rate < -0.05 {
		// High negative funding = bullish
		return 15
	} else if rate < -0.02 {
		return 10
	}
	
	return 0
}

// BuySellRatioScore analyzes taker buy/sell ratio (-15 to +15)
func BuySellRatioScore(buyVolume, sellVolume float64) float64 {
	if sellVolume == 0 {
		return 15
	}
	
	ratio := buyVolume / sellVolume
	
	if ratio > 1.5 {
		return 15 // Strong buying pressure
	} else if ratio > 1.2 {
		return 10
	} else if ratio > 1.1 {
		return 5
	} else if ratio < 0.67 {
		return -15 // Strong selling pressure
	} else if ratio < 0.83 {
		return -10
	} else if ratio < 0.9 {
		return -5
	}
	
	return 0
}

// Volatility calculates price volatility (standard deviation)
func Volatility(prices []float64) float64 {
	if len(prices) < 2 {
		return 0
	}
	
	avg := average(prices)
	sumSquares := 0.0
	
	for _, p := range prices {
		sumSquares += (p - avg) * (p - avg)
	}
	
	return math.Sqrt(sumSquares / float64(len(prices)))
}

// ATR calculates Average True Range
func ATR(highs, lows, closes []float64, period int) float64 {
	if len(highs) < period+1 || len(lows) < period+1 || len(closes) < period+1 {
		return 0
	}
	
	trs := make([]float64, 0)
	
	for i := 1; i < len(closes); i++ {
		tr := math.Max(
			highs[i]-lows[i],
			math.Max(
				math.Abs(highs[i]-closes[i-1]),
				math.Abs(lows[i]-closes[i-1]),
			),
		)
		trs = append(trs, tr)
	}
	
	return SMA(trs, period)
}

// BollingerBands calculates Bollinger Bands
func BollingerBands(prices []float64, period int, stdDev float64) (upper, middle, lower float64) {
	if len(prices) < period {
		return 0, 0, 0
	}
	
	middle = SMA(prices, period)
	
	// Calculate standard deviation
	recentPrices := prices[len(prices)-period:]
	volatility := Volatility(recentPrices)
	
	upper = middle + (volatility * stdDev)
	lower = middle - (volatility * stdDev)
	
	return upper, middle, lower
}

// StochRSI calculates Stochastic RSI
func StochRSI(prices []float64, rsiPeriod, stochPeriod int) float64 {
	if len(prices) < rsiPeriod+stochPeriod {
		return 50
	}
	
	// Calculate RSI values
	rsis := make([]float64, 0)
	for i := rsiPeriod; i <= len(prices); i++ {
		rsi := RSI(prices[:i], rsiPeriod)
		rsis = append(rsis, rsi)
	}
	
	if len(rsis) < stochPeriod {
		return 50
	}
	
	recent := rsis[len(rsis)-stochPeriod:]
	currentRSI := rsis[len(rsis)-1]
	
	minRSI := min(recent)
	maxRSI := max(recent)
	
	if maxRSI == minRSI {
		return 50
	}
	
	stochRSI := ((currentRSI - minRSI) / (maxRSI - minRSI)) * 100
	return stochRSI
}

// TrendStrength calculates how strong the current trend is (0-100)
func TrendStrength(prices []float64, period int) float64 {
	if len(prices) < period {
		return 0
	}
	
	// Count price increases vs decreases
	increases := 0
	decreases := 0
	recent := prices[len(prices)-period:]
	
	for i := 1; i < len(recent); i++ {
		if recent[i] > recent[i-1] {
			increases++
		} else if recent[i] < recent[i-1] {
			decreases++
		}
	}
	
	total := increases + decreases
	if total == 0 {
		return 0
	}
	
	// Return strength (0-100), with direction embedded
	// Positive = uptrend, Negative = downtrend
	if increases > decreases {
		return float64(increases) / float64(total) * 100
	}
	return -float64(decreases) / float64(total) * 100
}

// PricePosition calculates where current price is relative to range (-100 to +100)
func PricePosition(prices []float64, period int) float64 {
	if len(prices) < period {
		return 0
	}
	
	recent := prices[len(prices)-period:]
	current := prices[len(prices)-1]
	
	minPrice := min(recent)
	maxPrice := max(recent)
	
	if maxPrice == minPrice {
		return 0
	}
	
	// 0 = at bottom, 100 = at top
	position := ((current - minPrice) / (maxPrice - minPrice)) * 100
	
	// Convert to -100 to +100 scale centered at 50
	return (position - 50) * 2
}

// Helper functions

func average(data []float64) float64 {
	if len(data) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range data {
		sum += v
	}
	return sum / float64(len(data))
}

func min(data []float64) float64 {
	if len(data) == 0 {
		return 0
	}
	m := data[0]
	for _, v := range data[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func max(data []float64) float64 {
	if len(data) == 0 {
		return 0
	}
	m := data[0]
	for _, v := range data[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

// DecimalToFloat converts decimal to float64
func DecimalToFloat(d decimal.Decimal) float64 {
	f, _ := d.Float64()
	return f
}

// FloatToDecimal converts float64 to decimal
func FloatToDecimal(f float64) decimal.Decimal {
	return decimal.NewFromFloat(f)
}
