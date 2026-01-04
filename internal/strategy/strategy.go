package strategy

import (
	"context"
	"time"
)

// Signal represents a trading signal from a strategy
// This is the ONLY output format for all trading decisions
type Signal struct {
	Direction   Direction // UP, DOWN, or NO_TRADE (explicit!)
	Confidence  float64   // 0.0 to 1.0 (REQUIRED for risk sizing)
	Score       float64   // Raw score from strategy (-100 to +100)
	Strength    Strength  // WEAK, MODERATE, STRONG
	Reason      string    // Human-readable reason (REQUIRED for logging/debugging)
	Reasons     []string  // Additional supporting reasons
	Indicators  map[string]float64 // Individual indicator values
	GeneratedAt time.Time
	ExpiresAt   time.Time // When this signal becomes stale
}

// Direction represents the predicted price direction
// NO_TRADE is explicit - it's a decision, not absence of one
type Direction string

const (
	DirectionUp      Direction = "UP"
	DirectionDown    Direction = "DOWN"
	DirectionNoTrade Direction = "NO_TRADE" // Explicit skip decision
)

// Strength represents signal strength
type Strength string

const (
	StrengthWeak     Strength = "WEAK"
	StrengthModerate Strength = "MODERATE"
	StrengthStrong   Strength = "STRONG"
)

// MarketContext contains all data a strategy needs to make a decision
type MarketContext struct {
	Asset       string
	Timeframe   time.Duration
	CurrentPrice float64
	Prices      []float64          // Historical prices (newest last)
	Volumes     []float64          // Historical volumes
	Timestamps  []time.Time        // Timestamps for prices
	OrderBook   *OrderBookData     // Current order book
	FundingRate float64            // Current funding rate
	PriceChange24h float64         // 24h price change percentage
	
	// Market-specific data
	MarketID    string             // Polymarket market ID
	YesPrice    float64            // Current YES price
	NoPrice     float64            // Current NO price
	Liquidity   float64            // Available liquidity
	
	// Timing
	WindowStart time.Time          // When the prediction window starts
	WindowEnd   time.Time          // When the prediction window ends
	
	// Window identification (for one-position-per-window enforcement)
	WindowID    string             // Unique ID for this prediction window
}

// OrderBookData represents order book information
type OrderBookData struct {
	BidVolume   float64
	AskVolume   float64
	BidPrice    float64
	AskPrice    float64
	Spread      float64
	Imbalance   float64 // Positive = more bids, negative = more asks
}

// Strategy is the core interface that all trading strategies must implement
type Strategy interface {
	// Name returns a unique identifier for this strategy
	Name() string
	
	// Description returns a human-readable description
	Description() string
	
	// Asset returns the asset this strategy trades (e.g., "BTC", "ETH")
	Asset() string
	
	// Timeframe returns the prediction timeframe (e.g., 15m, 1h)
	Timeframe() time.Duration
	
	// Evaluate analyzes market context and returns a trading signal
	// This is the ONLY place where trading decisions are made
	Evaluate(ctx context.Context, market MarketContext) (Signal, error)
	
	// MinConfidence returns the minimum confidence required to trade
	MinConfidence() float64
	
	// Warmup returns the number of data points needed before strategy can evaluate
	Warmup() int
}

// BaseStrategy provides common functionality for strategies
type BaseStrategy struct {
	name          string
	description   string
	asset         string
	timeframe     time.Duration
	minConfidence float64
	warmupPeriods int
}

func NewBaseStrategy(name, description, asset string, timeframe time.Duration, minConfidence float64, warmup int) BaseStrategy {
	return BaseStrategy{
		name:          name,
		description:   description,
		asset:         asset,
		timeframe:     timeframe,
		minConfidence: minConfidence,
		warmupPeriods: warmup,
	}
}

func (b BaseStrategy) Name() string           { return b.name }
func (b BaseStrategy) Description() string    { return b.description }
func (b BaseStrategy) Asset() string          { return b.asset }
func (b BaseStrategy) Timeframe() time.Duration { return b.timeframe }
func (b BaseStrategy) MinConfidence() float64 { return b.minConfidence }
func (b BaseStrategy) Warmup() int            { return b.warmupPeriods }

// Helper functions

// CalculateStrength converts a score to a strength level
func CalculateStrength(absScore float64) Strength {
	switch {
	case absScore >= 70:
		return StrengthStrong
	case absScore >= 40:
		return StrengthModerate
	default:
		return StrengthWeak
	}
}

// CalculateConfidence converts a score to confidence (0-1)
func CalculateConfidence(absScore float64) float64 {
	// Map 0-100 score to 0.5-1.0 confidence
	// Score of 0 = 50% confident (coin flip)
	// Score of 100 = 100% confident
	return 0.5 + (absScore / 200.0)
}

// IsExpired checks if a signal has expired
func (s Signal) IsExpired() bool {
	return time.Now().After(s.ExpiresAt)
}

// IsTradeable returns true if the signal meets minimum requirements
func (s Signal) IsTradeable(minConfidence float64) bool {
	return s.Direction != DirectionNoTrade && 
		   s.Confidence >= minConfidence && 
		   !s.IsExpired()
}

// IsNoTrade returns true if this is an explicit no-trade decision
func (s Signal) IsNoTrade() bool {
	return s.Direction == DirectionNoTrade
}

// String returns a human-readable representation
func (s Signal) String() string {
	return s.Reason
}
