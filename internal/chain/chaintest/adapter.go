// Package chaintest provides a recording chain.Adapter for tests of the code
// that dispatches between chain families.
package chaintest

import (
	"context"
	"net/http"
	"sync"

	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/network"
)

var _ chain.Adapter = (*Adapter)(nil)

// Adapter answers every call with a harmless success and records which method
// was called for which network. It touches no network: a test asserts on
// Calls to prove which family's adapter a request reached, or that it reached
// none.
type Adapter struct {
	// Kind is what TokenKind reports.
	Kind string

	mu      sync.Mutex
	calls   []Call
	headers []http.Header
}

// Call is one recorded adapter call.
type Call struct {
	Method    string
	NetworkID string
}

// Calls returns every call so far, in order.
func (a *Adapter) Calls() []Call {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Call(nil), a.calls...)
}

// Headers returns the headers each VerifyEndpoint and ProbeEndpoint call
// received, in call order.
func (a *Adapter) Headers() []http.Header {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]http.Header, 0, len(a.headers))
	for _, headers := range a.headers {
		out = append(out, headers.Clone())
	}
	return out
}

func (a *Adapter) recordHeaders(headers http.Header) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.headers = append(a.headers, headers.Clone())
}

func (a *Adapter) record(method, networkID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, Call{Method: method, NetworkID: networkID})
}

func (a *Adapter) BalanceStream(
	_ context.Context, networkID string, _ []chain.AccountAddress, _ []chain.Asset, _ bool,
) <-chan chain.Balance {
	a.record("BalanceStream", networkID)
	out := make(chan chain.Balance)
	close(out)
	return out
}

func (a *Adapter) EstimateTransfer(
	_ context.Context, networkID string, req chain.TransferRequest,
) (chain.TransferEstimate, error) {
	a.record("EstimateTransfer", networkID)
	return chain.TransferEstimate{NetworkID: networkID, Amount: req.Amount, Fee: "0"}, nil
}

func (a *Adapter) EstimateMaxTransfer(
	_ context.Context, networkID string, _ chain.TransferRequest,
) (chain.TransferEstimate, error) {
	a.record("EstimateMaxTransfer", networkID)
	return chain.TransferEstimate{NetworkID: networkID, Amount: "0", Fee: "0"}, nil
}

func (a *Adapter) Send(
	_ context.Context, networkID string, _ chain.TransferRequest, _ chain.Key,
) (chain.Transaction, error) {
	a.record("Send", networkID)
	return chain.Transaction{NetworkID: networkID, Hash: "fake", Status: "pending"}, nil
}

func (a *Adapter) Transaction(_ context.Context, networkID, txID string) (chain.Transaction, error) {
	a.record("Transaction", networkID)
	return chain.Transaction{NetworkID: networkID, Hash: txID, Status: "pending"}, nil
}

// TokenKind is a property, not a call, and is not recorded.
func (a *Adapter) TokenKind() string { return a.Kind }

func (a *Adapter) TokenMetadata(_ context.Context, networkID, _ string) (string, uint8, error) {
	a.record("TokenMetadata", networkID)
	return "FAKE", 6, nil
}

func (a *Adapter) VerifyEndpoint(_ context.Context, item network.Network, _ string, headers http.Header) error {
	a.record("VerifyEndpoint", item.ID)
	a.recordHeaders(headers)
	return nil
}

func (a *Adapter) ProbeEndpoint(_ context.Context, item network.Network, _ string, headers http.Header) error {
	a.record("ProbeEndpoint", item.ID)
	a.recordHeaders(headers)
	return nil
}

func (a *Adapter) Invalidate(networkID string) {
	a.record("Invalidate", networkID)
}
