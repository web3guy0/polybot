#!/bin/bash
# Whale Strategy Paper Trading Runner
# Run this on your VPS: ./run_whale_paper.sh

# Create logs directory
mkdir -p logs

# Set log file name with timestamp
LOGFILE="logs/whale_paper_$(date +%Y%m%d_%H%M%S).log"

echo "🐋 Starting Whale Strategy (Paper Trading)"
echo "📁 Log file: $LOGFILE"
echo "⏰ Started at: $(date)"
echo ""
echo "To stop: Press Ctrl+C or run: pkill -f polybot"
echo "To check logs: tail -f $LOGFILE"
echo ""
echo "═══════════════════════════════════════════════════════════════"

# Run the bot and log output
go run ./cmd/polybot --whale 2>&1 | tee "$LOGFILE"
