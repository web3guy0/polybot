package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/web3guy0/polybot/internal/bot"
	"github.com/web3guy0/polybot/internal/config"
	"github.com/web3guy0/polybot/internal/database"
	"github.com/web3guy0/polybot/internal/scanner"
	"github.com/web3guy0/polybot/internal/trading"
)

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

	log.Info().
		Str("version", "1.0.0").
		Str("mode", cfg.Mode).
		Msg("🚀 Polybot starting...")

	// Initialize database
	db, err := database.New(cfg.DatabasePath)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize database")
	}

	// Initialize trading engine (for future auto-trading)
	tradingEngine := trading.NewEngine(cfg, db)

	// Initialize market scanner
	marketScanner := scanner.New(cfg, db, tradingEngine)

	// Initialize Telegram bot
	telegramBot, err := bot.New(cfg, db, marketScanner, tradingEngine)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize Telegram bot")
	}

	// Start services
	go marketScanner.Start()
	go telegramBot.Start()

	log.Info().Msg("✅ All services started successfully")

	// Wait for shutdown signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info().Msg("🛑 Shutting down...")
	marketScanner.Stop()
	telegramBot.Stop()
	log.Info().Msg("👋 Goodbye!")
}
