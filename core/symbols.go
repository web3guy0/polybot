package core

import (
	"sync"

	"github.com/shopspring/decimal"
)

// ═══════════════════════════════════════════════════════════════════════════════
// SYMBOLS - Market metadata management
// ═══════════════════════════════════════════════════════════════════════════════

// Market represents a prediction market
type Market struct {
	ID            string
	Question      string
	YesTokenID    string
	NoTokenID     string
	Active        bool
	EndDate       int64
	MinTickSize   decimal.Decimal
	MinOrderSize  decimal.Decimal
	RewardsByEnd  decimal.Decimal
	Volume24h     decimal.Decimal
}

// SymbolManager manages market metadata
type SymbolManager struct {
	mu      sync.RWMutex
	markets map[string]*Market
}

// NewSymbolManager creates a new symbol manager
func NewSymbolManager() *SymbolManager {
	return &SymbolManager{
		markets: make(map[string]*Market),
	}
}

// Add adds or updates a market
func (sm *SymbolManager) Add(m *Market) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.markets[m.ID] = m
}

// Get retrieves a market by ID
func (sm *SymbolManager) Get(id string) *Market {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.markets[id]
}

// GetByTokenID finds a market by YES or NO token ID
func (sm *SymbolManager) GetByTokenID(tokenID string) *Market {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	for _, m := range sm.markets {
		if m.YesTokenID == tokenID || m.NoTokenID == tokenID {
			return m
		}
	}
	return nil
}

// ActiveMarkets returns all active markets
func (sm *SymbolManager) ActiveMarkets() []*Market {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var active []*Market
	for _, m := range sm.markets {
		if m.Active {
			active = append(active, m)
		}
	}
	return active
}

// Count returns total number of markets
func (sm *SymbolManager) Count() int {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return len(sm.markets)
}
