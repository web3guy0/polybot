# 🤖 Polybot

**Crypto Prediction Trading Bot for Polymarket** - A Go application that uses technical analysis to predict cryptocurrency price movements and trade Polymarket prediction windows.

![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)
![License](https://img.shields.io/badge/License-MIT-green)

## 🚀 Features

- **6 Technical Indicators** - RSI, Momentum, Volume, Order Book, Funding Rate, Buy/Sell Ratio
- **Real-time Signals** - Generates UP/DOWN/NO_TRADE predictions with confidence scores
- **Risk Management** - Position sizing, daily limits, one-position-per-window enforcement
- **Telegram Bot** - Interactive alerts and manual/auto trading
- **Config-driven** - Change trading asset via environment variable
- **Clean Architecture** - Strategy → Risk → Trade pipeline

## 📊 How It Works

Polymarket offers prediction windows like "Will BTC go up in the next 15 minutes?"

This bot:
1. **Analyzes** real-time market data from Binance
2. **Generates** directional signals using 6 technical indicators
3. **Validates** signals through risk management (confidence, daily limits, etc.)
4. **Executes** trades on Polymarket (manual or automatic)

### Signal Generation

| Indicator | Weight | What It Measures |
|-----------|--------|------------------|
| RSI | 20% | Overbought/oversold conditions |
| Momentum | 25% | Price trend strength |
| Volume | 15% | Trading activity relative to average |
| Order Book | 20% | Buy/sell pressure imbalance |
| Funding Rate | 10% | Market sentiment (longs vs shorts) |
| Buy/Sell Ratio | 10% | Taker buy vs sell activity |

**Signal Strength:**
- Score > 70 = STRONG (trade signal)
- Score 40-70 = MODERATE  
- Score < 40 = WEAK (no trade)

## 🏗️ Architecture

```
polybot/
├── cmd/polybot/main.go       # Application entrypoint
├── internal/
│   ├── strategy/             # Trading strategies
│   │   ├── strategy.go       # Strategy interface & Signal types
│   │   └── crypto_15m.go     # 15-minute crypto strategy
│   ├── risk/                 # Risk management
│   │   └── manager.go        # Position sizing, daily limits
│   ├── markets/              # Market orchestration
│   │   └── manager.go        # Config-driven market handling
│   ├── predictor/            # Signal generation (READ-ONLY)
│   │   └── predictor.go      # Technical indicator analysis
│   ├── indicators/           # Technical indicators
│   │   └── indicators.go     # RSI, Momentum, etc.
│   ├── trading/              # Trade execution
│   │   ├── engine.go         # Order execution
│   │   └── btc_trader.go     # Polymarket trading
│   ├── datafeed/             # Data sources
│   │   └── binance.go        # Binance WebSocket feed
│   ├── binance/              # Binance client
│   ├── polymarket/           # Polymarket integration
│   │   ├── client.go         # API client
│   │   └── btc_scanner.go    # Window scanner
│   ├── bot/                  # Telegram bot
│   │   └── telegram.go       # Commands & alerts
│   ├── config/               # Configuration
│   └── database/             # SQLite persistence
├── .env.example              # Configuration template
└── README.md
```

## 🛠️ Setup

### Prerequisites

- Go 1.21+
- Telegram Bot Token (from [@BotFather](https://t.me/BotFather))
- Your Telegram Chat ID (from [@userinfobot](https://t.me/userinfobot))

### Installation

```bash
# Clone repository
git clone https://github.com/web3guy0/polybot.git
cd polybot

# Copy environment file
cp .env.example .env

# Edit .env with your values
nano .env

# Install dependencies
go mod tidy

# Build
go build -o polybot ./cmd/polybot

# Run
./polybot
```

## 📱 Telegram Commands

| Command | Description |
|---------|-------------|
| `/start` | Initialize bot & subscribe to alerts |
| `/help` | Show all commands |
| `/signal` | Get current prediction signal |
| `/windows` | View active prediction windows |
| `/status` | Bot & market status |
| `/trade UP/DOWN` | Execute manual trade |
| `/autotrade on/off` | Toggle automatic trading |
| `/stats` | Trading statistics |
| `/settings` | View/change settings |
| `/subscribe` | Enable signal alerts |
| `/unsubscribe` | Disable signal alerts |

## ⚙️ Configuration

### Core Settings

| Variable | Default | Description |
|----------|---------|-------------|
| `TRADING_ASSET` | `BTC` | Asset to trade (BTC, ETH, SOL) |
| `BTC_ENABLED` | `true` | Enable prediction system |
| `BTC_AUTO_TRADE` | `false` | Enable automatic trading |
| `BTC_ALERT_ONLY` | `true` | Only send alerts, don't trade |

### Risk Management

| Variable | Default | Description |
|----------|---------|-------------|
| `BANKROLL` | `100` | Total trading bankroll |
| `RISK_MAX_BET_SIZE` | `10` | Maximum bet per trade |
| `RISK_MAX_DAILY_LOSS` | `50` | Stop trading after this loss |
| `RISK_MAX_DAILY_TRADES` | `20` | Maximum trades per day |
| `RISK_MIN_CONFIDENCE` | `0.60` | Minimum signal confidence |
| `RISK_CHOP_FILTER` | `true` | Skip weak/choppy signals |

### Signal Settings

| Variable | Default | Description |
|----------|---------|-------------|
| `BTC_MIN_SIGNAL_SCORE` | `25` | Minimum score for trade |
| `BTC_MIN_CONFIDENCE` | `25` | Minimum confidence % |
| `BTC_MIN_ODDS` | `0.35` | Minimum acceptable odds |
| `BTC_MAX_ODDS` | `0.65` | Maximum acceptable odds |

## 🔧 Development

```bash
# Run tests
go test ./...

# Run with debug logging
DEBUG=true ./polybot

# Build for production
CGO_ENABLED=1 go build -ldflags="-s -w" -o polybot ./cmd/polybot
```

## 📈 Signal Flow

```
┌─────────────┐    ┌─────────────┐    ┌─────────────┐    ┌─────────────┐
│   Binance   │───▶│  Strategy   │───▶│    Risk     │───▶│   Trade     │
│  WebSocket  │    │  Evaluate   │    │   Manager   │    │  Execute    │
└─────────────┘    └─────────────┘    └─────────────┘    └─────────────┘
     Data              Signal          Validation         Execution
                    (UP/DOWN/NO)      (Size/Limits)      (Polymarket)
```

## ⚠️ Disclaimer

This software is for educational purposes only. Cryptocurrency and prediction market trading involves substantial risk. Use at your own risk. The authors are not responsible for any financial losses.

## 📄 License

MIT License - feel free to use and modify.

---

Built with 💜 by [@web3guy0](https://github.com/web3guy0)
