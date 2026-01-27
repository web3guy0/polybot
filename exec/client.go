package exec

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/rs/zerolog/log"
	"github.com/shopspring/decimal"
)

// ═══════════════════════════════════════════════════════════════════════════════
// POLYMARKET EXECUTION CLIENT
// ═══════════════════════════════════════════════════════════════════════════════
//
// Handles order placement and management with Polymarket CLOB API
// Supports proper EIP-712 signing for order authentication
//
// Order Types:
//   - GTC (Good Till Cancel) - stays in book until filled/cancelled
//   - GTD (Good Till Date) - expires at specific timestamp
//   - FOK (Fill or Kill) - immediate full fill or cancel
//   - FAK (Fill and Kill) - immediate partial fill, cancel rest (IOC)
//
// ═══════════════════════════════════════════════════════════════════════════════

const (
	PolymarketCLOB = "https://clob.polymarket.com"

	// Polygon Mainnet Contract Addresses
	CTFExchange  = "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"
	NegRiskExchange = "0xC5d563A36AE78145C45a50134d48A1215220f80a"
	ChainID      = 137

	// Signature types
	SigTypeEOA       = 0 // Standard EOA wallet
	SigTypePolyProxy = 1 // Polymarket proxy wallet (Magic/email)
	SigTypeBrowser   = 2 // Browser proxy

	// Order sides
	SideBuy  = "BUY"
	SideSell = "SELL"
)

// OrderType for Polymarket CLOB
type OrderType string

const (
	OrderTypeGTC OrderType = "GTC" // Good Till Cancel (limit)
	OrderTypeGTD OrderType = "GTD" // Good Till Date (limit with expiry)
	OrderTypeFOK OrderType = "FOK" // Fill or Kill (market, must fully fill)
	OrderTypeFAK OrderType = "FAK" // Fill and Kill / IOC (market, partial ok)
)

type Client struct {
	baseURL       string
	privateKey    *ecdsa.PrivateKey
	address       string
	funderAddress string
	apiKey        string
	apiSecret     string
	passphrase    string
	sigType       int
	dryRun        bool
	httpClient    *http.Client
}

// NewClient creates a new execution client
func NewClient() (*Client, error) {
	dryRun := os.Getenv("DRY_RUN") == "true"

	// Determine signature type (default to proxy wallet for Polymarket)
	sigType := SigTypePolyProxy
	if os.Getenv("SIG_TYPE") == "0" {
		sigType = SigTypeEOA
	}

	client := &Client{
		baseURL:       PolymarketCLOB,
		apiKey:        os.Getenv("CLOB_API_KEY"),
		apiSecret:     os.Getenv("CLOB_API_SECRET"),
		passphrase:    os.Getenv("CLOB_PASSPHRASE"),
		funderAddress: os.Getenv("FUNDER_ADDRESS"),
		sigType:       sigType,
		dryRun:        dryRun,
		httpClient:    &http.Client{Timeout: 30 * time.Second},
	}

	// Load private key
	pkHex := os.Getenv("WALLET_PRIVATE_KEY")
	if pkHex != "" {
		// Remove 0x prefix if present
		if len(pkHex) > 2 && pkHex[:2] == "0x" {
			pkHex = pkHex[2:]
		}
		pk, err := crypto.HexToECDSA(pkHex)
		if err != nil {
			return nil, fmt.Errorf("invalid private key: %w", err)
		}
		client.privateKey = pk
		client.address = crypto.PubkeyToAddress(pk.PublicKey).Hex()
	}

	mode := "DRY RUN"
	if !dryRun {
		mode = "LIVE"
	}
	log.Info().
		Str("mode", mode).
		Str("address", client.address).
		Msg("🚀 Execution client initialized")

	return client, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// ORDER TYPES & STRUCTURES
// ═══════════════════════════════════════════════════════════════════════════════

// SignedOrder represents a signed order ready for posting
type SignedOrder struct {
	Salt          string `json:"salt"`
	Maker         string `json:"maker"`
	Signer        string `json:"signer"`
	Taker         string `json:"taker"`
	TokenID       string `json:"tokenId"`
	MakerAmount   string `json:"makerAmount"`
	TakerAmount   string `json:"takerAmount"`
	Expiration    string `json:"expiration"`
	Nonce         string `json:"nonce"`
	FeeRateBps    string `json:"feeRateBps"`
	Side          string `json:"side"`
	SignatureType int    `json:"signatureType"`
	Signature     string `json:"signature"`
}

// OrderPayload is the full order submission payload
type OrderPayload struct {
	Order     SignedOrder `json:"order"`
	Owner     string      `json:"owner"`
	OrderType OrderType   `json:"orderType"`
	PostOnly  bool        `json:"postOnly,omitempty"`
}

// ═══════════════════════════════════════════════════════════════════════════════
// ORDER PLACEMENT
// ═══════════════════════════════════════════════════════════════════════════════

// PlaceLimitOrder places a GTC limit order (stays in book until filled/cancelled)
func (c *Client) PlaceLimitOrder(tokenID string, price, size decimal.Decimal, side string) (string, error) {
	return c.PlaceOrderWithType(tokenID, price, size, side, OrderTypeGTC, false)
}

// PlaceMarketOrder places a FOK market order (must fully fill or cancel)
func (c *Client) PlaceMarketOrder(tokenID string, price, size decimal.Decimal, side string) (string, error) {
	return c.PlaceOrderWithType(tokenID, price, size, side, OrderTypeFOK, false)
}

// PlaceIOCOrder places a FAK/IOC order (immediate partial fill, cancel rest)
func (c *Client) PlaceIOCOrder(tokenID string, price, size decimal.Decimal, side string) (string, error) {
	return c.PlaceOrderWithType(tokenID, price, size, side, OrderTypeFAK, false)
}

// PlaceOrder places a limit order on Polymarket (default GTC)
func (c *Client) PlaceOrder(tokenID string, price, size decimal.Decimal, side string) (string, error) {
	return c.PlaceLimitOrder(tokenID, price, size, side)
}

// PlaceOrderWithType places an order with specified type
func (c *Client) PlaceOrderWithType(tokenID string, price, size decimal.Decimal, side string, orderType OrderType, postOnly bool) (string, error) {
	if c.dryRun {
		orderID := fmt.Sprintf("DRY_%d", time.Now().UnixNano())
		log.Info().
			Str("order_id", orderID).
			Str("token", truncateToken(tokenID)).
			Str("side", side).
			Str("price", price.StringFixed(2)).
			Str("size", size.StringFixed(2)).
			Str("type", string(orderType)).
			Msg("📝 DRY RUN: Order would be placed")
		return orderID, nil
	}

	// Build the signed order
	signedOrder, err := c.buildSignedOrder(tokenID, price, size, side, orderType)
	if err != nil {
		return "", fmt.Errorf("build order failed: %w", err)
	}

	// Create order payload
	payload := OrderPayload{
		Order:     *signedOrder,
		Owner:     c.apiKey,
		OrderType: orderType,
		PostOnly:  postOnly,
	}

	// Send to API
	resp, err := c.post("/order", payload)
	if err != nil {
		return "", err
	}

	var result struct {
		OrderID   string `json:"orderID"`
		Status    string `json:"status"`
		ErrorMsg  string `json:"errorMsg"`
		Success   bool   `json:"success"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	if result.ErrorMsg != "" {
		return "", fmt.Errorf("API error: %s", result.ErrorMsg)
	}

	log.Info().
		Str("order_id", result.OrderID).
		Str("status", result.Status).
		Str("type", string(orderType)).
		Msg("✅ Order placed")

	return result.OrderID, nil
}

// buildSignedOrder creates a properly signed order for Polymarket
func (c *Client) buildSignedOrder(tokenID string, price, size decimal.Decimal, side string, orderType OrderType) (*SignedOrder, error) {
	// Determine maker (funder) - who holds the funds
	maker := c.funderAddress
	if maker == "" {
		maker = c.address
	}

	// Calculate amounts based on side (USDC has 6 decimals)
	// For BUY: makerAmount = USDC to spend, takerAmount = shares to receive
	// For SELL: makerAmount = shares to sell, takerAmount = USDC to receive
	usdcDecimals := decimal.NewFromInt(1000000) // 6 decimals

	var makerAmount, takerAmount decimal.Decimal
	var sideInt string
	
	if strings.ToUpper(side) == SideBuy {
		// BUY: spending USDC to get shares
		// makerAmount = size * price * 1e6 (USDC)
		// takerAmount = size * 1e6 (shares, also 6 decimals in Polymarket)
		makerAmount = size.Mul(price).Mul(usdcDecimals).Floor()
		takerAmount = size.Mul(usdcDecimals).Floor()
		sideInt = "BUY"
	} else {
		// SELL: selling shares to get USDC
		// makerAmount = size * 1e6 (shares)
		// takerAmount = size * price * 1e6 (USDC)
		makerAmount = size.Mul(usdcDecimals).Floor()
		takerAmount = size.Mul(price).Mul(usdcDecimals).Floor()
		sideInt = "SELL"
	}

	// Generate salt (random 256-bit number)
	salt := generateSalt()

	// Expiration: 0 for GTC/FOK/FAK, timestamp for GTD
	expiration := "0"
	if orderType == OrderTypeGTD {
		// 24 hours from now
		expiration = fmt.Sprintf("%d", time.Now().Add(24*time.Hour).Unix())
	}

	order := &SignedOrder{
		Salt:          salt,
		Maker:         maker,
		Signer:        c.address,
		Taker:         "0x0000000000000000000000000000000000000000", // Public order
		TokenID:       tokenID,
		MakerAmount:   makerAmount.String(),
		TakerAmount:   takerAmount.String(),
		Expiration:    expiration,
		Nonce:         "0",
		FeeRateBps:    "0",
		Side:          sideInt,
		SignatureType: c.sigType,
	}

	// Sign the order using EIP-712
	signature, err := c.signOrderEIP712(order)
	if err != nil {
		return nil, fmt.Errorf("signing failed: %w", err)
	}
	order.Signature = signature

	return order, nil
}

// signOrderEIP712 creates an EIP-712 signature for the order
func (c *Client) signOrderEIP712(order *SignedOrder) (string, error) {
	if c.privateKey == nil {
		return "", fmt.Errorf("private key not loaded")
	}

	// EIP-712 Domain Separator for Polymarket CTF Exchange
	domainSeparator := buildDomainSeparator(CTFExchange, ChainID)

	// Build order struct hash
	orderHash := buildOrderStructHash(order)

	// Combine: keccak256("\x19\x01" + domainSeparator + orderHash)
	var data []byte
	data = append(data, []byte("\x19\x01")...)
	data = append(data, domainSeparator[:]...)
	data = append(data, orderHash[:]...)
	
	finalHash := crypto.Keccak256(data)

	// Sign the hash
	sig, err := crypto.Sign(finalHash, c.privateKey)
	if err != nil {
		return "", err
	}

	// Ethereum uses v = 27/28, crypto.Sign returns 0/1
	if sig[64] < 27 {
		sig[64] += 27
	}

	return hexutil.Encode(sig), nil
}

// buildDomainSeparator creates the EIP-712 domain separator
func buildDomainSeparator(contractAddr string, chainID int) [32]byte {
	// EIP-712 Domain type hash
	domainTypeHash := crypto.Keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	
	nameHash := crypto.Keccak256([]byte("Polymarket CTF Exchange"))
	versionHash := crypto.Keccak256([]byte("1"))
	
	chainIDBig := big.NewInt(int64(chainID))
	chainIDBytes := common.LeftPadBytes(chainIDBig.Bytes(), 32)
	
	contractBytes := common.HexToAddress(contractAddr).Bytes()
	contractPadded := common.LeftPadBytes(contractBytes, 32)

	var data []byte
	data = append(data, domainTypeHash...)
	data = append(data, nameHash...)
	data = append(data, versionHash...)
	data = append(data, chainIDBytes...)
	data = append(data, contractPadded...)

	hash := crypto.Keccak256(data)
	var result [32]byte
	copy(result[:], hash)
	return result
}

// buildOrderStructHash creates the EIP-712 struct hash for an order
func buildOrderStructHash(order *SignedOrder) [32]byte {
	// Order type hash
	orderTypeHash := crypto.Keccak256([]byte("Order(uint256 salt,address maker,address signer,address taker,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint256 expiration,uint256 nonce,uint256 feeRateBps,uint8 side,uint8 signatureType)"))

	salt := padUint256(order.Salt)
	maker := common.LeftPadBytes(common.HexToAddress(order.Maker).Bytes(), 32)
	signer := common.LeftPadBytes(common.HexToAddress(order.Signer).Bytes(), 32)
	taker := common.LeftPadBytes(common.HexToAddress(order.Taker).Bytes(), 32)
	tokenID := padUint256(order.TokenID)
	makerAmount := padUint256(order.MakerAmount)
	takerAmount := padUint256(order.TakerAmount)
	expiration := padUint256(order.Expiration)
	nonce := padUint256(order.Nonce)
	feeRateBps := padUint256(order.FeeRateBps)
	
	// Side: 0 = BUY, 1 = SELL
	sideVal := 0
	if order.Side == "SELL" {
		sideVal = 1
	}
	sidePadded := common.LeftPadBytes([]byte{byte(sideVal)}, 32)
	
	// Signature type
	sigTypePadded := common.LeftPadBytes([]byte{byte(order.SignatureType)}, 32)

	var data []byte
	data = append(data, orderTypeHash...)
	data = append(data, salt...)
	data = append(data, maker...)
	data = append(data, signer...)
	data = append(data, taker...)
	data = append(data, tokenID...)
	data = append(data, makerAmount...)
	data = append(data, takerAmount...)
	data = append(data, expiration...)
	data = append(data, nonce...)
	data = append(data, feeRateBps...)
	data = append(data, sidePadded...)
	data = append(data, sigTypePadded...)

	hash := crypto.Keccak256(data)
	var result [32]byte
	copy(result[:], hash)
	return result
}

// padUint256 converts a string number to 32-byte big-endian padded
func padUint256(s string) []byte {
	n := new(big.Int)
	n.SetString(s, 10)
	return common.LeftPadBytes(n.Bytes(), 32)
}

// generateSalt creates a random salt for orders
func generateSalt() string {
	// Use crypto-safe random
	b := make([]byte, 32)
	rand.Read(b)
	n := new(big.Int).SetBytes(b)
	return n.String()
}

// truncateToken truncates tokenID for logging
func truncateToken(tokenID string) string {
	if len(tokenID) > 16 {
		return tokenID[:16] + "..."
	}
	return tokenID
}

// CancelOrder cancels an existing order
func (c *Client) CancelOrder(orderID string) error {
	if c.dryRun {
		log.Info().Str("order_id", orderID).Msg("📝 DRY RUN: Order would be cancelled")
		return nil
	}

	// Cancel uses DELETE with body containing orderID
	body := map[string]string{"orderID": orderID}
	_, err := c.deleteWithBody("/order", body)
	if err != nil {
		return fmt.Errorf("cancel order failed: %w", err)
	}
	
	log.Info().Str("order_id", orderID).Msg("🗑️ Order cancelled")
	return nil
}

// CancelAllOrders cancels all open orders
func (c *Client) CancelAllOrders() error {
	if c.dryRun {
		log.Info().Msg("📝 DRY RUN: All orders would be cancelled")
		return nil
	}

	_, err := c.deleteWithBody("/cancel-all", nil)
	if err != nil {
		return fmt.Errorf("cancel all orders failed: %w", err)
	}
	
	log.Info().Msg("🗑️ All orders cancelled")
	return nil
}

// GetBalance returns current balance from Polymarket
func (c *Client) GetBalance() (decimal.Decimal, error) {
	if c.dryRun {
		return decimal.NewFromFloat(100), nil // Simulated balance
	}

	if c.address == "" {
		return decimal.Zero, fmt.Errorf("no wallet address")
	}

	// Try CLOB balance-allowance endpoint (COLLATERAL = USDC balance)
	if c.apiKey != "" && c.apiSecret != "" {
		balance, err := c.getCLOBCollateralBalance()
		if err == nil && !balance.IsZero() {
			return balance, nil
		}
		log.Debug().Err(err).Msg("CLOB balance check failed, trying on-chain")
	}

	// Fallback: Try on-chain USDC for both funder and signer addresses
	addresses := []string{c.address}
	if c.funderAddress != "" && c.funderAddress != c.address {
		addresses = append(addresses, c.funderAddress)
	}

	totalBalance := decimal.Zero
	for _, addr := range addresses {
		balance, _ := c.getBalanceForAddress(addr)
		totalBalance = totalBalance.Add(balance)
	}

	return totalBalance, nil
}

// getCLOBCollateralBalance fetches USDC balance from CLOB balance-allowance endpoint
func (c *Client) getCLOBCollateralBalance() (decimal.Decimal, error) {
	// Polymarket CLOB balance-allowance endpoint for COLLATERAL (USDC)
	resp, err := c.get("/balance-allowance?asset_type=COLLATERAL&signature_type=1")
	if err != nil {
		return decimal.Zero, err
	}

	var result struct {
		Balance   string `json:"balance"`
		Allowance string `json:"allowance"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return decimal.Zero, err
	}

	if result.Balance == "" {
		return decimal.Zero, nil
	}

	balance, err := decimal.NewFromString(result.Balance)
	if err != nil {
		return decimal.Zero, err
	}

	// Balance is in micro USDC (6 decimals)
	return balance.Div(decimal.NewFromInt(1000000)), nil
}

// getBalanceForAddress gets on-chain USDC balance for an address
func (c *Client) getBalanceForAddress(address string) (decimal.Decimal, error) {
	// USDC.e on Polygon (what Polymarket uses)
	usdceAddress := "0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174"
	balance, err := c.getOnChainBalanceFor(address, usdceAddress)
	if err == nil && !balance.IsZero() {
		return balance, nil
	}

	// Native USDC on Polygon
	nativeUSDC := "0x3c499c542cEF5E3811e1192ce70d8cC03d5c3359"
	balance2, err := c.getOnChainBalanceFor(address, nativeUSDC)
	if err == nil {
		return balance.Add(balance2), nil
	}

	return balance, nil
}

// getOnChainBalanceFor fetches ERC20 balance for a specific address
func (c *Client) getOnChainBalanceFor(walletAddr, tokenAddr string) (decimal.Decimal, error) {
	polygonRPC := "https://polygon-rpc.com"
	
	// balanceOf(address) selector = 0x70a08231
	cleanAddr := walletAddr
	if len(cleanAddr) > 2 && cleanAddr[:2] == "0x" {
		cleanAddr = cleanAddr[2:]
	}
	paddedAddr := fmt.Sprintf("%064s", cleanAddr) // Pad to 64 hex chars
	data := "0x70a08231" + paddedAddr

	payload := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "eth_call",
		"params": []interface{}{
			map[string]string{
				"to":   tokenAddr,
				"data": data,
			},
			"latest",
		},
		"id": 1,
	}

	jsonBody, _ := json.Marshal(payload)
	resp, err := http.Post(polygonRPC, "application/json", bytes.NewBuffer(jsonBody))
	if err != nil {
		return decimal.Zero, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return decimal.Zero, err
	}

	var result struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return decimal.Zero, err
	}

	if result.Result == "" || result.Result == "0x" || result.Result == "0x0" {
		return decimal.Zero, nil
	}

	// Parse hex to decimal
	return parseHexBalance(result.Result)
}

// GetOpenOrders returns all open orders
func (c *Client) GetOpenOrders() ([]Order, error) {
	resp, err := c.get("/orders?status=live")
	if err != nil {
		return nil, err
	}

	var orders []Order
	if err := json.Unmarshal(resp, &orders); err != nil {
		return nil, err
	}

	return orders, nil
}

// Order represents an order from the API
type Order struct {
	ID        string          `json:"id"`
	TokenID   string          `json:"asset_id"`
	Price     decimal.Decimal `json:"price"`
	Size      decimal.Decimal `json:"original_size"`
	Filled    decimal.Decimal `json:"size_matched"`
	Side      string          `json:"side"`
	Status    string          `json:"status"`
	CreatedAt time.Time       `json:"created_at"`
}

// ═══════════════════════════════════════════════════════════════════════════════
// HTTP HELPERS
// ═══════════════════════════════════════════════════════════════════════════════

func (c *Client) get(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	c.addHeaders(req)
	return c.doRequest(req)
}

func (c *Client) post(path string, body interface{}) ([]byte, error) {
	jsonBody, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", c.baseURL+path, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.addHeaders(req)
	return c.doRequest(req)
}

func (c *Client) delete(path string) ([]byte, error) {
	req, err := http.NewRequest("DELETE", c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	c.addHeaders(req)
	return c.doRequest(req)
}

func (c *Client) deleteWithBody(path string, body interface{}) ([]byte, error) {
	var jsonBody []byte
	if body != nil {
		jsonBody, _ = json.Marshal(body)
	}
	req, err := http.NewRequest("DELETE", c.baseURL+path, bytes.NewBuffer(jsonBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.addHeaders(req)
	return c.doRequest(req)
}

func (c *Client) addHeaders(req *http.Request) {
	timestamp := fmt.Sprintf("%d", time.Now().Unix())

	// L2 Headers require POLY_ADDRESS (signer address)
	req.Header.Set("POLY_ADDRESS", c.address)
	req.Header.Set("POLY_API_KEY", c.apiKey)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_PASSPHRASE", c.passphrase)

	// Generate HMAC-SHA256 signature (base64 URL-safe encoded)
	if c.apiSecret != "" {
		// Message format: timestamp + method + requestPath (NO query params!)
		message := timestamp + req.Method + req.URL.Path
		
		if req.Body != nil {
			// Read body and reset
			bodyBytes, _ := io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			if len(bodyBytes) > 0 {
				message += string(bodyBytes)
			}
		}
		signature := c.hmacSign(message)
		req.Header.Set("POLY_SIGNATURE", signature)
	}
}

func (c *Client) doRequest(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// ═══════════════════════════════════════════════════════════════════════════════
// HMAC SIGNING (for API authentication)
// ═══════════════════════════════════════════════════════════════════════════════

func (c *Client) hmacSign(message string) string {
	// Decode base64 URL-safe secret (matches Python's urlsafe_b64decode)
	key, err := base64.URLEncoding.DecodeString(c.apiSecret)
	if err != nil {
		// Try standard encoding as fallback
		key, err = base64.StdEncoding.DecodeString(c.apiSecret)
		if err != nil {
			// If still fails, use raw secret
			key = []byte(c.apiSecret)
		}
	}
	
	// HMAC-SHA256
	h := hmac.New(sha256.New, key)
	h.Write([]byte(message))
	
	// Return URL-safe base64-encoded signature (matches Python's urlsafe_b64encode)
	return base64.URLEncoding.EncodeToString(h.Sum(nil))
}

// parseHexBalance parses a hex balance string to decimal (6 decimals for USDC)
func parseHexBalance(hexStr string) (decimal.Decimal, error) {
	if len(hexStr) < 2 {
		return decimal.Zero, nil
	}
	hexVal := hexStr[2:] // Remove 0x
	
	balance := decimal.Zero
	for _, ch := range hexVal {
		balance = balance.Mul(decimal.NewFromInt(16))
		var digit int64
		if ch >= '0' && ch <= '9' {
			digit = int64(ch - '0')
		} else if ch >= 'a' && ch <= 'f' {
			digit = int64(ch - 'a' + 10)
		} else if ch >= 'A' && ch <= 'F' {
			digit = int64(ch - 'A' + 10)
		}
		balance = balance.Add(decimal.NewFromInt(digit))
	}

	// USDC has 6 decimals
	return balance.Div(decimal.NewFromInt(1000000)), nil
}

// IsDryRun returns true if in dry run mode
func (c *Client) IsDryRun() bool {
	return c.dryRun
}
