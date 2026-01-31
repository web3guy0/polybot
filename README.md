# Polybot v8.0 PRO

Professional high-frequency trading bot for Polymarket 15-minute crypto prediction markets.

## Strategy

**Phase Scalper** — Fade overreactions during volatile phases of 15-minute windows

```
┌─────────────────────────────────────────────────────────────────────┐
│                    PHASE-BASED FADE SCALPING                        │
├─────────────────────────────────────────────────────────────────────┤
│                                                                     │
│  ┌──────────────────────────────────────────────────────────┐      │
│  │  MARKET PHASES (15-minute window lifecycle)              │      │
│  ├──────────────────────────────────────────────────────────┤      │
│  │  🟢 OPENING   │ 0-3 min   │ Fade ≥6¢ moves              │      │
│  │  🔴 DEAD ZONE │ 3-12 min  │ NO TRADING (noise zone)     │      │
│  │  🟡 CLOSING   │ 12-14 min │ Fade ≥4¢ panic moves        │      │
│  │  ⚫ FLAT      │ 14-15 min │ FORCE CLOSE all positions   │      │
│  └──────────────────────────────────────────────────────────┘      │
│                                                                     │
│  EDGE:                                                              │
│  • Opening overreactions often revert (trapped traders)             │
│  • Closing panic rarely persists (resolution < 60s)                 │
│  • Dead zone = random walk = no edge                                │
│                                                                     │
│  EXECUTION:                                                         │
│  • 50ms scan interval (fastest possible detection)                  │
│  • ≥2 consecutive ticks required (impulse filter)                   │
│  • TP: +2.5¢ | Timeout: 15s | No stop loss                         │
│                                                                     │
└─────────────────────────────────────────────────────────────────────┘
```

### Risk Management

- **Daily Loss Limit**: 3% of balance (circuit breaker trips)
- **Consecutive Losses**: 3 max (circuit breaker trips)
- **Per-Asset Losses**: 2 max (disables asset for session)
- **Cooldown**: 30s after exit before re-entry
- **Position Limit**: 25% of balance per position

## Quick Start

```bash
# Configure
cp .env.example .env
# Edit .env with your Polymarket credentials

# Build & Run
go build -o polybot ./cmd/main.go
./polybot
```

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| **Mode** |
| `PAPER_MODE` | true | Enable paper trading (no real orders) |
| `DRY_RUN` | true | Alias for paper mode |
| **Order Sizing** |
| `MARKET_ORDER_VALUE` | 1.1 | Market order size in $ |
| `LIMIT_ORDER_SHARES` | 5 | Limit order size in shares |
| **Phase Scalper** |
| `SCALPER_SCAN_MS` | 50 | Scan interval in milliseconds |
| `OPENING_FADE_THRESHOLD` | 0.06 | Min move to fade in opening (6¢) |
| `CLOSING_FADE_THRESHOLD` | 0.04 | Min move to fade in closing (4¢) |
| `TAKE_PROFIT_CENTS` | 0.025 | Take profit target (+2.5¢) |
| `MAX_TRADE_TIMEOUT_SEC` | 15 | Exit if no TP within timeout |
| **Risk Management** |
| `MAX_DAILY_LOSS_PCT` | 3 | Daily loss limit (% of balance) |
| `MAX_CONSECUTIVE_LOSSES` | 3 | Circuit breaker trigger |
| `MAX_POSITION_PCT` | 25 | Max single position (% of balance) |
| `POSITION_COOLDOWN_SEC` | 30 | Wait time after exit |
| **Execution** |
| `FILL_TIMEOUT_MS` | 500 | Order fill timeout |
| `MAX_ORDER_RETRIES` | 1 | Retry failed orders |
| `SLIPPAGE_BPS` | 50 | Max slippage (basis points) |

## Architecture

```
polybot/
├── cmd/main.go              # Entry point & orchestration
├── bot/telegram.go          # Telegram notifications & commands
├── core/
│   ├── engine.go            # Trading engine (tick processing)
│   └── router.go            # Signal routing
├── feeds/
│   ├── binance.go           # Binance price feed (100ms)
│   ├── chainlink.go         # Chainlink price feed (backup)
│   ├── polymarket_ws.go     # Polymarket WebSocket (live odds)
│   └── window_scanner.go    # Market window discovery
├── strategy/
│   ├── interface.go         # Strategy interface
│   └── phase_scalper.go     # Phase-based fade scalping
├── risk/
│   ├── gate.go              # Centralized risk approval
│   ├── manager.go           # Risk validation
│   └── sizing.go            # Position sizing
├── execution/
│   ├── adapter.go           # Execution adapter
│   ├── executor.go          # Order state machine
│   └── reconciler.go        # Position reconciliation
├── exec/client.go           # Polymarket CLOB client
└── storage/database.go      # PostgreSQL persistence
```

## Flow

```
                    ┌─────────────────────────────────────┐
                    │       POLYBOT v8.0 PRO FLOW         │
                    └─────────────────────────────────────┘
                                     │
        ┌────────────────────────────┼────────────────────────────┐
        ▼                            ▼                            ▼
   ┌─────────┐                ┌───────────┐                ┌───────────┐
   │ Binance │                │ Polymarket│                │  Window   │
   │  Feed   │                │    WS     │                │  Scanner  │
   │ (100ms) │                │  (odds)   │                │           │
   └────┬────┘                └─────┬─────┘                └─────┬─────┘
        │                           │                            │
        └──────────────────────────┬┘────────────────────────────┘
                                   ▼
                         ┌─────────────────┐
                         │  Phase Scalper  │
                         │   (50ms scan)   │
                         └────────┬────────┘
                                  │
                    ┌─────────────┴─────────────┐
                    ▼                           ▼
            ┌───────────────┐           ┌───────────────┐
            │   Risk Gate   │           │   Persister   │
            │  (approval)   │           │  (recovery)   │
            └───────┬───────┘           └───────────────┘
                    │
                    ▼
            ┌───────────────┐
            │   Executor    │
            │ (order FSM)   │
            └───────┬───────┘
                    │
                    ▼
            ┌───────────────┐
            │ CLOB Client   │
            │ (Polymarket)  │
            └───────────────┘
```

## Telegram Commands

| Command | Description |
|---------|-------------|
| `/status` | Bot status, mode, balance |
| `/balance` | Current account balance |
| `/stats` | Win rate, P&L, trade count |
| `/trades` | Last 10 trades |
| `/positions` | Open positions |
| `/pause` | Pause trading |
| `/resume` | Resume trading |
| `/ping` | Test bot connection |

## Professional Features

### ✅ Execution Layer
- Order state machine with timeout handling
- Fill monitoring with automatic retries
- Slippage protection

### ✅ Risk Gate
- Centralized risk approval for all trades
- Daily loss tracking with circuit breaker
- Per-asset loss tracking with auto-disable

### ✅ Position Persistence
- Crash recovery from database
- Graceful shutdown with position closure
- State persistence every 60 seconds

### ✅ Reconciliation
- Startup position recovery
- Risk state restoration
- Orphan position cleanup

## Requirements

- Go 1.21+
- PostgreSQL (for persistence)
- Polymarket API credentials (CLOB access)
- Telegram bot token (optional, for notifications)

## Environment Setup

Required credentials in `.env`:
```env
# Polymarket CLOB
CLOB_API_KEY=your-api-key
CLOB_API_SECRET=your-api-secret
CLOB_PASSPHRASE=your-passphrase
WALLET_PRIVATE_KEY=0x...
SIGNER_ADDRESS=0x...
FUNDER_ADDRESS=0x...
SIG_TYPE=1

# Database
DATABASE_URL=postgresql://...

# Telegram (optional)
TELEGRAM_BOT_TOKEN=...
TELEGRAM_CHAT_ID=...
```

## Why No Trades?

The Phase Scalper strategy requires **significant odds movements** to trigger trades:

- **OPENING phase**: Needs ≥6¢ move within 30 seconds
- **CLOSING phase**: Needs ≥4¢ move within 20 seconds

If the market is calm (odds staying at ~50/50), no opportunities will be detected. This is **by design** — the strategy only trades when there's a clear overreaction to fade.

**Check the logs for:**
```
📊 Opening overreaction detected  asset=BTC side=YES direction=UP magnitude=6.5¢
🔥 Closing panic detected        asset=ETH side=NO direction=DOWN magnitude=4.2¢
```

## License

MIT
