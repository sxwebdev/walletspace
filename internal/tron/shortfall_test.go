package tron

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/gotron/pkg/client"
	"github.com/sxwebdev/gotron/schema/pb/api"
)

func TestShortfall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		trx    string
		asset  Asset
		amount string
		fee    string
		want   string
	}{
		{
			// The case the node answers with a bare BANDWITH_ERROR: plenty of
			// USDT, nothing to pay the fee with.
			name:   "token transfer with no TRX for the fee",
			trx:    "0",
			asset:  AssetUSDT,
			amount: "10",
			fee:    "27.3",
			want:   "27.3",
		},
		{
			name:   "token transfer the balance covers",
			trx:    "30",
			asset:  AssetUSDT,
			amount: "10",
			fee:    "27.3",
			want:   "0",
		},
		{
			// Sending TRX spends the amount too, so the fee alone is not the test.
			name:   "TRX transfer counts the amount as well",
			trx:    "10",
			asset:  AssetTRX,
			amount: "10",
			fee:    "0.269",
			want:   "0.269",
		},
		{
			name:   "TRX transfer that just fits",
			trx:    "10.269",
			asset:  AssetTRX,
			amount: "10",
			fee:    "0.269",
			want:   "0",
		},
		{
			name:   "no fee at all",
			trx:    "5",
			asset:  AssetUSDT,
			amount: "1",
			fee:    "0",
			want:   "0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newTestService(func(_ context.Context, _ string) (Balance, error) {
				return Balance{
					TRX:       decimal.RequireFromString(tt.trx),
					USDT:      decimal.NewFromInt(1000),
					Activated: true,
				}, nil
			})

			got, err := s.Shortfall(t.Context(), someAddress, tt.asset,
				decimal.RequireFromString(tt.amount),
				Estimate{Fee: decimal.RequireFromString(tt.fee)})
			if err != nil {
				t.Fatalf("Shortfall() error = %v", err)
			}

			if !got.Equal(decimal.RequireFromString(tt.want)) {
				t.Errorf("Shortfall() = %s, want %s", got, tt.want)
			}
		})
	}
}

// The node reports why it refused a broadcast in a response code, not in the
// message. Codes that name something about the transaction have to reach the
// HTTP layer as ErrInvalidRequest, or a user error is logged as an upstream
// failure and answered with 502.
func TestChainErrorClassifiesBroadcastRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		code        api.ReturnResponseCode
		wantInvalid bool
	}{
		{name: "bandwidth", code: api.Return_BANDWITH_ERROR, wantInvalid: true},
		{name: "contract validate", code: api.Return_CONTRACT_VALIDATE_ERROR, wantInvalid: true},
		{name: "contract execution", code: api.Return_CONTRACT_EXE_ERROR, wantInvalid: true},
		{name: "too big", code: api.Return_TOO_BIG_TRANSACTION_ERROR, wantInvalid: true},
		// These say nothing about the request; retrying elsewhere may work.
		{name: "server busy", code: api.Return_SERVER_BUSY},
		{name: "no connection", code: api.Return_NO_CONNECTION},
		{name: "block unsolidified", code: api.Return_BLOCK_UNSOLIDIFIED},
		{name: "unknown", code: api.Return_OTHER_ERROR},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := newTestService(nil)
			// The message is deliberately empty: the node often sends none, and
			// the classification must not depend on one.
			err := s.chainError("broadcast transaction", &client.BroadcastError{Code: tt.code})

			if got := errors.Is(err, ErrInvalidRequest); got != tt.wantInvalid {
				t.Errorf("wraps ErrInvalidRequest = %v, want %v (error: %v)", got, tt.wantInvalid, err)
			}
		})
	}
}

// A contract validation refusal arrives by a different route than a broadcast
// rejection, and both have to land on the same side of the 400/502 split.
func TestChainErrorClassifiesContractValidation(t *testing.T) {
	t.Parallel()

	s := newTestService(nil)
	err := s.chainError("build transaction", &client.ContractValidateError{
		Message: "no unFreeze balance to withdraw",
	})

	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("error %v does not wrap ErrInvalidRequest", err)
	}

	// The node's own wording names the actual condition; nothing here replaces it.
	if !strings.Contains(err.Error(), "no unFreeze balance") {
		t.Errorf("error = %q, want it to carry the node's reason", err)
	}
}
