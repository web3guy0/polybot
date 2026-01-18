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
//   - Price to beat (for % move calculation)
//   - Current odds (YES/NO)
//
// ═══════════════════════════════════════════════════════════════════════════════

const (
	polymarketAPI   = "https://gamma-api.polymarket.com"
	windowScanFreq  = 10 * time.Second
)

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
	StartPrice    decimal.Decimal // Binance price at window start (cached)
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

// WindowScanner manages window discovery and tracking
type WindowScanner struct {
	mu      sync.RWMutex
	running bool
	stopCh  chan struct{}

	// Active windows by market ID
	windows map[string]*Window

	// Binance feed for start prices
	binanceFeed *BinanceFeed

	// Subscribers
	subscribers []chan *Window
}

// NewWindowScanner creates a new scanner
func NewWindowScanner(binanceFeed *BinanceFeed) *WindowScanner {
	return &WindowScanner{
		stopCh:      make(chan struct{}),
		windows:     make(map[string]*Window),
		binanceFeed: binanceFeed,
		subscribers: make([]chan *Window, 0),
	}
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

// scanLoop periodically fetches active windows
func (s *WindowScanner) scanLoop() {
	ticker := time.NewTicker(windowScanFreq)
	defer ticker.Stop()

	// Initial scan
	s.fetchWindows()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.fetchWindows()
			s.cleanupExpired()
		}
	}
}

// fetchWindows gets active 15-minute crypto windows from Polymarket
func (s *WindowScanner) fetchWindows() {
	// Search for active crypto price windows
	// Look for markets with "BTC", "ETH", "SOL" and "15 minutes" or "minute" timeframe
	assets := []string{"BTC", "ETH", "SOL"}

	for _, asset := range assets {
		s.fetchAssetWindows(asset)
	}
}

// fetchAssetWindows fetches windows for a specific asset
func (s *WindowScanner) fetchAssetWindows(asset string) {
	// Query Polymarket for active markets
	url := fmt.Sprintf("%s/markets?active=true&closed=false", polymarketAPI)

	resp, err := http.Get(url)
	if err != nil {
		log.Debug().Err(err).Msg("Failed to fetch markets")
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	var markets []struct {
		ID             string    `json:"id"`
		ConditionID    string    `json:"condition_id"`
		Question       string    `json:"question"`
		EndDate        time.Time `json:"end_date_iso"`
		Tokens         []struct {
			TokenID string `json:"token_id"`
			Outcome string `json:"outcome"`
		} `json:"tokens"`
		OutcomePrices string `json:"outcomePrices"` // JSON string "[0.55, 0.45]"
	}

	if err := json.Unmarshal(body, &markets); err != nil {
		log.Debug().Err(err).Msg("Failed to parse markets")
		return
	}

	// Filter for relevant windows
	for _, m := range markets {
		// Must be a 15-minute window for the asset
		if !strings.Contains(strings.ToUpper(m.Question), asset) {
			continue
		}
		if !strings.Contains(m.Question, "15 minute") && !strings.Contains(m.Question, "minute") {
			continue
		}
		if !strings.Contains(m.Question, "above") && !strings.Contains(m.Question, "Above") {
			continue
		}

		// Parse the question for price to beat
		priceToBeat := extractPriceFromQuestion(m.Question)
		if priceToBeat.IsZero() {
			continue
		}

		// Parse outcome prices
		var prices []float64
		if err := json.Unmarshal([]byte(m.OutcomePrices), &prices); err != nil || len(prices) < 2 {
			continue
		}

		// Find YES/NO token IDs
		var yesTokenID, noTokenID string
		for _, t := range m.Tokens {
			if t.Outcome == "Yes" {
				yesTokenID = t.TokenID
			} else if t.Outcome == "No" {
				noTokenID = t.TokenID
			}
		}

		// Get start price from Binance
		symbol := asset + "USDT"
		startPrice := s.binanceFeed.GetPrice(symbol)

		window := &Window{
			ID:          m.ConditionID,
			Asset:       asset,
			PriceToBeat: priceToBeat,
			EndTime:     m.EndDate,
			YesTokenID:  yesTokenID,
			NoTokenID:   noTokenID,
			YesPrice:    decimal.NewFromFloat(prices[0]),
			NoPrice:     decimal.NewFromFloat(prices[1]),
			Question:    m.Question,
			StartPrice:  startPrice,
			LastUpdated: time.Now(),
		}

		s.updateWindow(window)
	}
}

// updateWindow adds or updates a window
func (s *WindowScanner) updateWindow(window *Window) {
	s.mu.Lock()
	existing, exists := s.windows[window.ID]
	if !exists {
		// New window - cache the start price
		s.windows[window.ID] = window
		log.Info().
			Str("asset", window.Asset).
			Str("target", window.PriceToBeat.StringFixed(0)).
			Dur("remaining", window.TimeRemaining()).
			Msg("🎯 New window detected")
	} else {
		// Update prices only
		existing.YesPrice = window.YesPrice
		existing.NoPrice = window.NoPrice
		existing.LastUpdated = time.Now()
	}
	s.mu.Unlock()

	// Broadcast to subscribers
	s.broadcast(window)
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

// cleanupExpired removes expired windows
func (s *WindowScanner) cleanupExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for id, w := range s.windows {
		if w.IsExpired() {
			delete(s.windows, id)
			log.Debug().Str("id", id).Msg("Window expired, removed")
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
