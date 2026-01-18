package types

import (
	"time"

	"github.com/shopspring/decimal"
)

// ═══════════════════════════════════════════════════════════════════════════════
// SHARED TYPES - Avoid import cycles
// ═══════════════════════════════════════════════════════════════════════════════

// Position represents an open trade
type Position struct {
	ID          string
	Market      string
	Asset       string
	Side        string // "YES" or "NO"
	TokenID     string
	EntryPrice  decimal.Decimal
	Size        decimal.Decimal
	EntryTime   time.Time
	StopLoss    decimal.Decimal
	TakeProfit  decimal.Decimal
	Strategy    string
	HighPrice   decimal.Decimal // For trailing stop
}

// Trade represents a historical trade
type Trade struct {
	ID        string
	Asset     string
	Side      string
	Price     decimal.Decimal
	Size      decimal.Decimal
	Action    string // OPEN, CLOSE, TAKE_PROFIT, STOP_LOSS
	Strategy  string
	PnL       decimal.Decimal
	Timestamp time.Time
}
