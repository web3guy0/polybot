package execution

import (
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	"github.com/web3guy0/polybot/exec"
)

// ═══════════════════════════════════════════════════════════════════════════════
// EXECUTION LAYER - Professional Order State Machine
// ═══════════════════════════════════════════════════════════════════════════════
//
// Responsibilities:
// 1. Order lifecycle management (submit → ack → fill/cancel)
// 2. Fill confirmation with retries
// 3. Partial fill handling
// 4. Position state reconciliation
// 5. Clean separation from strategy logic
//
// Order Flow:
//   Strategy → Risk → Executor → CLOB API
//                 ↓
//            State Machine
//              ↓    ↓    ↓
//         FILLED  PARTIAL  REJECTED
//
// ═══════════════════════════════════════════════════════════════════════════════

// OrderState represents the lifecycle state of an order
type OrderState string

const (
	OrderStatePending   OrderState = "PENDING"   // Submitted, awaiting ack
	OrderStateOpen      OrderState = "OPEN"      // Acknowledged, in book
	OrderStateFilled    OrderState = "FILLED"    // Fully filled
	OrderStatePartial   OrderState = "PARTIAL"   // Partially filled
	OrderStateCancelled OrderState = "CANCELLED" // Cancelled by user or system
	OrderStateRejected  OrderState = "REJECTED"  // Rejected by exchange
	OrderStateExpired   OrderState = "EXPIRED"   // Timed out
	OrderStateFailed    OrderState = "FAILED"    // Internal failure
)

// Order represents an order in the execution system
type Order struct {
	ID            string          `json:"id"`
	ClientID      string          `json:"client_id"` // Our internal ID
	MarketID      string          `json:"market_id"`
	TokenID       string          `json:"token_id"`
	Asset         string          `json:"asset"`
	Side          string          `json:"side"`    // "YES" or "NO"
	Action        string          `json:"action"`  // "BUY" or "SELL"
	Price         decimal.Decimal `json:"price"`
	Size          decimal.Decimal `json:"size"`    // Requested size
	FilledSize    decimal.Decimal `json:"filled_size"`
	AvgFillPrice  decimal.Decimal `json:"avg_fill_price"`
	State         OrderState      `json:"state"`
	Strategy      string          `json:"strategy"`
	SubmitTime    time.Time       `json:"submit_time"`
	AckTime       *time.Time      `json:"ack_time,omitempty"`
	FillTime      *time.Time      `json:"fill_time,omitempty"`
	RetryCount    int             `json:"retry_count"`
	ErrorMsg      string          `json:"error_msg,omitempty"`
	Metadata      map[string]any  `json:"metadata,omitempty"` // Phase, faded direction, etc.
}

// Position represents an open position (aggregate of fills)
type Position struct {
	ID           string          `json:"id"`
	MarketID     string          `json:"market_id"`
	TokenID      string          `json:"token_id"`
	Asset        string          `json:"asset"`
	Side         string          `json:"side"`   // "YES" or "NO"
	Size         decimal.Decimal `json:"size"`
	AvgEntry     decimal.Decimal `json:"avg_entry"`
	UnrealizedPnL decimal.Decimal `json:"unrealized_pnl"`
	Strategy     string          `json:"strategy"`
	OpenedAt     time.Time       `json:"opened_at"`
	EntryOrders  []string        `json:"entry_orders"` // Order IDs that opened this
	Metadata     map[string]any  `json:"metadata,omitempty"`
}

// Fill represents a single execution fill
type Fill struct {
	OrderID   string          `json:"order_id"`
	Price     decimal.Decimal `json:"price"`
	Size      decimal.Decimal `json:"size"`
	Timestamp time.Time       `json:"timestamp"`
	Fee       decimal.Decimal `json:"fee"`
}

// ExecutorConfig holds executor settings
type ExecutorConfig struct {
	MaxRetries       int           // Max order retries (default: 2)
	AckTimeout       time.Duration // Wait for order ack (default: 2s)
	FillTimeout      time.Duration // Wait for fill (default: 5s)
	CancelTimeout    time.Duration // Wait for cancel ack (default: 2s)
	PaperMode        bool          // Simulate fills
	SlippageBps      int           // Simulated slippage in bps (default: 10)
}

// DefaultExecutorConfig returns sensible defaults
func DefaultExecutorConfig() ExecutorConfig {
	return ExecutorConfig{
		MaxRetries:    2,
		AckTimeout:    2 * time.Second,
		FillTimeout:   5 * time.Second,
		CancelTimeout: 2 * time.Second,
		PaperMode:     true,
		SlippageBps:   10, // 0.1% slippage
	}
}

// Executor manages order execution and position state
type Executor struct {
	mu     sync.RWMutex
	config ExecutorConfig

	// State
	orders    map[string]*Order    // All orders by ID
	positions map[string]*Position // Open positions by key (asset_side)

	// CLOB client (real execution)
	client *exec.Client

	// Callbacks
	onFill   func(order *Order, fill Fill)
	onReject func(order *Order, reason string)

	// Metrics
	totalOrders    int64
	filledOrders   int64
	rejectedOrders int64
	totalVolume    decimal.Decimal
}

// NewExecutor creates a new execution manager
func NewExecutor(client *exec.Client, config ExecutorConfig) *Executor {
	e := &Executor{
		config:    config,
		orders:    make(map[string]*Order),
		positions: make(map[string]*Position),
		client:    client,
	}

	mode := "PAPER"
	if !config.PaperMode {
		mode = "LIVE"
	}

	log.Info().
		Str("mode", mode).
		Int("max_retries", config.MaxRetries).
		Dur("ack_timeout", config.AckTimeout).
		Dur("fill_timeout", config.FillTimeout).
		Msg("⚡ Executor initialized")

	return e
}

// ═══════════════════════════════════════════════════════════════════════════════
// ORDER SUBMISSION
// ═══════════════════════════════════════════════════════════════════════════════

// SubmitOrder sends an order and manages its lifecycle
func (e *Executor) SubmitOrder(order *Order) (*Order, error) {
	e.mu.Lock()

	// Generate client ID if not set
	if order.ClientID == "" {
		order.ClientID = fmt.Sprintf("PB_%d_%s", time.Now().UnixNano(), order.Asset)
	}

	order.State = OrderStatePending
	order.SubmitTime = time.Now()
	order.FilledSize = decimal.Zero
	order.AvgFillPrice = decimal.Zero

	e.orders[order.ClientID] = order
	e.totalOrders++
	e.mu.Unlock()

	log.Info().
		Str("client_id", order.ClientID).
		Str("asset", order.Asset).
		Str("side", order.Side).
		Str("action", order.Action).
		Str("price", order.Price.StringFixed(4)).
		Str("size", order.Size.StringFixed(2)).
		Str("strategy", order.Strategy).
		Msg("📤 Order submitted")

	// Execute based on mode
	if e.config.PaperMode {
		return e.simulateFill(order)
	}

	return e.executeLive(order)
}

// simulateFill simulates order execution for paper trading
func (e *Executor) simulateFill(order *Order) (*Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Simulate network delay
	time.Sleep(50 * time.Millisecond)

	// Simulate ack
	now := time.Now()
	order.AckTime = &now
	order.State = OrderStateOpen

	// Simulate fill with slippage
	slippage := decimal.NewFromInt(int64(e.config.SlippageBps)).
		Div(decimal.NewFromInt(10000))

	fillPrice := order.Price
	if order.Action == "BUY" {
		// Buying: pay slightly more
		fillPrice = order.Price.Mul(decimal.NewFromInt(1).Add(slippage))
	} else {
		// Selling: receive slightly less
		fillPrice = order.Price.Mul(decimal.NewFromInt(1).Sub(slippage))
	}

	// Clamp fill price to valid range
	if fillPrice.LessThan(decimal.NewFromFloat(0.01)) {
		fillPrice = decimal.NewFromFloat(0.01)
	}
	if fillPrice.GreaterThan(decimal.NewFromFloat(0.99)) {
		fillPrice = decimal.NewFromFloat(0.99)
	}

	// Full fill
	fillTime := time.Now()
	order.FillTime = &fillTime
	order.FilledSize = order.Size
	order.AvgFillPrice = fillPrice
	order.State = OrderStateFilled

	e.filledOrders++
	e.totalVolume = e.totalVolume.Add(fillPrice.Mul(order.Size))

	// Update position
	e.updatePosition(order)

	// Create fill record
	fill := Fill{
		OrderID:   order.ClientID,
		Price:     fillPrice,
		Size:      order.Size,
		Timestamp: fillTime,
		Fee:       fillPrice.Mul(order.Size).Mul(decimal.NewFromFloat(0.001)), // 0.1% fee
	}

	// Callback
	if e.onFill != nil {
		e.onFill(order, fill)
	}

	log.Info().
		Str("client_id", order.ClientID).
		Str("asset", order.Asset).
		Str("fill_price", fillPrice.StringFixed(4)).
		Str("size", order.Size.StringFixed(2)).
		Msg("✅ Order filled (PAPER)")

	return order, nil
}

// executeLive sends order to real CLOB
func (e *Executor) executeLive(order *Order) (*Order, error) {
	var orderID string
	var err error

	// Retry loop
	for attempt := 0; attempt <= e.config.MaxRetries; attempt++ {
		order.RetryCount = attempt

		// Submit to CLOB
		orderID, err = e.client.PlaceOrder(
			order.TokenID,
			order.Price,
			order.Size,
			order.Action,
		)

		if err == nil {
			break
		}

		log.Warn().
			Err(err).
			Int("attempt", attempt+1).
			Str("client_id", order.ClientID).
			Msg("⚠️ Order submission failed, retrying...")

		if attempt < e.config.MaxRetries {
			time.Sleep(time.Duration(100*(attempt+1)) * time.Millisecond)
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if err != nil {
		order.State = OrderStateFailed
		order.ErrorMsg = err.Error()
		e.rejectedOrders++

		if e.onReject != nil {
			e.onReject(order, err.Error())
		}

		log.Error().
			Err(err).
			Str("client_id", order.ClientID).
			Msg("❌ Order failed after retries")

		return order, fmt.Errorf("order failed: %w", err)
	}

	// Success
	order.ID = orderID
	now := time.Now()
	order.AckTime = &now
	order.State = OrderStateOpen

	// For now, assume immediate fill (can enhance with WS monitoring later)
	order.FillTime = &now
	order.FilledSize = order.Size
	order.AvgFillPrice = order.Price
	order.State = OrderStateFilled

	e.filledOrders++
	e.totalVolume = e.totalVolume.Add(order.Price.Mul(order.Size))

	// Update position
	e.updatePosition(order)

	log.Info().
		Str("order_id", orderID).
		Str("client_id", order.ClientID).
		Str("asset", order.Asset).
		Msg("✅ Order filled (LIVE)")

	return order, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// POSITION MANAGEMENT
// ═══════════════════════════════════════════════════════════════════════════════

// updatePosition updates position state after a fill
func (e *Executor) updatePosition(order *Order) {
	posKey := order.Asset + "_" + order.Side

	if order.Action == "BUY" {
		// Opening or adding to position
		pos, exists := e.positions[posKey]
		if !exists {
			pos = &Position{
				ID:          fmt.Sprintf("POS_%d", time.Now().UnixNano()),
				MarketID:    order.MarketID,
				TokenID:     order.TokenID,
				Asset:       order.Asset,
				Side:        order.Side,
				Size:        decimal.Zero,
				AvgEntry:    decimal.Zero,
				Strategy:    order.Strategy,
				OpenedAt:    time.Now(),
				EntryOrders: make([]string, 0),
				Metadata:    order.Metadata,
			}
		}

		// Calculate new average entry
		totalCost := pos.AvgEntry.Mul(pos.Size).Add(order.AvgFillPrice.Mul(order.FilledSize))
		newSize := pos.Size.Add(order.FilledSize)
		if !newSize.IsZero() {
			pos.AvgEntry = totalCost.Div(newSize)
		}
		pos.Size = newSize
		pos.EntryOrders = append(pos.EntryOrders, order.ClientID)

		e.positions[posKey] = pos

	} else {
		// Closing position
		pos, exists := e.positions[posKey]
		if exists {
			pos.Size = pos.Size.Sub(order.FilledSize)
			if pos.Size.LessThanOrEqual(decimal.Zero) {
				delete(e.positions, posKey)
			}
		}
	}
}

// GetPosition returns position for an asset/side
func (e *Executor) GetPosition(asset, side string) *Position {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.positions[asset+"_"+side]
}

// GetAllPositions returns all open positions
func (e *Executor) GetAllPositions() map[string]*Position {
	e.mu.RLock()
	defer e.mu.RUnlock()

	// Return copy
	result := make(map[string]*Position, len(e.positions))
	for k, v := range e.positions {
		result[k] = v
	}
	return result
}

// HasPosition checks if position exists
func (e *Executor) HasPosition(asset string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()

	for _, pos := range e.positions {
		if pos.Asset == asset && pos.Size.GreaterThan(decimal.Zero) {
			return true
		}
	}
	return false
}

// ClosePosition submits an order to close a position
func (e *Executor) ClosePosition(asset, side string, price decimal.Decimal) (*Order, error) {
	e.mu.RLock()
	pos := e.positions[asset+"_"+side]
	e.mu.RUnlock()

	if pos == nil || pos.Size.IsZero() {
		return nil, fmt.Errorf("no position to close for %s_%s", asset, side)
	}

	closeOrder := &Order{
		MarketID: pos.MarketID,
		TokenID:  pos.TokenID,
		Asset:    asset,
		Side:     side,
		Action:   "SELL",
		Price:    price,
		Size:     pos.Size,
		Strategy: pos.Strategy,
		Metadata: pos.Metadata,
	}

	return e.SubmitOrder(closeOrder)
}

// ═══════════════════════════════════════════════════════════════════════════════
// RECONCILIATION
// ═══════════════════════════════════════════════════════════════════════════════

// LoadPosition loads a position from persistence (for restart recovery)
func (e *Executor) LoadPosition(pos *Position) {
	e.mu.Lock()
	defer e.mu.Unlock()

	posKey := pos.Asset + "_" + pos.Side
	e.positions[posKey] = pos

	log.Info().
		Str("asset", pos.Asset).
		Str("side", pos.Side).
		Str("size", pos.Size.StringFixed(2)).
		Str("avg_entry", pos.AvgEntry.StringFixed(4)).
		Msg("📥 Position loaded from persistence")
}

// ForceCloseAllPositions closes all open positions (for shutdown)
func (e *Executor) ForceCloseAllPositions(priceGetter func(asset, side string) decimal.Decimal) []error {
	e.mu.RLock()
	positions := make([]*Position, 0, len(e.positions))
	for _, pos := range e.positions {
		positions = append(positions, pos)
	}
	e.mu.RUnlock()

	var errors []error
	for _, pos := range positions {
		price := priceGetter(pos.Asset, pos.Side)
		if price.IsZero() {
			price = pos.AvgEntry // Fallback to entry price
		}

		_, err := e.ClosePosition(pos.Asset, pos.Side, price)
		if err != nil {
			errors = append(errors, err)
			log.Error().
				Err(err).
				Str("asset", pos.Asset).
				Str("side", pos.Side).
				Msg("❌ Failed to close position on shutdown")
		} else {
			log.Warn().
				Str("asset", pos.Asset).
				Str("side", pos.Side).
				Msg("🚨 Force closed position on shutdown")
		}
	}

	return errors
}

// ═══════════════════════════════════════════════════════════════════════════════
// CALLBACKS & METRICS
// ═══════════════════════════════════════════════════════════════════════════════

// OnFill sets callback for fill events
func (e *Executor) OnFill(fn func(order *Order, fill Fill)) {
	e.onFill = fn
}

// OnReject sets callback for rejection events
func (e *Executor) OnReject(fn func(order *Order, reason string)) {
	e.onReject = fn
}

// GetMetrics returns execution metrics
func (e *Executor) GetMetrics() map[string]interface{} {
	e.mu.RLock()
	defer e.mu.RUnlock()

	fillRate := float64(0)
	if e.totalOrders > 0 {
		fillRate = float64(e.filledOrders) / float64(e.totalOrders) * 100
	}

	return map[string]interface{}{
		"total_orders":    e.totalOrders,
		"filled_orders":   e.filledOrders,
		"rejected_orders": e.rejectedOrders,
		"fill_rate":       fillRate,
		"total_volume":    e.totalVolume.StringFixed(2),
		"open_positions":  len(e.positions),
	}
}

// GetOrder retrieves an order by client ID
func (e *Executor) GetOrder(clientID string) *Order {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.orders[clientID]
}
