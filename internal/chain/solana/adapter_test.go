package solana_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	solanachain "github.com/sxwebdev/walletspace/internal/chain/solana"
)

func TestNewRequiresARegistryAndAProvider(t *testing.T) {
	t.Parallel()

	if _, err := solanachain.New(nil, &provider{}); err == nil {
		t.Error("New(nil registry) succeeded")
	}
	if _, err := solanachain.New(networks(), nil); err == nil {
		t.Error("New(nil provider) succeeded")
	}
}

// Every endpoint is checked before use with its own credentials; one that
// serves another cluster is dropped, and only verified ones are reported
// healthy.
func TestClientVerifiesEachEndpointWithItsOwnCredentials(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		wrong := newNode(t, devnetGenesis)
		right := statusNode(t, reply{result: withContext([]any{nil})})
		p := &provider{
			endpoints: []string{wrong.server.URL + "/wrong", right.server.URL + "/right"},
			headers:   map[string]http.Header{right.server.URL + "/right": {"X-Key": {"right-key"}}},
		}
		// Both test servers share the in-memory network; route by path.
		mux := http.NewServeMux()
		mux.HandleFunc("/wrong", wrong.serve)
		mux.HandleFunc("/right", right.serve)
		p.client = newMuxClient(t, mux)
		adapter := newAdapter(t, p)
		if _, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err != nil {
			t.Fatalf("Transaction() error = %v", err)
		}
		if !slices.Equal(p.healthy, []string{right.server.URL + "/right"}) {
			t.Errorf("healthy endpoints = %v, want the verified one only", p.healthy)
		}
		if got := wrong.count("getSignatureStatuses"); got != 0 {
			t.Errorf("the wrong-cluster node was asked %d times", got)
		}
		for _, h := range wrong.seenHeaders() {
			if h.Get("X-Key") != "" {
				t.Error("another endpoint's credential reached the wrong node")
			}
		}
		for _, h := range right.seenHeaders() {
			if h.Get("X-Key") != "right-key" {
				t.Error("a request reached the keyed node without its credential")
			}
		}
	})
}

func TestClientFailsWhenNoEndpointVerifies(t *testing.T) {
	t.Parallel()

	wrong := newNode(t, devnetGenesis)
	adapter, _ := single(t, wrong)
	_, err := adapter.Transaction(t.Context(), "solana-mainnet", signature)
	if err == nil || !strings.Contains(err.Error(), "no verified Solana RPC for solana-mainnet") ||
		!strings.Contains(err.Error(), "expected "+mainnetGenesis) {
		t.Errorf("Transaction() error = %v", err)
	}
	empty := newAdapter(t, &provider{client: wrong.server.Client()})
	if _, err := empty.Transaction(t.Context(), "solana-mainnet", signature); err == nil ||
		!strings.Contains(err.Error(), "no endpoints are configured") {
		t.Errorf("Transaction(no endpoints) error = %v", err)
	}
	unguarded := newAdapter(t, &provider{endpoints: []string{wrong.server.URL}})
	if _, err := unguarded.Transaction(t.Context(), "solana-mainnet", signature); err == nil ||
		!strings.Contains(err.Error(), "guarded HTTP client") {
		t.Errorf("Transaction(no client) error = %v", err)
	}
	if got := wrong.count("getSignatureStatuses"); got != 0 {
		t.Errorf("unverified node asked %d times", got)
	}
}

// Concurrent first calls share one identity check.
func TestConcurrentFirstCallsVerifyOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := statusNode(t, reply{result: withContext([]any{nil})})
		adapter, _ := single(t, n)
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				if _, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err != nil {
					t.Errorf("Transaction() error = %v", err)
				}
			})
		}
		wg.Wait()
		if got := n.count("getGenesisHash"); got != 1 {
			t.Errorf("getGenesisHash calls = %d, want 1", got)
		}
	})
}

func TestInvalidateRebuildsAndForwardsToTheProvider(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := statusNode(t, reply{result: withContext([]any{nil})})
		adapter, p := single(t, n)
		for range 2 {
			if _, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err != nil {
				t.Fatalf("Transaction() error = %v", err)
			}
		}
		adapter.Invalidate("solana-mainnet")
		if _, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err != nil {
			t.Fatalf("Transaction() error = %v", err)
		}
		if got := n.count("getGenesisHash"); got != 2 {
			t.Errorf("getGenesisHash calls = %d, want 2 (one per build)", got)
		}
		if !slices.Equal(p.invalidated, []string{"solana-mainnet"}) {
			t.Errorf("provider invalidations = %v", p.invalidated)
		}
	})
}

// A node that stops answering is skipped for the next verified one, and the
// next call starts there.
func TestCallsFailOverToTheNextVerifiedEndpoint(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		first := statusNode(t, reply{status: http.StatusServiceUnavailable, raw: "busy"})
		second := statusNode(t, reply{result: withContext([]any{status("confirmed", false)})})
		mux := http.NewServeMux()
		mux.HandleFunc("/first", first.serve)
		mux.HandleFunc("/second", second.serve)
		p := &provider{endpoints: []string{"http://rpc.test/first", "http://rpc.test/second"}, client: newMuxClient(t, mux)}
		adapter := newAdapter(t, p)
		for range 2 {
			got, err := adapter.Transaction(t.Context(), "solana-mainnet", signature)
			if err != nil || got.Status != "confirmed" {
				t.Fatalf("Transaction() = %+v, %v", got, err)
			}
		}
		if got := first.count("getSignatureStatuses"); got != 1 {
			t.Errorf("failed node asked %d times, want once before failing over", got)
		}
		if got := second.count("getSignatureStatuses"); got != 2 {
			t.Errorf("second node asked %d times, want 2", got)
		}
	})
}

// A request the node judged malformed fails the same way everywhere and is not
// sent to the next node.
func TestInvalidParamsAreNotRetriedElsewhere(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		first := statusNode(t, reply{err: &rpcError{Code: -32602, Message: "Invalid params"}})
		second := statusNode(t, reply{result: withContext([]any{nil})})
		mux := http.NewServeMux()
		mux.HandleFunc("/first", first.serve)
		mux.HandleFunc("/second", second.serve)
		adapter := newAdapter(t, &provider{endpoints: []string{"http://rpc.test/first", "http://rpc.test/second"}, client: newMuxClient(t, mux)})
		if _, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err == nil || !strings.Contains(err.Error(), "Invalid params") {
			t.Errorf("Transaction() error = %v", err)
		}
		if got := second.count("getSignatureStatuses"); got != 0 {
			t.Errorf("second node asked %d times, want 0", got)
		}
	})
}

// newMuxClient serves several fake nodes from one in-memory server.
func newMuxClient(t *testing.T, mux *http.ServeMux) *http.Client {
	t.Helper()
	server := httptest.NewTestServer(t, mux)
	t.Cleanup(server.Close)
	return server.Client()
}

func TestClientReportsProviderFailures(t *testing.T) {
	t.Parallel()

	n := newNode(t, mainnetGenesis)
	broken := errors.New("provider broke")
	for name, p := range map[string]*provider{
		"endpoints": {endpoints: []string{n.server.URL}, client: n.server.Client(), endpointsErr: broken},
		"headers":   {endpoints: []string{n.server.URL}, client: n.server.Client(), headersErr: broken},
	} {
		adapter := newAdapter(t, p)
		if _, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); !errors.Is(err, broken) {
			t.Errorf("%s: Transaction() error = %v, want the provider's error", name, err)
		}
	}
	if got := n.total(); got != 0 {
		t.Errorf("RPC calls = %d, want none", got)
	}
}

// failingTransport stands for a network that refuses every connection.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("connection refused")
}

func TestClientReportsUnreachableEndpoints(t *testing.T) {
	t.Parallel()

	adapter := newAdapter(t, &provider{endpoints: []string{"https://rpc.example"}, client: &http.Client{Transport: failingTransport{}}})
	if _, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err == nil ||
		!strings.Contains(err.Error(), "connection refused") {
		t.Errorf("Transaction() error = %v", err)
	}
}

// Settings that change while endpoints are being verified make the verified
// set worthless: the call starts over and builds from the new settings.
func TestClientRebuildsWhenInvalidatedDuringABuild(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := statusNode(t, reply{result: withContext([]any{nil})})
		p := &provider{endpoints: []string{n.server.URL}, client: n.server.Client()}
		adapter := newAdapter(t, p)
		builds := 0
		p.onEndpoints = func() {
			builds++
			if builds == 1 {
				adapter.Invalidate("solana-mainnet")
			}
		}
		if _, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err != nil {
			t.Fatalf("Transaction() error = %v", err)
		}
		if builds != 2 {
			t.Errorf("builds = %d, want the first discarded and a second one used", builds)
		}
		if got := n.count("getGenesisHash"); got != 2 {
			t.Errorf("getGenesisHash calls = %d, want 2", got)
		}
	})
}

// A caller that leaves while a call is in flight gets its own cancellation,
// not a failover to the next node.
func TestCancelledCallsAreNotFailedOver(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		first := newNode(t, mainnetGenesis)
		first.handle("getSignatureStatuses", func([]json.RawMessage) reply {
			cancel()
			return reply{status: http.StatusServiceUnavailable, raw: "busy"}
		})
		second := statusNode(t, reply{result: withContext([]any{nil})})
		mux := http.NewServeMux()
		mux.HandleFunc("/first", first.serve)
		mux.HandleFunc("/second", second.serve)
		adapter := newAdapter(t, &provider{endpoints: []string{"http://rpc.test/first", "http://rpc.test/second"}, client: newMuxClient(t, mux)})
		if _, err := adapter.Transaction(ctx, "solana-mainnet", signature); !errors.Is(err, context.Canceled) {
			t.Errorf("Transaction() error = %v, want context.Canceled", err)
		}
		if got := second.count("getSignatureStatuses"); got != 0 {
			t.Errorf("second node asked %d times after the caller left", got)
		}
	})
}

// A caller that left while its discarded build ran does not start another.
func TestClientDoesNotRebuildForACallerThatLeft(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		n := statusNode(t, reply{result: withContext([]any{nil})})
		p := &provider{endpoints: []string{n.server.URL}, client: n.server.Client()}
		adapter := newAdapter(t, p)
		ctx, cancel := context.WithCancel(t.Context())
		builds := 0
		p.onEndpoints = func() {
			builds++
			adapter.Invalidate("solana-mainnet")
			cancel()
		}
		if _, err := adapter.Transaction(ctx, "solana-mainnet", signature); !errors.Is(err, context.Canceled) {
			t.Errorf("Transaction() error = %v, want context.Canceled", err)
		}
		// The shared build runs to its end for anyone else waiting on it.
		time.Sleep(time.Minute)
		if builds != 1 {
			t.Errorf("builds = %d, want 1", builds)
		}
	})
}

// A caller does not sit through a slow build it no longer waits for; the build
// finishes for later callers.
func TestACallerLeavesASlowBuildWithoutStoppingIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		hole := newNode(t, mainnetGenesis)
		hole.handle("getGenesisHash", func([]json.RawMessage) reply {
			time.Sleep(time.Minute)
			return reply{result: mainnetGenesis}
		})
		ok := statusNode(t, reply{result: withContext([]any{nil})})
		mux := http.NewServeMux()
		mux.HandleFunc("/hole", hole.serve)
		mux.HandleFunc("/ok", ok.serve)
		adapter := newAdapter(t, &provider{endpoints: []string{"http://rpc.test/hole", "http://rpc.test/ok"}, client: newMuxClient(t, mux)})

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		start := time.Now()
		if _, err := adapter.Transaction(ctx, "solana-mainnet", signature); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Transaction() error = %v, want the caller's deadline", err)
		}
		if waited := time.Since(start); waited != time.Second {
			t.Errorf("the caller waited %s, want 1s", waited)
		}

		// The build gives the black hole its 15 s and keeps the other node.
		time.Sleep(20 * time.Second)
		if got, err := adapter.Transaction(t.Context(), "solana-mainnet", signature); err != nil || got.Status != "pending" {
			t.Errorf("later Transaction() = %+v, %v", got, err)
		}
		if hole.count("getGenesisHash") != 1 || ok.count("getGenesisHash") != 1 {
			t.Errorf("identity checks = %d + %d, want one build", hole.count("getGenesisHash"), ok.count("getGenesisHash"))
		}
		time.Sleep(time.Minute)
	})
}
