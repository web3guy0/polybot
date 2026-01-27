package core

import (
	"sync"

	"github.com/web3guy0/polybot/feeds"
	"github.com/web3guy0/polybot/strategy"
)

// ═══════════════════════════════════════════════════════════════════════════════
// ROUTER - Routes market data to strategies based on subscriptions
// ═══════════════════════════════════════════════════════════════════════════════

type Router struct {
	mu            sync.RWMutex
	subscriptions map[string][]strategy.Strategy // market -> strategies
}

// NewRouter creates a new event router
func NewRouter() *Router {
	return &Router{
		subscriptions: make(map[string][]strategy.Strategy),
	}
}

// Subscribe registers a strategy for a market
func (r *Router) Subscribe(market string, strat strategy.Strategy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subscriptions[market] = append(r.subscriptions[market], strat)
}

// Route sends a tick to all subscribed strategies
func (r *Router) Route(tick feeds.Tick) []*strategy.Signal {
	r.mu.RLock()
	strategies := r.subscriptions[tick.Market]
	r.mu.RUnlock()

	var signals []*strategy.Signal

	for _, strat := range strategies {
		if signal := strat.OnTick(tick); signal != nil {
			signals = append(signals, signal)
		}
	}

	return signals
}

// SubscribeAll subscribes a strategy to all markets
func (r *Router) SubscribeAll(strat strategy.Strategy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subscriptions["*"] = append(r.subscriptions["*"], strat)
}
