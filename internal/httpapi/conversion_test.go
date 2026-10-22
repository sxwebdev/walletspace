package httpapi_test

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/sxwebdev/walletspace/internal/network"
)

func TestNativeWrapperQuotesShareNativePrice(t *testing.T) {
	t.Parallel()
	registry, err := network.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range registry.List() {
		if item.WrappedNative == nil {
			continue
		}
		t.Run(item.ID, func(t *testing.T) {
			t.Parallel()
			fixture := newPlatformFixture(t)
			native := item.ID + ":native"
			wrapped := item.ID + ":erc20:" + strings.ToLower(item.WrappedNative.Contract)
			response := platformRequest(t, fixture.handler, http.MethodGet, "/api/prices?"+url.Values{"asset_id": {native, wrapped}}.Encode(), nil)
			if response.Code != http.StatusOK {
				t.Fatalf("prices: %d %s", response.Code, response.Body)
			}
			body := decodeBody[struct {
				Quotes []struct {
					AssetID string `json:"asset_id"`
					Current string `json:"current_usd"`
				} `json:"quotes"`
			}](t, response)
			if item.Testnet {
				if len(body.Quotes) != 0 || fixture.prices.Calls() != 0 {
					t.Fatal("testnet assets must not receive USD quotes")
				}
				return
			}
			if !slices.Equal(fixture.prices.Requested(), []string{item.NativePrice}) {
				t.Fatalf("price identifiers = %v", fixture.prices.Requested())
			}
			if len(body.Quotes) != 2 || body.Quotes[0].Current != body.Quotes[1].Current {
				t.Fatalf("native/wrapped quotes = %+v", body.Quotes)
			}
		})
	}
}

func TestConversionActionRejectedBeforeAccountLookup(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ networkID, action string }{
		{"ethereum-mainnet", "swap"}, {"tron-mainnet", "wrap"}, {"tron-mainnet", "unwrap"},
	} {
		t.Run(tt.networkID+"/"+tt.action, func(t *testing.T) {
			t.Parallel()
			fixture := newPlatformFixture(t)
			response := platformRequest(t, fixture.handler, http.MethodPost, "/api/spaces/missing/networks/"+tt.networkID+"/transfers/estimate", map[string]any{"action": tt.action})
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "unsupported conversion action") {
				t.Fatalf("invalid action: %d %s", response.Code, response.Body)
			}
		})
	}
}
