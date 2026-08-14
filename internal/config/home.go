package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/sxwebdev/walletspace/internal/hostpolicy"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/storage"
)

const HomeSchemaVersion = 1

// Auto-lock is the only thing that takes a decrypted seed back out of memory
// after the user walks away, so it cannot be switched off. The bounds leave
// room for both a cautious and a relaxed setting without allowing "never".
const (
	MinAutoLock = time.Minute
	MaxAutoLock = 24 * time.Hour

	// How long one spending confirmation lasts.
	//
	// The window is time in which anything holding the capability token — an
	// injected script, another local process — can move funds without knowing
	// the password, so it is short by default and cannot be made long. The
	// floor is there because a window shorter than reading a confirmation
	// screen is a password prompt per transfer under another name.
	DefaultSendGrantTTL = 5 * time.Minute
	MinSendGrantTTL     = time.Minute
	MaxSendGrantTTL     = time.Hour
)

// ClampSendGrantTTL brings a configured spending window inside the allowed range.
func ClampSendGrantTTL(value time.Duration) time.Duration {
	switch {
	case value < MinSendGrantTTL:
		return MinSendGrantTTL
	case value > MaxSendGrantTTL:
		return MaxSendGrantTTL
	default:
		return value
	}
}

// ClampAutoLock brings a stored value inside the allowed range.
func ClampAutoLock(value time.Duration) time.Duration {
	switch {
	case value < MinAutoLock:
		return MinAutoLock
	case value > MaxAutoLock:
		return MaxAutoLock
	default:
		return value
	}
}

type ServerSettings struct {
	Addr        string `json:"addr" yaml:"addr"`
	OpenBrowser bool   `json:"open_browser" yaml:"open_browser"`
}

type SecuritySettings struct {
	AutoLock time.Duration `json:"auto_lock" yaml:"-"`
	// ConfirmSends asks for the space password before funds move, and
	// SendGrantTTL is how long one answer covers. Unlocking a space proves who
	// was there when it was unlocked; this is what a transfer is answerable to
	// instead.
	ConfirmSends bool          `json:"confirm_sends" yaml:"-"`
	SendGrantTTL time.Duration `json:"send_grant_ttl" yaml:"-"`
}

type rawSecuritySettings struct {
	AutoLock string `yaml:"auto_lock"`
	// A pointer so that a file written before this setting existed is read as
	// "not stated" and takes the default, rather than as an explicit false.
	// Turning the step-up off has to be something someone did on purpose.
	ConfirmSends *bool  `yaml:"confirm_sends"`
	SendGrantTTL string `yaml:"send_grant_ttl"`
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

// Endpoint is one RPC URL together with the credentials that belong to that
// URL and to nothing else.
//
// The credentials used to be a single map per network, unattached to any
// particular URL, which meant a provider key was sent to whichever endpoint the
// resolver happened to fall back to — an official public node, or something
// node discovery had suggested. Binding them here is what makes it possible to
// answer "does this request get the secret?" by looking at the request.
type Endpoint struct {
	URL     string            `json:"url" yaml:"url"`
	Headers map[string]string `json:"-" yaml:"headers,omitempty"`
	// HasHeaders tells the UI a secret is stored for this endpoint without
	// disclosing it. It is derived on the way out, never read from the file.
	HasHeaders bool `json:"has_headers,omitempty" yaml:"-"`
}

type NetworkOverride struct {
	Enabled   *bool             `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Endpoints []Endpoint        `json:"endpoints,omitempty" yaml:"endpoints,omitempty"`
	Explorer  *network.Explorer `json:"explorer,omitempty" yaml:"explorer,omitempty"`
	Discovery *bool             `json:"discovery_enabled,omitempty" yaml:"discovery_enabled,omitempty"`
}

// URLs returns the configured endpoint URLs as written, without expanding any
// ${ENV} reference in them.
func (o NetworkOverride) URLs() []string {
	if len(o.Endpoints) == 0 {
		return nil
	}
	out := make([]string, 0, len(o.Endpoints))
	for _, endpoint := range o.Endpoints {
		out = append(out, endpoint.URL)
	}
	return out
}

// NetworksSchemaVersion is the current networks.yaml layout. Version 2 replaced
// the per-network `rpc_urls` list and its detached `headers` map with a list of
// endpoints that each carry their own credentials.
const NetworksSchemaVersion = 2

type networkOverridesFile struct {
	SchemaVersion int                           `yaml:"schema_version"`
	Networks      map[string]rawNetworkOverride `yaml:"networks"`
}

type rawNetworkOverride struct {
	Enabled   *bool             `yaml:"enabled,omitempty"`
	Endpoints []Endpoint        `yaml:"endpoints,omitempty"`
	Explorer  *network.Explorer `yaml:"explorer,omitempty"`
	Discovery *bool             `yaml:"discovery_enabled,omitempty"`

	// Schema 1 only, read so an existing file can be migrated in place.
	RPCURLs []string          `yaml:"rpc_urls,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
}

// migrate turns a stored override of either schema into the current shape.
//
// A version 1 file says nothing about which endpoint its headers were meant
// for, so the only safe reading is the narrowest one: the credentials belong to
// the first custom URL, the one the user typed alongside them. Later URLs, the
// official fallbacks and anything discovery suggests get none.
func (raw rawNetworkOverride) migrate(version int) NetworkOverride {
	override := NetworkOverride{
		Enabled: raw.Enabled, Explorer: raw.Explorer, Discovery: raw.Discovery,
		Endpoints: raw.Endpoints,
	}
	if version >= NetworksSchemaVersion {
		return override
	}
	override.Endpoints = make([]Endpoint, 0, len(raw.RPCURLs))
	for index, rpcURL := range raw.RPCURLs {
		endpoint := Endpoint{URL: rpcURL}
		if index == 0 && len(raw.Headers) > 0 {
			endpoint.Headers = raw.Headers
		}
		override.Endpoints = append(override.Endpoints, endpoint)
	}
	if len(override.Endpoints) == 0 {
		override.Endpoints = nil
	}
	return override
}

func (o NetworkOverride) raw() rawNetworkOverride {
	return rawNetworkOverride{
		Enabled: o.Enabled, Endpoints: o.Endpoints,
		Explorer: o.Explorer, Discovery: o.Discovery,
	}
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
		// Port 0 asks the kernel for a free port on every start. A fixed,
		// well-known port is one of the two things a DNS-rebinding page needs
		// in advance — it cannot read the response that would tell it the port,
		// so it has to know where to aim beforehand.
		Server: ServerSettings{Addr: "127.0.0.1:0", OpenBrowser: true},
		Security: SecuritySettings{
			AutoLock: 15 * time.Minute,
			// On by default. Everything else that hands over lasting control of
			// the funds — the recovery phrase, a private key, the backup — asks
			// for the password, and spending them is the same decision.
			ConfirmSends: true, SendGrantTTL: DefaultSendGrantTTL,
		},
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
	if previous, ok := next[id]; ok {
		override = CarryHeadersForward(previous, override)
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
		if file.SchemaVersion < 1 || file.SchemaVersion > NetworksSchemaVersion {
			return fmt.Errorf("unsupported networks.yaml schema version %d", file.SchemaVersion)
		}
		for id, raw := range file.Networks {
			override := raw.migrate(file.SchemaVersion)
			if err := validateNetworkOverride(id, override, config.NodeDiscovery.AllowInsecureRPC); err != nil {
				return fmt.Errorf("validate network override %s: %w", id, err)
			}
			overrides[id] = override
		}
		if file.SchemaVersion < NetworksSchemaVersion {
			// Rewritten now rather than migrated again on every start, so the
			// credentials on disk stop claiming a scope the code no longer gives
			// them. A home directory that cannot be written to still runs — the
			// migration above already happened in memory.
			if migrated, err := marshalNetworkOverrides(overrides); err == nil {
				if storage.AtomicWrite(filepath.Join(m.home, "networks.yaml"), migrated) == nil {
					networkData = migrated
				}
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
	if config.Security.AutoLock < MinAutoLock || config.Security.AutoLock > MaxAutoLock {
		return fmt.Errorf(
			"%w: auto-lock must be between %s and %s — a space that never locks itself "+
				"leaves spendable keys in memory for as long as the process runs",
			ErrInvalidSettings, MinAutoLock, MaxAutoLock,
		)
	}
	// Checked whether or not the step-up is on. A value that is only rejected
	// once someone switches it back on would fail at the least useful moment,
	// and the form shows the field either way.
	if config.Security.SendGrantTTL < MinSendGrantTTL || config.Security.SendGrantTTL > MaxSendGrantTTL {
		return fmt.Errorf(
			"%w: the send confirmation must last between %s and %s",
			ErrInvalidSettings, MinSendGrantTTL, MaxSendGrantTTL,
		)
	}
	if config.NodeDiscovery.RefreshInterval <= 0 || config.NodeDiscovery.RequestTimeout <= 0 {
		return fmt.Errorf("%w: node discovery durations must be positive", ErrInvalidSettings)
	}
	return validateDiscoveryURL(config.NodeDiscovery)
}

// validateDiscoveryURL states the rule for a node-discovery URL once.
//
// The rule has two consequences — refused when someone saves it, dropped when
// it is read off disk — and they have to be two readings of one predicate. Two
// copies would eventually disagree about the same string, and the disagreement
// would be a URL that saves cleanly and then disables discovery on the next
// start, or one that cannot be saved and cannot be cleared either.
func validateDiscoveryURL(settings DiscoverySettings) error {
	if settings.URL == "" {
		if settings.Enabled {
			return fmt.Errorf("%w: node discovery URL is required when enabled", ErrInvalidSettings)
		}
		return nil
	}
	discoveryURL, err := url.Parse(settings.URL)
	if err != nil || discoveryURL.Scheme != "https" || discoveryURL.Host == "" ||
		discoveryURL.User != nil {
		return fmt.Errorf("%w: node discovery URL must be a valid HTTPS URL", ErrInvalidSettings)
	}
	// The same policy the dialer applies to the connection this URL becomes,
	// out of the same package, so the form cannot accept an address the dialer
	// will then refuse — which is what happened while this side kept its own
	// shorter copy of the rule. It is only the cheap half: no validator does
	// DNS, so a name that resolves privately is still settled at connect time.
	//
	// Checked whether or not discovery is enabled, for the same reason the send
	// confirmation window is checked whether or not the step-up is on. A value
	// refused only once someone switches the feature on fails at the least
	// useful moment, and the form shows the field either way.
	if !settings.AllowInsecureRPC && !hostpolicy.PublicHost(discoveryURL.Hostname()) {
		return fmt.Errorf(
			"%w: node discovery URL must point at a public host — "+
				"enable insecure RPC first if this is deliberate",
			ErrInvalidSettings,
		)
	}
	return nil
}

// clampDiscovery drops a stored discovery URL that could not be saved through
// the API, and switches discovery off along with it.
//
// Clamped rather than rejected, for the reason auto-lock is: this URL is edited
// on the /settings page, which is served by the process that a refused
// config.yaml stops from ever opening its port. Rejecting the file locks the
// user out of the only UI that can repair it, and hand-editing YAML is not a
// recovery path.
//
// Dropping is what "clamp" has to mean for an address. A duration has a nearest
// legal value; a host does not, and any substitute would be a host the user
// never named. The two alternatives both fail: keeping the string and only
// switching discovery off leaves a value the rule still refuses on the next
// start, since it is checked whether or not discovery is on, and clearing the
// URL while leaving Enabled true fails on "URL is required when enabled". Doing
// both is what makes the result something the validator accepts, and clearing
// the URL is also what keeps the promise the rule was added for: nothing is
// left aimed at the host, so the refresh timer cannot go on quietly polling an
// address the wallet has decided it will not connect to.
//
// The file is not rewritten, so nothing is destroyed behind the user's back.
// The stored line is simply not honoured, and /settings shows the empty field
// it will accept a new value into.
// The timings are clamped for the same reason and by the same rule that
// rejects them: a stored zero — or a negative, which a hand-edited file can
// hold — is refused by ValidateHomeConfig, and refusing it on load is the
// lockout this function exists to prevent. There is a nearest legal value here,
// unlike for the address, so they take the shipped default rather than being
// dropped.
func clampDiscovery(settings DiscoverySettings) DiscoverySettings {
	defaults := DefaultHomeConfig().NodeDiscovery
	if settings.RefreshInterval <= 0 {
		settings.RefreshInterval = defaults.RefreshInterval
	}
	if settings.RequestTimeout <= 0 {
		settings.RequestTimeout = defaults.RequestTimeout
	}
	if validateDiscoveryURL(settings) == nil {
		return settings
	}
	settings.Enabled = false
	settings.URL = ""
	return settings
}

func marshalConfig(config HomeConfig) ([]byte, error) {
	raw := rawHomeConfig{
		SchemaVersion: config.SchemaVersion, Server: config.Server, UI: config.UI,
		Security: rawSecuritySettings{
			AutoLock:     config.Security.AutoLock.String(),
			ConfirmSends: &config.Security.ConfirmSends,
			SendGrantTTL: config.Security.SendGrantTTL.String(),
		},
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
	// Clamped rather than rejected. A file written before auto-lock had bounds
	// may legitimately say 0, and refusing to start over it would lock the user
	// out of the UI that lets them fix it — while leaving 0 in place would keep
	// a space unlocked for as long as the process runs.
	autoLock = ClampAutoLock(autoLock)
	refresh, err := time.ParseDuration(raw.NodeDiscovery.RefreshInterval)
	if err != nil {
		return HomeConfig{}, fmt.Errorf("parse refresh_interval: %w", err)
	}
	timeout, err := time.ParseDuration(raw.NodeDiscovery.RequestTimeout)
	if err != nil {
		return HomeConfig{}, fmt.Errorf("parse request_timeout: %w", err)
	}
	// Absent means "written before the setting existed", which takes the
	// default rather than reading as an explicit off.
	confirmSends := true
	if raw.Security.ConfirmSends != nil {
		confirmSends = *raw.Security.ConfirmSends
	}
	grantTTL := DefaultSendGrantTTL
	if raw.Security.SendGrantTTL != "" {
		parsed, parseErr := time.ParseDuration(raw.Security.SendGrantTTL)
		if parseErr != nil {
			return HomeConfig{}, fmt.Errorf("parse send_grant_ttl: %w", parseErr)
		}
		grantTTL = parsed
	}
	return HomeConfig{
		SchemaVersion: raw.SchemaVersion, Server: raw.Server, UI: raw.UI,
		Security: SecuritySettings{
			AutoLock: autoLock, ConfirmSends: confirmSends,
			// Clamped for the same reason auto-lock is: a file that names an
			// hour and a half must not keep the wallet from starting, and must
			// not be honoured either.
			SendGrantTTL: ClampSendGrantTTL(grantTTL),
		},
		// Clamped on the way in, like auto-lock and the send window above, so
		// that a URL already on disk can never be the reason the wallet refuses
		// to start. A URL arriving through the API still goes to
		// ValidateHomeConfig and is still refused there; only what is already
		// stored is repaired, because only a stored value has nobody left to
		// tell.
		NodeDiscovery: clampDiscovery(DiscoverySettings{
			Enabled: raw.NodeDiscovery.Enabled, URL: raw.NodeDiscovery.URL,
			RefreshInterval: refresh, RequestTimeout: timeout,
			AllowInsecureRPC: raw.NodeDiscovery.AllowInsecureRPC,
		}),
	}, nil
}

func validateRPCURL(value string, allowInsecure bool) error {
	resolved, err := ExpandValue(value)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSettings, err)
	}
	// A comma or a pipe is legal inside a URL path but is a separator to the
	// Tron node list, so a value carrying one is checked here as a single
	// HTTPS URL and could be taken downstream as several endpoints — the extras
	// never having passed this function at all. One entry means one endpoint.
	if strings.ContainsAny(resolved, ",|") {
		return fmt.Errorf("%w: an RPC URL must not contain a comma or a pipe: %s", ErrInvalidSettings, value)
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
	seen := make(map[string]struct{}, len(override.Endpoints))
	for _, endpoint := range override.Endpoints {
		if err := validateRPCURL(endpoint.URL, allowInsecure); err != nil {
			return err
		}
		// Two entries for the same endpoint would make "which credentials does
		// this URL get?" ambiguous, and the answer decides where a secret goes.
		key, err := EndpointKey(endpoint.URL)
		if err != nil {
			return fmt.Errorf("%w: invalid RPC URL: %s", ErrInvalidSettings, endpoint.URL)
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("%w: RPC endpoint %s is listed twice", ErrInvalidSettings, endpoint.URL)
		}
		seen[key] = struct{}{}
		for name, value := range endpoint.Headers {
			if strings.TrimSpace(name) == "" || strings.ContainsAny(name, " \t\r\n:") ||
				value == "" || strings.ContainsAny(value, "\r\n") {
				return fmt.Errorf(
					"%w: provider headers contain an invalid name or empty value", ErrInvalidSettings,
				)
			}
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
	return nil
}

// EndpointKey reduces a URL to the identity a credential is bound to: scheme,
// host, port and path. Two spellings of the same endpoint have to agree, or a
// stored secret would silently stop being attached — but nothing beyond the
// path may be folded away, because that is where a provider puts the key.
func EndpointKey(value string) (string, error) {
	resolved, err := ExpandValue(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(resolved)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid RPC URL: %s", value)
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	path := strings.TrimSuffix(parsed.EscapedPath(), "/")

	return scheme + "://" + net.JoinHostPort(host, port) + path + "?" + parsed.RawQuery, nil
}

// CarryHeadersForward keeps a stored secret the browser was never shown.
//
// The API redacts credentials on the way out, so a UI that saves a network
// round-trips endpoints without them. A nil header map therefore means "leave
// whatever is stored for this URL alone"; an empty one means "delete it".
// Carrying forward is keyed on the endpoint, so moving a URL out of the list
// takes its credential with it.
func CarryHeadersForward(previous, next NetworkOverride) NetworkOverride {
	stored := make(map[string]map[string]string, len(previous.Endpoints))
	for _, endpoint := range previous.Endpoints {
		if len(endpoint.Headers) == 0 {
			continue
		}
		key, err := EndpointKey(endpoint.URL)
		if err != nil {
			continue
		}
		stored[key] = endpoint.Headers
	}
	endpoints := make([]Endpoint, 0, len(next.Endpoints))
	for _, endpoint := range next.Endpoints {
		if endpoint.Headers == nil {
			if key, err := EndpointKey(endpoint.URL); err == nil {
				endpoint.Headers = stored[key]
			}
		}
		endpoints = append(endpoints, endpoint)
	}
	if len(endpoints) == 0 {
		endpoints = nil
	}
	next.Endpoints = endpoints
	return next
}

func copyOverrides(source map[string]NetworkOverride) map[string]NetworkOverride {
	out := make(map[string]NetworkOverride, len(source))
	for id, override := range source {
		endpoints := make([]Endpoint, 0, len(override.Endpoints))
		for _, endpoint := range override.Endpoints {
			if endpoint.Headers != nil {
				headers := make(map[string]string, len(endpoint.Headers))
				maps.Copy(headers, endpoint.Headers)
				endpoint.Headers = headers
			}
			endpoints = append(endpoints, endpoint)
		}
		if len(endpoints) == 0 {
			endpoints = nil
		}
		override.Endpoints = endpoints
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
		out[id] = override
	}
	return out
}

func redactedOverrides(source map[string]NetworkOverride) map[string]NetworkOverride {
	out := copyOverrides(source)
	for id, override := range out {
		for index, endpoint := range override.Endpoints {
			override.Endpoints[index].HasHeaders = len(endpoint.Headers) > 0
			override.Endpoints[index].Headers = nil
		}
		out[id] = override
	}
	return out
}

func marshalNetworkOverrides(overrides map[string]NetworkOverride) ([]byte, error) {
	raw := make(map[string]rawNetworkOverride, len(overrides))
	for id, override := range overrides {
		raw[id] = override.raw()
	}
	data, err := yaml.Marshal(networkOverridesFile{
		SchemaVersion: NetworksSchemaVersion, Networks: raw,
	})
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

// RedactURL reduces an endpoint to the part that is safe to show or log.
//
// An RPC URL is written in configuration as an ${ENV} reference, but by the
// time it reaches an error message or a cache file it has been expanded — and
// providers routinely put the API key in the path (…/v3/<key>), the query
// (?apikey=…) or the userinfo. Only the scheme and host survive here; anything
// that followed is replaced by a marker, which still names the provider without
// carrying the credential along with it.
func RedactURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" {
		return "[redacted endpoint]"
	}
	redacted := parsed.Scheme + "://" + parsed.Host
	if trimmed := strings.Trim(parsed.Path, "/"); trimmed != "" {
		redacted += "/…"
	}
	if parsed.RawQuery != "" || parsed.User != nil {
		redacted += "?…"
	}

	return redacted
}

// RedactError rewrites every endpoint mentioned in an error so the message can
// be returned to the browser and written to the log.
//
// The cause is kept reachable. Returning a bare errors.New here would cut the
// chain, and callers that wrap the result — verifyRPCs does — would hand
// writePlatformError something whose sentinels no longer match, so a probe that
// timed out would be reported with the wrong status. Only Error() is redacted;
// errors.Is walks Unwrap without printing anything.
func RedactError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	redacted := endpointPattern.ReplaceAllStringFunc(message, RedactURL)
	if redacted == message {
		return err
	}

	return redactedError{message: redacted, cause: err}
}

type redactedError struct {
	message string
	cause   error
}

func (e redactedError) Error() string { return e.message }
func (e redactedError) Unwrap() error { return e.cause }

// endpointPattern matches a URL inside a longer message. It deliberately stops
// at whitespace and at the quote characters error strings tend to use.
var endpointPattern = regexp.MustCompile(`https?://[^\s"'` + "`" + `]+`)

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
