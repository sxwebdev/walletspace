package tron_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tronchain "github.com/sxwebdev/walletspace/internal/chain/tron"
	"github.com/sxwebdev/walletspace/internal/network"
	"golang.org/x/sync/errgroup"
)

type endpointProvider struct {
	url    string
	client *http.Client
}

func (p endpointProvider) Endpoints(context.Context, network.Network) ([]string, error) {
	return []string{p.url}, nil
}

func (p endpointProvider) HTTPClient(network.Network) *http.Client {
	return p.client
}

func TestWrongTronNetworkIdentityIsRejectedBeforeUse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jsonrpc" {
			t.Errorf("path = %q, want /jsonrpc", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1, "result": "0xcd8690dc",
		})
	}))
	t.Cleanup(server.Close)
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	mainnet, err := registry.Get("tron-mainnet")
	if err != nil {
		t.Fatalf("registry.Get() error = %v", err)
	}
	err = tronchain.VerifyEndpoint(
		t.Context(), mainnet, server.URL, nil, server.Client(),
	)
	if err == nil || !strings.Contains(err.Error(), "expected 0x2b6653dc") {
		t.Fatalf("VerifyEndpoint() error = %v", err)
	}
}

func TestProbeEndpointRequiresARecentHeadBlock(t *testing.T) {
	t.Parallel()

	var stale atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("TRON-PRO-API-KEY"); got != "secret" {
			t.Errorf("TRON-PRO-API-KEY = %q", got)
		}
		switch r.URL.Path {
		case "/jsonrpc":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 1, "result": "0x2b6653dc",
			})
		case "/wallet/getnowblock":
			blockTime := time.Now()
			if stale.Load() {
				blockTime = blockTime.Add(-10 * time.Minute)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"block_header": map[string]any{"raw_data": map[string]any{
					"number": 123, "timestamp": blockTime.UnixMilli(),
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	mainnet, err := registry.Get("tron-mainnet")
	if err != nil {
		t.Fatalf("registry.Get() error = %v", err)
	}
	if err := tronchain.ProbeEndpoint(
		t.Context(), mainnet, server.URL,
		http.Header{"Tron-Pro-Api-Key": []string{"secret"}}, server.Client(),
	); err != nil {
		t.Fatalf("ProbeEndpoint() error = %v", err)
	}
	stale.Store(true)
	if err := tronchain.ProbeEndpoint(
		t.Context(), mainnet, server.URL,
		http.Header{"Tron-Pro-Api-Key": []string{"secret"}}, server.Client(),
	); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("ProbeEndpoint(stale) error = %v", err)
	}
}

func TestConcurrentInitialHealthStaysWithinPublicRPCBudget(t *testing.T) {
	t.Parallel()

	var (
		requests atomic.Int64
		blocks   atomic.Int64
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/jsonrpc":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 1, "result": "0x2b6653dc",
			})
		case "/wallet/triggerconstantcontract":
			var request struct {
				Data string `json:"data"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode trigger request: %v", err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			value := strings.Repeat("0", 63) + "6"
			if request.Data == "95d89b41" {
				value = "55534454" + strings.Repeat("0", 56)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result":          map[string]any{"result": true},
				"constant_result": []string{value},
			})
		case "/wallet/getnowblock":
			blocks.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	adapter, err := tronchain.New(
		t.Context(),
		registry,
		nil,
		endpointProvider{url: server.URL, client: server.Client()},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("tron.New() error = %v", err)
	}
	t.Cleanup(adapter.Close)

	group, ctx := errgroup.WithContext(t.Context())
	for range 20 {
		group.Go(func() error {
			return adapter.Health(ctx, "tron-mainnet")
		})
	}
	if err := group.Wait(); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if got, want := requests.Load(), int64(3); got != want {
		t.Errorf("RPC requests = %d, want %d", got, want)
	}
	if got := blocks.Load(); got != 0 {
		t.Errorf("initial block probes = %d, want 0", got)
	}
}

// countingTransport counts the requests that went through the client a
// provider handed out, which is how a test tells the guarded client was used.
type countingTransport struct {
	base  http.RoundTripper
	count atomic.Int32
}

func (c *countingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	c.count.Add(1)
	return c.base.RoundTrip(request)
}

// The adapter's ProbeEndpoint is the complete Doctor probe — identity and a
// recent head block — sent through the HTTP client the endpoint provider
// supplies, never through a default one.
func TestAdapterProbeEndpointUsesTheProvidedClient(t *testing.T) {
	t.Parallel()

	var stale atomic.Bool
	var unkeyed atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every probe request carries the endpoint's credential; a keyed
		// TronGrid endpoint answers nothing without it.
		if r.Header.Get("TRON-PRO-API-KEY") != "secret" {
			unkeyed.Add(1)
			http.Error(w, "unauthorised", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/jsonrpc":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": "0xcd8690dc"})
		case "/wallet/getnowblock":
			blockTime := time.Now()
			if stale.Load() {
				blockTime = blockTime.Add(-10 * time.Minute)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"block_header": map[string]any{"raw_data": map[string]any{
					"number": 123, "timestamp": blockTime.UnixMilli(),
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	transport := &countingTransport{base: server.Client().Transport}
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	adapter, err := tronchain.New(
		t.Context(), registry, nil,
		endpointProvider{url: server.URL, client: &http.Client{Transport: transport}},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("tron.New() error = %v", err)
	}
	t.Cleanup(adapter.Close)
	nile, err := registry.Get("tron-nile")
	if err != nil {
		t.Fatalf("registry.Get() error = %v", err)
	}
	headers := http.Header{"Tron-Pro-Api-Key": {"secret"}}
	if err := adapter.ProbeEndpoint(t.Context(), nile, server.URL, headers); err != nil {
		t.Fatalf("ProbeEndpoint() error = %v", err)
	}
	if got := transport.count.Load(); got != 2 {
		t.Errorf("requests through the provided client = %d, want 2 (identity and head block)", got)
	}
	if got := unkeyed.Load(); got != 0 {
		t.Errorf("%d probe requests arrived without the endpoint's credential", got)
	}
	stale.Store(true)
	if err := adapter.ProbeEndpoint(t.Context(), nile, server.URL, headers); err == nil ||
		!strings.Contains(err.Error(), "stale") {
		t.Errorf("ProbeEndpoint(stale head) error = %v, want a stale head block", err)
	}
	if got := adapter.TokenKind(); got != "trc20" {
		t.Errorf("TokenKind() = %q, want trc20", got)
	}
}
