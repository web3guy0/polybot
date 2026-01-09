package polymarket

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

const (
	PolymarketWSURL = "wss://ws-subscriptions-clob.polymarket.com/ws/market"
)

// WSClient manages WebSocket connections to Polymarket
type WSClient struct {
	conn         *websocket.Conn
	mu           sync.RWMutex
	isConnected  bool
	subscribed   map[string]bool // conditionId -> subscribed
	
	// Price data: tokenId -> price info
	prices       map[string]*TokenPrice
	pricesMu     sync.RWMutex
	
	// Callbacks
	onPriceChange func(conditionId string, upPrice, downPrice decimal.Decimal)
	
	stopCh       chan struct{}
}

// TokenPrice holds real-time price info for a token
type TokenPrice struct {
	TokenID   string
	BestBid   decimal.Decimal
	BestAsk   decimal.Decimal
	LastPrice decimal.Decimal
	UpdatedAt time.Time
}

// WSMarketSnapshot is the initial order book snapshot
type WSMarketSnapshot struct {
	Market    string `json:"market"`
	AssetID   string `json:"asset_id"`
	Timestamp string `json:"timestamp"`
	Hash      string `json:"hash"`
	Bids      []struct {
		Price string `json:"price"`
		Size  string `json:"size"`
	} `json:"bids"`
	Asks []struct {
		Price string `json:"price"`
		Size  string `json:"size"`
	} `json:"asks"`
}

// WSPriceChange is a real-time price update
type WSPriceChange struct {
	Market       string `json:"market"`
	PriceChanges []struct {
		AssetID  string `json:"asset_id"`
		Price    string `json:"price"`
		Size     string `json:"size"`
		Side     string `json:"side"`
		Hash     string `json:"hash"`
		BestBid  string `json:"best_bid"`
		BestAsk  string `json:"best_ask"`
	} `json:"price_changes"`
	Timestamp string `json:"timestamp"`
	EventType string `json:"event_type"`
}

// NewWSClient creates a new WebSocket client
func NewWSClient() *WSClient {
	return &WSClient{
		subscribed: make(map[string]bool),
		prices:     make(map[string]*TokenPrice),
		stopCh:     make(chan struct{}),
	}
}

// Connect establishes WebSocket connection
func (c *WSClient) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	if c.isConnected {
		return nil
	}
	
	log.Info().Str("url", PolymarketWSURL).Msg("Connecting to Polymarket WebSocket...")
	
	conn, _, err := websocket.DefaultDialer.Dial(PolymarketWSURL, nil)
	if err != nil {
		return fmt.Errorf("websocket dial failed: %w", err)
	}
	
	c.conn = conn
	c.isConnected = true
	
	// Start message handler
	go c.readMessages()
	
	log.Info().Msg("✅ Connected to Polymarket WebSocket")
	return nil
}

// Subscribe to a market by token IDs (UP and DOWN)
func (c *WSClient) Subscribe(conditionId, upTokenId, downTokenId string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	if !c.isConnected {
		return fmt.Errorf("not connected")
	}
	
	if c.subscribed[conditionId] {
		return nil // Already subscribed
	}
	
	// Subscribe message format
	msg := map[string]interface{}{
		"type":       "market",
		"assets_ids": []string{upTokenId, downTokenId},
	}
	
	msgBytes, _ := json.Marshal(msg)
	if err := c.conn.WriteMessage(websocket.TextMessage, msgBytes); err != nil {
		return fmt.Errorf("subscribe failed: %w", err)
	}
	
	c.subscribed[conditionId] = true
	log.Info().Str("conditionId", conditionId[:16]+"...").Msg("📡 Subscribed to market WebSocket")
	
	return nil
}

// OnPriceChange sets callback for price updates
func (c *WSClient) OnPriceChange(callback func(conditionId string, upPrice, downPrice decimal.Decimal)) {
	c.onPriceChange = callback
}

// GetTokenPrice returns current price for a token
func (c *WSClient) GetTokenPrice(tokenId string) (decimal.Decimal, bool) {
	c.pricesMu.RLock()
	defer c.pricesMu.RUnlock()
	
	if p, ok := c.prices[tokenId]; ok {
		// Return best bid (what you can sell for) as the "price"
		return p.BestBid, true
	}
	return decimal.Zero, false
}

// GetMarketPrices returns UP and DOWN prices for a condition
func (c *WSClient) GetMarketPrices(upTokenId, downTokenId string) (upPrice, downPrice decimal.Decimal, ok bool) {
	c.pricesMu.RLock()
	defer c.pricesMu.RUnlock()
	
	upP, hasUp := c.prices[upTokenId]
	downP, hasDown := c.prices[downTokenId]
	
	if !hasUp || !hasDown {
		return decimal.Zero, decimal.Zero, false
	}
	
	// Use best bid for the prices (what you can actually sell at)
	return upP.BestBid, downP.BestBid, true
}

func (c *WSClient) readMessages() {
	for {
		select {
		case <-c.stopCh:
			return
		default:
		}
		
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			log.Error().Err(err).Msg("WebSocket read error")
			c.handleDisconnect()
			return
		}
		
		c.handleMessage(message)
	}
}

func (c *WSClient) handleMessage(data []byte) {
	// Try to parse as price change first
	var priceChange WSPriceChange
	if err := json.Unmarshal(data, &priceChange); err == nil && priceChange.EventType == "price_change" {
		c.handlePriceChange(&priceChange)
		return
	}
	
	// Try to parse as market snapshot (initial subscription response is an array)
	var snapshots []WSMarketSnapshot
	if err := json.Unmarshal(data, &snapshots); err == nil && len(snapshots) > 0 {
		for _, snap := range snapshots {
			c.handleSnapshot(&snap)
		}
		return
	}
}

func (c *WSClient) handleSnapshot(snap *WSMarketSnapshot) {
	c.pricesMu.Lock()
	defer c.pricesMu.Unlock()
	
	var bestBid, bestAsk decimal.Decimal
	
	if len(snap.Bids) > 0 {
		bestBid, _ = decimal.NewFromString(snap.Bids[0].Price)
	}
	if len(snap.Asks) > 0 {
		bestAsk, _ = decimal.NewFromString(snap.Asks[0].Price)
	}
	
	c.prices[snap.AssetID] = &TokenPrice{
		TokenID:   snap.AssetID,
		BestBid:   bestBid,
		BestAsk:   bestAsk,
		UpdatedAt: time.Now(),
	}
	
	log.Debug().
		Str("token", snap.AssetID[:16]+"...").
		Str("bid", bestBid.String()).
		Str("ask", bestAsk.String()).
		Msg("📊 Snapshot received")
}

func (c *WSClient) handlePriceChange(pc *WSPriceChange) {
	c.pricesMu.Lock()
	defer c.pricesMu.Unlock()
	
	for _, change := range pc.PriceChanges {
		bestBid, _ := decimal.NewFromString(change.BestBid)
		bestAsk, _ := decimal.NewFromString(change.BestAsk)
		
		existing, ok := c.prices[change.AssetID]
		if !ok {
			existing = &TokenPrice{TokenID: change.AssetID}
			c.prices[change.AssetID] = existing
		}
		
		existing.BestBid = bestBid
		existing.BestAsk = bestAsk
		existing.UpdatedAt = time.Now()
	}
}

func (c *WSClient) handleDisconnect() {
	c.mu.Lock()
	c.isConnected = false
	c.mu.Unlock()
	
	log.Warn().Msg("WebSocket disconnected, reconnecting in 5s...")
	
	time.Sleep(5 * time.Second)
	
	if err := c.Connect(); err != nil {
		log.Error().Err(err).Msg("Reconnect failed")
	} else {
		// Re-subscribe to all markets
		c.mu.Lock()
		c.subscribed = make(map[string]bool)
		c.mu.Unlock()
	}
}

// Close closes the WebSocket connection
func (c *WSClient) Close() {
	close(c.stopCh)
	
	c.mu.Lock()
	defer c.mu.Unlock()
	
	if c.conn != nil {
		c.conn.Close()
	}
	c.isConnected = false
}

// IsConnected returns connection status
func (c *WSClient) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.isConnected
}
