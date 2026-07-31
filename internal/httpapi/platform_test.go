package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/asset"
	evmchain "github.com/sxwebdev/walletspace/internal/chain/evm"
	tronchain "github.com/sxwebdev/walletspace/internal/chain/tron"
	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/httpapi"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/operation"
	"github.com/sxwebdev/walletspace/internal/rpcpool"
	"github.com/sxwebdev/walletspace/internal/space"
	"github.com/sxwebdev/walletspace/internal/vault"
)

type platformFixture struct {
	handler http.Handler
	spaces  *space.Manager
	evm     *evmchain.Adapter
	tron    *tronchain.Adapter
}

func newPlatformFixture(t *testing.T) platformFixture {
	t.Helper()
	home := t.TempDir()
	settings, err := config.NewHomeManager(home)
	if err != nil {
		t.Fatalf("config.NewHomeManager() error = %v", err)
	}
	spaces, err := space.NewManager(home, 15*time.Minute, vault.Params{
		Time: 1, MemoryKiB: 8 * 1024, Parallelism: 1,
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
	handler, err := httpapi.NewPlatform(
		spaces, settings, registry, operation.New(home), mustAssetStore(t, home), evm, tron,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("httpapi.NewPlatform() error = %v", err)
	}
	t.Cleanup(func() {
		tron.Close()
		evm.Close()
		spaces.Close()
	})
	return platformFixture{handler: handler, spaces: spaces, evm: evm, tron: tron}
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
		"name": "", "mnemonic": "", "password": "password",
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
			Addresses map[string]string `json:"addresses"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if payload.Space.Name != "default" || payload.Space.Locked ||
		!payload.MnemonicGenerated || payload.Mnemonic == "" {
		t.Errorf("created payload = %+v", payload)
	}
	if len(payload.Accounts) != 1 || payload.Accounts[0].Addresses["tron"] == "" ||
		payload.Accounts[0].Addresses["evm"] == "" {
		t.Errorf("accounts = %+v", payload.Accounts)
	}
}

func TestPlatformImportBadgeAndLockedSecrets(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	created, err := fixture.spaces.Create(space.CreateRequest{
		Password: "password", ImportedOnly: true,
	})
	if err != nil {
		t.Fatalf("spaces.Create() error = %v", err)
	}
	imported := platformRequest(t, fixture.handler, http.MethodPost,
		"/api/spaces/"+created.Space.ID+"/accounts/import", map[string]any{
			"curve":       "secp256k1",
			"private_key": "0000000000000000000000000000000000000000000000000000000000000001",
			"label":       "Treasury",
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
	exported := platformRequest(t, fixture.handler, http.MethodPost,
		"/api/spaces/"+created.Space.ID+"/accounts/"+payload.Account.ID+"/private-key",
		map[string]string{"family": "evm"})
	if exported.Code != http.StatusLocked {
		t.Fatalf("locked export = %d %s", exported.Code, exported.Body.String())
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
	page := platformRequest(t, fixture.handler, http.MethodGet, "/settings", nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `id="app"`) {
		t.Fatalf("GET /settings = %d %s", page.Code, page.Body.String())
	}
}

func TestPlatformRejectsTrailingJSON(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	request := httptest.NewRequestWithContext(
		t.Context(), http.MethodPost, "/api/spaces",
		strings.NewReader(`{"password":"password"} {"password":"second"}`),
	)
	request.Header.Set("Content-Type", "application/json")
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
