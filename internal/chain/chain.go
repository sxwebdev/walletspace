// Package chain defines protocol-neutral boundaries shared by chain adapters.
package chain

import (
	"context"
	"errors"
)

type Family string

const (
	FamilyTron Family = "tron"
	FamilyEVM  Family = "evm"
)

type Signer interface {
	Family() Family
	PublicKey() []byte
	SignDigest(ctx context.Context, digest []byte) ([]byte, error)
}

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
}

// ErrFeeChanged reports that the network moved past what the user approved, so
// the transaction on screen is no longer the one that would be signed. The
// caller has to show the new numbers and ask again.
var ErrFeeChanged = errors.New("fee estimate changed since it was approved")

type TransferEstimate struct {
	NetworkID            string `json:"network_id"`
	Amount               string `json:"amount"`
	Fee                  string `json:"fee"`
	GasLimit             uint64 `json:"gas_limit,omitempty"`
	FeeModel             string `json:"fee_model,omitempty"`
	MaxFeePerGas         string `json:"max_fee_per_gas,omitempty"`
	MaxPriorityFeePerGas string `json:"max_priority_fee_per_gas,omitempty"`
	Warning              string `json:"warning,omitempty"`
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
