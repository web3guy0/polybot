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
	"time"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/web3guy0/polybot/internal/arbitrage"
	"github.com/web3guy0/polybot/internal/binance"
	"github.com/web3guy0/polybot/internal/bot"
	"github.com/web3guy0/polybot/internal/chainlink"
	"github.com/web3guy0/polybot/internal/cmc"
	"github.com/web3guy0/polybot/internal/config"
	"github.com/web3guy0/polybot/internal/dashboard"
	"github.com/web3guy0/polybot/internal/database"
	"github.com/web3guy0/polybot/internal/polymarket"
)

const version = "5.0.0" // Mean Reversion Swing Trading

func main() {
	// Check for dashboard mode FIRST (before setting up zerolog)
	useDashboard := false
	useSwing := false      // Mean reversion swing trading
	useSniper := false     // Last minute sniper strategy
	for _, arg := range os.Args[1:] {
		if arg == "--dashboard" || arg == "-d" || arg == "--responsive" || arg == "-r" {
			useDashboard = true
		}
		if arg == "--swing" || arg == "-s" {
			useSwing = true
		}
		if arg == "--sniper" || arg == "--snipe" {
			useSniper = true
		}
	}

	// Create dashboard
	var dash *dashboard.ResponsiveDash
	
	if useDashboard {
		strategyName := "SCALPER"
		if useSwing {
			strategyName = "SWING"
		}
		if useSniper {
			strategyName = "SNIPER"
		}
		dash = dashboard.NewResponsiveDash(strategyName)
	}

	// Setup logging - route to dashboard if enabled
	if dash != nil {
		// Silent mode - logs go to dashboard
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
		log.Logger = log.Output(dash.Writer())
	} else {
		// Normal console logging
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

	// Check for analysis mode
	if len(os.Args) > 1 && os.Args[1] == "--analyze" {
		arbitrage.PrintProbabilityAnalysis()
		
		// Run Monte Carlo for all conditions
		conditions := arbitrage.MarketConditions{
			Profile:          arbitrage.MediumVolProfile,
			AvgSpread:        0.02,
			OrderBookDepth:   1000,
			CompetitorBots:   5,
			LatencyAdvantage: 50,
		}
		arbitrage.MonteCarloSimulation(conditions, 10000)
		return
	}

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

	// Multi-asset support: BTC, ETH, SOL
	// Can be configured via TRADING_ASSETS="BTC,ETH,SOL" or defaults to BTC only
	assets := cfg.TradingAssets
	if len(assets) == 0 {
		// Fallback to single asset config
		asset := cfg.TradingAsset
		if asset == "" {
			asset = "BTC"
		}
		assets = []string{asset}
	}

	log.Info().
		Str("version", version).
		Str("mode", "latency_arbitrage").
		Strs("assets", assets).
		Bool("dry_run", cfg.DryRun).
		Msg("⚡ Polybot Multi-Asset Arbitrage starting...")

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize database for trade logging
	db, dbErr := database.New(cfg.DatabasePath)
	if dbErr != nil {
		log.Warn().Err(dbErr).Msg("Database initialization failed (continuing without)")
	} else {
		log.Info().Msg("📊 Database connected for trade logging")
	}

	// ====== CORE COMPONENTS ======

	// 1. Binance client - real-time BTC price feed (still needed as fallback)
	binanceClient := binance.NewClient()
	if err := binanceClient.Start(); err != nil {
		log.Fatal().Err(err).Msg("Failed to start Binance client")
	}
	log.Info().Msg("📈 Binance WebSocket connected")

	// 1b. Multi-asset Binance client - BTC, ETH, SOL real-time prices
	binanceMulti := binance.NewMultiClient(assets...)
	if err := binanceMulti.Start(); err != nil {
		log.Warn().Err(err).Msg("⚠️ Failed to start multi-asset Binance")
	}

	// 2. Chainlink client - BTC/USD price feed on Polygon (what Polymarket uses for resolution)
	chainlinkClient := chainlink.NewClient()
	if err := chainlinkClient.Start(); err != nil {
		log.Warn().Err(err).Msg("⚠️ Failed to start Chainlink client - using CMC/Binance only")
	} else {
		log.Info().Msg("⛓️ Chainlink price feed connected (Polygon)")
	}

	// 2b. Multi-asset Chainlink client - BTC, ETH, SOL price feeds for Price to Beat
	multiChainlink := chainlink.NewMultiClient(assets...)
	if err := multiChainlink.Start(); err != nil {
		log.Warn().Err(err).Msg("⚠️ Failed to start multi-asset Chainlink - using CMC only for ETH/SOL")
	}

	// 3. CMC client - fast price updates for ALL assets (BTC, ETH, SOL)
	cmcClient := cmc.NewClient(assets...)
	cmcClient.Start()
	log.Info().Strs("assets", assets).Msg("📊 CMC price feed started (1s updates)")

	// 3b. Polymarket WebSocket client - real-time odds updates
	wsClient := polymarket.NewWSClient()
	if err := wsClient.Connect(); err != nil {
		log.Warn().Err(err).Msg("⚠️ Failed to connect Polymarket WebSocket - using HTTP polling")
		wsClient = nil
	}

	// 4. CLOB client - for trading and account data (shared across all engines)
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

	// 5. Create window scanners and arbitrage engines for EACH asset
	windowScanners := make([]*polymarket.WindowScanner, 0, len(assets))
	arbEngines := make([]*arbitrage.Engine, 0, len(assets))
	
	for _, asset := range assets {
		// Window scanner for this asset
		scanner := polymarket.NewWindowScanner(cfg.PolymarketAPIURL, asset)
		scanner.Start()
		windowScanners = append(windowScanners, scanner)
		log.Info().Str("asset", asset).Msg("🔍 Window scanner started")
		
		// Arbitrage engine for this asset
		engine := arbitrage.NewEngine(cfg, binanceClient, chainlinkClient, cmcClient, scanner)
		engine.SetAsset(asset) // Tell engine which asset it's tracking
		
		// Wire up database for storing window prices
		if db != nil {
			engine.SetDatabase(db)
		}
		
		// Wire up multi-asset Chainlink for Price to Beat
		if multiChainlink != nil {
			engine.SetMultiChainlink(multiChainlink)
		}
		
		// Wire up multi-asset Binance for all assets
		if binanceMulti != nil {
			engine.SetBinanceMulti(binanceMulti)
		}
		
		// Wire up WebSocket for real-time odds
		if wsClient != nil {
			engine.SetWSClient(wsClient)
		}
		
		if clobClient != nil {
			engine.SetCLOBClient(clobClient)
		}
		engine.Start()
		arbEngines = append(arbEngines, engine)
		log.Info().Str("asset", asset).Msg("⚡ Arbitrage engine started")
	}

	// ====== SWING STRATEGY (Mean Reversion) ======
	// Buy the dip (12¢+ drop), sell the bounce (+6¢ profit)
	// Trades volatility, not window resolution
	var swingStrategies []*arbitrage.SwingStrategy
	if useSwing {
		swingStrategies = make([]*arbitrage.SwingStrategy, 0, len(assets))
		for i, asset := range assets {
			swing := arbitrage.NewSwingStrategy(
				windowScanners[i],
				clobClient,
				db,
			)
			swing.SetEngine(arbEngines[i]) // For live price data
			swing.SetNotifier(nil) // Will set after telegram init
			if dash != nil {
				swing.SetDashboard(dash)
			}
			swing.Start()
			swingStrategies = append(swingStrategies, swing)
			log.Info().Str("asset", asset).Msg("🎢 Swing strategy started (mean reversion)")
		}
	}

	// ====== SCALPER STRATEGY ======
	// Buy extreme mispricings (<20¢) and sell when they bounce to 33¢+
	// CRITICAL: Uses engine's price-to-beat data to avoid late entries!
	var scalperStrategies []*arbitrage.ScalperStrategy
	if !useSwing && !useSniper {
		scalperStrategies = make([]*arbitrage.ScalperStrategy, 0, len(assets))
		for i, asset := range assets {
			scalper := arbitrage.NewScalperStrategy(
				windowScanners[i], 
				clobClient, 
				cfg.DryRun,
				cfg.ScalperPositionSize,    // Position size from config
			)
			// Link scalper to engine for price-to-beat data
			scalper.SetEngine(arbEngines[i])
			// Connect to database for trade logging
			if db != nil {
				scalper.SetDatabase(db)
			}
			scalper.Start()
			scalperStrategies = append(scalperStrategies, scalper)
			log.Info().Str("asset", asset).Bool("paper", cfg.DryRun).Msg("🎯 Scalper strategy started")
		}
	}
	
	// ====== SNIPER STRATEGY ======
	// Last-minute high-confidence trades: buy at 85-92¢ in final 2-3 mins
	// Target: 95¢ (5-10¢ profit) | Stop: 75¢ (tight risk control)
	var sniperStrategies []*arbitrage.SniperStrategy
	if useSniper {
		sniperStrategies = make([]*arbitrage.SniperStrategy, 0, len(assets))
		for i, asset := range assets {
			sniper := arbitrage.NewSniperStrategy(
				windowScanners[i],
				clobClient,
				cfg.SniperPositionSize,
			)
			// Link sniper to engine for price data
			sniper.SetEngine(arbEngines[i])
			// Connect to database for trade logging
			if db != nil {
				sniper.SetDatabase(db)
			}
			sniper.Start()
			sniperStrategies = append(sniperStrategies, sniper)
			log.Info().Str("asset", asset).Bool("paper", cfg.DryRun).Msg("🎯 Sniper strategy started")
		}
	}
	
	// Subscribe to WebSocket markets for all active windows
	if wsClient != nil {
		go func() {
			// Wait for initial window scan
			time.Sleep(3 * time.Second)
			for _, scanner := range windowScanners {
				for _, w := range scanner.GetActiveWindows() {
					if w.ConditionID != "" && w.YesTokenID != "" {
						wsClient.Subscribe(w.ConditionID, w.YesTokenID, w.NoTokenID)
					}
				}
			}
		}()
	}

	// ====== TELEGRAM BOT ======
	telegramBot, err := bot.New(cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize Telegram bot")
	}
	
	// Register engines and strategies for each asset
	// Also connect telegram as notifier for trade alerts
	for i, asset := range assets {
		telegramBot.AddEngine(asset, arbEngines[i])
		
		if useSwing && i < len(swingStrategies) {
			// Using swing strategy
			swingStrategies[i].SetNotifier(telegramBot)
			if dash != nil {
				swingStrategies[i].SetDashboard(dash)
			}
		} else if useSniper && i < len(sniperStrategies) {
			// Using sniper strategy
			sniperStrategies[i].SetNotifier(telegramBot)
			if dash != nil {
				sniperStrategies[i].SetDashboard(dash)
			}
		} else if !useSwing && !useSniper && i < len(scalperStrategies) {
			// Using scalper strategy (default)
			telegramBot.AddScalper(asset, scalperStrategies[i])
			scalperStrategies[i].SetNotifier(telegramBot)
			if dash != nil {
				scalperStrategies[i].SetDashboard(dash)
			}
		}
	}

	go telegramBot.Start()
	
	// Start dashboard if enabled
	if dash != nil {
		dash.Start()
	}

	// ====== STARTUP COMPLETE ======
	log.Info().Msg("✅ All systems online")
	log.Info().Msg("")
	if useSwing {
		log.Info().Msg("╔══════════════════════════════════════════╗")
		log.Info().Msg("║   🎢 MEAN REVERSION SWING TRADING 🎢      ║")
		log.Info().Msg("║                                          ║")
		log.Info().Msg("║  Strategy: Buy dips, sell bounces        ║")
		log.Info().Msg("║                                          ║")
		log.Info().Msgf("║  Assets: %-32s ║", formatAssets(assets))
		log.Info().Msg("║  → Track odds history (30s lookback)     ║")
		log.Info().Msg("║  → Buy when odds drop 12¢+ quickly       ║")
		log.Info().Msg("║  → Sell on 50% retracement bounce        ║")
		log.Info().Msg("║                                          ║")
		log.Info().Msg("║  📈 Entry: 8¢-65¢ range only            ║")
		log.Info().Msg("║  🎯 Exit: Bounce target / Stop loss      ║")
		log.Info().Msg("╚══════════════════════════════════════════╝")
	} else if useSniper {
		log.Info().Msg("╔══════════════════════════════════════════╗")
		log.Info().Msg("║   🎯 LAST MINUTE SNIPER STRATEGY 🎯       ║")
		log.Info().Msg("║                                          ║")
		log.Info().Msg("║  Strategy: High-confidence final trades  ║")
		log.Info().Msg("║                                          ║")
		log.Info().Msgf("║  Assets: %-32s ║", formatAssets(assets))
		log.Info().Msg("║  → Wait for last 1-3 minutes             ║")
		log.Info().Msg("║  → Buy at 85-92¢ if price moved 0.2%+   ║")
		log.Info().Msg("║  → Target 95¢ / Stop 75¢                ║")
		log.Info().Msg("║                                          ║")
		log.Info().Msg("║  🔫 Quick scalps on near-certain bets   ║")
		log.Info().Msg("║  ⚡ 5-10¢ profit, tight risk control     ║")
		log.Info().Msg("╚══════════════════════════════════════════╝")
	} else {
		log.Info().Msg("╔══════════════════════════════════════════╗")
		log.Info().Msg("║   MULTI-ASSET LATENCY ARBITRAGE ACTIVE   ║")
		log.Info().Msg("║                                          ║")
		log.Info().Msg("║  Strategy: Exploit price→odds lag        ║")
		log.Info().Msg("║                                          ║")
		log.Info().Msgf("║  Assets: %-32s ║", formatAssets(assets))
		log.Info().Msg("║  → Watch for price moves on CMC          ║")
		log.Info().Msg("║  → Buy stale Polymarket odds             ║")
		log.Info().Msg("║  → Exit at 75¢ OR hold to resolution     ║")
		log.Info().Msg("║                                          ║")
		log.Info().Msg("║  🚀 Dynamic Sizing: 1x/2x/3x by move     ║")
		log.Info().Msg("║  🎯 Scalper: Buy cheap, sell on bounce   ║")
		log.Info().Msg("╚══════════════════════════════════════════╝")
	}
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

	// Stop dashboard first
	if dash != nil {
		dash.Stop()
	}
	
	telegramBot.Stop()
	for _, engine := range arbEngines {
		engine.Stop()
	}
	for _, scalper := range scalperStrategies {
		scalper.Stop()
	}
	for _, scanner := range windowScanners {
		scanner.Stop()
	}
	cmcClient.Stop()
	chainlinkClient.Stop()
	binanceClient.Stop()

	log.Info().Msg("👋 Goodbye!")
}

// formatAssets formats asset list for display
func formatAssets(assets []string) string {
	if len(assets) == 1 {
		return assets[0]
	}
	result := ""
	for i, a := range assets {
		if i > 0 {
			result += ", "
		}
		result += a
	}
	return result
}
