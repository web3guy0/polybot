package binance

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// MultiClient handles multiple asset streams from Binance
type MultiClient struct {
	wsURL    string
	assets   []string
	conn     *websocket.Conn
	
	// Prices per asset
	prices   map[string]decimal.Decimal
	pricesMu sync.RWMutex
	
	running  bool
	stopCh   chan struct{}
}

// Asset symbols mapping
var binanceSymbols = map[string]string{
	"BTC": "btcusdt",
	"ETH": "ethusdt", 
	"SOL": "solusdt",
	"XRP": "xrpusdt",
}

// NewMultiClient creates a multi-asset Binance client
func NewMultiClient(assets ...string) *MultiClient {
	if len(assets) == 0 {
		assets = []string{"BTC", "ETH", "SOL"}
	}
	
	return &MultiClient{
		wsURL:  "wss://stream.binance.com:9443/ws",
		assets: assets,
		prices: make(map[string]decimal.Decimal),
		stopCh: make(chan struct{}),
	}
}

// Start connects to WebSocket for all assets
func (c *MultiClient) Start() error {
	c.running = true
	go c.runWebSocket()
	
	log.Info().
		Strs("assets", c.assets).
		Msg("📈 Binance multi-asset client started")
	return nil
}

// Stop closes the connection
func (c *MultiClient) Stop() {
	c.running = false
	close(c.stopCh)
	if c.conn != nil {
		c.conn.Close()
	}
}

// GetPrice returns current price for an asset
func (c *MultiClient) GetPrice(asset string) decimal.Decimal {
	c.pricesMu.RLock()
	defer c.pricesMu.RUnlock()
	
	if p, ok := c.prices[strings.ToUpper(asset)]; ok {
		return p
	}
	return decimal.Zero
}

func (c *MultiClient) runWebSocket() {
	for c.running {
		if err := c.connectWebSocket(); err != nil {
			log.Error().Err(err).Msg("Binance WS connection failed")
			time.Sleep(5 * time.Second)
			continue
		}
		
		c.readMessages()
		
		if c.running {
			log.Warn().Msg("Binance WS disconnected, reconnecting...")
			time.Sleep(1 * time.Second)
		}
	}
}

func (c *MultiClient) connectWebSocket() error {
	// Build combined stream URL for all assets
	var streams []string
	for _, asset := range c.assets {
		if symbol, ok := binanceSymbols[strings.ToUpper(asset)]; ok {
			streams = append(streams, symbol+"@trade")
		}
	}
	
	// Combined streams format: /stream?streams=btcusdt@trade/ethusdt@trade/solusdt@trade
	url := fmt.Sprintf("wss://stream.binance.com:9443/stream?streams=%s", 
		strings.Join(streams, "/"))
	
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		return fmt.Errorf("websocket dial failed: %w", err)
	}
	
	c.conn = conn
	log.Info().
		Strs("assets", c.assets).
		Msg("🔌 WebSocket connected to Binance (multi-asset)")
	return nil
}

func (c *MultiClient) readMessages() {
	for c.running {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if c.running {
				log.Error().Err(err).Msg("Binance WS read error")
			}
			return
		}
		
		c.handleMessage(message)
	}
}

func (c *MultiClient) handleMessage(data []byte) {
	// Combined stream format: {"stream":"btcusdt@trade","data":{...}}
	var wrapper struct {
		Stream string          `json:"stream"`
		Data   json.RawMessage `json:"data"`
	}
	
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return
	}
	
	// Parse trade data
	var trade struct {
		Symbol string `json:"s"`  // "BTCUSDT"
		Price  string `json:"p"`  // Trade price
	}
	
	if err := json.Unmarshal(wrapper.Data, &trade); err != nil {
		return
	}
	
	// Extract asset from symbol (BTCUSDT -> BTC)
	asset := strings.TrimSuffix(strings.ToUpper(trade.Symbol), "USDT")
	
	price, err := decimal.NewFromString(trade.Price)
	if err != nil {
		return
	}
	
	c.pricesMu.Lock()
	c.prices[asset] = price
	c.pricesMu.Unlock()
}
