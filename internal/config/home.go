package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/storage"
)

const HomeSchemaVersion = 1

type ServerSettings struct {
	Addr        string `json:"addr" yaml:"addr"`
	OpenBrowser bool   `json:"open_browser" yaml:"open_browser"`
}

type SecuritySettings struct {
	AutoLock time.Duration `json:"auto_lock" yaml:"-"`
}

type rawSecuritySettings struct {
	AutoLock string `yaml:"auto_lock"`
}

type DiscoverySettings struct {
	Enabled          bool          `json:"enabled" yaml:"enabled"`
	URL              string        `json:"url" yaml:"url"`
	RefreshInterval  time.Duration `json:"refresh_interval" yaml:"-"`
	RequestTimeout   time.Duration `json:"request_timeout" yaml:"-"`
	AllowInsecureRPC bool          `json:"allow_insecure_rpc" yaml:"allow_insecure_rpc"`
}

type rawDiscoverySettings struct {
	Enabled          bool   `yaml:"enabled"`
	URL              string `yaml:"url"`
	RefreshInterval  string `yaml:"refresh_interval"`
	RequestTimeout   string `yaml:"request_timeout"`
	AllowInsecureRPC bool   `yaml:"allow_insecure_rpc"`
}

type UISettings struct {
	LastSpaceID string `json:"last_space_id" yaml:"last_space_id"`
}

type HomeConfig struct {
	SchemaVersion int               `json:"schema_version" yaml:"schema_version"`
	Server        ServerSettings    `json:"server" yaml:"server"`
	Security      SecuritySettings  `json:"security" yaml:"-"`
	NodeDiscovery DiscoverySettings `json:"node_discovery" yaml:"-"`
	UI            UISettings        `json:"ui" yaml:"ui"`
}

type rawHomeConfig struct {
	SchemaVersion int                  `yaml:"schema_version"`
	Server        ServerSettings       `yaml:"server"`
	Security      rawSecuritySettings  `yaml:"security"`
	NodeDiscovery rawDiscoverySettings `yaml:"node_discovery"`
	UI            UISettings           `yaml:"ui"`
}

type NetworkOverride struct {
	Enabled    *bool             `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	RPCURLs    []string          `json:"rpc_urls,omitempty" yaml:"rpc_urls,omitempty"`
	Headers    map[string]string `json:"-" yaml:"headers,omitempty"`
	Explorer   *network.Explorer `json:"explorer,omitempty" yaml:"explorer,omitempty"`
	Discovery  *bool             `json:"discovery_enabled,omitempty" yaml:"discovery_enabled,omitempty"`
	HasHeaders bool              `json:"has_headers,omitempty" yaml:"-"`
}

type networkOverridesFile struct {
	SchemaVersion int                        `yaml:"schema_version"`
	Networks      map[string]NetworkOverride `yaml:"networks"`
}

type SettingsSnapshot struct {
	Config   HomeConfig                 `json:"config"`
	Networks map[string]NetworkOverride `json:"networks"`
	Revision string                     `json:"revision"`
}

var (
	ErrRevisionConflict = errors.New("settings revision conflict")
	ErrInvalidSettings  = errors.New("invalid settings")
)

type HomeManager struct {
	home     string
	mu       sync.RWMutex
	config   HomeConfig
	networks map[string]NetworkOverride
	revision string
}

func DefaultHomeConfig() HomeConfig {
	return HomeConfig{
		SchemaVersion: HomeSchemaVersion,
		Server:        ServerSettings{Addr: "127.0.0.1:8080", OpenBrowser: true},
		Security:      SecuritySettings{AutoLock: 15 * time.Minute},
		NodeDiscovery: DiscoverySettings{
			Enabled: false, URL: "",
			RefreshInterval: 30 * time.Minute, RequestTimeout: 5 * time.Second,
		},
		UI: UISettings{},
	}
}

func NewHomeManager(home string) (*HomeManager, error) {
	if err := storage.EnsureHome(home); err != nil {
		return nil, err
	}
	manager := &HomeManager{home: home}
	if err := manager.reload(); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *HomeManager) Home() string { return m.home }

func (m *HomeManager) Snapshot() SettingsSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return SettingsSnapshot{
		Config: m.config, Networks: redactedOverrides(m.networks), Revision: m.revision,
	}
}

func (m *HomeManager) NetworkOverride(id string) (NetworkOverride, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	override, ok := m.networks[id]
	if !ok {
		return NetworkOverride{}, false
	}
	return copyOverrides(map[string]NetworkOverride{id: override})[id], true
}

// ValidateNetwork checks an override against the currently active security
// settings without persisting it.
func (m *HomeManager) ValidateNetwork(id string, override NetworkOverride) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return validateNetworkOverride(id, override, m.config.NodeDiscovery.AllowInsecureRPC)
}

func (m *HomeManager) SaveConfig(next HomeConfig, expectedRevision string) (SettingsSnapshot, error) {
	if err := ValidateHomeConfig(next); err != nil {
		return SettingsSnapshot{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if expectedRevision != "" && expectedRevision != m.revision {
		return SettingsSnapshot{}, ErrRevisionConflict
	}
	for id, override := range m.networks {
		if err := validateNetworkOverride(id, override, next.NodeDiscovery.AllowInsecureRPC); err != nil {
			return SettingsSnapshot{}, fmt.Errorf("validate network override %s: %w", id, err)
		}
	}
	data, err := marshalConfig(next)
	if err != nil {
		return SettingsSnapshot{}, err
	}
	if err := storage.AtomicWrite(filepath.Join(m.home, "config.yaml"), data); err != nil {
		return SettingsSnapshot{}, err
	}
	m.config = next
	networkData, _ := marshalNetworkOverrides(m.networks)
	m.updateRevisionLocked(data, networkData)
	return SettingsSnapshot{Config: m.config, Networks: redactedOverrides(m.networks), Revision: m.revision}, nil
}

func (m *HomeManager) SaveNetwork(id string, override NetworkOverride, expectedRevision string) (SettingsSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := validateNetworkOverride(id, override, m.config.NodeDiscovery.AllowInsecureRPC); err != nil {
		return SettingsSnapshot{}, err
	}
	if expectedRevision != "" && expectedRevision != m.revision {
		return SettingsSnapshot{}, ErrRevisionConflict
	}
	next := copyOverrides(m.networks)
	if previous, ok := next[id]; ok && override.Headers == nil {
		override.Headers = previous.Headers
	}
	next[id] = override
	data, err := marshalNetworkOverrides(next)
	if err != nil {
		return SettingsSnapshot{}, fmt.Errorf("encode network settings: %w", err)
	}
	if err := storage.AtomicWrite(filepath.Join(m.home, "networks.yaml"), data); err != nil {
		return SettingsSnapshot{}, err
	}
	m.networks = next
	configData, _ := marshalConfig(m.config)
	m.updateRevisionLocked(configData, data)
	return SettingsSnapshot{Config: m.config, Networks: redactedOverrides(m.networks), Revision: m.revision}, nil
}

func (m *HomeManager) DeleteNetworkOverride(id, expectedRevision string) (SettingsSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if expectedRevision != "" && expectedRevision != m.revision {
		return SettingsSnapshot{}, ErrRevisionConflict
	}
	next := copyOverrides(m.networks)
	delete(next, id)
	data, err := marshalNetworkOverrides(next)
	if err != nil {
		return SettingsSnapshot{}, fmt.Errorf("encode network settings: %w", err)
	}
	if err := storage.AtomicWrite(filepath.Join(m.home, "networks.yaml"), data); err != nil {
		return SettingsSnapshot{}, err
	}
	m.networks = next
	configData, _ := marshalConfig(m.config)
	m.updateRevisionLocked(configData, data)
	return SettingsSnapshot{Config: m.config, Networks: redactedOverrides(m.networks), Revision: m.revision}, nil
}

func (m *HomeManager) reload() error {
	config := DefaultHomeConfig()
	configData, err := os.ReadFile(filepath.Join(m.home, "config.yaml"))
	if err == nil {
		config, err = unmarshalConfig(configData)
		if err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read home config: %w", err)
	} else {
		configData, _ = marshalConfig(config)
	}
	if err := ValidateHomeConfig(config); err != nil {
		return err
	}
	overrides := make(map[string]NetworkOverride)
	networkData, err := os.ReadFile(filepath.Join(m.home, "networks.yaml"))
	if err == nil {
		var file networkOverridesFile
		if err := yaml.Unmarshal(networkData, &file); err != nil {
			return fmt.Errorf("decode networks.yaml: %w", err)
		}
		if file.SchemaVersion != 1 {
			return fmt.Errorf("unsupported networks.yaml schema version %d", file.SchemaVersion)
		}
		overrides = file.Networks
		for id, override := range overrides {
			if err := validateNetworkOverride(id, override, config.NodeDiscovery.AllowInsecureRPC); err != nil {
				return fmt.Errorf("validate network override %s: %w", id, err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read network settings: %w", err)
	}
	m.config = config
	m.networks = overrides
	m.updateRevisionLocked(configData, networkData)
	return nil
}

func ValidateHomeConfig(config HomeConfig) error {
	if config.SchemaVersion != HomeSchemaVersion {
		return fmt.Errorf("%w: unsupported config schema version %d", ErrInvalidSettings, config.SchemaVersion)
	}
	host, _, err := net.SplitHostPort(config.Server.Addr)
	if err != nil {
		return fmt.Errorf("%w: invalid listen address: %v", ErrInvalidSettings, err)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("%w: server must listen on loopback", ErrInvalidSettings)
	}
	if config.Security.AutoLock < 0 {
		return fmt.Errorf("%w: auto-lock must not be negative", ErrInvalidSettings)
	}
	if config.NodeDiscovery.RefreshInterval <= 0 || config.NodeDiscovery.RequestTimeout <= 0 {
		return fmt.Errorf("%w: node discovery durations must be positive", ErrInvalidSettings)
	}
	if config.NodeDiscovery.URL == "" {
		if config.NodeDiscovery.Enabled {
			return fmt.Errorf("%w: node discovery URL is required when enabled", ErrInvalidSettings)
		}
		return nil
	}
	discoveryURL, err := url.Parse(config.NodeDiscovery.URL)
	if err != nil || discoveryURL.Scheme != "https" || discoveryURL.Host == "" || discoveryURL.User != nil {
		return fmt.Errorf("%w: node discovery URL must be a valid HTTPS URL", ErrInvalidSettings)
	}
	return nil
}

func marshalConfig(config HomeConfig) ([]byte, error) {
	raw := rawHomeConfig{
		SchemaVersion: config.SchemaVersion, Server: config.Server, UI: config.UI,
		Security: rawSecuritySettings{AutoLock: config.Security.AutoLock.String()},
		NodeDiscovery: rawDiscoverySettings{
			Enabled: config.NodeDiscovery.Enabled, URL: config.NodeDiscovery.URL,
			RefreshInterval:  config.NodeDiscovery.RefreshInterval.String(),
			RequestTimeout:   config.NodeDiscovery.RequestTimeout.String(),
			AllowInsecureRPC: config.NodeDiscovery.AllowInsecureRPC,
		},
	}
	data, err := yaml.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return data, nil
}

func unmarshalConfig(data []byte) (HomeConfig, error) {
	var raw rawHomeConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return HomeConfig{}, fmt.Errorf("decode config.yaml: %w", err)
	}
	autoLock, err := time.ParseDuration(raw.Security.AutoLock)
	if err != nil {
		return HomeConfig{}, fmt.Errorf("parse auto_lock: %w", err)
	}
	refresh, err := time.ParseDuration(raw.NodeDiscovery.RefreshInterval)
	if err != nil {
		return HomeConfig{}, fmt.Errorf("parse refresh_interval: %w", err)
	}
	timeout, err := time.ParseDuration(raw.NodeDiscovery.RequestTimeout)
	if err != nil {
		return HomeConfig{}, fmt.Errorf("parse request_timeout: %w", err)
	}
	return HomeConfig{
		SchemaVersion: raw.SchemaVersion, Server: raw.Server, UI: raw.UI,
		Security: SecuritySettings{AutoLock: autoLock},
		NodeDiscovery: DiscoverySettings{
			Enabled: raw.NodeDiscovery.Enabled, URL: raw.NodeDiscovery.URL,
			RefreshInterval: refresh, RequestTimeout: timeout,
			AllowInsecureRPC: raw.NodeDiscovery.AllowInsecureRPC,
		},
	}, nil
}

func validateRPCURL(value string, allowInsecure bool) error {
	resolved, err := ExpandValue(value)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	parsed, err := url.Parse(resolved)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("%w: invalid RPC URL: %s", ErrInvalidSettings, value)
	}
	if parsed.Scheme == "https" || (allowInsecure && parsed.Scheme == "http") {
		return nil
	}
	return fmt.Errorf("%w: RPC URL must use HTTPS: %s", ErrInvalidSettings, value)
}

func validateNetworkOverride(id string, override NetworkOverride, allowInsecure bool) error {
	registry, err := network.Builtin()
	if err != nil {
		return err
	}
	if _, err := registry.Get(id); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	for _, endpoint := range override.RPCURLs {
		if err := validateRPCURL(endpoint, allowInsecure); err != nil {
			return err
		}
	}
	if override.Explorer != nil {
		for kind, template := range map[string]string{
			"address": override.Explorer.Address,
			"tx":      override.Explorer.Tx,
			"block":   override.Explorer.Block,
		} {
			if template == "" {
				continue
			}
			parsed, err := url.Parse(template)
			if err != nil || parsed.Scheme != "https" || parsed.Host == "" ||
				parsed.User != nil || !strings.Contains(template, "{"+kind+"}") {
				return fmt.Errorf(
					"%w: explorer %s template must be HTTPS and contain {%s}",
					ErrInvalidSettings, kind, kind,
				)
			}
		}
	}
	for name, value := range override.Headers {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t\r\n:") ||
			value == "" || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%w: provider headers contain an invalid name or empty value", ErrInvalidSettings)
		}
	}
	return nil
}

func copyOverrides(source map[string]NetworkOverride) map[string]NetworkOverride {
	out := make(map[string]NetworkOverride, len(source))
	for id, override := range source {
		override.RPCURLs = append([]string(nil), override.RPCURLs...)
		if override.Enabled != nil {
			value := *override.Enabled
			override.Enabled = &value
		}
		if override.Discovery != nil {
			value := *override.Discovery
			override.Discovery = &value
		}
		if override.Explorer != nil {
			value := *override.Explorer
			override.Explorer = &value
		}
		if override.Headers != nil {
			override.Headers = make(map[string]string, len(override.Headers))
			for name, value := range source[id].Headers {
				override.Headers[name] = value
			}
		}
		out[id] = override
	}
	return out
}

func redactedOverrides(source map[string]NetworkOverride) map[string]NetworkOverride {
	out := copyOverrides(source)
	for id, override := range out {
		override.HasHeaders = len(override.Headers) > 0
		override.Headers = nil
		out[id] = override
	}
	return out
}

func marshalNetworkOverrides(overrides map[string]NetworkOverride) ([]byte, error) {
	data, err := yaml.Marshal(networkOverridesFile{SchemaVersion: 1, Networks: overrides})
	if err != nil {
		return nil, fmt.Errorf("encode network settings: %w", err)
	}
	return data, nil
}

func (m *HomeManager) updateRevisionLocked(configData, networkData []byte) {
	sum := sha256.New()
	_, _ = sum.Write(configData)
	_, _ = sum.Write(networkData)
	m.revision = hex.EncodeToString(sum.Sum(nil)[:16])
}

func ExpandValue(value string) (string, error) {
	var missing string
	expanded := os.Expand(value, func(name string) string {
		resolved, ok := os.LookupEnv(name)
		if !ok && missing == "" {
			missing = name
		}
		return resolved
	})
	if missing != "" {
		return "", fmt.Errorf("environment variable %s is not set", missing)
	}
	return expanded, nil
}
