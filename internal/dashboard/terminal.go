package dashboard

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// ═══════════════════════════════════════════════════════════════════════════
// PROFESSIONAL TERMINAL DASHBOARD - Institutional Grade
// ═══════════════════════════════════════════════════════════════════════════
// 
// Features:
// - Stable borders that never flicker
// - Only updates changed content
// - Color-coded status indicators
// - Real-time P&L tracking
// - Clean professional layout

const (
	// ANSI escape codes
	ClearScreen    = "\033[2J"
	MoveCursor     = "\033[%d;%dH"  // row, col
	HideCursor     = "\033[?25l"
	ShowCursor     = "\033[?25h"
	
	// Colors
	Reset       = "\033[0m"
	Bold        = "\033[1m"
	Dim         = "\033[2m"
	
	// Foreground colors
	FgBlack     = "\033[30m"
	FgRed       = "\033[31m"
	FgGreen     = "\033[32m"
	FgYellow    = "\033[33m"
	FgBlue      = "\033[34m"
	FgMagenta   = "\033[35m"
	FgCyan      = "\033[36m"
	FgWhite     = "\033[37m"
	
	// Background colors
	BgBlack     = "\033[40m"
	BgRed       = "\033[41m"
	BgGreen     = "\033[42m"
	BgBlue      = "\033[44m"
	
	// Box drawing characters (Unicode)
	TopLeft     = "╔"
	TopRight    = "╗"
	BottomLeft  = "╚"
	BottomRight = "╝"
	Horizontal  = "═"
	Vertical    = "║"
	TeeRight    = "╠"
	TeeLeft     = "╣"
	TeeDown     = "╦"
	TeeUp       = "╩"
	Cross       = "╬"
)

// Terminal dimensions
const (
	TermWidth  = 120
	TermHeight = 35
	
	// Panel positions
	LeftPanelWidth  = 58
	RightPanelWidth = 60
)

// ProDashboard is a professional-grade terminal UI
type ProDashboard struct {
	mu sync.RWMutex
	
	// State
	startTime time.Time
	running   bool
	
	// Market data
	markets map[string]*MarketData
	
	// Positions
	positions map[string]*PositionData
	
	// Signals
	signals []SignalData
	
	// Activity log
	logs []string
	
	// Stats
	totalTrades   int
	winningTrades int
	totalPnL      decimal.Decimal
	balance       decimal.Decimal
	
	// Update channels
	stopCh   chan struct{}
	updateCh chan struct{}
	
	// Screen buffer for diff updates
	lastScreen [][]rune
}

type MarketData struct {
	Asset       string
	LivePrice   decimal.Decimal
	PriceToBeat decimal.Decimal
	UpOdds      decimal.Decimal
	DownOdds    decimal.Decimal
	LastUpdate  time.Time
}

type PositionData struct {
	Asset      string
	Side       string
	EntryPrice decimal.Decimal
	CurrentPrice decimal.Decimal
	Size       int64
	PnL        decimal.Decimal
	PnLPct     decimal.Decimal
	Status     string
}

type SignalData struct {
	Asset  string
	Side   string
	Price  decimal.Decimal
	Prob   decimal.Decimal
	Signal string
	Time   time.Time
}

// NewProDashboard creates a new professional dashboard
func NewProDashboard() *ProDashboard {
	return &ProDashboard{
		startTime: time.Now(),
		running:   false,
		markets:   make(map[string]*MarketData),
		positions: make(map[string]*PositionData),
		signals:   make([]SignalData, 0),
		logs:      make([]string, 0),
		totalPnL:  decimal.Zero,
		balance:   decimal.Zero,
		stopCh:    make(chan struct{}),
		updateCh:  make(chan struct{}, 100),
	}
}

// Start begins the dashboard
func (d *ProDashboard) Start() {
	d.running = true
	d.startTime = time.Now()
	
	// Hide cursor and clear screen
	fmt.Print(HideCursor)
	fmt.Print(ClearScreen)
	
	// Draw static frame once
	d.drawFrame()
	
	// Start update loop
	go d.updateLoop()
}

// Stop stops the dashboard
func (d *ProDashboard) Stop() {
	d.running = false
	close(d.stopCh)
	fmt.Print(ShowCursor)
	fmt.Printf(MoveCursor, TermHeight+1, 1)
}

// updateLoop handles screen updates
func (d *ProDashboard) updateLoop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	
	for {
		select {
		case <-d.stopCh:
			return
		case <-ticker.C:
			d.updateContent()
		case <-d.updateCh:
			d.updateContent()
		}
	}
}

// drawFrame draws the static frame (borders) once
func (d *ProDashboard) drawFrame() {
	// Clear and position
	fmt.Print(ClearScreen)
	
	// === TOP HEADER BAR ===
	d.drawBox(1, 1, TermWidth, 3)
	
	// === LEFT PANEL: Market Data ===
	d.drawBox(1, 4, LeftPanelWidth, 10)
	d.writeAt(3, 4, FgCyan+Bold+" 📊 MARKET DATA & PRICE TO BEAT"+Reset)
	d.writeAt(3, 5, FgWhite+Dim+"Asset    │ Live Price   │ Price2Beat   │   UP   │  DOWN "+Reset)
	d.writeAt(3, 6, strings.Repeat("─", 54))
	
	// === RIGHT PANEL: Active Positions ===
	d.drawBox(LeftPanelWidth, 4, RightPanelWidth+2, 10)
	d.writeAt(LeftPanelWidth+2, 4, FgGreen+Bold+" 📈 ACTIVE POSITIONS"+Reset)
	d.writeAt(LeftPanelWidth+2, 5, FgWhite+Dim+"Asset │ Side │ Entry  │  Now   │  Size │    P&L    │ Status"+Reset)
	d.writeAt(LeftPanelWidth+2, 6, strings.Repeat("─", 58))
	
	// === BOTTOM LEFT: ML Signals ===
	d.drawBox(1, 14, LeftPanelWidth, 12)
	d.writeAt(3, 14, FgMagenta+Bold+" 🧠 ML SIGNALS & ANALYSIS"+Reset)
	d.writeAt(3, 15, FgWhite+Dim+"Asset │ Side │ Price │ P(rev) │  Edge  │ EV    │ Signal"+Reset)
	d.writeAt(3, 16, strings.Repeat("─", 54))
	
	// === BOTTOM RIGHT: Activity Log ===
	d.drawBox(LeftPanelWidth, 14, RightPanelWidth+2, 12)
	d.writeAt(LeftPanelWidth+2, 14, FgYellow+Bold+" 📋 ACTIVITY LOG"+Reset)
	d.writeAt(LeftPanelWidth+2, 15, strings.Repeat("─", 58))
	
	// === FOOTER ===
	d.writeAt(2, TermHeight-1, FgWhite+Dim+"Press Ctrl+C to exit │ Polybot ML Scalper v4.0"+Reset)
}

// drawBox draws a box with borders
func (d *ProDashboard) drawBox(x, y, width, height int) {
	// Top border
	d.writeAt(x, y, FgCyan+TopLeft+strings.Repeat(Horizontal, width-2)+TopRight+Reset)
	
	// Side borders
	for row := y + 1; row < y+height-1; row++ {
		d.writeAt(x, row, FgCyan+Vertical+Reset)
		d.writeAt(x+width-1, row, FgCyan+Vertical+Reset)
	}
	
	// Bottom border
	d.writeAt(x, y+height-1, FgCyan+BottomLeft+strings.Repeat(Horizontal, width-2)+BottomRight+Reset)
}

// writeAt writes text at specific position
func (d *ProDashboard) writeAt(x, y int, text string) {
	fmt.Printf(MoveCursor, y, x)
	fmt.Print(text)
}

// updateContent updates only the dynamic content
func (d *ProDashboard) updateContent() {
	d.mu.RLock()
	defer d.mu.RUnlock()
	
	// === UPDATE HEADER ===
	uptime := time.Since(d.startTime).Round(time.Second)
	winRate := 0.0
	if d.totalTrades > 0 {
		winRate = float64(d.winningTrades) / float64(d.totalTrades) * 100
	}
	
	pnlColor := FgGreen
	pnlSign := "+"
	if d.totalPnL.LessThan(decimal.Zero) {
		pnlColor = FgRed
		pnlSign = ""
	}
	
	header := fmt.Sprintf(" 🤖 POLYBOT ML SCALPER v4.0  %s│%s  ⏱️ %s  %s│%s  📊 %d trades  %s│%s  🎯 %.1f%% win  %s│%s  %s💰 %s$%s%s ",
		FgCyan, Reset,
		uptime,
		FgCyan, Reset,
		d.totalTrades,
		FgCyan, Reset,
		winRate,
		FgCyan, Reset,
		pnlColor+Bold, pnlSign, d.totalPnL.StringFixed(2), Reset)
	
	d.writeAt(3, 2, header+strings.Repeat(" ", 20))
	
	// === UPDATE MARKET DATA ===
	row := 7
	assets := []string{"BTC", "ETH", "SOL"}
	for _, asset := range assets {
		market, exists := d.markets[asset]
		if !exists {
			market = &MarketData{Asset: asset}
		}
		
		priceStr := "$" + market.LivePrice.StringFixed(2)
		if market.LivePrice.IsZero() {
			priceStr = "connecting..."
		}
		
		p2bStr := "$" + market.PriceToBeat.StringFixed(2)
		if market.PriceToBeat.IsZero() {
			p2bStr = "waiting..."
		}
		
		upStr := market.UpOdds.Mul(decimal.NewFromInt(100)).StringFixed(0) + "¢"
		downStr := market.DownOdds.Mul(decimal.NewFromInt(100)).StringFixed(0) + "¢"
		
		// Color odds based on cheapness
		upColor := FgWhite
		downColor := FgWhite
		if market.UpOdds.LessThan(decimal.NewFromFloat(0.20)) {
			upColor = FgGreen + Bold
		}
		if market.DownOdds.LessThan(decimal.NewFromFloat(0.20)) {
			downColor = FgGreen + Bold
		}
		
		line := fmt.Sprintf("%-6s   │ %-12s │ %-12s │ %s%-6s%s │ %s%-6s%s",
			asset, priceStr, p2bStr, upColor, upStr, Reset, downColor, downStr, Reset)
		d.writeAt(3, row, line+strings.Repeat(" ", 10))
		row++
	}
	
	// === UPDATE POSITIONS ===
	row = 7
	if len(d.positions) == 0 {
		d.writeAt(LeftPanelWidth+2, row, FgYellow+"No positions - scanning..."+strings.Repeat(" ", 30)+Reset)
		row++
	} else {
		for _, pos := range d.positions {
			pnlColor := FgGreen
			pnlSign := "+"
			if pos.PnL.LessThan(decimal.Zero) {
				pnlColor = FgRed
				pnlSign = ""
			}
			
			statusIcon := "🟢"
			if pos.Status == "SELLING" {
				statusIcon = "🔄"
			} else if pos.Status == "STOP" {
				statusIcon = "🔴"
			}
			
			line := fmt.Sprintf("%-5s │ %-4s │ %-6s │ %-6s │ %-5d │ %s%s$%-7s%s │ %s",
				pos.Asset,
				pos.Side,
				pos.EntryPrice.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢",
				pos.CurrentPrice.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢",
				pos.Size,
				pnlColor, pnlSign, pos.PnL.Abs().StringFixed(2), Reset,
				statusIcon)
			d.writeAt(LeftPanelWidth+2, row, line+strings.Repeat(" ", 10))
			row++
			if row > 12 {
				break
			}
		}
	}
	// Clear remaining rows
	for ; row <= 12; row++ {
		d.writeAt(LeftPanelWidth+2, row, strings.Repeat(" ", 58))
	}
	
	// === UPDATE ML SIGNALS ===
	row = 17
	if len(d.signals) == 0 {
		d.writeAt(3, row, FgYellow+"Analyzing markets..."+strings.Repeat(" ", 35)+Reset)
	} else {
		for i, sig := range d.signals {
			if i >= 5 {
				break
			}
			
			signalColor := FgRed
			if strings.Contains(sig.Signal, "BUY") || strings.Contains(sig.Signal, "✅") {
				signalColor = FgGreen
			} else if strings.Contains(sig.Signal, "WAIT") {
				signalColor = FgYellow
			}
			
			line := fmt.Sprintf("%-5s │ %-4s │ %-5s │ %5.0f%% │ %6s │ %5s │ %s%-8s%s",
				sig.Asset,
				sig.Side,
				sig.Price.Mul(decimal.NewFromInt(100)).StringFixed(0)+"¢",
				sig.Prob.Mul(decimal.NewFromInt(100)).InexactFloat64(),
				"-",
				"-",
				signalColor, sig.Signal, Reset)
			d.writeAt(3, row, line+strings.Repeat(" ", 10))
			row++
		}
	}
	// Clear remaining
	for ; row <= 24; row++ {
		d.writeAt(3, row, strings.Repeat(" ", 54))
	}
	
	// === UPDATE ACTIVITY LOG ===
	row = 16
	maxLogs := 8
	start := 0
	if len(d.logs) > maxLogs {
		start = len(d.logs) - maxLogs
	}
	for i := start; i < len(d.logs); i++ {
		log := d.logs[i]
		if len(log) > 56 {
			log = log[:53] + "..."
		}
		d.writeAt(LeftPanelWidth+2, row, log+strings.Repeat(" ", 58-len(log)))
		row++
	}
	// Clear remaining
	for ; row <= 24; row++ {
		d.writeAt(LeftPanelWidth+2, row, strings.Repeat(" ", 58))
	}
}

// === PUBLIC UPDATE METHODS ===

// UpdateMarket updates market data
func (d *ProDashboard) UpdateMarket(asset string, livePrice, priceToBeat, upOdds, downOdds decimal.Decimal) {
	d.mu.Lock()
	defer d.mu.Unlock()
	
	d.markets[asset] = &MarketData{
		Asset:       asset,
		LivePrice:   livePrice,
		PriceToBeat: priceToBeat,
		UpOdds:      upOdds,
		DownOdds:    downOdds,
		LastUpdate:  time.Now(),
	}
	
	d.triggerUpdate()
}

// UpdatePosition updates or adds a position
func (d *ProDashboard) UpdatePosition(asset, side string, entry, current decimal.Decimal, size int64, status string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	
	pnl := current.Sub(entry).Mul(decimal.NewFromInt(size))
	pnlPct := decimal.Zero
	if !entry.IsZero() {
		pnlPct = current.Sub(entry).Div(entry).Mul(decimal.NewFromInt(100))
	}
	
	d.positions[asset] = &PositionData{
		Asset:        asset,
		Side:         side,
		EntryPrice:   entry,
		CurrentPrice: current,
		Size:         size,
		PnL:          pnl,
		PnLPct:       pnlPct,
		Status:       status,
	}
	
	d.triggerUpdate()
}

// RemovePosition removes a position
func (d *ProDashboard) RemovePosition(asset string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.positions, asset)
	d.triggerUpdate()
}

// AddSignal adds an ML signal
func (d *ProDashboard) AddSignal(asset, side string, price, prob decimal.Decimal, signal string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	
	// Add to front, keep max 10
	sig := SignalData{
		Asset:  asset,
		Side:   side,
		Price:  price,
		Prob:   prob,
		Signal: signal,
		Time:   time.Now(),
	}
	
	d.signals = append([]SignalData{sig}, d.signals...)
	if len(d.signals) > 10 {
		d.signals = d.signals[:10]
	}
	
	d.triggerUpdate()
}

// AddLog adds a log entry
func (d *ProDashboard) AddLog(msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	
	timestamp := time.Now().Format("15:04:05")
	d.logs = append(d.logs, fmt.Sprintf("[%s] %s", timestamp, msg))
	if len(d.logs) > 50 {
		d.logs = d.logs[len(d.logs)-50:]
	}
	
	d.triggerUpdate()
}

// UpdateStats updates overall stats
func (d *ProDashboard) UpdateStats(totalTrades, winningTrades int, totalPnL, balance decimal.Decimal) {
	d.mu.Lock()
	defer d.mu.Unlock()
	
	d.totalTrades = totalTrades
	d.winningTrades = winningTrades
	d.totalPnL = totalPnL
	d.balance = balance
	
	d.triggerUpdate()
}

func (d *ProDashboard) triggerUpdate() {
	select {
	case d.updateCh <- struct{}{}:
	default:
	}
}

// Writer returns an io.Writer for log capture
func (d *ProDashboard) Writer() *ProDashboardWriter {
	return &ProDashboardWriter{dashboard: d}
}

// ProDashboardWriter implements io.Writer
type ProDashboardWriter struct {
	dashboard *ProDashboard
}

func (w *ProDashboardWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	if msg != "" && len(msg) > 10 {
		// Extract just the message part from zerolog output
		if idx := strings.Index(msg, "msg="); idx > 0 {
			msg = msg[idx+4:]
		}
		w.dashboard.AddLog(msg)
	}
	return len(p), nil
}
