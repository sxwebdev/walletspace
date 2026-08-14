// Package integration drives the assembled wallet the way an attacker would:
// over a real socket, through the real guard, against nodes that answer
// honestly right up until the moment it pays them not to.
//
// Every mechanism here is covered by a unit test somewhere else. What those
// cannot show is the seams — the guard reached over TCP rather than through
// httptest's in-process handler, an endpoint that survives verification and
// then misbehaves, an idempotency key travelling from one HTTP request to the
// next. The audit asked for exactly this suite, and the defects found while
// reviewing the remediation were nearly all at seams of this kind.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/asset"
	evmchain "github.com/sxwebdev/walletspace/internal/chain/evm"
	tronchain "github.com/sxwebdev/walletspace/internal/chain/tron"
	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/doctor"
	"github.com/sxwebdev/walletspace/internal/httpapi"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/operation"
	"github.com/sxwebdev/walletspace/internal/price"
	"github.com/sxwebdev/walletspace/internal/rpcpool"
	"github.com/sxwebdev/walletspace/internal/space"
	"github.com/sxwebdev/walletspace/internal/vault"
)

const (
	spacePassword = "integration-space-password"
	ethereum      = "ethereum-mainnet"
	nativeAsset   = ethereum + ":native"
	// A well-known throwaway key's address, which is what the fake node is
	// asked about. Nothing signs with it here; the wallet derives its own.
	burnAddress = "0x000000000000000000000000000000000000dEaD"
	// knownPhrase seeds every space so that "did the recovery phrase leak?" is a
	// question a test can answer. A generated phrase is twelve random words, so
	// grepping a response for any particular one of them almost always misses —
	// which is how the rebinding test came to assert nothing at all.
	knownPhrase = "abandon abandon abandon abandon abandon abandon " +
		"abandon abandon abandon abandon abandon about"
)

// wallet is a running Walletspace: a real listener on a real loopback port,
// with the capability token the process would have printed.
type wallet struct {
	t       *testing.T
	baseURL string
	host    string
	token   string
	client  *http.Client
}

func start(t *testing.T) *wallet {
	t.Helper()
	return startWithConfig(t, "")
}

// startWithConfig starts a wallet over a config.yaml written before it boots,
// which is the only way to exercise a setting the API deliberately will not
// change. An empty string means "no file", i.e. the shipped defaults.
func startWithConfig(t *testing.T, configYAML string) *wallet {
	t.Helper()

	home := t.TempDir()
	if configYAML != "" {
		if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(configYAML), 0o600); err != nil {
			t.Fatalf("write config.yaml: %v", err)
		}
	}
	settings, err := config.NewHomeManager(home)
	if err != nil {
		t.Fatalf("config.NewHomeManager() error = %v", err)
	}
	// Argon2 at the real parameters would put a second and 64 MiB on every
	// unlock in this file. The KDF is not what is under test here.
	spaces, err := space.NewManager(home, 15*time.Minute, vault.Params{
		Time: 2, MemoryKiB: 32 * 1024, Parallelism: 1,
	})
	if err != nil {
		t.Fatalf("space.NewManager() error = %v", err)
	}
	// Mirrors cmd/walletspace/main.go. Without it the suite exercises the
	// manager's own default rather than the settings that reach it, and the
	// whole config-to-manager path for the spending step-up — the file, the
	// decode, the push — would be free to break with every test still green.
	snapshot := settings.Snapshot()
	spaces.SetSendConfirmation(
		snapshot.Config.Security.ConfirmSends, snapshot.Config.Security.SendGrantTTL,
	)
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	resolver := rpcpool.New(settings)
	evm, err := evmchain.New(registry, resolver)
	if err != nil {
		t.Fatalf("evm.New() error = %v", err)
	}
	tron, err := tronchain.New(t.Context(), registry, settings, resolver, logger)
	if err != nil {
		t.Fatalf("tron.New() error = %v", err)
	}
	nodeDoctor, err := doctor.New(
		t.Context(), registry, resolver,
		func(context.Context, network.Network, string) error { return nil },
		doctor.Options{Interval: time.Hour},
	)
	if err != nil {
		t.Fatalf("doctor.New() error = %v", err)
	}

	// The listener first, exactly as main does: the guard's idea of which Host
	// values are its own comes from the address the kernel actually handed out.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	token, err := httpapi.NewToken()
	if err != nil {
		t.Fatalf("httpapi.NewToken() error = %v", err)
	}
	access, err := httpapi.LoopbackAccess(token, listener.Addr())
	if err != nil {
		t.Fatalf("httpapi.LoopbackAccess() error = %v", err)
	}
	handler, err := httpapi.NewPlatform(
		spaces, settings, registry, operation.New(home), mustAssets(t, home),
		evm, tron, nodeDoctor, stubPrices{}, access, logger,
	)
	if err != nil {
		t.Fatalf("httpapi.NewPlatform() error = %v", err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		nodeDoctor.Close()
		tron.Close()
		evm.Close()
		spaces.Close()
	})

	return &wallet{
		t:       t,
		baseURL: "http://" + listener.Addr().String(),
		host:    listener.Addr().String(),
		token:   token,
		client: &http.Client{
			Timeout: 30 * time.Second,
			// A redirect would be followed with the token still attached, and
			// nothing here should be redirecting in the first place.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func mustAssets(t *testing.T, home string) *asset.Store {
	t.Helper()
	store, err := asset.New(home)
	if err != nil {
		t.Fatalf("asset.New() error = %v", err)
	}
	return store
}

// stubPrices keeps the price feed from reaching the network. Balances are not
// what this suite is about.
type stubPrices struct{}

func (stubPrices) Quotes(context.Context, []string) (price.Snapshot, error) {
	return price.Snapshot{}, nil
}

type reply struct {
	status int
	body   []byte
	header http.Header
}

func (r reply) json(t *testing.T) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(r.body, &value); err != nil {
		t.Fatalf("decode %q: %v", r.body, err)
	}
	return value
}

func (r reply) text() string { return strings.TrimSpace(string(r.body)) }

// call makes an ordinary authenticated request, the way the UI does.
func (w *wallet) call(method, path string, body any, options ...func(*http.Request)) reply {
	w.t.Helper()
	return w.raw(method, path, body, append([]func(*http.Request){
		func(r *http.Request) { r.Header.Set(httpapi.TokenHeader, w.token) },
	}, options...)...)
}

// raw makes a request with nothing added: no token, no assumptions. The
// boundary tests build on it, because what is being tested is what the guard
// does with a request the UI would never send.
func (w *wallet) raw(method, path string, body any, options ...func(*http.Request)) reply {
	w.t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			w.t.Fatalf("json.Marshal() error = %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(w.t.Context(), method, w.baseURL+path, reader)
	if err != nil {
		w.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for _, option := range options {
		option(request)
	}
	response, err := w.client.Do(request)
	if err != nil {
		w.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		w.t.Fatalf("read %s %s: %v", method, path, err)
	}
	return reply{status: response.StatusCode, body: payload, header: response.Header}
}

func header(name, value string) func(*http.Request) {
	// net/http writes the Host header from Request.Host and drops any entry in
	// Request.Header, so header("Host", …) is silently ignored — a boundary test
	// built on it forges nothing. Refuse it here rather than let the next person
	// write a test that passes for no reason.
	if strings.EqualFold(name, "Host") {
		panic("use forgedHost: net/http ignores Request.Header[\"Host\"]")
	}
	return func(r *http.Request) { r.Header.Set(name, value) }
}

// forgedHost is what a DNS-rebinding page's request looks like on the wire: the
// attacker's own hostname, on a connection to this process's port.
func forgedHost(value string) func(*http.Request) {
	return func(r *http.Request) { r.Host = value }
}

// createSpace makes a space and returns its id. It is left unlocked, which is
// the state every interesting attack starts from.
func (w *wallet) createSpace() string {
	w.t.Helper()
	created := w.call(http.MethodPost, "/api/spaces", map[string]any{
		"name": "integration", "password": spacePassword, "first": true,
		"mnemonic": knownPhrase,
	})
	if created.status != http.StatusCreated {
		w.t.Fatalf("POST /api/spaces = %d %s", created.status, created.text())
	}
	body := created.json(w.t)
	spaceValue, ok := body["space"].(map[string]any)
	if !ok {
		w.t.Fatalf("POST /api/spaces returned no space: %s", created.text())
	}
	id, _ := spaceValue["id"].(string)
	if id == "" {
		w.t.Fatalf("POST /api/spaces returned no space id: %s", created.text())
	}
	return id
}

// deriveEVMAccount adds an Ethereum wallet to a space and returns its id.
func (w *wallet) deriveEVMAccount(spaceID string) string {
	w.t.Helper()
	derived := w.call(http.MethodPost, "/api/spaces/"+spaceID+"/accounts/derive", map[string]any{
		"network_id": ethereum,
	})
	if derived.status != http.StatusCreated {
		w.t.Fatalf("derive account = %d %s", derived.status, derived.text())
	}
	id, _ := derived.json(w.t)["id"].(string)
	if id == "" {
		w.t.Fatalf("derive account returned no id: %s", derived.text())
	}
	return id
}

// confirmSend opens the spending window. Sending is a step-up of its own:
// having the space unlocked is not authority to move what is in it.
func (w *wallet) confirmSend(spaceID string) {
	w.t.Helper()
	granted := w.call(http.MethodPost, "/api/spaces/"+spaceID+"/confirm-send",
		map[string]any{"password": spacePassword})
	if granted.status != http.StatusOK {
		w.t.Fatalf("confirm-send = %d %s", granted.status, granted.text())
	}
}

// revision reads the settings ETag, which every settings write is gated on.
func (w *wallet) revision() string {
	w.t.Helper()
	current := w.call(http.MethodGet, "/api/settings", nil)
	if current.status != http.StatusOK {
		w.t.Fatalf("GET /api/settings = %d %s", current.status, current.text())
	}
	value, _ := current.json(w.t)["revision"].(string)
	return value
}

// useNode points Ethereum at a node of the caller's choosing.
//
// Private addresses have to be allowed first. Every RPC connection goes through
// a dialer that refuses loopback, so a fake node on 127.0.0.1 is exactly what
// the wallet is built to reject — which is the point of the switch, and the
// reason an integration test has to throw it deliberately.
func (w *wallet) useNode(url string) {
	w.t.Helper()
	allowed := w.call(http.MethodPatch, "/api/settings/node-discovery", map[string]any{
		"enabled": false, "url": "", "refresh_interval": "30m",
		"request_timeout": "5s", "allow_insecure_rpc": true,
	}, header("If-Match", `"`+w.revision()+`"`))
	if allowed.status != http.StatusOK {
		w.t.Fatalf("PATCH node-discovery = %d %s", allowed.status, allowed.text())
	}
	saved := w.call(http.MethodPut, "/api/settings/networks/"+ethereum, map[string]any{
		"enabled":   true,
		"endpoints": []map[string]any{{"url": url}},
	}, header("If-Match", `"`+w.revision()+`"`))
	if saved.status != http.StatusOK {
		w.t.Fatalf("PUT network settings = %d %s", saved.status, saved.text())
	}
}

// rpcCall is one JSON-RPC request as the fake nodes below receive it.
type rpcCall struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// fakeNode is an Ethereum node that answers whatever the test tells it to.
type fakeNode struct {
	server *httptest.Server
	url    string

	mu    sync.Mutex
	calls map[string]int
	sent  [][]byte
	// nonce advances with each transaction the node receives, as a real one's
	// pending count does. Without it every rebuild of the same transfer signs
	// byte-identical bytes, and a genuine double spend is indistinguishable
	// from a re-send of the same transaction.
	nonce int
}

// newFakeNode starts a node that answers the ordinary queries plausibly.
//
// override gets first refusal on every call and reports whether it dealt with
// it — by writing an answer, or by not writing one at all, which is how a lost
// broadcast is staged. Anything it declines falls through to standard.
func newFakeNode(t *testing.T, override func(*fakeNode, rpcCall, http.ResponseWriter) bool) *fakeNode {
	t.Helper()
	node := &fakeNode{calls: map[string]int{}}
	node.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call rpcCall
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		node.mu.Lock()
		node.calls[call.Method]++
		if call.Method == "eth_sendRawTransaction" {
			node.sent = append(node.sent, append([]byte(nil), call.Params...))
			// Counted on receipt, not on a successful answer: a node that takes
			// the transaction and then loses the connection has still taken it.
			node.nonce++
		}
		node.mu.Unlock()

		if override != nil && override(node, call, w) {
			return
		}
		result, err := node.standard(call)
		node.answer(w, call, result, err)
	}))
	t.Cleanup(node.server.Close)
	node.url = node.server.URL
	return node
}

// answer writes one JSON-RPC reply.
func (n *fakeNode) answer(w http.ResponseWriter, call rpcCall, result any, err error) {
	response := map[string]any{"jsonrpc": "2.0", "id": call.ID}
	if err != nil {
		response["error"] = map[string]any{"code": -32000, "message": err.Error()}
	} else {
		response["result"] = result
	}
	_ = json.NewEncoder(w).Encode(response)
}

// standard is a node that behaves: mainnet, a funded account, a legacy fee
// model at one gwei.
func (n *fakeNode) standard(call rpcCall) (any, error) {
	switch call.Method {
	case "eth_chainId":
		return "0x1", nil
	case "eth_blockNumber":
		return "0x100", nil
	case "eth_getBalance":
		return "0xde0b6b3a7640000", nil // 1 ETH
	case "eth_estimateGas":
		return "0x5208", nil // 21,000
	case "eth_getTransactionCount":
		n.mu.Lock()
		defer n.mu.Unlock()
		return fmt.Sprintf("0x%x", 7+n.nonce), nil
	case "eth_gasPrice":
		return "0x3b9aca00", nil // 1 gwei
	case "eth_getBlockByNumber", "eth_maxPriorityFeePerGas":
		// No base fee: the legacy path is the one with the fewest moving parts.
		return nil, fmt.Errorf("legacy node")
	case "eth_sendRawTransaction":
		return "0x" + strings.Repeat("11", 32), nil
	default:
		return nil, fmt.Errorf("unexpected method %s", call.Method)
	}
}

func (n *fakeNode) count(method string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls[method]
}

func (n *fakeNode) broadcasts() int { return n.count("eth_sendRawTransaction") }

// distinctBroadcasts is what tells a re-send of the same bytes from a second,
// independently built transaction — the difference between a retry and a
// double spend.
func (n *fakeNode) distinctBroadcasts() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	seen := make(map[string]struct{}, len(n.sent))
	for _, params := range n.sent {
		seen[string(params)] = struct{}{}
	}
	return len(seen)
}
