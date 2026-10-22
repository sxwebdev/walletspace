package solana_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gagliardetto/solana-go/rpc/jsonrpc"
	solanachain "github.com/sxwebdev/walletspace/internal/chain/solana"
	"github.com/sxwebdev/walletspace/internal/network"
)

func TestVerifyEndpointChecksTheGenesisHash(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		item    network.Network
		genesis string
		wantErr string
	}{
		{name: "mainnet", item: mainnet(), genesis: mainnetGenesis},
		{name: "devnet", item: devnet(), genesis: devnetGenesis},
		{name: "devnet node for mainnet", item: mainnet(), genesis: devnetGenesis, wantErr: "expected " + mainnetGenesis},
		{name: "mainnet node for devnet", item: devnet(), genesis: mainnetGenesis, wantErr: "expected " + devnetGenesis},
		{name: "malformed hash", item: mainnet(), genesis: "not-base58-0OIl", wantErr: "genesis"},
		{name: "unknown cluster", item: networks()["solana-odd"], genesis: mainnetGenesis, wantErr: "unknown Solana cluster"},
		{name: "another family", item: networks()["tron-mainnet"], genesis: mainnetGenesis, wantErr: "not Solana"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t, tt.genesis)
			err := solanachain.VerifyEndpoint(t.Context(), tt.item, n.server.URL, nil, n.server.Client())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("VerifyEndpoint() error = %v", err)
				}
				if got := n.count("getGenesisHash"); got != 1 {
					t.Errorf("getGenesisHash calls = %d, want 1", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("VerifyEndpoint() error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// Without the guarded client a request would leave through the default
// transport, past the address policy; the adapter refuses instead.
func TestVerifyEndpointRefusesToRunWithoutAGuardedClient(t *testing.T) {
	t.Parallel()

	n := newNode(t, mainnetGenesis)
	for name, client := range map[string]*http.Client{"nil client": nil, "nil transport": {}} {
		err := solanachain.VerifyEndpoint(t.Context(), mainnet(), n.server.URL, nil, client)
		if err == nil || !strings.Contains(err.Error(), "guarded HTTP client") {
			t.Errorf("%s: VerifyEndpoint() error = %v", name, err)
		}
	}
	if got := n.total(); got != 0 {
		t.Errorf("requests = %d, want none", got)
	}
}

func TestEndpointCredentialsArriveWithoutReplacingTheContentType(t *testing.T) {
	t.Parallel()

	n := newNode(t, mainnetGenesis)
	headers := http.Header{"X-Api-Key": {"secret"}, "Content-Type": {"text/plain"}}
	if err := solanachain.VerifyEndpoint(t.Context(), mainnet(), n.server.URL, headers, n.server.Client()); err != nil {
		t.Fatalf("VerifyEndpoint() error = %v", err)
	}
	seen := n.seenHeaders()
	if len(seen) != 1 {
		t.Fatalf("requests = %d, want 1", len(seen))
	}
	if got := seen[0].Get("X-Api-Key"); got != "secret" {
		t.Errorf("X-Api-Key = %q, want the endpoint's credential", got)
	}
	if got := seen[0].Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if headers.Get("Content-Type") != "text/plain" {
		t.Error("the caller's header set was modified")
	}
}

// healthyNode answers every probe call as a current mainnet node.
func healthyNode(t *testing.T, blockAge time.Duration) *node {
	t.Helper()
	n := newNode(t, mainnetGenesis)
	n.handle("getHealth", func([]json.RawMessage) reply { return reply{result: "ok"} })
	n.handle("getSlot", func(params []json.RawMessage) reply {
		if len(params) != 1 || !strings.Contains(string(params[0]), `"confirmed"`) {
			t.Errorf("getSlot params = %s, want the confirmed commitment", params)
		}
		return reply{result: 250_000_000}
	})
	n.handle("getBlockTime", func(params []json.RawMessage) reply {
		if len(params) != 1 || string(params[0]) != "250000000" {
			t.Errorf("getBlockTime params = %s, want the confirmed slot", params)
		}
		return reply{result: time.Now().Add(-blockAge).Unix()}
	})
	return n
}

func TestProbeEndpointRefusesUnhealthyOrStaleNodes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		age     time.Duration
		setup   func(*node)
		wantErr string
	}{
		{name: "current", age: 2 * time.Second},
		{name: "just inside the age limit", age: 110 * time.Second},
		{name: "clock slightly behind the cluster", age: -90 * time.Second},
		{name: "stale head", age: 3 * time.Minute, wantErr: "stale"},
		{name: "head far in the future", age: -3 * time.Minute, wantErr: "stale"},
		{name: "node behind", age: time.Second, wantErr: "unhealthy", setup: func(n *node) {
			n.handle("getHealth", func([]json.RawMessage) reply {
				return reply{err: &rpcError{Code: -32005, Message: "Node is behind by 300 slots"}}
			})
		}},
		{name: "odd health answer", age: time.Second, wantErr: `health "warning"`, setup: func(n *node) {
			n.handle("getHealth", func([]json.RawMessage) reply { return reply{result: "warning"} })
		}},
		{name: "wrong cluster", age: time.Second, wantErr: "genesis", setup: func(n *node) {
			n.handle("getGenesisHash", func([]json.RawMessage) reply { return reply{result: devnetGenesis} })
		}},
		{name: "zero slot", age: time.Second, wantErr: "slot is zero", setup: func(n *node) {
			n.handle("getSlot", func([]json.RawMessage) reply { return reply{result: 0} })
		}},
		{name: "slot unreadable", age: time.Second, wantErr: "confirmed slot", setup: func(n *node) {
			n.handle("getSlot", func([]json.RawMessage) reply { return reply{result: "soon"} })
		}},
		{name: "block time missing", age: time.Second, wantErr: "no time", setup: func(n *node) {
			n.handle("getBlockTime", func([]json.RawMessage) reply { return reply{result: nil} })
		}},
		{name: "block time unavailable", age: time.Second, wantErr: "block time", setup: func(n *node) {
			n.handle("getBlockTime", func([]json.RawMessage) reply {
				return reply{err: &rpcError{Code: -32004, Message: "Block not available for slot"}}
			})
		}},
		{name: "health not served", age: time.Second, wantErr: "unhealthy", setup: func(n *node) {
			n.handle("getHealth", func([]json.RawMessage) reply { return reply{status: http.StatusBadGateway, raw: "<html>"} })
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := healthyNode(t, tt.age)
			if tt.setup != nil {
				tt.setup(n)
			}
			err := solanachain.ProbeEndpoint(t.Context(), mainnet(), n.server.URL, nil, n.server.Client())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ProbeEndpoint() error = %v", err)
				}
				for _, method := range []string{"getGenesisHash", "getHealth", "getSlot", "getBlockTime"} {
					if got := n.count(method); got != 1 {
						t.Errorf("%s calls = %d, want 1", method, got)
					}
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ProbeEndpoint() error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
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

func TestAdapterEndpointChecksUseTheProvidedClient(t *testing.T) {
	t.Parallel()

	n := healthyNode(t, time.Second)
	transport := &countingTransport{base: n.server.Client().Transport}
	adapter := newAdapter(t, &provider{endpoints: []string{n.server.URL}, client: &http.Client{Transport: transport}})
	headers := http.Header{"Authorization": {"Bearer secret"}}
	if err := adapter.VerifyEndpoint(t.Context(), mainnet(), n.server.URL, headers); err != nil {
		t.Fatalf("VerifyEndpoint() error = %v", err)
	}
	if got := transport.count.Load(); got != 1 {
		t.Errorf("requests through the provided client after verify = %d, want 1", got)
	}
	if err := adapter.ProbeEndpoint(t.Context(), mainnet(), n.server.URL, headers); err != nil {
		t.Fatalf("ProbeEndpoint() error = %v", err)
	}
	if got := transport.count.Load(); got != 5 {
		t.Errorf("requests through the provided client after probe = %d, want 5", got)
	}
	for i, seen := range n.seenHeaders() {
		if seen.Get("Authorization") != "Bearer secret" {
			t.Errorf("request %d arrived without the endpoint's credential", i)
		}
	}
	if err := adapter.ProbeEndpoint(t.Context(), devnet(), n.server.URL, headers); err == nil {
		t.Error("ProbeEndpoint(devnet) accepted a mainnet node")
	}
}

// The built-in registry must name each Solana cluster by a ChainID whose
// genesis hash the adapter knows; otherwise every endpoint of the network
// would be refused.
func TestBuiltinSolanaNetworksHaveAKnownGenesis(t *testing.T) {
	t.Parallel()

	builtin, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	for id, genesis := range map[string]string{"solana-mainnet": mainnetGenesis, "solana-devnet": devnetGenesis} {
		item, err := builtin.Get(id)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
		if item.Family != solanachain.Family || len(item.RPCFallbacks) == 0 {
			t.Errorf("%s = %+v, want a Solana network with a public fallback", id, item)
		}
		n := newNode(t, genesis)
		if err := solanachain.VerifyEndpoint(t.Context(), item, n.server.URL, nil, n.server.Client()); err != nil {
			t.Errorf("%s: a node of its own cluster is refused: %v", id, err)
		}
	}
	solanaNetworks := 0
	for _, item := range builtin.List() {
		if item.Family == solanachain.Family {
			solanaNetworks++
		}
	}
	if solanaNetworks != 2 {
		t.Errorf("built-in Solana networks = %d, want 2", solanaNetworks)
	}
}

// A node's error object reaches the person as its code and message, not as a
// dump of the library's struct.
func TestNodeErrorsAreReadable(t *testing.T) {
	t.Parallel()

	n := healthyNode(t, time.Second)
	n.handle("getHealth", func([]json.RawMessage) reply {
		return reply{err: &rpcError{Code: -32005, Message: "Node is behind by 300 slots"}}
	})
	err := solanachain.ProbeEndpoint(t.Context(), mainnet(), n.server.URL, nil, n.server.Client())
	if err == nil || !strings.HasSuffix(err.Error(), "node answered -32005: Node is behind by 300 slots") {
		t.Errorf("ProbeEndpoint() error = %v", err)
	}
	if _, ok := errors.AsType[*jsonrpc.RPCError](err); !ok {
		t.Errorf("error %v does not keep the node's error object", err)
	}
}

// Behind a load balancer the slot and its time can come from two backends,
// and the second may not have the block yet. Only that answer is retried, a
// few slots back, and the step-back does not count as head age. Run on the
// bubble's clock, so the ages are exact.
func TestProbeEndpointStepsBackOnlyForABlockNotYetAvailable(t *testing.T) {
	t.Parallel()

	notAvailable := blockAnswer{err: &rpcError{Code: -32004, Message: "Block not available for slot"}}
	at := func(age time.Duration) blockAnswer { return blockAnswer{age: age} }
	tests := []struct {
		name      string
		answers   []blockAnswer
		wantSlots []string
		wantErr   string
	}{
		{name: "once", answers: []blockAnswer{notAvailable, at(time.Second)}, wantSlots: []string{"250000000", "249999996"}},
		{
			// 2 min 2 s old at 8 slots back is 1 min 58.8 s at the head.
			name: "step-back not held against the head", answers: []blockAnswer{notAvailable, notAvailable, at(maxHeadAgeForTest + 2*time.Second)},
			wantSlots: []string{"250000000", "249999996", "249999992"},
		},
		{
			name: "older than the step-back explains", answers: []blockAnswer{notAvailable, notAvailable, at(maxHeadAgeForTest + 4*time.Second)},
			wantSlots: []string{"250000000", "249999996", "249999992"}, wantErr: "stale",
		},
		{
			name: "never available", answers: []blockAnswer{notAvailable, notAvailable, notAvailable, at(time.Second)},
			wantSlots: []string{"250000000", "249999996", "249999992"}, wantErr: "Block not available",
		},
		{
			name: "skipped slot is final", answers: []blockAnswer{{err: &rpcError{Code: -32009, Message: "Slot was skipped"}}, at(time.Second)},
			wantSlots: []string{"250000000"}, wantErr: "Slot was skipped",
		},
		{
			name: "unhealthy is final", answers: []blockAnswer{{err: &rpcError{Code: -32005, Message: "Node is unhealthy"}}, at(time.Second)},
			wantSlots: []string{"250000000"}, wantErr: "unhealthy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				n := healthyNode(t, time.Second)
				var slots []string
				answers := tt.answers
				n.handle("getBlockTime", func(params []json.RawMessage) reply {
					slots = append(slots, string(params[0]))
					answer := answers[0]
					answers = answers[1:]
					if answer.err != nil {
						return reply{err: answer.err}
					}
					return reply{result: time.Now().Add(-answer.age).Unix()}
				})
				err := solanachain.ProbeEndpoint(t.Context(), mainnet(), n.server.URL, nil, n.server.Client())
				if tt.wantErr == "" && err != nil {
					t.Errorf("ProbeEndpoint() error = %v", err)
				}
				if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
					t.Errorf("ProbeEndpoint() error = %v, want %q", err, tt.wantErr)
				}
				if !slices.Equal(slots, tt.wantSlots) {
					t.Errorf("block times asked for slots %v, want %v", slots, tt.wantSlots)
				}
			})
		})
	}
}

type blockAnswer struct {
	age time.Duration
	err *rpcError
}

// maxHeadAgeForTest mirrors the probe's two-minute head age limit.
const maxHeadAgeForTest = 2 * time.Minute

func TestProbeEndpointRefusesWhatVerifyRefuses(t *testing.T) {
	t.Parallel()

	n := healthyNode(t, time.Second)
	if err := solanachain.ProbeEndpoint(t.Context(), networks()["tron-mainnet"], n.server.URL, nil, n.server.Client()); err == nil ||
		!strings.Contains(err.Error(), "not Solana") {
		t.Errorf("ProbeEndpoint(tron) error = %v", err)
	}
	if err := solanachain.ProbeEndpoint(t.Context(), mainnet(), n.server.URL, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "guarded HTTP client") {
		t.Errorf("ProbeEndpoint(no client) error = %v", err)
	}
	if got := n.total(); got != 0 {
		t.Errorf("requests = %d, want none", got)
	}
}

// Headers that only name the content type are no credential: nothing extra is
// sent, and the JSON-RPC headers are kept.
func TestContentTypeAloneIsNoCredential(t *testing.T) {
	t.Parallel()

	n := newNode(t, mainnetGenesis)
	headers := http.Header{"Content-Type": {"text/plain"}, "Accept": {"text/html"}}
	if err := solanachain.VerifyEndpoint(t.Context(), mainnet(), n.server.URL, headers, n.server.Client()); err != nil {
		t.Fatalf("VerifyEndpoint() error = %v", err)
	}
	seen := n.seenHeaders()[0]
	if seen.Get("Content-Type") != "application/json" || seen.Get("Accept") != "application/json" {
		t.Errorf("headers = %v, want the JSON-RPC ones kept", seen)
	}
}
