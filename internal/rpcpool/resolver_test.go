package rpcpool

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/storage"
)

type settingsStub struct {
	snapshot config.SettingsSnapshot
	override config.NetworkOverride
	has      bool
	home     string
}

func (s settingsStub) Snapshot() config.SettingsSnapshot { return s.snapshot }
func (s settingsStub) Home() string                      { return s.home }
func (s settingsStub) NetworkOverride(string) (config.NetworkOverride, bool) {
	return s.override, s.has
}

// The endpoints discovery hands back are filtered. The request that fetches
// them was not: it went out on a plain dialer, so the one outbound call this
// process makes on a timer was also the only one that could reach the machine
// it runs on.
func TestDiscoveryItselfGoesThroughTheGuardedDialer(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"nodes":[{"url":"https://public.example"}]}`))
	}))
	t.Cleanup(service.Close)

	discovery := config.DiscoverySettings{
		Enabled: true, URL: service.URL, RequestTimeout: 5 * time.Second,
	}
	item := network.Network{ID: "ethereum-mainnet", Family: network.FamilyEVM, ChainID: "1"}

	resolver := New(settingsStub{})
	resolver.lookupIP = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "public.example" {
			return []net.IP{net.ParseIP("8.8.8.8")}, nil
		}
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	if _, err := resolver.discover(t.Context(), discovery, item); err == nil {
		t.Error("discover() against loopback error = nil, want a refused dial")
	}
	if got := hits.Load(); got != 0 {
		t.Errorf("the discovery service was reached %d times, want 0", got)
	}

	// With private RPC explicitly allowed — the same switch the dialer honours
	// for nodes — the request goes through, which is what proves the refusal
	// above came from the dialer and not from something else being broken.
	permissive := settingsStub{}
	permissive.snapshot.Config.NodeDiscovery.AllowInsecureRPC = true
	allowed := New(permissive)
	allowed.lookupIP = resolver.lookupIP
	discovery.AllowInsecureRPC = true
	endpoints, err := allowed.discover(t.Context(), discovery, item)
	if err != nil {
		t.Fatalf("discover() with private addresses allowed error = %v", err)
	}
	if len(endpoints) != 1 || endpoints[0] != "https://public.example" {
		t.Errorf("discover() = %v, want the one advertised endpoint", endpoints)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("the discovery service was reached %d times, want 1", got)
	}
}

func TestSafeDynamicEndpointRejectsSSRF(t *testing.T) {
	t.Parallel()

	resolver := New(settingsStub{})
	resolver.lookupIP = func(_ context.Context, host string) ([]net.IP, error) {
		switch host {
		case "public.example":
			return []net.IP{net.ParseIP("8.8.8.8")}, nil
		case "carrier.example":
			return []net.IP{net.ParseIP("100.64.1.1")}, nil
		default:
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
	}
	tests := []struct {
		endpoint string
		wantErr  bool
	}{
		{endpoint: "https://public.example"},
		{endpoint: "http://public.example", wantErr: true},
		{endpoint: "https://localhost", wantErr: true},
		{endpoint: "https://walletspace.local", wantErr: true},
		{endpoint: "https://private.example", wantErr: true},
		{endpoint: "https://carrier.example", wantErr: true},
		{endpoint: "file:///etc/passwd", wantErr: true},
		// A comma is legal in a URL path, so this parses as one HTTPS URL on a
		// public host and passes every check below. It is also the separator in
		// the Tron node list, so downstream it would become a second, unchecked
		// plaintext gRPC node pointed at loopback.
		{endpoint: "https://public.example/rpc,grpc://127.0.0.1:50051", wantErr: true},
		{endpoint: "https://public.example/rpc,http://169.254.169.254", wantErr: true},
		{endpoint: "https://public.example/rpc|1", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.endpoint, func(t *testing.T) {
			t.Parallel()
			err := resolver.safeDynamicEndpoint(t.Context(), test.endpoint, false)
			if (err != nil) != test.wantErr {
				t.Fatalf("safeDynamicEndpoint() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestExtractURLsHandlesDocumentShapes(t *testing.T) {
	t.Parallel()

	got := extractURLs(map[string]any{
		"nodes": []any{
			map[string]any{"rpc_url": "https://one.example"},
			map[string]any{"endpoint": "https://two.example"},
			map[string]any{"name": "https://ignored.example"},
		},
	})
	if len(got) != 2 {
		t.Fatalf("extractURLs() = %#v", got)
	}
}

func TestCacheWithoutNetworksCanBeUpdated(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	if err := storage.AtomicWrite(
		filepath.Join(home, "cache", "rpc-nodes.json"),
		[]byte(`{"schema_version":2}`),
	); err != nil {
		t.Fatalf("AtomicWrite(cache) error = %v", err)
	}
	resolver := New(settingsStub{
		home:     home,
		snapshot: config.SettingsSnapshot{Config: config.DefaultHomeConfig()},
	})
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	item, err := registry.Get("ethereum-mainnet")
	if err != nil {
		t.Fatalf("registry.Get() error = %v", err)
	}
	resolver.MarkHealthy(item, "https://rpc.example")
	if got := resolver.cache[item.ID].Endpoints; len(got) != 1 || got[0] != "https://rpc.example" {
		t.Fatalf("cached endpoints = %#v", got)
	}
}

func TestLegacyCacheDoesNotRestorePreviousTronDefault(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	data, err := json.Marshal(cacheFile{
		SchemaVersion: cacheSchemaVersion - 1,
		Networks: map[string]cacheEntry{
			"tron-mainnet": {
				Endpoints: []string{"https://api.trongrid.io"},
				Expires:   time.Now().Add(time.Hour),
			},
		},
	})
	if err != nil {
		t.Fatalf("encode legacy cache: %v", err)
	}
	if err := storage.AtomicWrite(filepath.Join(home, "cache", "rpc-nodes.json"), data); err != nil {
		t.Fatalf("AtomicWrite(cache) error = %v", err)
	}
	settings := settingsStub{
		home:     home,
		snapshot: config.SettingsSnapshot{Config: config.DefaultHomeConfig()},
	}
	settings.snapshot.Config.NodeDiscovery.Enabled = false
	resolver := New(settings)
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	item, err := registry.Get("tron-mainnet")
	if err != nil {
		t.Fatalf("registry.Get() error = %v", err)
	}

	got, err := resolver.Endpoints(t.Context(), item)
	if err != nil {
		t.Fatalf("Endpoints() error = %v", err)
	}
	if len(got) != 1 || got[0] != "https://tron-rpc.publicnode.com" {
		t.Fatalf("Endpoints() = %#v, want only PublicNode default", got)
	}
}

func TestUnsafeCachedEndpointIsNeverReturned(t *testing.T) {
	t.Parallel()

	settings := settingsStub{snapshot: config.SettingsSnapshot{Config: config.DefaultHomeConfig()}}
	settings.snapshot.Config.NodeDiscovery.Enabled = false
	resolver := New(settings)
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	item, err := registry.Get("ethereum-mainnet")
	if err != nil {
		t.Fatalf("registry.Get() error = %v", err)
	}
	resolver.cache[item.ID] = cacheEntry{
		Endpoints: []string{"file:///tmp/geth.ipc"},
		Expires:   time.Now().Add(time.Hour),
	}
	got, err := resolver.Endpoints(t.Context(), item)
	if err != nil {
		t.Fatalf("Endpoints() error = %v", err)
	}
	if len(got) != len(item.RPCFallbacks) || got[0] != item.RPCFallbacks[0] {
		t.Fatalf("Endpoints() = %#v, want only official fallback", got)
	}
}

func TestDisabledDiscoveryIgnoresSafeCachedEndpoint(t *testing.T) {
	t.Parallel()

	settings := settingsStub{snapshot: config.SettingsSnapshot{Config: config.DefaultHomeConfig()}}
	resolver := New(settings)
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	item, err := registry.Get("ethereum-mainnet")
	if err != nil {
		t.Fatalf("registry.Get() error = %v", err)
	}
	resolver.cache[item.ID] = cacheEntry{
		Endpoints: []string{"https://cached-discovery.example"},
		Expires:   time.Now().Add(time.Hour),
	}
	got, err := resolver.Endpoints(t.Context(), item)
	if err != nil {
		t.Fatalf("Endpoints() error = %v", err)
	}
	if len(got) != len(item.RPCFallbacks) || got[0] != item.RPCFallbacks[0] {
		t.Fatalf("Endpoints() = %#v, want only official fallback", got)
	}
}

func TestInvalidateRemovesEndpointFromMemoryAndDisk(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	resolver := New(settingsStub{
		home:     home,
		snapshot: config.SettingsSnapshot{Config: config.DefaultHomeConfig()},
	})
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	item, err := registry.Get("ethereum-mainnet")
	if err != nil {
		t.Fatalf("registry.Get() error = %v", err)
	}
	resolver.MarkHealthy(item, "https://custom.example")
	resolver.Invalidate(item.ID)
	if _, exists := resolver.cache[item.ID]; exists {
		t.Fatal("Invalidate() left the endpoint in memory")
	}

	data, err := os.ReadFile(filepath.Join(home, "cache", "rpc-nodes.json"))
	if err != nil {
		t.Fatalf("ReadFile(cache) error = %v", err)
	}
	var persisted cacheFile
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("decode cache: %v", err)
	}
	if _, exists := persisted.Networks[item.ID]; exists {
		t.Fatal("Invalidate() left the endpoint on disk")
	}
}

// A discovery service is untrusted, and every URL that survives extraction
// costs a DNS lookup before it can be judged. The response size cap alone
// bounds none of that: two megabytes of short URLs is ~100k of them.
func TestExtractURLsIsBounded(t *testing.T) {
	t.Parallel()

	t.Run("count", func(t *testing.T) {
		t.Parallel()

		flood := make([]any, 0, 50_000)
		for i := range 50_000 {
			flood = append(flood, fmt.Sprintf("https://node-%d.example", i))
		}
		got := extractURLs(map[string]any{"urls": flood})
		if len(got) > maxDiscoveryEndpoints {
			t.Errorf("extractURLs() returned %d endpoints, want at most %d", len(got), maxDiscoveryEndpoints)
		}
		if len(got) == 0 {
			t.Error("extractURLs() dropped everything, want the first few kept")
		}
	})

	t.Run("url length", func(t *testing.T) {
		t.Parallel()

		long := "https://" + strings.Repeat("a", maxDiscoveryURLLength) + ".example"
		if got := extractURLs(map[string]any{"url": long}); len(got) != 0 {
			t.Errorf("extractURLs() kept an oversized URL: %d entries", len(got))
		}
	})

	t.Run("depth", func(t *testing.T) {
		t.Parallel()

		// A document nested far past anything legitimate must not recurse
		// through all of it.
		var nested any = "https://deep.example"
		for range 5_000 {
			nested = map[string]any{"url": nested}
		}
		if got := extractURLs(nested); len(got) != 0 {
			t.Errorf("extractURLs() walked past the depth limit: %d entries", len(got))
		}
	})
}

// A provider credential belongs to the URL it was configured for. Endpoints()
// hands back the custom endpoints followed by the official fallbacks, so
// resolving headers per network — as this used to — sent one provider's key to
// whichever other node the pool moved on to when the first stopped answering.
func TestHeadersAreScopedToTheEndpointTheyBelongTo(t *testing.T) {
	t.Parallel()

	item := network.Network{ID: "ethereum-mainnet", Family: network.FamilyEVM}
	resolver := New(settingsStub{
		has: true,
		override: config.NetworkOverride{Endpoints: []config.Endpoint{
			{
				URL:     "https://paid.example/v3/abc",
				Headers: map[string]string{"Authorization": "Bearer secret"},
			},
			{URL: "https://second.example"},
		}},
	})

	tests := []struct {
		name     string
		endpoint string
		want     string
	}{
		{name: "configured", endpoint: "https://paid.example/v3/abc", want: "Bearer secret"},
		{name: "trailing slash", endpoint: "https://paid.example/v3/abc/", want: "Bearer secret"},
		{name: "explicit default port", endpoint: "https://paid.example:443/v3/abc", want: "Bearer secret"},
		{name: "other configured endpoint", endpoint: "https://second.example"},
		{name: "official fallback", endpoint: "https://ethereum-rpc.publicnode.com"},
		{name: "same host, another path", endpoint: "https://paid.example/v3/other"},
		{name: "same path, another host", endpoint: "https://attacker.example/v3/abc"},
		{name: "downgraded scheme", endpoint: "http://paid.example/v3/abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			headers, err := resolver.Headers(item, tt.endpoint)
			if err != nil {
				t.Fatalf("Headers() error = %v", err)
			}
			if got := headers.Get("Authorization"); got != tt.want {
				t.Errorf("Headers(%q).Authorization = %q, want %q", tt.endpoint, got, tt.want)
			}
		})
	}
}

// The dialer connects to the address it judged, so a name that verified as
// public and then answers with a private address on the next lookup does not
// get a connection. gRPC nodes reach it through GRPCDialContext.
func TestGRPCDialerRefusesPrivateAddresses(t *testing.T) {
	t.Parallel()

	resolver := New(settingsStub{})
	resolver.lookupIP = func(_ context.Context, host string) ([]net.IP, error) {
		if host == "rebind.example" {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}
		return []net.IP{net.ParseIP("169.254.169.254")}, nil
	}
	dial := resolver.GRPCDialContext(network.Network{ID: "tron-mainnet"})
	for _, address := range []string{
		"rebind.example:50051",
		"metadata.example:50051",
		"127.0.0.1:50051",
		"169.254.169.254:80",
	} {
		conn, err := dial(t.Context(), address)
		if err == nil {
			conn.Close()
			t.Errorf("GRPCDialContext() dialled %s", address)
		}
	}
}

// Go's To4 decodes only the IPv4-mapped form, so several ways of writing a
// private or loopback address as IPv6 sail through IsLoopback, IsPrivate and
// IsGlobalUnicast alike.
func TestPublicIPRejectsIPv6FormsOfPrivateAddresses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		address string
		want    bool
	}{
		{address: "2606:4700:4700::1111", want: true},
		{address: "::ffff:8.8.8.8", want: true},
		{address: "::127.0.0.1"},      // IPv4-compatible loopback
		{address: "::ffff:127.0.0.1"}, // IPv4-mapped loopback
		{address: "::ffff:169.254.169.254"},
		{address: "64:ff9b::7f00:1"},  // NAT64 loopback
		{address: "2002:7f00:1::"},    // 6to4 loopback
		{address: "2002:a9fe:a9fe::"}, // 6to4 link-local
		{address: "2001::1"},          // Teredo
		{address: "fd00::1"},
		{address: "fe80::1"},
		{address: "::1"},
	}
	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			t.Parallel()

			ip := net.ParseIP(tt.address)
			if ip == nil {
				t.Fatalf("ParseIP(%q) = nil", tt.address)
			}
			if got := publicIP(ip); got != tt.want {
				t.Errorf("publicIP(%s) = %v, want %v", tt.address, got, tt.want)
			}
		})
	}
}
