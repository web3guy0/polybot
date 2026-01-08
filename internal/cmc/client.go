// Package cmc provides CoinMarketCap price feed
// CMC uses the same Chainlink aggregated data as Polymarket Data Streams
// Updates faster than Chainlink on-chain (~1s vs ~30s)
package cmc

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// CMCResponse represents the API response
type CMCResponse struct {
	Data []struct {
		Symbol string `json:"symbol"`
		Quotes []struct {
			Price float64 `json:"price"`
		} `json:"quotes"`
	} `json:"data"`
}

// Client fetches BTC price from CoinMarketCap
type Client struct {
	httpClient   *http.Client
	currentPrice decimal.Decimal
	lastUpdate   time.Time
	mu           sync.RWMutex
	stopCh       chan struct{}
}

// NewClient creates a new CMC client
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 2 * time.Second},
		stopCh:     make(chan struct{}),
	}
}

// Start begins polling CMC for price updates
func (c *Client) Start() {
	// Fetch initial price
	c.fetchPrice()

	// Poll every 1 second for fast updates
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				c.fetchPrice()
			case <-c.stopCh:
				return
			}
		}
	}()

	log.Info().Msg("📊 CMC client started (1s polling)")
}

// Stop stops the client
func (c *Client) Stop() {
	close(c.stopCh)
}

// fetchPrice gets current BTC price from CMC
func (c *Client) fetchPrice() {
	url := "https://api.coinmarketcap.com/data-api/v3/cryptocurrency/quote/latest?slug=bitcoin&convertId=2781"
	
	resp, err := c.httpClient.Get(url)
	if err != nil {
		log.Debug().Err(err).Msg("CMC fetch failed")
		return
	}
	defer resp.Body.Close()

	var data CMCResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		log.Debug().Err(err).Msg("CMC parse failed")
		return
	}

	if len(data.Data) > 0 && len(data.Data[0].Quotes) > 0 {
		price := decimal.NewFromFloat(data.Data[0].Quotes[0].Price)
		
		c.mu.Lock()
		oldPrice := c.currentPrice
		c.currentPrice = price
		c.lastUpdate = time.Now()
		c.mu.Unlock()

		// Log significant changes
		if !oldPrice.IsZero() {
			diff := price.Sub(oldPrice).Abs()
			if diff.GreaterThan(decimal.NewFromFloat(10)) {
				log.Debug().
					Str("price", price.StringFixed(2)).
					Str("change", diff.StringFixed(2)).
					Msg("📊 CMC price update")
			}
		}
	}
}

// GetCurrentPrice returns the latest BTC price
func (c *Client) GetCurrentPrice() decimal.Decimal {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentPrice
}

// GetLastUpdate returns when price was last updated
func (c *Client) GetLastUpdate() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastUpdate
}

// IsStale returns true if price is older than 5 seconds
func (c *Client) IsStale() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return time.Since(c.lastUpdate) > 5*time.Second
}
