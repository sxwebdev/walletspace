package symbiosis_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/sxwebdev/walletspace/internal/tron/symbiosis"
)

func TestVerifyAcceptsRecordedQuotes(t *testing.T) {
	t.Parallel()
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			tx, want := load(t, f)
			route, err := symbiosis.Verify(tx, want, settlersFor(f.chainID))
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			if route.Venue != f.venue {
				t.Errorf("Venue = %q, want %q", route.Venue, f.venue)
			}
			// The quote's own minimum is what the calldata enforces: anything
			// else means the route was read from the wrong field.
			if route.MinOut.Cmp(want.MinOut) != 0 {
				t.Errorf("MinOut = %s, want %s", route.MinOut, want.MinOut)
			}
			if route.TronFee.Cmp(big.NewInt(250_000)) != 0 {
				t.Errorf("TronFee = %s, want 0.25 USDT", route.TronFee)
			}
			if route.DestinationFee.Sign() <= 0 {
				t.Errorf("DestinationFee = %s, want a positive fee", route.DestinationFee)
			}
			if route.Method != tx.FunctionSelector {
				t.Errorf("Method = %q, want %q", route.Method, tx.FunctionSelector)
			}
			if !bytes.Equal(route.Data[:4], mustHex(t, "a11b1198")) || !bytes.Equal(route.Data[4:], tx.Data) {
				t.Error("Data is not the metaRoute selector followed by the quote's arguments")
			}
		})
	}
}

func TestVerifyReportsTheStable(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		fixture string
		stable  common.Address
		fee     string
	}{
		{"arb_usdt", arbitrumUSDC, "250000"},
		{"bsc_usdt", bscUSDC, "250000000000000000"},
	} {
		t.Run(tt.fixture, func(t *testing.T) {
			t.Parallel()
			f := fixtureNamed(t, tt.fixture)
			tx, want := load(t, f)
			route, err := symbiosis.Verify(tx, want, settlersFor(f.chainID))
			if err != nil {
				t.Fatalf("Verify() error = %v", err)
			}
			if route.Stable != tt.stable || route.DestinationFee.String() != tt.fee {
				t.Errorf("Stable, DestinationFee = %s, %s; want %s, %s", route.Stable.Hex(), route.DestinationFee, tt.stable.Hex(), tt.fee)
			}
		})
	}
}

// The helper that builds the tampered routes below must not change a route by
// itself, or every rejection could be its doing.
func TestRewriteWithoutEditKeepsTheBytes(t *testing.T) {
	t.Parallel()
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			tx, _ := load(t, f)
			if got := rewrite(t, tx, func(*symbiosis.Layers) {}); !bytes.Equal(got.Data, tx.Data) {
				t.Error("decoding and encoding again changed the transaction")
			}
		})
	}
}

func TestVerifyRefusesTamperedRoutes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		fixture string
		edit    func(*symbiosis.Layers)
		wantErr error
	}{
		// The Tron gateway call.
		{"swap on Tron", "arb_usdt", func(l *symbiosis.Layers) { l.Route.FirstSwapCalldata = []byte{1} }, symbiosis.ErrUnexpected},
		{"second swap on Tron", "arb_usdt", func(l *symbiosis.Layers) { l.Route.SecondSwapCalldata = []byte{1} }, symbiosis.ErrUnexpected},
		{"TRX in", "arb_usdt", func(l *symbiosis.Layers) { l.Route.NativeIn = true }, symbiosis.ErrUnexpected},
		{"no approved token", "arb_usdt", func(l *symbiosis.Layers) { l.Route.ApprovedTokens = nil }, symbiosis.ErrUnexpected},
		{"another claimed token", "arb_usdt", func(l *symbiosis.Layers) { l.Route.ApprovedTokens[0] = stranger }, symbiosis.ErrUnexpected},
		{"another approved token", "arb_usdt", func(l *symbiosis.Layers) { l.Route.ApprovedTokens[1] = stranger }, symbiosis.ErrUnexpected},
		{"gateway spends more", "arb_usdt", func(l *symbiosis.Layers) { l.Route.Amount = big.NewInt(200_000_000) }, symbiosis.ErrMismatch},
		{"relayed elsewhere", "arb_usdt", func(l *symbiosis.Layers) { l.Route.RelayRecipient = stranger }, symbiosis.ErrUnexpected},

		// The first hop, Tron to the Symbiosis chain.
		{"portal locks more", "arb_usdt", func(l *symbiosis.Layers) { l.Synth.Amount = big.NewInt(200_000_000) }, symbiosis.ErrMismatch},
		{"portal locks another token", "arb_usdt", func(l *symbiosis.Layers) { l.Synth.Rtoken = stranger }, symbiosis.ErrUnexpected},
		{"leftovers to a stranger", "arb_usdt", func(l *symbiosis.Layers) { l.Synth.Chain2address = stranger }, symbiosis.ErrMismatch},
		{"first refund to a stranger", "arb_usdt", func(l *symbiosis.Layers) { l.Synth.SyntCaller = stranger }, symbiosis.ErrMismatch},
		{"first hop to another synthesis", "arb_usdt", func(l *symbiosis.Layers) { l.Synth.ReceiveSide = stranger }, symbiosis.ErrUnexpected},
		{"first hop via another bridge", "arb_usdt", func(l *symbiosis.Layers) { l.Synth.OppositeBridge = stranger }, symbiosis.ErrUnexpected},
		{"first hop to another chain", "arb_usdt", func(l *symbiosis.Layers) { l.Synth.ChainID = big.NewInt(42161) }, symbiosis.ErrUnexpected},
		{"first revert by a stranger", "arb_usdt", func(l *symbiosis.Layers) { l.Synth.RevertableAddress = stranger }, symbiosis.ErrUnexpected},
		{"first fee a tenth", "arb_usdt", func(l *symbiosis.Layers) { l.Synth.StableBridgingFee = big.NewInt(10_000_000) }, symbiosis.ErrMismatch},
		{"first fee a hair under a tenth", "arb_usdt", func(l *symbiosis.Layers) { l.Synth.StableBridgingFee = big.NewInt(9_999_999) }, nil},
		{"pool path of three", "arb_usdt", func(l *symbiosis.Layers) { l.Synth.SwapTokens = append(l.Synth.SwapTokens, stranger) }, symbiosis.ErrUnexpected},
		{"pool starts elsewhere", "arb_usdt", func(l *symbiosis.Layers) { l.Synth.SwapTokens[0] = stranger }, symbiosis.ErrUnexpected},
		{"unknown stable", "arb_usdt", func(l *symbiosis.Layers) { l.Synth.SwapTokens[1] = stranger }, symbiosis.ErrUnexpected},
		{"stable of another chain", "arb_usdt", func(l *symbiosis.Layers) {
			l.Synth.SwapTokens[1] = common.HexToAddress("0x076Cb7BEee7D1507DE7B964c29cbC849acaD5022")
		}, symbiosis.ErrMismatch},
		{"pool entered elsewhere", "arb_usdt", func(l *symbiosis.Layers) { l.Synth.SecondDexRouter = stranger }, symbiosis.ErrUnexpected},
		{"second hop started elsewhere", "arb_usdt", func(l *symbiosis.Layers) { l.Synth.FinalReceiveSide = stranger }, symbiosis.ErrUnexpected},
		{"second hop amount patched elsewhere", "arb_usdt", func(l *symbiosis.Layers) { l.Synth.FinalOffset = big.NewInt(68) }, symbiosis.ErrUnexpected},

		// The octopool swap on the Symbiosis chain.
		{"two pool calls", "arb_usdt", func(l *symbiosis.Layers) {
			l.Pool.Calldata = append(l.Pool.Calldata, []byte{1})
		}, symbiosis.ErrUnexpected},
		{"pool call to a stranger", "arb_usdt", func(l *symbiosis.Layers) { l.Pool.ReceiveSides[0] = stranger }, symbiosis.ErrUnexpected},
		{"pool amount patched elsewhere", "arb_usdt", func(l *symbiosis.Layers) { l.Pool.Offset[0] = big.NewInt(164) }, symbiosis.ErrUnexpected},
		{"pool path differs from the route's", "arb_usdt", func(l *symbiosis.Layers) { l.Pool.Path[1] = stranger }, symbiosis.ErrUnexpected},
		{"pool pays a stranger", "arb_usdt", func(l *symbiosis.Layers) { l.Pool.To = stranger }, symbiosis.ErrUnexpected},
		{"pool sells another asset", "arb_usdt", func(l *symbiosis.Layers) { l.Swap.FromID = big.NewInt(0) }, symbiosis.ErrUnexpected},
		{"pool buys another asset", "arb_usdt", func(l *symbiosis.Layers) { l.Swap.ToID = big.NewInt(5) }, symbiosis.ErrUnexpected},
		{"pool swap pays a stranger", "arb_usdt", func(l *symbiosis.Layers) { l.Swap.To = stranger }, symbiosis.ErrUnexpected},

		// The second hop, the Symbiosis chain to the destination.
		{"burns another synthetic", "arb_usdt", func(l *symbiosis.Layers) { l.Burn.SToken = stranger }, symbiosis.ErrUnexpected},
		{"destination pays a stranger", "arb_usdt", func(l *symbiosis.Layers) { l.Burn.Chain2address = stranger }, symbiosis.ErrMismatch},
		{"another destination chain", "arb_usdt", func(l *symbiosis.Layers) { l.Burn.ChainID = big.NewInt(56) }, symbiosis.ErrMismatch},
		{"another destination portal", "arb_usdt", func(l *symbiosis.Layers) { l.Burn.ReceiveSide = stranger }, symbiosis.ErrUnexpected},
		{"another destination bridge", "arb_usdt", func(l *symbiosis.Layers) { l.Burn.OppositeBridge = stranger }, symbiosis.ErrUnexpected},
		{"second refund to a stranger", "arb_usdt", func(l *symbiosis.Layers) { l.Burn.SyntCaller = stranger }, symbiosis.ErrUnexpected},
		{"second revert by a stranger", "arb_usdt", func(l *symbiosis.Layers) { l.Burn.RevertableAddress = stranger }, symbiosis.ErrUnexpected},
		{"second fee a tenth of the pool minimum", "arb_usdt", func(l *symbiosis.Layers) {
			// Rounded up: the bound is one tenth, not a hair under it.
			tenth := new(big.Int).Add(l.Swap.MinToAmount, big.NewInt(9))
			l.Burn.StableBridgingFee = tenth.Div(tenth, big.NewInt(10))
		}, symbiosis.ErrMismatch},
		{"second fee a hair under a tenth", "arb_usdt", func(l *symbiosis.Layers) {
			l.Burn.StableBridgingFee = new(big.Int).Div(l.Swap.MinToAmount, big.NewInt(10))
		}, nil},
		{"final swap through a stranger", "arb_usdt", func(l *symbiosis.Layers) { l.Burn.FinalReceiveSide = stranger }, symbiosis.ErrUnexpected},
		{"1inch amount patched elsewhere", "arb_usdt", func(l *symbiosis.Layers) { l.Burn.FinalOffset = big.NewInt(132) }, symbiosis.ErrUnexpected},
		{"0x amount patched elsewhere", "arb_eth", func(l *symbiosis.Layers) { l.Burn.FinalOffset = big.NewInt(196) }, symbiosis.ErrUnexpected},

		// A final swap through 1inch.
		{"1inch sells another token", "arb_usdt", func(l *symbiosis.Layers) { l.OneInch.Desc.SrcToken = stranger }, symbiosis.ErrUnexpected},
		{"1inch buys another token", "arb_usdt", func(l *symbiosis.Layers) { l.OneInch.Desc.DstToken = stranger }, symbiosis.ErrMismatch},
		{"1inch pays a stranger", "arb_usdt", func(l *symbiosis.Layers) { l.OneInch.Desc.DstReceiver = stranger }, symbiosis.ErrMismatch},
		{"1inch pays its caller", "arb_usdt", func(l *symbiosis.Layers) { l.OneInch.Desc.DstReceiver = common.Address{} }, symbiosis.ErrMismatch},
		{"1inch minimum lowered", "arb_usdt", func(l *symbiosis.Layers) { l.OneInch.Desc.MinReturnAmount = big.NewInt(1) }, symbiosis.ErrMismatch},
		{"1inch partial fill", "arb_usdt", func(l *symbiosis.Layers) { l.OneInch.Desc.Flags = big.NewInt(1) }, symbiosis.ErrUnexpected},

		// A final swap through 0x.
		{"0x sells another token", "arb_eth", func(l *symbiosis.Layers) { l.Exec.Token = stranger }, symbiosis.ErrUnexpected},
		{"0x draw by a stranger", "arb_eth", func(l *symbiosis.Layers) { l.Exec.Operator = stranger }, symbiosis.ErrUnexpected},
		{"0x call to a stranger", "arb_eth", func(l *symbiosis.Layers) { l.Exec.Operator, l.Exec.Target = stranger, stranger }, symbiosis.ErrUnexpected},
		{"0x pays a stranger", "arb_eth", func(l *symbiosis.Layers) { l.Settler.Slippage.Recipient = stranger }, symbiosis.ErrMismatch},
		{"0x buys another token", "arb_eth", func(l *symbiosis.Layers) { l.Settler.Slippage.BuyToken = stranger }, symbiosis.ErrMismatch},
		{"0x minimum lowered", "bsc_bnb", func(l *symbiosis.Layers) { l.Settler.Slippage.MinAmountOut = big.NewInt(1) }, symbiosis.ErrMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := fixtureNamed(t, tt.fixture)
			tx, want := load(t, f)
			tx = rewrite(t, tx, tt.edit)
			if _, err := symbiosis.Verify(tx, want, settlersFor(f.chainID)); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyRefusesAnotherEnvelope(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		edit    func(*symbiosis.Tx)
		wantErr error
	}{
		{"another contract", func(tx *symbiosis.Tx) { tx.To = other }, symbiosis.ErrUnexpected},
		{"approval to another contract", func(tx *symbiosis.Tx) { tx.ApproveTo = other }, symbiosis.ErrUnexpected},
		{"another sender", func(tx *symbiosis.Tx) { tx.From = other }, symbiosis.ErrMismatch},
		{"carries TRX", func(tx *symbiosis.Tx) { tx.Value = big.NewInt(1) }, symbiosis.ErrUnexpected},
		{"another function", func(tx *symbiosis.Tx) { tx.FunctionSelector = "metaRouterExecutorDontApprove()" }, symbiosis.ErrUnexpected},
		{"a word too many", func(tx *symbiosis.Tx) { tx.Data = append(tx.Data, make([]byte, 32)...) }, symbiosis.ErrMalformed},
		{"truncated", func(tx *symbiosis.Tx) { tx.Data = tx.Data[:len(tx.Data)-32] }, symbiosis.ErrMalformed},
		// The attack canonical decoding exists for: the same values, with the
		// tuple moved one word along, so that every offset at which a contract
		// patches an amount lands one word off.
		{"tuple moved", func(tx *symbiosis.Tx) {
			moved := slices.Concat(common.LeftPadBytes([]byte{0x40}, 32), make([]byte, 32), tx.Data[32:])
			tx.Data = moved
		}, symbiosis.ErrMalformed},
		// The same length and the same values, with a stray byte above the
		// relay recipient, which the ordinary decoder reads past: only the
		// comparison with the canonical bytes sees it.
		{"dirty address padding", func(tx *symbiosis.Tx) {
			tx.Data = slices.Clone(tx.Data)
			tx.Data[32+7*32] = 1
		}, symbiosis.ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := fixtureNamed(t, "arb_usdt")
			tx, want := load(t, f)
			tt.edit(&tx)
			if _, err := symbiosis.Verify(tx, want, settlersFor(f.chainID)); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// A moved tuple still decodes to exactly the values of the original under the
// ordinary ABI decoder, so what refuses it in Verify is the canonical check
// and not some value that happens to differ.
func TestMovedTupleDecodesToTheSameValues(t *testing.T) {
	t.Parallel()
	tx, _ := load(t, fixtureNamed(t, "arb_usdt"))
	gateway, err := abi.JSON(strings.NewReader(`[{"type":"function","name":"metaRoute","inputs":[{"name":"t","type":"tuple","components":[
		{"name":"a","type":"bytes"},{"name":"b","type":"bytes"},{"name":"c","type":"address[]"},{"name":"d","type":"address"},
		{"name":"e","type":"address"},{"name":"f","type":"uint256"},{"name":"g","type":"bool"},{"name":"h","type":"address"},
		{"name":"i","type":"bytes"}]}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	inputs := gateway.Methods["metaRoute"].Inputs
	original, err := inputs.Unpack(tx.Data)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := inputs.Unpack(slices.Concat(common.LeftPadBytes([]byte{0x40}, 32), make([]byte, 32), tx.Data[32:]))
	if err != nil {
		t.Fatalf("the ordinary decoder refused the moved tuple: %v", err)
	}
	if !reflect.DeepEqual(original, moved) {
		t.Fatal("the moved tuple decodes to other values; the envelope test proves nothing")
	}
}

func TestVerifyRefusesInnerCallsThatAreNotWhatTheyClaim(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		fixture string
		edit    func(*symbiosis.Layers)
		wantErr error
	}{
		{"other side is not metaSynthesize", "arb_usdt", func(l *symbiosis.Layers) {
			l.RawOtherSide = mustHex(t, "deadbeef")
		}, symbiosis.ErrUnexpected},
		{"other side shorter than a selector", "arb_usdt", func(l *symbiosis.Layers) {
			l.RawOtherSide = l.Route.OtherSideCalldata[:3]
		}, symbiosis.ErrUnexpected},
		{"other side with a word too many", "arb_usdt", func(l *symbiosis.Layers) {
			l.RawOtherSide = append(slices.Clone(l.Route.OtherSideCalldata), make([]byte, 32)...)
		}, symbiosis.ErrMalformed},
		{"pool entry is not a multicall", "arb_usdt", func(l *symbiosis.Layers) {
			l.RawPool = mustHex(t, "deadbeef")
		}, symbiosis.ErrUnexpected},
		{"pool call is not a swap", "arb_usdt", func(l *symbiosis.Layers) {
			l.RawPoolCall = mustHex(t, "deadbeef")
		}, symbiosis.ErrUnexpected},
		{"second hop is not a burn", "arb_usdt", func(l *symbiosis.Layers) {
			l.RawBurn = mustHex(t, "deadbeef")
		}, symbiosis.ErrUnexpected},
		{"1inch call with a word too many", "arb_usdt", func(l *symbiosis.Layers) {
			l.OneInch = nil
			l.Burn.FinalCallData = append(slices.Clone(l.Burn.FinalCallData), make([]byte, 32)...)
		}, symbiosis.ErrMalformed},
		{"0x call is not exec", "arb_eth", func(l *symbiosis.Layers) {
			l.Exec, l.Settler = nil, nil
			l.Burn.FinalCallData = mustHex(t, "deadbeef")
		}, symbiosis.ErrUnexpected},
		{"0x target is not execute", "arb_eth", func(l *symbiosis.Layers) {
			l.Settler = nil
			l.Exec.Data = mustHex(t, "deadbeef")
		}, symbiosis.ErrUnexpected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := fixtureNamed(t, tt.fixture)
			tx, want := load(t, f)
			tx = rewrite(t, tx, tt.edit)
			if _, err := symbiosis.Verify(tx, want, settlersFor(f.chainID)); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyDirectPayout(t *testing.T) {
	t.Parallel()
	f := fixtureNamed(t, "arb_usdt")
	tx, want := load(t, f)
	var minPooled, fee *big.Int
	tx = rewrite(t, tx, func(l *symbiosis.Layers) {
		l.OneInch = nil
		l.Burn.FinalCallData = nil
		l.Burn.FinalReceiveSide = common.Address{}
		minPooled, fee = l.Swap.MinToAmount, l.Burn.StableBridgingFee
	})
	wantOut := new(big.Int).Sub(minPooled, fee)

	t.Run("in the stable", func(t *testing.T) {
		t.Parallel()
		want := want
		want.Token, want.MinOut = arbitrumUSDC, wantOut
		route, err := symbiosis.Verify(tx, want, nil)
		if err != nil {
			t.Fatalf("Verify() error = %v", err)
		}
		if route.Venue != symbiosis.VenueDirect || route.MinOut.Cmp(wantOut) != 0 {
			t.Errorf("Venue, MinOut = %q, %s; want %q, %s", route.Venue, route.MinOut, symbiosis.VenueDirect, wantOut)
		}
	})
	t.Run("asked for more", func(t *testing.T) {
		t.Parallel()
		want := want
		want.Token, want.MinOut = arbitrumUSDC, new(big.Int).Add(wantOut, big.NewInt(1))
		if _, err := symbiosis.Verify(tx, want, nil); !errors.Is(err, symbiosis.ErrMismatch) {
			t.Fatalf("Verify() error = %v, want %v", err, symbiosis.ErrMismatch)
		}
	})
	t.Run("in another token", func(t *testing.T) {
		t.Parallel()
		if _, err := symbiosis.Verify(tx, want, nil); !errors.Is(err, symbiosis.ErrMismatch) {
			t.Fatalf("Verify() error = %v, want %v", err, symbiosis.ErrMismatch)
		}
	})
}

func TestVerifyComparesTheMinimumWithTheRequest(t *testing.T) {
	t.Parallel()
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			tx, want := load(t, f)
			want.MinOut.Add(want.MinOut, big.NewInt(1))
			if _, err := symbiosis.Verify(tx, want, settlersFor(f.chainID)); !errors.Is(err, symbiosis.ErrMismatch) {
				t.Fatalf("Verify() error = %v, want %v", err, symbiosis.ErrMismatch)
			}
		})
	}
}

func TestVerifyNeedsTheCurrentSettler(t *testing.T) {
	t.Parallel()
	f := fixtureNamed(t, "arb_eth")
	tests := []struct {
		name     string
		settlers []common.Address
		wantErr  error
	}{
		{"none", nil, symbiosis.ErrUnexpected},
		{"another chain's", settlersFor(56), symbiosis.ErrUnexpected},
		{"the zero address", []common.Address{{}}, symbiosis.ErrUnexpected},
		{"among others", []common.Address{stranger, settlers[42161]}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx, want := load(t, f)
			if _, err := symbiosis.Verify(tx, want, tt.settlers); !errors.Is(err, tt.wantErr) {
				t.Fatalf("Verify() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// A zero Settler from a registry that answered nothing must not let a route
// through whose exec names the zero address as well.
func TestVerifyRefusesAZeroSettlerOnBothSides(t *testing.T) {
	t.Parallel()
	f := fixtureNamed(t, "arb_eth")
	tx, want := load(t, f)
	tx = rewrite(t, tx, func(l *symbiosis.Layers) { l.Exec.Operator, l.Exec.Target = common.Address{}, common.Address{} })
	if _, err := symbiosis.Verify(tx, want, []common.Address{{}}); !errors.Is(err, symbiosis.ErrUnexpected) {
		t.Fatalf("Verify() error = %v, want %v", err, symbiosis.ErrUnexpected)
	}
}

func TestVerifyRefusesAnInvalidRequest(t *testing.T) {
	t.Parallel()
	f := fixtureNamed(t, "arb_usdt")
	tests := []struct {
		name string
		edit func(*symbiosis.Want)
	}{
		{"owner not an address", func(w *symbiosis.Want) { w.Owner = "T123" }},
		{"unsupported chain", func(w *symbiosis.Want) { w.ChainID = 10 }},
		{"no amount", func(w *symbiosis.Want) { w.Amount = nil }},
		{"zero amount", func(w *symbiosis.Want) { w.Amount = new(big.Int) }},
		{"no floor", func(w *symbiosis.Want) { w.MinOut = nil }},
		{"zero floor", func(w *symbiosis.Want) { w.MinOut = new(big.Int) }},
		{"no token", func(w *symbiosis.Want) { w.Token = common.Address{} }},
		{"no recipient", func(w *symbiosis.Want) { w.Recipient = common.Address{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tx, want := load(t, f)
			tt.edit(&want)
			_, err := symbiosis.Verify(tx, want, settlersFor(f.chainID))
			if err == nil {
				t.Fatal("Verify() accepted an invalid request")
			}
			// An invalid request is the caller's mistake, not a verdict on
			// the quote.
			for _, verdict := range []error{symbiosis.ErrMalformed, symbiosis.ErrUnexpected, symbiosis.ErrMismatch} {
				if errors.Is(err, verdict) {
					t.Errorf("Verify() error = %v, which blames the quote", err)
				}
			}
		})
	}
}

func TestVerifyRequestsAnotherAmount(t *testing.T) {
	t.Parallel()
	f := fixtureNamed(t, "arb_usdt")
	tx, want := load(t, f)
	want.Amount = big.NewInt(99_000_000)
	if _, err := symbiosis.Verify(tx, want, settlersFor(f.chainID)); !errors.Is(err, symbiosis.ErrMismatch) {
		t.Fatalf("Verify() error = %v, want %v", err, symbiosis.ErrMismatch)
	}
}

func TestVerifyRequestsAnotherChainOrToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(*symbiosis.Want)
	}{
		{"BSC instead of Arbitrum", func(w *symbiosis.Want) { w.ChainID = 56 }},
		{"the coin instead of USDT", func(w *symbiosis.Want) { w.Token = symbiosis.Native }},
		{"another recipient", func(w *symbiosis.Want) { w.Recipient = stranger }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := fixtureNamed(t, "arb_usdt")
			tx, want := load(t, f)
			tt.edit(&want)
			if _, err := symbiosis.Verify(tx, want, settlersFor(want.ChainID)); !errors.Is(err, symbiosis.ErrMismatch) {
				t.Fatalf("Verify() error = %v, want %v", err, symbiosis.ErrMismatch)
			}
		})
	}
}

func TestSettlerCalls(t *testing.T) {
	t.Parallel()
	current, previous := symbiosis.SettlerCalls()
	feature := common.LeftPadBytes([]byte{2}, 32)
	// The selectors the 0x registry answered on Arbitrum and BSC.
	if want := slices.Concat(mustHex(t, "6352211e"), feature); !bytes.Equal(current, want) {
		t.Errorf("current = %x, want %x", current, want)
	}
	if want := slices.Concat(mustHex(t, "e2603dc2"), feature); !bytes.Equal(previous, want) {
		t.Errorf("previous = %x, want %x", previous, want)
	}
}

func TestDecodeSettler(t *testing.T) {
	t.Parallel()
	got, err := symbiosis.DecodeSettler(mustHex(t, "00000000000000000000000093ea5e32e9c2be78a279be03f86b74917537df51"))
	if err != nil || got != settlers[42161] {
		t.Fatalf("DecodeSettler() = %s, %v; want %s", got.Hex(), err, settlers[42161].Hex())
	}
	if _, err := symbiosis.DecodeSettler(nil); err == nil {
		t.Fatal("DecodeSettler(nil) accepted an empty answer")
	}
}

func TestSupported(t *testing.T) {
	t.Parallel()
	for chainID, want := range map[uint64]bool{1: true, 56: true, 42161: true, 10: false, symbiosis.SymbiosisChainID: false} {
		if got := symbiosis.Supported(chainID); got != want {
			t.Errorf("Supported(%d) = %v, want %v", chainID, got, want)
		}
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
