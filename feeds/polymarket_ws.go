package feeds

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ═══════════════════════════════════════════════════════════════════════════════
// POLYMARKET WEBSOCKET FEED
// ═══════════════════════════════════════════════════════════════════════════════
//
// Connects to Polymarket WebSocket for live price updates
// Maintains in-memory orderbook state for fast lookups
//
// ═══════════════════════════════════════════════════════════════════════════════

const (
	PolymarketWSURL = "wss://ws-subscriptions-clob.polymarket.com/ws/market"
	reconnectDelay  = 5 * time.Second
	pingInterval    = 30 * time.Second
)

// Tick represents a price update event
type Tick struct {
	Market    string          // Market/condition ID
	Asset     string          // Token ID
	Side      string          // "YES" or "NO"
	BestBid   decimal.Decimal // Best bid price
	BestAsk   decimal.Decimal // Best ask price
	Mid       decimal.Decimal // Mid price
	Spread    decimal.Decimal // Bid-ask spread
	BidSize   decimal.Decimal // Size at best bid
	AskSize   decimal.Decimal // Size at best ask
	Volume24h decimal.Decimal // 24h volume
	Timestamp time.Time
}

// PolymarketFeed manages WebSocket connection and tick distribution
type PolymarketFeed struct {
	mu sync.RWMutex

	wsURL     string
	conn      *websocket.Conn
	connected bool
	running   bool
	stopCh    chan struct{}

	// Subscribers receive ticks
	subscribers []chan Tick

	// Current state per market
	orderbooks map[string]*Orderbook

	// Price cache for quick lookups
	prices map[string]decimal.Decimal // "market:side" -> price
}

// NewPolymarketFeed creates a new feed instance
func NewPolymarketFeed() *PolymarketFeed {
	return &PolymarketFeed{
		wsURL:       PolymarketWSURL,
		stopCh:      make(chan struct{}),
		subscribers: make([]chan Tick, 0),
		orderbooks:  make(map[string]*Orderbook),
		prices:      make(map[string]decimal.Decimal),
	}
}

// Start connects and begins processing
func (f *PolymarketFeed) Start() {
	f.mu.Lock()
	if f.running {
		f.mu.Unlock()
		return
	}
	f.running = true
	f.mu.Unlock()

	go f.connectionLoop()
	log.Info().Msg("📡 Feed started")
}

// Stop closes the connection
func (f *PolymarketFeed) Stop() {
	f.mu.Lock()
	defer f.mu.Unlock()

	if !f.running {
		return
	}

	f.running = false
	close(f.stopCh)

	if f.conn != nil {
		f.conn.Close()
	}

	log.Info().Msg("Feed stopped")
}

// Subscribe returns a channel that receives ticks
func (f *PolymarketFeed) Subscribe() chan Tick {
	f.mu.Lock()
	defer f.mu.Unlock()

	ch := make(chan Tick, 1000)
	f.subscribers = append(f.subscribers, ch)
	return ch
}

// GetPrice returns the current price for a market/side
func (f *PolymarketFeed) GetPrice(market, side string) decimal.Decimal {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.prices[market+":"+side]
}

// connectionLoop maintains the WebSocket connection
func (f *PolymarketFeed) connectionLoop() {
	for {
		select {
		case <-f.stopCh:
			return
		default:
		}

		if err := f.connect(); err != nil {
			log.Error().Err(err).Msg("Connection failed, retrying...")
			time.Sleep(reconnectDelay)
			continue
		}

		f.readLoop()
		time.Sleep(reconnectDelay)
	}
}

// connect establishes WebSocket connection
func (f *PolymarketFeed) connect() error {
	conn, _, err := websocket.DefaultDialer.Dial(f.wsURL, nil)
	if err != nil {
		return err
	}

	f.mu.Lock()
	f.conn = conn
	f.connected = true
	f.mu.Unlock()

	log.Info().Msg("🔌 WebSocket connected")

	// Start ping loop
	go f.pingLoop()

	return nil
}

// SubscribeMarket subscribes to a specific market
func (f *PolymarketFeed) SubscribeMarket(market string) error {
	f.mu.RLock()
	conn := f.conn
	f.mu.RUnlock()

	if conn == nil {
		return nil
	}

	msg := map[string]interface{}{
		"type":       "subscribe",
		"market":     market,
		"assets_ids": []string{},
		"channel":    "market",
	}

	return conn.WriteJSON(msg)
}

// pingLoop sends periodic pings to keep connection alive
func (f *PolymarketFeed) pingLoop() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-f.stopCh:
			return
		case <-ticker.C:
			f.mu.RLock()
			conn := f.conn
			connected := f.connected
			f.mu.RUnlock()

			if connected && conn != nil {
				conn.WriteMessage(websocket.PingMessage, nil)
			}
		}
	}
}

// readLoop reads messages from WebSocket
func (f *PolymarketFeed) readLoop() {
	for {
		select {
		case <-f.stopCh:
			return
		default:
		}

		f.mu.RLock()
		conn := f.conn
		f.mu.RUnlock()

		if conn == nil {
			return
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			log.Warn().Err(err).Msg("Read error")
			f.mu.Lock()
			f.connected = false
			f.mu.Unlock()
			return
		}

		f.processMessage(message)
	}
}

// WSMessage represents a WebSocket message from Polymarket
type WSMessage struct {
	EventType string          `json:"event_type"`
	Market    string          `json:"market"`
	Asset     string          `json:"asset_id"`
	Price     string          `json:"price"`
	Side      string          `json:"side"`
	Bids      [][]interface{} `json:"bids"`
	Asks      [][]interface{} `json:"asks"`
}

// processMessage handles incoming WebSocket messages
func (f *PolymarketFeed) processMessage(data []byte) {
	var msgs []WSMessage
	if err := json.Unmarshal(data, &msgs); err != nil {
		// Try single message
		var msg WSMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			return
		}
		msgs = []WSMessage{msg}
	}

	for _, msg := range msgs {
		switch msg.EventType {
		case "book":
			f.handleBookUpdate(msg)
		case "price_change":
			f.handlePriceChange(msg)
		case "last_trade_price":
			f.handleTradePrice(msg)
		}
	}
}

// handleBookUpdate processes orderbook updates
func (f *PolymarketFeed) handleBookUpdate(msg WSMessage) {
	f.mu.Lock()
	ob, exists := f.orderbooks[msg.Asset]
	if !exists {
		ob = NewOrderbook(msg.Market, msg.Asset)
		f.orderbooks[msg.Asset] = ob
	}
	f.mu.Unlock()

	// Update orderbook
	ob.UpdateFromWS(msg.Bids, msg.Asks)

	// Generate tick
	tick := Tick{
		Market:    msg.Market,
		Asset:     msg.Asset,
		BestBid:   ob.BestBid(),
		BestAsk:   ob.BestAsk(),
		BidSize:   ob.BestBidSize(),
		AskSize:   ob.BestAskSize(),
		Timestamp: time.Now(),
	}

	tick.Mid = tick.BestBid.Add(tick.BestAsk).Div(decimal.NewFromInt(2))
	tick.Spread = tick.BestAsk.Sub(tick.BestBid)

	// Determine side
	if ob.Side == "YES" {
		tick.Side = "YES"
	} else {
		tick.Side = "NO"
	}

	// Cache price
	f.mu.Lock()
	f.prices[msg.Market+":"+tick.Side] = tick.Mid
	f.mu.Unlock()

	// Distribute to subscribers
	f.broadcast(tick)
}

// handlePriceChange processes price change events
func (f *PolymarketFeed) handlePriceChange(msg WSMessage) {
	price, _ := decimal.NewFromString(msg.Price)

	// Determine side from asset ID (would need lookup)
	side := "YES" // Default

	tick := Tick{
		Market:    msg.Market,
		Asset:     msg.Asset,
		Side:      side,
		Mid:       price,
		BestBid:   price.Sub(decimal.NewFromFloat(0.01)),
		BestAsk:   price.Add(decimal.NewFromFloat(0.01)),
		Timestamp: time.Now(),
	}

	f.mu.Lock()
	f.prices[msg.Market+":"+side] = price
	f.mu.Unlock()

	f.broadcast(tick)
}

// handleTradePrice processes trade events
func (f *PolymarketFeed) handleTradePrice(msg WSMessage) {
	price, _ := decimal.NewFromString(msg.Price)

	tick := Tick{
		Market:    msg.Market,
		Asset:     msg.Asset,
		Side:      msg.Side,
		Mid:       price,
		Timestamp: time.Now(),
	}

	f.broadcast(tick)
}

// broadcast sends tick to all subscribers
func (f *PolymarketFeed) broadcast(tick Tick) {
	f.mu.RLock()
	subs := f.subscribers
	f.mu.RUnlock()

	for _, ch := range subs {
		select {
		case ch <- tick:
		default:
			// Skip if channel full
		}
	}
}
