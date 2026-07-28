package tron

import (
	"errors"
	"math/big"
	"testing"

	"github.com/shopspring/decimal"
)

// These drive the real gotron constructors rather than a hand-built error, so
// they still hold if the SDK changes which sentinel it returns — what matters
// here is that a pre-RPC refusal reaches the HTTP layer as ErrInvalidRequest.
func TestTRXAmount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		amount  string
		wantSUN int64
		wantErr bool
	}{
		{name: "whole TRX", amount: "1", wantSUN: 1_000_000},
		{name: "fractional", amount: "1.5", wantSUN: 1_500_000},
		{name: "one SUN", amount: "0.000001", wantSUN: 1},
		{name: "finer than SUN", amount: "0.0000001", wantErr: true},
		// Scaled by 1e6 this exceeds int64, which the chain cannot carry.
		{name: "overflows int64 SUN", amount: "18446744073710", wantErr: true},
		{name: "absurdly large", amount: "1e30", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := trxAmount(decimal.RequireFromString(tt.amount))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("trxAmount(%s) = %d, want an error", tt.amount, got)
				}

				if !errors.Is(err, ErrInvalidRequest) {
					t.Errorf("error %v does not wrap ErrInvalidRequest, so the API would answer 502", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("trxAmount(%s) error = %v", tt.amount, err)
			}

			if got.Int64() != tt.wantSUN {
				t.Errorf("trxAmount(%s) = %d SUN, want %d", tt.amount, got.Int64(), tt.wantSUN)
			}
		})
	}
}

func TestTokenAmount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		amount    string
		decimals  int32
		wantUnits string
		wantErr   bool
	}{
		{name: "whole token", amount: "1", decimals: 6, wantUnits: "1000000"},
		{name: "fractional", amount: "1.5", decimals: 6, wantUnits: "1500000"},
		{name: "smallest unit", amount: "0.000001", decimals: 6, wantUnits: "1"},
		{name: "too precise", amount: "0.0000001", decimals: 6, wantErr: true},
		// Wider than an ABI uint256 word.
		{name: "overflows uint256", amount: "1e70", decimals: 18, wantErr: true},
		{name: "decimals beyond the cap", amount: "1", decimals: 100, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := tokenAmount(decimal.RequireFromString(tt.amount), tt.decimals)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("tokenAmount(%s, %d) = %s, want an error", tt.amount, tt.decimals, got)
				}

				if !errors.Is(err, ErrInvalidRequest) {
					t.Errorf("error %v does not wrap ErrInvalidRequest, so the API would answer 502", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("tokenAmount(%s, %d) error = %v", tt.amount, tt.decimals, err)
			}

			if got.TokenUnits().String() != tt.wantUnits {
				t.Errorf("tokenAmount(%s, %d) = %s, want %s", tt.amount, tt.decimals, got.TokenUnits(), tt.wantUnits)
			}
		})
	}
}

func TestValidateDecimals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		decimals *big.Int
		want     int32
		wantErr  bool
	}{
		{name: "usdt", decimals: big.NewInt(6), want: 6},
		{name: "lower bound", decimals: big.NewInt(1), want: 1},
		{name: "upper bound", decimals: big.NewInt(maxTokenDecimals), want: maxTokenDecimals},
		// An all-zero response word parses as a legitimate 0, so a wrong
		// contract would otherwise scale every amount by 10^decimals.
		{name: "zero", decimals: big.NewInt(0), wantErr: true},
		{name: "negative", decimals: big.NewInt(-1), wantErr: true},
		{name: "past the uint256 cap", decimals: big.NewInt(maxTokenDecimals + 1), wantErr: true},
		// Accepting this used to let one balance render a gigabyte-long string.
		{name: "absurd", decimals: big.NewInt(1_000_000_000), wantErr: true},
		{name: "wider than int64", decimals: new(big.Int).Lsh(big.NewInt(1), 70), wantErr: true},
		{name: "nil", decimals: nil, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := validateDecimals(tt.decimals)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("validateDecimals(%v) = %d, want an error", tt.decimals, got)
				}
				return
			}

			if err != nil {
				t.Fatalf("validateDecimals(%v) error = %v", tt.decimals, err)
			}

			if got != tt.want {
				t.Errorf("validateDecimals(%v) = %d, want %d", tt.decimals, got, tt.want)
			}
		})
	}
}

// Whatever the startup guard admits, the send-time constructor must also take,
// or a contract inside one window and outside the other would make a valid
// request fail as if the caller had sent something wrong.
//
// Each case uses one minimal unit of the token, the only amount that fits at
// every scale — at 78 decimals even 1 whole token overflows an ABI uint256.
func TestDecimalsGuardMatchesConstructor(t *testing.T) {
	t.Parallel()

	for decimals := int32(1); decimals <= maxTokenDecimals+1; decimals++ {
		_, guardErr := validateDecimals(big.NewInt(int64(decimals)))
		_, convErr := tokenAmount(decimal.New(1, -decimals), decimals)

		if guardErr == nil && convErr != nil {
			t.Errorf("decimals %d passes the startup guard but the constructor rejects it: %v", decimals, convErr)
		}

		if guardErr != nil && convErr == nil {
			t.Errorf("decimals %d is rejected at startup but accepted by the constructor", decimals)
		}
	}
}
