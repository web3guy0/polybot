package execution

import (
	"encoding/json"

	"github.com/rs/zerolog/log"
	"github.com/web3guy0/polybot/storage"
	"github.com/web3guy0/polybot/strategy"
)

// ═══════════════════════════════════════════════════════════════════════════════
// ADAPTER - Makes Reconciler implement strategy.PositionPersister interface
// ═══════════════════════════════════════════════════════════════════════════════

// ReconcilerAdapter wraps Reconciler to implement strategy.PositionPersister
type ReconcilerAdapter struct {
	rec *Reconciler
}

// NewReconcilerAdapter creates an adapter for strategy integration
func NewReconcilerAdapter(rec *Reconciler) *ReconcilerAdapter {
	return &ReconcilerAdapter{rec: rec}
}

// PersistPosition implements strategy.PositionPersister
func (a *ReconcilerAdapter) PersistPosition(pos *strategy.PersistablePosition) error {
	if a.rec == nil || a.rec.db == nil {
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

	if err := a.rec.db.SaveExecutionPosition(dbPos); err != nil {
		log.Error().Err(err).Str("id", pos.ID).Msg("Failed to persist position")
		return err
	}

	return nil
}

// RemovePosition implements strategy.PositionPersister
func (a *ReconcilerAdapter) RemovePosition(id string) error {
	if a.rec == nil || a.rec.db == nil {
		return nil
	}
	return a.rec.db.DeleteExecutionPosition(id)
}
