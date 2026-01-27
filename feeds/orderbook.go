package feeds

import (
	"fmt"
	"sync"

	"github.com/shopspring/decimal"
)

// ═══════════════════════════════════════════════════════════════════════════════
// ORDERBOOK - Simple orderbook for price tracking
// ═══════════════════════════════════════════════════════════════════════════════

// Orderbook tracks bids and asks for a market
type Orderbook struct {
	mu      sync.RWMutex
	market  string
	asset   string
	Side    string // "YES" or "NO"
	bids    []Level
	asks    []Level
}

// Level represents a price level
type Level struct {
	Price decimal.Decimal
	Size  decimal.Decimal
}

// NewOrderbook creates a new orderbook
func NewOrderbook(market, asset string) *Orderbook {
	return &Orderbook{
		market: market,
		asset:  asset,
		bids:   make([]Level, 0),
		asks:   make([]Level, 0),
	}
}

// UpdateFromWS updates the orderbook from WebSocket data
func (ob *Orderbook) UpdateFromWS(bids, asks [][]interface{}) {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	ob.bids = parseLevelsInterface(bids)
	ob.asks = parseLevelsInterface(asks)
}

// BestBid returns the highest bid price
func (ob *Orderbook) BestBid() decimal.Decimal {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.bids) == 0 {
		return decimal.Zero
	}
	return ob.bids[0].Price
}

// BestAsk returns the lowest ask price
func (ob *Orderbook) BestAsk() decimal.Decimal {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.asks) == 0 {
		return decimal.Zero
	}
	return ob.asks[0].Price
}

// BestBidSize returns the size at best bid
func (ob *Orderbook) BestBidSize() decimal.Decimal {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.bids) == 0 {
		return decimal.Zero
	}
	return ob.bids[0].Size
}

// BestAskSize returns the size at best ask
func (ob *Orderbook) BestAskSize() decimal.Decimal {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.asks) == 0 {
		return decimal.Zero
	}
	return ob.asks[0].Size
}

// Mid returns the mid price
func (ob *Orderbook) Mid() decimal.Decimal {
	bid := ob.BestBid()
	ask := ob.BestAsk()
	if bid.IsZero() || ask.IsZero() {
		return decimal.Zero
	}
	return bid.Add(ask).Div(decimal.NewFromInt(2))
}

// Spread returns the bid-ask spread
func (ob *Orderbook) Spread() decimal.Decimal {
	return ob.BestAsk().Sub(ob.BestBid())
}

// parseLevelsInterface converts WS data to Level slice
func parseLevelsInterface(data [][]interface{}) []Level {
	levels := make([]Level, 0, len(data))
	for _, row := range data {
		if len(row) < 2 {
			continue
		}
		price, _ := decimal.NewFromString(fmt.Sprintf("%v", row[0]))
		size, _ := decimal.NewFromString(fmt.Sprintf("%v", row[1]))
		levels = append(levels, Level{Price: price, Size: size})
	}
	return levels
}
