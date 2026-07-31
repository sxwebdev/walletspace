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
}

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
)
