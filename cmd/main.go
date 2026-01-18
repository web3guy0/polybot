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
	log.Info().Msg("              POLYBOT v6.0 - SNIPER V3 EDITION")
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

	// 2. Binance feed (for real-time crypto prices)
	binanceFeed := feeds.NewBinanceFeed()
	binanceFeed.Start()
	log.Info().Msg("✅ Binance price feed initialized")

	// 3. Polymarket feeds
	polyFeed := feeds.NewPolymarketFeed()
	log.Info().Msg("✅ Polymarket feed initialized")

	// 4. Window Scanner (tracks 15-min crypto windows)
	windowScanner := feeds.NewWindowScanner(binanceFeed)
	windowScanner.Start()
	log.Info().Msg("✅ Window scanner initialized")

	// 5. Execution client
	executor, err := exec.NewClient()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize executor")
	}
	log.Info().Msg("✅ Execution layer initialized")

	// 6. Risk manager
	riskMgr := risk.NewManager()
	log.Info().Msg("✅ Risk layer initialized")

	// 7. Load strategies - SniperV3 is our main strategy
	sniperV3 := strategy.NewSniperV3(binanceFeed, windowScanner)
	strategies := []strategy.Strategy{
		sniperV3,
	}
	log.Info().Int("count", len(strategies)).Msg("✅ Strategies loaded")

	// 8. Core engine
	engine := core.NewEngine(polyFeed, executor, riskMgr, strategies, db)
	log.Info().Msg("✅ Core engine initialized")

	// ═══════════════════════════════════════════════════════════════════════════════
	// PRINT CONFIG
	// ═══════════════════════════════════════════════════════════════════════════════

	log.Info().Msg("")
	log.Info().Msg("╔══════════════════════════════════════════════════════════════╗")
	log.Info().Msg("║           🎯 SNIPER V3 - LAST MINUTE CONFIRMED SIGNALS       ║")
	log.Info().Msg("╠══════════════════════════════════════════════════════════════╣")
	log.Info().Msgf("║  Mode: %-52s ║", func() string {
		if os.Getenv("DRY_RUN") == "true" {
			return "PAPER TRADING"
		}
		return "LIVE TRADING"
	}())
	log.Info().Msg("║  Assets: BTC, ETH, SOL                                       ║")
	log.Info().Msg("║  Strategy: Sniper V3 (200ms detection)                       ║")
	log.Info().Msg("║                                                              ║")
	log.Info().Msg("║  Entry Zone: 88-93¢                                          ║")
	log.Info().Msg("║  Take Profit: 99¢                                            ║")
	log.Info().Msg("║  Stop Loss: 70¢                                              ║")
	log.Info().Msg("║  Time Window: Last 15-60 seconds                             ║")
	log.Info().Msg("║  Min Price Move: 0.10% (BTC/ETH), 0.15% (SOL)                ║")
	log.Info().Msg("║                                                              ║")
	log.Info().Msg("║  Logic: Buy nearly-confirmed winners in last minute          ║")
	log.Info().Msg("╚══════════════════════════════════════════════════════════════╝")
	log.Info().Msg("")

	// ═══════════════════════════════════════════════════════════════════════════════
	// START
	// ═══════════════════════════════════════════════════════════════════════════════

	// Start engine (handles Polymarket ticks and position monitoring)
	go engine.Start()

	// Start SniperV3's 200ms detection loop
	signalCh := make(chan *strategy.Signal, 100)
	go sniperV3.RunLoop(signalCh)

	// Process signals from sniper
	go func() {
		for signal := range signalCh {
			engine.ProcessSignal(signal, sniperV3.Name())
		}
	}()

	log.Info().Msg("🚀 All systems running...")

	// ═══════════════════════════════════════════════════════════════════════════════
	// GRACEFUL SHUTDOWN
	// ═══════════════════════════════════════════════════════════════════════════════

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Info().Msg("🛑 Shutting down...")
	engine.Stop()
	binanceFeed.Stop()
	windowScanner.Stop()

	if db != nil {
		db.Close()
	}

	log.Info().Msg("👋 Goodbye!")
}
