package feeds

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ═══════════════════════════════════════════════════════════════════════════════
// WINDOW SCANNER - Tracks active 15-minute crypto windows
// ═══════════════════════════════════════════════════════════════════════════════
//
// Scans Polymarket for:
//   - BTC Above $X in 15 minutes?
//   - ETH Above $X in 15 minutes?
//   - SOL Above $X in 15 minutes?
//
// Tracks:
//   - Window end time (for "time remaining" calculation)
//   - Price to beat (from Polymarket question)
//   - Binance start price (snapshot when window detected)
//   - Current odds (YES/NO)
//
// Price Discovery:
//   - Polymarket uses Chainlink Data Streams (paid)
//   - We use Binance spot price (close enough, free, 100ms)
//   - Snapshot Binance price when window first detected
//   - Store in DB for historical analysis
//
// ═══════════════════════════════════════════════════════════════════════════════

const (
	polymarketAPI = "https://gamma-api.polymarket.com"
)

// SnapshotSaver interface for database
type SnapshotSaver interface {
	SaveWindowSnapshot(marketID, asset string, priceToBeat, binancePrice, yesPrice, noPrice decimal.Decimal, windowEnd time.Time) error
	UpdateWindowOutcome(marketID string, binanceEndPrice decimal.Decimal, outcome string) error
	GetWindowStartPrice(marketID string) (decimal.Decimal, bool)
}

// BinanceHistorical interface for getting historical prices
type BinanceHistorical interface {
	GetHistoricalPrice(symbol string, timestamp int64) (decimal.Decimal, error)
}

// Window represents an active 15-minute market window
type Window struct {
	ID            string          // Market/condition ID
	Asset         string          // "BTC", "ETH", "SOL"
	PriceToBeat   decimal.Decimal // e.g., 105000 for "BTC > $105,000"
	EndTime       time.Time       // When the window closes
	YesTokenID    string          // Token ID for YES outcome
	NoTokenID     string          // Token ID for NO outcome
	YesPrice      decimal.Decimal // Current YES odds
	NoPrice       decimal.Decimal // Current NO odds
	Question      string          // Full question text
	StartPrice    decimal.Decimal // Binance price at window detection (cached)
	LastUpdated   time.Time
}

// TimeRemaining returns duration until window closes
func (w *Window) TimeRemaining() time.Duration {
	return time.Until(w.EndTime)
}

// TimeRemainingSeconds returns seconds until window closes
func (w *Window) TimeRemainingSeconds() float64 {
	return w.TimeRemaining().Seconds()
}

// IsInSniperZone returns true if window is in last 15-60 seconds
func (w *Window) IsInSniperZone(minSec, maxSec float64) bool {
	remaining := w.TimeRemainingSeconds()
	return remaining >= minSec && remaining <= maxSec
}

// IsExpired returns true if window has ended
func (w *Window) IsExpired() bool {
	return time.Now().After(w.EndTime)
}

// PriceFeed interface for price sources
type PriceFeed interface {
	GetPrice(symbol string) decimal.Decimal
}

// PolyFeed interface for live odds updates
type PolyFeed interface {
	SubscribeMarket(market string) error
	Subscribe() chan Tick
}

// WindowScanner manages window discovery and tracking
type WindowScanner struct {
	mu      sync.RWMutex
	running bool
	stopCh  chan struct{}

	// Active windows by market ID
	windows map[string]*Window
	
	// Token ID to Window mapping for fast lookups
	tokenToWindow map[string]*Window

	// Price feed (Chainlink or Binance) for current prices
	priceFeed PriceFeed

	// Binance feed for historical prices
	binanceFeed BinanceHistorical
	
	// Polymarket feed for live odds
	polyFeed PolyFeed

	// Database for snapshots (optional)
	db SnapshotSaver

	// Subscribers
	subscribers []chan *Window
}

// NewWindowScanner creates a new scanner
func NewWindowScanner(priceFeed PriceFeed) *WindowScanner {
	return &WindowScanner{
		stopCh:        make(chan struct{}),
		windows:       make(map[string]*Window),
		tokenToWindow: make(map[string]*Window),
		priceFeed:     priceFeed,
		subscribers:   make([]chan *Window, 0),
	}
}

// SetPolyFeed attaches polymarket feed for live odds
func (s *WindowScanner) SetPolyFeed(feed PolyFeed) {
	s.mu.Lock()
	s.polyFeed = feed
	s.mu.Unlock()
	
	// Start listening for price updates
	go s.listenOddsUpdates()
}

// SetBinanceFeed attaches binance feed for historical prices
func (s *WindowScanner) SetBinanceFeed(feed BinanceHistorical) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.binanceFeed = feed
}

// SetDatabase attaches database for snapshot storage
func (s *WindowScanner) SetDatabase(db SnapshotSaver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db = db
}

// Start begins scanning for windows
func (s *WindowScanner) Start() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()

	go s.scanLoop()
	log.Info().Msg("🔍 Window scanner started")
}

// Stop stops the scanner
func (s *WindowScanner) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return
	}

	s.running = false
	close(s.stopCh)
	log.Info().Msg("Window scanner stopped")
}

// Subscribe returns a channel that receives window updates
func (s *WindowScanner) Subscribe() chan *Window {
	s.mu.Lock()
	defer s.mu.Unlock()

	ch := make(chan *Window, 100)
	s.subscribers = append(s.subscribers, ch)
	return ch
}

// GetWindow returns a window by ID
func (s *WindowScanner) GetWindow(marketID string) *Window {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.windows[marketID]
}

// GetActiveWindows returns all non-expired windows
func (s *WindowScanner) GetActiveWindows() []*Window {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*Window
	for _, w := range s.windows {
		if !w.IsExpired() {
			result = append(result, w)
		}
	}
	return result
}

// GetSniperReadyWindows returns windows in sniper zone
func (s *WindowScanner) GetSniperReadyWindows(minSec, maxSec float64) []*Window {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []*Window
	for _, w := range s.windows {
		if w.IsInSniperZone(minSec, maxSec) {
			result = append(result, w)
		}
	}
	return result
}

// scanLoop - Smart window management
// Windows are PREDICTABLE: they start every 15 minutes exactly
// We capture the Chainlink price at EXACT window start time as PriceToBeat
func (s *WindowScanner) scanLoop() {
	assets := []string{"btc", "eth", "sol"}
	interval := int64(900) // 15 minutes

	// Initial fetch of current window
	s.fetchCurrentWindows(assets)

	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		now := time.Now().Unix()
		currentWindowStart := (now / interval) * interval
		nextWindowStart := currentWindowStart + interval
		timeUntilNext := nextWindowStart - now

		// Clean up expired windows
		s.cleanupExpired()

		// Wait until EXACTLY when next window starts to capture price to beat
		// The price to beat is the Chainlink price at the exact start moment
		sleepDuration := time.Duration(timeUntilNext) * time.Second
		if sleepDuration < time.Second {
			sleepDuration = time.Second
		}

		select {
		case <-s.stopCh:
			return
		case <-time.After(sleepDuration):
			// Capture price to beat AT the exact window start
			s.captureWindowStart(assets, nextWindowStart)
		}
	}
}

// captureWindowStart captures Chainlink price at exact window start (= price to beat)
func (s *WindowScanner) captureWindowStart(assets []string, windowStart int64) {
	for _, asset := range assets {
		assetUpper := strings.ToUpper(asset)
		
		// Get Chainlink price RIGHT NOW (this is the price to beat)
		priceToBeat := s.priceFeed.GetPrice(assetUpper)
		
		log.Info().
			Str("asset", assetUpper).
			Str("price_to_beat", priceToBeat.StringFixed(2)).
			Int64("window_start", windowStart).
			Msg("📍 Captured price to beat")
		
		// Fetch window from API with price to beat
		s.fetchUpDownWindowWithPrice(asset, windowStart, priceToBeat)
	}

	log.Info().
		Int64("window_start", windowStart).
		Int("assets", len(assets)).
		Msg("📊 New window cycle started")
}

// fetchCurrentWindows fetches current window for each asset
func (s *WindowScanner) fetchCurrentWindows(assets []string) {
	now := time.Now().Unix()
	interval := int64(900)
	currentWindowStart := (now / interval) * interval

	for _, asset := range assets {
		assetUpper := strings.ToUpper(asset)
		// Get current Chainlink price as approximate price to beat
		// (we missed the exact start, so use current as approximation)
		priceToBeat := s.priceFeed.GetPrice(assetUpper)
		s.fetchUpDownWindowWithPrice(asset, currentWindowStart, priceToBeat)
	}

	log.Info().
		Int64("window_start", currentWindowStart).
		Int("assets", len(assets)).
		Msg("📊 Windows synced")
}

// fetchUpDownWindow fetches a specific 15-minute up/down window by slug
func (s *WindowScanner) fetchUpDownWindow(asset string, startTimestamp int64) {
	s.fetchUpDownWindowWithPrice(asset, startTimestamp, decimal.Zero)
}

// fetchUpDownWindowWithPrice fetches window with a specific price to beat
func (s *WindowScanner) fetchUpDownWindowWithPrice(asset string, startTimestamp int64, priceToBeat decimal.Decimal) {
	slug := fmt.Sprintf("%s-updown-15m-%d", asset, startTimestamp)
	url := fmt.Sprintf("%s/events?slug=%s", polymarketAPI, slug)

	resp, err := http.Get(url)
	if err != nil {
		log.Debug().Err(err).Str("slug", slug).Msg("Failed to fetch window")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	
	var events []struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		Slug    string `json:"slug"`
		EndDate string `json:"endDate"`
		Markets []struct {
			ID            string `json:"id"`
			ConditionID   string `json:"conditionId"`
			Question      string `json:"question"`
			OutcomePrices string `json:"outcomePrices"` // "[\"0.55\", \"0.45\"]"
			Outcomes      string `json:"outcomes"`      // "[\"Up\", \"Down\"]"
			ClobTokenIds  string `json:"clobTokenIds"`  // "[\"tokenYes\", \"tokenNo\"]"
			Active        bool   `json:"active"`
			Closed        bool   `json:"closed"`
		} `json:"markets"`
	}

	if err := json.Unmarshal(body, &events); err != nil || len(events) == 0 {
		return
	}

	event := events[0]
	if len(event.Markets) == 0 {
		return
	}

	market := event.Markets[0]
	if !market.Active || market.Closed {
		return
	}

	// Parse outcome prices - format: "[\"0.595\", \"0.405\"]"
	var prices []string
	if err := json.Unmarshal([]byte(market.OutcomePrices), &prices); err != nil || len(prices) < 2 {
		log.Debug().Str("prices", market.OutcomePrices).Msg("Failed to parse prices")
		return
	}

	yesPrice, _ := decimal.NewFromString(prices[0]) // UP price
	noPrice, _ := decimal.NewFromString(prices[1])  // DOWN price

	// Parse token IDs
	var tokenIDs []string
	if err := json.Unmarshal([]byte(market.ClobTokenIds), &tokenIDs); err != nil || len(tokenIDs) < 2 {
		log.Debug().Str("tokens", market.ClobTokenIds).Msg("Failed to parse token IDs")
		return
	}

	// Parse end time from the event's endDate field
	endTime, err := time.Parse(time.RFC3339, event.EndDate)
	if err != nil {
		log.Debug().Str("endDate", event.EndDate).Msg("Failed to parse end date")
		return
	}
	
	// Skip if already expired
	if time.Now().After(endTime) {
		return
	}

	assetUpper := strings.ToUpper(asset)
	
	// Get start price: first check DB, then get from Binance historical API
	// The slug timestamp is the MARKET CREATION time (window start)
	var startPrice decimal.Decimal
	
	// Check if we already have this window stored
	s.mu.RLock()
	_, exists := s.windows[market.ConditionID]
	db := s.db
	binanceFeed := s.binanceFeed
	s.mu.RUnlock()
	
	if !exists {
		// New window - get the historical price at market creation time
		// First check DB
		if db != nil {
			if storedPrice, found := db.GetWindowStartPrice(market.ConditionID); found {
				startPrice = storedPrice
			}
		}
		
		// If not in DB, get from Binance historical API
		if startPrice.IsZero() && binanceFeed != nil {
			histPrice, err := binanceFeed.GetHistoricalPrice(assetUpper, startTimestamp)
			if err == nil && !histPrice.IsZero() {
				startPrice = histPrice
				log.Debug().
					Str("asset", assetUpper).
					Int64("ts", startTimestamp).
					Str("price", startPrice.StringFixed(2)).
					Msg("Got historical start price")
			}
		}
		
		// Fallback to current price if historical lookup failed
		if startPrice.IsZero() {
			startPrice = s.priceFeed.GetPrice(assetUpper)
		}
		
		// If price to beat was passed, use it; otherwise use startPrice
		if priceToBeat.IsZero() {
			priceToBeat = startPrice
		}
	} else {
		// Existing window - just get current prices from our cache
		s.mu.RLock()
		startPrice = s.windows[market.ConditionID].StartPrice
		priceToBeat = s.windows[market.ConditionID].PriceToBeat
		s.mu.RUnlock()
	}

	window := &Window{
		ID:          market.ConditionID,
		Asset:       assetUpper,
		PriceToBeat: priceToBeat,
		EndTime:     endTime,
		YesTokenID:  tokenIDs[0], // UP token
		NoTokenID:   tokenIDs[1], // DOWN token
		YesPrice:    yesPrice,    // UP price (probability it goes up)
		NoPrice:     noPrice,     // DOWN price (probability it goes down)
		Question:    market.Question,
		StartPrice:  startPrice,
		LastUpdated: time.Now(),
	}

	s.updateWindow(window)
}

// updateWindow adds or updates a window
func (s *WindowScanner) updateWindow(window *Window) {
	s.mu.Lock()
	existing, exists := s.windows[window.ID]
	isNew := !exists
	if isNew {
		// New window - cache the start price from Binance
		s.windows[window.ID] = window
	} else {
		// Update prices only
		existing.YesPrice = window.YesPrice
		existing.NoPrice = window.NoPrice
		existing.LastUpdated = time.Now()
	}
	db := s.db
	s.mu.Unlock()

	// Save snapshot to database for new windows
	if isNew {
		log.Info().
			Str("asset", window.Asset).
			Str("price_to_beat", window.PriceToBeat.StringFixed(2)).
			Str("up", window.YesPrice.StringFixed(2)).
			Str("down", window.NoPrice.StringFixed(2)).
			Float64("remaining_sec", window.TimeRemainingSeconds()).
			Msg("🎯 Window ready")

		// Save to DB if available
		if db != nil {
			if err := db.SaveWindowSnapshot(
				window.ID,
				window.Asset,
				window.PriceToBeat,
				window.StartPrice,
				window.YesPrice,
				window.NoPrice,
				window.EndTime,
			); err != nil {
				log.Warn().Err(err).Msg("Failed to save window snapshot")
			}
		}
	}

	// Broadcast to subscribers
	s.broadcast(window)
	
	// Subscribe to market for live odds (only for new windows)
	if isNew {
		s.mu.RLock()
		polyFeed := s.polyFeed
		s.mu.RUnlock()
		
		if polyFeed != nil {
			// Subscribe using the YES token ID (market ID)
			go polyFeed.SubscribeMarket(window.YesTokenID)
		}
	}
}

// listenOddsUpdates listens for live odds updates from WebSocket
func (s *WindowScanner) listenOddsUpdates() {
	s.mu.RLock()
	polyFeed := s.polyFeed
	s.mu.RUnlock()
	
	if polyFeed == nil {
		return
	}
	
	tickCh := polyFeed.Subscribe()
	
	for {
		select {
		case <-s.stopCh:
			return
		case tick := <-tickCh:
			s.handleOddsUpdate(tick)
		}
	}
}

// handleOddsUpdate processes a live odds update
func (s *WindowScanner) handleOddsUpdate(tick Tick) {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	// Find window by token ID
	window, exists := s.tokenToWindow[tick.Asset]
	if !exists {
		// Try looking up in windows map
		for _, w := range s.windows {
			if w.YesTokenID == tick.Asset || w.NoTokenID == tick.Asset {
				window = w
				// Cache for fast lookup
				s.tokenToWindow[tick.Asset] = w
				break
			}
		}
	}
	
	if window == nil {
		return
	}
	
	// Update odds based on which token this is
	if tick.Asset == window.YesTokenID {
		window.YesPrice = tick.Mid
	} else if tick.Asset == window.NoTokenID {
		window.NoPrice = tick.Mid
	}
	window.LastUpdated = time.Now()
}

// broadcast sends window to all subscribers
func (s *WindowScanner) broadcast(window *Window) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, ch := range s.subscribers {
		select {
		case ch <- window:
		default:
		}
	}
}

// cleanupExpired removes expired windows and records outcomes
func (s *WindowScanner) cleanupExpired() {
	s.mu.Lock()
	var expired []*Window
	for id, w := range s.windows {
		if w.IsExpired() {
			expired = append(expired, w)
			delete(s.windows, id)
		}
	}
	db := s.db
	pf := s.priceFeed
	s.mu.Unlock()

	// Record outcomes for expired windows
	for _, w := range expired {
		// Get final price from Chainlink feed
		endPrice := pf.GetPrice(w.Asset)

		// Determine outcome
		outcome := "NO"
		if endPrice.GreaterThanOrEqual(w.PriceToBeat) {
			outcome = "YES"
		}

		log.Debug().
			Str("asset", w.Asset).
			Str("outcome", outcome).
			Str("end_price", endPrice.StringFixed(2)).
			Str("target", w.PriceToBeat.StringFixed(0)).
			Msg("Window expired")

		// Update database
		if db != nil {
			db.UpdateWindowOutcome(w.ID, endPrice, outcome)
		}
	}
}

// extractPriceFromQuestion parses "BTC above $105,000" -> 105000
func extractPriceFromQuestion(question string) decimal.Decimal {
	// Look for $ followed by numbers
	// Examples:
	//   "BTC above $105,000 in 15 minutes"
	//   "ETH above $3,500 in 15 minutes"

	parts := strings.Split(question, "$")
	if len(parts) < 2 {
		return decimal.Zero
	}

	// Get the price part after $
	pricePart := parts[1]

	// Extract digits and commas
	var priceStr strings.Builder
	for _, c := range pricePart {
		if c >= '0' && c <= '9' {
			priceStr.WriteRune(c)
		} else if c == ',' {
			continue // Skip commas
		} else if c == '.' {
			priceStr.WriteRune(c)
		} else {
			break // Stop at first non-digit
		}
	}

	price, err := decimal.NewFromString(priceStr.String())
	if err != nil {
		return decimal.Zero
	}
	return price
}
