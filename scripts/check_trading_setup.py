#!/usr/bin/env python3
"""
Test Polymarket Trading Setup

Verifies all credentials are configured correctly for live trading.
"""

import os
import sys
import json
from dotenv import load_dotenv

# Load environment
load_dotenv()

def check_env(key, required=True, sensitive=False):
    value = os.getenv(key, "")
    if value:
        if sensitive:
            return f"✅ {key}: {'*' * 20}...{value[-4:]}" if len(value) > 4 else f"✅ {key}: SET"
        return f"✅ {key}: {value[:30]}..." if len(value) > 30 else f"✅ {key}: {value}"
    if required:
        return f"❌ {key}: NOT SET (REQUIRED)"
    return f"⚠️ {key}: NOT SET (optional)"

def main():
    print("=" * 60)
    print("🔍 POLYBOT TRADING SETUP CHECK")
    print("=" * 60)
    print()
    
    # Check environment variables
    print("📋 Environment Variables:")
    print(check_env("CLOB_API_KEY"))
    print(check_env("CLOB_API_SECRET", sensitive=True))
    print(check_env("CLOB_PASSPHRASE", sensitive=True))
    print(check_env("SIGNER_ADDRESS"))
    print(check_env("FUNDER_ADDRESS"))
    print(check_env("SIGNATURE_TYPE", required=False))
    print(check_env("WALLET_PRIVATE_KEY", sensitive=True))
    print()
    
    # Check if private key is missing
    private_key = os.getenv("WALLET_PRIVATE_KEY", "")
    if not private_key:
        print("⛔ CRITICAL: WALLET_PRIVATE_KEY is required for trading!")
        print()
        print("To trade on Polymarket, you need your wallet's private key.")
        print("This is the key that controls your SIGNER_ADDRESS.")
        print()
        print("How to get it:")
        print("1. If you used MetaMask to create the signer wallet:")
        print("   - Open MetaMask")
        print("   - Click the three dots next to your account")
        print("   - Account Details > Export Private Key")
        print()
        print("2. Add to .env:")
        print('   WALLET_PRIVATE_KEY=0xYourPrivateKeyHere')
        print()
        return False
    
    # Test py-clob-client
    print("🔌 Testing py-clob-client...")
    try:
        from py_clob_client.client import ClobClient
        from py_clob_client.clob_types import ApiCreds
        from py_clob_client.constants import POLYGON
        print("   ✅ py-clob-client imported successfully")
    except ImportError as e:
        print(f"   ❌ Failed to import py-clob-client: {e}")
        print("   Run: pip install py-clob-client")
        return False
    
    # Test connection
    print()
    print("🌐 Testing API Connection...")
    try:
        creds = ApiCreds(
            api_key=os.getenv("CLOB_API_KEY"),
            api_secret=os.getenv("CLOB_API_SECRET"),
            api_passphrase=os.getenv("CLOB_PASSPHRASE")
        )
        
        sig_type = int(os.getenv("SIGNATURE_TYPE", "1"))
        funder = os.getenv("FUNDER_ADDRESS", "")
        
        client = ClobClient(
            host="https://clob.polymarket.com",
            key=private_key,
            chain_id=POLYGON,
            creds=creds,
            signature_type=sig_type,
            funder=funder if funder else None
        )
        
        # Get balance
        balance = client.get_balance_allowance()
        balance_usd = float(balance.get("balance", 0)) / 1_000_000
        print(f"   ✅ Balance: ${balance_usd:.2f} USDC")
        
        if balance_usd < 1:
            print(f"   ⚠️ Low balance! Need at least $1 for trading")
        
        print()
        print("=" * 60)
        print("✅ ALL CHECKS PASSED - READY FOR TRADING!")
        print("=" * 60)
        return True
        
    except Exception as e:
        print(f"   ❌ Connection failed: {e}")
        return False

if __name__ == "__main__":
    success = main()
    sys.exit(0 if success else 1)
