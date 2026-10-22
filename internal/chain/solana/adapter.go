// Package solana implements the chain adapter for Solana clusters.
//
// Reads go through github.com/gagliardetto/solana-go over the resolver's
// guarded HTTP client. Sending arrives with a later stage; until then the
// transfer methods refuse with a clear error.
package solana

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/rpcpool"
	"golang.org/x/sync/singleflight"
)

// Networks looks up a network definition. *network.Registry implements it.
type Networks interface {
	Get(id string) (network.Network, error)
}

// EndpointProvider lists a network's endpoints in the order to try them and
// hands out the guarded HTTP client every request has to use. Unlike the other
// families the client is not optional: there is no fallback to an unguarded
// one.
type EndpointProvider interface {
	Endpoints(ctx context.Context, item network.Network) ([]string, error)
	HTTPClient(item network.Network) *http.Client
}

// HeaderProvider resolves the credentials belonging to one endpoint.
type HeaderProvider interface {
	Headers(item network.Network, endpoint string) (http.Header, error)
}

// EndpointReporter learns which endpoints passed verification.
type EndpointReporter interface {
	MarkHealthy(item network.Network, endpoint string)
}

// EndpointInvalidator forgets what the provider learned about a network.
type EndpointInvalidator interface {
	Invalidate(networkID string)
}

// HTTPClientProvider is the part of EndpointProvider that main asserts at
// compile time for the resolver, as it does for the other families.
type HTTPClientProvider interface {
	HTTPClient(item network.Network) *http.Client
}

// verifyTimeout bounds one endpoint's identity check while a network's client
// is being built.
const verifyTimeout = 15 * time.Second

var _ chain.Adapter = (*Adapter)(nil)

type Adapter struct {
	networks  Networks
	endpoints EndpointProvider

	mu      sync.Mutex
	clients map[string]*nodes
	version map[string]uint64
	init    singleflight.Group

	balances *balanceCache
	expiry   *expiryBook
	rents    *rentCache
	// build serializes a plan's message; a test replaces it to prove that a
	// message other than the intent is never signed.
	buildMessage func(*plan, solana.Hash) ([]byte, error)
	buildClose   func(*closePlan, solana.Hash) ([]byte, error)
	// swaps holds the Raydium quoter and the swap message builder.
	swaps *swapDeps
	// Test hooks: beforeCachePut runs between a stream's generation check
	// and its cache write; flightEnded runs when a shared token read ends,
	// before it is removed from the group.
	beforeCachePut func(lamports uint64)
	flightEnded    func()
}

// New builds the adapter. The endpoint provider is required: it is where the
// guarded HTTP client comes from.
func New(networks Networks, endpoints EndpointProvider) (*Adapter, error) {
	if networks == nil {
		return nil, errors.New("network registry is required")
	}
	if endpoints == nil {
		return nil, errors.New("endpoint provider is required")
	}
	return &Adapter{
		networks: networks, endpoints: endpoints,
		clients: make(map[string]*nodes), version: make(map[string]uint64),
		balances: newBalanceCache(), expiry: newExpiryBook(),
		rents:        &rentCache{items: make(map[string]rentEntry)},
		buildMessage: (*plan).build,
		buildClose:   (*closePlan).build,
		swaps:        &swapDeps{buildFinal: (*swapPlan).buildFinal},
	}, nil
}

// Invalidate drops the network's verified endpoints and cached balances, so
// the next call is built from the current settings. Recorded blockhash expiry
// heights are facts about signed transactions, not settings, and are kept.
func (a *Adapter) Invalidate(networkID string) {
	a.mu.Lock()
	delete(a.clients, networkID)
	a.version[networkID]++
	a.mu.Unlock()
	a.balances.dropNetwork(networkID)
	if invalidator, ok := a.endpoints.(EndpointInvalidator); ok {
		invalidator.Invalidate(networkID)
	}
}

// TokenKind is the asset kind SPL tokens are stored under.
func (a *Adapter) TokenKind() string { return "spl" }

// client returns the network's verified endpoints, building them on first use.
// Concurrent first calls share one build, so a page that opens many streams at
// once costs the public RPC one identity check per endpoint, not one per
// stream.
func (a *Adapter) client(ctx context.Context, networkID string) (network.Network, *nodes, error) {
	item, err := a.networks.Get(networkID)
	if err != nil {
		return network.Network{}, nil, err
	}
	if item.Family != Family {
		return network.Network{}, nil, fmt.Errorf("%w: %s is not Solana", chain.ErrInvalidRequest, networkID)
	}
	want, err := expectedGenesis(item)
	if err != nil {
		return network.Network{}, nil, fmt.Errorf("%w: %w", chain.ErrInvalidRequest, err)
	}
	for {
		a.mu.Lock()
		cached := a.clients[networkID]
		version := a.version[networkID]
		a.mu.Unlock()
		if cached != nil {
			return item, cached, nil
		}
		// Keyed by version as well: a build that started under the previous
		// settings must not be joined by a caller that already sees the new ones.
		key := fmt.Sprintf("%s#%d", networkID, version)
		build := a.init.DoChan(key, func() (any, error) {
			return a.install(ctx, item, want, version)
		})
		// The build runs on for whoever is still waiting and is stored for
		// later callers; a caller that leaves stops waiting at once instead
		// of sitting through every endpoint check.
		var value any
		select {
		case result := <-build:
			value, err = result.Val, result.Err
		case <-ctx.Done():
			return network.Network{}, nil, ctx.Err()
		}
		if errors.Is(err, errSettingsChanged) {
			// Start over under the new settings; a caller that has left
			// meanwhile is let go by the select above.
			continue
		}
		if err != nil {
			return network.Network{}, nil, err
		}
		return item, value.(*nodes), nil
	}
}

// keyed reports whether an endpoint carries a provider credential, in a
// header or in its URL. Only an endpoint with neither is held to the public
// RPC limits. The URL rule is the resolver's: user info, a query or a path is
// where providers put keys (https://mainnet.helius-rpc.com/?api-key=…).
func keyed(endpoint string, headers http.Header) bool {
	return len(requestHeaders(headers)) > 0 || !rpcpool.CredentialFreeURL(endpoint)
}

// errSettingsChanged reports a build whose settings were replaced while it ran.
var errSettingsChanged = errors.New("network settings changed during the endpoint check")

// install builds the network's endpoints and stores them before the shared
// build ends, so a caller that arrives just after it finds them cached rather
// than starting a second build.
func (a *Adapter) install(ctx context.Context, item network.Network, want string, version uint64) (*nodes, error) {
	a.mu.Lock()
	if existing := a.clients[item.ID]; existing != nil && a.version[item.ID] == version {
		a.mu.Unlock()
		return existing, nil
	}
	a.mu.Unlock()
	// Shared by every caller waiting on it, so one caller hanging up must not
	// fail the others; each endpoint check has its own bound.
	built, err := a.build(context.WithoutCancel(ctx), item, want)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	if a.version[item.ID] != version {
		// The settings changed while the endpoints were being checked; what
		// was verified may be exactly what the user just removed.
		a.mu.Unlock()
		return nil, errSettingsChanged
	}
	a.clients[item.ID] = built
	a.mu.Unlock()
	if reporter, ok := a.endpoints.(EndpointReporter); ok {
		for _, verified := range built.list {
			reporter.MarkHealthy(item, verified.endpoint)
		}
	}
	return built, nil
}

// build verifies every endpoint the provider lists, in its order, and keeps
// the ones that serve the network's cluster. Each endpoint gets only its own
// credentials.
func (a *Adapter) build(ctx context.Context, item network.Network, want string) (*nodes, error) {
	endpoints, err := a.endpoints.Endpoints(ctx, item)
	if err != nil {
		return nil, err
	}
	httpClient := a.endpoints.HTTPClient(item)
	var (
		verified []*node
		failures []error
	)
	for _, endpoint := range endpoints {
		var headers http.Header
		if provider, ok := a.endpoints.(HeaderProvider); ok {
			headers, err = provider.Headers(item, endpoint)
			if err != nil {
				return nil, err
			}
		}
		client, err := newRPC(endpoint, headers, httpClient)
		if err != nil {
			return nil, err
		}
		checkCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
		err = verifyGenesis(checkCtx, client, want)
		cancel()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		verified = append(verified, &node{
			endpoint: endpoint, client: client, keyed: keyed(endpoint, headers), pacer: &pacer{},
		})
	}
	if len(verified) == 0 {
		if len(failures) == 0 {
			failures = append(failures, errors.New("no endpoints are configured"))
		}
		return nil, fmt.Errorf("no verified Solana RPC for %s: %w", item.ID, errors.Join(failures...))
	}
	return newNodes(verified), nil
}
