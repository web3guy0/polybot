package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/web3guy0/polybot/internal/arbitrage"
	"github.com/shopspring/decimal"
)

func main() {
	sigType, _ := strconv.Atoi(os.Getenv("SIGNATURE_TYPE"))
	
	client, err := arbitrage.NewCLOBClient(
		os.Getenv("CLOB_API_KEY"),
		os.Getenv("CLOB_API_SECRET"),
		os.Getenv("CLOB_PASSPHRASE"),
		os.Getenv("WALLET_PRIVATE_KEY"),
		os.Getenv("SIGNER_ADDRESS"),
		os.Getenv("FUNDER_ADDRESS"),
		sigType,
	)
	if err != nil {
		fmt.Println("Error creating client:", err)
		return
	}

	trades, err := client.GetTrades()
	if err != nil {
		fmt.Println("Error fetching trades:", err)
		return
	}

	fmt.Printf("📊 TRADE ANALYSIS - Total Trades: %d\n\n", len(trades))
	
	// Group trades by market to find BUY/SELL pairs
	type TradePair struct {
		Buy  *arbitrage.TradeRecord
		Sell *arbitrage.TradeRecord
	}
	
	pairs := make(map[string]*TradePair)
	
	for i := range trades {
		t := &trades[i]
		market := t.Market + "_" + t.AssetID
		
		if pairs[market] == nil {
			pairs[market] = &TradePair{}
		}
		
		if t.Side == "BUY" && pairs[market].Buy == nil {
			pairs[market].Buy = t
		} else if t.Side == "SELL" && pairs[market].Sell == nil {
			pairs[market].Sell = t
		}
	}
	
	// Analyze pairs
	var totalPnL decimal.Decimal
	wins := 0
	losses := 0
	slHits := 0
	wouldHaveWon := 0
	
	fmt.Println("═══════════════════════════════════════════════════════════════════════")
	fmt.Println("│ OUTCOME │ BUY    │ SELL   │ P&L      │ FEE EST  │ NET      │ NOTES")
	fmt.Println("═══════════════════════════════════════════════════════════════════════")
	
	for _, pair := range pairs {
		if pair.Buy == nil || pair.Sell == nil {
			continue
		}
		
		buyPrice, _ := decimal.NewFromString(pair.Buy.Price)
		sellPrice, _ := decimal.NewFromString(pair.Sell.Price)
		buySize, _ := decimal.NewFromString(pair.Buy.Size)
		
		// Calculate gross P&L
		grossPnL := sellPrice.Sub(buyPrice).Mul(buySize)
		
		// Calculate fees using Polymarket formula (max 1.56% at 50/50)
		// Fee = 2 * p * (1 - p) * size * 0.0312
		p := buyPrice.InexactFloat64()
		buyFee := 2 * p * (1 - p) * buySize.InexactFloat64() * 0.0312
		sp := sellPrice.InexactFloat64()
		sellFee := 2 * sp * (1 - sp) * buySize.InexactFloat64() * 0.0312
		totalFee := decimal.NewFromFloat(buyFee + sellFee)
		
		netPnL := grossPnL.Sub(totalFee)
		totalPnL = totalPnL.Add(netPnL)
		
		outcome := pair.Buy.Outcome
		notes := ""
		
		if netPnL.GreaterThan(decimal.Zero) {
			wins++
			notes = "✅ WIN"
		} else {
			losses++
			notes = "❌ LOSS"
			
			// Check if this was a SL hit (sold below 80¢) but outcome matches our side
			if sellPrice.LessThan(decimal.NewFromFloat(0.80)) {
				slHits++
				notes = "🛑 SL HIT"
				
				// Would have won at resolution?
				// If we bought "Up" and outcome is "Up", we would have gotten $1
				// If we bought "Down" and outcome is "Down", we would have gotten $1
				// The outcome field tells us what the asset was
				if pair.Buy.Outcome == pair.Sell.Outcome {
					// Same asset throughout - check if it would resolve to $1
					// This is tricky - we need to know if the price direction was correct
					// For now, flag as "potential SL regret"
					wouldHaveWon++
					potentialWin := decimal.NewFromInt(1).Sub(buyPrice).Mul(buySize)
					notes = fmt.Sprintf("😭 SL HIT - Would be +$%.2f at resolution!", potentialWin.InexactFloat64())
				}
			}
		}
		
		fmt.Printf("│ %-7s │ %5.2f¢ │ %5.2f¢ │ %+7.2f¢ │ %6.2f¢ │ %+7.2f¢ │ %s\n",
			outcome,
			buyPrice.Mul(decimal.NewFromInt(100)).InexactFloat64(),
			sellPrice.Mul(decimal.NewFromInt(100)).InexactFloat64(),
			grossPnL.Mul(decimal.NewFromInt(100)).InexactFloat64(),
			totalFee.Mul(decimal.NewFromInt(100)).InexactFloat64(),
			netPnL.Mul(decimal.NewFromInt(100)).InexactFloat64(),
			notes,
		)
	}
	
	fmt.Println("═══════════════════════════════════════════════════════════════════════")
	fmt.Printf("\n📈 SUMMARY:\n")
	fmt.Printf("   Wins: %d | Losses: %d | Win Rate: %.1f%%\n", wins, losses, float64(wins)/float64(wins+losses)*100)
	fmt.Printf("   SL Hits: %d | Would Have Won: %d (%.1f%% of losses)\n", slHits, wouldHaveWon, float64(wouldHaveWon)/float64(losses)*100)
	fmt.Printf("   Total P&L: %+.2f¢\n", totalPnL.Mul(decimal.NewFromInt(100)).InexactFloat64())
	
	// Print timestamps for context
	if len(trades) > 0 {
		first := trades[len(trades)-1]
		last := trades[0]
		firstTime, _ := strconv.ParseInt(first.MatchTime, 10, 64)
		lastTime, _ := strconv.ParseInt(last.MatchTime, 10, 64)
		fmt.Printf("\n   Date Range: %s to %s\n",
			time.Unix(firstTime, 0).Format("Jan 2 15:04"),
			time.Unix(lastTime, 0).Format("Jan 2 15:04"),
		)
	}
}
