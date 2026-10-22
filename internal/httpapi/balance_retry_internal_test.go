package httpapi

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/chain"
)

// flakyPass answers every balance with its amount, except that each pair in
// fails errors that many times before it answers.
type flakyPass struct {
	mu      sync.Mutex
	fails   map[string]int
	reads   map[string]int
	refresh []bool
}

func (f *flakyPass) pass(
	_ context.Context, holders []chain.AccountAddress, assets []chain.Asset, refresh bool,
) <-chan chain.Balance {
	f.mu.Lock()
	f.refresh = append(f.refresh, refresh)
	var results []chain.Balance
	for _, holder := range holders {
		for _, asset := range assets {
			key := holder.AccountID + "/" + asset.ID
			f.reads[key]++
			result := chain.Balance{AccountID: holder.AccountID, AssetID: asset.ID}
			if f.fails[key] > 0 {
				f.fails[key]--
				result.Error = "429 Too Many Requests"
			} else {
				result.Amount = "1.5"
			}
			results = append(results, result)
		}
	}
	f.mu.Unlock()
	out := make(chan chain.Balance, len(results))
	for _, result := range results {
		out <- result
	}
	close(out)
	return out
}

func TestRetryBalancesRereadsOnlyWhatFailed(t *testing.T) {
	t.Parallel()

	holders := []chain.AccountAddress{{AccountID: "a", Address: "0xa"}, {AccountID: "b", Address: "0xb"}}
	assets := []chain.Asset{{ID: "native"}, {ID: "token"}}
	delays := []time.Duration{time.Millisecond, time.Millisecond}
	for _, tt := range []struct {
		name      string
		fails     map[string]int
		wantError []string
		wantReads map[string]int
	}{
		{
			name:      "every read succeeds",
			wantReads: map[string]int{"a/native": 1, "a/token": 1, "b/native": 1, "b/token": 1},
		},
		{
			name:      "a rate limit clears on the first retry",
			fails:     map[string]int{"a/token": 1},
			wantReads: map[string]int{"a/native": 1, "a/token": 2, "b/native": 1, "b/token": 1},
		},
		{
			name:      "a rate limit clears on the last retry",
			fails:     map[string]int{"b/native": 2},
			wantReads: map[string]int{"a/native": 1, "a/token": 1, "b/native": 3, "b/token": 1},
		},
		{
			name:      "a failure outlasting every retry keeps its error",
			fails:     map[string]int{"a/native": 5},
			wantError: []string{"a/native"},
			wantReads: map[string]int{"a/native": 3, "a/token": 1, "b/native": 1, "b/token": 1},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			flaky := &flakyPass{fails: tt.fails, reads: map[string]int{}}
			if flaky.fails == nil {
				flaky.fails = map[string]int{}
			}
			got := map[string]chain.Balance{}
			for result := range retryBalances(t.Context(), flaky.pass, holders, assets, false, delays) {
				key := result.AccountID + "/" + result.AssetID
				if _, dup := got[key]; dup {
					t.Errorf("%s sent twice", key)
				}
				got[key] = result
			}
			if len(got) != 4 {
				t.Fatalf("balances sent = %d, want 4: %+v", len(got), got)
			}
			var errored []string
			for key, result := range got {
				if result.Error != "" {
					errored = append(errored, key)
				} else if result.Amount != "1.5" {
					t.Errorf("%s amount = %q, want 1.5", key, result.Amount)
				}
			}
			if !slices.Equal(errored, tt.wantError) {
				t.Errorf("errored = %v, want %v", errored, tt.wantError)
			}
			for key, want := range tt.wantReads {
				if flaky.reads[key] != want {
					t.Errorf("%s read %d times, want %d", key, flaky.reads[key], want)
				}
			}
			// A retry goes past any cache: the cached answer is the one
			// that could not be trusted.
			if flaky.refresh[0] || slices.Contains(flaky.refresh[1:], false) {
				t.Errorf("refresh per pass = %v, want only retries refreshed", flaky.refresh)
			}
		})
	}
}

func TestRetryBalancesStopsWithItsContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	flaky := &flakyPass{fails: map[string]int{"a/native": 1}, reads: map[string]int{}}
	results := retryBalances(
		ctx, flaky.pass,
		[]chain.AccountAddress{{AccountID: "a"}}, []chain.Asset{{ID: "native"}}, false,
		[]time.Duration{time.Hour},
	)
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for result := range results {
			if !strings.Contains(result.Error, "429") {
				t.Errorf("unexpected result after cancel: %+v", result)
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retryBalances kept waiting out its delay after the context ended")
	}
	if flaky.reads["a/native"] != 1 {
		t.Errorf("reads after cancel = %d, want 1", flaky.reads["a/native"])
	}
}
