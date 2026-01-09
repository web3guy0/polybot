package binance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// Kline represents a candlestick
type Kline struct {
	OpenTime  int64
	Open      decimal.Decimal
	High      decimal.Decimal
	Low       decimal.Decimal
	Close     decimal.Decimal
	Volume    decimal.Decimal
	CloseTime int64
}

// OrderBookEntry represents a price level
type OrderBookEntry struct {
	Price    decimal.Decimal
	Quantity decimal.Decimal
}

// OrderBook represents the order book
type OrderBook struct {
	Bids []OrderBookEntry
	Asks []OrderBookEntry
}

// Trade represents a trade
type Trade struct {
	Price     decimal.Decimal
	Quantity  decimal.Decimal
	Time      int64
	IsBuyMaker bool
}

// Client handles Binance data
type Client struct {
	wsURL      string
	restURL    string
	conn       *websocket.Conn
	
	// Real-time data
	currentPrice decimal.Decimal
	priceBuffer  []PricePoint
	trades       []Trade
	orderBook    *OrderBook
	
	// Callbacks
	onPrice func(decimal.Decimal)
	onTrade func(Trade)
	
	mu         sync.RWMutex
	bufferMu   sync.RWMutex
	running    bool
	stopCh     chan struct{}
	reconnectCh chan struct{}
}

// PricePoint for tracking price history
type PricePoint struct {
	Price     decimal.Decimal
	Volume    decimal.Decimal
	Timestamp time.Time
}

// NewClient creates a new Binance client
func NewClient() *Client {
	return &Client{
		wsURL:       "wss://stream.binance.com:9443/ws",
		restURL:     "https://api.binance.com",
		priceBuffer: make([]PricePoint, 0, 1000),
		trades:      make([]Trade, 0, 1000),
		stopCh:      make(chan struct{}),
		reconnectCh: make(chan struct{}, 1),
	}
}

// SetPriceCallback sets callback for price updates
func (c *Client) SetPriceCallback(cb func(decimal.Decimal)) {
	c.onPrice = cb
}

// SetTradeCallback sets callback for trade updates
func (c *Client) SetTradeCallback(cb func(Trade)) {
	c.onTrade = cb
}

// Start connects to WebSocket and begins streaming
func (c *Client) Start() error {
	c.running = true
	
	// Initial REST fetch for historical data
	if err := c.fetchInitialData(); err != nil {
		log.Warn().Err(err).Msg("Failed to fetch initial data, continuing anyway")
	}
	
	// Connect WebSocket
	go c.runWebSocket()
	
	// Periodic reconnect checker
	go c.reconnectLoop()
	
	log.Info().Msg("📈 Binance client started")
	return nil
}

// Stop closes the WebSocket connection
func (c *Client) Stop() {
	c.running = false
	close(c.stopCh)
	if c.conn != nil {
		c.conn.Close()
	}
}

func (c *Client) runWebSocket() {
	for c.running {
		if err := c.connectWebSocket(); err != nil {
			log.Error().Err(err).Msg("WebSocket connection failed")
			time.Sleep(5 * time.Second)
			continue
		}
		
		c.readMessages()
		
		if c.running {
			log.Warn().Msg("WebSocket disconnected, reconnecting...")
			time.Sleep(1 * time.Second)
		}
	}
}

func (c *Client) connectWebSocket() error {
	// Subscribe to BTC/USDT trade stream and mini ticker
	url := fmt.Sprintf("%s/btcusdt@trade", c.wsURL)
	
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}
	
	conn, _, err := dialer.Dial(url, nil)
	if err != nil {
		return fmt.Errorf("websocket dial failed: %w", err)
	}
	
	c.conn = conn
	log.Info().Str("url", url).Msg("🔌 WebSocket connected to Binance")
	return nil
}

func (c *Client) readMessages() {
	for c.running {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if c.running {
				log.Error().Err(err).Msg("WebSocket read error")
			}
			return
		}
		
		c.handleMessage(message)
	}
}

func (c *Client) handleMessage(data []byte) {
	var msg map[string]interface{}
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	
	// Trade stream message
	if eventType, ok := msg["e"].(string); ok && eventType == "trade" {
		c.handleTradeMessage(msg)
	}
}

func (c *Client) handleTradeMessage(msg map[string]interface{}) {
	priceStr, _ := msg["p"].(string)
	qtyStr, _ := msg["q"].(string)
	isBuyMaker, _ := msg["m"].(bool)
	tradeTime, _ := msg["T"].(float64)
	
	price, _ := decimal.NewFromString(priceStr)
	qty, _ := decimal.NewFromString(qtyStr)
	
	trade := Trade{
		Price:      price,
		Quantity:   qty,
		Time:       int64(tradeTime),
		IsBuyMaker: isBuyMaker,
	}
	
	// Update current price
	c.mu.Lock()
	c.currentPrice = price
	c.trades = append(c.trades, trade)
	if len(c.trades) > 1000 {
		c.trades = c.trades[500:]
	}
	c.mu.Unlock()
	
	// Add to buffer
	c.bufferMu.Lock()
	c.priceBuffer = append(c.priceBuffer, PricePoint{
		Price:     price,
		Volume:    qty,
		Timestamp: time.Now(),
	})
	// Keep last 5 minutes of data
	cutoff := time.Now().Add(-5 * time.Minute)
	for len(c.priceBuffer) > 0 && c.priceBuffer[0].Timestamp.Before(cutoff) {
		c.priceBuffer = c.priceBuffer[1:]
	}
	c.bufferMu.Unlock()
	
	// Callbacks
	if c.onPrice != nil {
		c.onPrice(price)
	}
	if c.onTrade != nil {
		c.onTrade(trade)
	}
}

func (c *Client) reconnectLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ticker.C:
			// Check if we're receiving data
			c.mu.RLock()
			price := c.currentPrice
			c.mu.RUnlock()
			
			if price.IsZero() && c.running {
				log.Warn().Msg("No price data, triggering reconnect")
				select {
				case c.reconnectCh <- struct{}{}:
				default:
				}
			}
		case <-c.stopCh:
			return
		}
	}
}

func (c *Client) fetchInitialData() error {
	// Fetch recent klines for indicator calculation
	klines, err := c.GetKlines("BTCUSDT", "1m", 100)
	if err != nil {
		return err
	}
	
	// Populate buffer with kline close prices
	c.bufferMu.Lock()
	for _, k := range klines {
		c.priceBuffer = append(c.priceBuffer, PricePoint{
			Price:     k.Close,
			Volume:    k.Volume,
			Timestamp: time.Unix(k.CloseTime/1000, 0),
		})
	}
	c.bufferMu.Unlock()
	
	if len(klines) > 0 {
		c.mu.Lock()
		c.currentPrice = klines[len(klines)-1].Close
		c.mu.Unlock()
	}
	
	log.Info().Int("klines", len(klines)).Msg("Fetched initial price data")
	return nil
}

// GetKlines fetches historical klines via REST
func (c *Client) GetKlines(symbol, interval string, limit int) ([]Kline, error) {
	url := fmt.Sprintf("%s/api/v3/klines?symbol=%s&interval=%s&limit=%d",
		c.restURL, symbol, interval, limit)
	
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	var raw [][]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	
	klines := make([]Kline, len(raw))
	for i, k := range raw {
		openTime, _ := k[0].(float64)
		open, _ := decimal.NewFromString(k[1].(string))
		high, _ := decimal.NewFromString(k[2].(string))
		low, _ := decimal.NewFromString(k[3].(string))
		close, _ := decimal.NewFromString(k[4].(string))
		volume, _ := decimal.NewFromString(k[5].(string))
		closeTime, _ := k[6].(float64)
		
		klines[i] = Kline{
			OpenTime:  int64(openTime),
			Open:      open,
			High:      high,
			Low:       low,
			Close:     close,
			Volume:    volume,
			CloseTime: int64(closeTime),
		}
	}
	
	return klines, nil
}

// GetPriceAtTime fetches the BTC price at a specific timestamp using 1-second klines
// This is used to get the "Price to Beat" for Polymarket windows
// Returns the open price of the 1-second candle at the given timestamp
func (c *Client) GetPriceAtTime(t time.Time) (decimal.Decimal, error) {
	return c.GetAssetPriceAtTime("BTC", t)
}

// GetAssetPriceAtTime fetches any asset's price at a specific timestamp using 1-second klines
// asset should be "BTC", "ETH", or "SOL"
func (c *Client) GetAssetPriceAtTime(asset string, t time.Time) (decimal.Decimal, error) {
	// Map asset to Binance symbol
	symbol := asset + "USDT"
	
	// Convert to milliseconds for Binance API
	startMs := t.Unix() * 1000
	endMs := startMs + 1000 // Just 1 second
	
	url := fmt.Sprintf("%s/api/v3/klines?symbol=%s&interval=1s&startTime=%d&endTime=%d&limit=1",
		c.restURL, symbol, startMs, endMs)
	
	resp, err := http.Get(url)
	if err != nil {
		return decimal.Zero, fmt.Errorf("failed to fetch 1s kline: %w", err)
	}
	defer resp.Body.Close()
	
	var raw [][]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return decimal.Zero, fmt.Errorf("failed to decode kline: %w", err)
	}
	
	if len(raw) == 0 {
		return decimal.Zero, fmt.Errorf("no kline data for timestamp %d", t.Unix())
	}
	
	// Return the open price of the kline
	open, err := decimal.NewFromString(raw[0][1].(string))
	if err != nil {
		return decimal.Zero, fmt.Errorf("failed to parse price: %w", err)
	}
	
	return open, nil
}

// GetOrderBook fetches order book via REST
func (c *Client) GetOrderBook(symbol string, limit int) (*OrderBook, error) {
	url := fmt.Sprintf("%s/api/v3/depth?symbol=%s&limit=%d",
		c.restURL, symbol, limit)
	
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	var raw struct {
		Bids [][]string `json:"bids"`
		Asks [][]string `json:"asks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	
	book := &OrderBook{
		Bids: make([]OrderBookEntry, len(raw.Bids)),
		Asks: make([]OrderBookEntry, len(raw.Asks)),
	}
	
	for i, b := range raw.Bids {
		price, _ := decimal.NewFromString(b[0])
		qty, _ := decimal.NewFromString(b[1])
		book.Bids[i] = OrderBookEntry{Price: price, Quantity: qty}
	}
	for i, a := range raw.Asks {
		price, _ := decimal.NewFromString(a[0])
		qty, _ := decimal.NewFromString(a[1])
		book.Asks[i] = OrderBookEntry{Price: price, Quantity: qty}
	}
	
	c.mu.Lock()
	c.orderBook = book
	c.mu.Unlock()
	
	return book, nil
}

// GetFundingRate fetches perpetual funding rate
func (c *Client) GetFundingRate(symbol string) (decimal.Decimal, error) {
	url := fmt.Sprintf("https://fapi.binance.com/fapi/v1/fundingRate?symbol=%s&limit=1", symbol)
	
	resp, err := http.Get(url)
	if err != nil {
		return decimal.Zero, err
	}
	defer resp.Body.Close()
	
	var raw []struct {
		FundingRate string `json:"fundingRate"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return decimal.Zero, err
	}
	
	if len(raw) == 0 {
		return decimal.Zero, nil
	}
	
	rate, _ := decimal.NewFromString(raw[0].FundingRate)
	return rate, nil
}

// GetCurrentPrice returns the current BTC price
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

// GetRecentTrades returns recent trades
func (c *Client) GetRecentTrades() []Trade {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	result := make([]Trade, len(c.trades))
	copy(result, c.trades)
	return result
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

// GetVolumeInPeriod calculates total volume in duration
func (c *Client) GetVolumeInPeriod(duration time.Duration) decimal.Decimal {
	c.bufferMu.RLock()
	defer c.bufferMu.RUnlock()
	
	cutoff := time.Now().Add(-duration)
	volume := decimal.Zero
	
	for _, p := range c.priceBuffer {
		if p.Timestamp.After(cutoff) {
			volume = volume.Add(p.Volume)
		}
	}
	
	return volume
}

// GetBuySellRatio calculates buy/sell volume ratio
func (c *Client) GetBuySellRatio(duration time.Duration) decimal.Decimal {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	cutoff := time.Now().Add(-duration).UnixMilli()
	buyVol := decimal.Zero
	sellVol := decimal.Zero
	
	for _, t := range c.trades {
		if t.Time > cutoff {
			if t.IsBuyMaker {
				sellVol = sellVol.Add(t.Quantity)
			} else {
				buyVol = buyVol.Add(t.Quantity)
			}
		}
	}
	
	if sellVol.IsZero() {
		return decimal.NewFromInt(100)
	}
	
	return buyVol.Div(sellVol)
}

// GetPrices returns prices as float64 slice for indicator calculation
func (c *Client) GetPrices(count int) []float64 {
	c.bufferMu.RLock()
	defer c.bufferMu.RUnlock()
	
	start := 0
	if len(c.priceBuffer) > count {
		start = len(c.priceBuffer) - count
	}
	
	prices := make([]float64, 0, count)
	for i := start; i < len(c.priceBuffer); i++ {
		f, _ := c.priceBuffer[i].Price.Float64()
		prices = append(prices, f)
	}
	
	return prices
}

// GetVolumes returns volumes as float64 slice
func (c *Client) GetVolumes(count int) []float64 {
	c.bufferMu.RLock()
	defer c.bufferMu.RUnlock()
	
	start := 0
	if len(c.priceBuffer) > count {
		start = len(c.priceBuffer) - count
	}
	
	volumes := make([]float64, 0, count)
	for i := start; i < len(c.priceBuffer); i++ {
		f, _ := c.priceBuffer[i].Volume.Float64()
		volumes = append(volumes, f)
	}
	
	return volumes
}

// IsConnected returns connection status
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.running && !c.currentPrice.IsZero()
}

// ParseDecimal helper
func ParseDecimal(s string) decimal.Decimal {
	d, _ := decimal.NewFromString(s)
	return d
}

// ParseFloat helper
func ParseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
