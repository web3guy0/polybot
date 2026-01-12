

package dashboard

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"golang.org/x/term"
)

// ═══════════════════════════════════════════════════════════════════════════
// RESPONSIVE PROFESSIONAL DASHBOARD - Institutional Grade
// ═══════════════════════════════════════════════════════════════════════════
//
// Features:
// - Auto-detects terminal size
// - Responsive layout (adapts to any screen)
// - Smooth updates without flicker
// - Professional color scheme
// - Real-time metrics

// Layout modes based on terminal width
type LayoutMode int

const (
	LayoutCompact LayoutMode = iota // < 80 cols: stacked panels
	LayoutMedium                    // 80-120 cols: 2 columns
	LayoutWide                      // 120-160 cols: 3 columns
	LayoutUltra                     // 160+ cols: 4 columns
)

// Color scheme - Institutional dark theme
const (
	// Base colors
	cReset     = "\033[0m"
	cBold      = "\033[1m"
	cDim       = "\033[2m"
	cUnderline = "\033[4m"

	// Theme colors
	cPrimary   = "\033[38;5;39m"  // Cyan (thinner look)
	cSecondary = "\033[38;5;248m" // Gray
	cAccent    = "\033[38;5;220m" // Gold
	cSuccess   = "\033[38;5;82m"  // Green
	cDanger    = "\033[38;5;196m" // Red
	cWarning   = "\033[38;5;214m" // Orange
	cInfo      = "\033[38;5;39m"  // Cyan

	// Background
	cBgPanel  = "\033[48;5;235m" // Dark gray (subtler)
	cBgHeader = "\033[48;5;17m"  // Very dark blue
	cBgRow    = "\033[48;5;234m" // Slightly lighter

	// Box drawing - thick double lines for institutional grade look
	boxTL = "╔"
	boxTR = "╗"
	boxBL = "╚"
	boxBR = "╝"
	boxH  = "═"
	boxV  = "║"
	boxHB = "═"
	boxVB = "║"

	// Status indicators
	dotFilled = "●"
	dotEmpty  = "○"
	arrowUp   = "▲"
	arrowDown = "▼"
	barFull   = "█"
	barEmpty  = "░"
)

// ResponsiveDash is a professional responsive terminal dashboard
type ResponsiveDash struct {
	mu sync.RWMutex

	// Terminal dimensions
	width  int
	height int
	layout LayoutMode

	// State
	startTime time.Time
	running   bool
	stopCh    chan struct{}
	updateCh  chan struct{}

	// Data
	markets   map[string]*RMarketData
	positions map[string]*RPositionData
	signals   []RSignalData
	mlSignals map[string]*RMLSignal // ML analysis by asset
	logs      []string

	// Stats (cached to prevent flickering)
	totalTrades      int
	winningTrades    int
	totalPnL         decimal.Decimal
	balance          decimal.Decimal
	dayPnL           decimal.Decimal
	lastStatsUpdate  time.Time // Throttle stats updates to every 30s

	// Strategy info
	strategyName string
	strategyMode string
}

// RMarketData holds market information
type RMarketData struct {
	Asset         string
	LivePrice     decimal.Decimal
	PriceToBeat   decimal.Decimal
	UpOdds        decimal.Decimal
	DownOdds      decimal.Decimal
	Spread        decimal.Decimal
	Volume24h     decimal.Decimal
	TimeRemaining time.Duration // Time until window ends
	UpdatedAt     time.Time
}

// RPositionData holds position information
type RPositionData struct {
	Asset      string
	Side       string
	EntryPrice decimal.Decimal
	Current    decimal.Decimal
	Size       int64
	PnL        decimal.Decimal
	PnLPct     float64
	Status     string
	HoldTime   time.Duration
}

// RSignalData holds signal information
type RSignalData struct {
	Time       time.Time
	Asset      string
	Side       string
	Type       string // "BUY", "SELL", "SIGNAL"
	Price      decimal.Decimal
	Reason     string
	Confidence float64
}

// RMLSignal holds ML analysis data for display
type RMLSignal struct {
	Asset    string
	Side     string
	Price    decimal.Decimal
	ProbRev  float64 // P(reversal)
	Edge     string
	EV       string
	Signal   string
}

// NewResponsiveDash creates a new responsive dashboard
func NewResponsiveDash(strategyName string) *ResponsiveDash {
	return &ResponsiveDash{
		startTime:    time.Now(),
		stopCh:       make(chan struct{}),
		updateCh:     make(chan struct{}, 10),
		markets:      make(map[string]*RMarketData),
		positions:    make(map[string]*RPositionData),
		signals:      make([]RSignalData, 0, 50),
		mlSignals:    make(map[string]*RMLSignal),
		logs:         make([]string, 0, 100),
		totalPnL:     decimal.Zero,
		balance:      decimal.Zero,
		dayPnL:       decimal.Zero,
		strategyName: strategyName,
		strategyMode: "LIVE",
	}
}

// Start begins the dashboard render loop
func (d *ResponsiveDash) Start() {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return
	}
	d.running = true
	d.mu.Unlock()

	// Get initial terminal size
	d.updateTerminalSize()

	// Hide cursor, clear screen
	fmt.Print("\033[?25l") // Hide cursor
	fmt.Print("\033[2J")   // Clear screen

	go d.renderLoop()
	go d.sizeMonitor()
}

// Stop halts the dashboard
func (d *ResponsiveDash) Stop() {
	d.mu.Lock()
	if !d.running {
		d.mu.Unlock()
		return
	}
	d.running = false
	d.mu.Unlock()

	close(d.stopCh)

	// Show cursor, reset terminal
	fmt.Print("\033[?25h") // Show cursor
	fmt.Print("\033[0m")   // Reset colors
}

// updateTerminalSize gets current terminal dimensions
func (d *ResponsiveDash) updateTerminalSize() {
	width, height, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		width = 120
		height = 40
	}

	d.mu.Lock()
	d.width = width
	d.height = height

	// Determine layout mode
	switch {
	case width < 80:
		d.layout = LayoutCompact
	case width < 120:
		d.layout = LayoutMedium
	case width < 160:
		d.layout = LayoutWide
	default:
		d.layout = LayoutUltra
	}
	d.mu.Unlock()
}

// sizeMonitor watches for terminal resize
func (d *ResponsiveDash) sizeMonitor() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopCh:
			return
		case <-ticker.C:
			oldWidth, oldHeight := d.width, d.height
			d.updateTerminalSize()
			if d.width != oldWidth || d.height != oldHeight {
				// Terminal resized - trigger full redraw
				fmt.Print("\033[2J") // Clear screen
				d.triggerUpdate()
			}
		}
	}
}

// renderLoop is the main render loop
func (d *ResponsiveDash) renderLoop() {
	// Initial render
	d.render()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopCh:
			return
		case <-d.updateCh:
			d.render()
		case <-ticker.C:
			d.render()
		}
	}
}

// render draws the entire dashboard
func (d *ResponsiveDash) render() {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var buf strings.Builder
	buf.Grow(d.width * d.height * 2) // Pre-allocate

	// Move to home position
	buf.WriteString("\033[H")

	// Draw based on layout
	switch d.layout {
	case LayoutCompact:
		d.renderCompact(&buf)
	case LayoutMedium:
		d.renderMedium(&buf)
	case LayoutWide:
		d.renderWide(&buf)
	default:
		d.renderUltra(&buf)
	}

	// Output entire buffer at once (prevents flicker)
	fmt.Print(buf.String())
}

// ═══════════════════════════════════════════════════════════════════════════
// LAYOUT RENDERERS
// ═══════════════════════════════════════════════════════════════════════════

func (d *ResponsiveDash) renderCompact(buf *strings.Builder) {
	// Compact: Single column, essential info only
	panelH := (d.height - 6) / 3
	if panelH < 4 {
		panelH = 4
	}
	
	d.drawHeader(buf, d.width)
	d.drawStatsBar(buf, d.width)
	d.drawPanel(buf, "📋 POSITIONS", 0, 4, d.width, panelH, d.renderPositionsContent)
	d.drawPanel(buf, "🧠 ML SIGNALS", 0, 4+panelH, d.width, panelH, d.renderMLSignalsContent)
	d.drawPanel(buf, "📝 LOG", 0, 4+panelH*2, d.width, panelH, d.renderLogContent)
	d.drawStatusBar(buf, d.width, d.height)
}

func (d *ResponsiveDash) renderMedium(buf *strings.Builder) {
	// Medium: 2 columns, fixed proportions for stability
	leftW := d.width / 2
	rightW := d.width - leftW
	
	// Calculate panel heights
	panelH := (d.height - 6) / 2 // -4 for header/stats, -2 for status
	if panelH < 6 {
		panelH = 6
	}

	d.drawHeader(buf, d.width)
	d.drawStatsBar(buf, d.width)

	// Top row: Market Data (left) + Active Positions (right)  
	d.drawPanel(buf, "📊 MARKET DATA & PRICE TO BEAT", 0, 4, leftW, panelH, d.renderMarketContent)
	d.drawPanel(buf, "📋 ACTIVE POSITIONS", leftW, 4, rightW, panelH, d.renderPositionsContent)

	// Bottom row: ML Signals (left) + Activity Log (right)
	d.drawPanel(buf, "🧠 ML SIGNALS & ANALYSIS", 0, 4+panelH, leftW, panelH, d.renderMLSignalsContent)
	d.drawPanel(buf, "📝 ACTIVITY LOG", leftW, 4+panelH, rightW, panelH, d.renderLogContent)

	d.drawStatusBar(buf, d.width, d.height)
}
func (d *ResponsiveDash) renderWide(buf *strings.Builder) {
	// Bloomberg style: 3 panels top, 1 full-width log bottom
	col1W := d.width / 3
	col2W := d.width / 3
	col3W := d.width - col1W - col2W

	// Top panels get 45% height, activity log gets 55% (more space for logs)
	topH := (d.height - 6) * 45 / 100
	if topH < 8 {
		topH = 8
	}
	bottomH := d.height - 6 - topH
	if bottomH < 8 {
		bottomH = 8
	}

	d.drawHeader(buf, d.width)
	d.drawStatsBar(buf, d.width)

	// Top row: Market Data | Positions | ML Signals
	d.drawPanel(buf, "📊 MARKET DATA", 0, 4, col1W, topH, d.renderMarketContent)
	d.drawPanel(buf, "📋 POSITIONS", col1W, 4, col2W, topH, d.renderPositionsContent)
	d.drawPanel(buf, "🧠 ML SIGNALS", col1W+col2W, 4, col3W, topH, d.renderMLSignalsContent)

	// Bottom row: Full-width Activity Log
	d.drawPanel(buf, "📝 ACTIVITY LOG", 0, 4+topH, d.width, bottomH, d.renderLogContent)

	d.drawStatusBar(buf, d.width, d.height)
}

func (d *ResponsiveDash) renderUltra(buf *strings.Builder) {
	// Bloomberg style: 3 panels top, 1 full-width log bottom
	col1W := d.width / 3
	col2W := d.width / 3
	col3W := d.width - col1W - col2W

	// Top panels get 45% height, activity log gets 55% (more space for logs)
	topH := (d.height - 6) * 45 / 100
	if topH < 8 {
		topH = 8
	}
	bottomH := d.height - 6 - topH
	if bottomH < 8 {
		bottomH = 8
	}

	d.drawHeader(buf, d.width)
	d.drawStatsBar(buf, d.width)

	// Top row: Market Data | Positions | ML Signals
	d.drawPanel(buf, "📊 MARKET DATA", 0, 4, col1W, topH, d.renderMarketContent)
	d.drawPanel(buf, "📋 POSITIONS", col1W, 4, col2W, topH, d.renderPositionsContent)
	d.drawPanel(buf, "🧠 ML SIGNALS", col1W+col2W, 4, col3W, topH, d.renderMLSignalsContent)

	// Bottom row: Full-width Activity Log
	d.drawPanel(buf, "📝 ACTIVITY LOG", 0, 4+topH, d.width, bottomH, d.renderLogContent)

	d.drawStatusBar(buf, d.width, d.height)
}

// ═══════════════════════════════════════════════════════════════════════════
// COMPONENT RENDERERS
// ═══════════════════════════════════════════════════════════════════════════

func (d *ResponsiveDash) drawHeader(buf *strings.Builder, width int) {
	// Header bar
	buf.WriteString(cBgHeader + cBold)

	title := fmt.Sprintf(" 🤖 POLYBOT %s ", d.strategyName)
	mode := fmt.Sprintf(" %s ", d.strategyMode)
	uptime := fmt.Sprintf(" ⏱ %s ", d.formatDuration(time.Since(d.startTime)))

	// Center title
	padding := width - len(title) - len(mode) - len(uptime) - 4
	if padding < 0 {
		padding = 0
	}

	buf.WriteString(cPrimary + title)
	buf.WriteString(strings.Repeat(" ", padding/2))
	buf.WriteString(cAccent + mode)
	buf.WriteString(strings.Repeat(" ", padding-padding/2))
	buf.WriteString(cSecondary + uptime)
	buf.WriteString(cReset + "\n")
}

func (d *ResponsiveDash) drawStatsBar(buf *strings.Builder, width int) {
	// Stats bar with key metrics
	buf.WriteString(cBgPanel)

	// Calculate win rate
	winRate := 0.0
	if d.totalTrades > 0 {
		winRate = float64(d.winningTrades) / float64(d.totalTrades) * 100
	}

	// Format P&L with color
	pnlColor := cSuccess
	pnlSign := "+"
	if d.totalPnL.LessThan(decimal.Zero) {
		pnlColor = cDanger
		pnlSign = ""
	}

	dayPnlColor := cSuccess
	dayPnlSign := "+"
	if d.dayPnL.LessThan(decimal.Zero) {
		dayPnlColor = cDanger
		dayPnlSign = ""
	}

	stats := fmt.Sprintf(
		" %s💰 Balance: $%.2f%s │ %sP&L: %s$%.2f%s │ %sToday: %s$%.2f%s │ %s📊 Trades: %d (%d W)%s │ %s🎯 Win: %.1f%%%s ",
		cSecondary, d.balance.InexactFloat64(), cReset+cBgPanel,
		cSecondary, pnlColor+pnlSign, d.totalPnL.InexactFloat64(), cReset+cBgPanel,
		cSecondary, dayPnlColor+dayPnlSign, d.dayPnL.InexactFloat64(), cReset+cBgPanel,
		cSecondary, d.totalTrades, d.winningTrades, cReset+cBgPanel,
		cSecondary, winRate, cReset+cBgPanel,
	)

	// Pad to width
	visibleLen := d.visibleLen(stats)
	if visibleLen < width {
		stats += strings.Repeat(" ", width-visibleLen)
	}

	buf.WriteString(stats)
	buf.WriteString(cReset + "\n")
}

func (d *ResponsiveDash) drawPanel(buf *strings.Builder, title string, x, y, width, height int, contentFn func(*strings.Builder, int, int)) {
	if width < 10 || height < 4 {
		return
	}

	// Move to position
	buf.WriteString(fmt.Sprintf("\033[%d;%dH", y+1, x+1))

	// Top border with title
	buf.WriteString(cPrimary + boxTL)
	titleStr := fmt.Sprintf(" %s ", title)
	remaining := width - 2 - len(titleStr)
	if remaining > 0 {
		buf.WriteString(strings.Repeat(boxH, 2))
		buf.WriteString(cAccent + cBold + titleStr + cReset + cPrimary)
		buf.WriteString(strings.Repeat(boxH, remaining-2))
	}
	buf.WriteString(boxTR + cReset)

	// Content area
	contentBuf := &strings.Builder{}
	contentFn(contentBuf, width-4, height-2)
	contentLines := strings.Split(contentBuf.String(), "\n")

	for i := 0; i < height-2; i++ {
		buf.WriteString(fmt.Sprintf("\033[%d;%dH", y+2+i, x+1))
		buf.WriteString(cPrimary + boxV + cReset)

		content := ""
		if i < len(contentLines) {
			content = contentLines[i]
		}

		// Pad content to panel width
		visLen := d.visibleLen(content)
		padding := width - 4 - visLen
		if padding < 0 {
			padding = 0
			// Truncate content
			content = d.truncate(content, width-4)
		}

		buf.WriteString(" " + content + strings.Repeat(" ", padding) + " ")
		buf.WriteString(cPrimary + boxV + cReset)
	}

	// Bottom border
	buf.WriteString(fmt.Sprintf("\033[%d;%dH", y+height, x+1))
	buf.WriteString(cPrimary + boxBL + strings.Repeat(boxH, width-2) + boxBR + cReset)
}

func (d *ResponsiveDash) drawStatusBar(buf *strings.Builder, width, height int) {
	buf.WriteString(fmt.Sprintf("\033[%d;1H", height))
	buf.WriteString(cBgHeader + cDim)

	now := time.Now().Format("15:04:05")
	help := "q:Quit │ h:Help │ r:Refresh"
	layout := fmt.Sprintf("%dx%d %s", width, height, d.layoutName())

	content := fmt.Sprintf(" %s │ %s │ %s ", now, help, layout)
	padding := width - len(content)
	if padding < 0 {
		padding = 0
	}

	buf.WriteString(content)
	buf.WriteString(strings.Repeat(" ", padding))
	buf.WriteString(cReset)
}

// ═══════════════════════════════════════════════════════════════════════════
// CONTENT RENDERERS
// ═══════════════════════════════════════════════════════════════════════════

func (d *ResponsiveDash) renderMarketContent(buf *strings.Builder, width, height int) {
	// Professional table header with separators
	// Institutional-grade table with thick borders and proper columns
	if width >= 50 {
		buf.WriteString(fmt.Sprintf("%s%-5s ║ %-10s ║ %-10s ║ %5s ║ %3s ║ %3s ║ %5s%s\n",
			cSecondary+cBold, "Asset", "Price2Beat", "LivePrice", "Move%", "UP", "DN", "Time", cReset))
		buf.WriteString(cSecondary + "══════╬════════════╬════════════╬═══════╬════╬════╬══════" + cReset + "\n")
	} else if width >= 35 {
		buf.WriteString(fmt.Sprintf("%s%-5s ║ %-10s ║ %5s ║ %3s ║ %3s%s\n",
			cSecondary+cBold, "Asset", "LivePrice", "Move%", "UP", "DN", cReset))
		buf.WriteString(cSecondary + "══════╬════════════╬═══════╬════╬════" + cReset + "\n")
	} else {
		buf.WriteString(fmt.Sprintf("%sAsset ║ UP ║ DN%s\n", cSecondary+cBold, cReset))
		buf.WriteString(cSecondary + "══════╬════╬════" + cReset + "\n")
	}

	// Fixed order: BTC, ETH, SOL - always stable
	assets := []string{"BTC", "ETH", "SOL"}
	row := 0
	for _, asset := range assets {
		m, exists := d.markets[asset]
		if !exists {
			// Show placeholder row if no data yet
			if width >= 35 {
				buf.WriteString(fmt.Sprintf("%-5s ║ %s%-11s%s ║ %-11s ║ %-4s ║ %-4s\n",
					asset, cDim, "---", cReset, "---", "---", "---"))
			} else if width >= 20 {
				buf.WriteString(fmt.Sprintf("%-5s ║ %s%-9s%s ║ %-3s ║ %-3s\n",
					asset, cDim, "---", cReset, "---", "---"))
			} else {
				buf.WriteString(fmt.Sprintf("%-5s ║ %-3s ║ %-3s\n", asset, "---", "---"))
			}
			row++
			continue
		}
		if row >= height-3 {
			break
		}

		// Format prices
		livePrice := m.LivePrice.InexactFloat64()
		priceToBeat := m.PriceToBeat.InexactFloat64()
		upOdds := m.UpOdds.Mul(decimal.NewFromInt(100)).InexactFloat64()
		downOdds := m.DownOdds.Mul(decimal.NewFromInt(100)).InexactFloat64()
		
		// Calculate price move percentage
		var moveColor string
		var moveSign string
		movePct := 0.0
		if priceToBeat > 0 {
			movePct = ((livePrice - priceToBeat) / priceToBeat) * 100
		}
		if movePct > 0.05 {
			moveColor = cSuccess + cBold
			moveSign = "+"
		} else if movePct < -0.05 {
			moveColor = cDanger + cBold
			moveSign = ""
		} else {
			moveColor = cDim
			moveSign = "+"
			if movePct < 0 {
				moveSign = ""
			}
		}
		
		// Color odds - highlight cheap opportunities
		upColor := cDim
		downColor := cDim
		if upOdds <= 25 {
			upColor = cSuccess + cBold
		} else if upOdds <= 40 {
			upColor = cSuccess
		}
		if downOdds <= 25 {
			downColor = cSuccess + cBold
		} else if downOdds <= 40 {
			downColor = cDanger
		}

		// Format time remaining
		timeStr := "--"
		timeColor := cDim
		if m.TimeRemaining > 0 {
			mins := int(m.TimeRemaining.Minutes())
			if mins < 5 {
				timeColor = cDanger + cBold // Last 5 min = HOT
				timeStr = fmt.Sprintf("%dm", mins)
			} else if mins < 30 {
				timeColor = cWarning
				timeStr = fmt.Sprintf("%dm", mins)
			} else if mins < 60 {
				timeStr = fmt.Sprintf("%dm", mins)
			} else {
				timeStr = fmt.Sprintf("%dh", mins/60)
			}
		}

		if width >= 50 {
			buf.WriteString(fmt.Sprintf("%-5s ║ %s$%9.2f%s ║ %s$%9.2f%s ║ %s%s%4.2f%%%s ║ %s%2.0f¢%s ║ %s%2.0f¢%s ║ %s%5s%s\n",
				asset,
				cDim, priceToBeat, cReset,
				cSecondary, livePrice, cReset,
				moveColor, moveSign, movePct, cReset,
				upColor, upOdds, cReset,
				downColor, downOdds, cReset,
				timeColor, timeStr, cReset,
			))
		} else if width >= 35 {
			buf.WriteString(fmt.Sprintf("%-5s ║ %s$%9.2f%s ║ %s%s%4.2f%%%s ║ %s%2.0f¢%s ║ %s%2.0f¢%s\n",
				asset,
				cSecondary, livePrice, cReset,
				moveColor, moveSign, movePct, cReset,
				upColor, upOdds, cReset,
				downColor, downOdds, cReset,
			))
		} else {
			buf.WriteString(fmt.Sprintf("%-5s ║ %s%3.0f¢%s ║ %s%3.0f¢%s\n",
				asset,
				upColor, upOdds, cReset,
				downColor, downOdds, cReset,
			))
		}
		row++
	}
}

func (d *ResponsiveDash) renderPositionsContent(buf *strings.Builder, width, height int) {
	if len(d.positions) == 0 {
		buf.WriteString(cDim + "No positions - scanning..." + cReset)
		return
	}

	// Professional table header with thick separators
	if width >= 75 {
		buf.WriteString(fmt.Sprintf("%sAsset  ║ Side ║ Entry ║  Now  ║ Size ║   P&L   ║ Status%s\n",
			cSecondary+cBold, cReset))
		buf.WriteString(cSecondary + "═══════╬══════╬═══════╬═══════╬══════╬═════════╬════════" + cReset + "\n")
	} else if width >= 50 {
		buf.WriteString(fmt.Sprintf("%sAsset ║ Side ║ Entry ║  P&L  ║ Status%s\n",
			cSecondary+cBold, cReset))
		buf.WriteString(cSecondary + "══════╬══════╬═══════╬═══════╬═══════" + cReset + "\n")
	} else {
		buf.WriteString(fmt.Sprintf("%sAsset ║ Side ║  P&L%s\n", cSecondary+cBold, cReset))
		buf.WriteString(cSecondary + "══════╬══════╬═══════" + cReset + "\n")
	}

	row := 0
	for _, p := range d.positions {
		if row >= height-1 {
			break
		}

		// P&L color
		pnlColor := cSuccess
		pnlSign := "+"
		if p.PnL.LessThan(decimal.Zero) {
			pnlColor = cDanger
			pnlSign = ""
		}

		// Side color
		sideColor := cSuccess
		if p.Side == "DOWN" {
			sideColor = cDanger
		}

		// Status indicator with text
		statusText := "OPEN"
		statusColor := cSuccess
		switch p.Status {
		case "OPEN":
			statusText = "● OPEN"
			statusColor = cSuccess
		case "CLOSING":
			statusText = "◐ EXIT"
			statusColor = cWarning
		case "STOP":
			statusText = "◉ STOP"
			statusColor = cDanger
		}

		entry := p.EntryPrice.Mul(decimal.NewFromInt(100)).InexactFloat64()
		curr := p.Current.Mul(decimal.NewFromInt(100)).InexactFloat64()

		if width >= 75 {
			buf.WriteString(fmt.Sprintf("%-6s │ %s%-4s%s │ %4.0f¢ │ %4.0f¢ │ %4d │ %s%s$%-5.2f%s │ %s%s%s\n",
				p.Asset,
				sideColor, p.Side, cReset,
				entry, curr,
				p.Size,
				pnlColor, pnlSign, p.PnL.InexactFloat64(), cReset,
				statusColor, statusText, cReset,
			))
		} else if width >= 50 {
			buf.WriteString(fmt.Sprintf("%-5s │ %s%-4s%s │ %3.0f¢ │ %s%s%.2f%s │ %s%s%s\n",
				p.Asset,
				sideColor, p.Side, cReset,
				entry,
				pnlColor, pnlSign, p.PnL.InexactFloat64(), cReset,
				statusColor, statusText, cReset,
			))
		} else {
			buf.WriteString(fmt.Sprintf("%-5s │ %s%-4s%s │ %s%s%.2f%s\n",
				p.Asset,
				sideColor, p.Side, cReset,
				pnlColor, pnlSign, p.PnL.InexactFloat64(), cReset,
			))
		}
		row++
	}
}

func (d *ResponsiveDash) renderSignalsContent(buf *strings.Builder, width, height int) {
	if len(d.signals) == 0 {
		buf.WriteString(cDim + "Waiting for signals..." + cReset)
		return
	}

	// Show most recent signals first
	start := len(d.signals) - height
	if start < 0 {
		start = 0
	}

	for i := len(d.signals) - 1; i >= start; i-- {
		s := d.signals[i]

		// Type color and icon
		typeStr := ""
		switch s.Type {
		case "BUY":
			typeStr = cSuccess + "▶ BUY " + cReset
		case "SELL":
			typeStr = cDanger + "◀ SELL" + cReset
		case "STOP":
			typeStr = cWarning + "⬤ STOP" + cReset
		default:
			typeStr = cInfo + "◉ SIG " + cReset
		}

		// Side color
		sideColor := cSuccess
		if s.Side == "DOWN" {
			sideColor = cDanger
		}

		timeStr := s.Time.Format("15:04:05")

		if width >= 50 {
			buf.WriteString(fmt.Sprintf("%s %s %-6s %s%-4s%s %.0f¢\n",
				cDim+timeStr+cReset,
				typeStr,
				s.Asset,
				sideColor, s.Side, cReset,
				s.Price.Mul(decimal.NewFromInt(100)).InexactFloat64(),
			))
		} else {
			buf.WriteString(fmt.Sprintf("%s %-5s %s%-3s%s\n",
				typeStr, s.Asset,
				sideColor, s.Side[:2], cReset,
			))
		}
	}
}

func (d *ResponsiveDash) renderMLSignalsContent(buf *strings.Builder, width, height int) {
	if len(d.mlSignals) == 0 {
		buf.WriteString(cDim + "Analyzing markets...\n" + cReset)
		buf.WriteString(cDim + "ML model processing..." + cReset)
		return
	}

	// Professional table header with thick separators
	if width >= 70 {
		buf.WriteString(fmt.Sprintf("%sAsset  ║ Side ║ P(win) ║ Edge  ║   EV   ║ Signal%s\n",
			cSecondary+cBold, cReset))
		buf.WriteString(cSecondary + "═══════╬══════╬════════╬═══════╬════════╬══════════" + cReset + "\n")
	} else if width >= 45 {
		buf.WriteString(fmt.Sprintf("%sAsset ║ Side ║ Signal%s\n",
			cSecondary+cBold, cReset))
		buf.WriteString(cSecondary + "══════╬══════╬════════" + cReset + "\n")
	} else {
		buf.WriteString(fmt.Sprintf("%sAsset ║ Side ║ Signal%s\n", cSecondary+cBold, cReset))
		buf.WriteString(cSecondary + "══════╬══════╬════════" + cReset + "\n")
	}

	// Fixed order: BTC, ETH, SOL - always stable
	assets := []string{"BTC", "ETH", "SOL"}
	row := 0
	for _, asset := range assets {
		if row >= height-3 {
			break
		}

		ml, exists := d.mlSignals[asset]
		if !exists {
			// Show placeholder row if no data yet
			if width >= 45 {
				buf.WriteString(fmt.Sprintf("%-6s │ %s  -  %s │ %s  -  %s │ %s  -  %s\n",
					asset, cDim, cReset, cDim, cReset, cDim, cReset))
			} else {
				buf.WriteString(fmt.Sprintf("%-5s │  -  │  -\n", asset))
			}
			row++
			continue
		}

		// Side color with icon
		sideColor := cSuccess
		sideIcon := "▲"
		if ml.Side == "DOWN" {
			sideColor = cDanger
			sideIcon = "▼"
		}

		// Signal color and icon based on signal strength
		sigColor := cDim
		sigIcon := "◌"
		switch ml.Signal {
		case "STRONG_BUY":
			sigColor = cSuccess + cBold
			sigIcon = "◉"
		case "BUY":
			sigColor = cSuccess
			sigIcon = "●"
		case "STRONG_SELL", "SELL":
			sigColor = cDanger
			sigIcon = "○"
		case "HOLD":
			sigColor = cWarning
			sigIcon = "◐"
		case "SKIP":
			sigColor = cDim
			sigIcon = "◌"
		}

		// P(win) color based on probability
		probColor := cDim
		prob := ml.ProbRev * 100
		if prob >= 70 {
			probColor = cSuccess + cBold
		} else if prob >= 55 {
			probColor = cSuccess
		} else if prob >= 45 {
			probColor = cWarning
		} else {
			probColor = cDanger
		}

		if width >= 70 {
			buf.WriteString(fmt.Sprintf("%-6s │ %s%s%-3s%s │ %s%5.1f%%%s │ %-5s │ %-6s │ %s%s %s%s\n",
				ml.Asset,
				sideColor, sideIcon, ml.Side, cReset,
				probColor, prob, cReset,
				ml.Edge,
				ml.EV,
				sigColor, sigIcon, ml.Signal, cReset,
			))
		} else if width >= 45 {
			sideText := ml.Side
			if len(ml.Side) >= 2 {
				sideText = ml.Side[:2]
			}
			buf.WriteString(fmt.Sprintf("%-5s │ %s%s%-2s%s │ %s%4.0f%%%s │ %s%s%s\n",
				ml.Asset,
				sideColor, sideIcon, sideText, cReset,
				probColor, prob, cReset,
				sigColor, ml.Signal, cReset,
			))
		} else {
			buf.WriteString(fmt.Sprintf("%-5s │ %s%s%s │ %s%s%s\n",
				ml.Asset,
				sideColor, sideIcon, cReset,
				sigColor, ml.Signal, cReset,
			))
		}
		row++
	}
}

func (d *ResponsiveDash) renderLogContent(buf *strings.Builder, width, height int) {
	if len(d.logs) == 0 {
		buf.WriteString(cDim + "No activity yet..." + cReset)
		return
	}

	// Show most recent logs that fit
	maxLines := height - 2
	if maxLines < 1 {
		maxLines = 1
	}

	start := len(d.logs) - maxLines
	if start < 0 {
		start = 0
	}

	for i := start; i < len(d.logs); i++ {
		line := d.logs[i]
		// Only truncate if really needed
		if width > 10 {
			line = d.truncateAnsi(line, width-2)
		}
		buf.WriteString(line + "\n")
	}
}

// truncateAnsi truncates a string to maxLen visible characters, preserving ANSI codes
func (d *ResponsiveDash) truncateAnsi(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	
	var result strings.Builder
	visible := 0
	inEscape := false
	
	for _, r := range s {
		if r == '\033' {
			inEscape = true
			result.WriteRune(r)
			continue
		}
		if inEscape {
			result.WriteRune(r)
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		
		// Visible character
		if visible >= maxLen-3 {
			result.WriteString("...")
			break
		}
		result.WriteRune(r)
		visible++
	}
	
	return result.String()
}

// ═══════════════════════════════════════════════════════════════════════════
// PUBLIC API
// ═══════════════════════════════════════════════════════════════════════════

// UpdateMarket updates market data for an asset
func (d *ResponsiveDash) UpdateMarket(asset string, livePrice, priceToBeat, upOdds, downOdds decimal.Decimal) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Preserve existing TimeRemaining if set
	var timeRemaining time.Duration
	if existing, ok := d.markets[asset]; ok {
		timeRemaining = existing.TimeRemaining
	}

	d.markets[asset] = &RMarketData{
		Asset:         asset,
		LivePrice:     livePrice,
		PriceToBeat:   priceToBeat,
		UpOdds:        upOdds,
		DownOdds:      downOdds,
		Spread:        upOdds.Add(downOdds).Sub(decimal.NewFromInt(1)).Abs(),
		TimeRemaining: timeRemaining,
		UpdatedAt:     time.Now(),
	}

	d.triggerUpdate()
}

// UpdateMarketTime updates time remaining for a market
func (d *ResponsiveDash) UpdateMarketTime(asset string, timeRemaining time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if m, ok := d.markets[asset]; ok {
		m.TimeRemaining = timeRemaining
	}
}

// UpdatePosition updates or adds a position
func (d *ResponsiveDash) UpdatePosition(asset, side string, entry, current decimal.Decimal, size int64, status string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	pos, exists := d.positions[asset]
	if !exists {
		pos = &RPositionData{
			Asset:      asset,
			Side:       side,
			EntryPrice: entry,
			Size:       size,
		}
		d.positions[asset] = pos
	}

	pos.Current = current
	pos.Status = status
	pos.PnL = current.Sub(entry).Mul(decimal.NewFromInt(size))
	if !entry.IsZero() {
		pos.PnLPct = current.Sub(entry).Div(entry).InexactFloat64() * 100
	}

	d.triggerUpdate()
}

// RemovePosition removes a position
func (d *ResponsiveDash) RemovePosition(asset string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	delete(d.positions, asset)
	d.triggerUpdate()
}

// AddSignal adds a new signal
func (d *ResponsiveDash) AddSignal(asset, side, signalType string, price decimal.Decimal, reason string, confidence float64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.signals = append(d.signals, RSignalData{
		Time:       time.Now(),
		Asset:      asset,
		Side:       side,
		Type:       signalType,
		Price:      price,
		Reason:     reason,
		Confidence: confidence,
	})

	// Keep last 50 signals
	if len(d.signals) > 50 {
		d.signals = d.signals[len(d.signals)-50:]
	}

	d.triggerUpdate()
}

// AddLog adds a log message
func (d *ResponsiveDash) AddLog(msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Clean up the message - remove existing timestamps if present
	if len(msg) > 9 && msg[2] == ':' && msg[5] == ':' {
		// Message already has timestamp like "12:34:56 ..."
		d.logs = append(d.logs, msg)
	} else {
		timestamp := time.Now().Format("15:04:05")
		d.logs = append(d.logs, fmt.Sprintf("%s%s%s %s", cDim, timestamp, cReset, msg))
	}

	// Keep last 100 logs
	if len(d.logs) > 100 {
		d.logs = d.logs[len(d.logs)-100:]
	}

	d.triggerUpdate()
}

// UpdateStats updates overall statistics (throttled to every 30s to prevent flickering)
func (d *ResponsiveDash) UpdateStats(totalTrades, winningTrades int, totalPnL, balance decimal.Decimal) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Throttle stats updates to every 30 seconds to prevent flickering
	// Exception: always update if values changed significantly or first update
	now := time.Now()
	if !d.lastStatsUpdate.IsZero() && now.Sub(d.lastStatsUpdate) < 30*time.Second {
		// Only update if there's a significant change (new trade or balance change > $0.10)
		if totalTrades == d.totalTrades && 
		   balance.Sub(d.balance).Abs().LessThan(decimal.NewFromFloat(0.10)) {
			return // Skip update, too soon and no significant change
		}
	}

	d.totalTrades = totalTrades
	d.winningTrades = winningTrades
	d.totalPnL = totalPnL
	d.balance = balance
	d.lastStatsUpdate = now

	d.triggerUpdate()
}

// SetMode sets the trading mode (LIVE/PAPER/BACKTEST)
func (d *ResponsiveDash) SetMode(mode string) {
	d.mu.Lock()
	d.strategyMode = mode
	d.mu.Unlock()
	d.triggerUpdate()
}

// SetDayPnL sets today's P&L (only triggers update on significant change)
func (d *ResponsiveDash) SetDayPnL(pnl decimal.Decimal) {
	d.mu.Lock()
	defer d.mu.Unlock()
	
	// Only update if changed by at least $0.01 to prevent flickering
	if pnl.Sub(d.dayPnL).Abs().LessThan(decimal.NewFromFloat(0.01)) {
		return
	}
	
	d.dayPnL = pnl
	d.triggerUpdate()
}

// UpdateMLSignal updates ML analysis for an asset
func (d *ResponsiveDash) UpdateMLSignal(asset, side string, price decimal.Decimal, probRev float64, edge, ev, signal string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.mlSignals[asset] = &RMLSignal{
		Asset:   asset,
		Side:    side,
		Price:   price,
		ProbRev: probRev,
		Edge:    edge,
		EV:      ev,
		Signal:  signal,
	}

	d.triggerUpdate()
}

// ═══════════════════════════════════════════════════════════════════════════
// HELPERS
// ═══════════════════════════════════════════════════════════════════════════

func (d *ResponsiveDash) triggerUpdate() {
	select {
	case d.updateCh <- struct{}{}:
	default:
	}
}

func (d *ResponsiveDash) formatDuration(dur time.Duration) string {
	if dur < time.Minute {
		return fmt.Sprintf("%ds", int(dur.Seconds()))
	}
	if dur < time.Hour {
		return fmt.Sprintf("%dm%ds", int(dur.Minutes()), int(dur.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(dur.Hours()), int(dur.Minutes())%60)
}

func (d *ResponsiveDash) layoutName() string {
	switch d.layout {
	case LayoutCompact:
		return "COMPACT"
	case LayoutMedium:
		return "MEDIUM"
	case LayoutWide:
		return "WIDE"
	case LayoutUltra:
		return "ULTRA"
	}
	return "?"
}

// visibleLen returns the visible length of a string (excluding ANSI codes)
func (d *ResponsiveDash) visibleLen(s string) int {
	// Remove ANSI escape sequences
	inEscape := false
	count := 0
	for _, r := range s {
		if r == '\033' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		count++
	}
	return count
}

// truncate truncates a string to maxLen visible characters
func (d *ResponsiveDash) truncate(s string, maxLen int) string {
	if d.visibleLen(s) <= maxLen {
		return s
	}

	// Simple truncation - could be improved to handle ANSI codes
	runes := []rune(s)
	if len(runes) > maxLen-3 {
		return string(runes[:maxLen-3]) + "..."
	}
	return s
}

// Writer returns an io.Writer for logging
func (d *ResponsiveDash) Writer() *ResponsiveDashWriter {
	return &ResponsiveDashWriter{dash: d}
}

// ResponsiveDashWriter implements io.Writer
type ResponsiveDashWriter struct {
	dash *ResponsiveDash
}

func (w *ResponsiveDashWriter) Write(p []byte) (n int, err error) {
	msg := strings.TrimSpace(string(p))
	if msg == "" {
		return len(p), nil
	}
	
	// Parse zerolog JSON and format nicely
	formatted := w.formatZerologJSON(msg)
	if formatted != "" {
		w.dash.AddLog(formatted)
	}
	return len(p), nil
}

// formatZerologJSON parses zerolog JSON and formats it for display
func (w *ResponsiveDashWriter) formatZerologJSON(raw string) string {
	// Try to parse as JSON
	if !strings.HasPrefix(raw, "{") {
		return raw // Already formatted text
	}
	
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return raw // Not valid JSON, return as-is
	}
	
	// Get level and message
	level, _ := data["level"].(string)
	message, _ := data["message"].(string)
	
	// Skip debug level logs in dashboard
	if level == "debug" {
		return ""
	}
	
	// Skip noisy logs
	if strings.Contains(message, "Windows updated") || 
	   strings.Contains(message, "scan complete") ||
	   strings.Contains(message, "Position check") {
		return ""
	}
	
	// Build formatted output - keep it SHORT
	var sb strings.Builder
	
	// Message only (emoji already in message usually)
	if message != "" {
		sb.WriteString(message)
	}
	
	// Add asset if present and not in message
	if asset, ok := data["asset"].(string); ok && !strings.Contains(message, asset) {
		sb.WriteString(" [" + asset + "]")
	}
	
	return sb.String()
}
