package evm_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sxwebdev/walletspace/internal/chain"
	evmchain "github.com/sxwebdev/walletspace/internal/chain/evm"
	"github.com/sxwebdev/walletspace/internal/network"
)

type messageKey struct{}

func (messageKey) Family() chain.Family { return chain.FamilyEVM }
func (messageKey) PublicKey() []byte    { return make([]byte, 32) }
func (messageKey) SignMessage(context.Context, []byte) ([]byte, error) {
	return make([]byte, 64), nil
}

// EVM transactions are signed as digests: a message signer is refused before
// anything is built or asked of a node.
func TestSendRefusesAMessageSigner(t *testing.T) {
	t.Parallel()

	registry, err := network.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := evmchain.New(registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	_, err = adapter.Send(t.Context(), "ethereum-mainnet", chain.TransferRequest{}, messageKey{})
	if !errors.Is(err, chain.ErrSignerMismatch) {
		t.Errorf("Send(message signer) error = %v, want ErrSignerMismatch", err)
	}
}
