package operation

import (
	"fmt"
	"testing"
	"time"
)

// Every send, stake and deployment added a record and nothing ever removed one,
// so the file grew for the life of the space — and it is read and rewritten in
// full on every operation, so the cost was paid on each of them.
func TestPruneKeepsTheFileBoundedWithoutForgettingLiveWork(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	operations := make(map[string]Operation, maxOperations+8)
	live := make(map[string]struct{})
	for i := range maxOperations + 8 {
		key := fmt.Sprintf("key-%04d", i)
		item := Operation{
			Key: key, Status: StatusConfirmed,
			UpdatedAt: now.Add(-time.Duration(maxOperations-i) * time.Minute),
		}
		if i%100 == 0 {
			// Recent and unresolved: a replay is recognised by this record, and
			// dropping it would let the same intent be signed a second time.
			item.Status = StatusBroadcastUnknown
			item.UpdatedAt = now.Add(-time.Minute)
			live[key] = struct{}{}
		}
		operations[key] = item
	}

	if err := prune(operations, now); err != nil {
		t.Fatalf("prune() error = %v", err)
	}
	if len(operations) >= maxOperations {
		t.Errorf("prune() left %d records, want fewer than %d", len(operations), maxOperations)
	}
	for key := range live {
		if _, ok := operations[key]; !ok {
			t.Errorf("prune() dropped the unresolved record %s", key)
		}
	}
	// Oldest first: what survives among the resolved ones is the recent history.
	if _, ok := operations["key-0001"]; ok {
		t.Error("prune() kept the oldest resolved record while dropping newer ones")
	}
	if _, ok := operations[fmt.Sprintf("key-%04d", maxOperations+7)]; !ok {
		t.Error("prune() dropped the newest record")
	}
}

// An unresolved record is not kept for ever: after the retention window a
// transaction that was never resolved is stale, not pending.
func TestPruneEventuallyDropsStaleUnresolvedRecords(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	operations := make(map[string]Operation, maxOperations)
	for i := range maxOperations {
		key := fmt.Sprintf("key-%04d", i)
		operations[key] = Operation{
			Key: key, Status: StatusBroadcasting, UpdatedAt: now.Add(-2 * retention),
		}
	}
	if err := prune(operations, now); err != nil {
		t.Fatalf("prune() error = %v", err)
	}
	if len(operations) >= maxOperations {
		t.Errorf("prune() left %d stale records", len(operations))
	}
}

// When every record is both recent and unresolved there is nothing safe to
// drop, and the new operation is refused rather than one in flight forgotten.
func TestPruneRefusesRatherThanForgetAnInFlightTransaction(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	operations := make(map[string]Operation, maxOperations)
	for i := range maxOperations {
		key := fmt.Sprintf("key-%04d", i)
		operations[key] = Operation{
			Key: key, Status: StatusPending, UpdatedAt: now.Add(-time.Minute),
		}
	}
	if err := prune(operations, now); err == nil {
		t.Fatal("prune() = nil, want a refusal")
	}
	if len(operations) != maxOperations {
		t.Errorf("prune() dropped %d in-flight records", maxOperations-len(operations))
	}
}
