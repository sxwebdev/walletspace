package solana_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sxwebdev/walletspace/internal/chain"
)

// A node cannot make the wallet read an answer of unbounded size. The answer
// here is valid and says "confirmed" once read to the end; only a reader that
// stops at the cap fails on it.
func TestOversizedAnswersAreRefused(t *testing.T) {
	t.Parallel()

	answer := `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1,"pad":"` + strings.Repeat("a", 9<<20) +
		`"},"value":[{"slot":5,"confirmations":null,"err":null,"confirmationStatus":"confirmed"}]}}`
	n := statusNode(t, reply{raw: answer})
	adapter, _ := single(t, n)
	if got, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err == nil {
		t.Errorf("Transaction() = %+v, want the oversized answer refused", got)
	}
	small := strings.Replace(answer, strings.Repeat("a", 9<<20), strings.Repeat("a", 7<<20), 1)
	n2 := statusNode(t, reply{raw: small})
	adapter2, _ := single(t, n2)
	if got, err := adapter2.Transaction(t.Context(), "solana-mainnet", signature); err != nil || got.Status != "confirmed" {
		t.Errorf("Transaction(7 MiB) = %+v, %v, want an answer under the cap read", got, err)
	}
}

// Without a credential an endpoint is the public RPC, which allows 40 calls
// per method per 10 s: calls of one method are spaced by 250 ms, and calls of
// any method by 100 ms. A keyed endpoint is not held to that.
func TestPublicEndpointsArePaced(t *testing.T) {
	t.Parallel()

	for _, keyed := range []bool{false, true} {
		t.Run(fmt.Sprintf("keyed=%v", keyed), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				n := statusNode(t, reply{result: withContext([]any{nil})})
				n.handle("getBlockHeight", func([]json.RawMessage) reply { return reply{result: 1} })
				p := &provider{endpoints: []string{n.server.URL}, client: n.server.Client()}
				if keyed {
					p.headers = map[string]http.Header{n.server.URL: {"X-Key": {"k"}}}
				}
				adapter := newAdapter(t, p)
				adapter.RecordBlockhashExpiry("solana-mainnet", signature, 1_000)
				if _, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				// Each call: getSignatureStatuses then getBlockHeight.
				for range 3 {
					if _, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err != nil {
						t.Fatal(err)
					}
				}
				elapsed := time.Since(start)
				if keyed {
					if elapsed != 0 {
						t.Errorf("keyed calls took %s, want no pacing", elapsed)
					}
					return
				}
				// The first status read waits out the 250 ms left from the
				// warm-up call, and so does each following one.
				if elapsed < 750*time.Millisecond || elapsed >= 800*time.Millisecond {
					t.Errorf("three paced call pairs took %s, want 750 ms", elapsed)
				}
			})
		})
	}
}

// Token reads of a public endpoint run two at a time, keyed ones eight.
func TestTokenReadsAreBoundedPerNetwork(t *testing.T) {
	t.Parallel()

	for keyed, want := range map[bool]int{false: 2, true: 8} {
		t.Run(fmt.Sprintf("keyed=%v", keyed), func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				n := balanceNode(t, nil, nil)
				var mu sync.Mutex
				inFlight, peak := 0, 0
				n.handle("getTokenAccountsByOwner", func([]json.RawMessage) reply {
					mu.Lock()
					inFlight++
					peak = max(peak, inFlight)
					mu.Unlock()
					time.Sleep(5 * time.Second)
					mu.Lock()
					inFlight--
					mu.Unlock()
					return reply{result: withContext([]any{})}
				})
				p := &provider{endpoints: []string{n.server.URL}, client: n.server.Client()}
				if keyed {
					p.headers = map[string]http.Header{n.server.URL: {"X-Key": {"k"}}}
				}
				adapter := newAdapter(t, p)
				var accounts []chain.AccountAddress
				for i := range 12 {
					accounts = append(accounts, chain.AccountAddress{AccountID: fmt.Sprint(i), Address: key(byte(i + 10))})
				}
				got := collect(adapter.BalanceStream(t.Context(), "solana-mainnet", accounts,
					[]chain.Asset{spl("solana-mainnet", usdcMint, "USDC", 6)}, false))
				if len(got) != 12 {
					t.Errorf("balances = %d, want 12", len(got))
				}
				if peak != want {
					t.Errorf("concurrent token reads peaked at %d, want %d", peak, want)
				}
			})
		})
	}
}

// An answer of exactly the cap is read; the cap refuses only what is past it.
func TestAnAnswerOfExactlyTheCapIsRead(t *testing.T) {
	t.Parallel()

	head := `{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":1,"pad":"`
	tail := `"},"value":[{"slot":5,"confirmations":null,"err":null,"confirmationStatus":"confirmed"}]}}`
	answer := head + strings.Repeat("a", 8<<20-len(head)-len(tail)) + tail
	adapter, _ := single(t, statusNode(t, reply{raw: answer}))
	if got, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err != nil || got.Status != "confirmed" {
		t.Errorf("Transaction() = %+v, %v, want the answer read", got, err)
	}
	// One byte more inside the value, so the answer is not complete within
	// the cap.
	adapter, _ = single(t, statusNode(t, reply{raw: strings.Replace(answer, `"pad":"`, `"pad":"a`, 1)}))
	if _, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err == nil {
		t.Error("Transaction() read an answer one byte past the cap")
	}
}

// A caller whose deadline falls before its paced turn leaves without asking.
func TestACallerLeavesThePacingQueue(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := statusNode(t, reply{result: withContext([]any{nil})})
		adapter, _ := single(t, n)
		if _, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		if _, err := adapter.Transaction(ctx, "solana-mainnet", signature); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Transaction() error = %v, want the caller's deadline", err)
		}
		if got := n.count("getSignatureStatuses"); got != 1 {
			t.Errorf("status reads = %d, want only the first", got)
		}
	})
}
