# 🤖 Polybot - Latency Arbitrage Bot

**Multi-Asset Crypto Prediction Arbitrage for Polymarket**

Exploits the latency between real-time crypto prices and Polymarket prediction odds to capture arbitrage opportunities.

![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)
![License](https://img.shields.io/badge/License-MIT-green)

## 🎯 Strategy

Polymarket offers prediction windows like *"Will BTC be above $90,574.14 at 9:00 AM?"*

The "Price to Beat" is set at window open using **Chainlink Data Streams**. But odds update **slower than real prices** - this is the edge.

```
Timeline:
T=0:00  Window opens, Price to Beat = $90,574
T=0:05  BTC jumps to $90,800 (+0.25%) on Binance
T=0:05  UP odds still at 50¢ (stale!)      ← BUY HERE
T=0:15  UP odds catch up to 65¢            ← PROFIT
T=1:00  Window resolves, UP wins at $1.00  ← MAX PROFIT
```

## 🚀 Features

| Feature | Description |
|---------|-------------|
| **Multi-Asset** | BTC, ETH, SOL trading |
| **Real-time Prices** | Binance WebSocket, CoinMarketCap, Chainlink |
| **Pre-scheduled Capture** | Captures "Price to Beat" at exact T=0 |
| **WebSocket Odds** | Sub-100ms Polymarket order book updates |
| **Dynamic Sizing** | 1x/2x/3x position based on price move |
| **Telegram Bot** | Alerts, manual trading, status monitoring |
| **Auto-exit** | Takes profit at 75¢ or holds to resolution |

## 📊 How It Works

### Price Sources (Parallel Snapshot)
- **CoinMarketCap** - Primary price feed (1s polling)
- **Chainlink (Polygon)** - On-chain oracle prices
- **Binance** - Real-time WebSocket trades

### Entry Conditions
```
✅ Price moved ≥0.10% from "Price to Beat"
✅ Odds in entry range (25¢-65¢)
✅ Window age < 30 seconds (fresh)
✅ Sufficient liquidity
```

### Exit Strategy
- **Target**: 75¢ (50% profit from 50¢ entry)
- **Hold**: To resolution if odds don't reach target
- **Stop Loss**: 20% drawdown protection

## 🏗️ Architecture

```
polybot/
├── cmd/polybot/main.go          # Entrypoint
├── internal/
│   ├── arbitrage/
│   │   ├── engine.go            # Core arbitrage engine
│   │   ├── clob.go              # Polymarket CLOB trading
│   │   ├── odds.go              # Odds fetching
│   │   └── eip712.go            # Order signing
│   ├── binance/
│   │   ├── client.go            # BTC WebSocket
│   │   └── multi_client.go      # Multi-asset WebSocket
│   ├── chainlink/
│   │   ├── client.go            # Single-asset oracle
│   │   └── multi_client.go      # Multi-asset oracles
│   ├── cmc/
│   │   └── client.go            # CoinMarketCap API
│   ├── polymarket/
│   │   ├── client.go            # REST API
│   │   ├── window_scanner.go    # Market discovery
│   │   └── ws_client.go         # WebSocket odds
│   ├── bot/
│   │   └── arb_bot.go           # Telegram interface
│   ├── config/
│   │   └── config.go            # Environment config
│   └── database/
│       └── database.go          # PostgreSQL trades
└── deploy.sh                    # VPS deployment script
```

## ⚙️ Configuration

```bash
# .env file
# Polymarket API (derive from wallet)
POLYMARKET_API_KEY=your_api_key
POLYMARKET_API_SECRET=your_api_secret
POLYMARKET_API_PASSPHRASE=your_passphrase

# Wallet
WALLET_PRIVATE_KEY=your_private_key
SIGNER_ADDRESS=0x...
FUNDER_ADDRESS=0x...

# Telegram
TELEGRAM_BOT_TOKEN=your_bot_token
TELEGRAM_ALLOWED_USERS=your_user_id

# CoinMarketCap
CMC_API_KEY=your_cmc_key

# Database
DATABASE_URL=postgres://user:pass@host/db

# Trading Parameters
POSITION_SIZE=1              # USDC per trade
MIN_MOVE_PCT=0.10            # Min price move (0.10%)
ENTRY_MIN=0.25               # Min odds to buy
ENTRY_MAX=0.65               # Max odds to buy
EXIT_TARGET=0.75             # Take profit target
DRY_RUN=false                # Paper trading mode
```

## 🚀 Quick Start

### Local Development
```bash
# Clone and build
git clone https://github.com/web3guy0/polybot.git
cd polybot
go build -o polybot ./cmd/polybot

# Configure
cp .env.example .env
nano .env  # Fill in your credentials

# Run
./polybot
```

### VPS Deployment
```bash
# Deploy to VPS
chmod +x deploy.sh
./deploy.sh your-vps-ip root

# On VPS
cd /opt/polybot
cp config.env.template config.env
nano config.env
systemctl start polybot
journalctl -u polybot -f
```

## 📱 Telegram Commands

| Command | Description |
|---------|-------------|
| `/status` | Current positions and P&L |
| `/balance` | USDC balance |
| `/windows` | Active prediction windows |
| `/buy <token> <size>` | Manual buy order |
| `/sell <token> <size>` | Manual sell order |
| `/trades` | Recent trade history |
| `/help` | All commands |

## 📈 Performance Metrics

The bot logs:
- Price to Beat accuracy
- Entry timing (window age)
- Fill rates
- P&L per trade
- Win rate by asset

## ⚠️ Risks

- **Execution Risk**: Orders may not fill at expected price
- **Price Risk**: Price moves against position after entry
- **Oracle Risk**: Chainlink Data Streams differ from on-chain
- **Liquidity Risk**: Thin order books on low-volume markets

## 📄 License

MIT License - See [LICENSE](LICENSE) for details.

---

**Disclaimer**: This is experimental trading software. Use at your own risk. Past performance does not guarantee future results.
