// Package chainlink provides multi-asset Chainlink price feeds
// Supports BTC, ETH, SOL on Polygon for accurate "Price to Beat" determination
package chainlink

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// Chainlink Price Feed addresses on Polygon (what Polymarket uses!)
var FeedAddresses = map[string]struct {
	Address  string
	Decimals int
}{
	"BTC": {Address: "0xc907E116054Ad103354f2D350FD2514433D57F6f", Decimals: 8},
	"ETH": {Address: "0xF9680D99D6C9589e2a93a78A04A279e509205945", Decimals: 8},
	"SOL": {Address: "0x10C8264C0935b3B9870013e057f330Ff3e9C56dC", Decimals: 8},
}

// Multiple free RPCs to rotate through (avoids rate limits)
var PolygonRPCs = []string{
	"https://polygon-rpc.com",
	"https://rpc-mainnet.matic.quiknode.pro",
	"https://polygon-mainnet.public.blastapi.io",
	"https://polygon.llamarpc.com",
	"https://polygon.drpc.org",
	"https://1rpc.io/matic",
}

// MultiAssetPrice holds price data for an asset
type MultiAssetPrice struct {
	Asset       string
	Price       decimal.Decimal
	RoundID     uint64
	UpdatedAt   time.Time
	FeedAddress string
}

// MultiClient handles multiple Chainlink price feeds
type MultiClient struct {
	rpcURLs   []string
	rpcIndex  int
	rpcMu     sync.Mutex
	assets    []string
	prices    map[string]*MultiAssetPrice
	pricesMu  sync.RWMutex
	stopCh    chan struct{}
	running   bool
}

// NewMultiClient creates a multi-asset Chainlink client
func NewMultiClient(assets ...string) *MultiClient {
	if len(assets) == 0 {
		assets = []string{"BTC", "ETH", "SOL"}
	}
	
	prices := make(map[string]*MultiAssetPrice)
	for _, asset := range assets {
		if feed, ok := FeedAddresses[asset]; ok {
			prices[asset] = &MultiAssetPrice{
				Asset:       asset,
				FeedAddress: feed.Address,
			}
		}
	}
	
	return &MultiClient{
		rpcURLs:  PolygonRPCs,
		rpcIndex: 0,
		assets:   assets,
		prices:   prices,
		stopCh:   make(chan struct{}),
	}
}

// Start begins polling all price feeds
func (c *MultiClient) Start() error {
	c.running = true
	
	// Initial fetch
	c.fetchAllPrices()
	
	// Poll every 2 seconds for all assets
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		
		for {
			select {
			case <-ticker.C:
				c.fetchAllPrices()
			case <-c.stopCh:
				return
			}
		}
	}()
	
	log.Info().
		Strs("assets", c.assets).
		Msg("⛓️ Multi-asset Chainlink client started")
	return nil
}

// Stop stops the client
func (c *MultiClient) Stop() {
	c.running = false
	close(c.stopCh)
}

func (c *MultiClient) fetchAllPrices() {
	for asset := range c.prices {
		c.fetchPrice(asset)
	}
}

func (c *MultiClient) fetchPrice(asset string) {
	c.pricesMu.RLock()
	priceData, ok := c.prices[asset]
	c.pricesMu.RUnlock()
	
	if !ok {
		return
	}
	
	feed, ok := FeedAddresses[asset]
	if !ok {
		return
	}
	
	result, err := c.ethCall(priceData.FeedAddress, LatestRoundDataSelector)
	if err != nil {
		log.Debug().Err(err).Str("asset", asset).Msg("Chainlink fetch failed")
		return
	}
	
	if len(result) < 160 {
		return
	}
	
	// Parse answer (second 32-byte word)
	answer := new(big.Int).SetBytes(result[32:64])
	roundID := new(big.Int).SetBytes(result[0:32]).Uint64()
	
	price := decimal.NewFromBigInt(answer, -int32(feed.Decimals))
	
	c.pricesMu.Lock()
	c.prices[asset].Price = price
	c.prices[asset].RoundID = roundID
	c.prices[asset].UpdatedAt = time.Now()
	c.pricesMu.Unlock()
	
	log.Debug().
		Str("asset", asset).
		Str("price", price.StringFixed(2)).
		Msg("⛓️ Chainlink price updated")
}

// GetPrice returns current price for an asset
func (c *MultiClient) GetPrice(asset string) decimal.Decimal {
	c.pricesMu.RLock()
	defer c.pricesMu.RUnlock()
	
	if p, ok := c.prices[asset]; ok {
		return p.Price
	}
	return decimal.Zero
}

// GetHistoricalPrice fetches historical price at a specific time for any asset
func (c *MultiClient) GetHistoricalPrice(asset string, targetTime time.Time) (decimal.Decimal, error) {
	feed, ok := FeedAddresses[asset]
	if !ok {
		return decimal.Zero, fmt.Errorf("unknown asset: %s", asset)
	}
	
	targetTS := targetTime.Unix()
	
	// Get current round
	result, err := c.ethCall(feed.Address, LatestRoundDataSelector)
	if err != nil {
		return decimal.Zero, err
	}
	
	if len(result) < 160 {
		return decimal.Zero, fmt.Errorf("invalid round data")
	}
	
	currentRoundID := new(big.Int).SetBytes(result[0:32])
	
	// Search backwards for the round at targetTS
	for offset := int64(0); offset < 100; offset++ {
		testRound := new(big.Int).Sub(currentRoundID, big.NewInt(offset))
		
		roundHex := fmt.Sprintf("%064x", testRound)
		roundData, err := c.ethCall(feed.Address, "9a6fc8f5"+roundHex)
		if err != nil {
			continue
		}
		
		if len(roundData) < 160 {
			continue
		}
		
		answer := new(big.Int).SetBytes(roundData[32:64])
		updatedAt := new(big.Int).SetBytes(roundData[96:128]).Int64()
		
		price := decimal.NewFromBigInt(answer, -int32(feed.Decimals))
		
		if updatedAt <= targetTS {
			log.Debug().
				Str("asset", asset).
				Str("price", price.StringFixed(2)).
				Int64("round_ts", updatedAt).
				Int64("target_ts", targetTS).
				Msg("⛓️ Found historical price")
			return price, nil
		}
	}
	
	return decimal.Zero, fmt.Errorf("could not find historical price for %s", asset)
}

// getNextRPC rotates through available RPCs
func (c *MultiClient) getNextRPC() string {
	c.rpcMu.Lock()
	defer c.rpcMu.Unlock()
	
	rpc := c.rpcURLs[c.rpcIndex]
	c.rpcIndex = (c.rpcIndex + 1) % len(c.rpcURLs)
	return rpc
}

func (c *MultiClient) ethCall(feedAddress, selector string) ([]byte, error) {
	return c.ethCallWithRetry(feedAddress, selector, 3)
}

func (c *MultiClient) ethCallWithRetry(feedAddress, selector string, retries int) ([]byte, error) {
	var lastErr error
	
	for i := 0; i < retries; i++ {
		rpcURL := c.getNextRPC()
		
		payload := map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "eth_call",
			"params": []interface{}{
				map[string]string{
					"to":   feedAddress,
					"data": "0x" + selector,
				},
				"latest",
			},
			"id": 1,
		}
		
		body, err := json.Marshal(payload)
		if err != nil {
			lastErr = err
			continue
		}
		
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Post(rpcURL, "application/json", bytes.NewReader(body))
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close()
		
		var result struct {
			Result string `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			lastErr = err
			continue
		}
		
		if result.Error != nil {
			lastErr = fmt.Errorf("RPC error: %s", result.Error.Message)
			continue // Try next RPC
		}
		
		if len(result.Result) < 2 {
			lastErr = fmt.Errorf("empty result")
			continue
		}
		
		return hex.DecodeString(result.Result[2:])
	}
	
	return nil, lastErr
}
