package config_test

import (
	"errors"
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

func TestNetworkHeadersAreRedacted(t *testing.T) {
	t.Parallel()

	manager, err := config.NewHomeManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewHomeManager() error = %v", err)
	}
	snapshot := manager.Snapshot()
	snapshot, err = manager.SaveNetwork("ethereum-mainnet", config.NetworkOverride{
		RPCURLs: []string{"https://rpc.example.invalid"},
		Headers: map[string]string{"Authorization": "secret"},
	}, snapshot.Revision)
	if err != nil {
		t.Fatalf("SaveNetwork() error = %v", err)
	}
	if snapshot.Networks["ethereum-mainnet"].Headers != nil {
		t.Error("secret headers leaked through settings snapshot")
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
		RPCURLs: []string{"http://rpc.example"},
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
		len(afterFailure.Networks["ethereum-mainnet"].RPCURLs) != 1 {
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
		Headers: map[string]string{"Authorization": "secret\r\nX-Injected: true"},
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
		RPCURLs: []string{"file:///tmp/geth.ipc"},
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
