// Package polymarket provides Polymarket API integration
//
// btc_scanner.go - LEGACY COMPONENT  
// This file scans for BTC-specific prediction windows on Polymarket.
// TODO: Generalize to scan for any asset windows (ETH, SOL, etc.)
// The asset should be configurable via config.TradingAsset.
// Search patterns should be dynamically built from asset name.
package polymarket

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// BTCWindow represents a BTC up/down prediction window market
type BTCWindow struct {
	ID            string
	ConditionID   string
	Question      string
	Slug          string
	
	// Tokens
	YesTokenID    string
	NoTokenID     string
	
	// Prices
	YesPrice      decimal.Decimal // Price for "Up" outcome
	NoPrice       decimal.Decimal // Price for "Down" outcome
	
	// Market info
	Volume        decimal.Decimal
	Liquidity     decimal.Decimal
	EndDate       time.Time
	Active        bool
	Closed        bool
	
	// Parsed info
	WindowMinutes int    // 15, 60, etc.
	WindowType    string // "up_down", "price_range", etc.
	
	LastUpdated   time.Time
}

// BTCWindowScanner scans for BTC window markets
type BTCWindowScanner struct {
	client     *Client
	restURL    string
	
	windows    []BTCWindow
	windowsMu  sync.RWMutex
	
	onNewWindow func(BTCWindow)
	
	running    bool
	stopCh     chan struct{}
}

// NewBTCWindowScanner creates a new scanner
func NewBTCWindowScanner(apiURL string) *BTCWindowScanner {
	return &BTCWindowScanner{
		client:  NewClient(apiURL),
		restURL: apiURL,
		windows: make([]BTCWindow, 0),
		stopCh:  make(chan struct{}),
	}
}

// SetNewWindowCallback sets callback for new windows
func (s *BTCWindowScanner) SetNewWindowCallback(cb func(BTCWindow)) {
	s.onNewWindow = cb
}

// Start begins scanning for BTC windows
func (s *BTCWindowScanner) Start() {
	s.running = true
	go s.scanLoop()
	log.Info().Msg("🔍 BTC Window Scanner started")
}

// Stop stops the scanner
func (s *BTCWindowScanner) Stop() {
	s.running = false
	close(s.stopCh)
}

func (s *BTCWindowScanner) scanLoop() {
	// Scan immediately
	s.scan()
	
	// Then scan every 30 seconds
	ticker := time.NewTicker(30 * time.Second)
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

func (s *BTCWindowScanner) scan() {
	windows, err := s.fetchBTCWindows()
	if err != nil {
		log.Error().Err(err).Msg("Failed to fetch BTC windows")
		return
	}
	
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
	}
	
	log.Debug().Int("windows", len(windows)).Msg("BTC windows updated")
}

func (s *BTCWindowScanner) fetchBTCWindows() ([]BTCWindow, error) {
	// Search for BTC/Bitcoin up/down markets
	searchTerms := []string{
		"Bitcoin",
		"BTC",
		"bitcoin up",
		"bitcoin down",
	}
	
	allWindows := make([]BTCWindow, 0)
	seen := make(map[string]bool)
	
	for _, term := range searchTerms {
		windows, err := s.searchMarkets(term)
		if err != nil {
			continue
		}
		
		for _, w := range windows {
			if !seen[w.ID] {
				seen[w.ID] = true
				allWindows = append(allWindows, w)
			}
		}
	}
	
	return allWindows, nil
}

func (s *BTCWindowScanner) searchMarkets(query string) ([]BTCWindow, error) {
	// Build search URL
	params := url.Values{}
	params.Set("closed", "false")
	params.Set("active", "true")
	params.Set("limit", "50")
	
	searchURL := fmt.Sprintf("%s/markets?%s", s.restURL, params.Encode())
	
	resp, err := http.Get(searchURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	var markets []Market
	if err := json.NewDecoder(resp.Body).Decode(&markets); err != nil {
		return nil, err
	}
	
	windows := make([]BTCWindow, 0)
	_ = strings.ToLower(query) // query parameter for future use
	
	for _, m := range markets {
		questionLower := strings.ToLower(m.Question)
		
		// Filter for BTC-related markets
		if !strings.Contains(questionLower, "bitcoin") && 
		   !strings.Contains(questionLower, "btc") {
			continue
		}
		
		// Look for up/down prediction markets
		isUpDown := strings.Contains(questionLower, "up") || 
		            strings.Contains(questionLower, "down") ||
		            strings.Contains(questionLower, "higher") ||
		            strings.Contains(questionLower, "lower") ||
		            strings.Contains(questionLower, "above") ||
		            strings.Contains(questionLower, "below")
		
		if !isUpDown {
			continue
		}
		
		window := s.parseMarketToWindow(m)
		if window != nil {
			windows = append(windows, *window)
		}
	}
	
	return windows, nil
}

func (s *BTCWindowScanner) parseMarketToWindow(m Market) *BTCWindow {
	// Parse outcomes and prices
	var outcomes []string
	if err := json.Unmarshal([]byte(m.Outcomes), &outcomes); err != nil {
		return nil
	}
	
	var prices []string
	if err := json.Unmarshal([]byte(m.OutcomePrices), &prices); err != nil {
		return nil
	}
	
	if len(outcomes) < 2 || len(prices) < 2 {
		return nil
	}
	
	yesPrice, _ := decimal.NewFromString(prices[0])
	noPrice, _ := decimal.NewFromString(prices[1])
	volume, _ := decimal.NewFromString(m.Volume)
	
	// Parse end date
	var endDate time.Time
	if m.EndDate != "" {
		endDate, _ = time.Parse(time.RFC3339, m.EndDate)
	}
	
	// Detect window type
	windowMinutes := 15 // Default
	questionLower := strings.ToLower(m.Question)
	if strings.Contains(questionLower, "1 hour") || strings.Contains(questionLower, "60 min") {
		windowMinutes = 60
	} else if strings.Contains(questionLower, "5 min") {
		windowMinutes = 5
	} else if strings.Contains(questionLower, "30 min") {
		windowMinutes = 30
	}
	
	return &BTCWindow{
		ID:            m.ID,
		ConditionID:   m.ConditionID,
		Question:      m.Question,
		Slug:          m.Slug,
		YesPrice:      yesPrice,
		NoPrice:       noPrice,
		Volume:        volume,
		EndDate:       endDate,
		Active:        m.Active,
		Closed:        m.Closed,
		WindowMinutes: windowMinutes,
		WindowType:    "up_down",
		LastUpdated:   time.Now(),
	}
}

// GetActiveWindows returns currently active BTC windows
func (s *BTCWindowScanner) GetActiveWindows() []BTCWindow {
	s.windowsMu.RLock()
	defer s.windowsMu.RUnlock()
	
	active := make([]BTCWindow, 0)
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
func (s *BTCWindowScanner) GetWindowByID(id string) *BTCWindow {
	s.windowsMu.RLock()
	defer s.windowsMu.RUnlock()
	
	for _, w := range s.windows {
		if w.ID == id {
			return &w
		}
	}
	return nil
}

// GetBestWindow returns the window with best odds for a direction
func (s *BTCWindowScanner) GetBestWindow(direction string) *BTCWindow {
	s.windowsMu.RLock()
	defer s.windowsMu.RUnlock()
	
	var best *BTCWindow
	bestPrice := decimal.NewFromFloat(1.0) // We want lowest price
	
	for _, w := range s.windows {
		if !w.Active || w.Closed {
			continue
		}
		
		var price decimal.Decimal
		if direction == "UP" {
			price = w.YesPrice
		} else {
			price = w.NoPrice
		}
		
		// Lower price = better odds = higher potential profit
		if price.LessThan(bestPrice) && price.GreaterThan(decimal.Zero) {
			bestPrice = price
			windowCopy := w
			best = &windowCopy
		}
	}
	
	return best
}

// FetchWindowPrices fetches latest prices for a window
func (s *BTCWindowScanner) FetchWindowPrices(windowID string) (*BTCWindow, error) {
	url := fmt.Sprintf("%s/markets/%s", s.restURL, windowID)
	
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	var m Market
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	
	return s.parseMarketToWindow(m), nil
}

// GetOddsForDirection calculates implied probability for a direction
func (w *BTCWindow) GetOddsForDirection(direction string) (price decimal.Decimal, impliedProb decimal.Decimal, potentialProfit decimal.Decimal) {
	if direction == "UP" {
		price = w.YesPrice
	} else {
		price = w.NoPrice
	}
	
	// Implied probability = price (e.g., 0.55 = 55% implied probability)
	impliedProb = price.Mul(decimal.NewFromInt(100))
	
	// Potential profit per $1 bet = (1/price) - 1
	if !price.IsZero() {
		potentialProfit = decimal.NewFromInt(1).Div(price).Sub(decimal.NewFromInt(1))
	}
	
	return price, impliedProb, potentialProfit
}

// FormatWindow formats a window for display
func (w *BTCWindow) FormatWindow() string {
	yesOdds := decimal.NewFromInt(1).Div(w.YesPrice).Sub(decimal.NewFromInt(1)).Mul(decimal.NewFromInt(100))
	noOdds := decimal.NewFromInt(1).Div(w.NoPrice).Sub(decimal.NewFromInt(1)).Mul(decimal.NewFromInt(100))
	
	return fmt.Sprintf(`📊 *%s*

🟢 UP: $%s (%s%% return)
🔴 DOWN: $%s (%s%% return)

📈 Volume: $%s
⏰ Ends: %s`,
		w.Question,
		w.YesPrice.StringFixed(2), yesOdds.StringFixed(0),
		w.NoPrice.StringFixed(2), noOdds.StringFixed(0),
		w.Volume.StringFixed(0),
		w.EndDate.Format("15:04 MST"),
	)
}
