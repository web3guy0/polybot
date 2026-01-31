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

	CREATE TABLE IF NOT EXISTS window_snapshots (
		id SERIAL PRIMARY KEY,
		market_id TEXT NOT NULL,
		asset TEXT NOT NULL,
		price_to_beat NUMERIC(18,8) NOT NULL,
		binance_start_price NUMERIC(18,8) NOT NULL,
		binance_end_price NUMERIC(18,8),
		yes_price NUMERIC(18,8),
		no_price NUMERIC(18,8),
		window_end TIMESTAMP NOT NULL,
		created_at TIMESTAMP DEFAULT NOW(),
		resolved_at TIMESTAMP,
		outcome TEXT,
		UNIQUE(market_id, created_at)
	);

	CREATE TABLE IF NOT EXISTS execution_positions (
		id TEXT PRIMARY KEY,
		market_id TEXT NOT NULL,
		token_id TEXT NOT NULL,
		asset TEXT NOT NULL,
		side TEXT NOT NULL,
		size NUMERIC(18,8) NOT NULL,
		avg_entry NUMERIC(18,8) NOT NULL,
		strategy TEXT NOT NULL,
		opened_at TIMESTAMP DEFAULT NOW(),
		metadata TEXT DEFAULT '{}'
	);

	CREATE TABLE IF NOT EXISTS risk_state (
		date DATE PRIMARY KEY,
		balance NUMERIC(18,8) NOT NULL,
		daily_pnl NUMERIC(18,8) DEFAULT 0,
		consecutive_losses INT DEFAULT 0,
		circuit_tripped BOOLEAN DEFAULT FALSE,
		disabled_assets TEXT DEFAULT '[]'
	);

	CREATE INDEX IF NOT EXISTS idx_trades_created ON trades(created_at);
	CREATE INDEX IF NOT EXISTS idx_positions_status ON positions(status);
	CREATE INDEX IF NOT EXISTS idx_snapshots_market ON window_snapshots(market_id);
	CREATE INDEX IF NOT EXISTS idx_snapshots_created ON window_snapshots(created_at);
	CREATE INDEX IF NOT EXISTS idx_exec_positions_asset ON execution_positions(asset);
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

// ═══════════════════════════════════════════════════════════════════════════════
// WINDOW SNAPSHOTS - Price tracking for each 15-min window
// ═══════════════════════════════════════════════════════════════════════════════

// WindowSnapshot stores the Binance price when a window is first detected
type WindowSnapshot struct {
	ID               int64
	MarketID         string
	Asset            string
	PriceToBeat      decimal.Decimal
	BinanceStartPrice decimal.Decimal
	BinanceEndPrice   decimal.Decimal
	YesPrice          decimal.Decimal
	NoPrice           decimal.Decimal
	WindowEnd         time.Time
	CreatedAt         time.Time
	ResolvedAt        *time.Time
	Outcome           string
}

// SaveWindowSnapshot records a new window with its start price
func (d *Database) SaveWindowSnapshot(marketID, asset string, priceToBeat, binancePrice, yesPrice, noPrice decimal.Decimal, windowEnd time.Time) error {
	if !d.enabled {
		return nil
	}

	_, err := d.db.Exec(`
		INSERT INTO window_snapshots (market_id, asset, price_to_beat, binance_start_price, yes_price, no_price, window_end)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (market_id, created_at) DO NOTHING
	`, marketID, asset, priceToBeat, binancePrice, yesPrice, noPrice, windowEnd)

	return err
}

// UpdateWindowOutcome updates a window with final price and outcome
func (d *Database) UpdateWindowOutcome(marketID string, binanceEndPrice decimal.Decimal, outcome string) error {
	if !d.enabled {
		return nil
	}

	_, err := d.db.Exec(`
		UPDATE window_snapshots 
		SET binance_end_price = $2, outcome = $3, resolved_at = NOW()
		WHERE market_id = $1 AND resolved_at IS NULL
	`, marketID, binanceEndPrice, outcome)

	return err
}

// GetRecentSnapshots returns recent window snapshots for analysis
func (d *Database) GetRecentSnapshots(limit int) ([]WindowSnapshot, error) {
	if !d.enabled {
		return nil, nil
	}

	rows, err := d.db.Query(`
		SELECT id, market_id, asset, price_to_beat, binance_start_price, 
		       COALESCE(binance_end_price, 0), yes_price, no_price, window_end, created_at,
		       resolved_at, COALESCE(outcome, '')
		FROM window_snapshots ORDER BY created_at DESC LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snapshots []WindowSnapshot
	for rows.Next() {
		var s WindowSnapshot
		var resolvedAt sql.NullTime
		if err := rows.Scan(&s.ID, &s.MarketID, &s.Asset, &s.PriceToBeat, &s.BinanceStartPrice,
			&s.BinanceEndPrice, &s.YesPrice, &s.NoPrice, &s.WindowEnd, &s.CreatedAt,
			&resolvedAt, &s.Outcome); err != nil {
			continue
		}
		if resolvedAt.Valid {
			s.ResolvedAt = &resolvedAt.Time
		}
		snapshots = append(snapshots, s)
	}

	return snapshots, nil
}

// GetWindowStartPrice retrieves the stored start price for a market
func (d *Database) GetWindowStartPrice(marketID string) (decimal.Decimal, bool) {
	if !d.enabled {
		return decimal.Zero, false
	}

	var startPrice decimal.Decimal
	err := d.db.QueryRow(`
		SELECT binance_start_price FROM window_snapshots 
		WHERE market_id = $1 
		ORDER BY created_at DESC LIMIT 1
	`, marketID).Scan(&startPrice)

	if err != nil {
		return decimal.Zero, false
	}

	return startPrice, true
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

// ═══════════════════════════════════════════════════════════════════════════════
// EXECUTION POSITIONS - Persist active positions for crash recovery
// ═══════════════════════════════════════════════════════════════════════════════

// ExecutionPosition represents a persisted position for the execution layer
type ExecutionPosition struct {
	ID         string
	MarketID   string
	TokenID    string
	Asset      string
	Side       string
	Size       decimal.Decimal
	AvgEntry   decimal.Decimal
	Strategy   string
	OpenedAt   time.Time
	Metadata   string // JSON metadata
}

// SaveExecutionPosition persists an open position
func (d *Database) SaveExecutionPosition(pos *ExecutionPosition) error {
	if !d.enabled {
		return nil
	}

	_, err := d.db.Exec(`
		INSERT INTO execution_positions (id, market_id, token_id, asset, side, size, avg_entry, strategy, opened_at, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (id) DO UPDATE SET
			size = $6,
			avg_entry = $7,
			metadata = $10
	`, pos.ID, pos.MarketID, pos.TokenID, pos.Asset, pos.Side, pos.Size, pos.AvgEntry, pos.Strategy, pos.OpenedAt, pos.Metadata)

	if err != nil {
		log.Error().Err(err).Str("id", pos.ID).Msg("Failed to save execution position")
	}

	return err
}

// DeleteExecutionPosition removes a closed position
func (d *Database) DeleteExecutionPosition(id string) error {
	if !d.enabled {
		return nil
	}

	_, err := d.db.Exec(`DELETE FROM execution_positions WHERE id = $1`, id)
	return err
}

// GetAllExecutionPositions retrieves all open positions for recovery
func (d *Database) GetAllExecutionPositions() ([]*ExecutionPosition, error) {
	if !d.enabled {
		return nil, nil
	}

	rows, err := d.db.Query(`
		SELECT id, market_id, token_id, asset, side, size, avg_entry, strategy, opened_at, COALESCE(metadata, '{}')
		FROM execution_positions
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var positions []*ExecutionPosition
	for rows.Next() {
		pos := &ExecutionPosition{}
		err := rows.Scan(&pos.ID, &pos.MarketID, &pos.TokenID, &pos.Asset, &pos.Side, &pos.Size, &pos.AvgEntry, &pos.Strategy, &pos.OpenedAt, &pos.Metadata)
		if err != nil {
			log.Warn().Err(err).Msg("Failed to scan execution position")
			continue
		}
		positions = append(positions, pos)
	}

	return positions, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// RISK STATE - Persist risk gate state for crash recovery
// ═══════════════════════════════════════════════════════════════════════════════

// RiskState represents persisted risk state
type RiskState struct {
	Date              string
	Balance           decimal.Decimal
	DailyPnL          decimal.Decimal
	ConsecutiveLosses int
	CircuitTripped    bool
	DisabledAssets    string // JSON array
}

// SaveRiskState persists the current risk state
func (d *Database) SaveRiskState(state *RiskState) error {
	if !d.enabled {
		return nil
	}

	_, err := d.db.Exec(`
		INSERT INTO risk_state (date, balance, daily_pnl, consecutive_losses, circuit_tripped, disabled_assets)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (date) DO UPDATE SET
			balance = $2,
			daily_pnl = $3,
			consecutive_losses = $4,
			circuit_tripped = $5,
			disabled_assets = $6
	`, state.Date, state.Balance, state.DailyPnL, state.ConsecutiveLosses, state.CircuitTripped, state.DisabledAssets)

	return err
}

// GetRiskState retrieves the latest risk state
func (d *Database) GetRiskState(date string) (*RiskState, error) {
	if !d.enabled {
		return nil, nil
	}

	state := &RiskState{}
	err := d.db.QueryRow(`
		SELECT date, balance, daily_pnl, consecutive_losses, circuit_tripped, COALESCE(disabled_assets, '[]')
		FROM risk_state WHERE date = $1
	`, date).Scan(&state.Date, &state.Balance, &state.DailyPnL, &state.ConsecutiveLosses, &state.CircuitTripped, &state.DisabledAssets)

	if err != nil {
		return nil, err
	}

	return state, nil
}
