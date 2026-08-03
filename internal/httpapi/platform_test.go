package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
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

type platformFixture struct {
	handler http.Handler
	spaces  *space.Manager
	evm     *evmchain.Adapter
	tron    *tronchain.Adapter
	doctor  *doctor.Doctor
	prices  *priceFake
}

// The address the test guard accepts, and the token it demands. Requests built
// by platformRequest carry both; anything testing the boundary itself varies
// them deliberately.
const (
	testHost  = "127.0.0.1:8080"
	testToken = "test-capability-token"
)

func testAccess() httpapi.Access {
	return httpapi.Access{Token: testToken, Hosts: []string{testHost}}
}

type priceFake struct {
	mu        sync.Mutex
	requested []string
	calls     int
}

func (f *priceFake) Quotes(_ context.Context, identifiers []string) (price.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.requested = append([]string(nil), identifiers...)
	quotes := make(map[string]price.Quote, len(identifiers))
	for _, identifier := range identifiers {
		quotes[identifier] = price.Quote{
			Current: decimal.NewFromInt(2), Previous: decimal.NewFromInt(1),
			HasPrevious: true, Timestamp: time.Unix(100, 0).UTC(),
		}
	}
	return price.Snapshot{Quotes: quotes}, nil
}

func (f *priceFake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *priceFake) Requested() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requested...)
}

func newPlatformFixture(t *testing.T) platformFixture {
	t.Helper()
	home := t.TempDir()
	settings, err := config.NewHomeManager(home)
	if err != nil {
		t.Fatalf("config.NewHomeManager() error = %v", err)
	}
	spaces, err := space.NewManager(home, 15*time.Minute, vault.Params{
		Time: 2, MemoryKiB: 32 * 1024, Parallelism: 1,
	})
	if err != nil {
		t.Fatalf("space.NewManager() error = %v", err)
	}
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	evm, err := evmchain.New(registry, rpcpool.New(settings))
	if err != nil {
		t.Fatalf("evm.New() error = %v", err)
	}
	tron, err := tronchain.New(
		t.Context(), registry, settings, rpcpool.New(settings),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("tron.New() error = %v", err)
	}
	nodeDoctor, err := doctor.New(
		t.Context(), registry, rpcpool.New(settings),
		func(context.Context, network.Network, string) error { return nil },
		doctor.Options{Interval: time.Hour},
	)
	if err != nil {
		t.Fatalf("doctor.New() error = %v", err)
	}
	prices := &priceFake{}
	handler, err := httpapi.NewPlatform(
		spaces, settings, registry, operation.New(home), mustAssetStore(t, home), evm, tron, nodeDoctor,
		prices, testAccess(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("httpapi.NewPlatform() error = %v", err)
	}
	t.Cleanup(func() {
		nodeDoctor.Close()
		tron.Close()
		evm.Close()
		spaces.Close()
	})
	return platformFixture{
		handler: handler, spaces: spaces, evm: evm, tron: tron, doctor: nodeDoctor, prices: prices,
	}
}

func mustAssetStore(t *testing.T, home string) *asset.Store {
	t.Helper()
	store, err := asset.New(home)
	if err != nil {
		t.Fatalf("asset.New() error = %v", err)
	}
	return store
}

func platformRequest(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		reader = bytes.NewReader(data)
	}
	request := httptest.NewRequestWithContext(t.Context(), method, path, reader)
	if method != http.MethodGet && method != http.MethodHead {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Host = testHost
	request.Header.Set(httpapi.TokenHeader, testToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeBody[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
	return value
}

func TestPlatformFirstRunAndSecretHeaders(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	empty := platformRequest(t, fixture.handler, http.MethodGet, "/api/spaces", nil)
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"spaces":[]`) {
		t.Fatalf("GET /api/spaces = %d %s", empty.Code, empty.Body.String())
	}
	created := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces", map[string]any{
		"name": "", "mnemonic": "", "password": "test-vault-password",
		"network_id": "tron-mainnet",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /api/spaces = %d %s", created.Code, created.Body.String())
	}
	if got := created.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	var payload struct {
		Space struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Locked bool   `json:"locked"`
		} `json:"space"`
		MnemonicGenerated bool   `json:"mnemonic_generated"`
		Mnemonic          string `json:"mnemonic"`
		Accounts          []struct {
			NetworkIDs []string          `json:"network_ids"`
			Addresses  map[string]string `json:"addresses"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if payload.Space.Name != "default" || payload.Space.Locked ||
		!payload.MnemonicGenerated || payload.Mnemonic == "" {
		t.Errorf("created payload = %+v", payload)
	}
	if len(payload.Accounts) != 0 {
		t.Errorf("space creation unexpectedly created accounts: %+v", payload.Accounts)
	}
}

func TestPlatformImportBadgeAndLockedSecrets(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	created, err := fixture.spaces.Create(space.CreateRequest{
		Password: "test-vault-password", ImportedOnly: true,
	})
	if err != nil {
		t.Fatalf("spaces.Create() error = %v", err)
	}
	imported := platformRequest(t, fixture.handler, http.MethodPost,
		"/api/spaces/"+created.Space.ID+"/accounts/import", map[string]any{
			"curve":       "secp256k1",
			"private_key": "0000000000000000000000000000000000000000000000000000000000000001",
			"label":       "Treasury",
			"network_id":  "ethereum-mainnet",
		})
	if imported.Code != http.StatusCreated || !strings.Contains(imported.Body.String(), `"kind":"imported"`) {
		t.Fatalf("import response = %d %s", imported.Code, imported.Body.String())
	}
	var payload struct {
		Account struct {
			ID string `json:"id"`
		} `json:"account"`
	}
	if err := json.Unmarshal(imported.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode import: %v", err)
	}
	if err := fixture.spaces.Lock(created.Space.ID); err != nil {
		t.Fatalf("spaces.Lock() error = %v", err)
	}
	path := "/api/spaces/" + created.Space.ID + "/accounts/" + payload.Account.ID + "/private-key"

	// No password: refused before the lock state is even consulted.
	exported := platformRequest(t, fixture.handler, http.MethodPost, path,
		map[string]string{"family": "evm"})
	if exported.Code != http.StatusUnauthorized {
		t.Fatalf("export without a password = %d %s", exported.Code, exported.Body.String())
	}

	// Wrong password: the same answer, so neither confirms the other.
	wrong := platformRequest(t, fixture.handler, http.MethodPost, path,
		map[string]string{"family": "evm", "password": "not-the-password"})
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("export with a wrong password = %d %s", wrong.Code, wrong.Body.String())
	}

	// Right password, but the space is locked: the step-up does not substitute
	// for unlocking it.
	locked := platformRequest(t, fixture.handler, http.MethodPost, path,
		map[string]string{"family": "evm", "password": "test-vault-password"})
	if locked.Code != http.StatusLocked {
		t.Fatalf("locked export = %d %s", locked.Code, locked.Body.String())
	}
	if strings.Contains(locked.Body.String(), "private_key") {
		t.Error("a locked export returned key material")
	}
}

// An unlocked space is not evidence that the person asking is the owner: a tab
// left open, a same-origin script or any local client holding the capability
// token all inherit that state. The password is the one thing none of them has.
func TestPlatformSecretExportNeedsTheSpacePassword(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	created := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces", map[string]any{
		"password": "correct horse battery", "network_id": "tron-nile", "first": true,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /api/spaces = %d %s", created.Code, created.Body.String())
	}
	var space struct {
		Space struct {
			ID string `json:"id"`
		} `json:"space"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &space); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	path := "/api/spaces/" + space.Space.ID + "/mnemonic"

	for _, body := range []map[string]string{
		{},
		{"password": ""},
		{"password": "correct horse batter"},
	} {
		response := platformRequest(t, fixture.handler, http.MethodPost, path, body)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("mnemonic with %v = %d %s", body, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), "abandon") {
			t.Error("a refused request returned the recovery phrase")
		}
	}

	revealed := platformRequest(t, fixture.handler, http.MethodPost, path,
		map[string]string{"password": "correct horse battery"})
	if revealed.Code != http.StatusOK {
		t.Fatalf("mnemonic with the right password = %d %s", revealed.Code, revealed.Body.String())
	}
	if !strings.Contains(revealed.Body.String(), "mnemonic") {
		t.Errorf("mnemonic response = %s", revealed.Body.String())
	}
}

func TestPlatformDerivesPerNetworkAndReusesCompatibleWallet(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	created := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces", map[string]any{
		"password": "test-vault-password", "network_id": "tron-nile",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /api/spaces = %d %s", created.Code, created.Body.String())
	}
	var spacePayload struct {
		Space struct {
			ID string `json:"id"`
		} `json:"space"`
		Accounts []struct {
			ID    string  `json:"id"`
			Index *uint32 `json:"index"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &spacePayload); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	spaceID := spacePayload.Space.ID
	if len(spacePayload.Accounts) != 0 {
		t.Fatalf("space creation unexpectedly created accounts: %+v", spacePayload.Accounts)
	}
	type accountPayload struct {
		ID         string   `json:"id"`
		Index      *uint32  `json:"index"`
		NetworkIDs []string `json:"network_ids"`
	}

	nileZero := platformRequest(
		t, fixture.handler, http.MethodPost,
		"/api/spaces/"+spaceID+"/accounts/derive",
		map[string]string{"network_id": "tron-nile", "label": "Nile 0"},
	)
	if nileZero.Code != http.StatusCreated {
		t.Fatalf("derive Nile 0 = %d %s", nileZero.Code, nileZero.Body.String())
	}
	nileZeroAccount := decodeBody[accountPayload](t, nileZero)
	if nileZeroAccount.Index == nil || *nileZeroAccount.Index != 0 {
		t.Fatalf("first Nile index = %v, want 0", nileZeroAccount.Index)
	}

	nileOne := platformRequest(
		t, fixture.handler, http.MethodPost,
		"/api/spaces/"+spaceID+"/accounts/derive",
		map[string]string{"network_id": "tron-nile", "label": "Nile 1"},
	)
	if nileOne.Code != http.StatusCreated {
		t.Fatalf("derive Nile = %d %s", nileOne.Code, nileOne.Body.String())
	}
	nileAccount := decodeBody[accountPayload](t, nileOne)
	if nileAccount.Index == nil || *nileAccount.Index != 1 {
		t.Fatalf("Nile index = %v, want 1", nileAccount.Index)
	}

	ethereum := platformRequest(
		t, fixture.handler, http.MethodPost,
		"/api/spaces/"+spaceID+"/accounts/derive",
		map[string]string{"network_id": "ethereum-mainnet", "label": "EVM 0"},
	)
	bsc := platformRequest(
		t, fixture.handler, http.MethodPost,
		"/api/spaces/"+spaceID+"/accounts/derive",
		map[string]string{"network_id": "bsc-mainnet", "label": "BSC 0"},
	)
	if ethereum.Code != http.StatusCreated || bsc.Code != http.StatusCreated {
		t.Fatalf("derive EVM = %d/%d: %s %s", ethereum.Code, bsc.Code, ethereum.Body, bsc.Body)
	}
	ethereumAccount := decodeBody[accountPayload](t, ethereum)
	bscAccount := decodeBody[accountPayload](t, bsc)
	if ethereumAccount.Index == nil || *ethereumAccount.Index != 0 ||
		ethereumAccount.ID != bscAccount.ID ||
		len(bscAccount.NetworkIDs) != 2 {
		t.Fatalf("compatible EVM wallets were not reused: ETH=%+v BSC=%+v", ethereumAccount, bscAccount)
	}
}

func TestPlatformSettingsUseETagAndServeClientRoute(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	response := platformRequest(t, fixture.handler, http.MethodGet, "/api/settings", nil)
	if response.Code != http.StatusOK || response.Header().Get("ETag") == "" {
		t.Fatalf("GET /api/settings = %d, ETag %q", response.Code, response.Header().Get("ETag"))
	}
	if strings.Contains(response.Body.String(), "TRON-PRO-API-KEY") {
		t.Error("settings response leaked provider header")
	}
	var initial struct {
		NodeDiscovery struct {
			Enabled bool   `json:"enabled"`
			URL     string `json:"url"`
		} `json:"node_discovery"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &initial); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if initial.NodeDiscovery.Enabled || initial.NodeDiscovery.URL != "" {
		t.Fatalf("default discovery settings = %+v", initial.NodeDiscovery)
	}
	updated := platformRequest(t, fixture.handler, http.MethodPatch,
		"/api/settings/node-discovery", map[string]any{
			"enabled": true, "url": "  https://discovery.example  ",
			"refresh_interval": "30m", "request_timeout": "5s",
			"allow_insecure_rpc": false,
		})
	if updated.Code != http.StatusOK ||
		!strings.Contains(updated.Body.String(), `"url":"https://discovery.example"`) {
		t.Fatalf("PATCH discovery = %d %s", updated.Code, updated.Body.String())
	}
	page := platformRequest(t, fixture.handler, http.MethodGet, "/settings", nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `id="app"`) {
		t.Fatalf("GET /settings = %d %s", page.Code, page.Body.String())
	}
}

func TestPlatformPricesOnlyRequestsMainnetAssets(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	empty := platformRequest(t, fixture.handler, http.MethodGet, "/api/prices", nil)
	if empty.Code != http.StatusOK || fixture.prices.Calls() != 0 ||
		!strings.Contains(empty.Body.String(), `"quotes":[]`) {
		t.Fatalf("empty GET /api/prices = %d %s, provider calls = %d",
			empty.Code, empty.Body.String(), fixture.prices.Calls())
	}
	if got := empty.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("GET /api/prices Cache-Control = %q, want no-store", got)
	}
	response := platformRequest(t, fixture.handler, http.MethodGet,
		"/api/prices?asset_id=tron-mainnet:native&asset_id=ethereum-mainnet:native"+
			"&asset_id=bsc-mainnet:native&asset_id=tron-nile:native", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/prices = %d %s", response.Code, response.Body.String())
	}
	requested := fixture.prices.Requested()
	for _, want := range []string{"coingecko:tron", "coingecko:ethereum", "coingecko:binancecoin"} {
		if !slices.Contains(requested, want) {
			t.Errorf("price identifiers do not contain %q: %v", want, requested)
		}
	}
	for _, identifier := range requested {
		if strings.Contains(identifier, "nile") || strings.Contains(identifier, "sepolia") {
			t.Errorf("testnet identifier requested: %q", identifier)
		}
	}
	var payload struct {
		Quotes []struct {
			AssetID    string          `json:"asset_id"`
			CurrentUSD decimal.Decimal `json:"current_usd"`
		} `json:"quotes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode prices: %v", err)
	}
	found := false
	for _, quote := range payload.Quotes {
		if quote.AssetID == "tron-mainnet:native" {
			found = quote.CurrentUSD.Equal(decimal.NewFromInt(2))
		}
		if strings.Contains(quote.AssetID, "testnet") || strings.Contains(quote.AssetID, "tron-nile") {
			t.Errorf("testnet quote returned: %+v", quote)
		}
	}
	if !found {
		t.Fatalf("Tron native quote missing: %+v", payload.Quotes)
	}
}

func TestPlatformRejectsTrailingJSON(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	request := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/api/spaces",
		strings.NewReader(`{"password":"test-vault-password"} {"password":"second"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Host = testHost
	request.Header.Set(httpapi.TokenHeader, testToken)
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/spaces status = %d, want 400; body = %s", response.Code, response.Body)
	}
	if got := len(fixture.spaces.List()); got != 0 {
		t.Fatalf("spaces = %d, want no mutation", got)
	}
}

func TestPlatformRejectsShortPasswordAsClientError(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	response := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces", map[string]any{
		"password": "short",
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/spaces status = %d, want 400; body = %s", response.Code, response.Body)
	}
}
