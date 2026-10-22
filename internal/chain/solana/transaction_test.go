package solana_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain"
)

var signature = func() string {
	var raw [64]byte
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	return solana.SignatureFromBytes(raw[:]).String()
}()

func status(confirmation string, failed bool) map[string]any {
	value := map[string]any{"slot": 5, "confirmations": nil, "err": nil, "confirmationStatus": confirmation, "status": map[string]any{"Ok": nil}}
	if failed {
		value["err"] = map[string]any{"InstructionError": []any{0, "Custom"}}
		value["status"] = map[string]any{"Err": value["err"]}
	}
	return value
}

// statusNode answers getSignatureStatuses from a queue: each call takes the
// next answer, the last one repeats.
func statusNode(t *testing.T, answers ...reply) *node {
	t.Helper()
	n := newNode(t, mainnetGenesis)
	n.handle("getSignatureStatuses", func(params []json.RawMessage) reply {
		var signatures []string
		var opts struct {
			Search bool `json:"searchTransactionHistory"`
		}
		if len(params) != 2 || json.Unmarshal(params[0], &signatures) != nil || json.Unmarshal(params[1], &opts) != nil ||
			len(signatures) != 1 || signatures[0] != signature || !opts.Search {
			t.Errorf("getSignatureStatuses params = %s", params)
		}
		answer := answers[0]
		if len(answers) > 1 {
			answers = answers[1:]
		}
		return answer
	})
	return n
}

func TestTransactionStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		answer  reply
		want    string
		wantErr string
	}{
		{name: "confirmed", answer: reply{result: withContext([]any{status("confirmed", false)})}, want: "confirmed"},
		{name: "finalized", answer: reply{result: withContext([]any{status("finalized", false)})}, want: "confirmed"},
		{name: "processed only", answer: reply{result: withContext([]any{status("processed", false)})}, want: "pending"},
		{name: "processed with an error is not settled", answer: reply{result: withContext([]any{status("processed", true)})}, want: "pending"},
		{name: "failed", answer: reply{result: withContext([]any{status("confirmed", true)})}, want: "failed"},
		{name: "finalized failure", answer: reply{result: withContext([]any{status("finalized", true)})}, want: "failed"},
		{name: "unknown signature", answer: reply{result: withContext([]any{nil})}, want: "pending"},
		{name: "unknown confirmation status", answer: reply{result: withContext([]any{status("rooted", false)})}, wantErr: `unknown confirmation status "rooted"`},
		{name: "two answers for one signature", answer: reply{result: withContext([]any{nil, nil})}, wantErr: "answered 2 statuses"},
		{name: "null value", answer: reply{result: withContext(nil)}, wantErr: "transaction status"},
		{name: "node error", answer: reply{err: &rpcError{Code: -32005, Message: "unhealthy"}}, wantErr: "getSignatureStatuses: node answered -32005: unhealthy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			adapter, _ := single(t, statusNode(t, tt.answer))
			got, err := adapter.Transaction(t.Context(), "solana-mainnet", signature)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Transaction() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Transaction() error = %v", err)
			}
			want := chain.Transaction{NetworkID: "solana-mainnet", Hash: signature, Status: tt.want}
			if got != want {
				t.Errorf("Transaction() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestTransactionRefusesAnInvalidSignatureWithoutAsking(t *testing.T) {
	t.Parallel()

	n := statusNode(t, reply{result: withContext([]any{nil})})
	adapter, _ := single(t, n)
	for _, txID := range []string{"", "abc", "0x" + strings.Repeat("ab", 32), signature[:len(signature)-1] + "0", signature + signature} {
		if _, err := adapter.Transaction(t.Context(), "solana-mainnet", txID); !errors.Is(err, chain.ErrInvalidRequest) {
			t.Errorf("Transaction(%q) error = %v, want ErrInvalidRequest", txID, err)
		}
	}
	if got := n.total(); got != 0 {
		t.Errorf("RPC calls = %d, want none", got)
	}
}

// A signature no node knows is failed only once its blockhash has expired —
// with a margin for lagging nodes — and a second look still does not find it.
func TestTransactionTurnsFailedOnlyPastTheRecordedExpiry(t *testing.T) {
	t.Parallel()

	const lastValid = 1_000
	missing := reply{result: withContext([]any{nil})}
	tests := []struct {
		name     string
		record   bool
		height   uint64
		answers  []reply
		want     string
		statuses int
		heights  int
	}{
		{name: "nothing recorded", height: 5_000, answers: []reply{missing}, want: "pending", statuses: 1},
		{name: "before expiry", record: true, height: lastValid, answers: []reply{missing}, want: "pending", statuses: 1, heights: 1},
		{name: "inside the margin", record: true, height: lastValid + 150, answers: []reply{missing}, want: "pending", statuses: 1, heights: 1},
		{name: "past the margin", record: true, height: lastValid + 151, answers: []reply{missing}, want: "failed", statuses: 2, heights: 1},
		{
			name: "landed after all", record: true, height: lastValid + 500,
			answers: []reply{missing, {result: withContext([]any{status("finalized", false)})}}, want: "confirmed", statuses: 2, heights: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := statusNode(t, tt.answers...)
			n.handle("getBlockHeight", func(params []json.RawMessage) reply {
				if len(params) != 1 || !strings.Contains(string(params[0]), `"finalized"`) {
					t.Errorf("getBlockHeight params = %s, want the finalized commitment", params)
				}
				return reply{result: tt.height}
			})
			adapter, _ := single(t, n)
			if tt.record {
				adapter.RecordBlockhashExpiry("solana-mainnet", signature, lastValid)
				// Recorded for another network, it must not count here.
				adapter.RecordBlockhashExpiry("solana-devnet", signature, 0)
			}
			got, err := adapter.Transaction(t.Context(), "solana-mainnet", signature)
			if err != nil {
				t.Fatalf("Transaction() error = %v", err)
			}
			if got.Status != tt.want {
				t.Errorf("status = %q, want %q", got.Status, tt.want)
			}
			if got := n.count("getSignatureStatuses"); got != tt.statuses {
				t.Errorf("status reads = %d, want %d", got, tt.statuses)
			}
			if got := n.count("getBlockHeight"); got != tt.heights {
				t.Errorf("height reads = %d, want %d", got, tt.heights)
			}
		})
	}
}

// Once a transaction is settled its record is dropped, so a later "not found"
// from a pruned node is pending, not failed.
func TestTransactionForgetsTheExpiryOnceSettled(t *testing.T) {
	t.Parallel()

	n := statusNode(t, reply{result: withContext([]any{status("confirmed", false)})}, reply{result: withContext([]any{nil})})
	n.handle("getBlockHeight", func([]json.RawMessage) reply { return reply{result: 1_000_000} })
	adapter, _ := single(t, n)
	adapter.RecordBlockhashExpiry("solana-mainnet", signature, 10)
	if got, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err != nil || got.Status != "confirmed" {
		t.Fatalf("Transaction() = %+v, %v", got, err)
	}
	if got, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err != nil || got.Status != "pending" {
		t.Errorf("Transaction() after settling = %+v, %v, want pending", got, err)
	}
	if got := n.count("getBlockHeight"); got != 0 {
		t.Errorf("height reads = %d, want none once the record is gone", got)
	}
}

func TestTransactionReportsAFailedHeightRead(t *testing.T) {
	t.Parallel()

	n := statusNode(t, reply{result: withContext([]any{nil})})
	n.handle("getBlockHeight", func([]json.RawMessage) reply { return reply{err: &rpcError{Code: -32602, Message: "bad"}} })
	adapter, _ := single(t, n)
	adapter.RecordBlockhashExpiry("solana-mainnet", signature, 10)
	if _, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err == nil || !strings.Contains(err.Error(), "block height") {
		t.Errorf("Transaction() error = %v, want a block height error", err)
	}
}

func TestTransactionReportsAFailedSecondLook(t *testing.T) {
	t.Parallel()

	n := statusNode(t, reply{result: withContext([]any{nil})}, reply{err: &rpcError{Code: -32602, Message: "second look failed"}})
	n.handle("getBlockHeight", func([]json.RawMessage) reply { return reply{result: 1_000_000} })
	adapter, _ := single(t, n)
	adapter.RecordBlockhashExpiry("solana-mainnet", signature, 10)
	if got, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err == nil ||
		!strings.Contains(err.Error(), "second look failed") {
		t.Errorf("Transaction() = %+v, %v, want the second read's error, not failed", got, err)
	}
}
