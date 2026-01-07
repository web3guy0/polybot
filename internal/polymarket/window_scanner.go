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
	client  *Client
	restURL string
	asset   string // The asset to scan for (BTC, ETH, etc.)

	windows   []PredictionWindow
	windowsMu sync.RWMutex

	onNewWindow func(PredictionWindow)

	running bool
	stopCh  chan struct{}
}

// NewWindowScanner creates a new scanner for the given asset
func NewWindowScanner(apiURL string, asset string) *WindowScanner {
	return &WindowScanner{
		client:  NewClient(apiURL),
		restURL: apiURL,
		asset:   strings.ToUpper(asset),
		windows: make([]PredictionWindow, 0),
		stopCh:  make(chan struct{}),
	}
}

// SetNewWindowCallback sets callback for new windows
func (s *WindowScanner) SetNewWindowCallback(cb func(PredictionWindow)) {
	s.onNewWindow = cb
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

	// Then scan every 5 seconds for fast window detection
	ticker := time.NewTicker(5 * time.Second)
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

	log.Info().Int("found", len(windows)).Str("asset", s.asset).Msg("🔍 Windows scan complete")

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
		Active         bool   `json:"active"`
		Closed         bool   `json:"closed"`
		EndDate        string `json:"endDate"`
		StartTime      string `json:"startTime"`      // Event start time (when window opens)
		EventStartTime string `json:"eventStartTime"` // Alternative field name
		Markets []struct {
			ID             string `json:"id"`
			ConditionID    string `json:"conditionId"`
			Question       string `json:"question"`
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

// GetActiveWindows returns currently active prediction windows
func (s *WindowScanner) GetActiveWindows() []PredictionWindow {
	s.windowsMu.RLock()
	defer s.windowsMu.RUnlock()

	active := make([]PredictionWindow, 0)
	now := time.Now()

	for _, w := range s.windows {
		// Only include windows that haven't ended
		if w.Active && !w.Closed && (w.EndDate.IsZero() || w.EndDate.After(now)) {
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
