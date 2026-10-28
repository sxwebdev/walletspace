package httpapi_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/asset"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/httpapi"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/operation"
	"github.com/sxwebdev/walletspace/internal/refuel"
	"github.com/sxwebdev/walletspace/internal/space"
)

type refuelFake struct{ requests atomic.Int64 }

func (f *refuelFake) Quote(_ context.Context, req refuel.Request) (refuel.Quote, error) {
	f.requests.Add(1)
	return refuel.Quote{Provider: "Gas.zip", To: refuel.DepositAddress, Data: "0x02" + strings.ToLower(req.Recipient[2:]) + "0021", Deposit: "7000000000000", Expected: req.Needed.String(), SourcePriceUSD: decimal.NewFromInt(2000), DepositUSD: decimal.RequireFromString("0.014"), ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (*refuelFake) Delivery(context.Context, string, int64, string) (refuel.Delivery, error) {
	return refuel.Delivery{Status: "pending"}, nil
}

func TestRefuelIsBoundToTransferAndCannotPayTwiceAfterRestart(t *testing.T) {
	t.Parallel()
	provider := &refuelFake{}
	fixture := newPlatformFixture(t, httpapi.WithRefuelProvider(provider))
	var broadcasts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     jsontext.Value `json:"id"`
			Method string         `json:"method"`
			Params jsontext.Value `json:"params"`
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if err := json.Unmarshal(data, &req); err != nil {
			t.Error(err)
			return
		}
		var result any
		var rpcErr any
		switch req.Method {
		case "eth_chainId":
			if r.URL.Path == "/kaia" {
				result = "0x2019"
			} else {
				result = "0x1"
			}
		case "eth_blockNumber":
			result = "0x100"
		case "eth_getBalance":
			if r.URL.Path == "/kaia" {
				result = "0x0"
			} else {
				result = "0xde0b6b3a7640000"
			}
		case "eth_estimateGas":
			if r.URL.Path == "/kaia" {
				result = "0xea60"
			} else {
				result = "0x5208"
			}
		case "eth_gasPrice":
			result = "0x3b9aca00"
		case "eth_getTransactionCount":
			result = "0x7"
		case "eth_getBlockByNumber", "eth_maxPriorityFeePerGas":
			rpcErr = map[string]any{"code": -32601, "message": "legacy node"}
		case "eth_sendRawTransaction":
			broadcasts.Add(1)
			result = "0xignored"
		case "eth_getTransactionReceipt":
			result = nil
		default:
			t.Errorf("unexpected RPC: %s", req.Method)
			rpcErr = map[string]any{"code": -32601, "message": "unexpected method"}
		}
		response := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
		if rpcErr != nil {
			response["error"] = rpcErr
			delete(response, "result")
		}
		body, err := json.Marshal(response)
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	t.Cleanup(server.Close)
	snapshot := fixture.settings.Snapshot()
	cfg := snapshot.Config
	cfg.NodeDiscovery.AllowInsecureRPC = true
	cfg.NodeDiscovery.Enabled = false
	if _, err := fixture.settings.SaveConfig(cfg, snapshot.Revision); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ id, path string }{{"ethereum-mainnet", "/eth"}, {"kaia-mainnet", "/kaia"}} {
		snapshot = fixture.settings.Snapshot()
		if _, err := fixture.settings.SaveNetwork(item.id, config.NetworkOverride{Endpoints: []config.Endpoint{{URL: server.URL + item.path}}}, snapshot.Revision); err != nil {
			t.Fatal(err)
		}
	}
	created, err := fixture.spaces.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, 2)
	for i, networkID := range []string{"ethereum-mainnet", "kaia-mainnet"} {
		response := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces/"+created.Space.ID+"/accounts/import", map[string]any{"private_key": fmt.Sprintf("%064x", i+1), "curve": "secp256k1", "label": networkID, "network_id": networkID})
		if response.Code != http.StatusCreated {
			t.Fatalf("import = %d %s", response.Code, response.Body)
		}
		payload := decodeBody[struct {
			Account struct {
				ID string `json:"id"`
			} `json:"account"`
		}](t, response)
		ids = append(ids, payload.Account.ID)
	}
	contract := "0x3333333333333333333333333333333333333333"
	assetID := asset.ID("kaia-mainnet", "erc20", contract)
	if err := fixture.assets.Add(chain.Asset{ID: assetID, NetworkID: "kaia-mainnet", Kind: "erc20", Contract: contract, Decimals: 6, Name: "Test USDT", Symbol: "USDT"}); err != nil {
		t.Fatal(err)
	}
	base := "/api/spaces/" + created.Space.ID + "/networks/kaia-mainnet"
	transfer := map[string]any{"account_id": ids[1], "asset_id": assetID, "to": "0x4444444444444444444444444444444444444444", "amount": "10"}
	response := platformRequest(t, fixture.handler, http.MethodPost, base+"/gas-refuel/quotes", transfer)
	if response.Code != http.StatusOK {
		t.Fatalf("quote = %d %s", response.Code, response.Body)
	}
	quoted := decodeBody[struct {
		Quotes []refuel.Plan `json:"quotes"`
	}](t, response)
	if len(quoted.Quotes) != 1 {
		t.Fatalf("quotes = %s", response.Body)
	}
	plan := quoted.Quotes[0]
	if plan.SourceAccountID != ids[0] || plan.Target.AccountID != ids[1] || plan.Target.To != transfer["to"] || plan.Recipient == plan.Target.To || plan.Required != "0.000066" || plan.TotalUSD == "" {
		t.Fatalf("plan = %+v", plan)
	}
	if provider.requests.Load() != 1 || broadcasts.Load() != 0 {
		t.Fatal("quoting moved funds or repeated provider requests")
	}
	path := base + "/gas-refuel/" + plan.ID
	blocked := platformRequest(t, fixture.handler, http.MethodPost, path, map[string]any{})
	if blocked.Code != http.StatusUnauthorized && blocked.Code != http.StatusForbidden {
		t.Fatalf("missing spend confirmation = %d %s", blocked.Code, blocked.Body)
	}
	if _, err := fixture.spaces.ConfirmSend(t.Context(), created.Space.ID, "test-vault-password"); err != nil {
		t.Fatal(err)
	}
	sent := platformRequest(t, fixture.handler, http.MethodPost, path, map[string]any{})
	if sent.Code != http.StatusAccepted {
		t.Fatalf("top up = %d %s", sent.Code, sent.Body)
	}
	first := decodeBody[operation.Operation](t, sent)
	if first.TxHash == "" || broadcasts.Load() != 1 {
		t.Fatalf("operation = %+v, broadcasts %d", first, broadcasts.Load())
	}
	registry, err := network.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := httpapi.NewPlatform(fixture.spaces, fixture.settings, registry, operation.New(fixture.settings.Home()), fixture.assets, fixture.chains, fixture.evm, fixture.tron, fixture.doctor, fixture.prices, testAccess(), slog.New(slog.NewTextHandler(io.Discard, nil)), httpapi.WithRefuelProvider(provider))
	if err != nil {
		t.Fatal(err)
	}
	repeated := platformRequest(t, restarted, http.MethodPost, path, map[string]any{})
	if repeated.Code != http.StatusOK || broadcasts.Load() != 1 {
		t.Fatalf("replay = %d %s, broadcasts %d", repeated.Code, repeated.Body, broadcasts.Load())
	}
	second := decodeBody[operation.Operation](t, repeated)
	if first.TxHash != second.TxHash {
		t.Fatal("replay replaced the signed transaction")
	}
	another := platformRequest(t, restarted, http.MethodPost, base+"/gas-refuel/quotes", transfer)
	if another.Code != http.StatusConflict || !strings.Contains(another.Body.String(), plan.ID) {
		t.Fatalf("second funding quote allowed while pending: %d %s", another.Code, another.Body)
	}
	pending := platformRequest(t, restarted, http.MethodGet, path, nil)
	if pending.Code != http.StatusOK || !strings.Contains(pending.Body.String(), `"status":"pending"`) {
		t.Fatalf("pending = %d %s", pending.Code, pending.Body)
	}
}
