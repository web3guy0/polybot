#!/usr/bin/env python3
"""
Place Order via Polymarket CLOB
Usage: python place_order.py <token_id> <side> <size> <price>
  token_id: The conditional token ID
  side: BUY or SELL
  size: Number of shares
  price: Price per share (0.01 to 0.99)

Returns JSON: {"success": true, "order_id": "...", "status": "..."} or {"success": false, "error": "..."}
"""

import os
import sys
import json
from dotenv import load_dotenv

# Load .env from parent directory
load_dotenv(os.path.join(os.path.dirname(__file__), '..', '.env'))

try:
    from py_clob_client.client import ClobClient
    from py_clob_client.clob_types import ApiCreds, OrderArgs, MarketOrderArgs, OrderType
    from py_clob_client.order_builder.constants import BUY, SELL
    from py_clob_client.constants import POLYGON
except ImportError:
    print(json.dumps({"success": False, "error": "py-clob-client not installed. Run: pip install py-clob-client"}))
    sys.exit(1)

def main():
    if len(sys.argv) < 5:
        print(json.dumps({"success": False, "error": "Usage: place_order.py <token_id> <side> <size> <price>"}))
        sys.exit(1)
    
    token_id = sys.argv[1]
    side_str = sys.argv[2].upper()
    size = float(sys.argv[3])
    price = float(sys.argv[4])
    
    # Get credentials from environment
    host = os.getenv("POLYMARKET_CLOB_URL", "https://clob.polymarket.com")
    private_key = os.getenv("WALLET_PRIVATE_KEY", "")
    
    api_key = os.getenv("CLOB_API_KEY", "")
    api_secret = os.getenv("CLOB_API_SECRET", "")
    passphrase = os.getenv("CLOB_PASSPHRASE", "")
    
    funder = os.getenv("FUNDER_ADDRESS", "")
    
    if not private_key:
        print(json.dumps({"success": False, "error": "WALLET_PRIVATE_KEY not set in .env"}))
        sys.exit(1)
    
    if not api_key or not api_secret:
        print(json.dumps({"success": False, "error": "CLOB_API_KEY/SECRET not set in .env"}))
        sys.exit(1)
    
    try:
        # Create credentials object
        creds = ApiCreds(
            api_key=api_key,
            api_secret=api_secret,
            api_passphrase=passphrase
        )
        
        # Initialize client with private key for order signing
        # signature_type: 0=EOA, 1=POLY_PROXY (email login), 2=POLY_GNOSIS_SAFE
        sig_type = int(os.getenv("SIGNATURE_TYPE", "1"))
        
        client = ClobClient(
            host=host,
            key=private_key,
            chain_id=POLYGON,
            creds=creds,
            signature_type=sig_type,
            funder=funder if funder else None
        )
        
        # Determine side
        side = BUY if side_str == "BUY" else SELL
        
        # Create order
        order_args = OrderArgs(
            price=price,
            size=size,
            side=side,
            token_id=token_id
        )
        
        # Create and sign the order
        signed_order = client.create_order(order_args)
        
        # Post order with FOK (Fill or Kill) for market-like execution
        resp = client.post_order(signed_order, orderType=OrderType.FOK)
        
        if resp and isinstance(resp, dict):
            if resp.get("errorMsg"):
                print(json.dumps({
                    "success": False,
                    "error": resp.get("errorMsg", "Unknown error"),
                    "error_code": resp.get("errorCode", "")
                }))
            else:
                print(json.dumps({
                    "success": True,
                    "order_id": resp.get("orderID", ""),
                    "status": resp.get("status", ""),
                    "response": resp
                }))
        else:
            print(json.dumps({
                "success": True if resp else False,
                "response": str(resp)
            }))
            
    except Exception as e:
        print(json.dumps({"success": False, "error": str(e)}))
        sys.exit(1)

if __name__ == "__main__":
    main()
