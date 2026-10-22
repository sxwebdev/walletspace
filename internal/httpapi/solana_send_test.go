package httpapi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain"
	solanachain "github.com/sxwebdev/walletspace/internal/chain/solana"
	"github.com/sxwebdev/walletspace/internal/httpapi"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/operation"
	"github.com/sxwebdev/walletspace/internal/space"
)

type fixedEndpoint struct {
	url    string
	client *http.Client
}

func (e fixedEndpoint) Endpoints(context.Context, network.Network) ([]string, error) {
	return []string{e.url}, nil
}

func (e fixedEndpoint) HTTPClient(network.Network) *http.Client { return e.client }

// The transaction id — the signature — and the expiry height are on disk
// before the transaction reaches a node. When the node's answer is then lost,
// the operation is broadcast_unknown with that id, never failed.
func TestASolanaTransactionIsRecordedBeforeItIsBroadcast(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	fixture.spaces.SetSendConfirmation(false, 0)
	created, err := fixture.spaces.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatal(err)
	}
	spaceID := created.Space.ID
	var from string
	var atBroadcast operation.Operation
	var broadcast, snapshot []byte
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			ID     any               `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&call)
		context1 := func(value any) any { return map[string]any{"context": map[string]any{"slot": 1}, "value": value} }
		var result any
		switch call.Method {
		case "getGenesisHash":
			result = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
		case "getMultipleAccounts":
			var keys []string
			_ = json.Unmarshal(call.Params[0], &keys)
			value := make([]any, len(keys))
			for i, key := range keys {
				if key == from {
					value[i] = map[string]any{"lamports": 2_000_000_000, "owner": "11111111111111111111111111111111", "data": []string{"", "base64"}}
				}
			}
			result = context1(value)
		case "getMinimumBalanceForRentExemption":
			result = 650_240
		case "getRecentPrioritizationFees":
			result = []any{}
		case "simulateTransaction":
			result = context1(map[string]any{"err": nil, "unitsConsumed": 450})
		case "getLatestBlockhash":
			result = context1(map[string]any{"blockhash": "9zrUHnA1nCByPksy3aL8tQ47vqdaG2vnFs4HrxgcZj4F", "lastValidBlockHeight": 4242})
		case "getSignatureStatuses":
			result = context1([]any{nil})
		case "getBlockHeight":
			result = 4242 + 1000
		case "sendTransaction":
			var encoded string
			_ = json.Unmarshal(call.Params[0], &encoded)
			broadcast, _ = base64.StdEncoding.DecodeString(encoded)
			atBroadcast, _, _ = operation.New(fixture.settings.Home()).Get(spaceID, "send-1")
			// What a process that died at this instant would leave on disk.
			snapshot, _ = os.ReadFile(filepath.Join(fixture.settings.Home(), "spaces", spaceID, "operations.json"))
			// The answer is lost.
			http.Error(w, "upstream timeout", http.StatusGatewayTimeout)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
	t.Cleanup(node.Close)
	registry, err := network.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := solanachain.New(registry, fixedEndpoint{url: node.URL, client: node.Client()})
	if err != nil {
		t.Fatal(err)
	}
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
	from = account.Addresses["solana"]

	base := "/api/spaces/" + spaceID + "/networks/solana-mainnet/transfers"
	transfer := map[string]any{
		"account_id": account.ID, "asset_id": "solana-mainnet:native",
		"to": "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM", "amount": "0.5",
	}
	estimated := platformRequest(t, handler, http.MethodPost, base+"/estimate", transfer)
	if estimated.Code != http.StatusOK {
		t.Fatalf("estimate = %d %s", estimated.Code, estimated.Body)
	}
	shown := decodeBody[map[string]any](t, estimated)
	for _, field := range []string{"fee", "fee_model", "compute_unit_limit", "compute_unit_price_micro_lamports"} {
		if value, ok := shown[field]; ok {
			transfer[field] = value
		}
	}
	data, err := json.Marshal(transfer)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, base, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "send-1")
	request.Header.Set(httpapi.TokenHeader, testToken)
	request.Host = testHost
	response := serve(t, handler, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("send = %d %s", response.Code, response.Body)
	}
	if len(broadcast) < 65 {
		t.Fatalf("nothing was broadcast")
	}
	txID := solana.SignatureFromBytes(broadcast[1:65]).String()
	if atBroadcast.TxHash != txID || atBroadcast.Status != operation.StatusBroadcasting || atBroadcast.LastValidBlockHeight != 4242 {
		t.Errorf("record at broadcast = %+v, want %s broadcasting with expiry 4242", atBroadcast, txID)
	}
	sent := decodeBody[operation.Operation](t, response)
	if sent.TxHash != txID || sent.Status != operation.StatusBroadcastUnknown {
		t.Errorf("operation = %+v, want %s broadcast_unknown", sent, txID)
	}

	// The process died the moment the transaction left: the record on disk
	// is the one written before the broadcast. A new process knows the
	// transaction only from it, and once the chain is past the expiry it
	// says so: failed, and a replay of the request is told to try again
	// with a new key.
	if err := os.WriteFile(filepath.Join(fixture.settings.Home(), "spaces", spaceID, "operations.json"), snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	restartedAdapter, err := solanachain.New(registry, fixedEndpoint{url: node.URL, client: node.Client()})
	if err != nil {
		t.Fatal(err)
	}
	restartedChains, err := chain.NewAdapters(map[network.Family]chain.Adapter{network.FamilySolana: restartedAdapter})
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := httpapi.NewPlatform(
		fixture.spaces, fixture.settings, registry, operation.New(fixture.settings.Home()),
		fixture.assets, restartedChains, fixture.evm, fixture.tron, fixture.doctor, fixture.prices,
		testAccess(), slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	status := platformRequest(t, restarted, http.MethodGet,
		"/api/spaces/"+spaceID+"/networks/solana-mainnet/transactions/"+txID, nil)
	if status.Code != http.StatusOK || decodeBody[chain.Transaction](t, status).Status != operation.StatusFailed {
		t.Fatalf("status after restart = %d %s, want failed", status.Code, status.Body)
	}
	replay := httptest.NewRequestWithContext(t.Context(), http.MethodPost, base, bytes.NewReader(data))
	replay.Header.Set("Content-Type", "application/json")
	replay.Header.Set("Idempotency-Key", "send-1")
	replay.Header.Set(httpapi.TokenHeader, testToken)
	replay.Host = testHost
	if answer := serve(t, restarted, replay); answer.Code != http.StatusConflict ||
		!strings.Contains(answer.Body.String(), "new idempotency key") {
		t.Errorf("replay after expiry = %d %s, want 409 asking for a new key", answer.Code, answer.Body)
	}
}
