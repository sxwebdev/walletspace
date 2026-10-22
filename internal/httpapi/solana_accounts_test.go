package httpapi_test

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain"
	solanachain "github.com/sxwebdev/walletspace/internal/chain/solana"
	"github.com/sxwebdev/walletspace/internal/httpapi"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/operation"
	"github.com/sxwebdev/walletspace/internal/space"
)

const (
	closeUSDC    = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	closeTokenID = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
)

// closeNode is a Solana node with a wallet, its token accounts, and every
// transaction sent to it.
type closeNode struct {
	server *httptest.Server

	mu       sync.Mutex
	owner    string
	lamports uint64
	tokens   map[string][]byte // address → 165-byte token account
	sent     [][]byte
	onSend   func()
}

func emptyTokenAccount(mint, owner string, amount uint64) []byte {
	data := make([]byte, 165)
	copy(data[0:32], solana.MustPublicKeyFromBase58(mint).Bytes())
	copy(data[32:64], solana.MustPublicKeyFromBase58(owner).Bytes())
	binary.LittleEndian.PutUint64(data[64:72], amount)
	data[108] = 1
	return data
}

func newCloseNode(t *testing.T) *closeNode {
	t.Helper()
	node := &closeNode{tokens: map[string][]byte{}, lamports: 100_000_000}
	account := func(owner string, lamports uint64, data []byte) map[string]any {
		return map[string]any{
			"lamports": lamports, "owner": owner, "executable": false, "rentEpoch": 0,
			"data": []string{base64.StdEncoding.EncodeToString(data), "base64"}, "space": len(data),
		}
	}
	node.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     any               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&call)
		context1 := func(value any) any { return map[string]any{"context": map[string]any{"slot": 1}, "value": value} }
		node.mu.Lock()
		var result any
		switch call.Method {
		case "getGenesisHash":
			result = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
		case "getTokenAccountsByOwner":
			value := []any{}
			for address, data := range node.tokens {
				value = append(value, map[string]any{"pubkey": address, "account": account(closeTokenID, 2_039_280, data)})
			}
			result = context1(value)
		case "getMultipleAccounts":
			var keys []string
			_ = json.Unmarshal(call.Params[0], &keys)
			value := make([]any, len(keys))
			for i, key := range keys {
				if key == node.owner {
					value[i] = account("11111111111111111111111111111111", node.lamports, nil)
				} else if data, ok := node.tokens[key]; ok {
					value[i] = account(closeTokenID, 2_039_280, data)
				}
			}
			result = context1(value)
		case "getMinimumBalanceForRentExemption":
			result = 890_880
		case "getRecentPrioritizationFees":
			result = []any{}
		case "simulateTransaction":
			result = context1(map[string]any{"err": nil, "unitsConsumed": 3000})
		case "getLatestBlockhash":
			result = context1(map[string]any{"blockhash": "9zrUHnA1nCByPksy3aL8tQ47vqdaG2vnFs4HrxgcZj4F", "lastValidBlockHeight": 777})
		case "sendTransaction":
			var encoded string
			_ = json.Unmarshal(call.Params[0], &encoded)
			raw, _ := base64.StdEncoding.DecodeString(encoded)
			node.sent = append(node.sent, raw)
			onSend := node.onSend
			node.mu.Unlock()
			if onSend != nil {
				onSend()
			}
			node.mu.Lock()
			result = solana.SignatureFromBytes(raw[1:65]).String()
		}
		node.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
	t.Cleanup(node.server.Close)
	return node
}

type closeWallet struct {
	handler   http.Handler
	fixture   platformFixture
	node      *closeNode
	spaceID   string
	accountID string
	base      string
}

func newCloseWallet(t *testing.T) *closeWallet {
	t.Helper()
	fixture := newPlatformFixture(t)
	fixture.spaces.SetSendConfirmation(false, 0)
	node := newCloseNode(t)
	registry, err := network.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := solanachain.New(registry, fixedEndpoint{url: node.server.URL, client: node.server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	chains, err := chain.NewAdapters(map[network.Family]chain.Adapter{
		network.FamilySolana: adapter, network.FamilyTron: fixture.tron, network.FamilyEVM: fixture.evm,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewPlatform(
		fixture.spaces, fixture.settings, registry, operation.New(fixture.settings.Home()),
		fixture.assets, chains, fixture.evm, fixture.tron, fixture.doctor, fixture.prices,
		testAccess(), slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	created, err := fixture.spaces.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatal(err)
	}
	derived := platformRequest(t, handler, http.MethodPost, "/api/spaces/"+created.Space.ID+"/accounts/derive",
		map[string]string{"network_id": "solana-mainnet"})
	if derived.Code != http.StatusCreated {
		t.Fatalf("derive = %d %s", derived.Code, derived.Body)
	}
	account := decodeBody[struct {
		ID        string            `json:"id"`
		Addresses map[string]string `json:"addresses"`
	}](t, derived)
	node.owner = account.Addresses["solana"]
	return &closeWallet{
		handler: handler, fixture: fixture, node: node, spaceID: created.Space.ID, accountID: account.ID,
		base: "/api/spaces/" + created.Space.ID + "/networks/solana-mainnet/accounts/" + account.ID + "/token-accounts",
	}
}

func (c *closeWallet) close(t *testing.T, body map[string]any, key string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, c.base+"/close", bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set(httpapi.TokenHeader, testToken)
	request.Host = testHost
	return serve(t, c.handler, request)
}

// Listing, estimating and closing over the real routes: the transaction id and
// its expiry are on disk before the broadcast, and a replay of the same
// request answers with the same transaction instead of sending another.
func TestCloseTokenAccountsOverTheAPI(t *testing.T) {
	t.Parallel()

	c := newCloseWallet(t)
	empty, unknown, full := solana.PublicKeyFromBytes(append(make([]byte, 31), 41)).String(),
		solana.PublicKeyFromBytes(append(make([]byte, 31), 42)).String(),
		solana.PublicKeyFromBytes(append(make([]byte, 31), 43)).String()
	otherMint := solana.PublicKeyFromBytes(append(make([]byte, 31), 50)).String()
	c.node.tokens[empty] = emptyTokenAccount(closeUSDC, c.node.owner, 0)
	c.node.tokens[unknown] = emptyTokenAccount(otherMint, c.node.owner, 0)
	c.node.tokens[full] = emptyTokenAccount(closeUSDC, c.node.owner, 7)

	listed := platformRequest(t, c.handler, http.MethodGet, c.base+"/empty", nil)
	if listed.Code != http.StatusOK {
		t.Fatalf("list = %d %s", listed.Code, listed.Body)
	}
	list := decodeBody[struct {
		Accounts []struct {
			Address, Mint, Symbol, Rent string
		} `json:"accounts"`
		Max int `json:"max_per_transaction"`
	}](t, listed)
	if len(list.Accounts) != 2 || list.Max != solanachain.MaxCloseAccounts {
		t.Fatalf("list = %s", listed.Body)
	}
	symbols := map[string]string{}
	for _, item := range list.Accounts {
		symbols[item.Address] = item.Symbol
		if item.Rent != "0.00203928" {
			t.Errorf("rent = %s", item.Rent)
		}
	}
	if symbols[empty] != "USDC" || symbols[unknown] != "" {
		t.Errorf("symbols = %v, want USDC for the known mint and none for the other", symbols)
	}

	body := map[string]any{"accounts": []string{empty, unknown}}
	estimated := platformRequest(t, c.handler, http.MethodPost, c.base+"/close/estimate", body)
	if estimated.Code != http.StatusOK {
		t.Fatalf("estimate = %d %s", estimated.Code, estimated.Body)
	}
	estimate := decodeBody[map[string]any](t, estimated)
	if estimate["returned"] != "0.00407856" {
		t.Errorf("estimate = %v, want both rents returned", estimate)
	}
	for _, field := range []string{"fee", "fee_model", "compute_unit_limit", "compute_unit_price_micro_lamports"} {
		body[field] = estimate[field]
	}

	var atBroadcast operation.Operation
	c.node.onSend = func() {
		atBroadcast, _, _ = operation.New(c.fixture.settings.Home()).Get(c.spaceID, "close-1")
	}
	closed := c.close(t, body, "close-1")
	if closed.Code != http.StatusAccepted {
		t.Fatalf("close = %d %s", closed.Code, closed.Body)
	}
	answer := decodeBody[map[string]string](t, closed)
	if len(c.node.sent) != 1 {
		t.Fatalf("broadcasts = %d, want 1", len(c.node.sent))
	}
	txID := solana.SignatureFromBytes(c.node.sent[0][1:65]).String()
	if answer["tx_id"] != txID || answer["status"] != "pending" {
		t.Errorf("close answer = %v, want pending %s", answer, txID)
	}
	if atBroadcast.TxHash != txID || atBroadcast.Status != operation.StatusBroadcasting || atBroadcast.LastValidBlockHeight != 777 {
		t.Errorf("record at broadcast = %+v, want %s broadcasting with expiry 777", atBroadcast, txID)
	}

	replay := c.close(t, body, "close-1")
	if replay.Code != http.StatusOK || decodeBody[map[string]string](t, replay)["tx_id"] != txID {
		t.Errorf("replay = %d %s, want the same transaction", replay.Code, replay.Body)
	}
	if len(c.node.sent) != 1 {
		t.Errorf("broadcasts after the replay = %d, want still 1", len(c.node.sent))
	}

	// An account that received tokens since it was listed is a conflict and
	// nothing is sent.
	body["accounts"] = []string{full}
	if refused := c.close(t, body, "close-2"); refused.Code != http.StatusConflict {
		t.Errorf("close of a non-empty account = %d %s, want 409", refused.Code, refused.Body)
	}
	if len(c.node.sent) != 1 {
		t.Errorf("broadcasts = %d, want still 1", len(c.node.sent))
	}
}

// The routes refuse networks that are not Solana, as the Tron-only routes
// refuse other families, and accounts without a Solana address.
func TestCloseTokenAccountsRoutesAreSolanaOnly(t *testing.T) {
	t.Parallel()

	c := newCloseWallet(t)
	for _, networkID := range []string{"tron-mainnet", "ethereum-mainnet"} {
		path := "/api/spaces/" + c.spaceID + "/networks/" + networkID + "/accounts/" + c.accountID + "/token-accounts/empty"
		if response := platformRequest(t, c.handler, http.MethodGet, path, nil); response.Code != http.StatusBadRequest {
			t.Errorf("%s: list = %d %s, want 400", networkID, response.Code, response.Body)
		}
	}
	path := "/api/spaces/" + c.spaceID + "/networks/solana-mainnet/accounts/acc_missing/token-accounts/empty"
	if response := platformRequest(t, c.handler, http.MethodGet, path, nil); response.Code != http.StatusNotFound {
		t.Errorf("unknown account: list = %d %s, want 404", response.Code, response.Body)
	}
	if response := platformRequest(t, c.handler, http.MethodPost, c.base+"/close/estimate", map[string]any{"accounts": []string{}}); response.Code != http.StatusBadRequest {
		t.Errorf("estimate of nothing = %d %s, want 400", response.Code, response.Body)
	}
	if response := platformRequest(t, c.handler, http.MethodPost, c.base+"/close/estimate", map[string]any{"unknown": 1}); response.Code != http.StatusBadRequest {
		t.Errorf("estimate with an unknown field = %d %s, want 400", response.Code, response.Body)
	}
}

// Closing spends from the wallet (the fee), so it asks for the same spending
// step-up as a transfer.
func TestCloseTokenAccountsRequireTheSpendingStepUp(t *testing.T) {
	t.Parallel()

	c := newCloseWallet(t)
	c.fixture.spaces.SetSendConfirmation(true, 0)
	if response := c.close(t, map[string]any{"accounts": []string{c.node.owner}}, "close-1"); response.Code != http.StatusForbidden {
		t.Errorf("close without the step-up = %d %s, want 403", response.Code, response.Body)
	}
	if len(c.node.sent) != 0 {
		t.Error("something was sent")
	}
}
