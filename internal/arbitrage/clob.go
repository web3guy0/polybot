// Package arbitrage provides latency arbitrage functionality
//
// clob.go - Polymarket CLOB trading client
// Handles order placement, cancellation, and position management.
// Requires API key and wallet private key for signing.
//
// Reference: https://docs.polymarket.com/
// Python client: https://github.com/Polymarket/py-clob-client
package arbitrage

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// OrderSide represents buy or sell
type OrderSide string

const (
	OrderSideBuy  OrderSide = "BUY"
	OrderSideSell OrderSide = "SELL"
)

// OrderType represents the order type
type OrderType string

const (
	OrderTypeGTC OrderType = "GTC" // Good til cancelled
	OrderTypeFOK OrderType = "FOK" // Fill or kill
	OrderTypeGTD OrderType = "GTD" // Good til date
)

// Order represents a CLOB order
type Order struct {
	TokenID    string          `json:"tokenID"`
	Price      decimal.Decimal `json:"price"`
	Size       decimal.Decimal `json:"size"`
	Side       OrderSide       `json:"side"`
	OrderType  OrderType       `json:"type"`
	Expiration int64           `json:"expiration,omitempty"`
}

// OrderResponse from CLOB API
type OrderResponse struct {
	OrderID   string `json:"orderID"`
	Status    string `json:"status"`
	TxHash    string `json:"transactionHash,omitempty"`
	ErrorCode string `json:"errorCode,omitempty"`
	Message   string `json:"message,omitempty"`
}

// Position represents an open position
type Position struct {
	TokenID  string          `json:"asset"`
	Size     decimal.Decimal `json:"size"`
	AvgPrice decimal.Decimal `json:"avgPrice"`
	Side     OrderSide       `json:"side"`
}

// BalanceAllowance represents the balance/allowance response
type BalanceAllowance struct {
	Balance   string `json:"balance"`
	Allowance string `json:"allowance"`
}

// TradeRecord represents a trade from history
type TradeRecord struct {
	ID              string `json:"id"`
	TakerOrderID    string `json:"taker_order_id"`
	Market          string `json:"market"`
	AssetID         string `json:"asset_id"`
	Side            string `json:"side"`
	Size            string `json:"size"`
	FeeRateBps      string `json:"fee_rate_bps"`
	Price           string `json:"price"`
	Status          string `json:"status"`
	MatchTime       string `json:"match_time"`
	LastUpdate      string `json:"last_update"`
	Outcome         string `json:"outcome"`
	BucketIndex     int    `json:"bucket_index"`
	Owner           string `json:"owner"`
	MakerAddress    string `json:"maker_address"`
	TransactionHash string `json:"transaction_hash"`
	TraderSide      string `json:"trader_side"`
	Type            string `json:"type"`
}

// CLOBClient handles trading on Polymarket CLOB
type CLOBClient struct {
	baseURL       string
	apiKey        string
	apiSecret     string
	passphrase    string
	privateKey    *ecdsa.PrivateKey
	address       common.Address // Signing address
	funderAddress common.Address // Address that holds funds (may differ for proxy wallets)
	httpClient    *http.Client
	signatureType int // 0=EOA, 1=Magic/Email, 2=Proxy
}

// ApiCreds represents derived API credentials
type ApiCreds struct {
	ApiKey     string `json:"apiKey"`
	Secret     string `json:"secret"`
	Passphrase string `json:"passphrase"`
}

// NewCLOBClient creates a new CLOB trading client
// If apiKey/apiSecret are empty but walletPrivateKey is provided, credentials will be derived
func NewCLOBClient(apiKey, apiSecret, passphrase, walletPrivateKey, signerAddress, funderAddress string, signatureType int) (*CLOBClient, error) {
	client := &CLOBClient{
		baseURL:       "https://clob.polymarket.com",
		apiKey:        apiKey,
		apiSecret:     apiSecret,
		passphrase:    passphrase,
		httpClient:    &http.Client{Timeout: 2 * time.Second}, // ⚡ FAST timeout - 2s max
		signatureType: signatureType,
	}

	// Set signer address if provided (needed for API auth headers)
	if signerAddress != "" {
		client.address = common.HexToAddress(signerAddress)
	}

	// Set funder address if provided
	if funderAddress != "" {
		client.funderAddress = common.HexToAddress(funderAddress)
	}

	// Parse wallet private key if provided
	if walletPrivateKey != "" {
		// Remove 0x prefix if present
		if len(walletPrivateKey) > 2 && walletPrivateKey[:2] == "0x" {
			walletPrivateKey = walletPrivateKey[2:]
		}
		pk, err := crypto.HexToECDSA(walletPrivateKey)
		if err != nil {
			return nil, fmt.Errorf("invalid private key: %w", err)
		}
		client.privateKey = pk
		client.address = crypto.PubkeyToAddress(pk.PublicKey)
		
		// Set funder address (defaults to signing address if not specified)
		if funderAddress != "" {
			log.Info().
				Str("signer", client.address.Hex()).
				Str("funder", client.funderAddress.Hex()).
				Int("sig_type", signatureType).
				Msg("Wallet loaded (proxy mode)")
		} else {
			client.funderAddress = client.address
			log.Info().Str("address", client.address.Hex()).Msg("Wallet loaded")
		}

		// If no API credentials provided, derive them
		if apiKey == "" || apiSecret == "" {
			log.Info().Msg("Deriving API credentials from wallet...")
			creds, err := client.deriveApiCreds()
			if err != nil {
				return nil, fmt.Errorf("failed to derive API credentials: %w", err)
			}
			client.apiKey = creds.ApiKey
			client.apiSecret = creds.Secret
			client.passphrase = creds.Passphrase
			log.Info().Str("api_key", creds.ApiKey[:20]+"...").Msg("API credentials derived successfully")
		}
	} else if apiKey != "" && apiSecret != "" {
		// No private key but we have pre-derived credentials
		log.Info().
			Str("signer", signerAddress).
			Str("funder", funderAddress).
			Msg("CLOB client with pre-derived credentials")
	}

	if client.apiKey == "" || client.apiSecret == "" {
		return nil, fmt.Errorf("wallet private key required (add WALLET_PRIVATE_KEY to .env)")
	}

	if client.address == (common.Address{}) {
		return nil, fmt.Errorf("signer address required (add SIGNER_ADDRESS to .env)")
	}

	return client, nil
}

// GetBalance returns USDC balance from Polymarket
func (c *CLOBClient) GetBalance() (decimal.Decimal, error) {
	// Endpoint: /balance-allowance?asset_type=COLLATERAL&signature_type=0
	endpoint := "/balance-allowance"
	sigType := c.signatureType
	
	url := fmt.Sprintf("%s%s?asset_type=COLLATERAL&signature_type=%d", c.baseURL, endpoint, sigType)
	
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return decimal.Zero, err
	}

	c.signL2Request(req, "GET", endpoint, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return decimal.Zero, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return decimal.Zero, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var result BalanceAllowance
	if err := json.Unmarshal(body, &result); err != nil {
		return decimal.Zero, fmt.Errorf("parse error: %w, body: %s", err, string(body))
	}

	balance, err := decimal.NewFromString(result.Balance)
	if err != nil {
		return decimal.Zero, fmt.Errorf("invalid balance value: %s", result.Balance)
	}

	// Balance is in wei (6 decimals for USDC)
	return balance.Div(decimal.NewFromInt(1000000)), nil
}

// GetTrades returns trade history
func (c *CLOBClient) GetTrades() ([]TradeRecord, error) {
	endpoint := "/data/trades"
	url := fmt.Sprintf("%s%s", c.baseURL, endpoint)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	c.signL2Request(req, "GET", endpoint, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	// Response format: {"data": [...], "next_cursor": "..."}
	var result struct {
		Data       []TradeRecord `json:"data"`
		NextCursor string        `json:"next_cursor"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}

	return result.Data, nil
}

// GetPositions returns current positions (open orders)
func (c *CLOBClient) GetPositions() ([]Position, error) {
	// Polymarket doesn't have a direct positions endpoint
	// We can get open orders instead
	endpoint := "/data/orders"
	url := fmt.Sprintf("%s%s", c.baseURL, endpoint)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	c.signL2Request(req, "GET", endpoint, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	// Response format: {"data": [...], "next_cursor": "..."}
	var result struct {
		Data []struct {
			Asset    string `json:"asset_id"`
			Side     string `json:"side"`
			Size     string `json:"original_size"`
			Price    string `json:"price"`
			Status   string `json:"status"`
			OrderID  string `json:"id"`
			SizeFill string `json:"size_matched"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}

	positions := make([]Position, 0)
	for _, order := range result.Data {
		size, _ := decimal.NewFromString(order.Size)
		price, _ := decimal.NewFromString(order.Price)
		
		side := OrderSideBuy
		if order.Side == "SELL" {
			side = OrderSideSell
		}
		
		positions = append(positions, Position{
			TokenID:  order.Asset,
			Size:     size,
			AvgPrice: price,
			Side:     side,
		})
	}

	return positions, nil
}

// PlaceOrder places a limit order on the CLOB
func (c *CLOBClient) PlaceOrder(order Order) (*OrderResponse, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("API key required")
	}

	// Build order payload
	payload := map[string]interface{}{
		"tokenID":    order.TokenID,
		"price":      order.Price.String(),
		"size":       order.Size.String(),
		"side":       order.Side,
		"type":       order.OrderType,
		"feeRateBps": "1000",
	}

	if order.Expiration > 0 {
		payload["expiration"] = order.Expiration
	}

	body, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", c.baseURL+"/order", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	c.signL2Request(req, "POST", "/order", body)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	var orderResp OrderResponse
	if err := json.Unmarshal(respBody, &orderResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w, body: %s", err, string(respBody))
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return &orderResp, fmt.Errorf("order failed: %s - %s", orderResp.ErrorCode, orderResp.Message)
	}

	return &orderResp, nil
}

// PlaceMarketBuy places a market buy order using native Go EIP-712 signing
func (c *CLOBClient) PlaceMarketBuy(tokenID string, size decimal.Decimal) (*OrderResponse, error) {
	return c.placeOrder(tokenID, SideBuy, size, decimal.Zero) // Zero = fetch price
}

// PlaceMarketSell places a market sell order using native Go EIP-712 signing
func (c *CLOBClient) PlaceMarketSell(tokenID string, size decimal.Decimal) (*OrderResponse, error) {
	return c.placeOrder(tokenID, SideSell, size, decimal.Zero) // Zero = fetch price
}

// PlaceMarketBuyAtPrice places a market buy WITHOUT fetching price again (FAST!)
func (c *CLOBClient) PlaceMarketBuyAtPrice(tokenID string, size decimal.Decimal, marketPrice decimal.Decimal) (*OrderResponse, error) {
	return c.placeOrder(tokenID, SideBuy, size, marketPrice)
}

// PlaceMarketSellAtPrice places a market sell WITHOUT fetching price again (FAST!)
func (c *CLOBClient) PlaceMarketSellAtPrice(tokenID string, size decimal.Decimal, marketPrice decimal.Decimal) (*OrderResponse, error) {
	return c.placeOrder(tokenID, SideSell, size, marketPrice)
}

// placeOrder places an order using native Go EIP-712 signing (fast!)
// If marketPrice is provided (non-zero), skip the odds fetch for speed
func (c *CLOBClient) placeOrder(tokenID string, side int, size decimal.Decimal, marketPrice decimal.Decimal) (*OrderResponse, error) {
	start := time.Now()

	var price decimal.Decimal
	
	// ⚡ SPEED: If price already provided, skip the API call
	if !marketPrice.IsZero() {
		if side == SideBuy {
			// Add 3% slippage to ensure fill
			slippage := decimal.NewFromFloat(0.03)
			price = marketPrice.Add(slippage)
			if price.GreaterThan(decimal.NewFromFloat(0.99)) {
				price = decimal.NewFromFloat(0.99)
			}
		} else {
			// Subtract 3% for sells
			slippage := decimal.NewFromFloat(0.03)
			price = marketPrice.Sub(slippage)
			if price.LessThan(decimal.NewFromFloat(0.01)) {
				price = decimal.NewFromFloat(0.01)
			}
		}
	} else {
		// Fetch current best price (slower)
		fetcher := NewOddsFetcher()
		odds, err := fetcher.GetLiveOdds(tokenID)
		if err != nil {
			return nil, fmt.Errorf("failed to get odds: %w", err)
		}

		if side == SideBuy {
			slippage := decimal.NewFromFloat(0.03)
			price = odds.BestAsk.Add(slippage)
			if price.GreaterThan(decimal.NewFromFloat(0.99)) {
				price = decimal.NewFromFloat(0.99)
			}
		} else {
			slippage := decimal.NewFromFloat(0.03)
			price = odds.BestBid.Sub(slippage)
			if price.LessThan(decimal.NewFromFloat(0.01)) {
				price = decimal.NewFromFloat(0.01)
			}
		}
	}

	// Create order signer
	signer := NewOrderSigner(c.privateKey, c.address, c.funderAddress, c.signatureType)

	// Create and sign order
	signedOrder, err := signer.CreateSignedOrder(tokenID, side, price, size)
	if err != nil {
		return nil, fmt.Errorf("failed to sign order: %w", err)
	}

	// Log timing
	signTime := time.Since(start)

	sideStr := "BUY"
	if side == SideSell {
		sideStr = "SELL"
	}

	log.Info().
		Str("token", tokenID[:20]+"...").
		Str("side", sideStr).
		Str("size", size.String()).
		Str("price", price.String()).
		Dur("sign_time_ms", signTime).
		Msg("⚡ Order signed (native Go)")

	// Submit order to CLOB
	return c.submitSignedOrder(signedOrder)
}

// submitSignedOrder posts a signed order to the CLOB API (defaults to FOK)
func (c *CLOBClient) submitSignedOrder(signedOrder *SignedCTFOrder) (*OrderResponse, error) {
	return c.submitSignedOrderWithType(signedOrder, "FOK")
}

// submitSignedOrderWithType posts a signed order with a specific order type
func (c *CLOBClient) submitSignedOrderWithType(signedOrder *SignedCTFOrder, orderType string) (*OrderResponse, error) {
	start := time.Now()

	// Build request payload - owner must be API key!
	payload := signedOrder.ToAPIPayloadWithType(c.apiKey, orderType)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal order: %w", err)
	}

	// DEBUG: Log the full payload being sent
	log.Debug().
		RawJSON("request_payload", body).
		Msg("📤 Sending order to CLOB API")

	// Create request
	req, err := http.NewRequest("POST", c.baseURL+"/order", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// Add L2 auth headers
	c.signL2Request(req, "POST", "/order", body)

	// Execute request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	log.Debug().
		Int("status", resp.StatusCode).
		Dur("api_time_ms", time.Since(start)).
		RawJSON("response", respBody).
		Msg("CLOB API response")

	var orderResp OrderResponse
	if err := json.Unmarshal(respBody, &orderResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w, body: %s", err, string(respBody))
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return &orderResp, fmt.Errorf("order failed: %s - %s", orderResp.ErrorCode, orderResp.Message)
	}

	log.Info().
		Str("order_id", orderResp.OrderID).
		Str("status", orderResp.Status).
		Msg("✅ Order submitted successfully")

	return &orderResp, nil
}

// PlaceLimitOrder places a limit order with aggressive pricing for fast fills
// Uses FOK (Fill Or Kill) - order must fill immediately or is cancelled
// Returns the order ID for tracking
func (c *CLOBClient) PlaceLimitOrder(tokenID string, price, size decimal.Decimal, side string) (string, error) {
	sideInt := SideBuy
	if side == "SELL" {
		sideInt = SideSell
	}

	// Round price to valid tick size (0.01 for all prices on Polymarket)
	tickSize := decimal.NewFromFloat(0.01)
	
	// Add slippage for faster fills:
	// BUY: Pay slightly MORE to be at front of queue
	// SELL: Accept slightly LESS to fill faster
	slippage := decimal.NewFromFloat(0.02) // 2 cents slippage
	
	if sideInt == SideBuy {
		// Round DOWN base price, then ADD slippage
		price = price.Div(tickSize).Floor().Mul(tickSize)
		price = price.Add(slippage) // Pay 2¢ more to fill faster
	} else {
		// Round UP base price, then SUBTRACT slippage  
		price = price.Div(tickSize).Ceil().Mul(tickSize)
		price = price.Sub(slippage) // Accept 2¢ less to fill faster
		if price.LessThan(tickSize) {
			price = tickSize // Minimum 1¢
		}
	}

	// Ensure minimum size of 5 shares (Polymarket requirement)
	minSize := decimal.NewFromInt(5)
	if size.LessThan(minSize) {
		size = minSize
	}

	// Create order signer
	signer := NewOrderSigner(c.privateKey, c.address, c.funderAddress, c.signatureType)

	// Create and sign order at exact price (no slippage for limit orders)
	signedOrder, err := signer.CreateSignedOrder(tokenID, sideInt, price, size)
	if err != nil {
		return "", fmt.Errorf("failed to sign order: %w", err)
	}

	log.Info().
		Str("token", tokenID[:20]+"...").
		Str("side", side).
		Str("size", size.String()).
		Str("price", price.String()).
		Msg("⚡ Order signed (native Go)")

	// Submit as FAK (Fill And Kill) - fills what it can immediately, cancels rest
	// Better than FOK because it allows partial fills
	// Better than GTC because unfilled portion is cancelled (no ghost orders)
	resp, err := c.submitSignedOrderWithType(signedOrder, "FAK")
	if err != nil {
		return "", err
	}

	if resp.OrderID == "" {
		return "", fmt.Errorf("no order ID returned: %s", resp.Message)
	}

	log.Info().
		Str("order_id", resp.OrderID).
		Str("token_id", tokenID[:16]+"...").
		Str("price", price.String()).
		Str("size", size.String()).
		Str("side", side).
		Str("type", "FAK").
		Msg("📋 Order placed (Fill-And-Kill)")

	return resp.OrderID, nil
}

// GetOrderStatus checks the status of an order
func (c *CLOBClient) GetOrderStatus(orderID string) (status string, filledSize, filledPrice decimal.Decimal, err error) {
	req, err := http.NewRequest("GET", c.baseURL+"/order/"+orderID, nil)
	if err != nil {
		return "", decimal.Zero, decimal.Zero, err
	}

	c.signL2Request(req, "GET", "/order/"+orderID, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", decimal.Zero, decimal.Zero, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	var orderInfo struct {
		OrderID     string `json:"order_id"`
		Status      string `json:"status"` // "live", "matched", "cancelled"
		SizeFilled  string `json:"size_filled"`
		PriceFilled string `json:"price_filled"`
	}

	if err := json.Unmarshal(respBody, &orderInfo); err != nil {
		return "", decimal.Zero, decimal.Zero, err
	}

	filledSize, _ = decimal.NewFromString(orderInfo.SizeFilled)
	filledPrice, _ = decimal.NewFromString(orderInfo.PriceFilled)

	return orderInfo.Status, filledSize, filledPrice, nil
}

// CancelOrder cancels an open order
func (c *CLOBClient) CancelOrder(orderID string) error {
	body := []byte(fmt.Sprintf(`{"orderID":"%s"}`, orderID))
	
	req, err := http.NewRequest("DELETE", c.baseURL+"/order", bytes.NewReader(body))
	if err != nil {
		return err
	}

	c.signL2Request(req, "DELETE", "/order", body)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cancel failed: %s", string(respBody))
	}

	return nil
}

// GetOpenOrders returns all open orders
func (c *CLOBClient) GetOpenOrders() ([]Order, error) {
	endpoint := "/data/orders"
	req, err := http.NewRequest("GET", c.baseURL+endpoint, nil)
	if err != nil {
		return nil, err
	}

	c.signL2Request(req, "GET", endpoint, nil)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Data []Order `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	return result.Data, nil
}

// signL2Request adds Level 2 authentication headers
// Based on: https://github.com/Polymarket/py-clob-client/blob/main/py_clob_client/signing/hmac.py
func (c *CLOBClient) signL2Request(req *http.Request, method, path string, body []byte) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)

	// Build message to sign: timestamp + method + requestPath + body
	// Must match exactly what py-clob-client does
	message := timestamp + method + path
	if body != nil && len(body) > 0 {
		message += string(body)
	}

	// Decode secret using URL-safe base64 (py-clob-client uses urlsafe_b64decode)
	secretBytes, err := base64.URLEncoding.DecodeString(c.apiSecret)
	if err != nil {
		// Try with padding
		padded := c.apiSecret
		if len(padded)%4 != 0 {
			padded += strings.Repeat("=", 4-len(padded)%4)
		}
		secretBytes, err = base64.URLEncoding.DecodeString(padded)
		if err != nil {
			// Last resort: try standard base64
			secretBytes, _ = base64.StdEncoding.DecodeString(c.apiSecret)
		}
	}

	// HMAC-SHA256 signature
	h := hmac.New(sha256.New, secretBytes)
	h.Write([]byte(message))
	// URL-safe base64 encode the result (py-clob-client uses urlsafe_b64encode)
	signature := base64.URLEncoding.EncodeToString(h.Sum(nil))

	// Set headers - Polymarket uses UNDERSCORES not hyphens!
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("POLY_API_KEY", c.apiKey)
	req.Header.Set("POLY_SIGNATURE", signature)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_PASSPHRASE", c.passphrase)

	// POLY_ADDRESS must be the SIGNER address (not funder!)
	// The signer is the address that derives the API credentials
	if c.address != (common.Address{}) {
		req.Header.Set("POLY_ADDRESS", c.address.Hex())
	}
}

// TestConnection verifies API connectivity
func (c *CLOBClient) TestConnection() error {
	req, err := http.NewRequest("GET", c.baseURL+"/time", nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	log.Info().Msg("✅ CLOB API connection verified")
	return nil
}

// GetAddress returns the wallet address (for display purposes)
func (c *CLOBClient) GetAddress() string {
	if c.address != (common.Address{}) {
		return c.address.Hex()
	}
	return ""
}

// PrivateKey returns the private key (for signing)
func (c *CLOBClient) PrivateKey() *ecdsa.PrivateKey {
	return c.privateKey
}

// Address returns the signing address
func (c *CLOBClient) Address() common.Address {
	return c.address
}

// FunderAddress returns the funder address
func (c *CLOBClient) FunderAddress() common.Address {
	return c.funderAddress
}

// ApiKey returns the API key
func (c *CLOBClient) ApiKey() string {
	return c.apiKey
}

// Legacy function for backward compatibility
func (c *CLOBClient) signRequest(req *http.Request, body []byte) {
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)

	signPayload := timestamp + req.Method + req.URL.Path
	if body != nil {
		signPayload += string(body)
	}

	h := hmac.New(sha256.New, []byte(c.apiSecret))
	h.Write([]byte(signPayload))
	signature := hex.EncodeToString(h.Sum(nil))

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("POLY_API_KEY", c.apiKey)
	req.Header.Set("POLY_SIGNATURE", signature)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_PASSPHRASE", c.passphrase)

	if c.address != (common.Address{}) {
		req.Header.Set("POLY_ADDRESS", c.address.Hex())
	}
}

// deriveApiCreds derives API credentials from the wallet by signing a message
// This calls the CLOB API to create or get existing credentials
func (c *CLOBClient) deriveApiCreds() (*ApiCreds, error) {
	if c.privateKey == nil {
		return nil, fmt.Errorf("wallet private key required")
	}

	// First try to derive existing creds
	creds, err := c.createOrDeriveCreds()
	if err != nil {
		return nil, err
	}

	return creds, nil
}

// createOrDeriveCreds creates or retrieves API credentials
func (c *CLOBClient) createOrDeriveCreds() (*ApiCreds, error) {
	timestamp := time.Now().Unix()
	nonce := int64(0)

	// Sign the CLOB auth message (EIP-712)
	signature, err := c.signClobAuthMessage(timestamp, nonce)
	if err != nil {
		return nil, fmt.Errorf("failed to sign auth message: %w", err)
	}

	// Build L1 headers for the derive request
	// Use funder address for POLY_ADDRESS (where funds are held)
	polyAddress := c.funderAddress.Hex()
	if polyAddress == "0x0000000000000000000000000000000000000000" {
		polyAddress = c.address.Hex()
	}

	headers := map[string]string{
		"POLY_ADDRESS":   polyAddress,
		"POLY_SIGNATURE": signature,
		"POLY_TIMESTAMP": strconv.FormatInt(timestamp, 10),
		"POLY_NONCE":     strconv.FormatInt(nonce, 10),
	}

	// Try to derive existing credentials first
	req, _ := http.NewRequest("GET", c.baseURL+"/auth/derive-api-key", nil)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("derive request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		var creds ApiCreds
		if err := json.Unmarshal(body, &creds); err != nil {
			return nil, fmt.Errorf("failed to parse credentials: %w", err)
		}
		return &creds, nil
	}

	// If derive fails, try to create new credentials
	req, _ = http.NewRequest("POST", c.baseURL+"/auth/api-key", nil)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err = c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("create request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var creds ApiCreds
	if err := json.Unmarshal(body, &creds); err != nil {
		return nil, fmt.Errorf("failed to parse credentials: %w", err)
	}

	return &creds, nil
}

// signClobAuthMessage signs the CLOB authentication message using EIP-712
// Domain: {name: "ClobAuthDomain", version: "1", chainId: 137}
// Message: {address, timestamp, nonce, message: "This message attests that I control the given wallet"}
func (c *CLOBClient) signClobAuthMessage(timestamp int64, nonce int64) (string, error) {
	// EIP-712 Domain separator
	// keccak256("EIP712Domain(string name,string version,uint256 chainId)")
	domainTypeHash := crypto.Keccak256Hash([]byte("EIP712Domain(string name,string version,uint256 chainId)"))
	
	// Domain values
	nameHash := crypto.Keccak256Hash([]byte("ClobAuthDomain"))
	versionHash := crypto.Keccak256Hash([]byte("1"))
	chainId := big.NewInt(137) // Polygon
	
	// Encode domain separator
	domainSeparator := crypto.Keccak256Hash(
		domainTypeHash.Bytes(),
		nameHash.Bytes(),
		versionHash.Bytes(),
		common.LeftPadBytes(chainId.Bytes(), 32),
	)

	// ClobAuth type hash
	// keccak256("ClobAuth(address address,string timestamp,uint256 nonce,string message)")
	clobAuthTypeHash := crypto.Keccak256Hash([]byte("ClobAuth(address address,string timestamp,uint256 nonce,string message)"))

	// Use funder address for the auth message
	authAddress := c.funderAddress
	if authAddress == (common.Address{}) {
		authAddress = c.address
	}

	// Message values
	timestampStr := strconv.FormatInt(timestamp, 10)
	messageStr := "This message attests that I control the given wallet"

	// Encode struct
	structHash := crypto.Keccak256Hash(
		clobAuthTypeHash.Bytes(),
		common.LeftPadBytes(authAddress.Bytes(), 32),
		crypto.Keccak256Hash([]byte(timestampStr)).Bytes(),
		common.LeftPadBytes(big.NewInt(nonce).Bytes(), 32),
		crypto.Keccak256Hash([]byte(messageStr)).Bytes(),
	)

	// Final hash: keccak256("\x19\x01" + domainSeparator + structHash)
	rawData := append([]byte{0x19, 0x01}, domainSeparator.Bytes()...)
	rawData = append(rawData, structHash.Bytes()...)
	hash := crypto.Keccak256Hash(rawData)

	// Sign
	sig, err := crypto.Sign(hash.Bytes(), c.privateKey)
	if err != nil {
		return "", err
	}

	// Adjust V value for Ethereum compatibility
	if sig[64] < 27 {
		sig[64] += 27
	}

	return "0x" + hex.EncodeToString(sig), nil
}

// GetMidPrice fetches the mid price for a token from the CLOB orderbook
// This is faster than using the gamma API for price discovery
func (c *CLOBClient) GetMidPrice(tokenID string) (decimal.Decimal, error) {
	// Use the CLOB price endpoint for fast price lookup
	url := fmt.Sprintf("%s/price?token_id=%s&side=BUY", c.baseURL, tokenID)
	
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return decimal.Zero, err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return decimal.Zero, fmt.Errorf("price lookup failed: %d", resp.StatusCode)
	}
	
	var result struct {
		Price string `json:"price"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return decimal.Zero, err
	}
	
	return decimal.NewFromString(result.Price)
}

// GetBookPrice fetches the best bid/ask from the orderbook for a token
func (c *CLOBClient) GetBookPrice(tokenID string) (bestBid, bestAsk decimal.Decimal, err error) {
	url := fmt.Sprintf("%s/book?token_id=%s", c.baseURL, tokenID)
	
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return decimal.Zero, decimal.Zero, fmt.Errorf("book lookup failed: %d", resp.StatusCode)
	}
	
	var result struct {
		Bids []struct {
			Price string `json:"price"`
			Size  string `json:"size"`
		} `json:"bids"`
		Asks []struct {
			Price string `json:"price"`
			Size  string `json:"size"`
		} `json:"asks"`
	}
	
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	
	if len(result.Bids) > 0 {
		bestBid, _ = decimal.NewFromString(result.Bids[0].Price)
	}
	if len(result.Asks) > 0 {
		bestAsk, _ = decimal.NewFromString(result.Asks[0].Price)
	}
	
	return bestBid, bestAsk, nil
}
