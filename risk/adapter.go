package risk

import (
	"github.com/shopspring/decimal"
	"github.com/web3guy0/polybot/strategy"
)

// ═══════════════════════════════════════════════════════════════════════════════
// ADAPTER - Makes RiskGate implement strategy.TradeApprover interface
// ═══════════════════════════════════════════════════════════════════════════════
//
// This breaks the circular dependency:
//   strategy → risk (would cause cycle)
// Instead:
//   strategy defines interface
//   risk implements interface via adapter
//
// ═══════════════════════════════════════════════════════════════════════════════

// RiskGateAdapter wraps RiskGate to implement strategy.TradeApprover
type RiskGateAdapter struct {
	gate *RiskGate
}

// NewRiskGateAdapter creates an adapter for strategy integration
func NewRiskGateAdapter(gate *RiskGate) *RiskGateAdapter {
	return &RiskGateAdapter{gate: gate}
}

// CanEnter implements strategy.TradeApprover
func (a *RiskGateAdapter) CanEnter(req strategy.TradeApprovalRequest) strategy.TradeApprovalResponse {
	// Convert to internal request type
	internalReq := TradeRequest{
		Asset:    req.Asset,
		Side:     req.Side,
		Action:   req.Action,
		Price:    req.Price,
		Size:     req.Size,
		Strategy: req.Strategy,
		Phase:    req.Phase,
		Reason:   req.Reason,
		Metadata: req.Metadata,
	}

	// Call internal CanEnter
	approval := a.gate.CanEnter(internalReq)

	// Convert to strategy response type
	return strategy.TradeApprovalResponse{
		Approved:     approval.Approved,
		AdjustedSize: approval.AdjustedSize,
		RejectionMsg: approval.RejectionMsg,
		RiskScore:    approval.RiskScore,
	}
}

// RecordExit implements strategy.TradeApprover
func (a *RiskGateAdapter) RecordExit(asset string, pnl decimal.Decimal) {
	a.gate.RecordExit(asset, pnl)
}

// IsDailyLimitHit implements strategy.TradeApprover
func (a *RiskGateAdapter) IsDailyLimitHit() bool {
	return a.gate.IsDailyLimitHit()
}

// IsCircuitTripped implements strategy.TradeApprover
func (a *RiskGateAdapter) IsCircuitTripped() bool {
	return a.gate.IsCircuitTripped()
}

// IsAssetDisabled implements strategy.TradeApprover
func (a *RiskGateAdapter) IsAssetDisabled(asset string) bool {
	return a.gate.IsAssetDisabled(asset)
}
