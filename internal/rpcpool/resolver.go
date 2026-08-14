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
	"github.com/sxwebdev/walletspace/internal/hostpolicy"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/storage"
)

const (
	maxDiscoveryResponse = 2 << 20
	// Version 2 drops cached endpoints created before the Tron Mainnet default
	// moved from TronGrid to PublicNode. The cache is advisory and can be safely
	// rebuilt; user-defined network overrides live in a separate file.
	cacheSchemaVersion = 2

	// The response size cap alone bounds nothing that matters: two megabytes of
	// short URLs is on the order of a hundred thousand of them, and each one
	// costs a DNS lookup before it can be judged. These bound the work itself.
	//
	// maxDiscoveryEndpoints is what a network can usefully rotate between;
	// anything past it is noise or an attempt to make the wallet do work.
	maxDiscoveryEndpoints = 16
	// maxDiscoveryURLLength is generous for a real endpoint and far short of
	// something worth storing or resolving.
	maxDiscoveryURLLength = 512
	// maxDiscoveryNodes bounds the JSON walk itself, which recurses through
	// whatever shape the document happens to have.
	maxDiscoveryNodes = 10_000
	// maxDiscoveryDepth stops a deeply nested document from growing the stack.
	maxDiscoveryDepth = 32
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
	resolver := &Resolver{
		settings: settings,
		lookupIP: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		},
		cache: loadCache(settings.Home()),
	}
	resolver.client = &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			// No proxy, and the same dialer every RPC connection goes through.
			// Discovery is the one request this process makes on a timer to an
			// address the user typed once and then forgot about, and it was the
			// only one exempt from the address check — a proxy would have been
			// dialled in place of the host, which is the same exemption by
			// another route.
			Proxy: nil,
			// Built per dial, not captured once: whether a private address is
			// allowed is a setting, and a setting can change while this client
			// lives for the whole process.
			DialContext: func(ctx context.Context, networkName, address string) (net.Conn, error) {
				return resolver.DialContext(network.Network{})(ctx, networkName, address)
			},
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("redirects are disabled for RPC discovery")
		},
	}
	return resolver
}

func (r *Resolver) Endpoints(ctx context.Context, item network.Network) ([]string, error) {
	snapshot := r.settings.Snapshot()
	if override, ok := r.settings.NetworkOverride(item.ID); ok && len(override.Endpoints) > 0 {
		resolved := make([]string, 0, len(override.Endpoints))
		for _, endpoint := range override.Endpoints {
			value, err := config.ExpandValue(endpoint.URL)
			if err != nil {
				return nil, err
			}
			resolved = append(resolved, value)
		}
		return unique(append(resolved, item.RPCFallbacks...)), nil
	}
	if !snapshot.Config.NodeDiscovery.Enabled {
		return append([]string(nil), item.RPCFallbacks...), nil
	}
	if override, ok := r.settings.NetworkOverride(item.ID); ok && override.Discovery != nil && !*override.Discovery {
		return append([]string(nil), item.RPCFallbacks...), nil
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
	if !cacheableEndpoint(endpoint) {
		return
	}
	// A custom endpoint is the one place a provider credential can appear, and
	// by here the ${ENV} reference in the configuration has already been
	// expanded. Caching it would write the resolved secret to
	// cache/rpc-nodes.json, in plain text, from a configuration file that
	// deliberately held only a reference to it. There is nothing to gain
	// either: a custom URL is read from configuration on every start.
	if override, ok := r.settings.NetworkOverride(item.ID); ok && len(override.Endpoints) > 0 {
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

// cacheableEndpoint reports whether an endpoint can be written to disk without
// writing a credential with it.
//
// The provenance check in MarkHealthy is the main guard; this is the backstop
// for anything that reaches the cache by another route. Providers put keys in
// all three of these places, and none of them belong in a cache file that
// exists only to remember which host answered.
func cacheableEndpoint(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return false
	}

	return parsed.User == nil && parsed.RawQuery == "" && strings.Trim(parsed.Path, "/") == ""
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

// Headers returns the credentials configured for one endpoint, and only for
// that endpoint.
//
// Endpoints() hands back the user's custom URLs followed by the official
// fallbacks, and discovery can add more. A provider key belongs to the URL it
// was typed next to; sending it to whichever of those answered first would hand
// one provider's credential to another, or to a node discovery suggested.
// Anything that is not a configured endpoint gets nil.
func (r *Resolver) Headers(item network.Network, endpoint string) (http.Header, error) {
	override, ok := r.settings.NetworkOverride(item.ID)
	if !ok {
		return nil, nil
	}
	want, err := config.EndpointKey(endpoint)
	if err != nil {
		return nil, nil
	}
	for _, configured := range override.Endpoints {
		if len(configured.Headers) == 0 {
			continue
		}
		// An endpoint that cannot be resolved is reported rather than skipped.
		// Skipping it would send the request to a provider unauthenticated and
		// surface as a puzzling 401 from the node, instead of naming the
		// environment variable that is not set.
		key, err := config.EndpointKey(configured.URL)
		if err != nil {
			return nil, err
		}
		if key != want {
			continue
		}
		headers := make(http.Header, len(configured.Headers))
		for name, value := range configured.Headers {
			resolved, err := config.ExpandValue(value)
			if err != nil {
				return nil, err
			}
			headers.Set(name, resolved)
		}
		return headers, nil
	}
	return nil, nil
}

func (r *Resolver) HTTPClient(item network.Network) *http.Client {
	transport := &http.Transport{
		Proxy:                 nil,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		DialContext:           r.DialContext(item),
	}
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("RPC redirects are disabled")
		},
	}
}

// DialContext returns the dialer every RPC connection has to go through.
//
// It resolves the host itself and connects to a literal address, so the answer
// that was judged is the answer that gets dialled — a name that verified as
// public and then resolves to 127.0.0.1 or 169.254.169.254 on the next lookup
// does not get a connection. gRPC needs the dialer on its own rather than
// wrapped in an http.Client, which is why this is separate from HTTPClient.
func (r *Resolver) DialContext(_ network.Network) func(context.Context, string, string) (net.Conn, error) {
	allowPrivate := r.settings.Snapshot().Config.NodeDiscovery.AllowInsecureRPC
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return func(ctx context.Context, networkName, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if ip := net.ParseIP(host); ip != nil {
			if !allowPrivate && !publicIP(ip) {
				return nil, errors.New("RPC address is private or unsafe")
			}
			return dialer.DialContext(ctx, networkName, address)
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
	}
}

// GRPCDialContext adapts DialContext to grpc.WithContextDialer, which passes
// only the address.
func (r *Resolver) GRPCDialContext(item network.Network) func(context.Context, string) (net.Conn, error) {
	dial := r.DialContext(item)
	return func(ctx context.Context, address string) (net.Conn, error) {
		return dial(ctx, "tcp", address)
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
	// Both characters separate entries in the Tron node list, so a discovered
	// URL carrying one would be checked here as a single host and then taken
	// downstream as several — the extras skipping this function entirely.
	if strings.ContainsAny(endpoint, ",|") {
		return errors.New("RPC URL must not contain a comma or a pipe")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("invalid RPC URL")
	}
	if parsed.Scheme != "https" && !(allowInsecure && parsed.Scheme == "http") {
		return errors.New("RPC transport is not allowed")
	}
	host := strings.ToLower(parsed.Hostname())
	if hostpolicy.LocalName(host) {
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

// publicIP is the shared address policy under the name the dialer knows it by.
//
// The rule itself lives in hostpolicy because the settings validator enforces
// the same one when a URL is saved, and while it had a copy the copy was the
// weaker of the two — it accepted the IPv6 spellings this side has always
// refused, so a value the dialer would never connect to still saved cleanly.
func publicIP(ip net.IP) bool { return hostpolicy.PublicIP(ip) }

// extractURLs pulls candidate endpoints out of whatever shape the discovery
// service answered with.
//
// The walk is bounded three ways — how many URLs it will keep, how long each
// may be, and how many JSON nodes and levels it will visit — because every
// survivor costs a DNS lookup in safeDynamicEndpoint before it can be judged.
// A discovery service is explicitly untrusted, so the cost of its answer has to
// be capped before any of it is acted on, not after.
func extractURLs(value any) []string {
	var out []string
	visited := 0
	var walk func(any, string, int)
	walk = func(current any, key string, depth int) {
		if len(out) >= maxDiscoveryEndpoints || visited >= maxDiscoveryNodes || depth > maxDiscoveryDepth {
			return
		}
		visited++
		switch typed := current.(type) {
		case map[string]any:
			for childKey, child := range typed {
				walk(child, strings.ToLower(childKey), depth+1)
			}
		case []any:
			for _, child := range typed {
				walk(child, key, depth+1)
			}
		case string:
			if len(typed) > maxDiscoveryURLLength {
				return
			}
			if key == "" || strings.Contains(key, "url") || strings.Contains(key, "endpoint") ||
				strings.Contains(key, "rpc") || strings.Contains(key, "http") {
				if strings.HasPrefix(typed, "https://") || strings.HasPrefix(typed, "http://") {
					out = append(out, typed)
				}
			}
		}
	}
	walk(value, "", 0)
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
