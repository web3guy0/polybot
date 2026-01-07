// Package arbitrage provides latency arbitrage functionality
//
// odds.go - Real-time odds fetching from Polymarket CLOB
// Fetches live bid/ask prices from the order book to ensure we're
// getting the actual tradeable prices, not stale API data.
package arbitrage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// CLOBBookEntry represents a single bid/ask entry
type CLOBBookEntry struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

// CLOBBook represents the order book from Polymarket CLOB
type CLOBBook struct {
	Market   string          `json:"market"`
	AssetID  string          `json:"asset_id"`
	Bids     []CLOBBookEntry `json:"bids"`
	Asks     []CLOBBookEntry `json:"asks"`
	Hash     string          `json:"hash"`
}

// LiveOdds represents current tradeable odds for a market
type LiveOdds struct {
	TokenID      string
	BestBid      decimal.Decimal // Best price to sell at
	BestAsk      decimal.Decimal // Best price to buy at
	BidSize      decimal.Decimal
	AskSize      decimal.Decimal
	Spread       decimal.Decimal
	SpreadPct    decimal.Decimal
	LastUpdated  time.Time
}

// OddsFetcher fetches real-time odds from Polymarket CLOB
type OddsFetcher struct {
	baseURL    string
	httpClient *http.Client
}

// NewOddsFetcher creates a new odds fetcher
func NewOddsFetcher() *OddsFetcher {
	return &OddsFetcher{
		baseURL: "https://clob.polymarket.com",
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

// GetLiveOdds fetches the current order book for a token
func (f *OddsFetcher) GetLiveOdds(tokenID string) (*LiveOdds, error) {
	url := fmt.Sprintf("%s/book?token_id=%s", f.baseURL, tokenID)
	
	resp, err := f.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch book: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CLOB API returned status %d", resp.StatusCode)
	}

	var book CLOBBook
	if err := json.NewDecoder(resp.Body).Decode(&book); err != nil {
		return nil, fmt.Errorf("failed to decode book: %w", err)
	}

	odds := &LiveOdds{
		TokenID:     tokenID,
		LastUpdated: time.Now(),
	}

	// Parse best bid (highest buy order)
	if len(book.Bids) > 0 {
		odds.BestBid, _ = decimal.NewFromString(book.Bids[0].Price)
		odds.BidSize, _ = decimal.NewFromString(book.Bids[0].Size)
	}

	// Parse best ask (lowest sell order)
	if len(book.Asks) > 0 {
		odds.BestAsk, _ = decimal.NewFromString(book.Asks[0].Price)
		odds.AskSize, _ = decimal.NewFromString(book.Asks[0].Size)
	}

	// Calculate spread
	if !odds.BestBid.IsZero() && !odds.BestAsk.IsZero() {
		odds.Spread = odds.BestAsk.Sub(odds.BestBid)
		mid := odds.BestBid.Add(odds.BestAsk).Div(decimal.NewFromInt(2))
		if !mid.IsZero() {
			odds.SpreadPct = odds.Spread.Div(mid).Mul(decimal.NewFromInt(100))
		}
	}

	return odds, nil
}

// GetBothSideOdds fetches odds for both Up and Down tokens
func (f *OddsFetcher) GetBothSideOdds(upTokenID, downTokenID string) (*LiveOdds, *LiveOdds, error) {
	upOdds, err := f.GetLiveOdds(upTokenID)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch UP odds")
	}

	downOdds, err := f.GetLiveOdds(downTokenID)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to fetch DOWN odds")
	}

	return upOdds, downOdds, nil
}

// GetMidPrice returns the mid-market price for a token
func (f *OddsFetcher) GetMidPrice(tokenID string) (decimal.Decimal, error) {
	odds, err := f.GetLiveOdds(tokenID)
	if err != nil {
		return decimal.Zero, err
	}

	if odds.BestBid.IsZero() || odds.BestAsk.IsZero() {
		return decimal.Zero, fmt.Errorf("no liquidity")
	}

	return odds.BestBid.Add(odds.BestAsk).Div(decimal.NewFromInt(2)), nil
}

// CheckArbitrageCondition checks if odds are favorable for arbitrage
// Returns true if the market odds are stale (still near 50/50 despite a price move)
func (f *OddsFetcher) CheckArbitrageCondition(tokenID string, expectedDirection string, threshold decimal.Decimal) (bool, *LiveOdds, error) {
	odds, err := f.GetLiveOdds(tokenID)
	if err != nil {
		return false, nil, err
	}

	// For buying, we care about the ask price (what we pay)
	// If ask is below threshold, odds are still cheap = opportunity
	if odds.BestAsk.LessThan(threshold) {
		return true, odds, nil
	}

	return false, odds, nil
}
