package config

import (
"fmt"
"os"
"strconv"
"strings"
"time"

"github.com/shopspring/decimal"
)

// Config holds all configuration for the bot
type Config struct {
// Telegram
TelegramToken  string
TelegramChatID int64

// Trading Asset(s)
TradingAsset  string   // Single asset (backward compatible)
TradingAssets []string // Multi-asset: BTC, ETH, SOL

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
	ArbEnabled           bool
	ArbMinPriceMove      decimal.Decimal // e.g., 0.002 = 0.2%
	ArbMinOddsForEntry   decimal.Decimal // e.g., 0.35 = 35 cents (min entry)
	ArbMaxOddsForEntry   decimal.Decimal // e.g., 0.65 = 65 cents (max entry)
	ArbMinEdge           decimal.Decimal // e.g., 0.10 = 10%
	ArbPositionSize      decimal.Decimal // USD per trade
	ArbMaxDailyTrades    int
	ArbMaxTradesPerWindow int            // Max trades per 15-min window
	ArbCooldownSeconds   int

	// Scalper Strategy Settings
	ScalperPositionSize  decimal.Decimal // USD per scalp trade
	ScalperEntryThreshold decimal.Decimal // Enter when odds drop to this (e.g., 0.15)

	// Swing Strategy Settings
	SwingMaxPositionUSD decimal.Decimal // Max USD per swing trade
	SwingMaxPositions   int             // Max concurrent swing positions
	SwingMinDropPct     decimal.Decimal // Min drop % to trigger entry (e.g., 0.12 = 12¢)
	SwingCooldownSec    int             // Cooldown between trades

	// Sniper Strategy Settings (Last Minute)
	SniperPositionSizePct decimal.Decimal // % of balance per trade (e.g., 0.10 = 10%)
	SniperMinTimeMin      float64         // Min time remaining (minutes)
	SniperMaxTimeMin      float64         // Max time remaining (minutes)
	SniperMinPriceMove    decimal.Decimal // Min price move to confirm direction
	SniperMinOdds         decimal.Decimal // Min odds to buy (e.g., 0.79)
	SniperMaxOdds         decimal.Decimal // Max odds to buy (e.g., 0.90)
	
	// Per-Asset Price Movement (CRITICAL - varies by volatility)
	// BTC = 0.02% (stable, small moves are significant)
	// ETH = 0.04% (medium volatility)
	// SOL = 0.08% (volatile, needs bigger move to confirm)
	SniperBTCMinPriceMove decimal.Decimal
	SniperETHMinPriceMove decimal.Decimal
	SniperSOLMinPriceMove decimal.Decimal
	SniperTarget          decimal.Decimal // Quick flip target (e.g., 0.95)
	SniperStopLoss        decimal.Decimal // Stop loss (e.g., 0.75)
	SniperHoldToResolution bool           // Hold to resolution instead of quick flip (default: false)

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
TradingAsset:  getEnv("TRADING_ASSET", "BTC"),
TradingAssets: getEnvStringSlice("TRADING_ASSETS", []string{}), // e.g., "BTC,ETH,SOL"
DryRun:        getEnvBool("DRY_RUN", true),
Debug:         getEnvBool("DEBUG", false),

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
		ArbEnabled:           getEnvBool("ARB_ENABLED", true),
		ArbMinPriceMove:      getEnvDecimal("ARB_MIN_PRICE_MOVE", decimal.NewFromFloat(0.002)),
		ArbMinOddsForEntry:   getEnvDecimal("ARB_MIN_ODDS", decimal.NewFromFloat(0.35)),   // Min 35¢
		ArbMaxOddsForEntry:   getEnvDecimal("ARB_MAX_ODDS", decimal.NewFromFloat(0.65)),   // Max 65¢
		ArbMinEdge:           getEnvDecimal("ARB_MIN_EDGE", decimal.NewFromFloat(0.10)),
		ArbPositionSize:      getEnvDecimal("ARB_POSITION_SIZE", decimal.NewFromFloat(1)),
		ArbMaxDailyTrades:    getEnvInt("ARB_MAX_DAILY_TRADES", 200),
		ArbMaxTradesPerWindow: getEnvInt("ARB_MAX_TRADES_PER_WINDOW", 3),  // Max 3 trades per 15-min window
		ArbCooldownSeconds:   getEnvInt("ARB_COOLDOWN_SECONDS", 10),

		// Scalper Strategy
		ScalperPositionSize:   getEnvDecimal("SCALPER_POSITION_SIZE", decimal.NewFromFloat(0.50)),   // Default $0.50
		ScalperEntryThreshold: getEnvDecimal("SCALPER_ENTRY_THRESHOLD", decimal.NewFromFloat(0.15)), // 15¢ max

		// Swing Strategy
		SwingMaxPositionUSD: getEnvDecimal("SWING_MAX_POSITION_USD", decimal.NewFromFloat(2.0)),
		SwingMaxPositions:   getEnvInt("SWING_MAX_POSITIONS", 3),
		SwingMinDropPct:     getEnvDecimal("SWING_MIN_DROP_PCT", decimal.NewFromFloat(0.12)),
		SwingCooldownSec:    getEnvInt("SWING_COOLDOWN_SEC", 30),

		// Sniper Strategy (Last Minute) - NEW OPTIMIZED DEFAULTS
		SniperPositionSizePct: getEnvDecimal("SNIPER_POSITION_SIZE_PCT", decimal.NewFromFloat(0.10)), // 10% of balance
		SniperMinTimeMin:      getEnvFloat("SNIPER_MIN_TIME_MIN", 1.0),
		SniperMaxTimeMin:      getEnvFloat("SNIPER_MAX_TIME_MIN", 3.0),
		SniperMinPriceMove:    getEnvDecimal("SNIPER_MIN_PRICE_MOVE", decimal.NewFromFloat(0.002)),
		SniperMinOdds:         getEnvDecimal("SNIPER_MIN_ODDS", decimal.NewFromFloat(0.79)),  // Entry at 79¢+
		SniperMaxOdds:         getEnvDecimal("SNIPER_MAX_ODDS", decimal.NewFromFloat(0.90)),  // Entry up to 90¢
		SniperTarget:          getEnvDecimal("SNIPER_TARGET", decimal.NewFromFloat(0.99)),   // Exit at 99¢ (near resolution)
		SniperStopLoss:        getEnvDecimal("SNIPER_STOP_LOSS", decimal.NewFromFloat(0.50)), // SL at 50¢ (wide, let it breathe)
		SniperHoldToResolution: getEnvBool("SNIPER_HOLD_TO_RESOLUTION", false), // Default: quick flip at 99¢
		
		// Per-Asset Price Movement (CRITICAL - based on volatility)
		// BTC = low volatility, small moves are significant
		// ETH = medium volatility, needs moderate confirmation
		// SOL = high volatility, needs bigger move to filter noise
		SniperBTCMinPriceMove: getEnvDecimal("SNIPER_BTC_MIN_PRICE_MOVE", decimal.NewFromFloat(0.02)), // 0.02%
		SniperETHMinPriceMove: getEnvDecimal("SNIPER_ETH_MIN_PRICE_MOVE", decimal.NewFromFloat(0.04)), // 0.04%
		SniperSOLMinPriceMove: getEnvDecimal("SNIPER_SOL_MIN_PRICE_MOVE", decimal.NewFromFloat(0.08)), // 0.08%

		// Triple Exit Strategy
		ArbExitOddsThreshold: getEnvDecimal("ARB_EXIT_ODDS", decimal.NewFromFloat(0.75)),       // Sell at 75¢+
		ArbHoldThreshold:     getEnvDecimal("ARB_HOLD_THRESHOLD", decimal.NewFromFloat(0.005)), // 0.5% BTC confirms direction
		ArbStopLossPct:       getEnvDecimal("ARB_STOP_LOSS", decimal.NewFromFloat(0.20)),       // 🛑 20% stop-loss
		Bankroll:             getEnvDecimal("BANKROLL", decimal.NewFromFloat(5)),               // Your current balance

		// Database - supports PostgreSQL URL or SQLite path
		DatabasePath: getEnv("DATABASE_URL", getEnv("DATABASE_PATH", "data/polybot.db")),
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

func getEnvFloat(key string, defaultValue float64) float64 {
if value := os.Getenv(key); value != "" {
if f, err := strconv.ParseFloat(value, 64); err == nil {
return f
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

func getEnvStringSlice(key string, defaultValue []string) []string {
if value := os.Getenv(key); value != "" {
// Split by comma and trim whitespace
parts := make([]string, 0)
for _, part := range splitAndTrim(value, ",") {
if part != "" {
parts = append(parts, part)
}
}
if len(parts) > 0 {
return parts
}
}
return defaultValue
}

func splitAndTrim(s, sep string) []string {
parts := make([]string, 0)
for _, p := range strings.Split(s, sep) {
parts = append(parts, strings.TrimSpace(p))
}
return parts
}
