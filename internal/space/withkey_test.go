package space_test

import (
	"errors"
	"testing"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/chain"
)

// WithKey lends the kind of signer the family's curve calls for: a digest
// signer for secp256k1, a message signer for ed25519 — never the other.
func TestWithKeyPicksTheSignerByCurve(t *testing.T) {
	t.Parallel()

	manager := newManager(t, t.TempDir())
	created := createWithNileWallet(t, manager, "test-vault-password")
	id, tronID := created.Space.ID, created.Accounts[0].ID
	solana, err := manager.Derive(id, "solana-devnet", account.FamilySolana, "")
	if err != nil {
		t.Fatalf("Derive() error = %v", err)
	}

	var kinds []string
	record := func(key chain.Key) error {
		_, digest := key.(chain.DigestSigner)
		_, message := key.(chain.MessageSigner)
		switch {
		case digest && !message:
			kinds = append(kinds, "digest")
		case message && !digest:
			kinds = append(kinds, "message")
		default:
			kinds = append(kinds, "both or neither")
		}
		return nil
	}
	if err := manager.WithKey(t.Context(), id, tronID, "tron-nile", account.FamilyTron, record); err != nil {
		t.Fatalf("WithKey(tron) error = %v", err)
	}
	if err := manager.WithKey(t.Context(), id, solana.ID, "solana-devnet", account.FamilySolana, func(key chain.Key) error {
		if got := key.Family(); got != chain.FamilySolana {
			t.Errorf("Family() = %s", got)
		}
		return record(key)
	}); err != nil {
		t.Fatalf("WithKey(solana) error = %v", err)
	}
	if len(kinds) != 2 || kinds[0] != "digest" || kinds[1] != "message" {
		t.Errorf("signers = %v, want a digest signer for Tron and a message signer for Solana", kinds)
	}

	if err := manager.WithKey(t.Context(), id, tronID, "tron-nile", account.Family("btc"), record); !errors.Is(err, account.ErrUnsupportedFamily) {
		t.Errorf("WithKey(btc) error = %v, want ErrUnsupportedFamily", err)
	}
	if err := manager.WithKey(t.Context(), id, tronID, "tron-nile", account.FamilyTron, nil); err == nil {
		t.Error("WithKey(nil callback) succeeded")
	}
	failure := errors.New("callback failed")
	if err := manager.WithKey(t.Context(), id, solana.ID, "solana-devnet", account.FamilySolana, func(chain.Key) error {
		return failure
	}); !errors.Is(err, failure) {
		t.Errorf("WithKey() error = %v, want the callback's", err)
	}
}
