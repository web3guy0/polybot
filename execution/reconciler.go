package execution

import (
	"encoding/json"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	"github.com/web3guy0/polybot/storage"
)

// ═══════════════════════════════════════════════════════════════════════════════
// RECONCILIATION - Startup position recovery
// ═══════════════════════════════════════════════════════════════════════════════
//
// On startup, we need to:
// 1. Load any persisted positions from database
// 2. Verify they still exist on the exchange
// 3. Close orphaned positions or resume tracking them
//
// This prevents "ghost positions" after crashes
//
// ═══════════════════════════════════════════════════════════════════════════════

// Reconciler handles startup position recovery
type Reconciler struct {
	executor *Executor
	db       *storage.Database
}

// NewReconciler creates a position reconciler
func NewReconciler(executor *Executor, db *storage.Database) *Reconciler {
	return &Reconciler{
		executor: executor,
		db:       db,
	}
}

// RecoverPositions loads and validates persisted positions on startup
func (r *Reconciler) RecoverPositions() (int, error) {
	if r.db == nil || !r.db.IsEnabled() {
		log.Info().Msg("📦 No database - skipping position recovery")
		return 0, nil
	}

	// Load persisted positions
	persisted, err := r.db.GetAllExecutionPositions()
	if err != nil {
		log.Error().Err(err).Msg("❌ Failed to load persisted positions")
		return 0, err
	}

	if len(persisted) == 0 {
		log.Info().Msg("📦 No persisted positions to recover")
		return 0, nil
	}

	log.Warn().
		Int("count", len(persisted)).
		Msg("⚠️ Found persisted positions from previous session")

	recovered := 0
	for _, pos := range persisted {
		// Convert to execution.Position
		metadata := make(map[string]any)
		if pos.Metadata != "" && pos.Metadata != "{}" {
			_ = json.Unmarshal([]byte(pos.Metadata), &metadata)
		}

		execPos := &Position{
			ID:          pos.ID,
			MarketID:    pos.MarketID,
			TokenID:     pos.TokenID,
			Asset:       pos.Asset,
			Side:        pos.Side,
			Size:        pos.Size,
			AvgEntry:    pos.AvgEntry,
			Strategy:    pos.Strategy,
			OpenedAt:    pos.OpenedAt,
			EntryOrders: []string{},
			Metadata:    metadata,
		}

		// Load into executor
		r.executor.LoadPosition(execPos)
		recovered++

		log.Warn().
			Str("id", pos.ID).
			Str("asset", pos.Asset).
			Str("side", pos.Side).
			Str("size", pos.Size.StringFixed(2)).
			Time("opened_at", pos.OpenedAt).
			Msg("📥 Recovered position")
	}

	log.Info().
		Int("recovered", recovered).
		Msg("✅ Position recovery complete")

	return recovered, nil
}

// PersistPosition saves a position to database
func (r *Reconciler) PersistPosition(pos *Position) error {
	if r.db == nil || !r.db.IsEnabled() {
		return nil
	}

	metadata := "{}"
	if pos.Metadata != nil {
		if data, err := json.Marshal(pos.Metadata); err == nil {
			metadata = string(data)
		}
	}

	dbPos := &storage.ExecutionPosition{
		ID:       pos.ID,
		MarketID: pos.MarketID,
		TokenID:  pos.TokenID,
		Asset:    pos.Asset,
		Side:     pos.Side,
		Size:     pos.Size,
		AvgEntry: pos.AvgEntry,
		Strategy: pos.Strategy,
		OpenedAt: pos.OpenedAt,
		Metadata: metadata,
	}

	return r.db.SaveExecutionPosition(dbPos)
}

// RemovePosition deletes a position from database
func (r *Reconciler) RemovePosition(id string) error {
	if r.db == nil || !r.db.IsEnabled() {
		return nil
	}
	return r.db.DeleteExecutionPosition(id)
}

// ═══════════════════════════════════════════════════════════════════════════════
// RISK STATE RECOVERY
// ═══════════════════════════════════════════════════════════════════════════════

// SaveRiskState persists the current risk state
func (r *Reconciler) SaveRiskState(balance, dailyPnL decimal.Decimal, consecLosses int, circuitTripped bool, disabledAssets map[string]bool) error {
	if r.db == nil || !r.db.IsEnabled() {
		return nil
	}

	disabled := "[]"
	if len(disabledAssets) > 0 {
		assets := make([]string, 0, len(disabledAssets))
		for asset, isDisabled := range disabledAssets {
			if isDisabled {
				assets = append(assets, asset)
			}
		}
		if data, err := json.Marshal(assets); err == nil {
			disabled = string(data)
		}
	}

	state := &storage.RiskState{
		Date:              time.Now().Format("2006-01-02"),
		Balance:           balance,
		DailyPnL:          dailyPnL,
		ConsecutiveLosses: consecLosses,
		CircuitTripped:    circuitTripped,
		DisabledAssets:    disabled,
	}

	return r.db.SaveRiskState(state)
}

// LoadRiskState loads the risk state for today
func (r *Reconciler) LoadRiskState() (*storage.RiskState, error) {
	if r.db == nil || !r.db.IsEnabled() {
		return nil, nil
	}

	today := time.Now().Format("2006-01-02")
	return r.db.GetRiskState(today)
}
