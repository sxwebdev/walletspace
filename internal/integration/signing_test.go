package integration

import (
	"net/http"
	"strings"
	"sync"
	"testing"
)

// transfer is the request body the UI sends, with the fee the user agreed to.
func transfer(accountID string, approval map[string]any) map[string]any {
	body := map[string]any{
		"account_id": accountID,
		"asset_id":   nativeAsset,
		"to":         burnAddress,
		"amount":     "0.01",
	}
	for name, value := range approval {
		body[name] = value
	}
	return body
}

// fundedSpace is a space with an Ethereum wallet, pointed at the given node.
func fundedSpace(t *testing.T, node *fakeNode) (*wallet, string, string) {
	t.Helper()
	wallet := start(t)
	spaceID := wallet.createSpace()
	accountID := wallet.deriveEVMAccount(spaceID)
	wallet.useNode(node.url)
	wallet.confirmSend(spaceID)
	return wallet, spaceID, accountID
}

// An unlocked space is a statement about who was at the keyboard when it was
// opened, and nothing about who is asking now. Everything that hands over
// lasting control of the funds asks for the password again; until this, moving
// them did not — so a script injected into the page could not steal the seed
// but could spend everything it protects.
func TestSpendingNeedsThePasswordAndTheWindowDiesWithTheSession(t *testing.T) {
	t.Parallel()

	node := newFakeNode(t, nil)
	wallet := start(t)
	spaceID := wallet.createSpace()
	accountID := wallet.deriveEVMAccount(spaceID)
	wallet.useNode(node.url)
	base := "/api/spaces/" + spaceID + "/networks/" + ethereum

	estimated := wallet.call(http.MethodPost, base+"/transfers/estimate", transfer(accountID, nil))
	if estimated.status != http.StatusOK {
		t.Fatalf("estimate = %d %s", estimated.status, estimated.text())
	}
	shown := estimated.json(t)
	body := transfer(accountID, map[string]any{
		"fee_model":                shown["fee_model"],
		"gas_limit":                shown["gas_limit"],
		"max_fee_per_gas":          shown["max_fee_per_gas"],
		"max_priority_fee_per_gas": shown["max_priority_fee_per_gas"],
	})

	// Pricing a transfer is a read and stays open; signing one is not.
	unconfirmed := wallet.call(http.MethodPost, base+"/transfers", body,
		header("Idempotency-Key", "step-up-1"))
	if unconfirmed.status != http.StatusForbidden {
		t.Fatalf("send without a confirmation = %d %s, want 403", unconfirmed.status, unconfirmed.text())
	}
	if code := unconfirmed.json(t)["code"]; code != "send_confirmation_required" {
		t.Errorf("code = %v, want send_confirmation_required so the UI can prompt", code)
	}
	if node.broadcasts() != 0 {
		t.Fatalf("%d transactions were broadcast before anyone confirmed", node.broadcasts())
	}

	// A wrong password buys nothing, and the window only opens on the right one.
	refused := wallet.call(http.MethodPost, "/api/spaces/"+spaceID+"/confirm-send",
		map[string]any{"password": "not-the-password"})
	if refused.status == http.StatusOK {
		t.Fatal("the spending window opened on a wrong password")
	}
	wallet.confirmSend(spaceID)

	// Same request, same idempotency key: the confirmation authorises what was
	// already asked for rather than starting something new.
	sent := wallet.call(http.MethodPost, base+"/transfers", body,
		header("Idempotency-Key", "step-up-1"))
	if sent.status != http.StatusAccepted {
		t.Fatalf("send after confirming = %d %s, want 202", sent.status, sent.text())
	}

	// Locking the space takes the window with it. A grant that outlived the
	// session would be waiting for whoever unlocks the space next.
	if locked := wallet.call(http.MethodPost, "/api/spaces/"+spaceID+"/lock", map[string]any{}); locked.status != http.StatusOK {
		t.Fatalf("lock = %d %s", locked.status, locked.text())
	}
	if unlocked := wallet.call(http.MethodPost, "/api/spaces/"+spaceID+"/unlock",
		map[string]any{"password": spacePassword}); unlocked.status != http.StatusOK {
		t.Fatalf("unlock = %d %s", unlocked.status, unlocked.text())
	}
	after := wallet.call(http.MethodPost, base+"/transfers", body,
		header("Idempotency-Key", "step-up-2"))
	if after.status != http.StatusForbidden {
		t.Fatalf("send after re-unlocking = %d %s, want 403 — the window should not survive a lock",
			after.status, after.text())
	}
}

// The step-up is a setting, and a setting is only as good as the path that
// carries it. This drives that path from the file the wallet reads at start-up
// rather than from the manager's own default, which is what every other test
// here happens to exercise.
func TestTheSpendingStepUpFollowsTheConfigFile(t *testing.T) {
	t.Parallel()

	const withoutTheStepUp = `schema_version: 1
server:
  addr: 127.0.0.1:0
  open_browser: false
security:
  auto_lock: 15m0s
  confirm_sends: false
  send_grant_ttl: 5m0s
node_discovery:
  enabled: false
  url: ""
  refresh_interval: 30m0s
  request_timeout: 5s
  allow_insecure_rpc: true
ui:
  last_space_id: ""
`

	node := newFakeNode(t, nil)
	wallet := startWithConfig(t, withoutTheStepUp)
	spaceID := wallet.createSpace()
	accountID := wallet.deriveEVMAccount(spaceID)
	wallet.useNode(node.url)
	base := "/api/spaces/" + spaceID + "/networks/" + ethereum

	estimated := wallet.call(http.MethodPost, base+"/transfers/estimate", transfer(accountID, nil))
	if estimated.status != http.StatusOK {
		t.Fatalf("estimate = %d %s", estimated.status, estimated.text())
	}
	shown := estimated.json(t)
	sent := wallet.call(http.MethodPost, base+"/transfers", transfer(accountID, map[string]any{
		"fee_model":                shown["fee_model"],
		"gas_limit":                shown["gas_limit"],
		"max_fee_per_gas":          shown["max_fee_per_gas"],
		"max_priority_fee_per_gas": shown["max_priority_fee_per_gas"],
	}), header("Idempotency-Key", "config-off-1"))

	// Turned off in the file, nothing is asked. If this returns 403 the setting
	// is not reaching the manager; if every other test in this file still passes
	// while it does, they are testing a default rather than the wiring.
	if sent.status != http.StatusAccepted {
		t.Fatalf("send with the step-up switched off in config = %d %s, want 202",
			sent.status, sent.text())
	}
	if node.broadcasts() != 1 {
		t.Errorf("the node received %d transactions, want 1", node.broadcasts())
	}
}

// SEC-04, end to end. The node quotes one gwei while the confirmation screen is
// being drawn and a hundred once the user has pressed the button. The value on
// the screen is the value that may be signed, so the second answer has to end
// the transfer rather than change it.
func TestAFeeRaisedAfterTheConfirmationIsNeverSigned(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	quoted := false
	node := newFakeNode(t, func(n *fakeNode, call rpcCall, w http.ResponseWriter) bool {
		if call.Method != "eth_gasPrice" {
			return false
		}
		mu.Lock()
		defer mu.Unlock()
		if !quoted {
			quoted = true
			n.answer(w, call, "0x3b9aca00", nil) // 1 gwei, for the screen
			return true
		}
		n.answer(w, call, "0x174876e800", nil) // 100 gwei, for the signature
		return true
	})

	wallet, spaceID, accountID := fundedSpace(t, node)
	base := "/api/spaces/" + spaceID + "/networks/" + ethereum

	estimated := wallet.call(http.MethodPost, base+"/transfers/estimate",
		transfer(accountID, nil))
	if estimated.status != http.StatusOK {
		t.Fatalf("estimate = %d %s", estimated.status, estimated.text())
	}
	shown := estimated.json(t)
	if shown["max_fee_per_gas"] != "1000000000" {
		t.Fatalf("the screen was quoted %v wei per gas, want the first answer", shown["max_fee_per_gas"])
	}

	sent := wallet.call(http.MethodPost, base+"/transfers", transfer(accountID, map[string]any{
		"fee_model":                shown["fee_model"],
		"gas_limit":                shown["gas_limit"],
		"max_fee_per_gas":          shown["max_fee_per_gas"],
		"max_priority_fee_per_gas": shown["max_priority_fee_per_gas"],
	}), header("Idempotency-Key", "fee-swing-1"))

	if sent.status == http.StatusAccepted {
		t.Fatalf("the transfer was signed at a fee nobody approved: %s", sent.text())
	}
	if !strings.Contains(sent.text(), "approved") {
		t.Errorf("error = %s, want it to name the approval", sent.text())
	}
	if node.broadcasts() != 0 {
		t.Errorf("%d transactions were broadcast, want none", node.broadcasts())
	}
}

// SEC-06, end to end and on the path where it matters most: the node takes the
// transaction and the answer never arrives. The transfer may be on chain, so a
// repeat of the same request must not build and sign a second one — it has to
// come back with the id of the first.
func TestARetryAfterALostBroadcastDoesNotSignTwice(t *testing.T) {
	t.Parallel()

	node := newFakeNode(t, func(_ *fakeNode, call rpcCall, w http.ResponseWriter) bool {
		if call.Method != "eth_sendRawTransaction" {
			return false
		}
		// Taken and then silence: the connection closes with no reply, which is
		// the shape of a timeout, a reset, or a node that crashed holding a
		// perfectly valid transaction.
		if hijacker, ok := w.(http.Hijacker); ok {
			connection, _, err := hijacker.Hijack()
			if err == nil {
				_ = connection.Close()
			}
		}
		return true
	})

	wallet, spaceID, accountID := fundedSpace(t, node)
	base := "/api/spaces/" + spaceID + "/networks/" + ethereum

	estimated := wallet.call(http.MethodPost, base+"/transfers/estimate", transfer(accountID, nil))
	if estimated.status != http.StatusOK {
		t.Fatalf("estimate = %d %s", estimated.status, estimated.text())
	}
	shown := estimated.json(t)
	body := transfer(accountID, map[string]any{
		"fee_model":                shown["fee_model"],
		"gas_limit":                shown["gas_limit"],
		"max_fee_per_gas":          shown["max_fee_per_gas"],
		"max_priority_fee_per_gas": shown["max_priority_fee_per_gas"],
	})

	first := wallet.call(http.MethodPost, base+"/transfers", body,
		header("Idempotency-Key", "lost-answer-1"))
	if first.status != http.StatusAccepted {
		t.Fatalf("send = %d %s, want 202 with an unknown outcome", first.status, first.text())
	}
	outcome := first.json(t)
	if outcome["status"] != "broadcast_unknown" {
		t.Fatalf("status = %v, want broadcast_unknown — failed is what invites a second transfer", outcome["status"])
	}
	hash, _ := outcome["tx_hash"].(string)
	if len(hash) != 66 {
		t.Fatalf("tx_hash = %q, want the hash computed before the send", hash)
	}

	// The user presses send again. The UI keeps the idempotency key precisely so
	// that this is a question about the first transfer rather than a new one.
	second := wallet.call(http.MethodPost, base+"/transfers", body,
		header("Idempotency-Key", "lost-answer-1"))
	if second.status != http.StatusOK {
		t.Fatalf("retry = %d %s, want 200 describing the operation already in flight",
			second.status, second.text())
	}
	if again := second.json(t); again["tx_hash"] != hash {
		t.Errorf("retry reported tx_hash %v, want the original %s", again["tx_hash"], hash)
	}
	// Both counts, because either alone can be satisfied by the wrong thing: the
	// distinct count collapses two identical rebuilds into one, and the raw
	// count cannot tell a re-send of the same bytes from a second transfer. The
	// fake node advances its nonce per transaction received, so a rebuild after
	// the lost answer would be visibly different bytes.
	if got := node.broadcasts(); got != 1 {
		t.Errorf("the node received %d transactions, want 1", got)
	}
	if got := node.distinctBroadcasts(); got != 1 {
		t.Errorf("%d different transactions reached the node, want 1", got)
	}
}

// The other half of the same rule: an operation the node explicitly turned down
// never reached the chain, so a retry with a fresh key is the right advice and
// has to be given.
func TestARejectedTransferMayBeRetried(t *testing.T) {
	t.Parallel()

	node := newFakeNode(t, func(n *fakeNode, call rpcCall, w http.ResponseWriter) bool {
		if call.Method != "eth_sendRawTransaction" {
			return false
		}
		n.answer(w, call, nil, errInsufficientFunds{})
		return true
	})

	wallet, spaceID, accountID := fundedSpace(t, node)
	base := "/api/spaces/" + spaceID + "/networks/" + ethereum

	estimated := wallet.call(http.MethodPost, base+"/transfers/estimate", transfer(accountID, nil))
	shown := estimated.json(t)
	body := transfer(accountID, map[string]any{
		"fee_model":                shown["fee_model"],
		"gas_limit":                shown["gas_limit"],
		"max_fee_per_gas":          shown["max_fee_per_gas"],
		"max_priority_fee_per_gas": shown["max_priority_fee_per_gas"],
	})

	refused := wallet.call(http.MethodPost, base+"/transfers", body,
		header("Idempotency-Key", "rejected-1"))
	if refused.status == http.StatusAccepted {
		t.Fatalf("a rejected broadcast was reported as accepted: %s", refused.text())
	}
	// The premise of the test: the node answered and said no. Without this, any
	// failure before the first RPC call — a bad address, a locked space, a
	// refused fee — produces the same status and the same advice, and the branch
	// that decides whether re-signing is safe is never exercised.
	if got := node.broadcasts(); got != 1 {
		t.Fatalf("the node received %d transactions, want the one it rejected", got)
	}

	// Same key, so this is still the first operation: it failed before the
	// chain saw it, and the caller is told a new key is what to use.
	repeat := wallet.call(http.MethodPost, base+"/transfers", body,
		header("Idempotency-Key", "rejected-1"))
	if repeat.status != http.StatusConflict {
		t.Fatalf("repeat of a failed send = %d %s, want 409", repeat.status, repeat.text())
	}
	if !strings.Contains(repeat.text(), "new idempotency key") {
		t.Errorf("error = %s, want the advice to retry with a new key", repeat.text())
	}
}

// errInsufficientFunds is a node saying no, as opposed to a node saying
// nothing. The distinction is the whole of what decides whether a retry is safe.
type errInsufficientFunds struct{}

func (errInsufficientFunds) Error() string { return "insufficient funds for gas * price + value" }
