package config

import (
"fmt"
"os"
"strconv"
"time"

"github.com/shopspring/decimal"
)

// Config holds all configuration for the bot
type Config struct {
// Telegram
TelegramToken  string
TelegramChatID int64

// Trading Asset
TradingAsset string

// Mode
DryRun bool
Debug  bool

// Polymarket API
PolymarketAPIURL  string
PolymarketCLOBURL string

// CLOB Credentials
CLOBApiKey     string
CLOBApiSecret  string
CLOBPassphrase string

// Wallet
WalletPrivateKey string
WalletAddress    string
SignerAddress    string // Address that signed/derived the API credentials
FunderAddress    string // Address that holds funds (may differ from signing key)
SignatureType    int    // 0=EOA, 1=Magic/Email, 2=Proxy

	// Arbitrage Settings
	ArbEnabled         bool
	ArbMinPriceMove    decimal.Decimal // e.g., 0.002 = 0.2%
	ArbMinOddsForEntry decimal.Decimal // e.g., 0.35 = 35 cents (min entry)
	ArbMaxOddsForEntry decimal.Decimal // e.g., 0.65 = 65 cents (max entry)
	ArbMinEdge         decimal.Decimal // e.g., 0.10 = 10%
	ArbPositionSize    decimal.Decimal // USD per trade
	ArbMaxDailyTrades  int
	ArbCooldownSeconds int

	// Triple Exit Strategy
	ArbExitOddsThreshold decimal.Decimal // e.g., 0.75 = exit at 75¢ for quick flip
	ArbHoldThreshold     decimal.Decimal // e.g., 0.005 = 0.5% BTC move confirms direction
	ArbStopLossPct       decimal.Decimal // e.g., 0.20 = 20% stop-loss
	Bankroll     decimal.Decimal

// Database
DatabasePath string
}

// Load loads configuration from environment variables
func Load() (*Config, error) {
cfg := &Config{
// Telegram
TelegramToken: os.Getenv("TELEGRAM_BOT_TOKEN"),

// Trading
TradingAsset: getEnv("TRADING_ASSET", "BTC"),
DryRun:       getEnvBool("DRY_RUN", true),
Debug:        getEnvBool("DEBUG", false),

// Polymarket API
PolymarketAPIURL:  getEnv("POLYMARKET_API_URL", "https://gamma-api.polymarket.com"),
PolymarketCLOBURL: getEnv("POLYMARKET_CLOB_URL", "https://clob.polymarket.com"),

// CLOB Credentials
CLOBApiKey:     os.Getenv("CLOB_API_KEY"),
CLOBApiSecret:  os.Getenv("CLOB_API_SECRET"),
CLOBPassphrase: os.Getenv("CLOB_PASSPHRASE"),

// Wallet
WalletPrivateKey: os.Getenv("WALLET_PRIVATE_KEY"),
WalletAddress:    os.Getenv("WALLET_ADDRESS"),
SignerAddress:    os.Getenv("SIGNER_ADDRESS"),
FunderAddress:    os.Getenv("FUNDER_ADDRESS"),
SignatureType:    getEnvInt("SIGNATURE_TYPE", 0),

		// Arbitrage Settings
		ArbEnabled:         getEnvBool("ARB_ENABLED", true),
		ArbMinPriceMove:    getEnvDecimal("ARB_MIN_PRICE_MOVE", decimal.NewFromFloat(0.002)),
		ArbMinOddsForEntry: getEnvDecimal("ARB_MIN_ODDS", decimal.NewFromFloat(0.35)),   // Min 35¢
		ArbMaxOddsForEntry: getEnvDecimal("ARB_MAX_ODDS", decimal.NewFromFloat(0.65)),   // Max 65¢
		ArbMinEdge:         getEnvDecimal("ARB_MIN_EDGE", decimal.NewFromFloat(0.10)),
		ArbPositionSize:    getEnvDecimal("ARB_POSITION_SIZE", decimal.NewFromFloat(1)),
		ArbMaxDailyTrades:  getEnvInt("ARB_MAX_DAILY_TRADES", 200),
		ArbCooldownSeconds: getEnvInt("ARB_COOLDOWN_SECONDS", 10),

		// Triple Exit Strategy
		ArbExitOddsThreshold: getEnvDecimal("ARB_EXIT_ODDS", decimal.NewFromFloat(0.75)),       // Sell at 75¢+
		ArbHoldThreshold:     getEnvDecimal("ARB_HOLD_THRESHOLD", decimal.NewFromFloat(0.005)), // 0.5% BTC confirms direction
		ArbStopLossPct:       getEnvDecimal("ARB_STOP_LOSS", decimal.NewFromFloat(0.20)),       // 🛑 20% stop-loss
		Bankroll:     getEnvDecimal("BANKROLL", decimal.NewFromFloat(1000)),

// Database
DatabasePath: getEnv("DATABASE_PATH", "data/polybot.db"),
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

// Helper functions

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
