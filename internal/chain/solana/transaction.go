package solana

import (
	"context"
	"fmt"
	"sync"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/operation"
)

const (
	// expiryMargin is how far past a transaction's last valid block height
	// the finalized chain must be before a missing signature is called
	// failed. A healthy node may lag the cluster by up to 128 slots, and a
	// load-balanced public endpoint can answer two calls from two nodes; the
	// margin keeps a lagging node's "not found" from being read as "never
	// landed" — the one answer that lets the user sign the transfer again.
	expiryMargin = 150
	// maxExpiryRecords bounds the in-memory book. Dropping a record only
	// means that transaction stays pending instead of turning failed.
	maxExpiryRecords = 4096
)

// expiryBook remembers, per signed transaction, the last block height at
// which its blockhash is still valid.
type expiryBook struct {
	mu    sync.Mutex
	items map[string]uint64
}

func newExpiryBook() *expiryBook { return &expiryBook{items: make(map[string]uint64)} }

func expiryKey(networkID, signature string) string { return networkID + "\x00" + signature }

func (b *expiryBook) record(networkID, signature string, lastValid uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := expiryKey(networkID, signature)
	if _, exists := b.items[key]; !exists && len(b.items) >= maxExpiryRecords {
		for old := range b.items {
			delete(b.items, old)
			break
		}
	}
	b.items[key] = lastValid
}

func (b *expiryBook) lookup(networkID, signature string) (uint64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, ok := b.items[expiryKey(networkID, signature)]
	return value, ok
}

func (b *expiryBook) forget(networkID, signature string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.items, expiryKey(networkID, signature))
}

// RecordBlockhashExpiry remembers the last block height at which a signed
// transaction can still be included: the lastValidBlockHeight that came with
// its blockhash. Send records it before broadcasting; Transaction uses it to
// tell a transaction that can no longer land from one that has not landed
// yet. The record lives in memory, so after a restart such a transaction
// stays pending rather than turning failed.
func (a *Adapter) RecordBlockhashExpiry(networkID, signature string, lastValidBlockHeight uint64) {
	a.expiry.record(networkID, signature, lastValidBlockHeight)
}

// Transaction reports a signature's status at the confirmed commitment.
//
// confirmed — voted on by a supermajority — is what "confirmed" means for the
// other families too: in a block, one confirmation. The UI tracker stops at the
// first non-pending answer, and finalized would add ~13 s of waiting for a
// rollback that has never happened to a confirmed block.
//
// A transaction that executed with an error is failed, like a reverted EVM
// transaction. A signature no node knows is pending, unless its blockhash
// expiry was recorded and the finalized chain is past it by expiryMargin: then
// it can never be included and it is failed.
func (a *Adapter) Transaction(ctx context.Context, networkID, txID string) (chain.Transaction, error) {
	signature, err := solana.SignatureFromBase58(txID)
	if err != nil {
		return chain.Transaction{}, fmt.Errorf("%w: invalid Solana transaction signature", chain.ErrInvalidRequest)
	}
	_, nodes, err := a.client(ctx, networkID)
	if err != nil {
		return chain.Transaction{}, err
	}
	result := func(status string) (chain.Transaction, error) {
		if status != operation.StatusPending {
			a.expiry.forget(networkID, txID)
		}
		return chain.Transaction{NetworkID: networkID, Hash: txID, Status: status}, nil
	}
	found, status, err := signatureStatus(ctx, nodes, signature)
	if err != nil {
		return chain.Transaction{}, err
	}
	if found {
		return result(status)
	}
	lastValid, ok := a.expiry.lookup(networkID, txID)
	if !ok {
		return result(operation.StatusPending)
	}
	var height uint64
	err = nodes.do(ctx, "getBlockHeight", func(client *rpc.Client) error {
		var callErr error
		height, callErr = client.GetBlockHeight(ctx, rpc.CommitmentFinalized)
		return callErr
	})
	if err != nil {
		return chain.Transaction{}, fmt.Errorf("read Solana block height: %w", err)
	}
	if height <= lastValid+expiryMargin {
		return result(operation.StatusPending)
	}
	// Asked again after the height: had the transaction landed in time, it
	// would be in a finalized block by now, and this answer would show it.
	found, status, err = signatureStatus(ctx, nodes, signature)
	if err != nil {
		return chain.Transaction{}, err
	}
	if found {
		return result(status)
	}
	return result(operation.StatusFailed)
}

// signatureStatus asks for one signature, searching the ledger beyond the
// recent status cache.
func signatureStatus(ctx context.Context, nodes *nodes, signature solana.Signature) (bool, string, error) {
	var answer *rpc.GetSignatureStatusesResult
	err := nodes.do(ctx, "getSignatureStatuses", func(client *rpc.Client) error {
		var callErr error
		answer, callErr = client.GetSignatureStatuses(ctx, true, signature)
		if callErr == nil && len(answer.Value) != 1 {
			callErr = fmt.Errorf("answered %d statuses for one signature", len(answer.Value))
		}
		return callErr
	})
	if err != nil {
		return false, "", fmt.Errorf("read Solana transaction status: %w", err)
	}
	value := answer.Value[0]
	if value == nil {
		return false, "", nil
	}
	switch value.ConfirmationStatus {
	case rpc.ConfirmationStatusProcessed:
		// Executed on one fork only; its outcome, error included, is not
		// settled until the cluster votes on it.
		return true, operation.StatusPending, nil
	case rpc.ConfirmationStatusConfirmed, rpc.ConfirmationStatusFinalized:
		if value.Err != nil {
			return true, operation.StatusFailed, nil
		}
		return true, operation.StatusConfirmed, nil
	default:
		return false, "", fmt.Errorf(
			"read Solana transaction status: unknown confirmation status %q", value.ConfirmationStatus,
		)
	}
}
