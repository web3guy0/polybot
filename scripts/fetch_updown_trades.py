#!/usr/bin/env python3
"""
Fetch Up/Down trades from whale wallets with pagination.
Filters for Up/Down markets only to reduce data size.
"""
import requests
import json
import time
import sys
from datetime import datetime

WHALE_ADDRESSES = [
    "0x63ce342161250d705dc0b16df89036c8e5f9ba9a",  # Whale 1 - 100k+ trades
    "0x818f214c7f3e479cce1d964d53fe3db7297558cb",  # Whale 2
    "0x6031b6eed1c97e853c6e0f03ad3ce3529351f96d",  # Whale 3
]

BASE_URL = "https://data-api.polymarket.com/trades"
LIMIT = 500
MAX_TRADES_PER_WHALE = 50000  # Limit to avoid too much data

def is_updown_trade(trade):
    """Check if trade is for an Up or Down market"""
    title = trade.get("title", "").lower()
    return "up or down" in title

def fetch_whale_trades(address, max_trades=MAX_TRADES_PER_WHALE):
    """Fetch all Up/Down trades for a whale address"""
    all_trades = []
    offset = 0
    total_fetched = 0
    
    print(f"\nFetching trades for {address[:10]}...")
    
    while True:
        try:
            url = f"{BASE_URL}?maker={address}&limit={LIMIT}&offset={offset}"
            response = requests.get(url, timeout=30)
            response.raise_for_status()
            trades = response.json()
            
            if not trades:
                print(f"  No more trades at offset {offset}")
                break
            
            # Filter for Up/Down trades only
            updown_trades = [t for t in trades if is_updown_trade(t)]
            all_trades.extend(updown_trades)
            
            total_fetched += len(trades)
            print(f"  Offset {offset}: {len(trades)} total, {len(updown_trades)} up/down (cumulative: {len(all_trades)} up/down)")
            
            # Stop conditions
            if len(trades) < LIMIT:
                print(f"  Reached end of trades")
                break
            
            if len(all_trades) >= max_trades:
                print(f"  Reached max trades limit ({max_trades})")
                break
            
            if total_fetched >= max_trades * 2:  # Safety limit
                print(f"  Safety limit reached")
                break
                
            offset += LIMIT
            time.sleep(0.15)  # Rate limit
            
        except Exception as e:
            print(f"  Error at offset {offset}: {e}")
            time.sleep(1)
            continue
    
    return all_trades, total_fetched

def main():
    all_whale_trades = []
    summary = {}
    
    for i, address in enumerate(WHALE_ADDRESSES, 1):
        trades, total = fetch_whale_trades(address)
        all_whale_trades.extend(trades)
        summary[f"whale{i}"] = {
            "address": address,
            "total_fetched": total,
            "updown_trades": len(trades)
        }
        print(f"Whale {i}: {len(trades)} up/down trades from {total} total")
    
    # Save all trades
    output_file = "/workspaces/polybot/data/whale_trades/whale_updown_all.json"
    with open(output_file, 'w') as f:
        json.dump(all_whale_trades, f, indent=2)
    
    # Save summary
    summary["total_updown_trades"] = len(all_whale_trades)
    summary["fetched_at"] = datetime.now().isoformat()
    
    with open("/workspaces/polybot/data/whale_trades/fetch_summary.json", 'w') as f:
        json.dump(summary, f, indent=2)
    
    print(f"\n=== COMPLETE ===")
    print(f"Total Up/Down trades: {len(all_whale_trades)}")
    print(f"Saved to: {output_file}")
    
    # Show sample distribution
    if all_whale_trades:
        entry_prices = [float(t.get("price", 0)) for t in all_whale_trades]
        avg_price = sum(entry_prices) / len(entry_prices)
        under_50 = sum(1 for p in entry_prices if p < 0.50)
        print(f"\nEntry price analysis:")
        print(f"  Average entry: {avg_price:.2f}")
        print(f"  Under 50¢: {under_50} ({under_50/len(entry_prices)*100:.1f}%)")

if __name__ == "__main__":
    main()
