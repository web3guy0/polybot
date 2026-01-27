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
// CHAINLINK PRICE FEED - On-chain oracle prices
// ═══════════════════════════════════════════════════════════════════════════════
//
// Polymarket uses Chainlink Data Streams for price resolution.
// We use Chainlink's public on-chain price feeds which are close enough
// and free to access via public RPC or aggregator APIs.
//
// Sources:
//   1. Chainlink Price Feeds API (primary)
//   2. CoinMarketCap API (fallback)
//   3. Binance (last resort fallback)
//
// ═══════════════════════════════════════════════════════════════════════════════

const (
	// Chainlink feeds via public aggregators
	chainlinkAPIURL = "https://api.chain.link/v1/query"
	
	// CMC API (free tier)
	cmcAPIURL = "https://pro-api.coinmarketcap.com/v1/cryptocurrency/quotes/latest"
	
	// Backup: CryptoCompare (no key needed)
	cryptoCompareURL = "https://min-api.cryptocompare.com/data/pricemultifull"
	
	// Polling interval - 100ms for speed
	chainlinkInterval = 100 * time.Millisecond
)

// ChainlinkFeed provides Chainlink-aligned crypto prices
type ChainlinkFeed struct {
	mu      sync.RWMutex
	running bool
	stopCh  chan struct{}

	// Current prices
	prices map[string]decimal.Decimal // "BTC" -> price

	// CMC API key (optional, from env)
	cmcAPIKey string

	// Fallback to Binance
	binanceFallback *BinanceFeed

	// Subscribers
	subscribers []chan PriceUpdate
}

// NewChainlinkFeed creates a new Chainlink-aligned price feed
func NewChainlinkFeed(cmcAPIKey string) *ChainlinkFeed {
	return &ChainlinkFeed{
		stopCh:    make(chan struct{}),
		prices:    make(map[string]decimal.Decimal),
		cmcAPIKey: cmcAPIKey,
	}
}

// SetBinanceFallback sets Binance as fallback
func (f *ChainlinkFeed) SetBinanceFallback(bf *BinanceFeed) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.binanceFallback = bf
}

// Start begins polling for prices
func (f *ChainlinkFeed) Start() {
	f.mu.Lock()
	if f.running {
		f.mu.Unlock()
		return
	}
	f.running = true
	f.mu.Unlock()

	go f.pollLoop()
	log.Info().Msg("⛓️ Chainlink price feed started")
}

// Stop stops the feed
func (f *ChainlinkFeed) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.running {
		return
	}

	f.running = false
	close(f.stopCh)
	log.Info().Msg("Chainlink feed stopped")
}

// GetPrice returns the current price for an asset
func (f *ChainlinkFeed) GetPrice(asset string) decimal.Decimal {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.prices[asset]
}

// GetPrices returns all current prices
func (f *ChainlinkFeed) GetPrices() map[string]decimal.Decimal {
	f.mu.RLock()
	defer f.mu.RUnlock()

	result := make(map[string]decimal.Decimal)
	for k, v := range f.prices {
		result[k] = v
	}
	return result
}

// pollLoop continuously fetches prices
func (f *ChainlinkFeed) pollLoop() {
	assets := []string{"BTC", "ETH", "SOL"}

	ticker := time.NewTicker(chainlinkInterval)
	defer ticker.Stop()

	// Initial fetch
	f.fetchPrices(assets)

	for {
		select {
		case <-f.stopCh:
			return
		case <-ticker.C:
			f.fetchPrices(assets)
		}
	}
}

// fetchPrices gets prices from available sources
func (f *ChainlinkFeed) fetchPrices(assets []string) {
	// Try CryptoCompare first (free, reliable, close to Chainlink)
	if f.fetchFromCryptoCompare(assets) {
		return
	}

	// Try CMC if key available
	if f.cmcAPIKey != "" && f.fetchFromCMC(assets) {
		return
	}

	// Fallback to Binance
	f.fetchFromBinanceFallback(assets)
}

// fetchFromCryptoCompare gets prices from CryptoCompare
func (f *ChainlinkFeed) fetchFromCryptoCompare(assets []string) bool {
	// Build request: BTC,ETH,SOL -> USD
	symbols := ""
	for i, a := range assets {
		if i > 0 {
			symbols += ","
		}
		symbols += a
	}

	url := fmt.Sprintf("%s?fsyms=%s&tsyms=USD", cryptoCompareURL, symbols)

	resp, err := http.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return false
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}

	// Parse response
	var result struct {
		RAW map[string]struct {
			USD struct {
				PRICE float64 `json:"PRICE"`
			} `json:"USD"`
		} `json:"RAW"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return false
	}

	f.mu.Lock()
	for _, asset := range assets {
		if data, ok := result.RAW[asset]; ok {
			newPrice := decimal.NewFromFloat(data.USD.PRICE)
			oldPrice := f.prices[asset]
			f.prices[asset] = newPrice

			// Log significant changes
			if !oldPrice.IsZero() {
				diff := newPrice.Sub(oldPrice).Div(oldPrice).Mul(decimal.NewFromInt(100)).Abs()
				if diff.GreaterThan(decimal.NewFromFloat(0.01)) {
					log.Debug().
						Str("asset", asset).
						Str("price", newPrice.StringFixed(2)).
						Msg("Price update")
				}
			}
		}
	}
	f.mu.Unlock()

	return true
}

// fetchFromCMC gets prices from CoinMarketCap
func (f *ChainlinkFeed) fetchFromCMC(assets []string) bool {
	if f.cmcAPIKey == "" {
		return false
	}

	// Build symbol list
	symbols := ""
	for i, a := range assets {
		if i > 0 {
			symbols += ","
		}
		symbols += a
	}

	url := fmt.Sprintf("%s?symbol=%s", cmcAPIURL, symbols)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("X-CMC_PRO_API_KEY", f.cmcAPIKey)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return false
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}

	// Parse CMC response
	var result struct {
		Data map[string]struct {
			Quote struct {
				USD struct {
					Price float64 `json:"price"`
				} `json:"USD"`
			} `json:"quote"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return false
	}

	f.mu.Lock()
	for _, asset := range assets {
		if data, ok := result.Data[asset]; ok {
			f.prices[asset] = decimal.NewFromFloat(data.Quote.USD.Price)
		}
	}
	f.mu.Unlock()

	return true
}

// fetchFromBinanceFallback uses Binance as last resort
func (f *ChainlinkFeed) fetchFromBinanceFallback(assets []string) {
	f.mu.RLock()
	bf := f.binanceFallback
	f.mu.RUnlock()

	if bf == nil {
		return
	}

	f.mu.Lock()
	for _, asset := range assets {
		symbol := asset + "USDT"
		price := bf.GetPrice(symbol)
		if !price.IsZero() {
			f.prices[asset] = price
		}
	}
	f.mu.Unlock()
}

// Subscribe returns a channel for price updates
func (f *ChainlinkFeed) Subscribe() chan PriceUpdate {
	f.mu.Lock()
	defer f.mu.Unlock()

	ch := make(chan PriceUpdate, 100)
	f.subscribers = append(f.subscribers, ch)
	return ch
}
