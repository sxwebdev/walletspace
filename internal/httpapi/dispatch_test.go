package httpapi_test

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/chain/chaintest"
	"github.com/sxwebdev/walletspace/internal/httpapi"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/operation"
	"github.com/sxwebdev/walletspace/internal/space"
)

// A registered network whose family has no adapter — what a Solana network
// would be if its adapter were left out of the set — is refused on every
// generic route with an explicit error, and the Tron adapter, which used to
// be the fallback for anything that was not EVM, is never called for it.
//
// The handler is built with a set holding only a recording Tron adapter, so
// the EVM networks of the built-in registry play the family without an
// adapter. The same requests against a Tron network are the control: each one
// has to reach the Tron adapter exactly once, which proves the EVM refusals
// come from the dispatch and not from something earlier on the route.
func TestRoutesRefuseANetworkWhoseFamilyHasNoAdapter(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	tron := &chaintest.Adapter{Kind: "trc20"}
	chains, err := chain.NewAdapters(map[network.Family]chain.Adapter{network.FamilyTron: tron})
	if err != nil {
		t.Fatalf("chain.NewAdapters() error = %v", err)
	}
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	handler, err := httpapi.NewPlatform(
		fixture.spaces, fixture.settings, registry, operation.New(fixture.settings.Home()),
		fixture.assets, chains, fixture.evm, fixture.tron, fixture.doctor, fixture.prices,
		testAccess(), slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("httpapi.NewPlatform() error = %v", err)
	}
	created, err := fixture.spaces.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatalf("spaces.Create() error = %v", err)
	}
	spaceID := created.Space.ID
	accountIDs := make(map[string]string)
	for _, networkID := range []string{"tron-nile", "ethereum-mainnet"} {
		derived := platformRequest(t, handler, http.MethodPost, "/api/spaces/"+spaceID+"/accounts/derive",
			map[string]string{"network_id": networkID, "label": networkID})
		if derived.Code != http.StatusCreated {
			t.Fatalf("derive %s = %d %s", networkID, derived.Code, derived.Body)
		}
		accountIDs[networkID] = decodeBody[struct {
			ID string `json:"id"`
		}](t, derived).ID
	}

	type request struct {
		method, path string
		body         any
	}
	routes := []struct {
		name    string
		adapter string
		build   func(networkID, to, contract string) request
	}{
		{
			name: "balances", adapter: "BalanceStream",
			build: func(networkID, _, _ string) request {
				return request{http.MethodGet, "/api/spaces/" + spaceID + "/networks/" + networkID + "/balances", nil}
			},
		},
		{
			name: "estimate", adapter: "EstimateTransfer",
			build: func(networkID, to, _ string) request {
				return request{
					http.MethodPost, "/api/spaces/" + spaceID + "/networks/" + networkID + "/transfers/estimate",
					map[string]any{
						"account_id": accountIDs[networkID], "asset_id": networkID + ":native",
						"to": to, "amount": "1",
					},
				}
			},
		},
		{
			name: "estimate max", adapter: "EstimateMaxTransfer",
			build: func(networkID, to, _ string) request {
				return request{
					http.MethodPost, "/api/spaces/" + spaceID + "/networks/" + networkID + "/transfers/estimate",
					map[string]any{
						"account_id": accountIDs[networkID], "asset_id": networkID + ":native",
						"to": to, "amount": "max",
					},
				}
			},
		},
		{
			name: "transaction", adapter: "Transaction",
			build: func(networkID, _, _ string) request {
				return request{http.MethodGet, "/api/spaces/" + spaceID + "/networks/" + networkID + "/transactions/abc", nil}
			},
		},
		{
			name: "rpc test", adapter: "VerifyEndpoint",
			build: func(networkID, _, _ string) request {
				return request{
					http.MethodPost, "/api/settings/networks/" + networkID + "/rpc/test",
					map[string]any{"endpoints": []map[string]string{{"url": "https://rpc.example"}}},
				}
			},
		},
		{
			name: "add asset", adapter: "TokenMetadata",
			build: func(networkID, _, contract string) request {
				return request{
					http.MethodPost, "/api/settings/assets",
					map[string]string{"network_id": networkID, "contract": contract},
				}
			},
		},
	}
	// Not parallel: every route shares the one recording adapter, and the
	// control counts its calls.
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			before := len(tron.Calls())
			refused := route.build(
				"ethereum-mainnet", "0x000000000000000000000000000000000000dEaD",
				"0xdAC17F958D2ee523a2206206994597C13D831ec7",
			)
			response := platformRequest(t, handler, refused.method, refused.path, refused.body)
			if response.Code != http.StatusBadGateway ||
				!strings.Contains(response.Body.String(), "unsupported chain family") {
				t.Errorf("%s %s = %d %s, want 502 unsupported chain family",
					refused.method, refused.path, response.Code, response.Body)
			}
			if calls := tron.Calls()[before:]; len(calls) != 0 {
				t.Fatalf("an EVM request reached the Tron adapter: %v", calls)
			}

			control := route.build(
				"tron-nile", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf",
			)
			platformRequest(t, handler, control.method, control.path, control.body)
			calls := tron.Calls()[before:]
			if len(calls) != 1 || calls[0] != (chaintest.Call{Method: route.adapter, NetworkID: "tron-nile"}) {
				t.Errorf("the Tron control reached the Tron adapter as %v, want one %s", calls, route.adapter)
			}
		})
	}
}
