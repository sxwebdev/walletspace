package tron

import (
	"fmt"
	"math/big"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/gotron/pkg/client"
)

// validateDecimals checks what a token contract reports as its decimals before
// the service starts scaling every balance and every transfer by it.
//
// The accepted window has to match what the amount constructors take, or a
// perfectly good send would be refused at conversion time and blamed on the
// caller. gotron caps decimals at the digits of an ABI uint256; anything wider
// would also make Decimal().String() build a gigabyte-long number out of a
// single balance.
//
// Zero is excluded deliberately: an all-zero response word parses as a
// legitimate 0 — a wrong contract address or a fallback function is enough —
// and accepting it would scale everything by 10^decimals. No stablecoin lacks
// a fractional unit.
func validateDecimals(decimals *big.Int) (int32, error) {
	if decimals == nil {
		return 0, fmt.Errorf("%w: no decimals reported", ErrInvalidRequest)
	}

	if !decimals.IsInt64() || decimals.Int64() < 1 || decimals.Int64() > maxTokenDecimals {
		return 0, fmt.Errorf("reports unusable decimals %s, expected 1..%d", decimals, maxTokenDecimals)
	}

	return int32(decimals.Int64()), nil
}

// trxAmount converts a user-supplied TRX amount into the SUN the chain works
// in. The constructor is the single place that rejects an amount the chain
// cannot represent, so no separate range check belongs at the call site.
//
// It runs before any RPC, so a refusal is about the request itself: the error
// carries ErrInvalidRequest and the HTTP layer answers 400 without having to
// know which sentinel the SDK happens to use.
func trxAmount(amount decimal.Decimal) (client.SUN, error) {
	sun, err := client.FromTRX(amount)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}

	return sun, nil
}

// tokenAmount converts a user-supplied token amount into the contract's
// minimal units, rejecting anything finer than the token can represent rather
// than truncating it.
func tokenAmount(amount decimal.Decimal, decimals int32) (client.TokenAmount, error) {
	tokens, err := client.FromTokenDecimal(amount, decimals)
	if err != nil {
		return client.TokenAmount{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}

	return tokens, nil
}
