package operation

// The lifecycle of a signed operation. These used to be string literals spread
// across the HTTP layer, which is how the Tron paths came to report a lost
// broadcast as StatusFailed — the one status that tells the caller it is safe to
// build and sign the transaction again.
const (
	// StatusBuilding is set when the idempotency key is reserved, before
	// anything is signed. Nothing has left the process yet.
	StatusBuilding = "building"
	// StatusBroadcasting means the transaction is signed, its id is known, and
	// it is on its way to a node. A record left in this state is one the wallet
	// did not outlive; the transaction may or may not be on chain.
	StatusBroadcasting = "broadcasting"
	// StatusPending means a node accepted the transaction.
	StatusPending = "pending"
	// StatusConfirmed means it is in a block.
	StatusConfirmed = "confirmed"
	// StatusFailed is reserved for operations that provably never reached the
	// chain — the node answered with a rejection, or the failure happened before
	// the signature. Only these are safe to retry with a new transaction.
	StatusFailed = "failed"
	// StatusBroadcastUnknown means the transaction was sent and the answer was
	// lost. It may be on chain. Building a second transaction for the same
	// intent is exactly what must not happen here.
	StatusBroadcastUnknown = "broadcast_unknown"
)

// InFlight reports whether a transaction with this status may exist on chain.
// A new transaction must not be built for the same intent while it does.
func InFlight(status string) bool {
	switch status {
	case StatusBroadcasting, StatusBroadcastUnknown, StatusPending, StatusConfirmed:
		return true
	default:
		return false
	}
}
