# Polybot

High-frequency trading bot for Polymarket 15-minute crypto prediction markets.

## Strategy

**Sniper** - Last-minute confirmation trading

```
┌─────────────────────────────────────────────────────────────────┐
│                         THE MATH                                │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  Markets/day:     96 (15-min windows × 24h × 3 assets)         │
│  Target trades:   10+ favorable entries                         │
│  Win rate:        85% (confirmed direction)                     │
│                                                                 │
│  Entry:           88-93¢                                        │
│  Take Profit:     99¢ (+6-11¢)                                  │
│  Stop Loss:       70¢ (-18-23¢)                                 │
│                                                                 │
│  Per trade ($5):                                                │
│    Win:  0.85 × $0.45 = +$0.38                                  │
│    Loss: 0.15 × $1.00 = -$0.15                                  │
│    Net:  +$0.23/trade                                           │
│                                                                 │
│  Daily (10 trades): +$2.30                                      │
│  Monthly:           +$69                                        │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### Edge

1. **Time filter**: Only trade in last 15-60 seconds
2. **Price confirmation**: Require 0.1%+ move from target
3. **Speed**: 100ms scan interval = first to see opportunity
4. **Momentum**: Confirm direction isn't reversing

At 30 seconds remaining with 0.1%+ price move, outcome is ~90% locked.
We buy confirmed winners before price hits 99¢.

## Quick Start

```bash
# Configure
cp .env.example .env
# Edit .env with your credentials

# Run
go run ./cmd/main.go
```

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `MIN_TIME_SEC` | 15 | Min seconds before window close |
| `MAX_TIME_SEC` | 60 | Max seconds before window close |
| `MIN_ODDS` | 0.88 | Min entry price |
| `MAX_ODDS` | 0.93 | Max entry price |
| `TAKE_PROFIT` | 0.99 | Exit on profit |
| `STOP_LOSS` | 0.70 | Exit on loss |
| `SCAN_INTERVAL_MS` | 100 | Detection speed |
| `BTC_MIN_MOVE` | 0.10 | Min % move for BTC |
| `ETH_MIN_MOVE` | 0.10 | Min % move for ETH |
| `SOL_MIN_MOVE` | 0.15 | Min % move for SOL |

## Architecture

```
polybot/
├── cmd/main.go           # Entry point
├── bot/telegram.go       # Notifications
├── core/
│   ├── engine.go         # Trading engine
│   └── router.go         # Signal routing
├── feeds/
│   ├── binance.go        # Price feed (100ms)
│   ├── polymarket_ws.go  # Odds feed
│   └── window_scanner.go # Market discovery
├── strategy/
│   └── sniper.go         # Main strategy
├── risk/
│   ├── manager.go        # Risk validation
│   └── sizing.go         # Position sizing
├── exec/client.go        # Order execution
└── storage/database.go   # Trade history
```

## Flow

```
Binance (100ms) ─┐
                 ├─→ Sniper ─→ Signal ─→ Risk ─→ Execution
Window Scanner ──┘
```

## Telegram Commands

| Command | Description |
|---------|-------------|
| `/status` | Bot status |
| `/stats` | Win rate, P&L |
| `/pause` | Pause trading |
| `/resume` | Resume trading |

## Requirements

- Go 1.21+
- PostgreSQL (optional, for persistence)
- Polymarket API credentials
- Telegram bot token (optional)

## License

MIT
