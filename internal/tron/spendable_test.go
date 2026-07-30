package tron

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
)

func TestSpendableReservesOnlyPaidBandwidth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		fee        string
		activation string
		bandwidth  string
		want       string
	}{
		{
			name:       "bandwidth pool covers the transfer",
			fee:        "0",
			activation: "0",
			bandwidth:  "0",
			want:       "2000",
		},
		{
			name:       "bandwidth is paid in TRX",
			fee:        "0.269",
			activation: "0",
			bandwidth:  "0.269",
			want:       "1999.715",
		},
		{
			name:       "activation is the only fee",
			fee:        "1",
			activation: "1",
			bandwidth:  "0",
			want:       "1999",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newTestService(func(_ context.Context, _ string) (Balance, error) {
				return Balance{TRX: decimal.NewFromInt(2000), Activated: true}, nil
			})
			s.estimates.Set(someAddress+"|"+someAddress+"|trx", Estimate{
				Fee:          decimal.RequireFromString(tt.fee),
				Activation:   decimal.RequireFromString(tt.activation),
				bandwidthFee: decimal.RequireFromString(tt.bandwidth),
			}, 0)

			got, _, err := s.Spendable(t.Context(), someAddress, someAddress, AssetTRX)
			if err != nil {
				t.Fatalf("Spendable() error = %v", err)
			}

			if !got.Equal(decimal.RequireFromString(tt.want)) {
				t.Errorf("Spendable() = %s, want %s", got, tt.want)
			}
		})
	}
}
