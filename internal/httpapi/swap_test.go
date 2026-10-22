package httpapi_test

import (
	"net/http"
	"testing"
)

func swapPaths(spaceID, network string) (quote, swap string) {
	base := "/api/spaces/" + spaceID + "/networks/" + network + "/accounts/acc_none/"
	return base + "swap-quote", base + "swap"
}

func validSwap() map[string]any {
	return map[string]any{
		"in": "trx", "amount": "10", "min_out": "3.2",
		"route": map[string]any{"protocol": "v2", "fee": 3000}, "step": "swap",
	}
}

// A swap moves funds, so it sits behind the same step-up as a transfer: the
// password first, before the request is even read.
func TestASwapNeedsTheSpacePassword(t *testing.T) {
	t.Parallel()
	fixture, spaceID, _, _ := spendingFixture(t)
	_, swap := swapPaths(spaceID, "tron-nile")

	refused := platformRequest(t, fixture.handler, http.MethodPost, swap, validSwap())
	if refused.Code != http.StatusForbidden {
		t.Fatalf("unconfirmed swap = %d %s", refused.Code, refused.Body.String())
	}
	if code := decodeBody[map[string]string](t, refused)["code"]; code != "send_confirmation_required" {
		t.Errorf("code = %q, want send_confirmation_required", code)
	}
}

// Everything malformed is refused before any account, node or key is looked
// at.
func TestSwapRequestsAreValidated(t *testing.T) {
	t.Parallel()
	fixture, spaceID, _, _ := spendingFixture(t)
	if _, err := fixture.spaces.ConfirmSend(t.Context(), spaceID, "test-vault-password"); err != nil {
		t.Fatalf("spaces.ConfirmSend() error = %v", err)
	}
	quote, swap := swapPaths(spaceID, "tron-nile")
	with := func(key string, value any) map[string]any {
		body := validSwap()
		body[key] = value
		return body
	}
	tests := []struct {
		name string
		path string
		body any
		want int
	}{
		{"swap of an unknown asset", swap, with("in", "btc"), http.StatusBadRequest},
		{"swap with an unparseable amount", swap, with("amount", "ten"), http.StatusBadRequest},
		{"swap with an unparseable minimum", swap, with("min_out", "lots"), http.StatusBadRequest},
		{"swap of an unknown step", swap, with("step", "bridge"), http.StatusBadRequest},
		{"swap with a contract in the route", swap, with("route", map[string]any{"protocol": "v2", "fee": 3000, "router": "T…"}), http.StatusBadRequest},
		{"swap from a wallet that does not exist", swap, validSwap(), http.StatusNotFound},
		{"quote of an unknown asset", quote, map[string]any{"in": "eth", "amount": "1"}, http.StatusBadRequest},
		{"quote with an unparseable amount", quote, map[string]any{"in": "usdt", "amount": "x"}, http.StatusBadRequest},
		{"quote with an unknown field", quote, map[string]any{"in": "usdt", "amount": "1", "slippage": 5}, http.StatusBadRequest},
		{"quote for a wallet that does not exist", quote, map[string]any{"in": "usdt", "amount": "1"}, http.StatusNotFound},
	}
	for _, tt := range tests {
		response := platformRequest(t, fixture.handler, http.MethodPost, tt.path, tt.body)
		if response.Code != tt.want {
			t.Errorf("%s = %d %s, want %d", tt.name, response.Code, response.Body.String(), tt.want)
		}
	}

	evmQuote, _ := swapPaths(spaceID, "ethereum-mainnet")
	if response := platformRequest(t, fixture.handler, http.MethodPost, evmQuote,
		map[string]any{"in": "usdt", "amount": "1"}); response.Code != http.StatusBadRequest {
		t.Errorf("quote on an EVM network = %d %s, want 400", response.Code, response.Body.String())
	}
}
