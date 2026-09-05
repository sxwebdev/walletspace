package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/asset"
	"github.com/sxwebdev/walletspace/internal/chain"
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
	// settings is the same home manager the handler writes through, so a test
	// can put a value in config.yaml the way a person editing the file would.
	settings *config.HomeManager
	evm      *evmchain.Adapter
	tron     *tronchain.Adapter
	doctor   *doctor.Doctor
	prices   *priceFake
	assets   *asset.Store
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
	assets := mustAssetStore(t, home)
	handler, err := httpapi.NewPlatform(
		spaces, settings, registry, operation.New(home), assets, evm, tron, nodeDoctor,
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
		handler: handler, spaces: spaces, settings: settings,
		evm: evm, tron: tron, doctor: nodeDoctor, prices: prices, assets: assets,
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

func TestPlatformPricesRobinhoodWETH(t *testing.T) {
	t.Parallel()

	const wethContract = "0x0Bd7D308f8E1639FAb988df18A8011f41EAcAD73"
	for _, tt := range []struct {
		name           string
		networkID      string
		contract       string
		includeNative  bool
		wantIdentifier string
	}{
		{
			name: "built-in WETH without native ETH", networkID: "robinhood-mainnet",
			wantIdentifier: "coingecko:ethereum",
		},
		{
			name: "WETH and native ETH share a quote", networkID: "robinhood-mainnet",
			includeNative: true, wantIdentifier: "coingecko:ethereum",
		},
		{
			name: "configured lowercase contract", networkID: "robinhood-mainnet",
			contract: strings.ToLower(wethContract), wantIdentifier: "coingecko:ethereum",
		},
		{
			name: "configured uppercase contract", networkID: "robinhood-mainnet",
			contract: strings.ToUpper(wethContract), wantIdentifier: "coingecko:ethereum",
		},
		{
			name: "another token named WETH", networkID: "robinhood-mainnet",
			contract: "0x1111111111111111111111111111111111111111",
		},
		{
			name: "same contract on another network", networkID: "ethereum-mainnet",
			contract: wethContract, wantIdentifier: "ethereum:" + strings.ToLower(wethContract),
		},
		{
			name: "testnet excluded", networkID: "robinhood-testnet", contract: wethContract,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newPlatformFixture(t)
			assetID := asset.ID(tt.networkID, "erc20", wethContract)
			if tt.contract != "" {
				assetID = asset.ID(tt.networkID, "erc20", tt.contract)
				if err := fixture.assets.Add(chain.Asset{
					ID: assetID, NetworkID: tt.networkID, Kind: "erc20",
					Name: "Wrapped Ether", Symbol: "WETH", Decimals: 18, Contract: tt.contract,
				}); err != nil {
					t.Fatalf("add configured token: %v", err)
				}
			}
			assetIDs := []string{assetID}
			if tt.includeNative {
				assetIDs = append(assetIDs, "robinhood-mainnet:native", "ethereum-mainnet:native")
			}
			query := url.Values{"asset_id": assetIDs}
			response := platformRequest(t, fixture.handler, http.MethodGet, "/api/prices?"+query.Encode(), nil)
			if response.Code != http.StatusOK {
				t.Fatalf("GET /api/prices = %d %s", response.Code, response.Body.String())
			}
			payload := decodeBody[struct {
				Quotes []struct {
					AssetID string `json:"asset_id"`
					price.Quote
				} `json:"quotes"`
			}](t, response)
			if tt.wantIdentifier == "" {
				if len(payload.Quotes) != 0 || fixture.prices.Calls() != 0 {
					t.Fatalf("unpriced asset: quotes = %+v, provider calls = %d", payload.Quotes, fixture.prices.Calls())
				}
				return
			}
			if got := fixture.prices.Requested(); !slices.Equal(got, []string{tt.wantIdentifier}) {
				t.Errorf("requested price identifiers = %v, want only %q", got, tt.wantIdentifier)
			}
			if len(payload.Quotes) != len(assetIDs) {
				t.Fatalf("quotes = %+v, want one for each of %v", payload.Quotes, assetIDs)
			}
			slices.Sort(assetIDs)
			for i, quote := range payload.Quotes {
				if quote.AssetID != assetIDs[i] {
					t.Errorf("quote asset ID = %q, want %q", quote.AssetID, assetIDs[i])
				}
				if !quote.Current.Equal(decimal.NewFromInt(2)) ||
					!quote.Previous.Equal(decimal.NewFromInt(1)) || !quote.HasPrevious ||
					!quote.Timestamp.Equal(time.Unix(100, 0)) {
					t.Errorf("quote for %s = %+v, want current and 24h prices with their timestamp", quote.AssetID, quote.Quote)
				}
			}
		})
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

// securityPayload is the part of the settings response this file asserts on.
type securityPayload struct {
	Security struct {
		AutoLock     string `json:"auto_lock"`
		ConfirmSends bool   `json:"confirm_sends"`
		SendGrantTTL string `json:"send_grant_ttl"`
	} `json:"security"`
	RestartRequired []string `json:"restart_required"`
}

// spendingFixture is an unlocked space and the transfer that will be refused
// until it is confirmed. Nothing on the route past the gate is reachable — the
// space has no wallets — because nothing past the gate should be needed to
// refuse.
func spendingFixture(t *testing.T) (fixture platformFixture, spaceID, transfers string, transfer map[string]any) {
	t.Helper()
	fixture = newPlatformFixture(t)
	created, err := fixture.spaces.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatalf("spaces.Create() error = %v", err)
	}
	return fixture, created.Space.ID,
		"/api/spaces/" + created.Space.ID + "/networks/tron-nile/transfers",
		map[string]any{
			"account_id": "acc_none", "asset_id": "tron-nile:native",
			"to": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "amount": "1",
		}
}

// The spending step-up exists to stop anything that merely holds the capability
// token — a script injected into the page, another local process — from moving
// funds out of an unlocked space. Switching it off needed nothing but that same
// token, so refused transfer, PATCH, identical transfer accepted was the entire
// attack and the control was worth nothing against the caller it was built for.
func TestTheSpendingStepUpCannotBeSwitchedOffThroughTheAPI(t *testing.T) {
	t.Parallel()

	fixture, _, transfers, transfer := spendingFixture(t)
	// The gate is the first thing on the route, so this refusal owes nothing to
	// the account, the asset or a node being reachable.
	refused := platformRequest(t, fixture.handler, http.MethodPost, transfers, transfer)
	if refused.Code != http.StatusForbidden {
		t.Fatalf("transfer before confirming = %d %s", refused.Code, refused.Body.String())
	}

	before := decodeBody[securityPayload](
		t, platformRequest(t, fixture.handler, http.MethodGet, "/api/settings", nil),
	)
	if !before.Security.ConfirmSends {
		t.Fatal("the step-up is off before the test starts")
	}
	if !slices.Contains(before.RestartRequired, "security.confirm_sends") {
		t.Errorf("restart_required = %v, want it to name security.confirm_sends", before.RestartRequired)
	}

	off := platformRequest(t, fixture.handler, http.MethodPatch, "/api/settings/security", map[string]any{
		"auto_lock": "15m", "confirm_sends": false, "send_grant_ttl": "5m",
	})
	if off.Code != http.StatusForbidden {
		t.Fatalf("PATCH confirm_sends=false = %d %s", off.Code, off.Body.String())
	}
	// Refused with directions, not ignored: a save that reports success and
	// changes nothing teaches the user that the checkbox does not work.
	message := decodeBody[map[string]string](t, off)["error"]
	if !strings.Contains(message, "config.yaml") || !strings.Contains(strings.ToLower(message), "restart") {
		t.Errorf("refusal = %q, want it to name config.yaml and the restart", message)
	}
	if stored := fixture.settings.Snapshot().Config.Security.ConfirmSends; !stored {
		t.Error("the refused PATCH still reached the file")
	}

	// The identical transfer that was refused a moment ago is refused still.
	again := platformRequest(t, fixture.handler, http.MethodPost, transfers, transfer)
	if again.Code != http.StatusForbidden {
		t.Fatalf("transfer after the PATCH = %d %s", again.Code, again.Body.String())
	}

	// The form posts the whole block, so an echo of the stored value is accepted
	// and the two fields beside it still save.
	echo := platformRequest(t, fixture.handler, http.MethodPatch, "/api/settings/security", map[string]any{
		"auto_lock": "20m", "confirm_sends": true, "send_grant_ttl": "10m",
	})
	if echo.Code != http.StatusOK {
		t.Fatalf("PATCH echoing confirm_sends = %d %s", echo.Code, echo.Body.String())
	}
	saved := decodeBody[securityPayload](t, echo)
	if saved.Security.AutoLock != "20m0s" || saved.Security.SendGrantTTL != "10m0s" ||
		!saved.Security.ConfirmSends {
		t.Errorf("saved security settings = %+v", saved.Security)
	}
	// A client that sends only what it changed is accepted too, and switches
	// nothing off by leaving the field out.
	partial := platformRequest(t, fixture.handler, http.MethodPatch, "/api/settings/security", map[string]any{
		"auto_lock": "25m",
	})
	if partial.Code != http.StatusOK || !decodeBody[securityPayload](t, partial).Security.ConfirmSends {
		t.Fatalf("PATCH without confirm_sends = %d %s", partial.Code, partial.Body.String())
	}

	// Turning it back on is refused as well, because the file is the authority
	// in both directions. config.yaml is edited here the way a person would edit
	// it, and the running process keeps the value it started with — which is
	// what restart-only means.
	stored := fixture.settings.Snapshot().Config
	stored.Security.ConfirmSends = false
	if _, err := fixture.settings.SaveConfig(stored, ""); err != nil {
		t.Fatalf("SaveConfig(confirm_sends off) error = %v", err)
	}
	on := platformRequest(t, fixture.handler, http.MethodPatch, "/api/settings/security", map[string]any{
		"auto_lock": "15m", "confirm_sends": true, "send_grant_ttl": "5m",
	})
	if on.Code != http.StatusForbidden {
		t.Fatalf("PATCH confirm_sends=true = %d %s", on.Code, on.Body.String())
	}
	final := platformRequest(t, fixture.handler, http.MethodPost, transfers, transfer)
	if final.Code != http.StatusForbidden {
		t.Fatalf("transfer after the file changed = %d %s", final.Code, final.Body.String())
	}
}

// The gate's refusal is the one error the browser has to act on rather than
// display: its remedy is to ask the person at the keyboard for the password and
// repeat the identical request, idempotency key and approved fee included. So
// it carries a code, and the code has to survive wherever the check is raised
// from — which means the shared mapper owns it. Assembled at the call site, a
// check that moved anywhere else fell into the mapper's default branch: 502, a
// "platform request failed" line in the log, and nothing to prompt on.
func TestARefusedSpendTellsTheBrowserToAskForThePassword(t *testing.T) {
	t.Parallel()

	fixture, spaceID, transfers, transfer := spendingFixture(t)
	refused := platformRequest(t, fixture.handler, http.MethodPost, transfers, transfer)
	if refused.Code != http.StatusForbidden {
		t.Fatalf("unconfirmed transfer = %d %s", refused.Code, refused.Body.String())
	}
	body := decodeBody[map[string]string](t, refused)
	if body["code"] != "send_confirmation_required" {
		t.Errorf("code = %q, want send_confirmation_required so the UI can prompt", body["code"])
	}
	if body["error"] == "" {
		t.Error("the refusal carries no sentence for the person reading it")
	}

	// Confirmed, the same request passes the gate and fails further along for a
	// reason that has nothing to do with the step-up: the space has no wallets.
	if _, err := fixture.spaces.ConfirmSend(t.Context(), spaceID, "test-vault-password"); err != nil {
		t.Fatalf("spaces.ConfirmSend() error = %v", err)
	}
	confirmed := platformRequest(t, fixture.handler, http.MethodPost, transfers, transfer)
	if confirmed.Code != http.StatusNotFound {
		t.Fatalf("confirmed transfer = %d %s, want the gate open", confirmed.Code, confirmed.Body.String())
	}

	// A code is a promise that the UI has a remedy for exactly this refusal, so
	// it does not go on every error. A locked space is a different problem with
	// a different answer.
	if err := fixture.spaces.Lock(spaceID); err != nil {
		t.Fatalf("spaces.Lock() error = %v", err)
	}
	locked := platformRequest(t, fixture.handler, http.MethodPost, transfers, transfer)
	if locked.Code != http.StatusLocked {
		t.Fatalf("transfer from a locked space = %d %s", locked.Code, locked.Body.String())
	}
	if _, coded := decodeBody[map[string]string](t, locked)["code"]; coded {
		t.Errorf("a locked space answered with a code: %s", locked.Body.String())
	}
}

// The confirmation dialog can be dismissed while the derivation behind it is
// still running, and the browser then aborts and reports that nothing was
// confirmed. The handler has to hand the manager the request's own context for
// that report to be true — anything else and the window opens behind a page
// that has already said it did not.
func TestADismissedConfirmationLeavesNoWindowBehind(t *testing.T) {
	t.Parallel()

	fixture, spaceID, transfers, transfer := spendingFixture(t)
	dismissed, dismiss := context.WithCancel(t.Context())
	dismiss()
	request := httptest.NewRequestWithContext(
		dismissed, http.MethodPost, "/api/spaces/"+spaceID+"/confirm-send",
		strings.NewReader(`{"password":"test-vault-password"}`),
	)
	request.Host = testHost
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(httpapi.TokenHeader, testToken)
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)

	// 499: nobody is listening, and the alternatives all say something false.
	if response.Code != 499 {
		t.Fatalf("dismissed confirmation = %d %s, want 499", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "expires_at") {
		t.Errorf("the abandoned confirmation reported a window: %s", response.Body.String())
	}
	// And the transfer it would have authorised is refused exactly as before.
	refused := platformRequest(t, fixture.handler, http.MethodPost, transfers, transfer)
	if refused.Code != http.StatusForbidden {
		t.Fatalf("transfer after a dismissed confirmation = %d %s", refused.Code, refused.Body.String())
	}
	if code := decodeBody[map[string]string](t, refused)["code"]; code != "send_confirmation_required" {
		t.Errorf("code = %q, want send_confirmation_required", code)
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
