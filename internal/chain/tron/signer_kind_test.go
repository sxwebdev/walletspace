package tron_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/sxwebdev/walletspace/internal/chain"
	tronchain "github.com/sxwebdev/walletspace/internal/chain/tron"
	"github.com/sxwebdev/walletspace/internal/network"
)

type messageKey struct{}

func (messageKey) Family() chain.Family { return chain.FamilyTron }
func (messageKey) PublicKey() []byte    { return make([]byte, 32) }
func (messageKey) SignMessage(context.Context, []byte) ([]byte, error) {
	return make([]byte, 64), nil
}

// Tron transactions are signed as digests: a message signer is refused before
// any node is asked anything.
func TestSendRefusesAMessageSigner(t *testing.T) {
	t.Parallel()

	registry, err := network.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := tronchain.New(t.Context(), registry, nil, endpointProvider{url: "http://127.0.0.1:1"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	_, err = adapter.Send(t.Context(), "tron-nile", chain.TransferRequest{}, messageKey{})
	if !errors.Is(err, chain.ErrSignerMismatch) {
		t.Errorf("Send(message signer) error = %v, want ErrSignerMismatch", err)
	}
}
