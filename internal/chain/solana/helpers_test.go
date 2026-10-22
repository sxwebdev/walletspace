package solana_test

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain"
	solanachain "github.com/sxwebdev/walletspace/internal/chain/solana"
	"github.com/sxwebdev/walletspace/internal/network"
)

const (
	mainnetGenesis = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
	devnetGenesis  = "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG"
	usdcMint       = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	usdtMint       = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	tokenProgram   = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	token2022      = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	metaplex       = "metaqbxxUerdq28cj1RbAWkYQm3ybzjb6a8bt518x1s"
	systemProgram  = "11111111111111111111111111111111"
)

// Test addresses: valid 32-byte keys with no meaning on any cluster.
var (
	ownerA = key(1)
	ownerB = key(2)
)

func key(seed byte) string {
	var raw [32]byte
	for i := range raw {
		raw[i] = seed
	}
	return solana.PublicKeyFromBytes(raw[:]).String()
}

func mainnet() network.Network {
	return network.Network{
		ID: "solana-mainnet", Name: "Solana", ShortName: "Mainnet", Family: solanachain.Family,
		ChainID: "mainnet", Native: network.NativeAsset{Symbol: "SOL", Decimals: 9}, Enabled: true,
	}
}

func devnet() network.Network {
	item := mainnet()
	item.ID, item.ChainID, item.ShortName, item.Testnet = "solana-devnet", "devnet", "Devnet", true
	return item
}

// registry is the network lookup the adapter gets in tests.
type registry map[string]network.Network

func (r registry) Get(id string) (network.Network, error) {
	item, ok := r[id]
	if !ok {
		return network.Network{}, fmt.Errorf("%w: %s", network.ErrUnknownNetwork, id)
	}
	return item, nil
}

func networks() registry {
	tron := network.Network{ID: "tron-mainnet", Family: network.FamilyTron, ChainID: "mainnet"}
	odd := mainnet()
	odd.ID, odd.ChainID = "solana-odd", "testnet"
	return registry{"solana-mainnet": mainnet(), "solana-devnet": devnet(), "tron-mainnet": tron, "solana-odd": odd}
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// reply is one fake answer: a result, a JSON-RPC error, or a raw HTTP answer.
type reply struct {
	result any
	err    *rpcError
	status int
	raw    string
}

// handler answers one JSON-RPC method.
type handler func(params []json.RawMessage) reply

// node is a fake Solana JSON-RPC endpoint.
type node struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	handlers map[string]handler
	calls    map[string]int
	params   map[string][]json.RawMessage
	headers  []http.Header
}

func newNode(t *testing.T, genesis string) *node {
	t.Helper()
	n := &node{t: t, handlers: make(map[string]handler), calls: make(map[string]int), params: make(map[string][]json.RawMessage)}
	n.handle("getGenesisHash", func([]json.RawMessage) reply { return reply{result: genesis} })
	n.server = httptest.NewTestServer(t, http.HandlerFunc(n.serve))
	// URL is set by the first call to Client.
	_ = n.server.Client()
	t.Cleanup(n.server.Close)
	return n
}

func (n *node) handle(method string, h handler) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.handlers[method] = h
}

func (n *node) count(method string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls[method]
}

func (n *node) total() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	sum := 0
	for _, c := range n.calls {
		sum += c
	}
	return sum
}

func (n *node) lastParams(method string) []json.RawMessage {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.params[method]
}

func (n *node) seenHeaders() []http.Header {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]http.Header(nil), n.headers...)
}

func (n *node) serve(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID     any               `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		n.t.Errorf("decode RPC request: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	n.mu.Lock()
	n.calls[request.Method]++
	n.params[request.Method] = request.Params
	n.headers = append(n.headers, r.Header.Clone())
	h := n.handlers[request.Method]
	n.mu.Unlock()
	if h == nil {
		writeReply(w, request.ID, reply{err: &rpcError{Code: -32601, Message: "Method not found"}})
		return
	}
	writeReply(w, request.ID, h(request.Params))
}

func writeReply(w http.ResponseWriter, id any, answer reply) {
	if answer.status != 0 || answer.raw != "" {
		w.WriteHeader(max(answer.status, http.StatusOK))
		_, _ = w.Write([]byte(answer.raw))
		return
	}
	body := map[string]any{"jsonrpc": "2.0", "id": id}
	if answer.err != nil {
		body["error"] = answer.err
	} else {
		body["result"] = answer.result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// account is an account in the base64 encoding the adapter asks for.
func account(owner string, lamports uint64, data []byte) map[string]any {
	return map[string]any{
		"lamports": lamports, "owner": owner, "executable": false, "rentEpoch": 0,
		"data": []string{base64.StdEncoding.EncodeToString(data), "base64"}, "space": len(data),
	}
}

func withContext(value any) map[string]any {
	return map[string]any{"context": map[string]any{"slot": 1}, "value": value}
}

// tokenSlice is the first 72 bytes of an SPL token account.
func tokenSlice(mint, owner string, amount uint64) []byte {
	data := make([]byte, 72)
	copy(data[0:32], solana.MustPublicKeyFromBase58(mint).Bytes())
	copy(data[32:64], solana.MustPublicKeyFromBase58(owner).Bytes())
	binary.LittleEndian.PutUint64(data[64:72], amount)
	return data
}

func tokenAccount(mint, owner string, amount uint64) map[string]any {
	return map[string]any{"pubkey": key(byte(amount%200) + 50), "account": account(tokenProgram, 2039280, tokenSlice(mint, owner, amount))}
}

// requestedKeys decodes the address list of a getMultipleAccounts call.
func requestedKeys(t *testing.T, params []json.RawMessage) []string {
	t.Helper()
	var keys []string
	if len(params) == 0 || json.Unmarshal(params[0], &keys) != nil {
		t.Fatalf("getMultipleAccounts params = %s", params)
	}
	return keys
}

// requestedOwner decodes the owner of a getTokenAccountsByOwner call.
func requestedOwner(t *testing.T, params []json.RawMessage) string {
	t.Helper()
	var owner string
	if len(params) == 0 || json.Unmarshal(params[0], &owner) != nil {
		t.Errorf("getTokenAccountsByOwner params = %s", params)
	}
	return owner
}

// provider is the endpoint provider the adapter gets in tests.
type provider struct {
	endpoints []string
	client    *http.Client
	headers   map[string]http.Header
	// Failure and timing hooks.
	endpointsErr error
	headersErr   error
	onEndpoints  func()

	mu          sync.Mutex
	healthy     []string
	invalidated []string
}

func (p *provider) Endpoints(context.Context, network.Network) ([]string, error) {
	if p.onEndpoints != nil {
		p.onEndpoints()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.endpoints...), p.endpointsErr
}

// setEndpoints replaces the endpoint list, as a settings change does.
func (p *provider) setEndpoints(endpoints ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endpoints = endpoints
}

func (p *provider) HTTPClient(network.Network) *http.Client { return p.client }

func (p *provider) Headers(_ network.Network, endpoint string) (http.Header, error) {
	return p.headers[endpoint].Clone(), p.headersErr
}

func (p *provider) MarkHealthy(_ network.Network, endpoint string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.healthy = append(p.healthy, endpoint)
}

func (p *provider) Invalidate(networkID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.invalidated = append(p.invalidated, networkID)
}

func newAdapter(t *testing.T, p *provider) *solanachain.Adapter {
	t.Helper()
	adapter, err := solanachain.New(networks(), p)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return adapter
}

// single wires an adapter to one fake node.
func single(t *testing.T, n *node) (*solanachain.Adapter, *provider) {
	t.Helper()
	p := &provider{endpoints: []string{n.server.URL}, client: n.server.Client()}
	return newAdapter(t, p), p
}

func native(networkID string) chain.Asset {
	return chain.Asset{ID: networkID + ":native", NetworkID: networkID, Kind: "native", Symbol: "SOL", Decimals: 9}
}

func spl(networkID, mint, symbol string, decimals uint8) chain.Asset {
	return chain.Asset{
		ID: networkID + ":spl:" + mint, NetworkID: networkID, Kind: "spl", Symbol: symbol,
		Decimals: decimals, Contract: mint,
	}
}

func collect(ch <-chan chain.Balance) []chain.Balance {
	var out []chain.Balance
	for balance := range ch {
		out = append(out, balance)
	}
	return out
}

// byKey indexes balances by account and asset; a duplicate is a test failure
// unless the stream was expected to answer twice.
func byKey(t *testing.T, balances []chain.Balance) map[string]chain.Balance {
	t.Helper()
	out := make(map[string]chain.Balance, len(balances))
	for _, balance := range balances {
		k := balance.AccountID + "|" + balance.AssetID
		if _, dup := out[k]; dup {
			t.Errorf("balance %s answered twice", k)
		}
		out[k] = balance
	}
	return out
}
