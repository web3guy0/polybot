// Polybot - Latency Arbitrage Trading Bot for Polymarket
//
// This bot exploits the information lag between Binance BTC price movements
// and Polymarket odds updates on Bitcoin Up/Down prediction windows.
//
// Strategy:
// 1. Track BTC price at window start from Binance WebSocket
// 2. Detect significant price moves (>0.2%)
// 3. Check if Polymarket odds are stale (still ~50/50)
// 4. Buy the winning side at discounted odds
// 5. Collect $1 on resolution
//
// Reference: @PurpleThunderBicycleMountain - $330k profit in 1 month
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/web3guy0/polybot/internal/arbitrage"
	"github.com/web3guy0/polybot/internal/binance"
	"github.com/web3guy0/polybot/internal/bot"
	"github.com/web3guy0/polybot/internal/config"
	"github.com/web3guy0/polybot/internal/database"
	"github.com/web3guy0/polybot/internal/polymarket"
)

const version = "4.0.0"

func main() {
	// Setup logging
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	zerolog.SetGlobalLevel(zerolog.InfoLevel)

	// Load environment
	if err := godotenv.Load(); err != nil {
		log.Warn().Msg("No .env file found, using environment variables")
	}

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load configuration")
	}

	if cfg.Debug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}

	asset := cfg.TradingAsset
	if asset == "" {
		asset = "BTC"
	}

	log.Info().
		Str("version", version).
		Str("mode", "latency_arbitrage").
		Str("asset", asset).
		Bool("dry_run", cfg.DryRun).
		Msg("⚡ Polybot Arbitrage starting...")

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize database
	db, err := database.New(cfg.DatabasePath)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize database")
	}

	// ====== CORE COMPONENTS ======

	// 1. Binance client - real-time BTC price feed
	binanceClient := binance.NewClient()
	if err := binanceClient.Start(); err != nil {
		log.Fatal().Err(err).Msg("Failed to start Binance client")
	}
	log.Info().Msg("📈 Binance WebSocket connected")

	// 2. Window scanner - find active prediction windows
	windowScanner := polymarket.NewWindowScanner(cfg.PolymarketAPIURL, asset)
	windowScanner.Start()
	log.Info().Str("asset", asset).Msg("🔍 Window scanner started")

	// 3. CLOB client - for trading and account data
	// Works with either: API credentials OR wallet private key (will derive creds)
	var clobClient *arbitrage.CLOBClient
	if cfg.WalletPrivateKey != "" || (cfg.CLOBApiKey != "" && cfg.CLOBApiSecret != "") {
		clobClient, err = arbitrage.NewCLOBClient(
			cfg.CLOBApiKey,
			cfg.CLOBApiSecret,
			cfg.CLOBPassphrase,
			cfg.WalletPrivateKey,
			cfg.SignerAddress,
			cfg.FunderAddress,
			cfg.SignatureType,
		)
		if err != nil {
			log.Warn().Err(err).Msg("⚠️ Failed to initialize CLOB client - trading disabled")
		} else {
			log.Info().Msg("💳 CLOB client initialized")
		}
	} else {
		log.Warn().Msg("⚠️ No credentials - add CLOB_API_KEY/SECRET to .env for trading")
	}

	// 4. Arbitrage engine - the money maker
	arbEngine := arbitrage.NewEngine(cfg, binanceClient, windowScanner)

	// Connect CLOB client to engine for order execution
	if clobClient != nil {
		arbEngine.SetCLOBClient(clobClient)
	}

	// Start the arbitrage engine
	arbEngine.Start()
	log.Info().Msg("⚡ Arbitrage engine started")

	// ====== TELEGRAM BOT ======
	telegramBot, err := bot.NewArbBot(cfg, db, binanceClient, windowScanner, arbEngine, clobClient)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize Telegram bot")
	}

	go telegramBot.Start()

	// ====== STARTUP COMPLETE ======
	log.Info().Msg("✅ All systems online")
	log.Info().Msg("")
	log.Info().Msg("╔══════════════════════════════════════════╗")
	log.Info().Msg("║     LATENCY ARBITRAGE MODE ACTIVE        ║")
	log.Info().Msg("║                                          ║")
	log.Info().Msg("║  Strategy: Exploit Binance→Polymarket    ║")
	log.Info().Msg("║            information lag               ║")
	log.Info().Msg("║                                          ║")
	log.Info().Msg("║  BTC moves on Binance                    ║")
	log.Info().Msg("║  → Polymarket odds stale                 ║")
	log.Info().Msg("║  → Buy mispriced outcome                 ║")
	log.Info().Msg("║  → Collect $1 on resolution              ║")
	log.Info().Msg("╚══════════════════════════════════════════╝")
	log.Info().Msg("")
	log.Info().Msg("💡 Use /help for commands")

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-quit:
		log.Info().Msg("🛑 Received shutdown signal")
	case <-ctx.Done():
		log.Info().Msg("🛑 Context cancelled")
	}

	// Graceful shutdown
	log.Info().Msg("Shutting down...")

	telegramBot.Stop()
	arbEngine.Stop()
	windowScanner.Stop()
	binanceClient.Stop()

	log.Info().Msg("👋 Goodbye!")
}
