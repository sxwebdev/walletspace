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

// Begin reserved the key trimmed and Update looked it up raw. net/textproto
// strips spaces and tabs from a header value but not a vertical tab, a form
// feed or a non-breaking space, so a key padded with one of those was reserved
// under one name and searched for under another — and the mismatch surfaced
// only after the transaction had been signed and broadcast: Update returned
// "operation not found", the txid was lost, the record stuck at building, and
// every retry got a permanent conflict.
func TestKeyNormalizationSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()

	store := operation.New(t.TempDir())
	spaceID := "spc_00000000000000000000000000"
	for _, padded := range []string{
		"  plain  ", "\vvertical\v", "\fform-feed\f", " nbsp ", "\ttab\n",
	} {
		created, existing, err := store.Begin(spaceID, padded, "hash-"+padded, "tron-mainnet")
		if err != nil {
			t.Fatalf("Begin(%q) error = %v", padded, err)
		}
		if existing {
			t.Fatalf("Begin(%q) reported an existing operation", padded)
		}
		updated, err := store.Update(spaceID, padded, "txid", operation.StatusPending)
		if err != nil {
			t.Fatalf("Update(%q) error = %v", padded, err)
		}
		if updated.Key != created.Key || updated.TxHash != "txid" {
			t.Errorf("Update(%q) = %+v, want the record Begin created", padded, updated)
		}
	}
}

// The transaction id is written before the signature, and a later failure must
// not erase it: it is the only thing that says which transaction may be on
// chain, and losing it is what turns a lost answer into a second transfer.
func TestUpdateKeepsAKnownTransactionID(t *testing.T) {
	t.Parallel()

	store := operation.New(t.TempDir())
	spaceID := "spc_00000000000000000000000000"
	if _, _, err := store.Begin(spaceID, "key", "hash", "tron-mainnet"); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if _, err := store.Update(spaceID, "key", "txid", operation.StatusBroadcasting); err != nil {
		t.Fatalf("Update(broadcasting) error = %v", err)
	}
	got, err := store.Update(spaceID, "key", "", operation.StatusBroadcastUnknown)
	if err != nil {
		t.Fatalf("Update(unknown) error = %v", err)
	}
	if got.TxHash != "txid" || got.Status != operation.StatusBroadcastUnknown {
		t.Errorf("Update() = %+v, want the recorded txid kept", got)
	}
}

// Only a status that proves the transaction never reached a node may lead to a
// second one being built.
func TestInFlightCoversEveryStatusThatMayExistOnChain(t *testing.T) {
	t.Parallel()

	for status, want := range map[string]bool{
		operation.StatusBuilding:         false,
		operation.StatusFailed:           false,
		operation.StatusBroadcasting:     true,
		operation.StatusBroadcastUnknown: true,
		operation.StatusPending:          true,
		operation.StatusConfirmed:        true,
	} {
		if got := operation.InFlight(status); got != want {
			t.Errorf("InFlight(%q) = %v, want %v", status, got, want)
		}
	}
}
