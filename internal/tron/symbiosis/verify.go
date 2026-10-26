package symbiosis

import (
	"errors"
	"fmt"
	"math/big"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/sxwebdev/gotron/pkg/tronutils"
)

// A rejected route wraps one of these, so a caller can tell a response that
// does not decode from one that goes somewhere unknown and from one that goes
// somewhere other than where the user asked. All three mean the same thing for
// the transaction: it is not signed.
var (
	// ErrMalformed is a transaction that is not the canonical encoding of the
	// calls it names.
	ErrMalformed = errors.New("symbiosis route is malformed")
	// ErrUnexpected is a route through a contract, a function or a shape this
	// package does not know. It may be an upgrade on Symbiosis's side; it is
	// refused all the same until the deployment here is updated to match.
	ErrUnexpected = errors.New("symbiosis route takes an unknown path")
	// ErrMismatch is a route that contradicts the request: another sender,
	// amount, recipient, chain or token, a lower minimum or an outsized fee.
	ErrMismatch = errors.New("symbiosis route does not match the request")
)

// Tx is the transaction a Symbiosis quote asks the wallet to sign, as its API
// sends it.
type Tx struct {
	// From and To are base58 Tron addresses.
	From string
	To   string
	// FunctionSelector is the signature of the called function, the way a Tron
	// node's transaction builder takes it.
	FunctionSelector string
	// Data is the ABI-encoded arguments, without the selector.
	Data []byte
	// Value is the TRX the call carries, in SUN; nil is none.
	Value *big.Int
	// ApproveTo is the contract the quote asks USDT to be approved to.
	ApproveTo string
}

// Want is what the user asked for. Nothing in it may come from the quote: it
// is what the quote is checked against.
type Want struct {
	// Owner is the Tron address that pays, in base58.
	Owner string
	// Amount is the USDT spent, in its base units.
	Amount *big.Int
	// ChainID is the EVM chain the route ends on.
	ChainID uint64
	// Token is what the recipient receives there: an ERC20, or Native.
	Token common.Address
	// Recipient receives Token on the destination.
	Recipient common.Address
	// MinOut is the least the recipient may receive, in Token's base units. It
	// must come from the wallet's own prices, never from the quote: a quote
	// that names its own floor can name a floor of nothing.
	MinOut *big.Int
}

// Venue is where a route's last swap happens.
type Venue string

const (
	// VenueDirect unlocks the stable on the destination and pays it out as it
	// is: the route ends in the token the user asked for.
	VenueDirect Venue = "direct"
	// VenueOneInch swaps the stable through the 1inch aggregation router.
	VenueOneInch Venue = "1inch"
	// VenueZeroEx swaps the stable through a 0x Settler.
	VenueZeroEx Venue = "0x"
)

// Route is a verified quote: the call to sign and what it promises.
type Route struct {
	// Method is the signature of the call, and Data the complete calldata,
	// selector included.
	Method string
	Data   []byte
	// TronFee is the USDT the Tron hop keeps, in USDT's base units.
	TronFee *big.Int
	// Stable is the token the route unlocks on the destination, and
	// DestinationFee what the second hop keeps of it, in its base units.
	Stable         common.Address
	DestinationFee *big.Int
	Venue          Venue
	// MinOut is the least the destination contracts let the recipient
	// receive; a swap that would pay less reverts there.
	MinOut *big.Int
}

// Byte offsets at which the contracts write the amount they hold into the call
// they make next. Each is the end of the amount word in the canonical encoding
// counted from the start of the bytes value, its length word included.
const (
	// metaBurnAmountOffset is MetaBurnTransaction.amount, after the selector,
	// the tuple's offset word and stableBridgingFee.
	metaBurnAmountOffset = 100
	// octopoolAmountOffset is the octopool swap's fromAmount, after the
	// selector, fromID and toID.
	octopoolAmountOffset = 100
	// oneInchAmountOffset is desc.amount, after the selector, executor and
	// the four addresses that open the static desc tuple.
	oneInchAmountOffset = 196
	// execAmountOffset is AllowanceHolder.exec's amount, after the selector,
	// operator and token.
	execAmountOffset = 100
)

// oneInchMaxTail is how much the 1inch API may append to its swap call: a
// source tag (four bytes in every quote seen), which the router ignores.
const oneInchMaxTail = 31

// maxFeeShare bounds each hop's bridging fee: no more than one part in ten of
// what the hop carries. The fee is Symbiosis's to set and is shown to the user
// before signing; the bound only stops a quote whose fee would swallow the
// transfer, since a fee is kept even when a transfer is reverted.
const maxFeeShare = 10

// settlerFeature is the 0x registry's feature id for the Settler that takers
// call through AllowanceHolder.
const settlerFeature = 2

// SettlerCalls returns the two calls to ZeroExDeployer on a destination that
// name the Settlers a route may end in: the current one (ownerOf) and the one
// it replaced (prev), which 0x keeps working for a while after a rotation so
// that quotes in flight still settle. Both answers are handed to Verify.
func SettlerCalls() (current, previous []byte) {
	feature := common.LeftPadBytes([]byte{settlerFeature}, 32)
	return slices.Concat(deployerABI.Methods["ownerOf"].ID, feature),
		slices.Concat(deployerABI.Methods["prev"].ID, feature)
}

// DecodeSettler reads the address out of the answer to either SettlerCalls
// call.
func DecodeSettler(result []byte) (common.Address, error) {
	values, err := deployerABI.Methods["ownerOf"].Outputs.Unpack(result)
	if err != nil {
		return common.Address{}, fmt.Errorf("decode Settler: %w", err)
	}
	return values[0].(common.Address), nil
}

// Verify checks a quote's transaction against what the user asked for and
// returns what it promises. settlers are the 0x Settlers current on the
// destination (see SettlerCalls); a route that ends in 0x is refused without
// them.
//
// What it establishes: the transaction spends exactly want.Amount of the
// owner's USDT through the pinned Tron contracts; every hop calls only pinned
// contracts with canonically encoded arguments; whatever the route pays out,
// in success or after a failed final swap, goes to want.Recipient on
// want.ChainID; the destination contracts revert a final swap that pays less
// than want.MinOut; and a revert of the first hop refunds the owner on Tron.
// What it cannot establish is that the Symbiosis relayers deliver: that, and
// a revert of the second hop, rest with Symbiosis (see operators).
func Verify(tx Tx, want Want, settlers []common.Address) (Route, error) {
	owner, dest, err := want.check()
	if err != nil {
		return Route{}, err
	}

	method := gatewayABI.Methods["metaRoute"]
	switch {
	case tx.To != Gateway:
		return Route{}, fmt.Errorf("%w: the transaction calls %s, not the Symbiosis gateway", ErrUnexpected, tx.To)
	case tx.ApproveTo != Gateway:
		return Route{}, fmt.Errorf("%w: the quote asks for an approval to %s, not to the Symbiosis gateway", ErrUnexpected, tx.ApproveTo)
	case tx.From != want.Owner:
		return Route{}, fmt.Errorf("%w: the transaction is sent from %s, not %s", ErrMismatch, tx.From, want.Owner)
	case tx.Value != nil && tx.Value.Sign() != 0:
		return Route{}, fmt.Errorf("%w: the transaction carries %s SUN; a USDT route carries none", ErrUnexpected, tx.Value)
	case tx.FunctionSelector != method.Sig:
		return Route{}, fmt.Errorf("%w: the transaction calls %q", ErrUnexpected, tx.FunctionSelector)
	}

	data := slices.Concat(method.ID, tx.Data)
	var route metaRouteTx
	if err := decode(gatewayABI, "metaRoute", data, &route); err != nil {
		return Route{}, err
	}
	if err := checkMetaRoute(route, want); err != nil {
		return Route{}, err
	}

	var synth metaSynthesizeTx
	if err := decode(portalABI, "metaSynthesize", route.OtherSideCalldata, &synth); err != nil {
		return Route{}, err
	}
	stable, minPooled, err := checkSynthesize(synth, want, owner, dest)
	if err != nil {
		return Route{}, err
	}

	var burn metaBurnTx
	if err := decode(synthesisABI, "metaBurnSyntheticToken", synth.FinalCalldata, &burn); err != nil {
		return Route{}, err
	}
	venue, minOut, err := checkBurn(burn, want, dest, stable, minPooled, settlers)
	if err != nil {
		return Route{}, err
	}
	if minOut.Cmp(want.MinOut) < 0 {
		return Route{}, fmt.Errorf("%w: the route guarantees %s, less than the %s asked for", ErrMismatch, minOut, want.MinOut)
	}

	return Route{
		Method:         method.Sig,
		Data:           data,
		TronFee:        synth.StableBridgingFee,
		Stable:         stable.Real,
		DestinationFee: burn.StableBridgingFee,
		Venue:          venue,
		MinOut:         minOut,
	}, nil
}

// check validates the request itself and resolves what it names.
func (w Want) check() (common.Address, destination, error) {
	owner, err := evmAddress(w.Owner)
	if err != nil {
		return common.Address{}, destination{}, err
	}
	dest, ok := destinations[w.ChainID]
	switch {
	case !ok:
		return common.Address{}, destination{}, fmt.Errorf("routes to chain %d are not supported", w.ChainID)
	case w.Amount == nil || w.Amount.Sign() <= 0:
		return common.Address{}, destination{}, errors.New("the amount must be positive")
	case w.MinOut == nil || w.MinOut.Sign() <= 0:
		// A zero floor would accept a route that pays nothing.
		return common.Address{}, destination{}, errors.New("the minimum received must be positive")
	case w.Token == (common.Address{}):
		return common.Address{}, destination{}, errors.New("the token received is not set")
	case w.Recipient == (common.Address{}):
		return common.Address{}, destination{}, errors.New("the recipient is not set")
	}
	return owner, dest, nil
}

// checkMetaRoute checks the call the Tron gateway makes: claim the owner's
// USDT and hand all of it to the Tron portal, with no swap on Tron.
func checkMetaRoute(r metaRouteTx, want Want) error {
	switch {
	case len(r.FirstSwapCalldata) != 0 || len(r.SecondSwapCalldata) != 0:
		return fmt.Errorf("%w: the route swaps on Tron", ErrUnexpected)
	case r.NativeIn:
		return fmt.Errorf("%w: the route spends TRX", ErrUnexpected)
	case len(r.ApprovedTokens) == 0 || slices.ContainsFunc(r.ApprovedTokens, func(a common.Address) bool { return a != tronUSDT }):
		return fmt.Errorf("%w: the route moves tokens other than USDT: %v", ErrUnexpected, r.ApprovedTokens)
	case r.Amount.Cmp(want.Amount) != 0:
		return fmt.Errorf("%w: the route spends %s USDT units, not %s", ErrMismatch, r.Amount, want.Amount)
	case r.RelayRecipient != tronPortal:
		return fmt.Errorf("%w: the route hands the USDT to %s, not the Symbiosis portal", ErrUnexpected, r.RelayRecipient.Hex())
	}
	return nil
}

// checkSynthesize checks the request the Tron portal relays to the Symbiosis
// chain: mint the synthetic, swap it in the octopool for the destination's
// stable, burn that toward the destination. It returns the stable and the
// least the octopool swap may yield.
func checkSynthesize(s metaSynthesizeTx, want Want, owner common.Address, dest destination) (synth, *big.Int, error) {
	switch {
	case s.Amount.Cmp(want.Amount) != 0:
		return synth{}, nil, fmt.Errorf("%w: the portal locks %s USDT units, not %s", ErrMismatch, s.Amount, want.Amount)
	case s.Rtoken != tronUSDT:
		return synth{}, nil, fmt.Errorf("%w: the portal locks %s, not USDT", ErrUnexpected, s.Rtoken.Hex())
	case s.Chain2address != want.Recipient:
		// metaMintSwap pays anything left on the Symbiosis chain to it.
		return synth{}, nil, fmt.Errorf("%w: the first hop pays leftovers to %s", ErrMismatch, s.Chain2address.Hex())
	case s.SyntCaller != owner:
		// A reverted first hop unlocks the USDT on Tron to it.
		return synth{}, nil, fmt.Errorf("%w: a revert of the first hop would refund %s", ErrMismatch, s.SyntCaller.Hex())
	case s.ReceiveSide != synthesis || s.OppositeBridge != bridge || !is(s.ChainID, SymbiosisChainID):
		return synth{}, nil, fmt.Errorf("%w: the first hop goes to %s via %s on chain %s",
			ErrUnexpected, s.ReceiveSide.Hex(), s.OppositeBridge.Hex(), s.ChainID)
	case !slices.Contains(operators, s.RevertableAddress):
		return synth{}, nil, fmt.Errorf("%w: the first hop may be reverted by %s", ErrUnexpected, s.RevertableAddress.Hex())
	case outsized(s.StableBridgingFee, s.Amount):
		return synth{}, nil, fmt.Errorf("%w: the first hop keeps %s of %s", ErrMismatch, s.StableBridgingFee, s.Amount)
	case len(s.SwapTokens) != 2 || s.SwapTokens[0] != tronUSDTSynth.Token:
		return synth{}, nil, fmt.Errorf("%w: the octopool swap goes through %v", ErrUnexpected, s.SwapTokens)
	case s.SecondDexRouter != multicall:
		return synth{}, nil, fmt.Errorf("%w: the octopool swap is entered through %s", ErrUnexpected, s.SecondDexRouter.Hex())
	case s.FinalReceiveSide != synthesis || !is(s.FinalOffset, metaBurnAmountOffset):
		return synth{}, nil, fmt.Errorf("%w: the second hop is started by %s at offset %s",
			ErrUnexpected, s.FinalReceiveSide.Hex(), s.FinalOffset)
	}
	stable, ok := dest.stable(s.SwapTokens[1])
	if !ok {
		for chainID, other := range destinations {
			if _, ok := other.stable(s.SwapTokens[1]); ok {
				return synth{}, nil, fmt.Errorf("%w: the route unlocks its stable on chain %d", ErrMismatch, chainID)
			}
		}
		return synth{}, nil, fmt.Errorf("%w: the route unlocks %s on the destination", ErrUnexpected, s.SwapTokens[1].Hex())
	}
	minPooled, err := checkPoolSwap(s.SecondSwapCalldata, stable)
	if err != nil {
		return synth{}, nil, err
	}
	return stable, minPooled, nil
}

// checkPoolSwap checks the multicall that swaps the Tron synthetic for the
// destination's in the octopool and hands the result to the MetaRouter, which
// burns it toward the destination. It returns the swap's minimum.
func checkPoolSwap(data []byte, stable synth) (*big.Int, error) {
	var call multicallArgs
	if err := decode(multicallABI, "multicall", data, &call); err != nil {
		return nil, err
	}
	switch {
	case len(call.Calldata) != 1 || len(call.ReceiveSides) != 1 || len(call.Offset) != 1:
		return nil, fmt.Errorf("%w: the Symbiosis-chain multicall makes %d calls", ErrUnexpected, len(call.Calldata))
	case call.ReceiveSides[0] != octopool || !is(call.Offset[0], octopoolAmountOffset):
		return nil, fmt.Errorf("%w: the Symbiosis-chain swap calls %s at offset %s",
			ErrUnexpected, call.ReceiveSides[0].Hex(), call.Offset[0])
	case !slices.Equal(call.Path, []common.Address{tronUSDTSynth.Token, stable.Token}):
		return nil, fmt.Errorf("%w: the Symbiosis-chain swap goes through %v", ErrUnexpected, call.Path)
	case call.To != metaRouter:
		// The MetaRouter burns what it holds; anyone else would just keep it.
		return nil, fmt.Errorf("%w: the Symbiosis-chain swap pays %s", ErrUnexpected, call.To.Hex())
	}

	var swap octopoolSwapArgs
	if err := decode(octopoolABI, "swap", call.Calldata[0], &swap); err != nil {
		return nil, err
	}
	switch {
	case !is(swap.FromID, tronUSDTSynth.PoolIndex) || !is(swap.ToID, stable.PoolIndex):
		// A swap into another asset would leave it with the multicall router,
		// where the next caller can sweep it.
		return nil, fmt.Errorf("%w: the octopool swap trades asset %s for %s", ErrUnexpected, swap.FromID, swap.ToID)
	case swap.To != multicall:
		return nil, fmt.Errorf("%w: the octopool swap pays %s", ErrUnexpected, swap.To.Hex())
	}
	return swap.MinToAmount, nil
}

// checkBurn checks the request the Symbiosis chain relays to the destination
// and the final swap there. It returns where that swap happens and the least
// the recipient receives.
func checkBurn(b metaBurnTx, want Want, dest destination, stable synth, minPooled *big.Int, settlers []common.Address) (Venue, *big.Int, error) {
	switch {
	case b.SToken != stable.Token:
		return "", nil, fmt.Errorf("%w: the second hop burns %s, not what the swap bought", ErrUnexpected, b.SToken.Hex())
	case b.Chain2address != want.Recipient:
		// The destination portal pays to it, directly or after a failed swap.
		return "", nil, fmt.Errorf("%w: the destination pays %s", ErrMismatch, b.Chain2address.Hex())
	case !is(b.ChainID, want.ChainID):
		return "", nil, fmt.Errorf("%w: the second hop goes to chain %s, not %d", ErrMismatch, b.ChainID, want.ChainID)
	case b.ReceiveSide != dest.Portal || b.OppositeBridge != dest.Bridge:
		return "", nil, fmt.Errorf("%w: the second hop goes to %s via %s", ErrUnexpected, b.ReceiveSide.Hex(), b.OppositeBridge.Hex())
	case !slices.Contains(operators, b.SyntCaller) || !slices.Contains(operators, b.RevertableAddress):
		return "", nil, fmt.Errorf("%w: a revert of the second hop would go to %s on the request of %s",
			ErrUnexpected, b.SyntCaller.Hex(), b.RevertableAddress.Hex())
	case outsized(b.StableBridgingFee, minPooled):
		return "", nil, fmt.Errorf("%w: the second hop keeps %s of at least %s", ErrMismatch, b.StableBridgingFee, minPooled)
	}

	switch {
	case len(b.FinalCallData) == 0:
		if want.Token != stable.Real {
			return "", nil, fmt.Errorf("%w: the route pays out %s, not %s", ErrMismatch, stable.Real.Hex(), want.Token.Hex())
		}
		return VenueDirect, new(big.Int).Sub(minPooled, b.StableBridgingFee), nil
	case b.FinalReceiveSide == oneInchRouter && is(b.FinalOffset, oneInchAmountOffset):
		minOut, err := checkOneInch(b.FinalCallData, want, stable)
		return VenueOneInch, minOut, err
	case b.FinalReceiveSide == dest.AllowanceHolder && is(b.FinalOffset, execAmountOffset):
		minOut, err := checkZeroEx(b.FinalCallData, want, stable, settlers)
		return VenueZeroEx, minOut, err
	}
	return "", nil, fmt.Errorf("%w: the final swap calls %s at offset %s", ErrUnexpected, b.FinalReceiveSide.Hex(), b.FinalOffset)
}

// checkOneInch checks a final swap through the 1inch router. The router pays
// desc.dstReceiver and reverts below desc.minReturnAmount whatever its
// executor and data do, so those two are all that has to be right.
func checkOneInch(data []byte, want Want, stable synth) (*big.Int, error) {
	var call oneInchSwapArgs
	if err := decodeTail(oneInchABI, "swap", data, &call, oneInchMaxTail); err != nil {
		return nil, err
	}
	switch d := call.Desc; {
	case d.SrcToken != stable.Real:
		return nil, fmt.Errorf("%w: the 1inch swap sells %s", ErrUnexpected, d.SrcToken.Hex())
	case d.DstToken != want.Token:
		return nil, fmt.Errorf("%w: the 1inch swap buys %s, not %s", ErrMismatch, d.DstToken.Hex(), want.Token.Hex())
	case d.DstReceiver != want.Recipient:
		return nil, fmt.Errorf("%w: the 1inch swap pays %s", ErrMismatch, d.DstReceiver.Hex())
	case d.Flags.Sign() != 0:
		// Flags switch on partial fills and permits, none of which a route
		// needs; refusing them keeps the minimum meaning what it says.
		return nil, fmt.Errorf("%w: the 1inch swap sets flags %s", ErrUnexpected, d.Flags)
	}
	return call.Desc.MinReturnAmount, nil
}

// checkZeroEx checks a final swap through 0x. AllowanceHolder lets the operator
// draw the stable for the length of one call to the target, so both must be a
// genuine Settler: anything else could draw it and return. The Settler then
// pays slippage.recipient and reverts below slippage.minAmountOut whatever its
// actions do.
func checkZeroEx(data []byte, want Want, stable synth, settlers []common.Address) (*big.Int, error) {
	var call execArgs
	if err := decode(allowanceHolderABI, "exec", data, &call); err != nil {
		return nil, err
	}
	switch {
	case call.Token != stable.Real:
		return nil, fmt.Errorf("%w: the 0x swap sells %s", ErrUnexpected, call.Token.Hex())
	case call.Operator != call.Target || !slices.Contains(settlers, call.Target) || call.Target == (common.Address{}):
		return nil, fmt.Errorf("%w: the 0x swap lets %s draw the stable for %s, not a current Settler",
			ErrUnexpected, call.Operator.Hex(), call.Target.Hex())
	}
	var exec settlerExecuteArgs
	if err := decode(settlerABI, "execute", call.Data, &exec); err != nil {
		return nil, err
	}
	switch s := exec.Slippage; {
	case s.Recipient != want.Recipient:
		return nil, fmt.Errorf("%w: the 0x swap pays %s", ErrMismatch, s.Recipient.Hex())
	case s.BuyToken != want.Token:
		return nil, fmt.Errorf("%w: the 0x swap buys %s, not %s", ErrMismatch, s.BuyToken.Hex(), want.Token.Hex())
	}
	return exec.Slippage.MinAmountOut, nil
}

func (d destination) stable(token common.Address) (synth, bool) {
	i := slices.IndexFunc(d.Stables, func(s synth) bool { return s.Token == token })
	if i < 0 {
		return synth{}, false
	}
	return d.Stables[i], true
}

// outsized reports a fee that is not below one part in maxFeeShare of base.
func outsized(fee, base *big.Int) bool {
	return new(big.Int).Mul(fee, big.NewInt(maxFeeShare)).Cmp(base) >= 0
}

// is compares a decoded uint256 with a small constant.
func is(got *big.Int, want uint64) bool {
	return got.IsUint64() && got.Uint64() == want
}

// evmAddress turns a base58check Tron address into the 20 bytes the ABI holds.
func evmAddress(addr string) (common.Address, error) {
	raw, err := tronutils.DecodeCheck(addr)
	if err != nil {
		return common.Address{}, fmt.Errorf("invalid Tron address %q: %w", addr, err)
	}
	return common.BytesToAddress(raw[1:]), nil
}
