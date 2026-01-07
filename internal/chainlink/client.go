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

// Chainlink Price Feed addresses on Polygon
const (
	// BTC/USD Price Feed on Polygon (what Polymarket uses for resolution)
	BTCUSDFeedAddress = "0xc907E116054Ad103354f2D350FD2514433D57F6f"
	
	// Polygon RPC endpoints
	PolygonRPC = "https://polygon-rpc.com"
	
	// ABI function selectors
	LatestAnswerSelector   = "50d25bcd" // latestAnswer()
	LatestRoundDataSelector = "feaf968c" // latestRoundData()
	DecimalsSelector        = "313ce567" // decimals()
)

// PricePoint for tracking price history
type PricePoint struct {
	Price     decimal.Decimal
	Timestamp time.Time
	RoundID   uint64
}

// Client handles Chainlink price feed data
type Client struct {
	rpcURL       string
	feedAddress  string
	decimals     int
	
	// Real-time data
	currentPrice decimal.Decimal
	priceBuffer  []PricePoint
	lastRoundID  uint64
	
	// Callbacks
	onPrice func(decimal.Decimal)
	
	mu         sync.RWMutex
	bufferMu   sync.RWMutex
	running    bool
	stopCh     chan struct{}
	
	// Polling interval
	pollInterval time.Duration
}

// NewClient creates a new Chainlink client
func NewClient() *Client {
	return &Client{
		rpcURL:       PolygonRPC,
		feedAddress:  BTCUSDFeedAddress,
		decimals:     8, // Chainlink BTC/USD uses 8 decimals
		priceBuffer:  make([]PricePoint, 0, 1000),
		stopCh:       make(chan struct{}),
		pollInterval: 1 * time.Second, // Poll every second
	}
}

// SetPriceCallback sets callback for price updates
func (c *Client) SetPriceCallback(cb func(decimal.Decimal)) {
	c.onPrice = cb
}

// SetPollInterval sets how often to poll the price feed
func (c *Client) SetPollInterval(d time.Duration) {
	c.pollInterval = d
}

// Start begins polling the Chainlink price feed
func (c *Client) Start() error {
	c.running = true
	
	// Initial fetch
	if err := c.fetchPrice(); err != nil {
		log.Warn().Err(err).Msg("Initial Chainlink fetch failed, continuing anyway")
	}
	
	// Start polling loop
	go c.pollLoop()
	
	log.Info().
		Str("feed", c.feedAddress).
		Str("network", "Polygon").
		Msg("⛓️ Chainlink client started")
	return nil
}

// Stop stops the client
func (c *Client) Stop() {
	c.running = false
	close(c.stopCh)
}

func (c *Client) pollLoop() {
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			if err := c.fetchPrice(); err != nil {
				log.Debug().Err(err).Msg("Chainlink price fetch failed")
			}
		case <-c.stopCh:
			return
		}
	}
}

// fetchPrice fetches the latest price from Chainlink
func (c *Client) fetchPrice() error {
	// Call latestRoundData() for full round info
	result, err := c.ethCall(LatestRoundDataSelector)
	if err != nil {
		// Fallback to latestAnswer()
		return c.fetchLatestAnswer()
	}
	
	// Parse latestRoundData response:
	// (uint80 roundId, int256 answer, uint256 startedAt, uint256 updatedAt, uint80 answeredInRound)
	if len(result) < 160 { // 5 * 32 bytes
		return c.fetchLatestAnswer()
	}
	
	// Parse answer (second 32-byte word)
	answerBytes := result[32:64]
	answer := new(big.Int).SetBytes(answerBytes)
	
	// Parse roundId (first 32-byte word, lower 80 bits)
	roundIDBytes := result[0:32]
	roundID := new(big.Int).SetBytes(roundIDBytes).Uint64()
	
	// Convert to decimal with proper decimals
	price := decimal.NewFromBigInt(answer, -int32(c.decimals))
	
	// Only update if price changed or new round
	c.mu.Lock()
	oldPrice := c.currentPrice
	c.currentPrice = price
	newRound := roundID != c.lastRoundID
	c.lastRoundID = roundID
	c.mu.Unlock()
	
	// Add to buffer
	c.bufferMu.Lock()
	c.priceBuffer = append(c.priceBuffer, PricePoint{
		Price:     price,
		Timestamp: time.Now(),
		RoundID:   roundID,
	})
	// Keep last 5 minutes
	cutoff := time.Now().Add(-5 * time.Minute)
	for len(c.priceBuffer) > 0 && c.priceBuffer[0].Timestamp.Before(cutoff) {
		c.priceBuffer = c.priceBuffer[1:]
	}
	c.bufferMu.Unlock()
	
	// Callback if price changed
	if !price.Equal(oldPrice) || newRound {
		if c.onPrice != nil {
			c.onPrice(price)
		}
		
		log.Debug().
			Str("price", price.StringFixed(2)).
			Uint64("round", roundID).
			Msg("⛓️ Chainlink price update")
	}
	
	return nil
}

func (c *Client) fetchLatestAnswer() error {
	result, err := c.ethCall(LatestAnswerSelector)
	if err != nil {
		return err
	}
	
	if len(result) < 32 {
		return fmt.Errorf("invalid response length: %d", len(result))
	}
	
	answer := new(big.Int).SetBytes(result)
	price := decimal.NewFromBigInt(answer, -int32(c.decimals))
	
	c.mu.Lock()
	oldPrice := c.currentPrice
	c.currentPrice = price
	c.mu.Unlock()
	
	// Add to buffer
	c.bufferMu.Lock()
	c.priceBuffer = append(c.priceBuffer, PricePoint{
		Price:     price,
		Timestamp: time.Now(),
	})
	c.bufferMu.Unlock()
	
	if !price.Equal(oldPrice) && c.onPrice != nil {
		c.onPrice(price)
	}
	
	return nil
}

// ethCall performs an eth_call RPC request
func (c *Client) ethCall(selector string) ([]byte, error) {
	payload := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "eth_call",
		"params": []interface{}{
			map[string]string{
				"to":   c.feedAddress,
				"data": "0x" + selector,
			},
			"latest",
		},
		"id": 1,
	}
	
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(c.rpcURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("RPC request failed: %w", err)
	}
	defer resp.Body.Close()
	
	var result struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	
	if result.Error != nil {
		return nil, fmt.Errorf("RPC error: %s", result.Error.Message)
	}
	
	// Remove 0x prefix and decode
	if len(result.Result) < 2 {
		return nil, fmt.Errorf("empty result")
	}
	
	return hex.DecodeString(result.Result[2:])
}

// GetHistoricalPrice fetches the Chainlink price at a specific timestamp by querying on-chain rounds
// This is the "Price to Beat" for Polymarket windows
func (c *Client) GetHistoricalPrice(targetTime time.Time) (decimal.Decimal, error) {
	targetTS := targetTime.Unix()
	
	// First get current round
	result, err := c.ethCall(LatestRoundDataSelector)
	if err != nil {
		return decimal.Zero, err
	}
	
	if len(result) < 160 {
		return decimal.Zero, fmt.Errorf("invalid round data")
	}
	
	// Parse current round ID (first 32 bytes, but it's a uint80 packed weird)
	currentRoundID := new(big.Int).SetBytes(result[0:32])
	
	// Binary search backwards to find the round at targetTS
	// Chainlink updates every ~30 seconds, so search round by round for accuracy
	// Search up to 100 rounds back (~50 minutes)
	
	var bestPrice decimal.Decimal
	var bestTS int64
	var bestRound *big.Int
	
	for offset := int64(0); offset < 100; offset++ {
		testRound := new(big.Int).Sub(currentRoundID, big.NewInt(offset))
		
		// Call getRoundData(uint80 _roundId)
		// Function selector: 9a6fc8f5
		roundHex := fmt.Sprintf("%064x", testRound)
		roundData, err := c.ethCall("9a6fc8f5" + roundHex)
		if err != nil {
			continue
		}
		
		if len(roundData) < 160 {
			continue
		}
		
		// Parse: roundId, answer, startedAt, updatedAt, answeredInRound
		answer := new(big.Int).SetBytes(roundData[32:64])
		updatedAt := new(big.Int).SetBytes(roundData[96:128]).Int64()
		
		price := decimal.NewFromBigInt(answer, -int32(c.decimals))
		
		// Found a round at or just before target time - this is the one Polymarket uses
		if updatedAt <= targetTS {
			timeDiff := targetTS - updatedAt
			log.Debug().
				Str("price", price.StringFixed(2)).
				Int64("round_ts", updatedAt).
				Int64("target_ts", targetTS).
				Int64("time_diff_sec", timeDiff).
				Msg("⛓️ Found historical Chainlink price")
			return price, nil
		}
		
		// Track closest price so far (just after target)
		if bestTS == 0 || updatedAt < bestTS {
			bestTS = updatedAt
			bestPrice = price
			bestRound = testRound
		}
	}
	
	// Return best match if exact not found
	if !bestPrice.IsZero() {
		log.Debug().
			Str("price", bestPrice.StringFixed(2)).
			Str("round", bestRound.String()).
			Msg("⛓️ Using closest historical Chainlink price")
		return bestPrice, nil
	}
	
	return decimal.Zero, fmt.Errorf("could not find historical price")
}

// GetCurrentPrice returns the current Chainlink BTC price
func (c *Client) GetCurrentPrice() decimal.Decimal {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentPrice
}

// GetPriceBuffer returns recent price data
func (c *Client) GetPriceBuffer() []PricePoint {
	c.bufferMu.RLock()
	defer c.bufferMu.RUnlock()
	
	result := make([]PricePoint, len(c.priceBuffer))
	copy(result, c.priceBuffer)
	return result
}

// GetLastRoundID returns the last round ID
func (c *Client) GetLastRoundID() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastRoundID
}

// GetPriceChange calculates price change over duration
func (c *Client) GetPriceChange(duration time.Duration) (decimal.Decimal, decimal.Decimal) {
	c.bufferMu.RLock()
	defer c.bufferMu.RUnlock()
	
	if len(c.priceBuffer) < 2 {
		return decimal.Zero, decimal.Zero
	}
	
	cutoff := time.Now().Add(-duration)
	var oldPrice decimal.Decimal
	
	for _, p := range c.priceBuffer {
		if p.Timestamp.After(cutoff) {
			break
		}
		oldPrice = p.Price
	}
	
	if oldPrice.IsZero() {
		oldPrice = c.priceBuffer[0].Price
	}
	
	currentPrice := c.priceBuffer[len(c.priceBuffer)-1].Price
	change := currentPrice.Sub(oldPrice)
	changePct := change.Div(oldPrice).Mul(decimal.NewFromInt(100))
	
	return change, changePct
}

// CompareToBinance returns the difference between Chainlink and Binance prices
// Positive = Chainlink higher, Negative = Binance higher
func (c *Client) CompareToBinance(binancePrice decimal.Decimal) (diff decimal.Decimal, pctDiff decimal.Decimal) {
	c.mu.RLock()
	chainlinkPrice := c.currentPrice
	c.mu.RUnlock()
	
	if chainlinkPrice.IsZero() || binancePrice.IsZero() {
		return decimal.Zero, decimal.Zero
	}
	
	diff = chainlinkPrice.Sub(binancePrice)
	pctDiff = diff.Div(binancePrice).Mul(decimal.NewFromInt(100))
	
	return diff, pctDiff
}

// GetPriceAtTime returns the Chainlink price closest to the given timestamp
// by searching the price buffer. If the time is before buffer start, returns the oldest price.
// This is used to get the "Price to Beat" for Polymarket windows.
func (c *Client) GetPriceAtTime(t time.Time) decimal.Decimal {
	c.bufferMu.RLock()
	defer c.bufferMu.RUnlock()
	
	if len(c.priceBuffer) == 0 {
		return decimal.Zero
	}
	
	// If target time is before our buffer, return oldest price
	if t.Before(c.priceBuffer[0].Timestamp) {
		return c.priceBuffer[0].Price
	}
	
	// Find the price closest to but not after the target time
	var bestPrice decimal.Decimal
	for _, p := range c.priceBuffer {
		if p.Timestamp.After(t) {
			break
		}
		bestPrice = p.Price
	}
	
	if bestPrice.IsZero() {
		return c.priceBuffer[0].Price
	}
	
	return bestPrice
}
