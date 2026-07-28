package tron

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// trxDecimals is the number of fractional digits of TRX (1 TRX = 1e6 SUN).
const trxDecimals = 6

// ErrInvalidRequest marks failures caused by the caller's input rather than by
// the network, so the HTTP layer can answer 400 instead of 502.
var ErrInvalidRequest = errors.New("invalid request")

// toTokenUnits converts a human-readable token amount into the contract's
// smallest unit, rejecting values with more precision than the token has.
func toTokenUnits(amount decimal.Decimal, decimals int32) (decimal.Decimal, error) {
	raw := amount.Shift(decimals)
	if !raw.Equal(raw.Truncate(0)) {
		return decimal.Zero, fmt.Errorf("%w: amount %s has more than %d decimal places", ErrInvalidRequest, amount, decimals)
	}

	return raw, nil
}

// checkFitsInt64 reports whether amount still fits an int64 once scaled to the
// smallest unit. Both gotron transfer paths funnel the scaled value through
// decimal.IntPart, which keeps only the low 64 bits of anything larger — a
// silent wrap that would send a different amount than the one requested.
func checkFitsInt64(amount decimal.Decimal, decimals int32) error {
	if !amount.Shift(decimals).Truncate(0).BigInt().IsInt64() {
		return fmt.Errorf("%w: amount %s is too large to be represented on chain", ErrInvalidRequest, amount)
	}

	return nil
}

// fromTokenUnits converts a raw on-chain balance into a human-readable amount.
func fromTokenUnits(raw *big.Int, decimals int32) decimal.Decimal {
	if raw == nil {
		return decimal.Zero
	}

	return decimal.NewFromBigInt(raw, -decimals)
}

// balanceCache serves recently fetched balances so that re-rendering the UI
// does not hit the RPC nodes again.
type balanceCache struct {
	ttl time.Duration

	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	balance Balance
	at      time.Time
}

func newBalanceCache(ttl time.Duration) *balanceCache {
	return &balanceCache{ttl: ttl, entries: make(map[string]cacheEntry)}
}

// get returns a cached balance when it is still fresh at time now.
func (c *balanceCache) get(addr string, now time.Time) (Balance, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[addr]
	if !ok || now.Sub(e.at) >= c.ttl {
		return Balance{}, false
	}

	return e.balance, true
}

func (c *balanceCache) put(addr string, b Balance, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[addr] = cacheEntry{balance: b, at: now}
}

func (c *balanceCache) invalidate(addresses ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, addr := range addresses {
		delete(c.entries, addr)
	}
}
