// Package sunswap encodes swaps between TRX and USDT on SunSwap V2, V3 and V4
// and computes what a V4 pool pays out.
//
// Everything here is local: calldata is built from typed arguments, never
// taken from a node or an aggregator API, so a signed swap can be compared
// byte for byte with what this package produced. The package makes no network
// calls of its own; callers read chain state and hand the answers in.
package sunswap

import (
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

// Deployment is SunSwap on one network: the contracts a swap goes through and
// the pools worth quoting. Every address is fixed here rather than accepted from
// a request, so a caller can choose a route but never the contract that
// receives the approval or the funds.
type Deployment struct {
	// Token is the TRC20 traded against TRX: USDT.
	Token string
	// WTRX is the wrapped native coin V2 and V3 pools hold instead of TRX.
	WTRX string

	V2Router string

	V3Router string
	V3Quoter string
	// V3Fees are the fee tiers of the TRX/USDT pools worth quoting.
	V3Fees []uint32

	UniversalRouter string
	Permit2         string
	V4PoolManager   string
	// V4Pools are the hookless native TRX/USDT pools worth quoting.
	V4Pools []V4Pool
}

// V4Pool names one V4 pool by its fee and tick spacing.
type V4Pool struct {
	Fee         uint32
	TickSpacing int
}

// deployments are keyed by the network's chain id as the network registry
// spells it. The mainnet addresses are the ones sun.io's own SDK uses, each
// checked on chain against the others: routers and quoters name the factory
// and WTRX listed here, and the universal router names this pool manager.
var deployments = map[string]Deployment{
	"mainnet": {
		Token:           "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
		WTRX:            "TNUC9Qb1rRpS5CbWLmNMxXBjyFoydXjWFR",
		V2Router:        "TNJVzGqKBWkJxJB5XYSqGAwUTV15U24pPq",
		V3Router:        "TQAvWQpT9H916GckwWDJNhYZvQMkuRL7PN",
		V3Quoter:        "TLhZ48yfHygMLM2uZr87zJJusHjGen97gh",
		V3Fees:          []uint32{500, 3000},
		UniversalRouter: "TQqgNg13s2DjvXhW1ky4v6TsR8wZGvb7Y4",
		Permit2:         "TTJxU3P8rHycAyFY4kVtGNfmnMH4ezcuM9",
		V4PoolManager:   "TVjuTE3V5bMVdpfNhid8kD2v35T2k1u1Br",
		V4Pools:         []V4Pool{{Fee: 500, TickSpacing: 10}},
	},
	"nile": {
		Token:           "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf",
		WTRX:            "TYsbWxNnyTgsZaTFaue9hqpxkU3Fkco94a",
		V2Router:        "TMn1qrmYUMSTXo9babrJLzepKZoPC7M6Sy",
		V3Router:        "TFkswj6rUfK3cQtFGzungCkNXxD2UCpEVD",
		V3Quoter:        "TUcM2gkpWEJxBpkweLdVoRp6DAUsw2vWR6",
		V3Fees:          []uint32{500, 10000},
		UniversalRouter: "TEgq4237arNE7jX74KCDkc1MXdZeWNkGVj",
		Permit2:         "TYQuuhGbEMxF7nZxUHV3uHJxAVVAegNU9h",
		V4PoolManager:   "TVivLPeq7FMmTG8Z7HaiBgHTsMwCEcipKT",
		V4Pools:         []V4Pool{{Fee: 500, TickSpacing: 10}, {Fee: 3000, TickSpacing: 1}},
	},
}

// ForNetwork returns the deployment on the network with the given chain id,
// and whether there is one.
func ForNetwork(chainID string) (Deployment, bool) {
	d, ok := deployments[chainID]
	return d, ok
}

// Addresses is a deployment's contracts in the form the ABI takes.
type Addresses struct {
	Token, WTRX, V2Router, V3Router, V3Quoter, UniversalRouter, Permit2, V4PoolManager common.Address
}

// Addresses decodes every contract of the deployment, failing on the first
// address that does not decode.
func (d Deployment) Addresses() (Addresses, error) {
	var out Addresses
	for _, field := range []struct {
		value string
		into  *common.Address
	}{
		{d.Token, &out.Token}, {d.WTRX, &out.WTRX}, {d.V2Router, &out.V2Router},
		{d.V3Router, &out.V3Router}, {d.V3Quoter, &out.V3Quoter},
		{d.UniversalRouter, &out.UniversalRouter}, {d.Permit2, &out.Permit2},
		{d.V4PoolManager, &out.V4PoolManager},
	} {
		addr, err := EVMAddress(field.value)
		if err != nil {
			return Addresses{}, fmt.Errorf("SunSwap deployment: %w", err)
		}
		*field.into = addr
	}
	return out, nil
}
