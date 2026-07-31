package rpcpool

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
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
	}
	for _, test := range tests {
		test := test
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
