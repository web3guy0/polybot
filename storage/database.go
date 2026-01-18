package storage

import (
	"database/sql"
	"os"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	_ "github.com/lib/pq"
)

// ═══════════════════════════════════════════════════════════════════════════════
// DATABASE - Trade persistence layer
// ═══════════════════════════════════════════════════════════════════════════════

type Database struct {
	db      *sql.DB
	enabled bool
}

// Trade represents a trade record
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

// NewDatabase creates a new database connection
func NewDatabase() (*Database, error) {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		log.Warn().Msg("DATABASE_URL not set, running without persistence")
		return &Database{enabled: false}, nil
	}

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, err
	}

	if err := db.Ping(); err != nil {
		return nil, err
	}

	database := &Database{db: db, enabled: true}

	// Create tables if not exist
	if err := database.migrate(); err != nil {
		return nil, err
	}

	log.Info().Msg("💾 Database connected")
	return database, nil
}

// migrate creates necessary tables
func (d *Database) migrate() error {
	if !d.enabled {
		return nil
	}

	schema := `
	CREATE TABLE IF NOT EXISTS trades (
		id TEXT PRIMARY KEY,
		asset TEXT NOT NULL,
		side TEXT NOT NULL,
		price NUMERIC(18,8) NOT NULL,
		size NUMERIC(18,8) NOT NULL,
		action TEXT NOT NULL,
		strategy TEXT NOT NULL,
		pnl NUMERIC(18,8) DEFAULT 0,
		created_at TIMESTAMP DEFAULT NOW()
	);

	CREATE TABLE IF NOT EXISTS positions (
		id TEXT PRIMARY KEY,
		market TEXT NOT NULL,
		asset TEXT NOT NULL,
		side TEXT NOT NULL,
		token_id TEXT NOT NULL,
		entry_price NUMERIC(18,8) NOT NULL,
		size NUMERIC(18,8) NOT NULL,
		stop_loss NUMERIC(18,8) NOT NULL,
		take_profit NUMERIC(18,8) NOT NULL,
		strategy TEXT NOT NULL,
		opened_at TIMESTAMP DEFAULT NOW(),
		closed_at TIMESTAMP,
		exit_price NUMERIC(18,8),
		pnl NUMERIC(18,8),
		status TEXT DEFAULT 'OPEN'
	);

	CREATE TABLE IF NOT EXISTS daily_stats (
		date DATE PRIMARY KEY,
		trades INT DEFAULT 0,
		wins INT DEFAULT 0,
		losses INT DEFAULT 0,
		pnl NUMERIC(18,8) DEFAULT 0,
		equity NUMERIC(18,8) DEFAULT 0
	);

	CREATE INDEX IF NOT EXISTS idx_trades_created ON trades(created_at);
	CREATE INDEX IF NOT EXISTS idx_positions_status ON positions(status);
	`

	_, err := d.db.Exec(schema)
	return err
}

// LogTrade records a trade action
func (d *Database) LogTrade(id, asset, side string, price, size decimal.Decimal, action, strategy string) error {
	if !d.enabled {
		return nil
	}

	_, err := d.db.Exec(`
		INSERT INTO trades (id, asset, side, price, size, action, strategy)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, id, asset, side, price, size, action, strategy)

	if err != nil {
		log.Error().Err(err).Msg("Failed to log trade")
	}

	return err
}

// OpenPosition records a new position
func (d *Database) OpenPosition(id, market, asset, side, tokenID string, entryPrice, size, stopLoss, takeProfit decimal.Decimal, strategy string) error {
	if !d.enabled {
		return nil
	}

	_, err := d.db.Exec(`
		INSERT INTO positions (id, market, asset, side, token_id, entry_price, size, stop_loss, take_profit, strategy)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, id, market, asset, side, tokenID, entryPrice, size, stopLoss, takeProfit, strategy)

	return err
}

// ClosePosition updates position as closed
func (d *Database) ClosePosition(id string, exitPrice, pnl decimal.Decimal) error {
	if !d.enabled {
		return nil
	}

	_, err := d.db.Exec(`
		UPDATE positions 
		SET status = 'CLOSED', closed_at = NOW(), exit_price = $2, pnl = $3
		WHERE id = $1
	`, id, exitPrice, pnl)

	return err
}

// GetOpenPositions returns all open positions
func (d *Database) GetOpenPositions() ([]map[string]interface{}, error) {
	if !d.enabled {
		return nil, nil
	}

	rows, err := d.db.Query(`
		SELECT id, market, asset, side, token_id, entry_price, size, stop_loss, take_profit, strategy, opened_at
		FROM positions WHERE status = 'OPEN'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var positions []map[string]interface{}
	for rows.Next() {
		var id, market, asset, side, tokenID, strategy string
		var entryPrice, size, stopLoss, takeProfit decimal.Decimal
		var openedAt time.Time

		if err := rows.Scan(&id, &market, &asset, &side, &tokenID, &entryPrice, &size, &stopLoss, &takeProfit, &strategy, &openedAt); err != nil {
			continue
		}

		positions = append(positions, map[string]interface{}{
			"id":          id,
			"market":      market,
			"asset":       asset,
			"side":        side,
			"token_id":    tokenID,
			"entry_price": entryPrice,
			"size":        size,
			"stop_loss":   stopLoss,
			"take_profit": takeProfit,
			"strategy":    strategy,
			"opened_at":   openedAt,
		})
	}

	return positions, nil
}

// UpdateDailyStats updates or inserts daily stats
func (d *Database) UpdateDailyStats(trades, wins, losses int, pnl, equity decimal.Decimal) error {
	if !d.enabled {
		return nil
	}

	today := time.Now().Format("2006-01-02")

	_, err := d.db.Exec(`
		INSERT INTO daily_stats (date, trades, wins, losses, pnl, equity)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (date) DO UPDATE SET
			trades = daily_stats.trades + $2,
			wins = daily_stats.wins + $3,
			losses = daily_stats.losses + $4,
			pnl = daily_stats.pnl + $5,
			equity = $6
	`, today, trades, wins, losses, pnl, equity)

	return err
}

// GetRecentTrades returns recent trade history
func (d *Database) GetRecentTrades(limit int) ([]Trade, error) {
	if !d.enabled {
		return nil, nil
	}

	rows, err := d.db.Query(`
		SELECT id, asset, side, price, size, action, strategy, pnl, created_at
		FROM trades ORDER BY created_at DESC LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var trades []Trade
	for rows.Next() {
		var t Trade
		if err := rows.Scan(&t.ID, &t.Asset, &t.Side, &t.Price, &t.Size, &t.Action, &t.Strategy, &t.PnL, &t.Timestamp); err != nil {
			continue
		}
		trades = append(trades, t)
	}

	return trades, nil
}

// Close closes the database connection
func (d *Database) Close() {
	if d.db != nil {
		d.db.Close()
	}
}

// IsEnabled returns if database is enabled
func (d *Database) IsEnabled() bool {
	return d.enabled
}
