package strategy

import (
	"github.com/shopspring/decimal"
	"github.com/web3guy0/polybot/feeds"
)

// ═══════════════════════════════════════════════════════════════════════════════
// STRATEGY INTERFACE - Plug-in pattern for strategies
// ═══════════════════════════════════════════════════════════════════════════════
//
// All strategies implement this interface:
//   OnTick(Tick) *Signal
//
// Engine calls OnTick for each market update, strategy returns nil or Signal
//
// ═══════════════════════════════════════════════════════════════════════════════

// Strategy is the interface all trading strategies must implement
type Strategy interface {
	// Name returns the strategy identifier
	Name() string

	// OnTick processes a price tick and returns a signal (or nil)
	OnTick(tick feeds.Tick) *Signal

	// Enabled returns whether strategy is active
	Enabled() bool

	// Config returns strategy configuration
	Config() map[string]interface{}
}

// Signal represents a trade signal from a strategy
type Signal struct {
	Market     string          // Market/condition ID
	Asset      string          // Token ID (YES or NO)
	TokenID    string          // Full token ID for API
	Side       string          // "YES" or "NO"
	Direction  string          // "LONG" or "SHORT"
	Entry      decimal.Decimal // Entry price
	TakeProfit decimal.Decimal // Take profit price
	StopLoss   decimal.Decimal // Stop loss price
	Confidence decimal.Decimal // 0-1 confidence score
	Reason     string          // Human-readable reason
	Strategy   string          // Source strategy name
}

// ═══════════════════════════════════════════════════════════════════════════════
// SIGNAL BUILDER - Helper for creating signals
// ═══════════════════════════════════════════════════════════════════════════════

// SignalBuilder helps construct signals with validation
type SignalBuilder struct {
	signal *Signal
}

// NewSignal creates a new signal builder
func NewSignal() *SignalBuilder {
	return &SignalBuilder{
		signal: &Signal{
			Direction:  "LONG",
			Confidence: decimal.NewFromFloat(0.5),
		},
	}
}

// Market sets the market ID
func (sb *SignalBuilder) Market(market string) *SignalBuilder {
	sb.signal.Market = market
	return sb
}

// Asset sets the asset/token ID
func (sb *SignalBuilder) Asset(asset string) *SignalBuilder {
	sb.signal.Asset = asset
	return sb
}

// TokenID sets the full token ID
func (sb *SignalBuilder) TokenID(tokenID string) *SignalBuilder {
	sb.signal.TokenID = tokenID
	return sb
}

// Side sets YES or NO
func (sb *SignalBuilder) Side(side string) *SignalBuilder {
	sb.signal.Side = side
	return sb
}

// Entry sets the entry price
func (sb *SignalBuilder) Entry(price decimal.Decimal) *SignalBuilder {
	sb.signal.Entry = price
	return sb
}

// TakeProfit sets the TP price
func (sb *SignalBuilder) TakeProfit(price decimal.Decimal) *SignalBuilder {
	sb.signal.TakeProfit = price
	return sb
}

// StopLoss sets the SL price
func (sb *SignalBuilder) StopLoss(price decimal.Decimal) *SignalBuilder {
	sb.signal.StopLoss = price
	return sb
}

// Confidence sets the confidence level (0-1)
func (sb *SignalBuilder) Confidence(conf decimal.Decimal) *SignalBuilder {
	sb.signal.Confidence = conf
	return sb
}

// Reason sets the signal reason
func (sb *SignalBuilder) Reason(reason string) *SignalBuilder {
	sb.signal.Reason = reason
	return sb
}

// Strategy sets the source strategy name
func (sb *SignalBuilder) Strategy(name string) *SignalBuilder {
	sb.signal.Strategy = name
	return sb
}

// Build returns the completed signal
func (sb *SignalBuilder) Build() *Signal {
	return sb.signal
}

// ═══════════════════════════════════════════════════════════════════════════════
// VALIDATION HELPERS
// ═══════════════════════════════════════════════════════════════════════════════

// Validate checks if a signal is well-formed
func (s *Signal) Validate() bool {
	if s.Market == "" || s.Asset == "" {
		return false
	}
	if s.Entry.IsZero() {
		return false
	}
	if s.TakeProfit.IsZero() || s.StopLoss.IsZero() {
		return false
	}
	// TP should be above entry for long, SL below
	if s.Direction == "LONG" {
		if s.TakeProfit.LessThanOrEqual(s.Entry) {
			return false
		}
		if s.StopLoss.GreaterThanOrEqual(s.Entry) {
			return false
		}
	}
	return true
}

// RiskReward calculates risk:reward ratio
func (s *Signal) RiskReward() decimal.Decimal {
	risk := s.Entry.Sub(s.StopLoss).Abs()
	reward := s.TakeProfit.Sub(s.Entry).Abs()
	if risk.IsZero() {
		return decimal.Zero
	}
	return reward.Div(risk)
}
