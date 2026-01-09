package arbitrage

import (
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
	"github.com/web3guy0/polybot/internal/polymarket"
)

// SmartDualStrategy implements "Wait for Cheap" approach
// Instead of placing both orders upfront, we:
// 1. Wait for one side to become VERY cheap (<35¢)
// 2. Enter that side
// 3. Wait for reversal to make other side cheap
// 4. Enter other side if it gets cheap enough
//
// This exploits mean-reversion in prediction markets during volatile periods
type SmartDualStrategy struct {
	windowScanner *polymarket.WindowScanner
	clobClient    *CLOBClient

	// Configuration
	cheapThreshold  decimal.Decimal // Enter when odds drop below this (e.g., 0.35)
	maxCombinedCost decimal.Decimal // Max total cost for both sides (e.g., 0.92)
	positionSize    decimal.Decimal // USDC per side

	// Paper trading mode
	paperTrade bool

	// Tracked positions
	positions map[string]*SmartDualPosition
	mu        sync.RWMutex

	// Statistics
	stats *DualSideStats

	stopCh chan struct{}
}

// SmartDualPosition tracks a smart dual-side position
type SmartDualPosition struct {
	WindowID string
	Question string
	Asset    string

	// Window timing
	StartTime time.Time
	EndTime   time.Time

	// First entry (the cheap side)
	FirstSide      string          // "UP" or "DOWN"
	FirstEntryTime time.Time
	FirstPrice     decimal.Decimal
	FirstFilled    bool

	// Second entry (waiting for reversal)
	SecondSide       string
	SecondEntryTime  time.Time
	SecondPrice      decimal.Decimal
	SecondFilled     bool
	SecondTargetHit  bool            // Did price ever reach our target?
	SecondLowestSeen decimal.Decimal // Lowest price we saw for second side

	// Price tracking
	UpLowestSeen   decimal.Decimal
	DownLowestSeen decimal.Decimal
	UpHighestSeen  decimal.Decimal
	DownHighestSeen decimal.Decimal

	// Outcome
	Status    string // "waiting", "first_filled", "both_filled", "expired", "resolved"
	Winner    string // "UP", "DOWN", or ""
	TotalCost decimal.Decimal
	Payout    decimal.Decimal
	Profit    decimal.Decimal
}

// DualSideStats tracks paper trading statistics
type DualSideStats struct {
	mu sync.RWMutex

	// Window tracking
	TotalWindowsTracked int
	WindowsWithCheapSide int // At least one side hit cheap threshold

	// Entry stats
	FirstEntries  int // We entered first side
	SecondEntries int // We entered second side (reversal happened)
	BothFilled    int // Both sides filled

	// Outcome stats (when both filled)
	BothFilledWins   int // Both filled and we profited
	BothFilledLosses int // Both filled but cost > $1 (shouldn't happen)

	// Single side outcomes
	SingleSideWins   int // Only one filled, and it won
	SingleSideLosses int // Only one filled, and it lost

	// Never entered
	NeverEntered int // No side got cheap enough

	// Profit tracking
	TotalProfit     decimal.Decimal
	TotalLoss       decimal.Decimal
	GrossProfit     decimal.Decimal

	// Timing stats
	AvgTimeToFirstEntry  time.Duration
	AvgTimeToSecondEntry time.Duration
	AvgTimeBetweenFills  time.Duration

	// Price stats
	AvgFirstEntryPrice  decimal.Decimal
	AvgSecondEntryPrice decimal.Decimal
	AvgCombinedCost     decimal.Decimal
}

func NewSmartDualStrategy(scanner *polymarket.WindowScanner, clobClient *CLOBClient, paperTrade bool) *SmartDualStrategy {
	return &SmartDualStrategy{
		windowScanner:   scanner,
		clobClient:      clobClient,
		cheapThreshold:  decimal.NewFromFloat(0.38), // Enter when side drops to 38¢
		maxCombinedCost: decimal.NewFromFloat(0.92), // Max 92¢ total for 8% min profit
		positionSize:    decimal.NewFromFloat(5.0),  // $5 per side
		paperTrade:      paperTrade,
		positions:       make(map[string]*SmartDualPosition),
		stats:           &DualSideStats{},
		stopCh:          make(chan struct{}),
	}
}

// SetCheapThreshold sets the price threshold for "cheap" entry
func (s *SmartDualStrategy) SetCheapThreshold(price float64) {
	s.cheapThreshold = decimal.NewFromFloat(price)
}

// Start begins the strategy monitoring loop
func (s *SmartDualStrategy) Start() {
	mode := "LIVE"
	if s.paperTrade {
		mode = "PAPER"
	}

	log.Info().
		Str("mode", mode).
		Str("cheap_threshold", s.cheapThreshold.String()).
		Str("max_combined", s.maxCombinedCost.String()).
		Str("position_size", s.positionSize.String()).
		Msg("🎲 Smart Dual-Side Strategy started")

	go s.monitorLoop()
}

func (s *SmartDualStrategy) monitorLoop() {
	ticker := time.NewTicker(500 * time.Millisecond) // Check every 500ms for speed
	statsTicker := time.NewTicker(1 * time.Minute)   // Log stats every minute
	defer ticker.Stop()
	defer statsTicker.Stop()

	for {
		select {
		case <-ticker.C:
			s.checkWindows()
		case <-statsTicker.C:
			s.logStats()
		case <-s.stopCh:
			s.logFinalStats()
			return
		}
	}
}

func (s *SmartDualStrategy) checkWindows() {
	windows := s.windowScanner.GetActiveWindows()

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	for i := range windows {
		w := &windows[i]

		// Skip windows with no odds data
		if w.YesPrice.IsZero() && w.NoPrice.IsZero() {
			continue
		}

		// Initialize new position tracking
		if _, exists := s.positions[w.ID]; !exists {
			s.positions[w.ID] = &SmartDualPosition{
				WindowID:        w.ID,
				Question:        w.Question,
				Asset:           w.Asset,
				StartTime:       w.StartDate,
				EndTime:         w.EndDate,
				Status:          "waiting",
				UpLowestSeen:    decimal.NewFromFloat(1.0),
				DownLowestSeen:  decimal.NewFromFloat(1.0),
				UpHighestSeen:   decimal.Zero,
				DownHighestSeen: decimal.Zero,
			}
			s.stats.mu.Lock()
			s.stats.TotalWindowsTracked++
			s.stats.mu.Unlock()

			log.Debug().
				Str("window", truncate(w.Question, 50)).
				Str("up", w.YesPrice.String()).
				Str("down", w.NoPrice.String()).
				Msg("📊 [DUAL] Tracking new window")
		}

		pos := s.positions[w.ID]

		// Update price tracking
		if !w.YesPrice.IsZero() {
			if w.YesPrice.LessThan(pos.UpLowestSeen) {
				pos.UpLowestSeen = w.YesPrice
			}
			if w.YesPrice.GreaterThan(pos.UpHighestSeen) {
				pos.UpHighestSeen = w.YesPrice
			}
		}
		if !w.NoPrice.IsZero() {
			if w.NoPrice.LessThan(pos.DownLowestSeen) {
				pos.DownLowestSeen = w.NoPrice
			}
			if w.NoPrice.GreaterThan(pos.DownHighestSeen) {
				pos.DownHighestSeen = w.NoPrice
			}
		}

		// STATE MACHINE
		switch pos.Status {
		case "waiting":
			// Look for first cheap entry
			s.checkFirstEntry(pos, w)

		case "first_filled":
			// Look for reversal (second side becomes cheap)
			s.checkSecondEntry(pos, w)

		case "both_filled":
			// Both filled - just wait for resolution
			// Already guaranteed profit (unless combined > $1)
		}

		// Check for window expiry/resolution
		if now.After(pos.EndTime) && pos.Status != "resolved" && pos.Status != "expired" {
			s.handleResolution(pos, w)
		}
	}

	// Clean up old positions
	for id, pos := range s.positions {
		if now.After(pos.EndTime.Add(5 * time.Minute)) {
			delete(s.positions, id)
		}
	}
}

func (s *SmartDualStrategy) checkFirstEntry(pos *SmartDualPosition, w *polymarket.PredictionWindow) {
	// Check if UP is cheap enough
	if !w.YesPrice.IsZero() && w.YesPrice.LessThanOrEqual(s.cheapThreshold) {
		s.enterFirstSide(pos, w, "UP", w.YesPrice, w.YesTokenID)
		return
	}

	// Check if DOWN is cheap enough
	if !w.NoPrice.IsZero() && w.NoPrice.LessThanOrEqual(s.cheapThreshold) {
		s.enterFirstSide(pos, w, "DOWN", w.NoPrice, w.NoTokenID)
		return
	}
}

func (s *SmartDualStrategy) enterFirstSide(pos *SmartDualPosition, w *polymarket.PredictionWindow, side string, price decimal.Decimal, tokenID string) {
	pos.FirstSide = side
	pos.FirstPrice = price
	pos.FirstEntryTime = time.Now()
	pos.FirstFilled = true
	pos.Status = "first_filled"
	pos.TotalCost = price

	// Set up second side target
	if side == "UP" {
		pos.SecondSide = "DOWN"
		pos.SecondLowestSeen = pos.DownLowestSeen
	} else {
		pos.SecondSide = "UP"
		pos.SecondLowestSeen = pos.UpLowestSeen
	}

	s.stats.mu.Lock()
	s.stats.FirstEntries++
	s.stats.WindowsWithCheapSide++
	s.stats.mu.Unlock()

	log.Info().
		Str("window", truncate(pos.Question, 40)).
		Str("side", side).
		Str("price", price.String()).
		Str("mode", s.modeStr()).
		Msg("🎯 [DUAL] FIRST ENTRY - waiting for reversal")

	// Place actual order if not paper trading
	if !s.paperTrade && s.clobClient != nil && tokenID != "" {
		size := s.positionSize.Div(price).Round(2)
		orderID, err := s.clobClient.PlaceLimitOrder(tokenID, price, size, "BUY")
		if err != nil {
			log.Error().Err(err).Msg("Failed to place first side order")
		} else {
			log.Info().Str("order_id", orderID).Msg("📗 First side order placed")
		}
	}
}

func (s *SmartDualStrategy) checkSecondEntry(pos *SmartDualPosition, w *polymarket.PredictionWindow) {
	var secondPrice decimal.Decimal
	var tokenID string

	if pos.SecondSide == "UP" {
		secondPrice = w.YesPrice
		tokenID = w.YesTokenID
		if !w.YesPrice.IsZero() && w.YesPrice.LessThan(pos.SecondLowestSeen) {
			pos.SecondLowestSeen = w.YesPrice
		}
	} else {
		secondPrice = w.NoPrice
		tokenID = w.NoTokenID
		if !w.NoPrice.IsZero() && w.NoPrice.LessThan(pos.SecondLowestSeen) {
			pos.SecondLowestSeen = w.NoPrice
		}
	}

	// Calculate max price we can pay for second side
	maxSecondPrice := s.maxCombinedCost.Sub(pos.FirstPrice)

	// Check if second side is cheap enough AND within our budget
	if !secondPrice.IsZero() && secondPrice.LessThanOrEqual(maxSecondPrice) {
		pos.SecondPrice = secondPrice
		pos.SecondEntryTime = time.Now()
		pos.SecondFilled = true
		pos.SecondTargetHit = true
		pos.Status = "both_filled"
		pos.TotalCost = pos.FirstPrice.Add(secondPrice)

		// Guaranteed payout is $1 (one side WILL win)
		pos.Payout = decimal.NewFromInt(1)
		pos.Profit = pos.Payout.Sub(pos.TotalCost)

		s.stats.mu.Lock()
		s.stats.SecondEntries++
		s.stats.BothFilled++
		s.stats.mu.Unlock()

		profitPct := pos.Profit.Div(pos.TotalCost).Mul(decimal.NewFromInt(100))

		log.Info().
			Str("window", truncate(pos.Question, 40)).
			Str("first", fmt.Sprintf("%s@%s", pos.FirstSide, pos.FirstPrice.String())).
			Str("second", fmt.Sprintf("%s@%s", pos.SecondSide, pos.SecondPrice.String())).
			Str("total_cost", pos.TotalCost.String()).
			Str("profit", pos.Profit.String()).
			Str("profit_pct", profitPct.StringFixed(1)+"%").
			Str("mode", s.modeStr()).
			Msg("🎉🎉 [DUAL] BOTH SIDES FILLED - GUARANTEED PROFIT! 🎉🎉")

		// Place actual order if not paper trading
		if !s.paperTrade && s.clobClient != nil && tokenID != "" {
			size := s.positionSize.Div(secondPrice).Round(2)
			orderID, err := s.clobClient.PlaceLimitOrder(tokenID, secondPrice, size, "BUY")
			if err != nil {
				log.Error().Err(err).Msg("Failed to place second side order")
			} else {
				log.Info().Str("order_id", orderID).Msg("📕 Second side order placed")
			}
		}
	}
}

func (s *SmartDualStrategy) handleResolution(pos *SmartDualPosition, w *polymarket.PredictionWindow) {
	pos.Status = "resolved"

	s.stats.mu.Lock()
	defer s.stats.mu.Unlock()

	if pos.Status == "both_filled" || (pos.FirstFilled && pos.SecondFilled) {
		// Both sides filled - guaranteed win
		s.stats.BothFilledWins++
		s.stats.TotalProfit = s.stats.TotalProfit.Add(pos.Profit)
		s.stats.GrossProfit = s.stats.GrossProfit.Add(pos.Profit)

		log.Info().
			Str("window", truncate(pos.Question, 40)).
			Str("profit", pos.Profit.String()).
			Msg("✅ [DUAL] Window resolved - GUARANTEED PROFIT")

	} else if pos.FirstFilled && !pos.SecondFilled {
		// Only first side filled - 50/50 outcome
		// Determine winner based on final odds (crude approximation)
		// In reality, we'd check resolution API

		// For paper trading, estimate based on which side was cheaper at end
		var won bool
		if pos.FirstSide == "UP" {
			// We bought UP, check if UP was favored at end
			won = w.YesPrice.GreaterThan(decimal.NewFromFloat(0.5))
		} else {
			won = w.NoPrice.GreaterThan(decimal.NewFromFloat(0.5))
		}

		if won {
			profit := decimal.NewFromInt(1).Sub(pos.FirstPrice)
			s.stats.SingleSideWins++
			s.stats.TotalProfit = s.stats.TotalProfit.Add(profit)
			s.stats.GrossProfit = s.stats.GrossProfit.Add(profit)

			log.Info().
				Str("window", truncate(pos.Question, 40)).
				Str("side", pos.FirstSide).
				Str("entry", pos.FirstPrice.String()).
				Str("profit", profit.String()).
				Str("second_lowest", pos.SecondLowestSeen.String()).
				Msg("✅ [DUAL] Single side WIN (reversal didn't happen)")
		} else {
			loss := pos.FirstPrice.Neg()
			s.stats.SingleSideLosses++
			s.stats.TotalLoss = s.stats.TotalLoss.Add(pos.FirstPrice)
			s.stats.GrossProfit = s.stats.GrossProfit.Add(loss)

			log.Warn().
				Str("window", truncate(pos.Question, 40)).
				Str("side", pos.FirstSide).
				Str("entry", pos.FirstPrice.String()).
				Str("loss", pos.FirstPrice.String()).
				Str("second_lowest", pos.SecondLowestSeen.String()).
				Msg("❌ [DUAL] Single side LOSS (reversal didn't happen)")
		}

	} else {
		// Never entered - no side got cheap enough
		s.stats.NeverEntered++

		log.Debug().
			Str("window", truncate(pos.Question, 40)).
			Str("up_lowest", pos.UpLowestSeen.String()).
			Str("down_lowest", pos.DownLowestSeen.String()).
			Str("threshold", s.cheapThreshold.String()).
			Msg("⏭️ [DUAL] Never entered - no side got cheap enough")
	}
}

func (s *SmartDualStrategy) modeStr() string {
	if s.paperTrade {
		return "PAPER"
	}
	return "LIVE"
}

func (s *SmartDualStrategy) logStats() {
	s.stats.mu.RLock()
	defer s.stats.mu.RUnlock()

	if s.stats.TotalWindowsTracked == 0 {
		return
	}

	// Calculate rates
	cheapRate := float64(s.stats.WindowsWithCheapSide) / float64(s.stats.TotalWindowsTracked) * 100
	bothFilledRate := float64(s.stats.BothFilled) / float64(max(s.stats.FirstEntries, 1)) * 100
	winRate := float64(s.stats.BothFilledWins+s.stats.SingleSideWins) /
		float64(max(s.stats.BothFilledWins+s.stats.SingleSideWins+s.stats.SingleSideLosses, 1)) * 100

	log.Info().
		Int("windows_tracked", s.stats.TotalWindowsTracked).
		Int("cheap_opportunities", s.stats.WindowsWithCheapSide).
		Str("cheap_rate", fmt.Sprintf("%.1f%%", cheapRate)).
		Int("first_entries", s.stats.FirstEntries).
		Int("both_filled", s.stats.BothFilled).
		Str("both_filled_rate", fmt.Sprintf("%.1f%%", bothFilledRate)).
		Int("single_wins", s.stats.SingleSideWins).
		Int("single_losses", s.stats.SingleSideLosses).
		Str("win_rate", fmt.Sprintf("%.1f%%", winRate)).
		Str("gross_profit", s.stats.GrossProfit.StringFixed(2)).
		Msg("📊 [DUAL] STRATEGY STATS")
}

func (s *SmartDualStrategy) logFinalStats() {
	s.stats.mu.RLock()
	defer s.stats.mu.RUnlock()

	log.Info().Msg("")
	log.Info().Msg("╔══════════════════════════════════════════════════════════════╗")
	log.Info().Msg("║           SMART DUAL-SIDE STRATEGY FINAL REPORT              ║")
	log.Info().Msg("╠══════════════════════════════════════════════════════════════╣")
	log.Info().Msgf("║  Windows Tracked:      %6d                                 ║", s.stats.TotalWindowsTracked)
	log.Info().Msgf("║  Cheap Opportunities:  %6d (%.1f%%)                         ║",
		s.stats.WindowsWithCheapSide,
		float64(s.stats.WindowsWithCheapSide)/float64(max(s.stats.TotalWindowsTracked, 1))*100)
	log.Info().Msg("╠══════════════════════════════════════════════════════════════╣")
	log.Info().Msgf("║  First Entries:        %6d                                 ║", s.stats.FirstEntries)
	log.Info().Msgf("║  Both Filled:          %6d (%.1f%% reversal rate)           ║",
		s.stats.BothFilled,
		float64(s.stats.BothFilled)/float64(max(s.stats.FirstEntries, 1))*100)
	log.Info().Msgf("║  Single Side Wins:     %6d                                 ║", s.stats.SingleSideWins)
	log.Info().Msgf("║  Single Side Losses:   %6d                                 ║", s.stats.SingleSideLosses)
	log.Info().Msgf("║  Never Entered:        %6d                                 ║", s.stats.NeverEntered)
	log.Info().Msg("╠══════════════════════════════════════════════════════════════╣")

	totalTrades := s.stats.BothFilledWins + s.stats.SingleSideWins + s.stats.SingleSideLosses
	if totalTrades > 0 {
		winRate := float64(s.stats.BothFilledWins+s.stats.SingleSideWins) / float64(totalTrades) * 100
		log.Info().Msgf("║  WIN RATE:             %5.1f%%                                ║", winRate)
	}
	log.Info().Msgf("║  GROSS PROFIT:         $%s                              ║", s.stats.GrossProfit.StringFixed(2))
	log.Info().Msg("╚══════════════════════════════════════════════════════════════╝")
	log.Info().Msg("")
}

// GetStats returns current statistics
func (s *SmartDualStrategy) GetStats() *DualSideStats {
	return s.stats
}

// Stop halts the strategy
func (s *SmartDualStrategy) Stop() {
	close(s.stopCh)
}

// GetActivePositions returns currently tracked positions
func (s *SmartDualStrategy) GetActivePositions() []*SmartDualPosition {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var positions []*SmartDualPosition
	for _, pos := range s.positions {
		positions = append(positions, pos)
	}
	return positions
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
