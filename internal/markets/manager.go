package markets

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	
	"github.com/web3guy0/polybot/internal/risk"
	"github.com/web3guy0/polybot/internal/strategy"
)

// MarketManager handles all markets in a config-driven way
// Markets are defined in config, not code
type MarketManager struct {
	markets     map[string]*ManagedMarket
	strategies  map[string]strategy.Strategy
	riskManager *risk.RiskManager
	dataFeeds   map[string]DataFeed
	executor    TradeExecutor
	
	bankroll    decimal.Decimal
	
	signalCh    chan MarketSignal
	tradeCh     chan TradeResult
	stopCh      chan struct{}
	mu          sync.RWMutex
}

// MarketConfig defines a market to trade (from config file)
type MarketConfig struct {
	ID           string        `json:"id"`
	Asset        string        `json:"asset"`
	Timeframe    time.Duration `json:"timeframe"`
	StrategyName string        `json:"strategy"`
	MaxBet       float64       `json:"max_bet"`
	Enabled      bool          `json:"enabled"`
	
	// Optional overrides
	MinConfidence *float64 `json:"min_confidence,omitempty"`
	MinOdds       *float64 `json:"min_odds,omitempty"`
	MaxOdds       *float64 `json:"max_odds,omitempty"`
}

// ManagedMarket represents an actively managed market
type ManagedMarket struct {
	Config      MarketConfig
	Strategy    strategy.Strategy
	LastSignal  *strategy.Signal
	LastUpdate  time.Time
	IsActive    bool
	
	// Stats
	Trades      int
	Wins        int
	PnL         decimal.Decimal
}

// MarketSignal bundles a signal with its market
type MarketSignal struct {
	MarketID string
	Signal   strategy.Signal
	Context  strategy.MarketContext
}

// TradeResult represents the outcome of a trade
type TradeResult struct {
	MarketID  string
	Success   bool
	PnL       decimal.Decimal
	Error     error
}

// DataFeed provides market data for a specific asset
type DataFeed interface {
	GetPrices(asset string, count int) []float64
	GetVolumes(asset string, count int) []float64
	GetOrderBook(asset string) *strategy.OrderBookData
	GetFundingRate(asset string) float64
	GetCurrentPrice(asset string) float64
}

// TradeExecutor handles actual trade execution
type TradeExecutor interface {
	ExecuteTrade(ctx context.Context, marketID string, direction strategy.Direction, size decimal.Decimal) error
	GetMarketInfo(marketID string) (*MarketInfo, error)
}

// MarketInfo contains current market state
type MarketInfo struct {
	ID        string
	YesPrice  float64
	NoPrice   float64
	Liquidity float64
	ExpiresAt time.Time
}

// NewMarketManager creates a new market manager
func NewMarketManager(riskMgr *risk.RiskManager, bankroll decimal.Decimal) *MarketManager {
	return &MarketManager{
		markets:     make(map[string]*ManagedMarket),
		strategies:  make(map[string]strategy.Strategy),
		riskManager: riskMgr,
		dataFeeds:   make(map[string]DataFeed),
		bankroll:    bankroll,
		signalCh:    make(chan MarketSignal, 100),
		tradeCh:     make(chan TradeResult, 100),
		stopCh:      make(chan struct{}),
	}
}

// RegisterStrategy registers a strategy for use
func (m *MarketManager) RegisterStrategy(s strategy.Strategy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.strategies[s.Name()] = s
	log.Info().Str("strategy", s.Name()).Msg("📊 Registered strategy")
}

// RegisterDataFeed registers a data feed for an asset
func (m *MarketManager) RegisterDataFeed(asset string, feed DataFeed) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dataFeeds[asset] = feed
	log.Info().Str("asset", asset).Msg("📡 Registered data feed")
}

// SetExecutor sets the trade executor
func (m *MarketManager) SetExecutor(executor TradeExecutor) {
	m.executor = executor
}

// AddMarket adds a market from config
func (m *MarketManager) AddMarket(config MarketConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	// Find strategy
	strat, ok := m.strategies[config.StrategyName]
	if !ok {
		return fmt.Errorf("strategy %s not found", config.StrategyName)
	}
	
	// Verify data feed exists
	if _, ok := m.dataFeeds[config.Asset]; !ok {
		return fmt.Errorf("no data feed for asset %s", config.Asset)
	}
	
	market := &ManagedMarket{
		Config:   config,
		Strategy: strat,
		IsActive: config.Enabled,
	}
	
	m.markets[config.ID] = market
	
	log.Info().
		Str("id", config.ID).
		Str("asset", config.Asset).
		Str("strategy", config.StrategyName).
		Bool("enabled", config.Enabled).
		Msg("🎯 Added market")
	
	return nil
}

// LoadMarkets loads markets from config
func (m *MarketManager) LoadMarkets(configs []MarketConfig) error {
	for _, cfg := range configs {
		if err := m.AddMarket(cfg); err != nil {
			log.Warn().Err(err).Str("id", cfg.ID).Msg("Failed to add market")
		}
	}
	return nil
}

// Start begins market monitoring
func (m *MarketManager) Start(ctx context.Context) {
	log.Info().Msg("🚀 Market manager starting...")
	
	// Start signal processor
	go m.processSignals(ctx)
	
	// Start trade result processor
	go m.processResults(ctx)
	
	// Start market evaluation loops
	m.mu.RLock()
	for id, market := range m.markets {
		if market.IsActive {
			go m.evaluateMarketLoop(ctx, id)
		}
	}
	m.mu.RUnlock()
}

// evaluateMarketLoop continuously evaluates a market
func (m *MarketManager) evaluateMarketLoop(ctx context.Context, marketID string) {
	m.mu.RLock()
	market, ok := m.markets[marketID]
	m.mu.RUnlock()
	
	if !ok {
		return
	}
	
	// Evaluate every 10 seconds for 15m windows
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	
	log.Info().Str("market", marketID).Msg("📈 Starting market evaluation loop")
	
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.evaluateMarket(ctx, market)
		}
	}
}

// evaluateMarket runs strategy evaluation for a market
func (m *MarketManager) evaluateMarket(ctx context.Context, market *ManagedMarket) {
	// Build market context
	marketCtx := m.buildMarketContext(market)
	
	// Run strategy
	signal, err := market.Strategy.Evaluate(ctx, marketCtx)
	if err != nil {
		log.Error().Err(err).Str("market", market.Config.ID).Msg("Strategy evaluation failed")
		return
	}
	
	// Update last signal
	m.mu.Lock()
	market.LastSignal = &signal
	market.LastUpdate = time.Now()
	m.mu.Unlock()
	
	// Only emit tradeable signals
	if signal.IsTradeable(market.Strategy.MinConfidence()) {
		m.signalCh <- MarketSignal{
			MarketID: market.Config.ID,
			Signal:   signal,
			Context:  marketCtx,
		}
	}
}

// buildMarketContext creates market context from data feeds
func (m *MarketManager) buildMarketContext(market *ManagedMarket) strategy.MarketContext {
	asset := market.Config.Asset
	
	m.mu.RLock()
	feed, ok := m.dataFeeds[asset]
	m.mu.RUnlock()
	
	if !ok {
		return strategy.MarketContext{Asset: asset}
	}
	
	ctx := strategy.MarketContext{
		Asset:       asset,
		Timeframe:   market.Config.Timeframe,
		CurrentPrice: feed.GetCurrentPrice(asset),
		Prices:      feed.GetPrices(asset, 100),
		Volumes:     feed.GetVolumes(asset, 100),
		OrderBook:   feed.GetOrderBook(asset),
		FundingRate: feed.GetFundingRate(asset),
		MarketID:    market.Config.ID,
	}
	
	// Get market info from executor if available
	if m.executor != nil {
		if info, err := m.executor.GetMarketInfo(market.Config.ID); err == nil && info != nil {
			ctx.YesPrice = info.YesPrice
			ctx.NoPrice = info.NoPrice
			ctx.Liquidity = info.Liquidity
			ctx.WindowEnd = info.ExpiresAt
		}
	}
	
	return ctx
}

// processSignals handles incoming signals
func (m *MarketManager) processSignals(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case sig := <-m.signalCh:
			m.handleSignal(ctx, sig)
		}
	}
}

// handleSignal processes a signal through risk management
func (m *MarketManager) handleSignal(ctx context.Context, sig MarketSignal) {
	log.Info().
		Str("market", sig.MarketID).
		Str("direction", string(sig.Signal.Direction)).
		Float64("confidence", sig.Signal.Confidence).
		Str("strength", string(sig.Signal.Strength)).
		Msg("📊 Signal received")
	
	// Check with risk manager
	decision := m.riskManager.CanTrade(sig.Signal, sig.Context, m.bankroll)
	
	if !decision.Allowed {
		log.Info().
			Str("market", sig.MarketID).
			Str("reason", decision.Reason).
			Msg("❌ Trade rejected by risk manager")
		return
	}
	
	// Log warnings
	for _, warn := range decision.Warnings {
		log.Warn().Str("market", sig.MarketID).Msg(warn)
	}
	
	// Execute trade
	if m.executor != nil {
		log.Info().
			Str("market", sig.MarketID).
			Str("direction", string(sig.Signal.Direction)).
			Str("size", decision.SuggestedSize.String()).
			Msg("🚀 Executing trade")
		
		err := m.executor.ExecuteTrade(ctx, sig.MarketID, sig.Signal.Direction, decision.SuggestedSize)
		if err != nil {
			log.Error().Err(err).Str("market", sig.MarketID).Msg("Trade execution failed")
			return
		}
		
		// Record trade with risk manager
		m.riskManager.RecordTrade(decision.SuggestedSize)
		
		// Update market stats
		m.mu.Lock()
		if market, ok := m.markets[sig.MarketID]; ok {
			market.Trades++
		}
		m.mu.Unlock()
	}
}

// processResults handles trade results
func (m *MarketManager) processResults(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stopCh:
			return
		case result := <-m.tradeCh:
			m.handleResult(result)
		}
	}
}

// handleResult processes a trade result
func (m *MarketManager) handleResult(result TradeResult) {
	// Update risk manager
	m.riskManager.RecordResult(result.PnL)
	
	// Update market stats
	m.mu.Lock()
	if market, ok := m.markets[result.MarketID]; ok {
		market.PnL = market.PnL.Add(result.PnL)
		if result.PnL.IsPositive() {
			market.Wins++
		}
	}
	m.mu.Unlock()
	
	log.Info().
		Str("market", result.MarketID).
		Str("pnl", result.PnL.String()).
		Bool("success", result.Success).
		Msg("📈 Trade result recorded")
}

// Stop stops the market manager
func (m *MarketManager) Stop() {
	close(m.stopCh)
}

// GetSignalChannel returns the signal channel for external consumers
func (m *MarketManager) GetSignalChannel() <-chan MarketSignal {
	return m.signalCh
}

// RecordResult allows external components to report trade results
func (m *MarketManager) RecordResult(result TradeResult) {
	m.tradeCh <- result
}

// GetMarketStats returns stats for all markets
func (m *MarketManager) GetMarketStats() map[string]MarketStats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	
	stats := make(map[string]MarketStats)
	for id, market := range m.markets {
		stats[id] = MarketStats{
			ID:         id,
			Asset:      market.Config.Asset,
			Strategy:   market.Config.StrategyName,
			IsActive:   market.IsActive,
			Trades:     market.Trades,
			Wins:       market.Wins,
			PnL:        market.PnL,
			WinRate:    safeWinRate(market.Wins, market.Trades),
			LastSignal: market.LastSignal,
			LastUpdate: market.LastUpdate,
		}
	}
	return stats
}

// MarketStats is a snapshot of market statistics
type MarketStats struct {
	ID         string
	Asset      string
	Strategy   string
	IsActive   bool
	Trades     int
	Wins       int
	PnL        decimal.Decimal
	WinRate    float64
	LastSignal *strategy.Signal
	LastUpdate time.Time
}

func safeWinRate(wins, trades int) float64 {
	if trades == 0 {
		return 0
	}
	return float64(wins) / float64(trades) * 100
}
