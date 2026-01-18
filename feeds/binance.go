package feeds

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ═══════════════════════════════════════════════════════════════════════════════
// BINANCE PRICE FEED - Real-time BTC/ETH/SOL prices
// ═══════════════════════════════════════════════════════════════════════════════
//
// Used for:
//   - Calculating price movement from "price to beat"
//   - Confirming direction for sniper entries
//
// ═══════════════════════════════════════════════════════════════════════════════

const (
	binanceAPIURL   = "https://api.binance.com/api/v3/ticker/price"
	binanceInterval = 200 * time.Millisecond // 200ms for fast detection
)

// BinanceFeed provides real-time crypto prices
type BinanceFeed struct {
	mu      sync.RWMutex
	running bool
	stopCh  chan struct{}

	// Current prices
	prices map[string]decimal.Decimal // "BTCUSDT" -> price

	// Subscribers
	subscribers []chan PriceUpdate
}

// PriceUpdate represents a price change event
type PriceUpdate struct {
	Symbol    string
	Price     decimal.Decimal
	Timestamp time.Time
}

// NewBinanceFeed creates a new Binance feed
func NewBinanceFeed() *BinanceFeed {
	return &BinanceFeed{
		stopCh:      make(chan struct{}),
		prices:      make(map[string]decimal.Decimal),
		subscribers: make([]chan PriceUpdate, 0),
	}
}

// Start begins polling Binance for prices
func (f *BinanceFeed) Start() {
	f.mu.Lock()
	if f.running {
		f.mu.Unlock()
		return
	}
	f.running = true
	f.mu.Unlock()

	go f.pollLoop()
	log.Info().Dur("interval", binanceInterval).Msg("📈 Binance feed started")
}

// Stop stops the feed
func (f *BinanceFeed) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.running {
		return
	}

	f.running = false
	close(f.stopCh)
	log.Info().Msg("Binance feed stopped")
}

// Subscribe returns a channel for price updates
func (f *BinanceFeed) Subscribe() chan PriceUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()

	ch := make(chan PriceUpdate, 100)
	f.subscribers = append(f.subscribers, ch)
	return ch
}

// GetPrice returns the current price for a symbol
func (f *BinanceFeed) GetPrice(symbol string) decimal.Decimal {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.prices[symbol]
}

// GetPrices returns all current prices
func (f *BinanceFeed) GetPrices() map[string]decimal.Decimal {
	f.mu.RLock()
	defer f.mu.RUnlock()

	result := make(map[string]decimal.Decimal)
	for k, v := range f.prices {
		result[k] = v
	}
	return result
}

// pollLoop continuously fetches prices
func (f *BinanceFeed) pollLoop() {
	symbols := []string{"BTCUSDT", "ETHUSDT", "SOLUSDT"}

	ticker := time.NewTicker(binanceInterval)
	defer ticker.Stop()

	// Initial fetch
	f.fetchPrices(symbols)

	for {
		select {
		case <-f.stopCh:
			return
		case <-ticker.C:
			f.fetchPrices(symbols)
		}
	}
}

// fetchPrices gets current prices from Binance
func (f *BinanceFeed) fetchPrices(symbols []string) {
	for _, symbol := range symbols {
		price, err := f.fetchPrice(symbol)
		if err != nil {
			continue
		}

		f.mu.Lock()
		oldPrice := f.prices[symbol]
		f.prices[symbol] = price
		f.mu.Unlock()

		// Only broadcast if price changed
		if !price.Equal(oldPrice) {
			update := PriceUpdate{
				Symbol:    symbol,
				Price:     price,
				Timestamp: time.Now(),
			}
			f.broadcast(update)
		}
	}
}

// fetchPrice gets a single price from Binance
func (f *BinanceFeed) fetchPrice(symbol string) (decimal.Decimal, error) {
	url := fmt.Sprintf("%s?symbol=%s", binanceAPIURL, symbol)

	resp, err := http.Get(url)
	if err != nil {
		return decimal.Zero, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return decimal.Zero, err
	}

	var result struct {
		Price string `json:"price"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return decimal.Zero, err
	}

	return decimal.NewFromString(result.Price)
}

// broadcast sends update to all subscribers
func (f *BinanceFeed) broadcast(update PriceUpdate) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	for _, ch := range f.subscribers {
		select {
		case ch <- update:
		default:
			// Channel full, skip
		}
	}
}
