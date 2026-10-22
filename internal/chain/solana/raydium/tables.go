package raydium

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/url"
	"strings"

	"github.com/gagliardetto/solana-go"
)

// DefaultPoolKeysURL is Raydium's read API (API v3), which names the address
// lookup table each pool is published with.
//
// Terms: https://docs.raydium.io/sdk-api/rest-api, section "Auth" — every
// service but the LaunchLab Forum needs no auth: "Hit them anonymously over
// HTTPS." API v3 is listed there as "the default front door for the UI and
// most integrators"; above the published limits the hosts answer 429 with
// Retry-After, which the Quoter honours for this endpoint as for quotes
// (they share one pace). Read 2026-10-22.
const DefaultPoolKeysURL = "https://api-v3.raydium.io"

// MaxLookupPools bounds one LookupTables request.
const MaxLookupPools = 10

// LookupTables asks Raydium which address lookup table each pool is published
// with. The answer only names candidates: a table's contents are read from
// the chain and every address taken from it is checked there, so a wrong name
// can make a transaction larger or refused, never different. Pools the API
// does not know, or names without a valid table, are left out of the map.
func (q *Quoter) LookupTables(ctx context.Context, pools []solana.PublicKey) (map[solana.PublicKey]solana.PublicKey, error) {
	if len(pools) == 0 || len(pools) > MaxLookupPools {
		return nil, fmt.Errorf("raydium: %d pools, want 1..%d", len(pools), MaxLookupPools)
	}
	ids := make([]string, len(pools))
	for i, pool := range pools {
		ids[i] = pool.String()
	}
	endpoint := q.poolKeysURL.JoinPath("pools", "key", "ids")
	endpoint.RawQuery = url.Values{"ids": {strings.Join(ids, ",")}}.Encode()
	body, err := q.get(ctx, endpoint, "pool keys")
	if err != nil {
		return nil, err
	}
	var resp struct {
		Success bool `json:"success"`
		Data    []*struct {
			ID                 string `json:"id"`
			LookupTableAccount string `json:"lookupTableAccount"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("%w: pool keys: %w", ErrAPI, err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("%w: pool keys request failed", ErrAPI)
	}
	wanted := make(map[solana.PublicKey]bool, len(pools))
	for _, pool := range pools {
		wanted[pool] = true
	}
	out := make(map[solana.PublicKey]solana.PublicKey, len(pools))
	for _, item := range resp.Data {
		if item == nil {
			continue
		}
		pool, err := parseKey(item.ID)
		if err != nil || !wanted[pool] {
			continue
		}
		table, err := parseKey(item.LookupTableAccount)
		if err != nil || table == pool {
			continue
		}
		out[pool] = table
	}
	return out, nil
}
