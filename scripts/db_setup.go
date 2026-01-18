package main

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

func main() {
	godotenv.Load()

	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		fmt.Println("❌ DATABASE_URL not set")
		os.Exit(1)
	}

	fmt.Println("🔌 Connecting to database...")
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		fmt.Printf("❌ Connection error: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		fmt.Printf("❌ Ping error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✅ Database connected!")

	// List all tables
	fmt.Println("\n📋 Current tables:")
	rows, err := db.Query(`
		SELECT tablename FROM pg_tables 
		WHERE schemaname = 'public'
		ORDER BY tablename
	`)
	if err != nil {
		fmt.Printf("❌ Query error: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	tables := []string{}
	for rows.Next() {
		var table string
		rows.Scan(&table)
		tables = append(tables, table)
		fmt.Printf("  - %s\n", table)
	}

	if len(tables) == 0 {
		fmt.Println("  (no tables found)")
	}

	// Count rows in each table
	fmt.Println("\n📊 Row counts:")
	for _, table := range tables {
		var count int
		err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&count)
		if err == nil {
			fmt.Printf("  - %s: %d rows\n", table, count)
		}
	}

	// Clean ALL tables
	fmt.Println("\n🧹 CLEANING ALL TABLES...")
	for _, table := range tables {
		_, err := db.Exec(fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", table))
		if err != nil {
			fmt.Printf("  ⚠️ Failed to drop %s: %v\n", table, err)
		} else {
			fmt.Printf("  ✅ Dropped %s\n", table)
		}
	}

	// Create v6.0 schema
	fmt.Println("\n📝 Creating v6.0 schema...")
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

	CREATE TABLE IF NOT EXISTS backtests (
		id SERIAL PRIMARY KEY,
		strategy TEXT NOT NULL,
		start_date TIMESTAMP NOT NULL,
		end_date TIMESTAMP NOT NULL,
		initial_equity NUMERIC(18,8) NOT NULL,
		final_equity NUMERIC(18,8) NOT NULL,
		total_trades INT DEFAULT 0,
		wins INT DEFAULT 0,
		losses INT DEFAULT 0,
		max_drawdown NUMERIC(18,8),
		sharpe_ratio NUMERIC(10,4),
		created_at TIMESTAMP DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_trades_created ON trades(created_at);
	CREATE INDEX IF NOT EXISTS idx_positions_status ON positions(status);
	CREATE INDEX IF NOT EXISTS idx_daily_stats_date ON daily_stats(date);
	`

	_, err = db.Exec(schema)
	if err != nil {
		fmt.Printf("❌ Schema creation error: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✅ v6.0 schema created!")

	// Test insert
	fmt.Println("\n🧪 Testing INSERT...")
	testID := fmt.Sprintf("TEST_%d", time.Now().UnixNano())
	_, err = db.Exec(`
		INSERT INTO trades (id, asset, side, price, size, action, strategy)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, testID, "BTC_15m_UP", "YES", 0.90, 10.0, "OPEN", "Breakout15M")
	if err != nil {
		fmt.Printf("❌ Insert error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ Inserted test trade: %s\n", testID)

	// Test select
	fmt.Println("\n🧪 Testing SELECT...")
	var id, asset, side, action, strategy string
	var price, size float64
	err = db.QueryRow(`SELECT id, asset, side, price, size, action, strategy FROM trades WHERE id = $1`, testID).
		Scan(&id, &asset, &side, &price, &size, &action, &strategy)
	if err != nil {
		fmt.Printf("❌ Select error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ Retrieved: %s | %s | %s | $%.2f | %.2f shares | %s | %s\n",
		id, asset, side, price, size, action, strategy)

	// Clean test data
	fmt.Println("\n🧹 Cleaning test data...")
	_, err = db.Exec("DELETE FROM trades WHERE id = $1", testID)
	if err != nil {
		fmt.Printf("⚠️ Delete error: %v\n", err)
	} else {
		fmt.Println("✅ Test data cleaned!")
	}

	// Final verification
	fmt.Println("\n📊 Final table counts:")
	for _, table := range []string{"trades", "positions", "daily_stats", "backtests"} {
		var count int
		err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&count)
		if err == nil {
			fmt.Printf("  - %s: %d rows\n", table, count)
		}
	}

	fmt.Println("\n✅ DATABASE READY FOR BACKTESTING!")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Tables created:")
	fmt.Println("  • trades      - Record all trade executions")
	fmt.Println("  • positions   - Track open/closed positions")
	fmt.Println("  • daily_stats - Daily P&L tracking")
	fmt.Println("  • backtests   - Store backtest results")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
}
