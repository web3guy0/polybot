// Package polymarket provides Polymarket API integration
//
// window_scanner.go - Scans for crypto prediction windows on Polymarket
// Searches for "Will [ASSET] go up/down in the next X minutes?" markets
// Asset is configurable via constructor (BTC, ETH, SOL, etc.)
package polymarket

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

// PredictionWindow represents a crypto up/down prediction window market
type PredictionWindow struct {
	ID            string
	ConditionID   string
	Question      string
	Slug          string
	Asset         string // BTC, ETH, SOL, etc.

	// Tokens
	YesTokenID string
	NoTokenID  string

	// Prices (YES = "Up", NO = "Down")
	YesPrice decimal.Decimal // Price for "Up" outcome
	NoPrice  decimal.Decimal // Price for "Down" outcome

	// Price to Beat - the reference price from Polymarket!
	PriceToBeat decimal.Decimal // Starting price for resolution
	CurrentPrice decimal.Decimal // Current price from Polymarket's feed

	// Market info
	Volume    decimal.Decimal
	Liquidity decimal.Decimal
	StartDate time.Time // Window start time (used to get "Price to Beat")
	EndDate   time.Time
	Active    bool
	Closed    bool

	// Parsed info
	WindowMinutes int    // 15, 60, etc.
	WindowType    string // "up_down", "price_range", etc.

	LastUpdated time.Time
}

// WindowScanner scans for crypto prediction window markets
type WindowScanner struct {
	client       *Client
	restURL      string
	asset        string // The asset to scan for (BTC, ETH, etc.)
	priceFetcher *CLOBPriceFetcher // For real-time CLOB prices
	wsClient     *WSClient         // WebSocket for real-time odds (FAST!)

	windows   []PredictionWindow
	windowsMu sync.RWMutex

	onNewWindow func(PredictionWindow)

	running bool
	stopCh  chan struct{}
}

// NewWindowScanner creates a new scanner for the given asset
func NewWindowScanner(apiURL string, asset string) *WindowScanner {
	return &WindowScanner{
		client:       NewClient(apiURL),
		restURL:      apiURL,
		asset:        strings.ToUpper(asset),
		windows:      make([]PredictionWindow, 0),
		stopCh:       make(chan struct{}),
		priceFetcher: NewCLOBPriceFetcher(), // Add CLOB price fetcher
	}
}

// SetNewWindowCallback sets callback for new windows
func (s *WindowScanner) SetNewWindowCallback(cb func(PredictionWindow)) {
	s.onNewWindow = cb
}

// SetWSClient sets the WebSocket client for real-time odds
func (s *WindowScanner) SetWSClient(ws *WSClient) {
	s.wsClient = ws
}

// Start begins scanning for prediction windows
func (s *WindowScanner) Start() {
	s.running = true
	go s.scanLoop()
	log.Info().Str("asset", s.asset).Msg("🔍 Window Scanner started")
}

// Stop stops the scanner
func (s *WindowScanner) Stop() {
	s.running = false
	close(s.stopCh)
}

func (s *WindowScanner) scanLoop() {
	// Scan immediately
	s.scan()

	// Scan every 200ms for ULTRA-FAST odds detection - latency is everything!
	ticker := time.NewTicker(250 * time.Millisecond) // Scan windows every 250ms for FAST detection
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.scan()
		case <-s.stopCh:
			return
		}
	}
}

func (s *WindowScanner) scan() {
	windows, err := s.fetchWindows()
	if err != nil {
		log.Error().Err(err).Str("asset", s.asset).Msg("Failed to fetch windows")
		return
	}

	// CRITICAL: Fetch LIVE prices from WebSocket first (FASTEST!), fallback to CLOB REST
	for i := range windows {
		if windows[i].YesTokenID != "" && windows[i].NoTokenID != "" {
			// Try WebSocket first (instant, no network latency!)
			if s.wsClient != nil && s.wsClient.IsConnected() {
				upPrice, downPrice, ok := s.wsClient.GetMarketPrices(windows[i].YesTokenID, windows[i].NoTokenID)
				if ok && !upPrice.IsZero() {
					windows[i].YesPrice = upPrice
					windows[i].NoPrice = downPrice
					// Subscribe if not already (for future updates)
					s.wsClient.Subscribe(windows[i].ConditionID, windows[i].YesTokenID, windows[i].NoTokenID)
					continue // Got WS price, skip REST
				}
				// No WS price yet - subscribe for next time
				s.wsClient.Subscribe(windows[i].ConditionID, windows[i].YesTokenID, windows[i].NoTokenID)
			}
			
			// Fallback to REST (slower, only if WS didn't have price)
			upPrice, downPrice, err := s.priceFetcher.GetLivePrices(windows[i].YesTokenID, windows[i].NoTokenID)
			if err == nil && !upPrice.IsZero() {
				windows[i].YesPrice = upPrice
				windows[i].NoPrice = downPrice
			}
		}
	}

	log.Debug().Int("found", len(windows)).Str("asset", s.asset).Msg("🔍 Windows scan complete")

	s.windowsMu.Lock()
	oldWindows := make(map[string]bool)
	for _, w := range s.windows {
		oldWindows[w.ID] = true
	}

	s.windows = windows
	s.windowsMu.Unlock()

	// Notify about new windows
	for _, w := range windows {
		if !oldWindows[w.ID] && s.onNewWindow != nil {
			s.onNewWindow(w)
		}
		// Log each window found
		log.Debug().
			Str("id", w.ID).
			Str("question", w.Question[:min(50, len(w.Question))]).
			Str("up", w.YesPrice.String()).
			Str("down", w.NoPrice.String()).
			Msg("📊 Window")
	}

	log.Debug().Int("windows", len(windows)).Str("asset", s.asset).Msg("Windows updated")
}

func (s *WindowScanner) fetchWindows() ([]PredictionWindow, error) {
	// Polymarket crypto windows use timestamp-based slugs:
	// btc-updown-15m-{timestamp} where timestamp is Unix time aligned to the window interval
	// Example: btc-updown-15m-1767707100 for the 15-minute window starting at that time

	now := time.Now().Unix()
	allWindows := make([]PredictionWindow, 0)
	seen := make(map[string]bool)

	// Window types: (prefix, interval in seconds)
	windowTypes := []struct {
		suffix   string
		interval int64
	}{
		{"5m", 300},    // 5 minutes
		{"15m", 900},   // 15 minutes
		{"1h", 3600},   // 1 hour
		{"4h", 14400},  // 4 hours
	}

	// Asset slug prefixes
	assetPrefix := strings.ToLower(s.asset) + "-updown"

	for _, wt := range windowTypes {
		// Calculate current window timestamp (aligned to interval)
		windowTs := (now / wt.interval) * wt.interval
		slug := fmt.Sprintf("%s-%s-%d", assetPrefix, wt.suffix, windowTs)

		window, err := s.fetchWindowBySlug(slug)
		if err != nil {
			log.Debug().Str("slug", slug).Err(err).Msg("Window not found")
			continue
		}

		if window != nil && !seen[window.ID] {
			seen[window.ID] = true
			allWindows = append(allWindows, *window)
		}
	}

	return allWindows, nil
}

// fetchWindowBySlug fetches a single window by its slug from gamma API
func (s *WindowScanner) fetchWindowBySlug(slug string) (*PredictionWindow, error) {
	// Use gamma-api.polymarket.com/events endpoint
	eventsURL := fmt.Sprintf("https://gamma-api.polymarket.com/events?slug=%s", slug)

	resp, err := http.Get(eventsURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var events []struct {
		ID             string `json:"id"`
		Title          string `json:"title"`
		Slug           string `json:"slug"`
		Description    string `json:"description"` // Contains price to beat info!
		Active         bool   `json:"active"`
		Closed         bool   `json:"closed"`
		EndDate        string `json:"endDate"`
		StartTime      string `json:"startTime"`      // Event start time (when window opens)
		EventStartTime string `json:"eventStartTime"` // Alternative field name
		Markets []struct {
			ID             string `json:"id"`
			ConditionID    string `json:"conditionId"`
			Question       string `json:"question"`
			Description    string `json:"description"` // May contain current price info
			Outcomes       string `json:"outcomes"`
			OutcomePrices  string `json:"outcomePrices"`
			ClobTokenIds   string `json:"clobTokenIds"`
			Volume         string `json:"volume"`
			EventStartTime string `json:"eventStartTime"` // Market-level start time
		} `json:"markets"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, err
	}

	if len(events) == 0 || len(events[0].Markets) == 0 {
		return nil, nil
	}

	event := events[0]
	market := event.Markets[0]

	// Skip markets with no prices (no liquidity)
	if market.OutcomePrices == "" || market.OutcomePrices == "null" {
		return nil, nil
	}

	// Parse outcomes ["Up", "Down"]
	var outcomes []string
	if err := json.Unmarshal([]byte(market.Outcomes), &outcomes); err != nil {
		return nil, err
	}

	// Parse prices ["0.51", "0.49"]
	var prices []string
	if err := json.Unmarshal([]byte(market.OutcomePrices), &prices); err != nil {
		return nil, err
	}

	// Parse token IDs
	var tokenIDs []string
	if err := json.Unmarshal([]byte(market.ClobTokenIds), &tokenIDs); err != nil {
		return nil, err
	}

	if len(prices) < 2 || len(tokenIDs) < 2 {
		return nil, nil
	}

	yesPrice, _ := decimal.NewFromString(prices[0])
	noPrice, _ := decimal.NewFromString(prices[1])
	volume, _ := decimal.NewFromString(market.Volume)

	// Parse end date
	var endDate time.Time
	if event.EndDate != "" {
		endDate, _ = time.Parse(time.RFC3339, event.EndDate)
	}

	// Parse start date (event start time = when window opens)
	var startDate time.Time
	if market.EventStartTime != "" {
		startDate, _ = time.Parse(time.RFC3339, market.EventStartTime)
	} else if event.StartTime != "" {
		startDate, _ = time.Parse(time.RFC3339, event.StartTime)
	} else if event.EventStartTime != "" {
		startDate, _ = time.Parse(time.RFC3339, event.EventStartTime)
	}

	// Detect window minutes from slug
	windowMinutes := 15
	if strings.Contains(slug, "-5m-") {
		windowMinutes = 5
	} else if strings.Contains(slug, "-1h-") {
		windowMinutes = 60
	} else if strings.Contains(slug, "-4h-") {
		windowMinutes = 240
	}

	// Extract price to beat from description
	// Format: "Price to beat: $90,385.67" or similar
	priceToBeat := s.extractPriceToBeat(event.Description + " " + market.Description)

	return &PredictionWindow{
		ID:            market.ID,
		ConditionID:   market.ConditionID,
		Question:      event.Title,
		Slug:          event.Slug,
		Asset:         s.asset,
		YesTokenID:    tokenIDs[0], // Up token
		NoTokenID:     tokenIDs[1], // Down token
		YesPrice:      yesPrice,
		NoPrice:       noPrice,
		PriceToBeat:   priceToBeat,
		Volume:        volume,
		StartDate:     startDate,
		EndDate:       endDate,
		Active:        event.Active,
		Closed:        event.Closed,
		WindowMinutes: windowMinutes,
		WindowType:    "up_down",
		LastUpdated:   time.Now(),
	}, nil
}

// matchesAsset checks if a question matches the scanner's asset
func (s *WindowScanner) matchesAsset(questionLower string) bool {
	switch s.asset {
	case "BTC":
		return strings.Contains(questionLower, "bitcoin") || strings.Contains(questionLower, "btc")
	case "ETH":
		return strings.Contains(questionLower, "ethereum") || strings.Contains(questionLower, "eth")
	case "SOL":
		return strings.Contains(questionLower, "solana") || strings.Contains(questionLower, "sol")
	default:
		return strings.Contains(questionLower, strings.ToLower(s.asset))
	}
}

// GetActiveWindows returns currently active prediction windows with REAL-TIME WebSocket prices
func (s *WindowScanner) GetActiveWindows() []PredictionWindow {
	s.windowsMu.RLock()
	defer s.windowsMu.RUnlock()

	active := make([]PredictionWindow, 0)
	now := time.Now()

	for _, w := range s.windows {
		// Only include windows that haven't ended
		if w.Active && !w.Closed && (w.EndDate.IsZero() || w.EndDate.After(now)) {
			// Get REAL-TIME prices from WebSocket (instant, no network latency!)
			if s.wsClient != nil && s.wsClient.IsConnected() && w.YesTokenID != "" {
				if upPrice, downPrice, ok := s.wsClient.GetMarketPrices(w.YesTokenID, w.NoTokenID); ok {
					w.YesPrice = upPrice
					w.NoPrice = downPrice
				}
			}
			active = append(active, w)
		}
	}

	return active
}

// GetWindowByID returns a specific window
func (s *WindowScanner) GetWindowByID(id string) *PredictionWindow {
	s.windowsMu.RLock()
	defer s.windowsMu.RUnlock()

	for _, w := range s.windows {
		if w.ID == id {
			return &w
		}
	}
	return nil
}

// GetBestWindow returns the window with best liquidity/odds
func (s *WindowScanner) GetBestWindow() *PredictionWindow {
	windows := s.GetActiveWindows()
	if len(windows) == 0 {
		return nil
	}

	// Find window with highest volume (most liquid)
	best := &windows[0]
	for i := range windows {
		if windows[i].Volume.GreaterThan(best.Volume) {
			best = &windows[i]
		}
	}

	return best
}

// extractPriceToBeat extracts price from description text
// Looks for patterns like "$90,385.67" or "price to beat: $3,080.45"
func (s *WindowScanner) extractPriceToBeat(text string) decimal.Decimal {
	// Common patterns in Polymarket descriptions
	// Pattern: $XX,XXX.XX or $XXXX.XX
	
	// Look for dollar amount after common keywords
	text = strings.ToLower(text)
	
	// Keywords that precede the price
	keywords := []string{
		"price to beat:",
		"price to beat",
		"starting price:",
		"starting price",
		"reference price:",
		"reference price",
	}
	
	for _, kw := range keywords {
		idx := strings.Index(text, kw)
		if idx >= 0 {
			// Extract text after keyword
			after := text[idx+len(kw):]
			price := s.parseFirstPrice(after)
			if !price.IsZero() {
				return price
			}
		}
	}
	
	return decimal.Zero
}

// parseFirstPrice extracts the first dollar price from text
func (s *WindowScanner) parseFirstPrice(text string) decimal.Decimal {
	// Find $ followed by digits
	start := strings.Index(text, "$")
	if start < 0 {
		return decimal.Zero
	}
	
	// Extract number after $
	numStr := ""
	for i := start + 1; i < len(text); i++ {
		c := text[i]
		if (c >= '0' && c <= '9') || c == '.' || c == ',' {
			if c != ',' { // Skip commas
				numStr += string(c)
			}
		} else if len(numStr) > 0 {
			break
		}
	}
	
	if numStr == "" {
		return decimal.Zero
	}
	
	price, err := decimal.NewFromString(numStr)
	if err != nil {
		return decimal.Zero
	}
	
	return price
}
