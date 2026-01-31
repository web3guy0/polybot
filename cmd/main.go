package main

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/bot"
	"github.com/web3guy0/polybot/core"
	"github.com/web3guy0/polybot/exec"
	"github.com/web3guy0/polybot/execution"
	"github.com/web3guy0/polybot/feeds"
	"github.com/web3guy0/polybot/risk"
	"github.com/web3guy0/polybot/storage"
	"github.com/web3guy0/polybot/strategy"
)

const VERSION = "v8.0 PRO"

func main() {
	// ═══════════════════════════════════════════════════════════════════════════════
	// BOOTSTRAP
	// ═══════════════════════════════════════════════════════════════════════════════

	if err := godotenv.Load(); err != nil {
		log.Warn().Msg("No .env file found")
	} else {
		log.Info().Msg("✅ .env file loaded successfully")
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
	log.Info().Msgf("         POLYBOT %s - PROFESSIONAL TRADING SYSTEM", VERSION)
	log.Info().Msg("═══════════════════════════════════════════════════════════════")

	// Log key env values for debugging
	log.Debug().
		Str("PAPER_MODE", os.Getenv("PAPER_MODE")).
		Str("DRY_RUN", os.Getenv("DRY_RUN")).
		Str("INITIAL_BALANCE", os.Getenv("INITIAL_BALANCE")).
		Str("MARKET_ORDER_VALUE", os.Getenv("MARKET_ORDER_VALUE")).
		Str("LIMIT_ORDER_SHARES", os.Getenv("LIMIT_ORDER_SHARES")).
		Str("MAX_DAILY_LOSS_PCT", os.Getenv("MAX_DAILY_LOSS_PCT")).
		Str("MAX_POSITION_PCT", os.Getenv("MAX_POSITION_PCT")).
		Str("DATABASE_URL", func() string { 
			if os.Getenv("DATABASE_URL") != "" { 
				return "SET" 
			}
			return "NOT SET"
		}()).
		Msg("📋 Environment variables loaded")

	paperMode := os.Getenv("PAPER_MODE") != "false" // Default to paper mode
	initialBalance := decimal.NewFromFloat(1000)
	if balStr := os.Getenv("INITIAL_BALANCE"); balStr != "" {
		if bal, err := decimal.NewFromString(balStr); err == nil {
			initialBalance = bal
		}
	}

	// ═══════════════════════════════════════════════════════════════════════════════
	// LAYER 1: STORAGE (Persistence)
	// ═══════════════════════════════════════════════════════════════════════════════

	db, err := storage.NewDatabase()
	if err != nil {
		log.Warn().Err(err).Msg("Database unavailable - running without persistence")
		db = nil
	} else {
		log.Info().Msg("✅ Storage layer initialized")
	}

	// ═══════════════════════════════════════════════════════════════════════════════
	// LAYER 2: FEEDS (Market Data)
	// ═══════════════════════════════════════════════════════════════════════════════

	// Binance (fallback price source)
	binanceFeed := feeds.NewBinanceFeed()
	binanceFeed.Start()
	log.Info().Msg("✅ Binance feed initialized")

	// Chainlink (primary - matches Polymarket resolution)
	cmcKey := os.Getenv("CMC_API_KEY")
	chainlinkFeed := feeds.NewChainlinkFeed(cmcKey)
	chainlinkFeed.SetBinanceFallback(binanceFeed)
	chainlinkFeed.Start()
	log.Info().Msg("✅ Chainlink feed initialized")

	// Polymarket (odds data)
	polyFeed := feeds.NewPolymarketFeed()
	log.Info().Msg("✅ Polymarket feed initialized")

	// Window Scanner (15-min crypto window tracker)
	windowScanner := feeds.NewWindowScanner(chainlinkFeed)
	if db != nil {
		windowScanner.SetDatabase(db)
	}
	windowScanner.SetBinanceFeed(binanceFeed)
	windowScanner.SetPolyFeed(polyFeed)
	windowScanner.Start()
	log.Info().Msg("✅ Window scanner initialized")

	// ═══════════════════════════════════════════════════════════════════════════════
	// LAYER 3: RISK GATE (Centralized Approval)
	// ═══════════════════════════════════════════════════════════════════════════════

	riskGate := risk.NewRiskGate(initialBalance)
	
	// Setup circuit breaker callback
	riskGate.OnCircuitTrip(func(reason string) {
		log.Error().Str("reason", reason).Msg("🚨 CIRCUIT BREAKER TRIPPED")
	})
	log.Info().Msg("✅ Risk Gate initialized")

	// Legacy risk manager (for engine compatibility)
	riskMgr := risk.NewManager()

	// ═══════════════════════════════════════════════════════════════════════════════
	// LAYER 4: EXECUTION (Order Management)
	// ═══════════════════════════════════════════════════════════════════════════════

	// CLOB client (raw API access)
	clobClient, err := exec.NewClient()
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize CLOB client")
	}

	// Execution layer with state machine
	execConfig := execution.DefaultExecutorConfig()
	execConfig.PaperMode = paperMode
	executor := execution.NewExecutor(clobClient, execConfig)
	log.Info().Msg("✅ Executor initialized")

	// Reconciler (position persistence & recovery)
	reconciler := execution.NewReconciler(executor, db)
	log.Info().Msg("✅ Reconciler initialized")

	// ═══════════════════════════════════════════════════════════════════════════════
	// STARTUP RECONCILIATION
	// ═══════════════════════════════════════════════════════════════════════════════

	if db != nil && db.IsEnabled() {
		recovered, err := reconciler.RecoverPositions()
		if err != nil {
			log.Error().Err(err).Msg("Position recovery failed")
		} else if recovered > 0 {
			log.Warn().Int("count", recovered).Msg("⚠️ Recovered positions from previous session")
		}

		// Load risk state for today
		riskState, err := reconciler.LoadRiskState()
		if err == nil && riskState != nil {
			riskGate.SetBalance(riskState.Balance)
			log.Info().
				Str("balance", riskState.Balance.StringFixed(2)).
				Str("daily_pnl", riskState.DailyPnL.StringFixed(2)).
				Msg("📥 Risk state loaded from persistence")
		}
	}

	// ═══════════════════════════════════════════════════════════════════════════════
	// LAYER 5: STRATEGY (Alpha Logic)
	// ═══════════════════════════════════════════════════════════════════════════════

	phaseScalper := strategy.NewPhaseScalper(polyFeed, windowScanner, paperMode)
	
	// Wire up professional layers via adapters (breaks circular dependencies)
	riskAdapter := risk.NewRiskGateAdapter(riskGate)
	persisterAdapter := execution.NewReconcilerAdapter(reconciler)
	phaseScalper.SetRiskGate(riskAdapter)
	phaseScalper.SetPersister(persisterAdapter)
	// phaseScalper.SetExecutor(executor) // Enable when ready for live execution
	
	// In LIVE mode, sync balance from exchange
	if !paperMode {
		realBalance, err := clobClient.GetBalance()
		log.Debug().
			Err(err).
			Str("balance", realBalance.StringFixed(2)).
			Bool("is_zero", realBalance.IsZero()).
			Msg("🔍 Checking exchange balance")
		if err == nil && !realBalance.IsZero() {
			phaseScalper.SetBalance(realBalance)
			riskGate.SetBalance(realBalance)
		} else {
			log.Warn().Err(err).Msg("⚠️ Could not sync real balance, using INITIAL_BALANCE")
		}
	}
	
	log.Info().Msg("✅ Phase Scalper initialized (with Risk Gate + Persister)")

	// ═══════════════════════════════════════════════════════════════════════════════
	// LAYER 6: ENGINE (Orchestration)
	// ═══════════════════════════════════════════════════════════════════════════════

	engine := core.NewEngine(polyFeed, clobClient, riskMgr, []strategy.Strategy{}, db)
	log.Info().Msg("✅ Engine initialized")

	// ═══════════════════════════════════════════════════════════════════════════════
	// LAYER 7: NOTIFICATIONS (Telegram)
	// ═══════════════════════════════════════════════════════════════════════════════

	var tgBot *bot.TelegramBot
	if tg, err := bot.NewTelegramBot(engine); err != nil {
		log.Warn().Err(err).Msg("Telegram unavailable")
	} else {
		tgBot = tg
		tgBot.Start()
		engine.SetTradeNotifier(tgBot)
		log.Info().Msg("✅ Telegram initialized")
	}

	// ═══════════════════════════════════════════════════════════════════════════════
	// STATUS BANNER
	// ═══════════════════════════════════════════════════════════════════════════════

	mode := "PAPER"
	if !paperMode {
		mode = "LIVE"
	}
	
	// Use real balance if available, otherwise initial balance
	displayBalance := initialBalance
	if !paperMode {
		if realBal, err := clobClient.GetBalance(); err == nil && !realBal.IsZero() {
			displayBalance = realBal
		}
	}

	log.Info().Msg("")
	log.Info().Msg("╔═══════════════════════════════════════════════════════════════╗")
	log.Info().Msgf("║        POLYBOT %s - PROFESSIONAL TRADING SYSTEM        ║", VERSION)
	log.Info().Msg("╠═══════════════════════════════════════════════════════════════╣")
	log.Info().Msgf("║  Mode:        %-45s ║", mode)
	log.Info().Msgf("║  Balance:     $%-44s ║", displayBalance.StringFixed(2))
	log.Info().Msg("║  Assets:      BTC, ETH, SOL                                   ║")
	log.Info().Msg("║                                                               ║")
	log.Info().Msg("║  ┌─────────────────────────────────────────────────────────┐  ║")
	log.Info().Msg("║  │  ARCHITECTURE                                           │  ║")
	log.Info().Msg("║  │  ✓ Execution Layer   (Order State Machine)              │  ║")
	log.Info().Msg("║  │  ✓ Risk Gate         (Centralized Approval)             │  ║")
	log.Info().Msg("║  │  ✓ Position Persist  (Crash Recovery)                   │  ║")
	log.Info().Msg("║  │  ✓ Reconciliation    (Startup Recovery)                 │  ║")
	log.Info().Msg("║  │  ✓ Graceful Shutdown (Force Close Positions)            │  ║")
	log.Info().Msg("║  └─────────────────────────────────────────────────────────┘  ║")
	log.Info().Msg("║                                                               ║")
	log.Info().Msg("║  ┌─────────────────────────────────────────────────────────┐  ║")
	log.Info().Msg("║  │  PHASE GATES                                            │  ║")
	log.Info().Msg("║  │  🟢 OPENING:   0-3 min   │ Fade ≥6¢ moves               │  ║")
	log.Info().Msg("║  │  🔴 DEAD ZONE: 3-12 min  │ NO TRADING                   │  ║")
	log.Info().Msg("║  │  🟡 CLOSING:   12-14 min │ Fade ≥4¢ panic (70% size)    │  ║")
	log.Info().Msg("║  │  ⚫ FLAT:      14-15 min │ FORCE CLOSE                  │  ║")
	log.Info().Msg("║  └─────────────────────────────────────────────────────────┘  ║")
	log.Info().Msg("║                                                               ║")
	log.Info().Msg("║  ┌─────────────────────────────────────────────────────────┐  ║")
	log.Info().Msg("║  │  RISK CONTROLS                                          │  ║")
	log.Info().Msg("║  │  TP: +2.5¢  │  Timeout: 15s  │  No Stop Loss            │  ║")
	log.Info().Msg("║  │  Impulse: ≥2 consecutive ticks required                 │  ║")
	log.Info().Msg("║  │  Risk Gate: Centralized approval for all trades         │  ║")
	log.Info().Msg("║  │  Daily limit: 3% of balance                             │  ║")
	log.Info().Msg("║  │  Circuit breaker: 3 consecutive losses                  │  ║")
	log.Info().Msg("║  │  Asset disable: After 2 losses per asset                │  ║")
	log.Info().Msg("║  │  Cooldown: 30s after exit                               │  ║")
	log.Info().Msg("║  └─────────────────────────────────────────────────────────┘  ║")
	log.Info().Msg("╚═══════════════════════════════════════════════════════════════╝")
	log.Info().Msg("")

	// ═══════════════════════════════════════════════════════════════════════════════
	// START
	// ═══════════════════════════════════════════════════════════════════════════════

	go engine.Start()
	go phaseScalper.Start()

	// Stats printer
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			// Phase Scalper stats
			stats := phaseScalper.GetPaperStats()
			log.Info().Interface("strategy", stats).Msg("📊 Phase Scalper")
			
			// Risk Gate stats
			riskStats := riskGate.GetStats()
			log.Info().Interface("risk", riskStats).Msg("🛡️ Risk Gate")
			
			// Execution stats
			execStats := executor.GetMetrics()
			log.Info().Interface("execution", execStats).Msg("⚡ Executor")
		}
	}()

	// Periodic state persistence
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if db != nil && db.IsEnabled() {
				// Save risk state
				stats := riskGate.GetStats()
				balance, _ := decimal.NewFromString(stats["balance"].(string))
				dailyPnL, _ := decimal.NewFromString(stats["daily_pnl"].(string))
				consecLosses := stats["consecutive_losses"].(int)
				circuitTripped := stats["circuit_tripped"].(bool)
				disabledAssets := stats["disabled_assets"].(map[string]bool)
				
				if err := reconciler.SaveRiskState(balance, dailyPnL, consecLosses, circuitTripped, disabledAssets); err != nil {
					log.Warn().Err(err).Msg("Failed to persist risk state")
				}
			}
		}
	}()

	log.Info().Msg("🚀 Running...")

	if tgBot != nil {
		tgBot.NotifyStartup(mode)
	}

	// ═══════════════════════════════════════════════════════════════════════════════
	// GRACEFUL SHUTDOWN
	// ═══════════════════════════════════════════════════════════════════════════════

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Warn().Msg("🛑 Shutdown signal received...")
	log.Info().Msg("")
	log.Info().Msg("═══════════════════════════════════════════════════════════════")
	log.Info().Msg("                    GRACEFUL SHUTDOWN")
	log.Info().Msg("═══════════════════════════════════════════════════════════════")

	// 1. Stop Phase Scalper (this force-closes positions)
	log.Info().Msg("Stopping Phase Scalper (closing positions)...")
	phaseScalper.Stop()

	// 2. Final risk state persistence
	if db != nil && db.IsEnabled() {
		stats := riskGate.GetStats()
		balance, _ := decimal.NewFromString(stats["balance"].(string))
		dailyPnL, _ := decimal.NewFromString(stats["daily_pnl"].(string))
		consecLosses := stats["consecutive_losses"].(int)
		circuitTripped := stats["circuit_tripped"].(bool)
		disabledAssets := stats["disabled_assets"].(map[string]bool)
		
		if err := reconciler.SaveRiskState(balance, dailyPnL, consecLosses, circuitTripped, disabledAssets); err != nil {
			log.Warn().Err(err).Msg("Failed to persist final risk state")
		} else {
			log.Info().Msg("✅ Risk state persisted")
		}
	}

	// 3. Stop other components
	log.Info().Msg("Stopping feeds...")
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

	log.Info().Msg("")
	log.Info().Msg("═══════════════════════════════════════════════════════════════")
	log.Info().Msg("                       SHUTDOWN COMPLETE")
	log.Info().Msg("═══════════════════════════════════════════════════════════════")
	log.Info().Msg("👋 Goodbye!")
}
