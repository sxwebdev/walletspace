package operation_test

import (
	"testing"

	"github.com/sxwebdev/walletspace/internal/operation"
)

// An expiring transaction's id and height are written together and kept
// through later updates and a fresh store over the same home, as after a
// restart.
func TestExpiryIsKeptWithItsTransaction(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	store := operation.New(home)
	const spaceID = "spc_0123456789abcdef0123456789abcdef"
	if _, _, err := store.Begin(spaceID, "key-1", "hash", "solana-mainnet"); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if _, err := store.UpdateExpiring(spaceID, "key-1", "sig-1", operation.StatusBroadcasting, 0); err == nil {
		t.Error("UpdateExpiring() without a height succeeded")
	}
	if _, err := store.UpdateExpiring(spaceID, "key-1", "", operation.StatusBroadcasting, 9); err == nil {
		t.Error("UpdateExpiring() without a transaction id succeeded")
	}
	if got, ok, _ := store.Get(spaceID, "key-1"); !ok || got.TxHash != "" || got.LastValidBlockHeight != 0 {
		t.Errorf("record after refused writes = %+v, want it untouched", got)
	}
	if _, err := store.UpdateExpiring(spaceID, "missing", "sig-1", operation.StatusBroadcasting, 9); err == nil {
		t.Error("UpdateExpiring() for an unknown operation succeeded")
	}
	written, err := store.UpdateExpiring(spaceID, " key-1 ", "sig-1", operation.StatusBroadcasting, 123)
	if err != nil || written.TxHash != "sig-1" || written.Status != operation.StatusBroadcasting || written.LastValidBlockHeight != 123 {
		t.Fatalf("UpdateExpiring() = %+v, %v", written, err)
	}
	if _, err := store.Update(spaceID, "key-1", "", operation.StatusPending); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	restarted := operation.New(home)
	found, ok, err := restarted.FindByTxHash(spaceID, "sig-1")
	if err != nil || !ok || found.LastValidBlockHeight != 123 || found.Status != operation.StatusPending {
		t.Errorf("FindByTxHash() = %+v, %v, %v, want the height kept through later updates", found, ok, err)
	}
	if _, ok, err := restarted.FindByTxHash(spaceID, "sig-9"); ok || err != nil {
		t.Errorf("FindByTxHash(unknown) = %v, %v", ok, err)
	}
	if _, ok, err := restarted.FindByTxHash(spaceID, ""); ok || err != nil {
		t.Errorf("FindByTxHash(empty) = %v, %v", ok, err)
	}
	if _, _, err := restarted.FindByTxHash("bad", "sig-1"); err == nil {
		t.Error("FindByTxHash() with an invalid space succeeded")
	}
}
