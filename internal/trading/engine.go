// Package trading provides trade execution functionality
//
// engine.go - Generic order execution engine for Polymarket
// Used for placing directional bets on prediction markets.
package trading

import (
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/internal/config"
	"github.com/web3guy0/polybot/internal/database"
	"github.com/web3guy0/polybot/internal/polymarket"
)

// Engine handles trade execution on Polymarket
type Engine struct {
	cfg     *config.Config
	db      *database.Database
	enabled bool
}

// TradeResult represents the outcome of a trade
type TradeResult struct {
	Success bool
	TxHash  string
	Amount  decimal.Decimal
	Price   decimal.Decimal
	Error   error
}

// NewEngine creates a new trading engine
func NewEngine(cfg *config.Config, db *database.Database) *Engine {
	return &Engine{
		cfg:     cfg,
		db:      db,
		enabled: cfg.TradingEnabled,
	}
}

// IsEnabled returns whether trading is enabled
func (e *Engine) IsEnabled() bool {
	return e.enabled && !e.cfg.DryRun
}

// Enable enables the trading engine
func (e *Engine) Enable() {
	e.enabled = true
	log.Info().Msg("🤖 Trading engine enabled")
}

// Disable disables the trading engine
func (e *Engine) Disable() {
	e.enabled = false
	log.Info().Msg("🛑 Trading engine disabled")
}

// PlaceOrder places a directional bet on a prediction market
// side: "YES" or "NO"
func (e *Engine) PlaceOrder(market polymarket.ParsedMarket, side string, amount, price decimal.Decimal) (*TradeResult, error) {
	if !e.enabled {
		return &TradeResult{Success: false, Error: nil}, nil
	}

	log.Info().
		Str("market", market.ID).
		Str("side", side).
		Str("amount", amount.String()).
		Str("price", price.String()).
		Msg("📝 Placing order")

	if e.cfg.DryRun {
		log.Info().
			Str("market", truncate(market.Question, 50)).
			Str("side", side).
			Str("amount", amount.String()).
			Msg("🧪 DRY RUN: Would place order")
		return &TradeResult{
			Success: true,
			Amount:  amount,
			Price:   price,
		}, nil
	}

	// TODO: Implement Polymarket CLOB order placement
	// This requires:
	// 1. API key authentication
	// 2. Signing with wallet private key
	// 3. Order submission to CLOB

	trade := &database.Trade{
		MarketID:  market.ID,
		Side:      side,
		Amount:    amount,
		Price:     price,
		Status:    "executed",
		CreatedAt: time.Now(),
	}
	e.db.SaveTrade(trade)

	return &TradeResult{
		Success: true,
		Amount:  amount,
		Price:   price,
	}, nil
}

// GetBalance returns the current wallet balance
func (e *Engine) GetBalance() (decimal.Decimal, error) {
	// TODO: Implement balance fetching from wallet
	return decimal.NewFromFloat(0), nil
}

// GetOpenPositions returns current open positions
func (e *Engine) GetOpenPositions() ([]database.Trade, error) {
	// TODO: Implement position tracking
	return nil, nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
