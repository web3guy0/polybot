package database

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type Database struct {
	db *gorm.DB
}

// Models

type Market struct {
	ID            string `gorm:"primaryKey"`
	Question      string
	Slug          string
	YesPrice      decimal.Decimal `gorm:"type:decimal(10,6)"`
	NoPrice       decimal.Decimal `gorm:"type:decimal(10,6)"`
	Volume        decimal.Decimal `gorm:"type:decimal(20,2)"`
	EndDate       time.Time
	Active        bool
	LastChecked   time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type Opportunity struct {
	ID          uint            `gorm:"primaryKey;autoIncrement"`
	MarketID    string          `gorm:"index"`
	Question    string
	YesPrice    decimal.Decimal `gorm:"type:decimal(10,6)"`
	NoPrice     decimal.Decimal `gorm:"type:decimal(10,6)"`
	TotalPrice  decimal.Decimal `gorm:"type:decimal(10,6)"`
	SpreadPct   decimal.Decimal `gorm:"type:decimal(10,4)"`
	Type        string
	AlertSent   bool
	TradedAt    *time.Time
	Profit      decimal.Decimal `gorm:"type:decimal(20,6)"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Trade struct {
	ID           uint            `gorm:"primaryKey;autoIncrement"`
	MarketID     string          `gorm:"index"`
	Side         string          // "YES" or "NO"
	Amount       decimal.Decimal `gorm:"type:decimal(20,6)"`
	Price        decimal.Decimal `gorm:"type:decimal(10,6)"`
	Status       string          // "pending", "executed", "failed"
	TxHash       string
	ProfitLoss   decimal.Decimal `gorm:"type:decimal(20,6)"`
	ErrorMessage string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ArbTrade represents an arbitrage trade for the latency arb strategy
type ArbTrade struct {
	ID             string          `gorm:"primaryKey"`
	Asset          string          `gorm:"index"` // BTC, ETH, SOL
	WindowID       string          `gorm:"index"`
	Question       string
	Direction      string          // "UP" or "DOWN"
	TokenID        string
	EntryPrice     decimal.Decimal `gorm:"type:decimal(10,6)"`
	ExitPrice      decimal.Decimal `gorm:"type:decimal(10,6)"`
	Amount         decimal.Decimal `gorm:"type:decimal(20,6)"`
	Shares         decimal.Decimal `gorm:"type:decimal(20,6)"`
	BTCAtEntry     decimal.Decimal `gorm:"type:decimal(20,6)"`
	BTCAtStart     decimal.Decimal `gorm:"type:decimal(20,6)"`
	PriceChangePct decimal.Decimal `gorm:"type:decimal(10,6)"`
	Edge           decimal.Decimal `gorm:"type:decimal(10,6)"`
	SizeMultiplier string          // "1x", "2x", "3x"
	Status         string          `gorm:"index"` // "open", "filled", "exited", "won", "lost"
	ExitType       string          // "quick_flip", "resolution", "stop_loss"
	Profit         decimal.Decimal `gorm:"type:decimal(20,6)"`
	EnteredAt      time.Time
	ExitedAt       *time.Time
	ResolvedAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Alert struct {
	ID         uint   `gorm:"primaryKey;autoIncrement"`
	MarketID   string `gorm:"index"`
	ChatID     int64
	MessageID  int
	Type       string
	SpreadPct  decimal.Decimal `gorm:"type:decimal(10,4)"`
	CreatedAt  time.Time
}

type UserSettings struct {
	ChatID          int64 `gorm:"primaryKey"`
	AlertsEnabled   bool  `gorm:"default:true"`
	MinSpreadPct    decimal.Decimal `gorm:"type:decimal(10,4);default:2.0"`
	TradingEnabled  bool  `gorm:"default:false"`
	MaxTradeSize    decimal.Decimal `gorm:"type:decimal(20,6);default:100"`
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ScalpTrade represents a scalping trade with ML features
type ScalpTrade struct {
	TradeID      string `gorm:"column:trade_id;primaryKey"`
	Asset        string `gorm:"column:asset;index"`
	WindowID     string `gorm:"column:window_id"`
	WindowTitle  string `gorm:"column:window_title"`
	Side         string `gorm:"column:side"`      // "UP" or "DOWN"
	TokenID      string `gorm:"column:token_id"`

	// Entry
	EntryPrice   decimal.Decimal `gorm:"column:entry_price;type:decimal(20,6)"`
	EntrySize    int64           `gorm:"column:entry_size"`
	EntryCost    decimal.Decimal `gorm:"column:entry_cost;type:decimal(20,6)"`
	EntryTime    time.Time       `gorm:"column:entry_time"`
	EntryOrderID string          `gorm:"column:entry_order_id"`

	// Exit
	ExitPrice    decimal.Decimal `gorm:"column:exit_price;type:decimal(20,6)"`
	ExitSize     int64           `gorm:"column:exit_size"`
	ExitValue    decimal.Decimal `gorm:"column:exit_value;type:decimal(20,6)"`
	ExitTime     *time.Time      `gorm:"column:exit_time"`
	ExitOrderID  string          `gorm:"column:exit_order_id"`
	ExitType     string          `gorm:"column:exit_type"` // profit_target, early_exit, stop_loss, timeout

	// P&L
	ProfitLoss   decimal.Decimal `gorm:"column:profit_loss;type:decimal(20,6)"`
	ProfitPct    decimal.Decimal `gorm:"column:profit_pct;type:decimal(10,4)"`

	// ML Features at Entry
	MLProbability    decimal.Decimal `gorm:"column:ml_probability;type:decimal(10,4)"`
	MLEntryThreshold decimal.Decimal `gorm:"column:ml_entry_threshold;type:decimal(10,4)"`
	MLProfitTarget   decimal.Decimal `gorm:"column:ml_profit_target;type:decimal(10,4)"`
	MLStopLoss       decimal.Decimal `gorm:"column:ml_stop_loss;type:decimal(10,4)"`
	Volatility15m    decimal.Decimal `gorm:"column:volatility_15m;type:decimal(10,6)"`
	Momentum1m       decimal.Decimal `gorm:"column:momentum_1m;type:decimal(10,6)"`
	Momentum5m       decimal.Decimal `gorm:"column:momentum_5m;type:decimal(10,6)"`
	PriceAtEntry     decimal.Decimal `gorm:"column:price_at_entry;type:decimal(20,6)"`
	TimeRemainingMin int             `gorm:"column:time_remaining_min"`

	// Status
	Status    string    `gorm:"column:status;index"` // OPEN, CLOSED, EXPIRED
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (ScalpTrade) TableName() string {
	return "scalp_trades"
}

// WindowPrice stores the captured "Price to Beat" for each prediction window
// This is captured at exact window start time and stored for later lookup
type WindowPrice struct {
	ID           uint            `gorm:"primaryKey;autoIncrement"`
	WindowSlug   string          `gorm:"uniqueIndex"` // e.g., "btc-updown-15m-1768040100"
	Asset        string          `gorm:"index"`       // BTC, ETH, SOL
	PriceToBeat  decimal.Decimal `gorm:"type:decimal(20,6)"`
	Source       string          // "chainlink", "binance", "cmc"
	WindowStart  time.Time       `gorm:"index"`
	CapturedAt   time.Time
	CreatedAt    time.Time
}

func (WindowPrice) TableName() string {
	return "window_prices"
}

func New(dbPath string) (*Database, error) {
	var db *gorm.DB
	var err error

	// Check if this is a PostgreSQL connection string
	if strings.HasPrefix(dbPath, "postgres://") || strings.HasPrefix(dbPath, "postgresql://") {
		// PostgreSQL connection
		db, err = gorm.Open(postgres.Open(dbPath), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Silent),
		})
		if err != nil {
			return nil, err
		}
		log.Info().Msg("Database connected (PostgreSQL)")
	} else {
		// SQLite fallback
		dir := filepath.Dir(dbPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
		db, err = gorm.Open(sqlite.Open(dbPath), &gorm.Config{
			Logger: logger.Default.LogMode(logger.Silent),
		})
		if err != nil {
			return nil, err
		}
		log.Info().Str("path", dbPath).Msg("Database initialized (SQLite)")
	}

	// Auto migrate all models
	if err := db.AutoMigrate(&Market{}, &Opportunity{}, &Trade{}, &ArbTrade{}, &Alert{}, &UserSettings{}, &WindowPrice{}); err != nil {
		return nil, err
	}

	return &Database{db: db}, nil
}

// Market operations

func (d *Database) SaveMarket(market *Market) error {
	return d.db.Save(market).Error
}

func (d *Database) GetMarket(id string) (*Market, error) {
	var market Market
	err := d.db.First(&market, "id = ?", id).Error
	return &market, err
}

// Opportunity operations

func (d *Database) SaveOpportunity(opp *Opportunity) error {
	return d.db.Create(opp).Error
}

func (d *Database) GetRecentOpportunities(limit int) ([]Opportunity, error) {
	var opps []Opportunity
	err := d.db.Order("created_at DESC").Limit(limit).Find(&opps).Error
	return opps, err
}

func (d *Database) GetLastAlertTime(marketID string) (time.Time, error) {
	var alert Alert
	err := d.db.Where("market_id = ?", marketID).Order("created_at DESC").First(&alert).Error
	if err != nil {
		return time.Time{}, err
	}
	return alert.CreatedAt, nil
}

// Alert operations

func (d *Database) SaveAlert(alert *Alert) error {
	return d.db.Create(alert).Error
}

// Trade operations

func (d *Database) SaveTrade(trade *Trade) error {
	return d.db.Create(trade).Error
}

func (d *Database) UpdateTrade(trade *Trade) error {
	return d.db.Save(trade).Error
}

func (d *Database) GetTradesByMarket(marketID string) ([]Trade, error) {
	var trades []Trade
	err := d.db.Where("market_id = ?", marketID).Order("created_at DESC").Find(&trades).Error
	return trades, err
}

func (d *Database) GetTotalProfitLoss() (decimal.Decimal, error) {
	var result struct {
		Total decimal.Decimal
	}
	err := d.db.Model(&Trade{}).Select("COALESCE(SUM(profit_loss), 0) as total").Scan(&result).Error
	return result.Total, err
}

// User settings operations

func (d *Database) GetUserSettings(chatID int64) (*UserSettings, error) {
	var settings UserSettings
	err := d.db.FirstOrCreate(&settings, UserSettings{ChatID: chatID}).Error
	return &settings, err
}

func (d *Database) SaveUserSettings(settings *UserSettings) error {
	return d.db.Save(settings).Error
}

// Stats operations

func (d *Database) GetStats() (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	var opportunityCount int64
	d.db.Model(&Opportunity{}).Count(&opportunityCount)
	stats["total_opportunities"] = opportunityCount

	var tradeCount int64
	d.db.Model(&Trade{}).Count(&tradeCount)
	stats["total_trades"] = tradeCount

	pnl, _ := d.GetTotalProfitLoss()
	stats["total_pnl"] = pnl

	var marketCount int64
	d.db.Model(&Market{}).Where("active = ?", true).Count(&marketCount)
	stats["active_markets"] = marketCount

	return stats, nil
}

// ============ ARBITRAGE TRADE OPERATIONS ============

// SaveArbTrade saves an arbitrage trade to the database
func (d *Database) SaveArbTrade(trade *ArbTrade) error {
	trade.CreatedAt = time.Now()
	trade.UpdatedAt = time.Now()
	return d.db.Create(trade).Error
}

// UpdateArbTrade updates an existing arbitrage trade
func (d *Database) UpdateArbTrade(trade *ArbTrade) error {
	trade.UpdatedAt = time.Now()
	return d.db.Save(trade).Error
}

// GetArbTrade retrieves a single arbitrage trade by ID
func (d *Database) GetArbTrade(id string) (*ArbTrade, error) {
	var trade ArbTrade
	err := d.db.First(&trade, "id = ?", id).Error
	return &trade, err
}

// DeleteArbTrade deletes an arbitrage trade by ID (used for test cleanup)
func (d *Database) DeleteArbTrade(id string) error {
	return d.db.Delete(&ArbTrade{}, "id = ?", id).Error
}

// GetRecentArbTrades gets recent arbitrage trades
func (d *Database) GetRecentArbTrades(limit int) ([]ArbTrade, error) {
	var trades []ArbTrade
	err := d.db.Order("entered_at DESC").Limit(limit).Find(&trades).Error
	return trades, err
}

// GetOpenArbTrades gets all open arbitrage trades
func (d *Database) GetOpenArbTrades() ([]ArbTrade, error) {
	var trades []ArbTrade
	err := d.db.Where("status IN ?", []string{"open", "filled"}).Order("entered_at DESC").Find(&trades).Error
	return trades, err
}

// GetArbTradesByAsset gets trades for a specific asset
func (d *Database) GetArbTradesByAsset(asset string, limit int) ([]ArbTrade, error) {
	var trades []ArbTrade
	err := d.db.Where("asset = ?", asset).Order("entered_at DESC").Limit(limit).Find(&trades).Error
	return trades, err
}

// GetArbTradeStats gets aggregate statistics for arbitrage trades
func (d *Database) GetArbTradeStats() (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	// Total trades
	var totalCount int64
	d.db.Model(&ArbTrade{}).Count(&totalCount)
	stats["total_trades"] = totalCount

	// Won/Lost
	var wonCount int64
	d.db.Model(&ArbTrade{}).Where("status = ?", "won").Count(&wonCount)
	stats["won_trades"] = wonCount

	var lostCount int64
	d.db.Model(&ArbTrade{}).Where("status = ?", "lost").Count(&lostCount)
	stats["lost_trades"] = lostCount

	// Open positions
	var openCount int64
	d.db.Model(&ArbTrade{}).Where("status IN ?", []string{"open", "filled"}).Count(&openCount)
	stats["open_positions"] = openCount

	// Total profit
	var profitResult struct {
		Total decimal.Decimal
	}
	d.db.Model(&ArbTrade{}).Select("COALESCE(SUM(profit), 0) as total").Scan(&profitResult)
	stats["total_profit"] = profitResult.Total

	// By asset
	type AssetCount struct {
		Asset string
		Count int64
	}
	var assetCounts []AssetCount
	d.db.Model(&ArbTrade{}).Select("asset, count(*) as count").Group("asset").Scan(&assetCounts)
	assetStats := make(map[string]int64)
	for _, ac := range assetCounts {
		assetStats[ac.Asset] = ac.Count
	}
	stats["by_asset"] = assetStats

	return stats, nil
}

// ============ SCALP TRADE OPERATIONS ============

// SaveScalpTrade saves a new scalp trade entry
func (d *Database) SaveScalpTrade(trade *ScalpTrade) error {
	trade.CreatedAt = time.Now()
	trade.UpdatedAt = time.Now()
	return d.db.Create(trade).Error
}

// UpdateScalpTrade updates an existing scalp trade (e.g., on exit)
func (d *Database) UpdateScalpTrade(trade *ScalpTrade) error {
	trade.UpdatedAt = time.Now()
	return d.db.Save(trade).Error
}

// GetOpenScalpTrades gets all OPEN scalp trades
func (d *Database) GetOpenScalpTrades() ([]ScalpTrade, error) {
	var trades []ScalpTrade
	err := d.db.Where("status = ?", "OPEN").Find(&trades).Error
	return trades, err
}

// GetScalpTrade retrieves a scalp trade by ID
func (d *Database) GetScalpTrade(tradeID string) (*ScalpTrade, error) {
	var trade ScalpTrade
	err := d.db.First(&trade, "trade_id = ?", tradeID).Error
	return &trade, err
}

// GetRecentScalpTrades gets recent scalp trades
func (d *Database) GetRecentScalpTrades(limit int) ([]ScalpTrade, error) {
	var trades []ScalpTrade
	err := d.db.Order("created_at DESC").Limit(limit).Find(&trades).Error
	return trades, err
}

// GetScalpTradeStats gets aggregate statistics
func (d *Database) GetScalpTradeStats() (map[string]interface{}, error) {
	stats := make(map[string]interface{})

	var totalCount int64
	d.db.Model(&ScalpTrade{}).Count(&totalCount)
	stats["total_trades"] = totalCount

	var closedCount int64
	d.db.Model(&ScalpTrade{}).Where("status = ?", "CLOSED").Count(&closedCount)
	stats["closed_trades"] = closedCount

	var openCount int64
	d.db.Model(&ScalpTrade{}).Where("status = ?", "OPEN").Count(&openCount)
	stats["open_trades"] = openCount

	// Total P&L
	var profitResult struct {
		Total decimal.Decimal
	}
	d.db.Model(&ScalpTrade{}).Where("status = ?", "CLOSED").Select("COALESCE(SUM(profit_loss), 0) as total").Scan(&profitResult)
	stats["total_profit"] = profitResult.Total

	// Win rate
	var winCount int64
	d.db.Model(&ScalpTrade{}).Where("status = ? AND profit_loss > 0", "CLOSED").Count(&winCount)
	if closedCount > 0 {
		stats["win_rate"] = float64(winCount) / float64(closedCount)
	} else {
		stats["win_rate"] = 0.0
	}

	return stats, nil
}

// UpdateMLLearning updates the ml_learning table with trade outcome
func (d *Database) UpdateMLLearning(asset string, priceBucket int, profit decimal.Decimal, won bool) error {
	// Using raw SQL since this table has UPSERT logic
	sql := `
		INSERT INTO ml_learning (asset, price_bucket, total_trades, winning_trades, total_profit, updated_at)
		VALUES ($1, $2, 1, $3, $4, NOW())
		ON CONFLICT (asset, price_bucket) DO UPDATE SET
			total_trades = ml_learning.total_trades + 1,
			winning_trades = ml_learning.winning_trades + $3,
			total_profit = ml_learning.total_profit + $4,
			win_rate = (ml_learning.winning_trades + $3)::DECIMAL / (ml_learning.total_trades + 1),
			updated_at = NOW()
	`
	winInt := 0
	if won {
		winInt = 1
	}
	return d.db.Exec(sql, asset, priceBucket, winInt, profit).Error
}

// UpdateDailyStats updates the daily_stats table
func (d *Database) UpdateDailyStats(profit decimal.Decimal, asset string, won bool) error {
	today := time.Now().Format("2006-01-02")
	winInt := 0
	loseInt := 0
	if won {
		winInt = 1
	} else {
		loseInt = 1
	}
	
	// Build asset-specific columns
	assetTradesCol := "btc_trades"
	assetProfitCol := "btc_profit"
	switch asset {
	case "ETH":
		assetTradesCol = "eth_trades"
		assetProfitCol = "eth_profit"
	case "SOL":
		assetTradesCol = "sol_trades"
		assetProfitCol = "sol_profit"
	}

	sql := `
		INSERT INTO daily_stats (date, total_trades, winning_trades, losing_trades, total_profit, ` + assetTradesCol + `, ` + assetProfitCol + `)
		VALUES ($1, 1, $2, $3, $4, 1, $4)
		ON CONFLICT (date) DO UPDATE SET
			total_trades = daily_stats.total_trades + 1,
			winning_trades = daily_stats.winning_trades + $2,
			losing_trades = daily_stats.losing_trades + $3,
			total_profit = daily_stats.total_profit + $4,
			` + assetTradesCol + ` = daily_stats.` + assetTradesCol + ` + 1,
			` + assetProfitCol + ` = daily_stats.` + assetProfitCol + ` + $4,
			updated_at = NOW()
	`
	return d.db.Exec(sql, today, winInt, loseInt, profit).Error
}

// WindowPrice operations - store/retrieve Price to Beat for prediction windows

// SaveWindowPrice stores the captured price for a window (upsert)
func (d *Database) SaveWindowPrice(wp *WindowPrice) error {
	// Use upsert - if window slug already exists, don't overwrite
	return d.db.Where("window_slug = ?", wp.WindowSlug).FirstOrCreate(wp).Error
}

// GetWindowPrice retrieves the stored price for a window by slug
func (d *Database) GetWindowPrice(windowSlug string) (*WindowPrice, error) {
	var wp WindowPrice
	err := d.db.Where("window_slug = ?", windowSlug).First(&wp).Error
	if err != nil {
		return nil, err
	}
	return &wp, nil
}

// GetWindowPriceByAssetAndTime retrieves price by asset and approximate window start time
func (d *Database) GetWindowPriceByAssetAndTime(asset string, windowStart time.Time) (*WindowPrice, error) {
	var wp WindowPrice
	// Allow 30 second tolerance for matching window start
	startMin := windowStart.Add(-30 * time.Second)
	startMax := windowStart.Add(30 * time.Second)
	err := d.db.Where("asset = ? AND window_start BETWEEN ? AND ?", asset, startMin, startMax).First(&wp).Error
	if err != nil {
		return nil, err
	}
	return &wp, nil
}

// CleanOldWindowPrices removes window prices older than 24 hours
func (d *Database) CleanOldWindowPrices() error {
	cutoff := time.Now().Add(-24 * time.Hour)
	return d.db.Where("window_start < ?", cutoff).Delete(&WindowPrice{}).Error
}
