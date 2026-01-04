package predictor

import (
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"

	"github.com/web3guy0/polybot/internal/binance"
	"github.com/web3guy0/polybot/internal/config"
	"github.com/web3guy0/polybot/internal/indicators"
)

// Direction represents predicted price direction
type Direction string

const (
	DirectionUp   Direction = "UP"
	DirectionDown Direction = "DOWN"
	DirectionSkip Direction = "SKIP"
)

// Signal represents a combined trading signal
type Signal struct {
	Direction   Direction
	Confidence  float64 // 0-100
	TotalScore  float64 // -100 to +100
	
	// Component scores
	Momentum      float64 // -30 to +30
	RSI           float64 // -20 to +20
	VolumeSignal  float64 // -15 to +15
	OrderBook     float64 // -20 to +20
	FundingRate   float64 // -15 to +15
	BuySellRatio  float64 // -15 to +15
	TrendStrength float64 // Additional context
	
	// Market data
	CurrentPrice decimal.Decimal
	PriceChange1m decimal.Decimal
	PriceChange5m decimal.Decimal
	RSIValue     float64
	
	// Metadata
	Timestamp time.Time
	MarketID  string
}

// Predictor analyzes market data and generates signals
// IMPORTANT: This is a READ-ONLY component - it ONLY generates signals/alerts
// It does NOT execute trades. Trade execution is handled by trading/engine.go
// This separation ensures analysis and execution are decoupled.
type Predictor struct {
	cfg           *config.Config
	binanceClient *binance.Client
	
	// Signal history
	signals     []Signal
	signalMu    sync.RWMutex
	
	// Callbacks
	onSignal func(Signal)
	
	running bool
	stopCh  chan struct{}
}

// NewPredictor creates a new predictor
func NewPredictor(cfg *config.Config, binanceClient *binance.Client) *Predictor {
	return &Predictor{
		cfg:           cfg,
		binanceClient: binanceClient,
		signals:       make([]Signal, 0, 100),
		stopCh:        make(chan struct{}),
	}
}

// SetSignalCallback sets callback for new signals
func (p *Predictor) SetSignalCallback(cb func(Signal)) {
	p.onSignal = cb
}

// Start begins the prediction loop
func (p *Predictor) Start() {
	p.running = true
	go p.predictionLoop()
	log.Info().Msg("🧠 Predictor started")
}

// Stop stops the predictor
func (p *Predictor) Stop() {
	p.running = false
	close(p.stopCh)
}

func (p *Predictor) predictionLoop() {
	// Generate signals every 10 seconds
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			signal := p.GenerateSignal()
			
			// Store signal
			p.signalMu.Lock()
			p.signals = append(p.signals, signal)
			if len(p.signals) > 100 {
				p.signals = p.signals[1:]
			}
			p.signalMu.Unlock()
			
			// Callback if significant signal
			if signal.Direction != DirectionSkip && p.onSignal != nil {
				p.onSignal(signal)
			}
			
			log.Debug().
				Str("direction", string(signal.Direction)).
				Float64("confidence", signal.Confidence).
				Float64("score", signal.TotalScore).
				Msg("Signal generated")
				
		case <-p.stopCh:
			return
		}
	}
}

// GenerateSignal analyzes current market and generates a signal
func (p *Predictor) GenerateSignal() Signal {
	signal := Signal{
		Timestamp:    time.Now(),
		CurrentPrice: p.binanceClient.GetCurrentPrice(),
	}
	
	// Get price data
	prices := p.binanceClient.GetPrices(100)
	if len(prices) < 20 {
		signal.Direction = DirectionSkip
		return signal
	}
	
	// Calculate price changes
	_, change1m := p.binanceClient.GetPriceChange(1 * time.Minute)
	_, change5m := p.binanceClient.GetPriceChange(5 * time.Minute)
	signal.PriceChange1m = change1m
	signal.PriceChange5m = change5m
	
	// Get current direction for volume analysis
	priceDirection := 0.0
	if len(prices) >= 2 {
		priceDirection = prices[len(prices)-1] - prices[len(prices)-2]
	}
	
	// 1. Momentum Score (-30 to +30)
	signal.Momentum = indicators.MomentumScore(prices, 10)
	
	// 2. RSI Score (-20 to +20)
	signal.RSIValue = indicators.RSI(prices, 14)
	signal.RSI = indicators.RSIScore(signal.RSIValue)
	
	// 3. Volume Score (-15 to +15)
	volumes := p.binanceClient.GetVolumes(30)
	if len(volumes) > 0 {
		currentVol := volumes[len(volumes)-1]
		avgVol := average(volumes)
		signal.VolumeSignal = indicators.VolumeScore(currentVol, avgVol, priceDirection)
	}
	
	// 4. Order Book Imbalance (-20 to +20)
	orderBook, err := p.binanceClient.GetOrderBook("BTCUSDT", 20)
	if err == nil && orderBook != nil {
		bidVol := 0.0
		askVol := 0.0
		for _, b := range orderBook.Bids {
			f, _ := b.Quantity.Float64()
			bidVol += f
		}
		for _, a := range orderBook.Asks {
			f, _ := a.Quantity.Float64()
			askVol += f
		}
		signal.OrderBook = indicators.OrderBookImbalanceScore(bidVol, askVol)
	}
	
	// 5. Funding Rate Score (-15 to +15)
	fundingRate, err := p.binanceClient.GetFundingRate("BTCUSDT")
	if err == nil {
		f, _ := fundingRate.Float64()
		signal.FundingRate = indicators.FundingRateScore(f)
	}
	
	// 6. Buy/Sell Ratio (-15 to +15)
	buySellRatio := p.binanceClient.GetBuySellRatio(1 * time.Minute)
	if !buySellRatio.IsZero() {
		r, _ := buySellRatio.Float64()
		signal.BuySellRatio = indicators.BuySellRatioScore(r, 1.0)
	}
	
	// 7. Trend Strength (additional context)
	signal.TrendStrength = indicators.TrendStrength(prices, 20)
	
	// Calculate total score
	signal.TotalScore = signal.Momentum + signal.RSI + signal.VolumeSignal + 
		signal.OrderBook + signal.FundingRate + signal.BuySellRatio
	
	// Apply trend strength modifier
	// Strong trends get a boost, weak trends get dampened
	trendMultiplier := 1.0
	if absFloat(signal.TrendStrength) > 70 {
		trendMultiplier = 1.15
	} else if absFloat(signal.TrendStrength) < 30 {
		trendMultiplier = 0.85
	}
	signal.TotalScore *= trendMultiplier
	
	// Determine direction
	minScore := p.cfg.BTCMinSignalScore
	if minScore == 0 {
		minScore = 25 // Default threshold
	}
	
	if signal.TotalScore >= float64(minScore) {
		signal.Direction = DirectionUp
	} else if signal.TotalScore <= -float64(minScore) {
		signal.Direction = DirectionDown
	} else {
		signal.Direction = DirectionSkip
	}
	
	// Calculate confidence (0-100 based on score magnitude)
	signal.Confidence = absFloat(signal.TotalScore)
	if signal.Confidence > 100 {
		signal.Confidence = 100
	}
	
	return signal
}

// GetCurrentSignal returns the most recent signal
func (p *Predictor) GetCurrentSignal() Signal {
	p.signalMu.RLock()
	defer p.signalMu.RUnlock()
	
	if len(p.signals) == 0 {
		return Signal{Direction: DirectionSkip}
	}
	return p.signals[len(p.signals)-1]
}

// GetSignalHistory returns recent signals
func (p *Predictor) GetSignalHistory(count int) []Signal {
	p.signalMu.RLock()
	defer p.signalMu.RUnlock()
	
	if len(p.signals) <= count {
		result := make([]Signal, len(p.signals))
		copy(result, p.signals)
		return result
	}
	
	result := make([]Signal, count)
	copy(result, p.signals[len(p.signals)-count:])
	return result
}

// GetAccuracyStats calculates prediction accuracy
func (p *Predictor) GetAccuracyStats() (total, correct int, accuracy float64) {
	p.signalMu.RLock()
	defer p.signalMu.RUnlock()
	
	// This would need actual outcome tracking
	// For now, return placeholder
	return 0, 0, 0
}

// FormatSignal formats a signal for display
func (p *Predictor) FormatSignal(s Signal) string {
	dirEmoji := "⏸️"
	if s.Direction == DirectionUp {
		dirEmoji = "🟢"
	} else if s.Direction == DirectionDown {
		dirEmoji = "🔴"
	}
	
	return fmt.Sprintf(`%s *BTC Signal: %s*
	
📊 *Score: %.1f* (Confidence: %.0f%%)

*Components:*
├ Momentum: %+.1f
├ RSI (%.0f): %+.1f
├ Volume: %+.1f
├ Order Book: %+.1f
├ Funding: %+.1f
└ Buy/Sell: %+.1f

💰 Price: $%s
📈 1m: %s%% | 5m: %s%%`,
		dirEmoji, s.Direction,
		s.TotalScore, s.Confidence,
		s.Momentum,
		s.RSIValue, s.RSI,
		s.VolumeSignal,
		s.OrderBook,
		s.FundingRate,
		s.BuySellRatio,
		s.CurrentPrice.StringFixed(2),
		s.PriceChange1m.StringFixed(3),
		s.PriceChange5m.StringFixed(3),
	)
}

// QuickSignal generates a signal immediately (non-blocking)
func (p *Predictor) QuickSignal() Signal {
	return p.GenerateSignal()
}

// Helper functions

func average(data []float64) float64 {
	if len(data) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range data {
		sum += v
	}
	return sum / float64(len(data))
}

func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
