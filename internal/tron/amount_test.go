package tron

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestToTokenUnits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		amount   string
		decimals int32
		want     string
		wantErr  bool
	}{
		{name: "whole token", amount: "1", decimals: 6, want: "1000000"},
		{name: "fractional", amount: "1.5", decimals: 6, want: "1500000"},
		{name: "smallest unit", amount: "0.000001", decimals: 6, want: "1"},
		{name: "zero decimals token", amount: "7", decimals: 0, want: "7"},
		{name: "eighteen decimals", amount: "0.5", decimals: 18, want: "500000000000000000"},
		// Anything below the token's precision would be silently truncated on
		// chain, so it must be rejected instead.
		{name: "too much precision", amount: "0.0000001", decimals: 6, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := toTokenUnits(decimal.RequireFromString(tt.amount), tt.decimals)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("toTokenUnits(%s, %d) = %s, want an error", tt.amount, tt.decimals, got)
				}
				return
			}

			if err != nil {
				t.Fatalf("toTokenUnits(%s, %d) error = %v", tt.amount, tt.decimals, err)
			}

			if got.String() != tt.want {
				t.Errorf("toTokenUnits(%s, %d) = %s, want %s", tt.amount, tt.decimals, got, tt.want)
			}
		})
	}
}

func TestFromTokenUnits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		raw      *big.Int
		decimals int32
		want     string
	}{
		{name: "nil is zero", raw: nil, decimals: 6, want: "0"},
		{name: "zero", raw: big.NewInt(0), decimals: 6, want: "0"},
		{name: "one token", raw: big.NewInt(1_000_000), decimals: 6, want: "1"},
		{name: "fractional", raw: big.NewInt(1_234_567), decimals: 6, want: "1.234567"},
		{name: "no decimals", raw: big.NewInt(42), decimals: 0, want: "42"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := fromTokenUnits(tt.raw, tt.decimals).String(); got != tt.want {
				t.Errorf("fromTokenUnits(%v, %d) = %s, want %s", tt.raw, tt.decimals, got, tt.want)
			}
		})
	}
}

func TestTokenUnitsRoundTrip(t *testing.T) {
	t.Parallel()

	const decimals = 6

	for _, amount := range []string{"0.000001", "1", "1.234567", "999999.999999"} {
		raw, err := toTokenUnits(decimal.RequireFromString(amount), decimals)
		if err != nil {
			t.Fatalf("toTokenUnits(%s) error = %v", amount, err)
		}

		back := fromTokenUnits(raw.BigInt(), decimals)
		if back.String() != amount {
			t.Errorf("round trip of %s gave %s", amount, back)
		}
	}
}

func TestCheckFitsInt64(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		amount   string
		decimals int32
		wantErr  bool
	}{
		{name: "ordinary amount", amount: "1.5", decimals: 6},
		{name: "large but representable", amount: "9000000000000", decimals: 6},
		// Both gotron transfer paths scale the amount and then call
		// decimal.IntPart, which keeps only the low 64 bits of anything larger
		// — the node would accept a transfer of a completely different value.
		{name: "overflows int64 when scaled", amount: "18446744073710", decimals: 6, wantErr: true},
		{name: "absurdly large", amount: "1e30", decimals: 6, wantErr: true},
		{name: "large is fine with zero decimals", amount: "9000000000000000000", decimals: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := checkFitsInt64(decimal.RequireFromString(tt.amount), tt.decimals)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("checkFitsInt64(%s, %d) = nil, want an error", tt.amount, tt.decimals)
				}

				if !errors.Is(err, ErrInvalidRequest) {
					t.Errorf("error %v does not wrap ErrInvalidRequest, so the API would answer 502", err)
				}
				return
			}

			if err != nil {
				t.Errorf("checkFitsInt64(%s, %d) error = %v", tt.amount, tt.decimals, err)
			}
		})
	}
}

func TestToTokenUnitsMarksBadInput(t *testing.T) {
	t.Parallel()

	_, err := toTokenUnits(decimal.RequireFromString("0.0000001"), 6)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("error %v does not wrap ErrInvalidRequest", err)
	}
}

func TestBalanceCacheExpiry(t *testing.T) {
	t.Parallel()

	const ttl = 15 * time.Second

	c := newBalanceCache(ttl)
	base := time.Unix(1_700_000_000, 0)
	want := Balance{TRX: decimal.RequireFromString("5"), USDT: decimal.RequireFromString("2"), Activated: true}

	if _, ok := c.get("TAddr", base); ok {
		t.Error("get() on an empty cache returned a hit")
	}

	c.put("TAddr", want, base)

	got, ok := c.get("TAddr", base.Add(ttl-time.Nanosecond))
	if !ok {
		t.Fatal("get() just before the TTL expired returned a miss")
	}

	if !got.TRX.Equal(want.TRX) || !got.USDT.Equal(want.USDT) {
		t.Errorf("cached balance = %+v, want %+v", got, want)
	}

	if _, ok := c.get("TAddr", base.Add(ttl)); ok {
		t.Error("get() exactly at the TTL returned a hit, want a miss")
	}
}

func TestBalanceCacheInvalidate(t *testing.T) {
	t.Parallel()

	c := newBalanceCache(time.Minute)
	now := time.Unix(1_700_000_000, 0)

	c.put("TFrom", Balance{TRX: decimal.NewFromInt(1)}, now)
	c.put("TTo", Balance{TRX: decimal.NewFromInt(2)}, now)
	c.put("TOther", Balance{TRX: decimal.NewFromInt(3)}, now)

	// After a transfer both sides are stale and must be re-fetched.
	c.invalidate("TFrom", "TTo")

	for _, addr := range []string{"TFrom", "TTo"} {
		if _, ok := c.get(addr, now); ok {
			t.Errorf("get(%s) after invalidate returned a hit", addr)
		}
	}

	if _, ok := c.get("TOther", now); !ok {
		t.Error("invalidate evicted an unrelated address")
	}
}
