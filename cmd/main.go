package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/web3guy0/polybot/bot"
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
	log.Info().Msg("                    POLYBOT v6.0 - SNIPER")
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

	// 2. Binance feed (fallback price source)
	binanceFeed := feeds.NewBinanceFeed()
	binanceFeed.Start()
	log.Info().Msg("✅ Binance price feed initialized")

	// 3. Chainlink-aligned price feed (primary - matches Polymarket resolution)
	cmcKey := os.Getenv("CMC_API_KEY") // Optional
	chainlinkFeed := feeds.NewChainlinkFeed(cmcKey)
	chainlinkFeed.SetBinanceFallback(binanceFeed)
	chainlinkFeed.Start()
	log.Info().Msg("✅ Chainlink price feed initialized")

	// 4. Polymarket feeds
	polyFeed := feeds.NewPolymarketFeed()
	log.Info().Msg("✅ Polymarket feed initialized")

	// 5. Window Scanner (tracks 15-min crypto windows)
	windowScanner := feeds.NewWindowScanner(chainlinkFeed)
	if db != nil {
		windowScanner.SetDatabase(db) // Save snapshots to DB
	}
	windowScanner.SetBinanceFeed(binanceFeed) // For historical price lookups
	windowScanner.SetPolyFeed(polyFeed)       // For live odds updates
	windowScanner.Start()
	log.Info().Msg("✅ Window scanner initialized")

	// 6. Execution client
	executor, err := exec.NewClient()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize executor")
	}
	log.Info().Msg("✅ Execution layer initialized")

	// 7. Risk manager
	riskMgr := risk.NewManager()
	log.Info().Msg("✅ Risk layer initialized")

	// 8. Sniper strategy (uses Chainlink prices)
	sniper := strategy.NewSniper(chainlinkFeed, windowScanner)
	strategies := []strategy.Strategy{sniper}
	log.Info().Msg("✅ Strategy loaded")

	// 9. Core engine
	engine := core.NewEngine(polyFeed, executor, riskMgr, strategies, db)
	log.Info().Msg("✅ Engine initialized")

	// 10. Telegram bot (optional - fails gracefully if not configured)
	var tgBot *bot.TelegramBot
	if tg, err := bot.NewTelegramBot(engine); err != nil {
		log.Warn().Err(err).Msg("Telegram bot not available")
	} else {
		tgBot = tg
		tgBot.Start()
		engine.SetTradeNotifier(tgBot) // Wire up trade notifications
		log.Info().Msg("✅ Telegram initialized")
	}

	// ═══════════════════════════════════════════════════════════════════════════════
	// STATUS
	// ═══════════════════════════════════════════════════════════════════════════════

	mode := "LIVE"
	if os.Getenv("DRY_RUN") == "true" {
		mode = "PAPER"
	}
	
	minTime := os.Getenv("MIN_TIME_SEC")
	if minTime == "" {
		minTime = "15"
	}
	maxTime := os.Getenv("MAX_TIME_SEC")
	if maxTime == "" {
		maxTime = "60"
	}
	minOdds := os.Getenv("MIN_ODDS")
	if minOdds == "" {
		minOdds = "0.88"
	}
	maxOdds := os.Getenv("MAX_ODDS")
	if maxOdds == "" {
		maxOdds = "0.93"
	}
	scanMs := os.Getenv("SCAN_INTERVAL_MS")
	if scanMs == "" {
		scanMs = "100"
	}

	log.Info().Msg("")
	log.Info().Msg("╔═══════════════════════════════════════╗")
	log.Info().Msg("║           POLYBOT SNIPER              ║")
	log.Info().Msg("╠═══════════════════════════════════════╣")
	log.Info().Msgf("║  Mode:    %-27s ║", mode)
	log.Info().Msg("║  Assets:  BTC, ETH, SOL               ║")
	log.Info().Msgf("║  Scan:    %-27s ║", scanMs+"ms")
	log.Info().Msgf("║  Entry:   %-27s ║", minOdds[2:]+"¢-"+maxOdds[2:]+"¢")
	log.Info().Msg("║  TP/SL:   99¢ / 70¢                   ║")
	log.Info().Msgf("║  Window:  %-27s ║", minTime+"-"+maxTime+" sec")
	log.Info().Msg("╚═══════════════════════════════════════╝")
	log.Info().Msg("")

	// ═══════════════════════════════════════════════════════════════════════════════
	// START
	// ═══════════════════════════════════════════════════════════════════════════════

	// Start engine
	go engine.Start()

	// Start sniper's fast scan loop
	signalCh := make(chan *strategy.Signal, 100)
	go sniper.RunLoop(signalCh)

	// Process signals
	go func() {
		for sig := range signalCh {
			engine.ProcessSignal(sig, sniper.Name())
		}
	}()

	log.Info().Msg("🚀 Running...")

	// Telegram startup
	if tgBot != nil {
		mode := "PAPER"
		if os.Getenv("DRY_RUN") != "true" {
			mode = "LIVE"
		}
		tgBot.NotifyStartup(mode)
	}

	// ═══════════════════════════════════════════════════════════════════════════════
	// GRACEFUL SHUTDOWN
	// ═══════════════════════════════════════════════════════════════════════════════

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Info().Msg("🛑 Shutting down...")
	engine.Stop()
	chainlinkFeed.Stop()
	binanceFeed.Stop()
	windowScanner.Stop()

	if tgBot != nil {
		tgBot.Stop()
	}

	if db != nil {
		db.Close()
	}

	log.Info().Msg("👋 Goodbye!")
}
