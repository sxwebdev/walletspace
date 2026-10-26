// Package symbiosis checks a Symbiosis cross-chain swap out of Tron before it
// is signed.
//
// Symbiosis builds the transaction itself: its API answers a quote with a
// metaRoute call whose arguments nest the calls every later hop makes — the
// Tron portal, the swap on the Symbiosis chain, the unlock on the destination
// and the final DEX swap there. The wallet cannot build that call on its own,
// so it does the next best thing: it decodes every layer, requires each to be
// encoded canonically, and compares every address that receives an approval,
// a call or funds against contracts fixed in this package and against what the
// user asked for. A response that differs anywhere is refused; nothing in it
// is taken on trust except the parts the contracts themselves enforce.
//
// The package makes no network calls. The one fact it needs from a chain —
// which 0x Settler is current on the destination — callers read with
// SettlerCalls and hand in.
package symbiosis

import "github.com/ethereum/go-ethereum/common"

// SymbiosisChainID is the chain the Symbiosis pool lives on. Every route out of
// Tron mints a synthetic there, swaps it in the octopool and burns the result
// toward the destination.
const SymbiosisChainID = 13863860

// Gateway is the Tron contract a route is sent to and USDT is approved to. It
// claims exactly the amount of one metaRoute call from the sender and hands it
// to the router behind it.
const Gateway = "TPuaJ6gnYfE9gLUDFrbWbPx3tMnjG8max1"

// Native is how both DEX aggregators a route may end in name the destination's
// own coin.
var Native = common.HexToAddress("0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE")

// synth is a synthetic token on the Symbiosis chain and what it stands for.
type synth struct {
	// Token is the synthetic on the Symbiosis chain.
	Token common.Address
	// Real is the token the synthetic is unlocked as on its home chain.
	Real common.Address
	// PoolIndex is the synthetic's index in the octopool.
	PoolIndex uint64
}

// destination is the Symbiosis deployment on a chain a route can end on.
type destination struct {
	Portal common.Address
	Bridge common.Address
	// AllowanceHolder is the 0x contract a 0x final swap is entered through.
	AllowanceHolder common.Address
	// Stables are the synthetics a route may unlock on this chain.
	Stables []synth
}

// The addresses below come from three places, and every one was checked
// against at least two of them. The Symbiosis SDK (symbiosis-js-sdk 3.11.60,
// config/mainnet.ts) names the Symbiosis-chain contracts and each destination's
// portal and bridge. The chains themselves confirm the rest: the Symbiosis
// fabric maps each synthetic to its real token (getRealRepresentation), the
// octopool maps each synthetic to its index (assetToIndex, indexToAsset), and
// the Tron gateway and portal were deployed by the same account as the portal
// the SDK still lists — the API has moved to them since the SDK was published.
var (
	tronUSDT   = common.HexToAddress("0xa614f803b6fd780986a42c78ec9c7f77e6ded13c")
	tronPortal = common.HexToAddress("0x21fb7f70b95d119f7594267a79e2897290a21caf")

	synthesis  = common.HexToAddress("0x45CFd6FB7999328F189aaD2739Fba4Be6C45E5bf")
	bridge     = common.HexToAddress("0x1a039cE63AE35a67Bf0E9F6DbFaE969639D59eC8")
	metaRouter = common.HexToAddress("0x2b7Aa8bDc40B6d3d19d0dE7480c4db8d5B6495e2")
	multicall  = common.HexToAddress("0xBbAD2fe9558e55EbfA04b3B5Bff0b6c4E2ffDD2C")
	octopool   = common.HexToAddress("0xC3255E317481B95A3e61844c274dE8BAF8eDF397")

	// tronUSDTSynth is what the Tron portal's USDT becomes on the Symbiosis
	// chain: the token every route out of Tron starts the octopool swap with.
	tronUSDTSynth = synth{
		Token:     common.HexToAddress("0x4e7498c4db259065b73d21c81A5b41cadbEc7d4D"),
		Real:      tronUSDT,
		PoolIndex: 18,
	}

	// operators are Symbiosis's own accounts on the Symbiosis chain. A route
	// names one as the revertable address of both hops, the only account that
	// may ask for a stuck transfer to be reverted, and as the caller of the burn,
	// so that a revert of the second hop lands with it rather than in a contract
	// that cannot pay it back. That makes them the one place a route trusts
	// Symbiosis with funds: only if the destination refuses the transfer, and
	// only for as long as Symbiosis takes to refund it. The first is what the
	// API sends today, the second what the SDK names; both are busy externally
	// owned accounts on the Symbiosis chain.
	operators = []common.Address{
		common.HexToAddress("0x67f9b3E561383493B3f874fEAE0c53c2cD23851D"),
		common.HexToAddress("0xd99ac0681b904991169a4f398B9043781ADbe0C3"),
	}

	// oneInchRouter is the 1inch aggregation router v6, at one address on every
	// chain. It pays the swap out to the receiver its call names and reverts
	// unless that is at least the call's minimum, whatever its executor does.
	oneInchRouter = common.HexToAddress("0x111111125421cA6dc452d289314280a0f8842A65")

	// allowanceHolder is the 0x AllowanceHolder on every chain with Cancun.
	allowanceHolder = common.HexToAddress("0x0000000000001fF3684f28c67538d4D072C22734")

	// ZeroExDeployer is the 0x registry that names the current Settler of each
	// feature on every chain; see SettlerCalls.
	ZeroExDeployer = common.HexToAddress("0x00000000000004533Fe15556B1E086BB1A72cEae")
)

var destinations = map[uint64]destination{
	1: {
		Portal:          common.HexToAddress("0xb8f275fBf7A959F4BCE59999A2EF122A099e81A8"),
		Bridge:          common.HexToAddress("0x5523985926Aa12BA58DC5Ad00DDca99678D7227E"),
		AllowanceHolder: allowanceHolder,
		Stables: []synth{{
			Token:     common.HexToAddress("0xFAEb87361cB1925aFdd1967BD896F0c8FeacB890"),
			Real:      common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
			PoolIndex: 0,
		}},
	},
	56: {
		Portal:          common.HexToAddress("0x5Aa5f7f84eD0E5db0a4a85C3947eA16B53352FD4"),
		Bridge:          common.HexToAddress("0xb8f275fBf7A959F4BCE59999A2EF122A099e81A8"),
		AllowanceHolder: allowanceHolder,
		Stables: []synth{{
			Token:     common.HexToAddress("0x076Cb7BEee7D1507DE7B964c29cbC849acaD5022"),
			Real:      common.HexToAddress("0x8AC76a51cc950d9822D68b83fE1Ad97B32Cd580d"),
			PoolIndex: 5,
		}},
	},
	42161: {
		Portal:          common.HexToAddress("0x01A3c8E513B758EBB011F7AFaf6C37616c9C24d9"),
		Bridge:          common.HexToAddress("0x5523985926Aa12BA58DC5Ad00DDca99678D7227E"),
		AllowanceHolder: allowanceHolder,
		Stables: []synth{{
			Token:     common.HexToAddress("0xc317169a336b484f65B0AB4A794bBe66a7491E83"),
			Real:      common.HexToAddress("0xaf88d065e77c8cC2239327C5EDb3A432268e5831"),
			PoolIndex: 11,
		}},
	},
}

// Supported reports whether a route may end on the EVM chain with this id.
func Supported(chainID uint64) bool {
	_, ok := destinations[chainID]
	return ok
}
