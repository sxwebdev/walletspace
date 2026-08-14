package config_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/storage"
)

func TestHomeManagerDefaultsAndRevisionConflicts(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager, err := config.NewHomeManager(home)
	if err != nil {
		t.Fatalf("NewHomeManager() error = %v", err)
	}
	initial := manager.Snapshot()
	if initial.Config.NodeDiscovery.Enabled || initial.Config.NodeDiscovery.URL != "" {
		t.Fatalf("node discovery defaults = %+v, want disabled with empty URL", initial.Config.NodeDiscovery)
	}
	next := initial.Config
	next.Security.AutoLock = 30 * time.Minute
	next.NodeDiscovery.Enabled = true
	next.NodeDiscovery.URL = "https://discovery.example"
	saved, err := manager.SaveConfig(next, initial.Revision)
	if err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	if saved.Revision == initial.Revision {
		t.Error("revision did not change")
	}
	if _, err := manager.SaveConfig(initial.Config, initial.Revision); !errors.Is(err, config.ErrRevisionConflict) {
		t.Fatalf("stale SaveConfig() error = %v", err)
	}
	info, err := os.Stat(filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatalf("Stat(config.yaml) error = %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config mode = %o", info.Mode().Perm())
	}
	contents, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(config.yaml) error = %v", err)
	}
	if strings.Contains(string(contents), "last_network_id") {
		t.Error("config still persists a global default network")
	}
	reloaded, err := config.NewHomeManager(home)
	if err != nil {
		t.Fatalf("NewHomeManager(saved settings) error = %v", err)
	}
	if discovery := reloaded.Snapshot().Config.NodeDiscovery; !discovery.Enabled || discovery.URL != "https://discovery.example" {
		t.Fatalf("reloaded node discovery = %+v", discovery)
	}
}

func TestInvalidHomeSettingsHaveTypedError(t *testing.T) {
	t.Parallel()

	current := config.DefaultHomeConfig()
	current.Server.Addr = "0.0.0.0:8080"
	if err := config.ValidateHomeConfig(current); !errors.Is(err, config.ErrInvalidSettings) {
		t.Fatalf("ValidateHomeConfig() error = %v, want ErrInvalidSettings", err)
	}
	current = config.DefaultHomeConfig()
	current.NodeDiscovery.Enabled = true
	if err := config.ValidateHomeConfig(current); !errors.Is(err, config.ErrInvalidSettings) {
		t.Fatalf("ValidateHomeConfig() missing discovery URL error = %v, want ErrInvalidSettings", err)
	}
	current = config.DefaultHomeConfig()
	current.NodeDiscovery.URL = "https://"
	if err := config.ValidateHomeConfig(current); !errors.Is(err, config.ErrInvalidSettings) {
		t.Fatalf("ValidateHomeConfig() malformed URL error = %v, want ErrInvalidSettings", err)
	}
}

// Discovery is polled on a timer, so a URL aimed at the local machine turns the
// wallet into a scheduled client of whatever is listening there. The dialer
// refuses the connection anyway; this is about failing while the person who
// typed it is still looking at the form.
func TestLocalDiscoveryURLsAreRefused(t *testing.T) {
	t.Parallel()

	local := []string{
		"https://127.0.0.1:9000/",
		"https://localhost/nodes",
		"https://[::1]:9000/",
		"https://192.168.1.10/",
		"https://169.254.169.254/latest/meta-data",
		"https://gateway.local/",
		// Go's To4 decodes only the IPv4-mapped form, so every spelling below
		// reads as ordinary global unicast to IsLoopback, IsPrivate and
		// IsGlobalUnicast alike — including the 6to4 wrapping of
		// 169.254.169.254, the cloud metadata address. They passed here while
		// the dialer refused them at connect time, because this check kept a
		// second, shorter opinion of what "public" means instead of sharing the
		// dialer's.
		"https://[::127.0.0.1]/nodes",      // IPv4-compatible loopback
		"https://[::ffff:127.0.0.1]/nodes", // IPv4-mapped loopback
		"https://[64:ff9b::7f00:1]/nodes",  // NAT64 loopback
		"https://[2002:7f00:1::]/nodes",    // 6to4 loopback
		"https://[2002:a9fe:a9fe::]/nodes", // 6to4 link-local
		"https://[2001::1]/nodes",          // Teredo
		"https://100.64.1.1/nodes",         // CGNAT
	}
	for _, value := range local {
		current := config.DefaultHomeConfig()
		current.NodeDiscovery.Enabled = true
		current.NodeDiscovery.URL = value
		if err := config.ValidateHomeConfig(current); !errors.Is(err, config.ErrInvalidSettings) {
			t.Errorf("ValidateHomeConfig(%q) error = %v, want ErrInvalidSettings", value, err)
		}

		// Unless the user has said, in as many words, that private RPC is what
		// they want — which is the same switch that lets the dialer through.
		permissive := current
		permissive.NodeDiscovery.AllowInsecureRPC = true
		if err := config.ValidateHomeConfig(permissive); err != nil {
			t.Errorf("ValidateHomeConfig(%q, insecure allowed) error = %v, want nil", value, err)
		}
	}

	public := config.DefaultHomeConfig()
	public.NodeDiscovery.Enabled = true
	public.NodeDiscovery.URL = "https://discovery.example/nodes"
	if err := config.ValidateHomeConfig(public); err != nil {
		t.Errorf("ValidateHomeConfig(public host) error = %v, want nil", err)
	}
}

// writeStoredConfig lays down a config.yaml the way an older wallet would have
// left it, so the test starts from a file on disk rather than from a struct the
// validator has already been over.
func writeStoredConfig(
	t *testing.T, home string, enabled bool, discoveryURL string, allowInsecure bool,
) {
	t.Helper()

	body := fmt.Sprintf(`schema_version: 1
server:
  addr: 127.0.0.1:0
  open_browser: true
security:
  auto_lock: 15m0s
  confirm_sends: true
  send_grant_ttl: 5m0s
node_discovery:
  enabled: %t
  url: %q
  refresh_interval: 30m0s
  request_timeout: 5s
  allow_insecure_rpc: %t
ui:
  last_space_id: ""
`, enabled, discoveryURL, allowInsecure)
	if err := storage.AtomicWrite(filepath.Join(home, "config.yaml"), []byte(body)); err != nil {
		t.Fatalf("AtomicWrite(config.yaml) error = %v", err)
	}
}

// A value already on disk must never be the reason the wallet will not start.
//
// The discovery URL is edited on the /settings page, which is served by the
// process that a refused config.yaml stops from ever opening its port — so
// rejecting the file leaves hand-editing YAML as the only way back in.
// Auto-lock settled the shape of this first: clamp what is stored on disk, and
// refuse what someone is trying to save.
func TestAStoredPrivateDiscoveryURLStillStartsTheWallet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		enabled       bool
		url           string
		allowInsecure bool
		wantEnabled   bool
		wantURL       string
	}{
		{name: "private URL with discovery off", url: "https://192.168.1.50/nodes"},
		{name: "private URL with discovery on", enabled: true, url: "https://192.168.1.50/nodes"},
		{
			name: "public URL is left alone", enabled: true,
			url:         "https://discovery.example/nodes",
			wantEnabled: true, wantURL: "https://discovery.example/nodes",
		},
		{
			// Someone who has said in as many words that private RPC is what
			// they want keeps it across a restart. The clamp repairs what the
			// validator would refuse and nothing else, so it must read the same
			// switch the validator reads rather than the host alone.
			name: "private URL survives when insecure RPC is allowed", enabled: true,
			url:           "https://192.168.1.50/nodes",
			allowInsecure: true,
			wantEnabled:   true, wantURL: "https://192.168.1.50/nodes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			home := t.TempDir()
			writeStoredConfig(t, home, tt.enabled, tt.url, tt.allowInsecure)

			manager, err := config.NewHomeManager(home)
			if err != nil {
				t.Fatalf("NewHomeManager() error = %v", err)
			}
			// Dropped and switched off, not merely tolerated: the rule exists to
			// stop a timer polling that host, so honouring the stored value would
			// answer the startup problem by reopening the one the rule is for.
			discovery := manager.Snapshot().Config.NodeDiscovery
			if discovery.Enabled != tt.wantEnabled || discovery.URL != tt.wantURL {
				t.Errorf(
					"loaded discovery = {Enabled:%v URL:%q}, want {Enabled:%v URL:%q}",
					discovery.Enabled, discovery.URL, tt.wantEnabled, tt.wantURL,
				)
			}
			// The rest of the block survives, so the clamp repairs one field
			// rather than resetting the section around it.
			if discovery.RefreshInterval != 30*time.Minute ||
				discovery.RequestTimeout != 5*time.Second {
				t.Errorf("clamp reset the surrounding discovery settings: %+v", discovery)
			}
		})
	}
}

// The same lockout, reached through the timings rather than the address. A
// stored zero is refused by the validator exactly as a private host is, and a
// hand-edited file can hold one — so it has to be repaired on load too. Unlike
// an address, a duration has a nearest legal value, so these take the shipped
// default instead of being dropped.
func TestStoredDiscoveryTimingsCannotStopTheWallet(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	body := `schema_version: 1
server:
  addr: 127.0.0.1:0
  open_browser: true
security:
  auto_lock: 15m0s
  confirm_sends: true
  send_grant_ttl: 5m0s
node_discovery:
  enabled: true
  url: "https://discovery.example/nodes"
  refresh_interval: 0s
  request_timeout: -1s
  allow_insecure_rpc: false
ui:
  last_space_id: ""
`
	if err := storage.AtomicWrite(filepath.Join(home, "config.yaml"), []byte(body)); err != nil {
		t.Fatalf("AtomicWrite(config.yaml) error = %v", err)
	}

	manager, err := config.NewHomeManager(home)
	if err != nil {
		t.Fatalf("NewHomeManager() error = %v, want the wallet to start", err)
	}
	discovery := manager.Snapshot().Config.NodeDiscovery
	defaults := config.DefaultHomeConfig().NodeDiscovery
	if discovery.RefreshInterval != defaults.RefreshInterval ||
		discovery.RequestTimeout != defaults.RequestTimeout {
		t.Errorf("loaded timings = %s/%s, want the defaults %s/%s",
			discovery.RefreshInterval, discovery.RequestTimeout,
			defaults.RefreshInterval, defaults.RequestTimeout)
	}
	// Repaired, not reset: the URL beside them is legal and stays.
	if !discovery.Enabled || discovery.URL != "https://discovery.example/nodes" {
		t.Errorf("clamp disturbed the rest of the block: %+v", discovery)
	}

	// And a zero arriving through the API is still refused, so the person
	// typing it finds out rather than silently getting something else.
	next := manager.Snapshot().Config
	next.NodeDiscovery.RequestTimeout = 0
	if _, err := manager.SaveConfig(next, manager.Snapshot().Revision); !errors.Is(err, config.ErrInvalidSettings) {
		t.Errorf("SaveConfig(zero timeout) error = %v, want ErrInvalidSettings", err)
	}
}

// Clamping on load must not become clamping everywhere. A URL arriving through
// the settings API is refused outright, so whoever typed it finds out while
// they are still looking at the form — and finds out what to do about it.
func TestSavingAPrivateDiscoveryURLIsStillRefused(t *testing.T) {
	t.Parallel()

	manager, err := config.NewHomeManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewHomeManager() error = %v", err)
	}
	snapshot := manager.Snapshot()

	// Refused whether or not discovery is on, for the reason the send
	// confirmation window is checked whether or not the step-up is on: a value
	// rejected only once someone enables the feature fails at the least useful
	// moment, and the form shows the field either way.
	for _, enabled := range []bool{true, false} {
		next := snapshot.Config
		next.NodeDiscovery.Enabled = enabled
		next.NodeDiscovery.URL = "https://192.168.1.50/nodes"
		_, err := manager.SaveConfig(next, snapshot.Revision)
		if !errors.Is(err, config.ErrInvalidSettings) {
			t.Fatalf("SaveConfig(enabled=%v) error = %v, want ErrInvalidSettings", enabled, err)
		}
		if !strings.Contains(err.Error(), "insecure") {
			t.Errorf("SaveConfig() error = %q, does not name the way out", err)
		}
	}
	if after := manager.Snapshot().Config.NodeDiscovery; after.Enabled || after.URL != "" {
		t.Errorf("a refused save reached the stored settings: %+v", after)
	}
}

func TestNetworkHeadersAreRedacted(t *testing.T) {
	t.Parallel()

	manager, err := config.NewHomeManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewHomeManager() error = %v", err)
	}
	snapshot := manager.Snapshot()
	snapshot, err = manager.SaveNetwork("ethereum-mainnet", config.NetworkOverride{
		Endpoints: []config.Endpoint{{
			URL:     "https://rpc.example.invalid",
			Headers: map[string]string{"Authorization": "secret"},
		}},
	}, snapshot.Revision)
	if err != nil {
		t.Fatalf("SaveNetwork() error = %v", err)
	}
	stored := snapshot.Networks["ethereum-mainnet"].Endpoints
	if len(stored) != 1 {
		t.Fatalf("stored endpoints = %+v", stored)
	}
	if stored[0].Headers != nil {
		t.Error("secret headers leaked through settings snapshot")
	}
	if !stored[0].HasHeaders {
		t.Error("snapshot does not report that a credential is stored")
	}
}

func TestTighteningRPCPolicyRejectsExistingInsecureOverrides(t *testing.T) {
	t.Parallel()

	manager, err := config.NewHomeManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewHomeManager() error = %v", err)
	}
	snapshot := manager.Snapshot()
	permissive := snapshot.Config
	permissive.NodeDiscovery.AllowInsecureRPC = true
	snapshot, err = manager.SaveConfig(permissive, snapshot.Revision)
	if err != nil {
		t.Fatalf("SaveConfig(permissive) error = %v", err)
	}
	snapshot, err = manager.SaveNetwork("ethereum-mainnet", config.NetworkOverride{
		Endpoints: []config.Endpoint{{URL: "http://rpc.example"}},
	}, snapshot.Revision)
	if err != nil {
		t.Fatalf("SaveNetwork(http) error = %v", err)
	}

	strict := snapshot.Config
	strict.NodeDiscovery.AllowInsecureRPC = false
	if _, err := manager.SaveConfig(strict, snapshot.Revision); !errors.Is(err, config.ErrInvalidSettings) {
		t.Fatalf("SaveConfig(strict) error = %v, want ErrInvalidSettings", err)
	}
	afterFailure := manager.Snapshot()
	if !afterFailure.Config.NodeDiscovery.AllowInsecureRPC ||
		len(afterFailure.Networks["ethereum-mainnet"].Endpoints) != 1 {
		t.Fatalf("failed policy change mutated settings: %+v", afterFailure)
	}

	snapshot, err = manager.DeleteNetworkOverride("ethereum-mainnet", afterFailure.Revision)
	if err != nil {
		t.Fatalf("DeleteNetworkOverride() error = %v", err)
	}
	strict = snapshot.Config
	strict.NodeDiscovery.AllowInsecureRPC = false
	if _, err := manager.SaveConfig(strict, snapshot.Revision); err != nil {
		t.Fatalf("SaveConfig(strict after cleanup) error = %v", err)
	}
}

func TestInvalidNetworkFileIsRejectedOnStartup(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	if err := storage.AtomicWrite(filepath.Join(home, "networks.yaml"), []byte(`
schema_version: 1
networks:
  unknown-mainnet:
    rpc_urls:
      - http://127.0.0.1:8545
`)); err != nil {
		t.Fatalf("AtomicWrite(networks.yaml) error = %v", err)
	}
	if _, err := config.NewHomeManager(home); err == nil {
		t.Fatal("NewHomeManager() accepted an unknown and insecure network override")
	}
}

func TestProviderHeaderInjectionIsRejected(t *testing.T) {
	t.Parallel()

	manager, err := config.NewHomeManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewHomeManager() error = %v", err)
	}
	snapshot := manager.Snapshot()
	_, err = manager.SaveNetwork("ethereum-mainnet", config.NetworkOverride{
		Endpoints: []config.Endpoint{{
			URL:     "https://rpc.example.invalid",
			Headers: map[string]string{"Authorization": "secret\r\nX-Injected: true"},
		}},
	}, snapshot.Revision)
	if err == nil {
		t.Fatal("SaveNetwork() accepted a header value containing CRLF")
	}
	if _, exists := manager.Snapshot().Networks["ethereum-mainnet"]; exists {
		t.Fatal("invalid override mutated settings")
	}
}

func TestNetworkValidationRunsWithoutPersisting(t *testing.T) {
	t.Parallel()

	manager, err := config.NewHomeManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewHomeManager() error = %v", err)
	}
	if err := manager.ValidateNetwork("ethereum-mainnet", config.NetworkOverride{
		Endpoints: []config.Endpoint{{URL: "file:///tmp/geth.ipc"}},
	}); err == nil {
		t.Fatal("ValidateNetwork() accepted an IPC endpoint")
	}
	if len(manager.Snapshot().Networks) != 0 {
		t.Fatal("ValidateNetwork() persisted an override")
	}
}

func TestNetworkSnapshotsDoNotAliasManagerState(t *testing.T) {
	t.Parallel()

	manager, err := config.NewHomeManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewHomeManager() error = %v", err)
	}
	enabled := true
	discovery := false
	snapshot := manager.Snapshot()
	saved, err := manager.SaveNetwork("ethereum-mainnet", config.NetworkOverride{
		Enabled: &enabled, Discovery: &discovery,
		Explorer: &network.Explorer{Tx: "https://example.test/tx/{tx}"},
	}, snapshot.Revision)
	if err != nil {
		t.Fatalf("SaveNetwork() error = %v", err)
	}
	external := saved.Networks["ethereum-mainnet"]
	*external.Enabled = false
	*external.Discovery = true
	external.Explorer.Tx = "https://mutated.test/tx/{tx}"

	internal, ok := manager.NetworkOverride("ethereum-mainnet")
	if !ok {
		t.Fatal("NetworkOverride() missing")
	}
	if !*internal.Enabled || *internal.Discovery ||
		internal.Explorer.Tx != "https://example.test/tx/{tx}" {
		t.Fatalf("manager state was mutated through snapshot: %+v", internal)
	}
}

// The Tron node list separates entries on a comma, so a saved RPC URL carrying
// one is checked here as a single HTTPS endpoint and could be taken downstream
// as several — the extras never having been validated or probed.
func TestSaveNetworkRejectsDelimitersInAnRPCURL(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	manager, err := config.NewHomeManager(home)
	if err != nil {
		t.Fatalf("NewHomeManager() error = %v", err)
	}

	for _, rpcURL := range []string{
		"https://public.example/rpc,grpc://127.0.0.1:50051",
		"https://public.example/rpc|1",
	} {
		_, err := manager.SaveNetwork("tron-nile", config.NetworkOverride{
			Endpoints: []config.Endpoint{{URL: rpcURL}},
		}, "")
		if err == nil {
			t.Errorf("SaveNetwork(%q) accepted a delimiter", rpcURL)
			continue
		}
		if !errors.Is(err, config.ErrInvalidSettings) {
			t.Errorf("SaveNetwork(%q) error = %v, want ErrInvalidSettings", rpcURL, err)
		}
	}

	if _, err := manager.SaveNetwork("tron-nile", config.NetworkOverride{
		Endpoints: []config.Endpoint{{URL: "https://nile.trongrid.io"}},
	}, ""); err != nil {
		t.Errorf("SaveNetwork() rejected an ordinary URL: %v", err)
	}
}

// An RPC URL is written in configuration as an ${ENV} reference, but by the
// time it reaches an error message, a log line or the endpoint cache it has
// been expanded — and providers put the key in the path, the query or the
// userinfo. None of those may survive.
func TestRedactURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain host is kept whole", in: "https://tron-rpc.publicnode.com", want: "https://tron-rpc.publicnode.com"},
		{name: "key in the path", in: "https://mainnet.infura.io/v3/0123456789abcdef", want: "https://mainnet.infura.io/…"},
		{name: "key in the query", in: "https://rpc.example?apikey=0123456789", want: "https://rpc.example?…"},
		// url.URL.Host excludes the userinfo, so the credential is dropped
		// rather than shortened.
		{name: "key in the userinfo", in: "https://user:secret@rpc.example", want: "https://rpc.example?…"},
		{name: "unparsable", in: "::not a url::", want: "[redacted endpoint]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := config.RedactURL(tt.in)
			if got != tt.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRedactErrorStripsEveryEndpoint(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf(
		"verify RPC https://mainnet.infura.io/v3/SECRETKEY: dial https://other.example?token=SECRET2 failed",
	)
	got := config.RedactError(err).Error()

	for _, secret := range []string{"SECRETKEY", "SECRET2"} {
		if strings.Contains(got, secret) {
			t.Errorf("RedactError() = %q, still contains %q", got, secret)
		}
	}
	if !strings.Contains(got, "mainnet.infura.io") {
		t.Errorf("RedactError() = %q, want the provider host kept for diagnosis", got)
	}
	if config.RedactError(nil) != nil {
		t.Error("RedactError(nil) should stay nil")
	}
}

// A schema 1 file has one header map per network, unattached to any URL. There
// is no record of which endpoint it was meant for, so the migration takes the
// narrowest reading available: the first custom URL, the one the user typed the
// secret next to. Later URLs, the official fallbacks and anything discovery
// suggests get nothing.
func TestNetworksFileMigratesHeadersOntoTheFirstEndpoint(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	if err := storage.AtomicWrite(filepath.Join(home, "networks.yaml"), []byte(`
schema_version: 1
networks:
  ethereum-mainnet:
    enabled: true
    rpc_urls:
      - https://paid.example/v3/abc
      - https://spare.example
    headers:
      Authorization: Bearer secret
`)); err != nil {
		t.Fatalf("AtomicWrite(networks.yaml) error = %v", err)
	}
	manager, err := config.NewHomeManager(home)
	if err != nil {
		t.Fatalf("NewHomeManager() error = %v", err)
	}
	override, ok := manager.NetworkOverride("ethereum-mainnet")
	if !ok {
		t.Fatal("NetworkOverride() missing after migration")
	}
	if got := override.URLs(); len(got) != 2 ||
		got[0] != "https://paid.example/v3/abc" || got[1] != "https://spare.example" {
		t.Fatalf("URLs() = %#v", got)
	}
	if override.Endpoints[0].Headers["Authorization"] != "Bearer secret" {
		t.Errorf("credential did not migrate: %#v", override.Endpoints[0].Headers)
	}
	if len(override.Endpoints[1].Headers) != 0 {
		t.Errorf("credential spread to a second endpoint: %#v", override.Endpoints[1].Headers)
	}
	if override.Enabled == nil || !*override.Enabled {
		t.Error("enabled flag did not survive migration")
	}

	// Rewritten on disk, so the credential stops claiming a scope the code no
	// longer gives it, and a second start reads the file it just wrote.
	written, err := os.ReadFile(filepath.Join(home, "networks.yaml"))
	if err != nil {
		t.Fatalf("ReadFile(networks.yaml) error = %v", err)
	}
	if !strings.Contains(string(written), "schema_version: 2") ||
		strings.Contains(string(written), "rpc_urls") {
		t.Fatalf("networks.yaml was not migrated in place:\n%s", written)
	}
	reopened, err := config.NewHomeManager(home)
	if err != nil {
		t.Fatalf("NewHomeManager(reopen) error = %v", err)
	}
	again, _ := reopened.NetworkOverride("ethereum-mainnet")
	if len(again.Endpoints) != 2 ||
		again.Endpoints[0].Headers["Authorization"] != "Bearer secret" {
		t.Fatalf("migrated file did not round-trip: %#v", again.Endpoints)
	}
}

// The API never returns a stored credential, so a UI saving a network cannot
// send one back. An absent header map has to mean "leave it alone" — and an
// empty one has to still mean "delete it", or a secret could never be removed.
func TestSavingANetworkKeepsCredentialsItWasNotGiven(t *testing.T) {
	t.Parallel()

	manager, err := config.NewHomeManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewHomeManager() error = %v", err)
	}
	if _, err := manager.SaveNetwork("ethereum-mainnet", config.NetworkOverride{
		Endpoints: []config.Endpoint{
			{URL: "https://paid.example", Headers: map[string]string{"Authorization": "secret"}},
			{URL: "https://spare.example"},
		},
	}, ""); err != nil {
		t.Fatalf("SaveNetwork() error = %v", err)
	}

	if _, err := manager.SaveNetwork("ethereum-mainnet", config.NetworkOverride{
		Endpoints: []config.Endpoint{
			{URL: "https://paid.example"},
			{URL: "https://spare.example"},
		},
	}, ""); err != nil {
		t.Fatalf("SaveNetwork(no headers) error = %v", err)
	}
	kept, _ := manager.NetworkOverride("ethereum-mainnet")
	if kept.Endpoints[0].Headers["Authorization"] != "secret" {
		t.Errorf("a credential the browser was never shown was dropped: %#v", kept.Endpoints[0])
	}

	if _, err := manager.SaveNetwork("ethereum-mainnet", config.NetworkOverride{
		Endpoints: []config.Endpoint{{URL: "https://paid.example", Headers: map[string]string{}}},
	}, ""); err != nil {
		t.Fatalf("SaveNetwork(clear) error = %v", err)
	}
	cleared, _ := manager.NetworkOverride("ethereum-mainnet")
	if len(cleared.Endpoints[0].Headers) != 0 {
		t.Errorf("an explicitly cleared credential survived: %#v", cleared.Endpoints[0])
	}
}

// Two entries for one endpoint would make "which credentials does this URL
// get?" ambiguous, and that answer decides where a secret is sent.
func TestSaveNetworkRejectsADuplicateEndpoint(t *testing.T) {
	t.Parallel()

	manager, err := config.NewHomeManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewHomeManager() error = %v", err)
	}
	_, err = manager.SaveNetwork("ethereum-mainnet", config.NetworkOverride{
		Endpoints: []config.Endpoint{
			{URL: "https://paid.example/v3", Headers: map[string]string{"Authorization": "one"}},
			{URL: "https://paid.example:443/v3/", Headers: map[string]string{"Authorization": "two"}},
		},
	}, "")
	if !errors.Is(err, config.ErrInvalidSettings) {
		t.Fatalf("SaveNetwork() error = %v, want ErrInvalidSettings", err)
	}
}

// Redaction rewrites the message, not the error. Callers wrap the result —
// verifyRPCs does — and hand it to the HTTP layer, which picks a status code by
// matching sentinels; returning a bare errors.New here cut that chain, so a
// probe that timed out was reported with the wrong status.
func TestRedactErrorHidesTheEndpointButKeepsTheCause(t *testing.T) {
	t.Parallel()

	cause := fmt.Errorf(
		"read https://provider.example/v3/secret-key: %w", context.DeadlineExceeded,
	)
	redacted := config.RedactError(cause)

	if strings.Contains(redacted.Error(), "secret-key") {
		t.Errorf("RedactError() = %q, still carries the credential", redacted)
	}
	if !strings.Contains(redacted.Error(), "https://provider.example") {
		t.Errorf("RedactError() = %q, dropped the host that names the provider", redacted)
	}
	if !errors.Is(redacted, context.DeadlineExceeded) {
		t.Error("RedactError() broke the chain; sentinel-based status mapping cannot match")
	}
	// And once a caller wraps it again, which is the shape that actually reaches
	// the HTTP layer.
	wrapped := fmt.Errorf("verify RPC %s: %w", config.RedactURL("https://x.example"), redacted)
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Error("the chain does not survive being wrapped by the caller")
	}
	if strings.Contains(wrapped.Error(), "secret-key") {
		t.Errorf("wrapped error = %q, leaks the credential", wrapped)
	}
}
