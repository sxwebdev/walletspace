package httpapi_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/chain/chaintest"
	solanachain "github.com/sxwebdev/walletspace/internal/chain/solana"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
	"github.com/sxwebdev/walletspace/internal/httpapi"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/operation"
	"github.com/sxwebdev/walletspace/internal/space"
)

const (
	swapPool      = "2QdhepnKRTLjjSqPL1PtKNwqrUkoLee5Gqs8bvZhRdMv" // CLMM SOL/USDC, captured on mainnet
	swapExtension = "9z9VTNvaTpJuwjn4LSnjHwZgUR9iGuy59BwXTNbxRF6s"
	swapTable     = "47Fi43gX1EHVN1LX1Nu73MDpmZ3yooB8fwtBuxufyzX7"
	swapTick      = "8Zo3x55Q81YL8Bev6EPULCNmoGNtwCT58d1KswzRTvbQ"
	wsol          = "So11111111111111111111111111111111111111112"
	usdc          = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	tokenProgram  = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
)

type rawAccount struct {
	Owner    string `json:"owner"`
	Lamports uint64 `json:"lamports"`
	Data     string `json:"data"`
}

func capturedAccount(t *testing.T, address string) rawAccount {
	t.Helper()
	for _, dir := range []string{"../chain/solana/raydium/testdata/accounts", "../chain/solana/testdata/swap"} {
		raw, err := os.ReadFile(filepath.Join(dir, address+".json"))
		if err != nil {
			continue
		}
		var account rawAccount
		if err := json.Unmarshal(raw, &account); err != nil {
			t.Fatal(err)
		}
		return account
	}
	t.Fatalf("no fixture for %s", address)
	return rawAccount{}
}

func mintAccount(decimals byte) rawAccount {
	data := make([]byte, 82)
	data[44], data[45] = decimals, 1
	return rawAccount{Owner: tokenProgram, Lamports: 1, Data: base64.StdEncoding.EncodeToString(data)}
}

func tokenAccountOf(mint, owner string, amount uint64) rawAccount {
	data := make([]byte, 165)
	copy(data, solana.MustPublicKeyFromBase58(mint).Bytes())
	copy(data[32:], solana.MustPublicKeyFromBase58(owner).Bytes())
	binary.LittleEndian.PutUint64(data[64:], amount)
	data[108] = 1
	return rawAccount{Owner: tokenProgram, Lamports: 1_488_440, Data: base64.StdEncoding.EncodeToString(data)}
}

func ataOf(owner, mint string) string {
	address, _, err := solana.FindAssociatedTokenAddress(solana.MustPublicKeyFromBase58(owner), solana.MustPublicKeyFromBase58(mint))
	if err != nil {
		panic(err)
	}
	return address.String()
}

// swapNode is a Solana node for one SOL → USDC swap: the captured pool, a
// simulation that pays 1.043581 USDC, and a record of what is sent.
type swapNode struct {
	mu       sync.Mutex
	accounts map[string]rawAccount
	payout   uint64
	sent     [][]byte
}

func (n *swapNode) serve(w http.ResponseWriter, r *http.Request) {
	var call struct {
		ID     any               `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&call)
	withContext := func(value any) any { return map[string]any{"context": map[string]any{"slot": 1}, "value": value} }
	encode := func(a rawAccount) any {
		return map[string]any{"owner": a.Owner, "lamports": a.Lamports, "data": []string{a.Data, "base64"}, "executable": false, "rentEpoch": 0}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	var result any
	switch call.Method {
	case "getGenesisHash":
		result = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
	case "getMultipleAccounts":
		var keys []string
		_ = json.Unmarshal(call.Params[0], &keys)
		value := make([]any, len(keys))
		for i, key := range keys {
			if found, ok := n.accounts[key]; ok {
				value[i] = encode(found)
			}
		}
		result = withContext(value)
	case "getMinimumBalanceForRentExemption":
		result = 1_488_440
		if string(call.Params[0]) == "0" {
			result = 650_240
		}
	case "getRecentPrioritizationFees":
		result = []any{}
	case "getSlot":
		result = 400_000_000
	case "getBlockTime":
		result = 1_792_670_000
	case "simulateTransaction":
		var opts struct {
			Accounts *struct {
				Addresses []string `json:"addresses"`
			} `json:"accounts"`
		}
		_ = json.Unmarshal(call.Params[1], &opts)
		value := map[string]any{"err": nil, "unitsConsumed": 120_000, "logs": []string{}}
		if opts.Accounts != nil {
			var after []any
			for _, address := range opts.Accounts.Addresses {
				var owner string
				for key := range n.accounts {
					if ataOf(key, usdc) == address {
						owner = key
					}
				}
				after = append(after, encode(tokenAccountOf(usdc, owner, n.payout)))
			}
			value["accounts"] = after
		}
		result = withContext(value)
	case "getLatestBlockhash":
		result = withContext(map[string]any{"blockhash": "9zrUHnA1nCByPksy3aL8tQ47vqdaG2vnFs4HrxgcZj4F", "lastValidBlockHeight": 4242})
	case "sendTransaction":
		var encoded string
		_ = json.Unmarshal(call.Params[0], &encoded)
		raw, _ := base64.StdEncoding.DecodeString(encoded)
		n.sent = append(n.sent, raw)
		result = solana.SignatureFromBytes(raw[1:65]).String()
	case "getSignatureStatuses":
		result = withContext([]any{nil})
	case "getBlockHeight":
		result = 4000
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
}

func TestSolanaSwapOverHTTP(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	fixture.spaces.SetSendConfirmation(false, 0)
	created, err := fixture.spaces.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatal(err)
	}
	spaceID := created.Space.ID

	n := &swapNode{accounts: map[string]rawAccount{}, payout: 1_043_581}
	for _, address := range []string{swapPool, swapExtension, swapTable} {
		n.accounts[address] = capturedAccount(t, address)
	}
	n.accounts[wsol], n.accounts[usdc] = mintAccount(9), mintAccount(6)
	node := httptest.NewServer(http.HandlerFunc(n.serve))
	t.Cleanup(node.Close)

	keys, err := os.ReadFile("../chain/solana/raydium/testdata/poolkeys_api_v3.json")
	if err != nil {
		t.Fatal(err)
	}
	quote, err := os.ReadFile("../chain/solana/raydium/testdata/quotes/sol_usdc_small.json")
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/compute/swap-base-in":
			_, _ = w.Write(quote)
		case "/pools/key/ids":
			_, _ = w.Write(keys)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)

	registry, err := network.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := solanachain.New(registry, fixedEndpoint{url: node.URL, client: node.Client()})
	if err != nil {
		t.Fatal(err)
	}
	quoter, err := raydium.NewQuoter(api.Client(), api.URL, raydium.WithPoolKeysURL(api.URL))
	if err != nil {
		t.Fatal(err)
	}
	adapter.UseRaydiumQuoter(quoter)
	chains, err := chain.NewAdapters(map[network.Family]chain.Adapter{network.FamilySolana: adapter})
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
	derived := platformRequest(t, handler, http.MethodPost, "/api/spaces/"+spaceID+"/accounts/derive",
		map[string]string{"network_id": "solana-mainnet"})
	if derived.Code != http.StatusCreated {
		t.Fatalf("derive = %d %s", derived.Code, derived.Body)
	}
	account := decodeBody[struct {
		ID        string            `json:"id"`
		Addresses map[string]string `json:"addresses"`
	}](t, derived)
	owner := account.Addresses["solana"]
	n.mu.Lock()
	n.accounts[owner] = rawAccount{Owner: "11111111111111111111111111111111", Lamports: 2_000_000_000}
	n.accounts[ataOf(owner, usdc)] = tokenAccountOf(usdc, owner, 0)
	n.mu.Unlock()

	base := "/api/spaces/" + spaceID + "/networks/solana-mainnet/accounts/" + account.ID + "/solana/"
	request := map[string]any{
		"in_asset_id": "solana-mainnet:native", "out_asset_id": "solana-mainnet:spl:" + usdc, "amount": "0.01", "slippage_bps": 50,
	}
	quoted := platformRequest(t, handler, http.MethodPost, base+"swap-quote", request)
	if quoted.Code != http.StatusOK {
		t.Fatalf("quote = %d %s", quoted.Code, quoted.Body)
	}
	shown := decodeBody[map[string]any](t, quoted)
	if shown["amount_out"] != "1.043581" || shown["min_out"] != "1.038363" || shown["source"] != "raydium_api" ||
		shown["fee"] != "0.000005151" || shown["message_version"] != "legacy" {
		t.Fatalf("quote %s", quoted.Body)
	}

	swap := map[string]any{}
	for k, v := range request {
		swap[k] = v
	}
	for _, field := range []string{"min_out", "route", "compute_unit_limit", "compute_unit_price_micro_lamports", "token_accounts_created", "fee"} {
		swap[field] = shown[field]
	}
	send := func(key string) *httptest.ResponseRecorder {
		data, err := json.Marshal(swap)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, base+"swap", bytes.NewReader(data))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set(httpapi.TokenHeader, testToken)
		r.Host = testHost
		return serve(t, handler, r)
	}
	response := send("swap-1")
	if response.Code != http.StatusAccepted {
		t.Fatalf("swap = %d %s", response.Code, response.Body)
	}
	answer := decodeBody[map[string]string](t, response)
	n.mu.Lock()
	sent := append([][]byte(nil), n.sent...)
	n.mu.Unlock()
	if len(sent) != 1 {
		t.Fatalf("%d transactions sent", len(sent))
	}
	pub := solana.MustPublicKeyFromBase58(owner)
	if !ed25519.Verify(pub[:], sent[0][65:], sent[0][1:65]) {
		t.Fatal("the broadcast signature does not verify")
	}
	txID := solana.SignatureFromBytes(sent[0][1:65]).String()
	if answer["tx_id"] != txID || answer["status"] != operation.StatusPending {
		t.Fatalf("answer %v", answer)
	}
	stored, found, err := operation.New(fixture.settings.Home()).Get(spaceID, "swap-1")
	if err != nil || !found || stored.TxHash != txID || stored.LastValidBlockHeight != 4242 {
		t.Fatalf("operation %+v found %v err %v", stored, found, err)
	}

	// A replay answers with the same transaction and sends nothing.
	if replay := send("swap-1"); replay.Code != http.StatusOK || decodeBody[map[string]string](t, replay)["tx_id"] != txID {
		t.Fatalf("replay = %d %s", replay.Code, replay.Body)
	}
	// The same key with another minimum is another request.
	swap["min_out"] = "1.04"
	if conflict := send("swap-1"); conflict.Code != http.StatusConflict {
		t.Fatalf("changed replay = %d %s", conflict.Code, conflict.Body)
	}
	// A minimum the pool no longer pays is refused before signing.
	swap["min_out"] = "1.05"
	if refused := send("swap-2"); refused.Code != http.StatusConflict ||
		decodeBody[map[string]string](t, refused)["code"] != "not_broadcast" {
		t.Fatalf("stale minimum = %d %s", refused.Code, refused.Body)
	}
	n.mu.Lock()
	if len(n.sent) != 1 {
		t.Fatalf("%d transactions sent", len(n.sent))
	}
	n.mu.Unlock()

	// The status is read the way a transfer's is.
	status := platformRequest(t, handler, http.MethodGet, "/api/spaces/"+spaceID+"/networks/solana-mainnet/transactions/"+txID, nil)
	if status.Code != http.StatusOK || decodeBody[chain.Transaction](t, status).Status != operation.StatusPending {
		t.Fatalf("status = %d %s", status.Code, status.Body)
	}

	// Devnet does not offer swaps.
	devnet := "/api/spaces/" + spaceID + "/networks/solana-devnet/accounts/" + account.ID + "/solana/swap-quote"
	devnetRequest := map[string]any{
		"in_asset_id": "solana-devnet:native", "out_asset_id": "solana-devnet:spl:4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
		"amount": "0.01", "slippage_bps": 50,
	}
	if refused := platformRequest(t, handler, http.MethodPost, devnet, devnetRequest); refused.Code != http.StatusBadRequest {
		t.Fatalf("devnet quote = %d %s", refused.Code, refused.Body)
	}
	// Unknown fields are refused.
	request["extra"] = 1
	if refused := platformRequest(t, handler, http.MethodPost, base+"swap-quote", request); refused.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d %s", refused.Code, refused.Body)
	}
}

func TestSolanaSwapRefusals(t *testing.T) {
	t.Parallel()
	fixture := newPlatformFixture(t)
	created, err := fixture.spaces.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatal(err)
	}
	spaceID := created.Space.ID
	node := httptest.NewServer(http.HandlerFunc((&swapNode{accounts: map[string]rawAccount{}}).serve))
	t.Cleanup(node.Close)
	registry, err := network.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := solanachain.New(registry, fixedEndpoint{url: node.URL, client: node.Client()})
	if err != nil {
		t.Fatal(err)
	}
	platform := func(solanaAdapter chain.Adapter) http.Handler {
		chains, err := chain.NewAdapters(map[network.Family]chain.Adapter{network.FamilySolana: solanaAdapter})
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
		return handler
	}
	handler := platform(adapter)
	derived := platformRequest(t, handler, http.MethodPost, "/api/spaces/"+spaceID+"/accounts/derive",
		map[string]string{"network_id": "solana-mainnet"})
	if derived.Code != http.StatusCreated {
		t.Fatalf("derive = %d %s", derived.Code, derived.Body)
	}
	accountID := decodeBody[struct {
		ID string `json:"id"`
	}](t, derived).ID
	path := func(space, networkID, account, action string) string {
		return "/api/spaces/" + space + "/networks/" + networkID + "/accounts/" + account + "/solana/" + action
	}
	good := map[string]any{
		"in_asset_id": "solana-mainnet:native", "out_asset_id": "solana-mainnet:spl:" + usdc, "amount": "0.01", "slippage_bps": 50,
	}
	with := func(key string, value any) map[string]any {
		out := map[string]any{}
		for k, v := range good {
			out[k] = v
		}
		out[key] = value
		return out
	}
	tests := []struct {
		name    string
		handler http.Handler
		path    string
		body    any
		want    int
	}{
		{"an unknown network", handler, path(spaceID, "solana-nowhere", accountID, "swap-quote"), good, http.StatusNotFound},
		{"a Tron network", handler, path(spaceID, "tron-mainnet", accountID, "swap-quote"), good, http.StatusBadRequest},
		{"no Solana adapter", fixture.handler, path(spaceID, "solana-mainnet", accountID, "swap-quote"), good, http.StatusBadGateway},
		{"an adapter that does not swap", platform(&chaintest.Adapter{}), path(spaceID, "solana-mainnet", accountID, "swap-quote"), good, http.StatusNotImplemented},
		{"an unknown asset", handler, path(spaceID, "solana-mainnet", accountID, "swap-quote"), with("out_asset_id", "nope"), http.StatusBadRequest},
		{"an unknown space", handler, path("spc_nope", "solana-mainnet", accountID, "swap-quote"), good, http.StatusNotFound},
		{"an unknown account", handler, path(spaceID, "solana-mainnet", "acc_nope", "swap-quote"), good, http.StatusNotFound},
		{"a bad body", handler, path(spaceID, "solana-mainnet", accountID, "swap-quote"), with("extra", true), http.StatusBadRequest},
		{"a refused quote", handler, path(spaceID, "solana-mainnet", accountID, "swap-quote"), with("slippage_bps", 0), http.StatusBadRequest},
		{"a swap with a bad body", handler, path(spaceID, "solana-mainnet", accountID, "swap"), with("extra", true), http.StatusBadRequest},
		{"a swap on an unknown network", handler, path(spaceID, "solana-nowhere", accountID, "swap"), good, http.StatusNotFound},
	}
	fixture.spaces.SetSendConfirmation(false, 0)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := platformRequest(t, tt.handler, http.MethodPost, tt.path, tt.body)
			if response.Code != tt.want {
				t.Fatalf("status %d, want %d: %s", response.Code, tt.want, response.Body)
			}
		})
	}
	// A swap the user has not confirmed with the password is refused
	// before anything is read.
	fixture.spaces.SetSendConfirmation(true, 0)
	if response := platformRequest(t, handler, http.MethodPost, path(spaceID, "solana-mainnet", accountID, "swap"), good); response.Code != http.StatusForbidden {
		t.Fatalf("unconfirmed swap = %d %s", response.Code, response.Body)
	}
}
