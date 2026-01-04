package datafeed

import (
	"sync"
	
	"github.com/web3guy0/polybot/internal/binance"
	"github.com/web3guy0/polybot/internal/strategy"
)

// BinanceDataFeed implements the DataFeed interface using Binance data
type BinanceDataFeed struct {
	client *binance.Client
	mu     sync.RWMutex
}

// NewBinanceDataFeed creates a new Binance data feed
func NewBinanceDataFeed(client *binance.Client) *BinanceDataFeed {
	return &BinanceDataFeed{
		client: client,
	}
}

// GetPrices returns historical prices for an asset
func (f *BinanceDataFeed) GetPrices(asset string, count int) []float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	
	// For now, only support BTC
	if asset != "BTC" {
		return nil
	}
	
	return f.client.GetPrices(count)
}

// GetVolumes returns historical volumes for an asset
func (f *BinanceDataFeed) GetVolumes(asset string, count int) []float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	
	if asset != "BTC" {
		return nil
	}
	
	return f.client.GetVolumes(count)
}

// GetOrderBook returns current order book data for an asset
func (f *BinanceDataFeed) GetOrderBook(asset string) *strategy.OrderBookData {
	f.mu.RLock()
	defer f.mu.RUnlock()
	
	if asset != "BTC" {
		return nil
	}
	
	ob, err := f.client.GetOrderBook("BTCUSDT", 20)
	if err != nil || ob == nil {
		return nil
	}
	
	// Sum bid and ask volumes
	var bidVol, askVol float64
	var bidPrice, askPrice float64
	
	for _, b := range ob.Bids {
		vol, _ := b.Quantity.Float64()
		bidVol += vol
		if bidPrice == 0 {
			bidPrice, _ = b.Price.Float64()
		}
	}
	
	for _, a := range ob.Asks {
		vol, _ := a.Quantity.Float64()
		askVol += vol
		if askPrice == 0 {
			askPrice, _ = a.Price.Float64()
		}
	}
	
	// Calculate spread
	spread := 0.0
	if bidPrice > 0 {
		spread = (askPrice - bidPrice) / bidPrice
	}
	
	// Calculate imbalance
	totalVol := bidVol + askVol
	var imbalance float64
	if totalVol > 0 {
		imbalance = (bidVol - askVol) / totalVol
	}
	
	return &strategy.OrderBookData{
		BidVolume: bidVol,
		AskVolume: askVol,
		BidPrice:  bidPrice,
		AskPrice:  askPrice,
		Spread:    spread,
		Imbalance: imbalance,
	}
}

// GetFundingRate returns current funding rate for an asset
func (f *BinanceDataFeed) GetFundingRate(asset string) float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	
	if asset != "BTC" {
		return 0
	}
	
	rate, err := f.client.GetFundingRate("BTCUSDT")
	if err != nil {
		return 0
	}
	
	fr, _ := rate.Float64()
	return fr
}

// GetCurrentPrice returns the current price for an asset
func (f *BinanceDataFeed) GetCurrentPrice(asset string) float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	
	if asset != "BTC" {
		return 0
	}
	
	price, _ := f.client.GetCurrentPrice().Float64()
	return price
}
