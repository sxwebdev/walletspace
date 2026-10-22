package solana

import (
	"encoding/json"
	"strings"
	"testing"
)

// The runtime's errors arrive as JSON of several shapes; each is rendered
// readably, naming the account a rent error is about when it can.
func TestDescribe(t *testing.T) {
	t.Parallel()

	message := build(t, solPlan())
	tests := []struct {
		name    string
		failure any
		want    string
	}{
		{name: "fee", failure: "InsufficientFundsForFee", want: "does not cover the fee"},
		{name: "no account", failure: "AccountNotFound", want: "sending account does not exist"},
		{name: "other string", failure: "AccountInUse", want: "AccountInUse"},
		{name: "rent, number", failure: map[string]any{"InsufficientFundsForRent": map[string]any{"account_index": float64(0)}}, want: "account " + testPayer.String()},
		{name: "rent, json number", failure: map[string]any{"InsufficientFundsForRent": map[string]any{"account_index": json.Number("1")}}, want: "account " + testRecipient.String()},
		{name: "rent, index out of range", failure: map[string]any{"InsufficientFundsForRent": map[string]any{"account_index": float64(40)}}, want: "an account would be left"},
		{name: "rent, no index", failure: map[string]any{"InsufficientFundsForRent": map[string]any{}}, want: "an account would be left"},
		{name: "rent, bad json number", failure: map[string]any{"InsufficientFundsForRent": map[string]any{"account_index": json.Number("x")}}, want: "an account would be left"},
		{name: "other object", failure: map[string]any{"InstructionError": []any{2, "Custom"}}, want: "InstructionError"},
	}
	for _, tt := range tests {
		if got := describe(tt.failure, message); !strings.Contains(got, tt.want) {
			t.Errorf("%s: describe() = %q, want it to contain %q", tt.name, got, tt.want)
		}
	}
}

// A rent error naming no usable account is reported as the runtime's own.
func TestRentAccountNeedsAUsableIndex(t *testing.T) {
	t.Parallel()

	message := build(t, solPlan())
	for name, failure := range map[string]any{
		"not an object":   "InsufficientFundsForRent",
		"not rent":        map[string]any{"InstructionError": 1},
		"out of range":    map[string]any{"InsufficientFundsForRent": map[string]any{"account_index": float64(9)}},
		"negative":        map[string]any{"InsufficientFundsForRent": map[string]any{"account_index": float64(-1)}},
		"no index at all": map[string]any{"InsufficientFundsForRent": map[string]any{}},
	} {
		if _, ok := rentAccount(failure, message); ok {
			t.Errorf("%s: rentAccount() found an account", name)
		}
	}
	if _, ok := rentAccount(map[string]any{"InsufficientFundsForRent": map[string]any{"account_index": float64(0)}}, []byte{1}); ok {
		t.Error("rentAccount() found an account in an undecodable message")
	}
}
