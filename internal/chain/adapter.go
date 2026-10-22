package chain

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"net/http"
	"slices"

	"github.com/sxwebdev/walletspace/internal/network"
)

// ErrUnsupportedFamily reports a network whose family no adapter serves. It is
// what every family dispatch answers instead of handing the network to
// whichever adapter happened to be the old fallback.
var ErrUnsupportedFamily = errors.New("unsupported chain family")

// Adapter is what every chain family offers the parts of the wallet that do not
// care which chain they talk to: balances, plain transfers, transaction status,
// token lookup and endpoint checks.
//
// Features only one family has — Tron resources, staking, deployment and swaps,
// EVM wrapping and gas funding — stay on the concrete adapters and are reached
// through their own types, behind an explicit family check.
type Adapter interface {
	// BalanceStream reads every asset of every holder and closes the channel
	// when done. refresh asks an adapter that caches balances to read past its
	// cache; an adapter that reads live every time ignores it.
	BalanceStream(
		ctx context.Context, networkID string, accounts []AccountAddress, assets []Asset, refresh bool,
	) <-chan Balance
	EstimateTransfer(ctx context.Context, networkID string, req TransferRequest) (TransferEstimate, error)
	// EstimateMaxTransfer returns the largest amount that can be sent now, fee
	// included.
	EstimateMaxTransfer(ctx context.Context, networkID string, req TransferRequest) (TransferEstimate, error)
	// Send signs with whatever kind of key the family uses: an adapter
	// asserts the signer interface it needs (DigestSigner, MessageSigner) and
	// answers ErrSignerMismatch for any other.
	Send(ctx context.Context, networkID string, req TransferRequest, signer Key) (Transaction, error)
	Transaction(ctx context.Context, networkID, txID string) (Transaction, error)
	// TokenKind is the asset kind this family's tokens are stored under.
	TokenKind() string
	TokenMetadata(ctx context.Context, networkID, contract string) (symbol string, decimals uint8, err error)
	// VerifyEndpoint checks that an endpoint serves this network, cheaply
	// enough to run when a user saves it.
	VerifyEndpoint(ctx context.Context, item network.Network, endpoint string, headers http.Header) error
	// ProbeEndpoint is the Node Doctor's periodic check of an endpoint: at
	// least everything VerifyEndpoint proves. What else it checks is the
	// family's choice — Tron also refuses a stale head block, EVM checks
	// nothing beyond VerifyEndpoint.
	ProbeEndpoint(ctx context.Context, item network.Network, endpoint string, headers http.Header) error
	// Invalidate drops whatever the adapter cached for a network, so the next
	// call is built from the current settings.
	Invalidate(networkID string)
}

// BlockhashExpiryRecorder is implemented by an adapter whose transactions
// expire at a block height. The caller hands it the height it persisted with
// the operation, so a transaction that can no longer land is reported failed
// after a restart too.
type BlockhashExpiryRecorder interface {
	RecordBlockhashExpiry(networkID, txID string, lastValidBlockHeight uint64)
}

// Adapters is the family-keyed set of chain adapters. A family that is not in
// the set is refused with ErrUnsupportedFamily; there is no default adapter.
type Adapters struct {
	byFamily map[network.Family]Adapter
}

// NewAdapters builds the set. Every entry needs a family and an adapter: a nil
// one would turn a configuration mistake into a panic on the first request.
func NewAdapters(adapters map[network.Family]Adapter) (*Adapters, error) {
	byFamily := make(map[network.Family]Adapter, len(adapters))
	for family, adapter := range adapters {
		if family == "" || adapter == nil {
			return nil, fmt.Errorf("chain adapter for family %q is missing", family)
		}
		byFamily[family] = adapter
	}
	return &Adapters{byFamily: byFamily}, nil
}

// For returns the adapter that serves a family.
func (a *Adapters) For(family network.Family) (Adapter, error) {
	adapter, ok := a.byFamily[family]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedFamily, family)
	}
	return adapter, nil
}

// All yields every adapter, ordered by family.
func (a *Adapters) All() iter.Seq2[network.Family, Adapter] {
	return func(yield func(network.Family, Adapter) bool) {
		for _, family := range slices.Sorted(maps.Keys(a.byFamily)) {
			if !yield(family, a.byFamily[family]) {
				return
			}
		}
	}
}
