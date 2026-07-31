// Package rpcpool resolves and filters RPC endpoints without trusting discovery metadata.
package rpcpool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/storage"
)

const (
	maxDiscoveryResponse = 2 << 20
	// Version 2 drops cached endpoints created before the Tron Mainnet default
	// moved from TronGrid to PublicNode. The cache is advisory and can be safely
	// rebuilt; user-defined network overrides live in a separate file.
	cacheSchemaVersion = 2
)

type Settings interface {
	Snapshot() config.SettingsSnapshot
	NetworkOverride(id string) (config.NetworkOverride, bool)
	Home() string
}

type Resolver struct {
	settings Settings
	client   *http.Client
	lookupIP func(context.Context, string) ([]net.IP, error)

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	Endpoints []string  `json:"endpoints"`
	Expires   time.Time `json:"expires"`
}

type cacheFile struct {
	SchemaVersion int                   `json:"schema_version"`
	Networks      map[string]cacheEntry `json:"networks"`
}

func New(settings Settings) *Resolver {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &Resolver{
		settings: settings,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           dialer.DialContext,
				TLSHandshakeTimeout:   5 * time.Second,
				ResponseHeaderTimeout: 5 * time.Second,
			},
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("redirects are disabled for RPC discovery")
			},
		},
		lookupIP: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
		cache: loadCache(settings.Home()),
	}
}

func (r *Resolver) Endpoints(ctx context.Context, item network.Network) ([]string, error) {
	snapshot := r.settings.Snapshot()
	if override, ok := r.settings.NetworkOverride(item.ID); ok && len(override.RPCURLs) > 0 {
		resolved := make([]string, 0, len(override.RPCURLs))
		for _, endpoint := range override.RPCURLs {
			value, err := config.ExpandValue(endpoint)
			if err != nil {
				return nil, err
			}
			resolved = append(resolved, value)
		}
		return unique(append(resolved, item.RPCFallbacks...)), nil
	}

	r.mu.Lock()
	cached := r.cache[item.ID]
	r.mu.Unlock()
	if len(cached.Endpoints) > 0 && time.Now().Before(cached.Expires) {
		safe := make([]string, 0, len(cached.Endpoints))
		for _, endpoint := range cached.Endpoints {
			if err := r.safeDynamicEndpoint(ctx, endpoint, snapshot.Config.NodeDiscovery.AllowInsecureRPC); err == nil {
				safe = append(safe, endpoint)
			}
		}
		if len(safe) > 0 {
			return unique(append(safe, item.RPCFallbacks...)), nil
		}
	}

	if !snapshot.Config.NodeDiscovery.Enabled {
		return append([]string(nil), item.RPCFallbacks...), nil
	}
	if override, ok := r.settings.NetworkOverride(item.ID); ok && override.Discovery != nil && !*override.Discovery {
		return append([]string(nil), item.RPCFallbacks...), nil
	}

	discovered, err := r.discover(ctx, snapshot.Config.NodeDiscovery, item)
	if err != nil {
		// Discovery is advisory. Official fallbacks keep the wallet available.
		return append([]string(nil), item.RPCFallbacks...), nil
	}
	return unique(append(discovered, item.RPCFallbacks...)), nil
}

func (r *Resolver) MarkHealthy(item network.Network, endpoint string) {
	if r.settings.Home() == "" {
		return
	}
	snapshot := r.settings.Snapshot()
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.cache[item.ID]
	current.Endpoints = unique(append([]string{endpoint}, current.Endpoints...))
	if len(current.Endpoints) > 8 {
		current.Endpoints = current.Endpoints[:8]
	}
	current.Expires = time.Now().Add(snapshot.Config.NodeDiscovery.RefreshInterval)
	r.cache[item.ID] = current
	data, err := json.MarshalIndent(cacheFile{SchemaVersion: cacheSchemaVersion, Networks: r.cache}, "", "  ")
	if err == nil {
		_ = storage.AtomicWrite(
			path.Join(r.settings.Home(), "cache", "rpc-nodes.json"), append(data, '\n'),
		)
	}
}

// Invalidate removes endpoints learned from previous clients. In particular,
// deleting a custom override must not leave that provider active through the
// health cache.
func (r *Resolver) Invalidate(networkID string) {
	if r.settings.Home() == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cache, networkID)
	data, err := json.MarshalIndent(cacheFile{SchemaVersion: cacheSchemaVersion, Networks: r.cache}, "", "  ")
	if err == nil {
		_ = storage.AtomicWrite(
			path.Join(r.settings.Home(), "cache", "rpc-nodes.json"), append(data, '\n'),
		)
	}
}

func (r *Resolver) Headers(item network.Network) (http.Header, error) {
	override, ok := r.settings.NetworkOverride(item.ID)
	if !ok || len(override.Headers) == 0 {
		return nil, nil
	}
	headers := make(http.Header, len(override.Headers))
	for name, value := range override.Headers {
		resolved, err := config.ExpandValue(value)
		if err != nil {
			return nil, err
		}
		headers.Set(name, resolved)
	}
	return headers, nil
}

func (r *Resolver) HTTPClient(_ network.Network) *http.Client {
	allowPrivate := r.settings.Snapshot().Config.NodeDiscovery.AllowInsecureRPC
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		DialContext: func(ctx context.Context, networkName, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			ips, err := r.lookupIP(ctx, host)
			if err != nil || len(ips) == 0 {
				return nil, errors.New("RPC host cannot be resolved")
			}
			for _, ip := range ips {
				if !allowPrivate && !publicIP(ip) {
					continue
				}
				return dialer.DialContext(ctx, networkName, net.JoinHostPort(ip.String(), port))
			}
			return nil, errors.New("RPC resolved only to private or unsafe addresses")
		},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("RPC redirects are disabled")
		},
	}
}

func (r *Resolver) discover(ctx context.Context, settings config.DiscoverySettings, item network.Network) ([]string, error) {
	base, err := url.Parse(settings.URL)
	if err != nil {
		return nil, fmt.Errorf("parse discovery URL: %w", err)
	}
	if item.Family == network.FamilyEVM {
		base.Path = path.Join(base.Path, "/api/v1/nodes", item.ChainID)
	} else {
		base.Path = path.Join(base.Path, "/api/v1/tron/nodes", item.ChainID)
	}
	requestCtx, cancel := context.WithTimeout(ctx, settings.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("query node discovery: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("node discovery returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDiscoveryResponse+1))
	if err != nil {
		return nil, fmt.Errorf("read node discovery: %w", err)
	}
	if len(data) > maxDiscoveryResponse {
		return nil, errors.New("node discovery response is too large")
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("decode node discovery: %w", err)
	}
	raw := extractURLs(value)
	endpoints := make([]string, 0, len(raw))
	for _, endpoint := range raw {
		if err := r.safeDynamicEndpoint(requestCtx, endpoint, settings.AllowInsecureRPC); err == nil {
			endpoints = append(endpoints, endpoint)
		}
	}
	if len(endpoints) == 0 {
		return nil, errors.New("node discovery returned no safe endpoints")
	}
	sort.Strings(endpoints)
	return unique(endpoints), nil
}

func (r *Resolver) safeDynamicEndpoint(ctx context.Context, endpoint string, allowInsecure bool) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("invalid RPC URL")
	}
	if parsed.Scheme != "https" && !(allowInsecure && parsed.Scheme == "http") {
		return errors.New("RPC transport is not allowed")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return errors.New("local RPC host is not allowed")
	}
	ips, err := r.lookupIP(ctx, host)
	if err != nil || len(ips) == 0 {
		return errors.New("RPC host cannot be resolved")
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return errors.New("private RPC address is not allowed")
		}
	}
	return nil
}

func publicIP(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() ||
		ip.IsUnspecified() {
		return false
	}
	for _, network := range unsafeSpecialNetworks {
		if network.Contains(ip) {
			return false
		}
	}
	return true
}

var unsafeSpecialNetworks = mustCIDRs(
	"100.64.0.0/10",   // shared address space, often routes into carrier infrastructure
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // documentation
	"198.18.0.0/15",   // benchmarking
	"198.51.100.0/24", // documentation
	"203.0.113.0/24",  // documentation
	"2001:db8::/32",   // documentation
)

func mustCIDRs(values ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			panic(err)
		}
		out = append(out, network)
	}
	return out
}

func extractURLs(value any) []string {
	var out []string
	var walk func(any, string)
	walk = func(current any, key string) {
		switch typed := current.(type) {
		case map[string]any:
			for childKey, child := range typed {
				walk(child, strings.ToLower(childKey))
			}
		case []any:
			for _, child := range typed {
				walk(child, key)
			}
		case string:
			if key == "" || strings.Contains(key, "url") || strings.Contains(key, "endpoint") ||
				strings.Contains(key, "rpc") || strings.Contains(key, "http") {
				if strings.HasPrefix(typed, "https://") || strings.HasPrefix(typed, "http://") {
					out = append(out, typed)
				}
			}
		}
	}
	walk(value, "")
	return out
}

func unique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func loadCache(home string) map[string]cacheEntry {
	out := make(map[string]cacheEntry)
	if home == "" {
		return out
	}
	data, err := os.ReadFile(path.Join(home, "cache", "rpc-nodes.json"))
	if err != nil {
		return out
	}
	var file cacheFile
	if json.Unmarshal(data, &file) != nil || file.SchemaVersion != cacheSchemaVersion {
		return out
	}
	if file.Networks == nil {
		return out
	}
	return file.Networks
}
