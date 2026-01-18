package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/web3guy0/polybot/core"
	"github.com/web3guy0/polybot/exec"
	"github.com/web3guy0/polybot/feeds"
	"github.com/web3guy0/polybot/risk"
	"github.com/web3guy0/polybot/storage"
	"github.com/web3guy0/polybot/strategy"
)

func main() {
	// ═══════════════════════════════════════════════════════════════════════════════
	// BOOTSTRAP
	// ═══════════════════════════════════════════════════════════════════════════════

	// Load environment
	if err := godotenv.Load(); err != nil {
		log.Warn().Msg("No .env file found")
	}

	// Setup logging
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"})

	if os.Getenv("DEBUG") == "true" {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	} else {
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

	log.Info().Msg("═══════════════════════════════════════════════════════════════")
	log.Info().Msg("              POLYBOT v6.0 - MODULAR TRADING ENGINE")
	log.Info().Msg("═══════════════════════════════════════════════════════════════")

	// ═══════════════════════════════════════════════════════════════════════════════
	// INITIALIZE COMPONENTS
	// ═══════════════════════════════════════════════════════════════════════════════

	// 1. Storage (for state persistence)
	db, err := storage.NewDatabase()
	if err != nil {
		log.Warn().Err(err).Msg("Database connection failed, continuing without persistence")
	} else {
		log.Info().Msg("✅ Storage layer initialized")
	}

	// 2. Feeds (real-time data)
	feed := feeds.NewPolymarketFeed()
	log.Info().Msg("✅ Feed layer initialized")

	// 3. Execution client
	executor, err := exec.NewClient()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize executor")
	}
	log.Info().Msg("✅ Execution layer initialized")

	// 4. Risk manager
	riskMgr := risk.NewManager()
	log.Info().Msg("✅ Risk layer initialized")

	// 5. Load strategies
	strategies := []strategy.Strategy{
		strategy.NewBreakout15M(),
	}
	log.Info().Int("count", len(strategies)).Msg("✅ Strategies loaded")

	// 6. Core engine
	engine := core.NewEngine(feed, executor, riskMgr, strategies, db)
	log.Info().Msg("✅ Core engine initialized")

	// ═══════════════════════════════════════════════════════════════════════════════
	// START
	// ═══════════════════════════════════════════════════════════════════════════════

	log.Info().Msg("")
	log.Info().Msg("╔══════════════════════════════════════════╗")
	log.Info().Msg("║         🚀 ENGINE STARTING...            ║")
	log.Info().Msg("║                                          ║")
	log.Info().Msgf("║  Mode: %s                          ║", func() string {
		if os.Getenv("DRY_RUN") == "true" {
			return "PAPER"
		}
		return "LIVE "
	}())
	log.Info().Msg("║  Assets: BTC, ETH, SOL                   ║")
	log.Info().Msg("║  Strategy: 15m Breakout                  ║")
	log.Info().Msg("╚══════════════════════════════════════════╝")
	log.Info().Msg("")

	// Start engine
	go engine.Start()

	// ═══════════════════════════════════════════════════════════════════════════════
	// GRACEFUL SHUTDOWN
	// ═══════════════════════════════════════════════════════════════════════════════

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Info().Msg("🛑 Shutting down...")
	engine.Stop()

	if db != nil {
		db.Close()
	}

	log.Info().Msg("👋 Goodbye!")
}
