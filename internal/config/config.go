package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// MarketConfig defines a single market to trade
type MarketConfig struct {
	ID            string        `json:"id"`
	Asset         string        `json:"asset"`
	Timeframe     time.Duration `json:"timeframe"`
	Strategy      string        `json:"strategy"`
	MaxBet        float64       `json:"max_bet"`
	Enabled       bool          `json:"enabled"`
	MinConfidence float64       `json:"min_confidence,omitempty"`
	MinOdds       float64       `json:"min_odds,omitempty"`
	MaxOdds       float64       `json:"max_odds,omitempty"`
}

// RiskConfig defines risk management parameters
type RiskConfig struct {
	MaxBetSize       decimal.Decimal `json:"max_bet_size"`
	MaxDailyLoss     decimal.Decimal `json:"max_daily_loss"`
	MaxDailyTrades   int             `json:"max_daily_trades"`
	MaxDailyExposure decimal.Decimal `json:"max_daily_exposure"`
	MinConfidence    float64         `json:"min_confidence"`
	MinLiquidity     decimal.Decimal `json:"min_liquidity"`
	TradeCooldown    time.Duration   `json:"trade_cooldown"`
	ChopFilter       bool            `json:"chop_filter"`
	ChopThreshold    float64         `json:"chop_threshold"`
}

type Config struct {
	// Bot Settings
	Mode  string // "prediction" mode
	Debug bool

	// Telegram
	TelegramToken  string
	TelegramChatID int64

	// Alert Settings
	AlertCooldown time.Duration

	// Polymarket API Settings
	PolymarketBatchSize int
	PolymarketMaxRPS    int

	// Trading Settings (for future auto-trading)
	TradingEnabled bool
	MaxTradeSize   decimal.Decimal
	MinProfitPct   decimal.Decimal
	DryRun         bool

	// Polymarket
	PolymarketAPIURL string
	PolymarketWSURL  string
	PolymarketCLOBURL string

	// Database
	DatabasePath string

	// Wallet (for future trading)
	WalletPrivateKey string
	WalletAddress    string
	
	// Bankroll for risk management
	Bankroll decimal.Decimal
	
	// Risk Management
	Risk RiskConfig
	
	// Markets to trade (config-driven)
	Markets []MarketConfig
	
	// BTC Prediction Settings (legacy - use Markets instead)
	// TODO: Deprecated - migrate to Markets[] config
	BTCEnabled        bool
	BTCMinSignalScore int              // Minimum score to trigger trade (-100 to 100)
	BTCMinConfidence  float64          // Minimum confidence percentage
	BTCMaxBetSize     decimal.Decimal  // Max bet per trade
	BTCMinOdds        decimal.Decimal  // Minimum odds to bet (e.g., 0.40 = 40%)
	BTCMaxOdds        decimal.Decimal  // Maximum odds to bet (e.g., 0.65 = 65%)
	BTCCooldown       time.Duration    // Cooldown between trades
	BTCAutoTrade      bool             // Enable automatic trading
	BTCAlertOnly      bool             // Only send alerts, don't trade
	
	// Primary trading asset (configurable via env)
	// Use TRADING_ASSET env var to set (default: BTC)
	// Supports: BTC, ETH, SOL, etc.
	TradingAsset string
}

func Load() (*Config, error) {
	cfg := &Config{
		// Defaults
		Mode:                getEnv("BOT_MODE", "prediction"),
		Debug:               getEnvBool("DEBUG", false),
		TelegramToken:       os.Getenv("TELEGRAM_BOT_TOKEN"),
		AlertCooldown:       getEnvDuration("ALERT_COOLDOWN", 5*time.Minute),
		PolymarketBatchSize: getEnvInt("POLYMARKET_BATCH_SIZE", 1000),
		PolymarketMaxRPS:    getEnvInt("POLYMARKET_MAX_RPS", 5),
		TradingEnabled:      getEnvBool("TRADING_ENABLED", false),
		MaxTradeSize:        getEnvDecimal("MAX_TRADE_SIZE", decimal.NewFromFloat(100)),
		DryRun:              getEnvBool("DRY_RUN", true),
		PolymarketAPIURL:    getEnv("POLYMARKET_API_URL", "https://gamma-api.polymarket.com"),
		PolymarketWSURL:     getEnv("POLYMARKET_WS_URL", "wss://ws-subscriptions-clob.polymarket.com/ws"),
		PolymarketCLOBURL:   getEnv("POLYMARKET_CLOB_URL", "https://clob.polymarket.com"),
		DatabasePath:        getEnv("DATABASE_PATH", "data/polybot.db"),
		WalletPrivateKey:    os.Getenv("WALLET_PRIVATE_KEY"),
		WalletAddress:       os.Getenv("WALLET_ADDRESS"),
		
		// Bankroll
		Bankroll:          getEnvDecimal("BANKROLL", decimal.NewFromFloat(100)),
		
		// Risk Management
		Risk: RiskConfig{
			MaxBetSize:       getEnvDecimal("RISK_MAX_BET_SIZE", decimal.NewFromFloat(10)),
			MaxDailyLoss:     getEnvDecimal("RISK_MAX_DAILY_LOSS", decimal.NewFromFloat(50)),
			MaxDailyTrades:   getEnvInt("RISK_MAX_DAILY_TRADES", 20),
			MaxDailyExposure: getEnvDecimal("RISK_MAX_DAILY_EXPOSURE", decimal.NewFromFloat(100)),
			MinConfidence:    getEnvFloat("RISK_MIN_CONFIDENCE", 0.60),
			MinLiquidity:     getEnvDecimal("RISK_MIN_LIQUIDITY", decimal.NewFromFloat(1000)),
			TradeCooldown:    getEnvDuration("RISK_TRADE_COOLDOWN", 30*time.Second),
			ChopFilter:       getEnvBool("RISK_CHOP_FILTER", true),
			ChopThreshold:    getEnvFloat("RISK_CHOP_THRESHOLD", 25.0),
		},
		
		// Prediction Settings
		BTCEnabled:        getEnvBool("BTC_ENABLED", true),
		BTCMinSignalScore: getEnvInt("BTC_MIN_SIGNAL_SCORE", 25),
		BTCMinConfidence:  getEnvFloat("BTC_MIN_CONFIDENCE", 25.0),
		BTCMaxBetSize:     getEnvDecimal("BTC_MAX_BET_SIZE", decimal.NewFromFloat(50)),
		BTCMinOdds:        getEnvDecimal("BTC_MIN_ODDS", decimal.NewFromFloat(0.35)),
		BTCMaxOdds:        getEnvDecimal("BTC_MAX_ODDS", decimal.NewFromFloat(0.65)),
		BTCCooldown:       getEnvDuration("BTC_COOLDOWN", 2*time.Minute),
		BTCAutoTrade:      getEnvBool("BTC_AUTO_TRADE", false),
		BTCAlertOnly:      getEnvBool("BTC_ALERT_ONLY", true),
		
		// Primary trading asset (configurable)
		TradingAsset:      getEnv("TRADING_ASSET", "BTC"),
	}
	
	// Default markets using configured asset
	if cfg.BTCEnabled {
		asset := cfg.TradingAsset
		cfg.Markets = []MarketConfig{
			{
				ID:            fmt.Sprintf("%s_15m", strings.ToLower(asset)),
				Asset:         asset,
				Timeframe:     15 * time.Minute,
				Strategy:      "crypto_15m",
				MaxBet:        cfg.BTCMaxBetSize.InexactFloat64(),
				Enabled:       true,
				MinConfidence: 0.60,
				MinOdds:       cfg.BTCMinOdds.InexactFloat64(),
				MaxOdds:       cfg.BTCMaxOdds.InexactFloat64(),
			},
		}
	}

	// Parse chat ID
	if chatID := os.Getenv("TELEGRAM_CHAT_ID"); chatID != "" {
		id, err := strconv.ParseInt(chatID, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid TELEGRAM_CHAT_ID: %w", err)
		}
		cfg.TelegramChatID = id
	}

	// Validate required fields
	if cfg.TelegramToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}

	return cfg, nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		return value == "true" || value == "1" || value == "yes"
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if i, err := strconv.Atoi(value); err == nil {
			return i
		}
	}
	return defaultValue
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	if value := os.Getenv(key); value != "" {
		if d, err := time.ParseDuration(value); err == nil {
			return d
		}
	}
	return defaultValue
}

func getEnvDecimal(key string, defaultValue decimal.Decimal) decimal.Decimal {
	if value := os.Getenv(key); value != "" {
		if d, err := decimal.NewFromString(value); err == nil {
			return d
		}
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if f, err := strconv.ParseFloat(value, 64); err == nil {
			return f
		}
	}
	return defaultValue
}
