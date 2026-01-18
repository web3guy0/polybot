package feeds

import (
	"sort"
	"sync"

	"github.com/shopspring/decimal"
)

// ═══════════════════════════════════════════════════════════════════════════════
// ORDERBOOK - In-memory orderbook state
// ═══════════════════════════════════════════════════════════════════════════════

// PriceLevel represents a single price level in the orderbook
type PriceLevel struct {
	Price decimal.Decimal
	Size  decimal.Decimal
}

// Orderbook maintains the current state of an orderbook
type Orderbook struct {
	mu     sync.RWMutex
	Market string
	Asset  string
	Side   string // "YES" or "NO"
	Bids   []PriceLevel
	Asks   []PriceLevel
}

// NewOrderbook creates a new orderbook instance
func NewOrderbook(market, asset string) *Orderbook {
	return &Orderbook{
		Market: market,
		Asset:  asset,
		Bids:   make([]PriceLevel, 0),
		Asks:   make([]PriceLevel, 0),
	}
}

// UpdateFromWS updates orderbook from WebSocket data
func (ob *Orderbook) UpdateFromWS(bids, asks [][]interface{}) {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	// Parse bids
	ob.Bids = make([]PriceLevel, 0, len(bids))
	for _, bid := range bids {
		if len(bid) >= 2 {
			price, _ := parseDecimal(bid[0])
			size, _ := parseDecimal(bid[1])
			if size.GreaterThan(decimal.Zero) {
				ob.Bids = append(ob.Bids, PriceLevel{Price: price, Size: size})
			}
		}
	}

	// Parse asks
	ob.Asks = make([]PriceLevel, 0, len(asks))
	for _, ask := range asks {
		if len(ask) >= 2 {
			price, _ := parseDecimal(ask[0])
			size, _ := parseDecimal(ask[1])
			if size.GreaterThan(decimal.Zero) {
				ob.Asks = append(ob.Asks, PriceLevel{Price: price, Size: size})
			}
		}
	}

	// Sort: bids descending, asks ascending
	sort.Slice(ob.Bids, func(i, j int) bool {
		return ob.Bids[i].Price.GreaterThan(ob.Bids[j].Price)
	})
	sort.Slice(ob.Asks, func(i, j int) bool {
		return ob.Asks[i].Price.LessThan(ob.Asks[j].Price)
	})
}

// BestBid returns the highest bid price
func (ob *Orderbook) BestBid() decimal.Decimal {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.Bids) == 0 {
		return decimal.Zero
	}
	return ob.Bids[0].Price
}

// BestAsk returns the lowest ask price
func (ob *Orderbook) BestAsk() decimal.Decimal {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.Asks) == 0 {
		return decimal.Zero
	}
	return ob.Asks[0].Price
}

// BestBidSize returns size at best bid
func (ob *Orderbook) BestBidSize() decimal.Decimal {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.Bids) == 0 {
		return decimal.Zero
	}
	return ob.Bids[0].Size
}

// BestAskSize returns size at best ask
func (ob *Orderbook) BestAskSize() decimal.Decimal {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	if len(ob.Asks) == 0 {
		return decimal.Zero
	}
	return ob.Asks[0].Size
}

// Spread returns the bid-ask spread
func (ob *Orderbook) Spread() decimal.Decimal {
	return ob.BestAsk().Sub(ob.BestBid())
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

// Imbalance returns orderbook imbalance: positive = more bids, negative = more asks
func (ob *Orderbook) Imbalance() decimal.Decimal {
	ob.mu.RLock()
	defer ob.mu.RUnlock()

	bidVol := decimal.Zero
	askVol := decimal.Zero

	// Sum top 3 levels
	for i := 0; i < 3 && i < len(ob.Bids); i++ {
		bidVol = bidVol.Add(ob.Bids[i].Size)
	}
	for i := 0; i < 3 && i < len(ob.Asks); i++ {
		askVol = askVol.Add(ob.Asks[i].Size)
	}

	total := bidVol.Add(askVol)
	if total.IsZero() {
		return decimal.Zero
	}

	// Returns -1 to +1
	return bidVol.Sub(askVol).Div(total)
}

// Depth returns total volume within a price range
func (ob *Orderbook) Depth(priceLevels int) (bidDepth, askDepth decimal.Decimal) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()

	for i := 0; i < priceLevels && i < len(ob.Bids); i++ {
		bidDepth = bidDepth.Add(ob.Bids[i].Size)
	}
	for i := 0; i < priceLevels && i < len(ob.Asks); i++ {
		askDepth = askDepth.Add(ob.Asks[i].Size)
	}

	return bidDepth, askDepth
}

// parseDecimal converts interface{} to decimal
func parseDecimal(v interface{}) (decimal.Decimal, bool) {
	switch val := v.(type) {
	case string:
		d, err := decimal.NewFromString(val)
		return d, err == nil
	case float64:
		return decimal.NewFromFloat(val), true
	case int:
		return decimal.NewFromInt(int64(val)), true
	case int64:
		return decimal.NewFromInt(val), true
	default:
		return decimal.Zero, false
	}
}
