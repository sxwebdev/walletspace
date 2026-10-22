// Package chain defines protocol-neutral boundaries shared by chain adapters.
package chain

import (
	"context"
	"errors"
)

type Family string

const (
	FamilyTron   Family = "tron"
	FamilyEVM    Family = "evm"
	FamilySolana Family = "solana"
)

// Key is what every signer has in common: the family it signs for and the
// public half of its key. Which signatures it can make depends on the curve,
// and that is a separate interface on purpose — no value can both sign a
// digest and sign a message, so a caller holding the wrong kind cannot reach
// the other operation at all.
type Key interface {
	Family() Family
	// PublicKey is the uncompressed 65-byte point for secp256k1 and the
	// 32-byte public key for ed25519.
	PublicKey() []byte
}

// DigestSigner signs with a secp256k1 key, as Tron and EVM transactions are
// signed: the caller hashes the transaction, the signer signs exactly 32
// bytes and answers with a 65-byte recoverable signature R‖S‖V.
type DigestSigner interface {
	Key
	SignDigest(ctx context.Context, digest []byte) ([]byte, error)
}

// MessageSigner signs with an ed25519 key, as Solana transactions are signed:
// the signature covers the whole serialized message, which ed25519 hashes
// itself, and is 64 bytes.
type MessageSigner interface {
	Key
	SignMessage(ctx context.Context, message []byte) ([]byte, error)
}

// Signer is the digest signer under the name the Tron and EVM code has always
// used for it.
type Signer = DigestSigner

// ErrSignerMismatch reports a request for a kind of signature the account's
// key cannot make: a digest signature from an ed25519 key, or a message
// signature from a secp256k1 key.
var ErrSignerMismatch = errors.New("the account's key cannot make this kind of signature")

type AccountAddress struct {
	AccountID string `json:"account_id"`
	Address   string `json:"address"`
}

type Asset struct {
	ID         string `json:"id"`
	NetworkID  string `json:"network_id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Symbol     string `json:"symbol"`
	Decimals   uint8  `json:"decimals"`
	Contract   string `json:"contract,omitempty"`
	Configured bool   `json:"configured,omitempty"`
}

type Balance struct {
	AccountID string `json:"account_id"`
	AssetID   string `json:"asset_id"`
	Amount    string `json:"amount,omitempty"`
	Stale     bool   `json:"stale,omitempty"`
	Error     string `json:"error,omitempty"`
}

type TransferRequest struct {
	// Data and BeforeBroadcast are only set by trusted backend workflows. They
	// are never decoded from the public transfer DTO.
	Data            []byte                  `json:"-"`
	BeforeBroadcast func(hash string) error `json:"-"`
	// BeforeSigning is called by a family whose transactions expire at a
	// block height (Solana) with that height, before the transaction is
	// signed, so the caller can record it in the same write as the
	// transaction id. An error stops the signing.
	BeforeSigning func(lastValidBlockHeight uint64) error `json:"-"`
	// Action is empty for transfers, or wrap/unwrap for native coin conversion.
	Action    string
	AccountID string
	From      string
	To        string
	Asset     Asset
	Amount    string
	// Approved is the fee ceiling the user saw and confirmed. It is what gets
	// signed: without it the sender would re-ask the node at signing time and
	// commit to whatever it answered, which is a different transaction from the
	// one that was on screen.
	Approved *FeeApproval
}

// FeeApproval is the fee the user agreed to, carried back from the estimate
// they were shown.
//
// These are ceilings, not predictions. EIP-1559 charges the base fee plus the
// tip and refunds the rest, so signing with the approved caps means the sender
// can never be charged more than what was displayed — however the node answers
// in between.
type FeeApproval struct {
	FeeModel             string
	GasLimit             uint64
	MaxFeePerGas         string
	MaxPriorityFeePerGas string

	// Solana. The compute-unit limit and price are signed as approved, so the
	// priority fee is exactly what was shown; the rent is the most a new
	// recipient token account may cost; Fee is the total that was shown.
	ComputeUnitLimit              uint64
	ComputeUnitPriceMicroLamports uint64
	CreatesTokenAccount           bool
	TokenAccountRent              string
	Fee                           string
}

// ErrFeeChanged reports that the network moved past what the user approved, so
// the transaction on screen is no longer the one that would be signed. The
// caller has to show the new numbers and ask again.
var ErrFeeChanged = errors.New("fee estimate changed since it was approved")

// ErrStateChanged reports that what the user approved was computed from chain
// state that no longer holds — a swap price that moved past the approved
// minimum, an approval that is already in place or not yet confirmed. Nothing
// was signed; the caller has to read the chain again and show the result.
var ErrStateChanged = errors.New("chain state changed since it was read")

type TransferEstimate struct {
	NetworkID            string `json:"network_id"`
	Amount               string `json:"amount"`
	Fee                  string `json:"fee"`
	GasLimit             uint64 `json:"gas_limit,omitempty"`
	FeeModel             string `json:"fee_model,omitempty"`
	MaxFeePerGas         string `json:"max_fee_per_gas,omitempty"`
	MaxPriorityFeePerGas string `json:"max_priority_fee_per_gas,omitempty"`
	Warning              string `json:"warning,omitempty"`
	NativeBalance        string `json:"native_balance,omitempty"`
	FeeShortfall         string `json:"fee_shortfall,omitempty"`
	AdditionalFee        string `json:"additional_fee,omitempty"`
	EstimatedFee         string `json:"estimated_fee,omitempty"`

	// Solana: echoed back on send as the approval.
	ComputeUnitLimit              uint64 `json:"compute_unit_limit,omitempty"`
	ComputeUnitPriceMicroLamports uint64 `json:"compute_unit_price_micro_lamports,omitempty"`
	CreatesTokenAccount           bool   `json:"creates_token_account,omitempty"`
	TokenAccountRent              string `json:"token_account_rent,omitempty"`
}

type Transaction struct {
	NetworkID string `json:"network_id"`
	Hash      string `json:"hash"`
	Status    string `json:"status"`
}

var (
	ErrInvalidRequest = errors.New("invalid chain request")
	ErrNotSupported   = errors.New("operation is not supported")
	// ErrBroadcastUnknown reports that a signed transaction was sent and the
	// answer never came back. It is not a failure: the transaction may well be
	// on chain, and the one thing that must not follow is building a second one
	// for the same intent. An error wrapping this always travels with the
	// locally computed transaction id.
	ErrBroadcastUnknown = errors.New("the transaction was sent but the node's answer was lost")
)
