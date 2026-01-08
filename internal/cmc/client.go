// Package cmc provides CoinMarketCap price feed for multiple assets
// CMC uses the same Chainlink aggregated data as Polymarket Data Streams
// Updates faster than Chainlink on-chain (~1s vs ~30s)
// Supports BTC, ETH, SOL for multi-asset arbitrage
package cmc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

// AssetPrice holds price data for a single asset
type AssetPrice struct {
	Price      decimal.Decimal
	LastUpdate time.Time
}

// Client fetches crypto prices from CoinMarketCap
type Client struct {
	httpClient *http.Client
	assets     []string // Assets to track (e.g., ["bitcoin", "ethereum", "solana"])
	prices     map[string]*AssetPrice
	mu         sync.RWMutex
	stopCh     chan struct{}
}

// Slug mappings for CMC API
var slugMap = map[string]string{
	"BTC": "bitcoin",
	"ETH": "ethereum",
	"SOL": "solana",
}

// NewClient creates a new CMC client for specific assets
func NewClient(assets ...string) *Client {
	if len(assets) == 0 {
		assets = []string{"BTC"} // Default to BTC only
	}
	
	// Convert to CMC slugs
	slugs := make([]string, 0, len(assets))
	for _, asset := range assets {
		if slug, ok := slugMap[strings.ToUpper(asset)]; ok {
			slugs = append(slugs, slug)
		}
	}
	
	return &Client{
		httpClient: &http.Client{Timeout: 2 * time.Second},
		assets:     slugs,
		prices:     make(map[string]*AssetPrice),
		stopCh:     make(chan struct{}),
	}
}

// Start begins polling CMC for price updates
func (c *Client) Start() {
	// Initialize price maps
	for _, slug := range c.assets {
		c.prices[slug] = &AssetPrice{}
	}
	
	// Fetch initial prices
	c.fetchPrices()

	// Poll every 1 second for fast updates
	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				c.fetchPrices()
			case <-c.stopCh:
				return
			}
		}
	}()

	log.Info().
		Strs("assets", c.assets).
		Msg("📊 CMC client started (1s polling)")
}

// Stop stops the client
func (c *Client) Stop() {
	close(c.stopCh)
}

// fetchPrices gets current prices from CMC for all tracked assets
func (c *Client) fetchPrices() {
	// Build URL with all slugs
	slugs := strings.Join(c.assets, ",")
	url := fmt.Sprintf("https://api.coinmarketcap.com/data-api/v3/cryptocurrency/quote/latest?slug=%s&convertId=2781", slugs)
	
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

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, asset := range data.Data {
		if len(asset.Quotes) > 0 {
			price := decimal.NewFromFloat(asset.Quotes[0].Price)
			
			// Map symbol back to slug
			slug := ""
			switch asset.Symbol {
			case "BTC":
				slug = "bitcoin"
			case "ETH":
				slug = "ethereum"
			case "SOL":
				slug = "solana"
			}
			
			if slug != "" && c.prices[slug] != nil {
				oldPrice := c.prices[slug].Price
				c.prices[slug].Price = price
				c.prices[slug].LastUpdate = time.Now()

				// Log significant changes
				if !oldPrice.IsZero() {
					diff := price.Sub(oldPrice).Abs()
					threshold := decimal.NewFromFloat(1) // $1 for BTC/ETH, will adjust for SOL
					if slug == "solana" {
						threshold = decimal.NewFromFloat(0.1) // $0.10 for SOL
					}
					if diff.GreaterThan(threshold) {
						log.Debug().
							Str("asset", asset.Symbol).
							Str("price", price.StringFixed(2)).
							Str("change", diff.StringFixed(2)).
							Msg("📊 CMC price update")
					}
				}
			}
		}
	}
}

// GetCurrentPrice returns the latest BTC price (backward compatible)
func (c *Client) GetCurrentPrice() decimal.Decimal {
	return c.GetAssetPrice("BTC")
}

// GetAssetPrice returns the latest price for a specific asset
func (c *Client) GetAssetPrice(asset string) decimal.Decimal {
	slug, ok := slugMap[strings.ToUpper(asset)]
	if !ok {
		return decimal.Zero
	}
	
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	if p, exists := c.prices[slug]; exists {
		return p.Price
	}
	return decimal.Zero
}

// GetLastUpdate returns when BTC price was last updated (backward compatible)
func (c *Client) GetLastUpdate() time.Time {
	return c.GetAssetLastUpdate("BTC")
}

// GetAssetLastUpdate returns when a specific asset was last updated
func (c *Client) GetAssetLastUpdate(asset string) time.Time {
	slug, ok := slugMap[strings.ToUpper(asset)]
	if !ok {
		return time.Time{}
	}
	
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	if p, exists := c.prices[slug]; exists {
		return p.LastUpdate
	}
	return time.Time{}
}

// IsStale returns true if BTC price is older than 5 seconds (backward compatible)
func (c *Client) IsStale() bool {
	return c.IsAssetStale("BTC")
}

// IsAssetStale returns true if asset price is older than 5 seconds
func (c *Client) IsAssetStale(asset string) bool {
	slug, ok := slugMap[strings.ToUpper(asset)]
	if !ok {
		return true
	}
	
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	if p, exists := c.prices[slug]; exists {
		return time.Since(p.LastUpdate) > 5*time.Second
	}
	return true
}
