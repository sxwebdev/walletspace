package operation_test

import (
	"errors"
	"testing"

	"github.com/sxwebdev/walletspace/internal/operation"
)

func TestIdempotencyContract(t *testing.T) {
	t.Parallel()

	store := operation.New(t.TempDir())
	hash, err := operation.RequestHash(struct{ Amount string }{Amount: "1"})
	if err != nil {
		t.Fatalf("RequestHash() error = %v", err)
	}
	first, existing, err := store.Begin("spc_abc123", "request-1", hash, "ethereum-mainnet")
	if err != nil || existing {
		t.Fatalf("Begin() = %+v, %v, %v", first, existing, err)
	}
	same, existing, err := store.Begin("spc_abc123", "request-1", hash, "ethereum-mainnet")
	if err != nil || !existing || same != first {
		t.Fatalf("repeated Begin() = %+v, %v, %v", same, existing, err)
	}
	if _, _, err := store.Begin("spc_abc123", "request-1", "different", "ethereum-mainnet"); !errors.Is(err, operation.ErrConflict) {
		t.Fatalf("conflicting Begin() error = %v", err)
	}
	updated, err := store.Update("spc_abc123", "request-1", "0x1234", "pending")
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated.TxHash != "0x1234" || updated.Status != "pending" {
		t.Errorf("updated = %+v", updated)
	}
	found, err := store.UpdateByTxHash("spc_abc123", "0x1234", "confirmed")
	if err != nil || !found {
		t.Fatalf("UpdateByTxHash() = %v, %v", found, err)
	}
	replayed, existing, err := store.Begin("spc_abc123", "request-1", hash, "ethereum-mainnet")
	if err != nil || !existing {
		t.Fatalf("Begin(replay) = %+v, %v, %v", replayed, existing, err)
	}
	if replayed.Status != "confirmed" {
		t.Errorf("replayed status = %q, want confirmed", replayed.Status)
	}
}

func TestUpdatesRejectInvalidSpaceID(t *testing.T) {
	t.Parallel()

	store := operation.New(t.TempDir())
	if _, err := store.Update("../outside", "request-1", "0x1234", "pending"); err == nil {
		t.Error("Update() accepted an invalid space id")
	}
	if _, err := store.UpdateByTxHash("../outside", "0x1234", "confirmed"); err == nil {
		t.Error("UpdateByTxHash() accepted an invalid space id")
	}
}
