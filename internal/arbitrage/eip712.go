// Package arbitrage provides latency arbitrage functionality
//
// eip712.go - Native Go EIP-712 order signing for Polymarket CTF Exchange
// Eliminates Python subprocess latency (500ms -> 5ms)
// Based on: https://github.com/Polymarket/py-order-utils
package arbitrage

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"math/rand"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/shopspring/decimal"
)

// Polymarket CTF Exchange contract addresses (Polygon Mainnet)
const (
	PolygonChainID            = 137
	CTFExchangeAddress        = "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"
	CollateralAddress         = "0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174" // USDC
	ConditionalTokensAddress  = "0x4D97DCd97eC945f40cF65F87097ACe5EA0476045"
	ZeroAddress               = "0x0000000000000000000000000000000000000000"
)

// Signature types
const (
	SignatureTypeEOA       = 0 // Externally Owned Account
	SignatureTypePolyProxy = 1 // Polymarket Proxy (email login)
	SignatureTypeGnosisSafe = 2 // Gnosis Safe
)

// Order sides
const (
	SideBuy  = 0
	SideSell = 1
)

// CTFOrder represents a Polymarket CTF Exchange order
type CTFOrder struct {
	Salt          *big.Int       `json:"salt"`
	Maker         common.Address `json:"maker"`
	Signer        common.Address `json:"signer"`
	Taker         common.Address `json:"taker"`
	TokenID       *big.Int       `json:"tokenId"`
	MakerAmount   *big.Int       `json:"makerAmount"`
	TakerAmount   *big.Int       `json:"takerAmount"`
	Expiration    *big.Int       `json:"expiration"`
	Nonce         *big.Int       `json:"nonce"`
	FeeRateBps    *big.Int       `json:"feeRateBps"`
	Side          uint8          `json:"side"`
	SignatureType uint8          `json:"signatureType"`
}

// SignedCTFOrder is an order with its signature
type SignedCTFOrder struct {
	Order     *CTFOrder `json:"order"`
	Signature string    `json:"signature"`
}

// OrderSigner handles EIP-712 order signing
type OrderSigner struct {
	privateKey    *ecdsa.PrivateKey
	signerAddress common.Address
	funderAddress common.Address
	chainID       int64
	exchangeAddr  common.Address
	signatureType int
}

// NewOrderSigner creates a new EIP-712 order signer
func NewOrderSigner(privateKey *ecdsa.PrivateKey, signerAddr, funderAddr common.Address, signatureType int) *OrderSigner {
	return &OrderSigner{
		privateKey:    privateKey,
		signerAddress: signerAddr,
		funderAddress: funderAddr,
		chainID:       PolygonChainID,
		exchangeAddr:  common.HexToAddress(CTFExchangeAddress),
		signatureType: signatureType,
	}
}

// CreateOrder creates an unsigned order
func (s *OrderSigner) CreateOrder(tokenID string, side int, price, size decimal.Decimal) (*CTFOrder, error) {
	// Parse token ID as big.Int
	tokenIDInt := new(big.Int)
	tokenIDInt.SetString(tokenID, 10)

	// Calculate amounts based on side
	// USDC has 6 decimals, shares have 6 decimals
	var makerAmount, takerAmount *big.Int

	// Convert to 6 decimal units
	priceFloat, _ := price.Float64()
	sizeFloat, _ := size.Float64()

	if side == SideBuy {
		// Buying: we give USDC (maker), receive shares (taker)
		// makerAmount = size * price (USDC we spend)
		// takerAmount = size (shares we get)
		usdcAmount := sizeFloat * priceFloat
		makerAmount = toTokenDecimals(usdcAmount)
		takerAmount = toTokenDecimals(sizeFloat)
	} else {
		// Selling: we give shares (maker), receive USDC (taker)
		// makerAmount = size (shares we sell)
		// takerAmount = size * price (USDC we receive)
		makerAmount = toTokenDecimals(sizeFloat)
		usdcAmount := sizeFloat * priceFloat
		takerAmount = toTokenDecimals(usdcAmount)
	}

	// Generate random salt
	salt := generateSalt()

	// Use funder address as maker (who holds the funds)
	maker := s.funderAddress
	if maker == (common.Address{}) {
		maker = s.signerAddress
	}

	order := &CTFOrder{
		Salt:          salt,
		Maker:         maker,
		Signer:        s.signerAddress,
		Taker:         common.HexToAddress(ZeroAddress), // Public order
		TokenID:       tokenIDInt,
		MakerAmount:   makerAmount,
		TakerAmount:   takerAmount,
		Expiration:    big.NewInt(0), // No expiration
		Nonce:         big.NewInt(0),
		FeeRateBps:    big.NewInt(0), // 0% fee
		Side:          uint8(side),
		SignatureType: uint8(s.signatureType),
	}

	return order, nil
}

// SignOrder signs an order using EIP-712
func (s *OrderSigner) SignOrder(order *CTFOrder) (*SignedCTFOrder, error) {
	// Build EIP-712 typed data
	typedData := s.buildTypedData(order)

	// Hash the typed data
	domainSeparator, err := typedData.HashStruct("EIP712Domain", typedData.Domain.Map())
	if err != nil {
		return nil, fmt.Errorf("failed to hash domain: %w", err)
	}

	messageHash, err := typedData.HashStruct(typedData.PrimaryType, typedData.Message)
	if err != nil {
		return nil, fmt.Errorf("failed to hash message: %w", err)
	}

	// Create EIP-712 hash: keccak256("\x19\x01" || domainSeparator || messageHash)
	rawData := []byte(fmt.Sprintf("\x19\x01%s%s", string(domainSeparator), string(messageHash)))
	hash := crypto.Keccak256Hash(rawData)

	// Sign the hash
	signature, err := crypto.Sign(hash.Bytes(), s.privateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign: %w", err)
	}

	// Adjust V value (Ethereum uses 27/28)
	if signature[64] < 27 {
		signature[64] += 27
	}

	return &SignedCTFOrder{
		Order:     order,
		Signature: fmt.Sprintf("0x%x", signature),
	}, nil
}

// CreateSignedOrder creates and signs an order in one call
func (s *OrderSigner) CreateSignedOrder(tokenID string, side int, price, size decimal.Decimal) (*SignedCTFOrder, error) {
	order, err := s.CreateOrder(tokenID, side, price, size)
	if err != nil {
		return nil, err
	}

	return s.SignOrder(order)
}

// buildTypedData builds the EIP-712 typed data structure
func (s *OrderSigner) buildTypedData(order *CTFOrder) apitypes.TypedData {
	return apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
			"Order": {
				{Name: "salt", Type: "uint256"},
				{Name: "maker", Type: "address"},
				{Name: "signer", Type: "address"},
				{Name: "taker", Type: "address"},
				{Name: "tokenId", Type: "uint256"},
				{Name: "makerAmount", Type: "uint256"},
				{Name: "takerAmount", Type: "uint256"},
				{Name: "expiration", Type: "uint256"},
				{Name: "nonce", Type: "uint256"},
				{Name: "feeRateBps", Type: "uint256"},
				{Name: "side", Type: "uint8"},
				{Name: "signatureType", Type: "uint8"},
			},
		},
		PrimaryType: "Order",
		Domain: apitypes.TypedDataDomain{
			Name:              "Polymarket CTF Exchange",
			Version:           "1",
			ChainId:           math.NewHexOrDecimal256(s.chainID),
			VerifyingContract: s.exchangeAddr.Hex(),
		},
		Message: apitypes.TypedDataMessage{
			"salt":          order.Salt.String(),
			"maker":         order.Maker.Hex(),
			"signer":        order.Signer.Hex(),
			"taker":         order.Taker.Hex(),
			"tokenId":       order.TokenID.String(),
			"makerAmount":   order.MakerAmount.String(),
			"takerAmount":   order.TakerAmount.String(),
			"expiration":    order.Expiration.String(),
			"nonce":         order.Nonce.String(),
			"feeRateBps":    order.FeeRateBps.String(),
			"side":          fmt.Sprintf("%d", order.Side),
			"signatureType": fmt.Sprintf("%d", order.SignatureType),
		},
	}
}

// toTokenDecimals converts a float amount to token decimals (6 for USDC)
func toTokenDecimals(amount float64) *big.Int {
	// Multiply by 10^6 for 6 decimal places
	scaled := amount * 1e6
	return big.NewInt(int64(scaled))
}

// generateSalt generates a random 256-bit salt
func generateSalt() *big.Int {
	rand.Seed(time.Now().UnixNano())
	salt := new(big.Int)
	// Generate 32 random bytes
	bytes := make([]byte, 32)
	for i := range bytes {
		bytes[i] = byte(rand.Intn(256))
	}
	salt.SetBytes(bytes)
	return salt
}

// ToAPIPayload converts a signed order to the API payload format
func (o *SignedCTFOrder) ToAPIPayload() map[string]interface{} {
	return map[string]interface{}{
		"order": map[string]interface{}{
			"salt":          o.Order.Salt.String(),
			"maker":         o.Order.Maker.Hex(),
			"signer":        o.Order.Signer.Hex(),
			"taker":         o.Order.Taker.Hex(),
			"tokenId":       o.Order.TokenID.String(),
			"makerAmount":   o.Order.MakerAmount.String(),
			"takerAmount":   o.Order.TakerAmount.String(),
			"expiration":    o.Order.Expiration.String(),
			"nonce":         o.Order.Nonce.String(),
			"feeRateBps":    o.Order.FeeRateBps.String(),
			"side":          fmt.Sprintf("%d", o.Order.Side),
			"signatureType": fmt.Sprintf("%d", o.Order.SignatureType),
		},
		"signature": o.Signature,
		"owner":     o.Order.Maker.Hex(),
		"orderType": "FOK", // Fill or Kill for immediate execution
	}
}
