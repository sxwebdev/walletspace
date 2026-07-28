package tron

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestBalancesServesCacheWithoutRefetching(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	s := newTestService(func(_ context.Context, addr string) (Balance, error) {
		calls.Add(1)
		return Balance{TRX: decimal.NewFromInt(1), USDT: decimal.NewFromInt(2), Activated: true}, nil
	})

	addrs := []string{"TAddr0", "TAddr1"}

	first, errs := s.Balances(t.Context(), addrs, false)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	if len(first) != 2 || calls.Load() != 2 {
		t.Fatalf("first call returned %d balances after %d fetches, want 2 and 2", len(first), calls.Load())
	}

	second, _ := s.Balances(t.Context(), addrs, false)
	if calls.Load() != 2 {
		t.Errorf("second call issued %d fetches in total, want the cache to serve it", calls.Load())
	}

	if len(second) != 2 {
		t.Errorf("second call returned %d balances, want 2 from cache", len(second))
	}
}

func TestBalancesCacheStillExpires(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	s := newTestService(func(_ context.Context, addr string) (Balance, error) {
		calls.Add(1)
		return Balance{TRX: decimal.NewFromInt(1), Activated: true}, nil
	})

	// A cache hit must not push its own expiry out. Reading repeatedly inside
	// the TTL used to re-stamp the entry on every call, so it never went stale
	// and the UI showed an indefinitely old balance.
	s.cache = newBalanceCache(50 * time.Millisecond)

	addrs := []string{"TAddr0"}

	s.Balances(t.Context(), addrs, false)
	for range 5 {
		time.Sleep(15 * time.Millisecond)
		s.Balances(t.Context(), addrs, false)
	}

	if got := calls.Load(); got < 2 {
		t.Errorf("fetch count = %d after repeatedly reading past the TTL, want the entry to expire and refetch", got)
	}
}

func TestBalancesRefreshBypassesCache(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	s := newTestService(func(_ context.Context, addr string) (Balance, error) {
		calls.Add(1)
		return Balance{TRX: decimal.NewFromInt(1)}, nil
	})

	addrs := []string{"TAddr0"}

	s.Balances(t.Context(), addrs, false)
	s.Balances(t.Context(), addrs, true)

	if got := calls.Load(); got != 2 {
		t.Errorf("fetch count = %d, want 2 (refresh must not use the cache)", got)
	}
}

func TestBalancesIsolatesPerAddressFailures(t *testing.T) {
	t.Parallel()

	boom := errors.New("node unreachable")
	s := newTestService(func(_ context.Context, addr string) (Balance, error) {
		if addr == "TBad" {
			return Balance{}, boom
		}

		return Balance{TRX: decimal.NewFromInt(3), Activated: true}, nil
	})

	out, errs := s.Balances(t.Context(), []string{"TGood", "TBad"}, false)

	if _, ok := out["TGood"]; !ok {
		t.Error("a healthy address was dropped because another one failed")
	}

	if !errors.Is(errs["TBad"], boom) {
		t.Errorf("errs[TBad] = %v, want the fetch error", errs["TBad"])
	}

	// A failed address must not be cached as a success.
	if _, ok := s.cache.get("TBad", time.Now()); ok {
		t.Error("a failed fetch was written to the cache")
	}
}

func TestBalancesSurvivesPanickingFetch(t *testing.T) {
	t.Parallel()

	// errgroup does not recover, and the gotron client indexes its result
	// slice unguarded — an unrecovered panic here would kill the daemon.
	s := newTestService(func(_ context.Context, addr string) (Balance, error) {
		if addr == "TPanic" {
			panic("index out of range [0] with length 0")
		}

		return Balance{TRX: decimal.NewFromInt(4), Activated: true}, nil
	})

	out, errs := s.Balances(t.Context(), []string{"TOk", "TPanic"}, false)

	if _, ok := out["TOk"]; !ok {
		t.Error("the healthy address was lost when another one panicked")
	}

	if errs["TPanic"] == nil {
		t.Error("the panicking address produced no error")
	}
}

func TestBalancesRespectsWorkerLimit(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		running int
		peak    int
	)

	s := newTestService(func(_ context.Context, addr string) (Balance, error) {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()

		time.Sleep(5 * time.Millisecond)

		mu.Lock()
		running--
		mu.Unlock()

		return Balance{}, nil
	})
	s.workers = 2

	addrs := make([]string, 0, 8)
	for i := range 8 {
		addrs = append(addrs, string(rune('A'+i)))
	}

	s.Balances(t.Context(), addrs, false)

	mu.Lock()
	defer mu.Unlock()
	if peak > 2 {
		t.Errorf("peak concurrency = %d, want at most the configured 2", peak)
	}
}

func newTestService(fetch func(context.Context, string) (Balance, error)) *Service {
	return &Service{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		token:   TokenInfo{Contract: "TContract", Symbol: "USDT", Decimals: 6},
		nodes:   1,
		workers: 4,
		cache:   newBalanceCache(balanceTTL),
		fetch:   fetch,
	}
}
