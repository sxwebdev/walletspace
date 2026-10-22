package operation_test

import (
	"github.com/sxwebdev/walletspace/internal/operation"
	"testing"
)

func TestGetDoesNotReserveAnOperation(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	store := operation.New(home)
	const space = "spc_0123456789abcdef0123456789abcdef"
	if _, found, err := store.Get(space, "refuel-123"); err != nil || found {
		t.Fatalf("missing operation = %t, %v", found, err)
	}
	if _, repeated, err := store.Begin(space, "refuel-123", "request", "ethereum-mainnet"); err != nil || repeated {
		t.Fatalf("lookup reserved the operation: %t, %v", repeated, err)
	}
	if _, err := store.Update(space, "refuel-123", "0xhash", operation.StatusBroadcasting); err != nil {
		t.Fatal(err)
	}
	got, found, err := operation.New(home).Get(space, " refuel-123 ")
	if err != nil || !found || got.TxHash != "0xhash" || got.Status != operation.StatusBroadcasting {
		t.Fatalf("reloaded operation = %+v, %t, %v", got, found, err)
	}
}
