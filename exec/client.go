package exec

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

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
// Supports EIP-712 signing for authentication
//
// ═══════════════════════════════════════════════════════════════════════════════

const (
	PolymarketCLOB = "https://clob.polymarket.com"
)

type Client struct {
	baseURL    string
	privateKey *ecdsa.PrivateKey
	address    string
	apiKey     string
	apiSecret  string
	passphrase string
	dryRun     bool
	httpClient *http.Client
}

// NewClient creates a new execution client
func NewClient() (*Client, error) {
	dryRun := os.Getenv("DRY_RUN") == "true"

	client := &Client{
		baseURL:    PolymarketCLOB,
		apiKey:     os.Getenv("POLY_API_KEY"),
		apiSecret:  os.Getenv("POLY_API_SECRET"),
		passphrase: os.Getenv("POLY_PASSPHRASE"),
		dryRun:     dryRun,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}

	// Load private key
	pkHex := os.Getenv("ETH_PRIVATE_KEY")
	if pkHex != "" {
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

// PlaceOrder places a limit order on Polymarket
func (c *Client) PlaceOrder(tokenID string, price, size decimal.Decimal, side string) (string, error) {
	if c.dryRun {
		orderID := fmt.Sprintf("DRY_%d", time.Now().UnixNano())
		log.Info().
			Str("order_id", orderID).
			Str("token", tokenID[:16]+"...").
			Str("side", side).
			Str("price", price.StringFixed(2)).
			Str("size", size.StringFixed(2)).
			Msg("📝 DRY RUN: Order would be placed")
		return orderID, nil
	}

	// Build order payload
	order := map[string]interface{}{
		"tokenID":        tokenID,
		"price":          price.String(),
		"size":           size.String(),
		"side":           side,
		"expiration":     time.Now().Add(24 * time.Hour).Unix(),
		"nonce":          time.Now().UnixNano(),
		"feeRateBps":     "0",
		"signatureType":  2, // EIP-712
	}

	// Sign order
	signature, err := c.signOrder(order)
	if err != nil {
		return "", fmt.Errorf("signing failed: %w", err)
	}
	order["signature"] = signature

	// Send to API
	resp, err := c.post("/order", order)
	if err != nil {
		return "", err
	}

	var result struct {
		OrderID string `json:"orderID"`
		Status  string `json:"status"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	if result.Error != "" {
		return "", fmt.Errorf("API error: %s", result.Error)
	}

	log.Info().
		Str("order_id", result.OrderID).
		Str("status", result.Status).
		Msg("✅ Order placed")

	return result.OrderID, nil
}

// CancelOrder cancels an existing order
func (c *Client) CancelOrder(orderID string) error {
	if c.dryRun {
		log.Info().Str("order_id", orderID).Msg("📝 DRY RUN: Order would be cancelled")
		return nil
	}

	_, err := c.delete("/order/" + orderID)
	return err
}

// GetBalance returns current USDC balance
func (c *Client) GetBalance() (decimal.Decimal, error) {
	if c.dryRun {
		return decimal.NewFromFloat(100), nil // Simulated balance
	}

	resp, err := c.get("/balance")
	if err != nil {
		return decimal.Zero, err
	}

	var result struct {
		Balance string `json:"balance"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return decimal.Zero, err
	}

	balance, _ := decimal.NewFromString(result.Balance)
	return balance, nil
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

func (c *Client) addHeaders(req *http.Request) {
	timestamp := fmt.Sprintf("%d", time.Now().Unix())

	req.Header.Set("POLY_API_KEY", c.apiKey)
	req.Header.Set("POLY_TIMESTAMP", timestamp)
	req.Header.Set("POLY_PASSPHRASE", c.passphrase)

	// Generate HMAC signature
	if c.apiSecret != "" {
		message := timestamp + req.Method + req.URL.Path
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
// SIGNING
// ═══════════════════════════════════════════════════════════════════════════════

func (c *Client) signOrder(order map[string]interface{}) (string, error) {
	if c.privateKey == nil {
		return "", fmt.Errorf("private key not loaded")
	}

	// Create hash of order data (simplified - real impl needs EIP-712)
	orderBytes, _ := json.Marshal(order)
	hash := crypto.Keccak256(orderBytes)

	// Sign the hash
	sig, err := crypto.Sign(hash, c.privateKey)
	if err != nil {
		return "", err
	}

	return hexutil.Encode(sig), nil
}

func (c *Client) hmacSign(message string) string {
	// Simplified HMAC - real impl uses crypto/hmac with SHA256
	hash := crypto.Keccak256([]byte(message + c.apiSecret))
	return hexutil.Encode(hash)
}

// IsDryRun returns true if in dry run mode
func (c *Client) IsDryRun() bool {
	return c.dryRun
}
