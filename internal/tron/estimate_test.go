package tron

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

// A valid mainnet address, so validation passes and the amount check is what
// the test actually exercises.
const someAddress = "TEeKaYdpN6ujnpVZ1SkohE6Ru6gd9vGC2A"

// These inputs are refused before any node call, so they must not be reported
// as an upstream failure: the HTTP layer keys on ErrInvalidRequest to answer
// 400 rather than 502, and a 502 would also be retried against every node
// first.
func TestEstimateRejectsBadInputBeforeCallingTheChain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		to     string
		asset  Asset
		amount decimal.Decimal
	}{
		{name: "zero amount", to: someAddress, asset: AssetTRX, amount: decimal.Zero},
		{name: "negative amount", to: someAddress, asset: AssetTRX, amount: decimal.NewFromInt(-1)},
		{name: "invalid recipient", to: "nonsense", asset: AssetTRX, amount: decimal.NewFromInt(1)},
		{name: "unknown asset", to: someAddress, asset: Asset("btc"), amount: decimal.NewFromInt(1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// A Service with no client at all: reaching the chain would panic,
			// which is exactly the guarantee under test.
			s := newTestService(nil)

			_, err := s.Estimate(t.Context(), someAddress, tt.to, tt.asset, tt.amount)
			if err == nil {
				t.Fatal("Estimate() = nil, want an error")
			}

			if !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("error %v does not wrap ErrInvalidRequest, so the API would answer 502", err)
			}
		})
	}
}

func TestEstimateServesRepeatedCallsFromCache(t *testing.T) {
	t.Parallel()

	s := newTestService(nil)

	// Pricing a transfer is five or six node calls; the UI asks on every pause
	// in typing, and a keyless public endpoint allows about three per second.
	want := Estimate{Fee: decimal.RequireFromString("0.269")}
	s.estimates.Set(someAddress+"|"+someAddress+"|trx", want, 0)

	got, err := s.Estimate(t.Context(), someAddress, someAddress, AssetTRX, decimal.NewFromInt(1))
	if err != nil {
		t.Fatalf("Estimate() error = %v", err)
	}

	if !got.Fee.Equal(want.Fee) {
		t.Errorf("fee = %s, want the cached %s", got.Fee, want.Fee)
	}
}

func TestInvalidateClearsEstimates(t *testing.T) {
	t.Parallel()

	s := newTestService(nil)
	s.estimates.Set("a|b|trx", Estimate{Fee: decimal.NewFromInt(1)}, 0)

	// Sending can activate the recipient, which makes every later transfer to
	// them about 1 TRX cheaper — the cached figure would overstate it.
	s.invalidate("a", "b")

	if item := s.estimates.Get("a|b|trx"); item != nil {
		t.Error("an estimate survived a transfer that may have activated the recipient")
	}
}
