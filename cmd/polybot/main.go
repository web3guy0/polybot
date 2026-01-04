// Polybot - Crypto Prediction Trading Bot for Polymarket
//
// This bot uses technical indicators to predict cryptocurrency price movements
// and trades Polymarket prediction windows based on those signals.
//
// Architecture: Strategy → Risk → Trade
// - Strategy generates signals (UP/DOWN/NO_TRADE) with confidence
// - Risk manager validates signals and sizes positions
// - Trading engine executes approved trades
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/internal/binance"
	"github.com/web3guy0/polybot/internal/bot"
	"github.com/web3guy0/polybot/internal/config"
	"github.com/web3guy0/polybot/internal/database"
	"github.com/web3guy0/polybot/internal/datafeed"
	"github.com/web3guy0/polybot/internal/markets"
	"github.com/web3guy0/polybot/internal/polymarket"
	"github.com/web3guy0/polybot/internal/predictor"
	"github.com/web3guy0/polybot/internal/risk"
	"github.com/web3guy0/polybot/internal/strategy"
	"github.com/web3guy0/polybot/internal/trading"
)

const version = "3.1.0"

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
		Str("asset", asset).
		Msg("🚀 Polybot starting...")

	// Initialize database
	db, err := database.New(cfg.DatabasePath)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize database")
	}

	// Initialize trading engine
	tradingEngine := trading.NewEngine(cfg, db)

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ====== PREDICTION SYSTEM ======
	var binanceClient *binance.Client
	var marketManager *markets.MarketManager
	var riskManager *risk.RiskManager
	var pred *predictor.Predictor
	var windowScanner *polymarket.BTCWindowScanner
	var trader *trading.BTCTrader

	if cfg.BTCEnabled {
		log.Info().Str("asset", asset).Msg("🔶 Initializing prediction system...")

		// 1. Initialize Binance client (data source)
		binanceClient = binance.NewClient()
		if err := binanceClient.Start(); err != nil {
			log.Error().Err(err).Msg("Failed to start Binance client")
		}

		// 2. Initialize Risk Manager
		riskConfig := risk.RiskConfig{
			MaxBetSize:       cfg.Risk.MaxBetSize,
			MinBetSize:       decimal.NewFromFloat(1.0),
			MaxPositionPct:   0.05,
			MaxDailyLoss:     cfg.Risk.MaxDailyLoss,
			MaxDailyTrades:   cfg.Risk.MaxDailyTrades,
			MaxDailyExposure: cfg.Risk.MaxDailyExposure,
			MinConfidence:    cfg.Risk.MinConfidence,
			MinStrength:      strategy.StrengthModerate,
			MinLiquidity:     cfg.Risk.MinLiquidity,
			MaxSpread:        0.05,
			MinOdds:          cfg.BTCMinOdds.InexactFloat64(),
			MaxOdds:          cfg.BTCMaxOdds.InexactFloat64(),
			MinTimeToExpiry:  cfg.BTCCooldown,
			TradeCooldown:    cfg.Risk.TradeCooldown,
			ChopFilter:       cfg.Risk.ChopFilter,
			ChopThreshold:    cfg.Risk.ChopThreshold,
		}
		riskManager = risk.NewRiskManager(riskConfig)

		// 3. Initialize Market Manager
		marketManager = markets.NewMarketManager(riskManager, cfg.Bankroll)

		// 4. Register strategy for configured asset
		crypto15mStrategy := strategy.NewCrypto15mStrategy(asset)
		marketManager.RegisterStrategy(crypto15mStrategy)

		// 5. Register data feed
		binanceFeed := datafeed.NewBinanceDataFeed(binanceClient)
		marketManager.RegisterDataFeed(asset, binanceFeed)

		// 6. Load markets from config
		marketConfigs := make([]markets.MarketConfig, len(cfg.Markets))
		for i, m := range cfg.Markets {
			marketConfigs[i] = markets.MarketConfig{
				ID:           m.ID,
				Asset:        m.Asset,
				Timeframe:    m.Timeframe,
				StrategyName: m.Strategy,
				MaxBet:       m.MaxBet,
				Enabled:      m.Enabled,
			}
		}
		if err := marketManager.LoadMarkets(marketConfigs); err != nil {
			log.Error().Err(err).Msg("Failed to load markets")
		}

		// 7. Start market manager
		go marketManager.Start(ctx)

		// ====== TELEGRAM BOT COMPONENTS ======
		// Window scanner for Polymarket
		windowScanner = polymarket.NewBTCWindowScanner(cfg.PolymarketAPIURL)
		windowScanner.Start()

		// Predictor for signal generation
		pred = predictor.NewPredictor(cfg, binanceClient)
		pred.Start()

		// Trader for executing trades
		trader = trading.NewBTCTrader(cfg, db, windowScanner, pred)
		if cfg.BTCAutoTrade {
			trader.Start()
		}

		log.Info().Msg("✅ Prediction system initialized")
	}

	// Initialize Telegram bot
	telegramBot, err := bot.New(cfg, db, tradingEngine, binanceClient, pred, windowScanner, trader)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize Telegram bot")
	}

	// Start bot
	go telegramBot.Start()

	log.Info().Msg("✅ All services started")
	log.Info().Msg("📊 Architecture: Strategy → Risk → Trade")
	log.Info().Msg("💡 Use /help to see available commands")

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info().Msg("🛑 Shutting down...")

	// Graceful shutdown
	cancel()
	telegramBot.Stop()
	if marketManager != nil {
		marketManager.Stop()
	}
	if binanceClient != nil {
		binanceClient.Stop()
	}

	log.Info().Msg("👋 Goodbye!")
}
