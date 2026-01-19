package strategy

import (
"os"
"strconv"
"sync"
"time"

"github.com/rs/zerolog/log"
"github.com/shopspring/decimal"

"github.com/web3guy0/polybot/feeds"
)

// ═══════════════════════════════════════════════════════════════════════════════
// SNIPER STRATEGY
// ═══════════════════════════════════════════════════════════════════════════════
//
// MATH (per 24h):
//   - 96 windows available (15-min × 24h, 3 assets with overlap)
//   - Target: 10+ favorable entries @ 85% win rate
//   - Entry: 88-93¢, TP: 99¢ (+6-11¢), SL: 70¢ (-18-23¢)
//   - Expected: 10 trades × $5 × 0.85 × $0.09 = +$3.82/day
//   - Risk: 10 × $5 × 0.15 × -$0.20 = -$1.50/day
//   - Net: +$2.32/day (46% ROI on $5 stake)
//
// EDGE:
//   - At 30 sec remaining with 0.1%+ move = ~90% outcome locked
//   - 100ms scan = fastest detection
//   - Buy confirmed winners before 99¢
//
// ═══════════════════════════════════════════════════════════════════════════════

// Sniper implements the last-minute confirmation strategy
type Sniper struct {
mu      sync.RWMutex
enabled bool

// Config
minTimeSec float64
maxTimeSec float64
minOdds    decimal.Decimal
maxOdds    decimal.Decimal
takeProfit decimal.Decimal
stopLoss   decimal.Decimal

// Per-asset thresholds
btcMinMove decimal.Decimal
ethMinMove decimal.Decimal
solMinMove decimal.Decimal

// Speed
scanIntervalMs int

// Sources (PriceFeed interface - Chainlink or Binance)
priceFeed     feeds.PriceFeed
windowScanner *feeds.WindowScanner

// State
lastSignal   map[string]time.Time
cooldown     time.Duration
priceHistory map[string][]pricePoint

// Stats
signalCount int
}

type pricePoint struct {
price     decimal.Decimal
timestamp time.Time
}

// NewSniper creates the sniper strategy
func NewSniper(priceFeed feeds.PriceFeed, windowScanner *feeds.WindowScanner) *Sniper {
s := &Sniper{
enabled:        true,
minTimeSec:     envFloat("MIN_TIME_SEC", 15),
maxTimeSec:     envFloat("MAX_TIME_SEC", 60),
minOdds:        envDecimal("MIN_ODDS", 0.88),
maxOdds:        envDecimal("MAX_ODDS", 0.93),
takeProfit:     envDecimal("TAKE_PROFIT", 0.99),
stopLoss:       envDecimal("STOP_LOSS", 0.70),
btcMinMove:     envDecimal("BTC_MIN_MOVE", 0.10),
ethMinMove:     envDecimal("ETH_MIN_MOVE", 0.10),
solMinMove:     envDecimal("SOL_MIN_MOVE", 0.15),
scanIntervalMs: envInt("SCAN_INTERVAL_MS", 100),
priceFeed:      priceFeed,
windowScanner:  windowScanner,
lastSignal:     make(map[string]time.Time),
cooldown:       10 * time.Second,
priceHistory:   make(map[string][]pricePoint),
}

log.Info().
Float64("time_window", s.minTimeSec).
Str("entry", s.minOdds.StringFixed(2)+"-"+s.maxOdds.StringFixed(2)).
Int("scan_ms", s.scanIntervalMs).
Msg("🎯 Sniper ready")

return s
}

func (s *Sniper) Name() string    { return "Sniper" }
func (s *Sniper) Enabled() bool   { s.mu.RLock(); defer s.mu.RUnlock(); return s.enabled }
func (s *Sniper) OnTick(_ feeds.Tick) *Signal { return nil }

func (s *Sniper) Config() map[string]interface{} {
return map[string]interface{}{
"time_window": s.minTimeSec,
"entry_zone":  s.minOdds.String() + "-" + s.maxOdds.String(),
"scan_ms":     s.scanIntervalMs,
}
}

// RunLoop is the fast scan loop - 100ms for rocket speed
func (s *Sniper) RunLoop(signalCh chan<- *Signal) {
interval := time.Duration(s.scanIntervalMs) * time.Millisecond
ticker := time.NewTicker(interval)
defer ticker.Stop()

log.Info().Int("ms", s.scanIntervalMs).Msg("⚡ Scan loop active")

for range ticker.C {
if sig := s.scan(); sig != nil {
signalCh <- sig
}
}
}

func (s *Sniper) scan() *Signal {
s.mu.Lock()
defer s.mu.Unlock()

if !s.enabled {
return nil
}

windows := s.windowScanner.GetSniperReadyWindows(s.minTimeSec, s.maxTimeSec)
for _, w := range windows {
if sig := s.evaluate(w); sig != nil {
return sig
}
}
return nil
}

func (s *Sniper) evaluate(w *feeds.Window) *Signal {
// Cooldown check
if last, ok := s.lastSignal[w.ID]; ok && time.Since(last) < s.cooldown {
return nil
}

// Get Chainlink-aligned price (current)
price := s.priceFeed.GetPrice(w.Asset)
if price.IsZero() {
return nil
}

// Price to beat is captured at window start from Chainlink
// This is what we compare against to determine Up/Down
if w.PriceToBeat.IsZero() {
return nil
}

// Track for momentum
s.trackPrice(w.Asset, price)

// Calculate move % from price to beat
move := price.Sub(w.PriceToBeat).Div(w.PriceToBeat).Mul(decimal.NewFromInt(100))
absMove := move.Abs()
minMove := s.getMinMove(w.Asset)

if absMove.LessThan(minMove) {
return nil
}

// Determine side
isAbove := move.GreaterThan(decimal.Zero)
var tokenID, side string
var odds decimal.Decimal

if isAbove {
tokenID, side, odds = w.YesTokenID, "YES", w.YesPrice
} else {
tokenID, side, odds = w.NoTokenID, "NO", w.NoPrice
}

// Check entry zone
if odds.LessThan(s.minOdds) || odds.GreaterThan(s.maxOdds) {
return nil
}

// Momentum confirmation
if !s.checkMomentum(w.Asset, isAbove) {
return nil
}

// SIGNAL!
s.signalCount++
s.lastSignal[w.ID] = time.Now()
timeLeft := w.TimeRemainingSeconds()

log.Info().
Str("asset", w.Asset).
Str("side", side).
Str("odds", odds.StringFixed(2)).
Str("move", move.StringFixed(2)+"%").
Float64("sec_left", timeLeft).
Msg("🎯 SIGNAL")

return NewSignal().
Market(w.ID).
Asset(w.Asset).
TokenID(tokenID).
Side(side).
Entry(odds).
TakeProfit(s.takeProfit).
StopLoss(s.stopLoss).
Confidence(s.calcConfidence(absMove, timeLeft)).
Reason(w.Asset + " " + move.StringFixed(2) + "% " + side).
Strategy(s.Name()).
Build()
}

func (s *Sniper) getMinMove(asset string) decimal.Decimal {
switch asset {
case "BTC":
return s.btcMinMove
case "ETH":
return s.ethMinMove
case "SOL":
return s.solMinMove
default:
return s.btcMinMove
}
}

func (s *Sniper) trackPrice(symbol string, price decimal.Decimal) {
s.priceHistory[symbol] = append(s.priceHistory[symbol], pricePoint{price, time.Now()})

// Keep last 30 seconds only
cutoff := time.Now().Add(-30 * time.Second)
var filtered []pricePoint
for _, p := range s.priceHistory[symbol] {
if p.timestamp.After(cutoff) {
filtered = append(filtered, p)
}
}
s.priceHistory[symbol] = filtered
}

func (s *Sniper) checkMomentum(symbol string, expectUp bool) bool {
history := s.priceHistory[symbol]
if len(history) < 3 {
return true // Not enough data, allow
}

// Check last 5 seconds
cutoff := time.Now().Add(-5 * time.Second)
var recent []decimal.Decimal
for _, p := range history {
if p.timestamp.After(cutoff) {
recent = append(recent, p.price)
}
}

if len(recent) < 2 {
return true
}

velocity := recent[len(recent)-1].Sub(recent[0])

if expectUp {
return velocity.GreaterThanOrEqual(decimal.Zero) // Not falling
}
return velocity.LessThanOrEqual(decimal.Zero) // Not rising
}

func (s *Sniper) calcConfidence(absMove decimal.Decimal, secLeft float64) decimal.Decimal {
// Base: bigger move = higher confidence
conf := 0.70 + absMove.InexactFloat64()*0.5
// Bonus: less time = higher confidence
conf += (60 - secLeft) / 60 * 0.10
if conf > 0.95 {
conf = 0.95
}
return decimal.NewFromFloat(conf)
}

// ═══════════════════════════════════════════════════════════════════════════════
// HELPERS
// ═══════════════════════════════════════════════════════════════════════════════

func envFloat(key string, fallback float64) float64 {
if v := os.Getenv(key); v != "" {
if f, err := strconv.ParseFloat(v, 64); err == nil {
return f
}
}
return fallback
}

func envDecimal(key string, fallback float64) decimal.Decimal {
if v := os.Getenv(key); v != "" {
if d, err := decimal.NewFromString(v); err == nil {
return d
}
}
return decimal.NewFromFloat(fallback)
}

func envInt(key string, fallback int) int {
if v := os.Getenv(key); v != "" {
if i, err := strconv.Atoi(v); err == nil {
return i
}
}
return fallback
}
