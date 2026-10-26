package sunswap

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/sxwebdev/gotron/pkg/tronutils"
	"github.com/sxwebdev/walletspace/internal/evmabi"
)

// Calldata is a contract call encoded locally: the canonical signature the
// selector was derived from, and the complete bytes. The signature travels with
// the bytes because a node-side transaction builder asks for both, and the
// bytes are what a signed transaction is checked against.
type Calldata struct {
	Method string
	Data   []byte
}

// Words returns the arguments after the selector as 32-byte words.
//
// ABI encoding is a sequence of whole words whatever the argument types, so any
// call — tuples and bytes[] included — can be handed to a builder that only
// understands static types as that many bytes32 arguments under the real
// signature. The builder then produces the same bytes this package encoded.
func (c Calldata) Words() ([][32]byte, error) {
	if len(c.Data) < 4 || (len(c.Data)-4)%32 != 0 {
		return nil, fmt.Errorf("calldata of %d bytes is not a selector and whole words", len(c.Data))
	}
	words := make([][32]byte, (len(c.Data)-4)/32)
	for i := range words {
		copy(words[i][:], c.Data[4+32*i:])
	}
	return words, nil
}

// EVMAddress turns a base58check Tron address into the 20-byte form the ABI
// expects. DecodeCheck refuses anything but a 21-byte, 0x41-prefixed address
// with a valid checksum, so a malformed address never becomes the zero address
// in calldata.
func EVMAddress(addr string) (common.Address, error) {
	raw, err := tronutils.DecodeCheck(addr)
	if err != nil {
		return common.Address{}, fmt.Errorf("invalid Tron address %q: %w", addr, err)
	}
	return common.BytesToAddress(raw[1:]), nil
}

// TronAddress is the inverse of EVMAddress.
func TronAddress(addr common.Address) string {
	return tronutils.EncodeCheck(append([]byte{0x41}, addr.Bytes()...))
}

// The ABIs below are the slices of each contract this package calls, checked
// against the ABI each contract publishes on chain.
var (
	erc20ABI = mustABI(`[
		{"type":"function","name":"approve","inputs":[{"name":"spender","type":"address"},{"name":"amount","type":"uint256"}]},
		{"type":"function","name":"allowance","inputs":[{"name":"owner","type":"address"},{"name":"spender","type":"address"}],"outputs":[{"type":"uint256"}]}
	]`)
	permit2ABI = mustABI(`[
		{"type":"function","name":"approve","inputs":[{"name":"token","type":"address"},{"name":"spender","type":"address"},{"name":"amount","type":"uint160"},{"name":"expiration","type":"uint48"}]},
		{"type":"function","name":"allowance","inputs":[{"name":"owner","type":"address"},{"name":"token","type":"address"},{"name":"spender","type":"address"}],
		 "outputs":[{"name":"amount","type":"uint160"},{"name":"expiration","type":"uint48"},{"name":"nonce","type":"uint48"}]}
	]`)
	v2RouterABI = mustABI(`[
		{"type":"function","name":"getAmountsOut","inputs":[{"name":"amountIn","type":"uint256"},{"name":"path","type":"address[]"}],"outputs":[{"name":"amounts","type":"uint256[]"}]},
		{"type":"function","name":"swapExactETHForTokens","stateMutability":"payable","inputs":[{"name":"amountOutMin","type":"uint256"},{"name":"path","type":"address[]"},{"name":"to","type":"address"},{"name":"deadline","type":"uint256"}]},
		{"type":"function","name":"swapExactTokensForETH","inputs":[{"name":"amountIn","type":"uint256"},{"name":"amountOutMin","type":"uint256"},{"name":"path","type":"address[]"},{"name":"to","type":"address"},{"name":"deadline","type":"uint256"}]}
	]`)
	v3RouterABI = mustABI(`[
		{"type":"function","name":"exactInputSingle","stateMutability":"payable","inputs":[{"name":"params","type":"tuple","components":[
			{"name":"tokenIn","type":"address"},{"name":"tokenOut","type":"address"},{"name":"fee","type":"uint24"},
			{"name":"recipient","type":"address"},{"name":"deadline","type":"uint256"},{"name":"amountIn","type":"uint256"},
			{"name":"amountOutMinimum","type":"uint256"},{"name":"sqrtPriceLimitX96","type":"uint160"}]}]},
		{"type":"function","name":"unwrapWETH9","stateMutability":"payable","inputs":[{"name":"amountMinimum","type":"uint256"},{"name":"recipient","type":"address"}]},
		{"type":"function","name":"multicall","stateMutability":"payable","inputs":[{"name":"data","type":"bytes[]"}]}
	]`)
	v3QuoterABI = mustABI(`[
		{"type":"function","name":"quoteExactInputSingle","inputs":[{"name":"tokenIn","type":"address"},{"name":"tokenOut","type":"address"},{"name":"fee","type":"uint24"},{"name":"amountIn","type":"uint256"},{"name":"sqrtPriceLimitX96","type":"uint160"}],"outputs":[{"name":"amountOut","type":"uint256"}]}
	]`)
	v3PoolABI = mustABI(`[
		{"type":"function","name":"slot0","outputs":[{"name":"sqrtPriceX96","type":"uint160"},{"name":"tick","type":"int24"},{"name":"observationIndex","type":"uint16"},{"name":"observationCardinality","type":"uint16"},{"name":"observationCardinalityNext","type":"uint16"},{"name":"feeProtocol","type":"uint8"},{"name":"unlocked","type":"bool"}]},
		{"type":"function","name":"liquidity","outputs":[{"type":"uint128"}]},
		{"type":"function","name":"tickBitmap","inputs":[{"name":"wordPosition","type":"int16"}],"outputs":[{"type":"uint256"}]},
		{"type":"function","name":"ticks","inputs":[{"name":"tick","type":"int24"}],"outputs":[{"name":"liquidityGross","type":"uint128"},{"name":"liquidityNet","type":"int128"},{"name":"feeGrowthOutside0X128","type":"uint256"},{"name":"feeGrowthOutside1X128","type":"uint256"},{"name":"tickCumulativeOutside","type":"int56"},{"name":"secondsPerLiquidityOutsideX128","type":"uint160"},{"name":"secondsOutside","type":"uint32"},{"name":"initialized","type":"bool"}]}
	]`)
	v4PoolManagerABI = mustABI(`[
		{"type":"function","name":"getSlot0","inputs":[{"name":"id","type":"bytes32"}],"outputs":[{"name":"sqrtPriceX96","type":"uint160"},{"name":"tick","type":"int24"},{"name":"protocolFee","type":"uint24"},{"name":"lpFee","type":"uint24"}]},
		{"type":"function","name":"getLiquidity","inputs":[{"name":"id","type":"bytes32"}],"outputs":[{"type":"uint128"}]},
		{"type":"function","name":"getPoolBitmapInfo","inputs":[{"name":"id","type":"bytes32"},{"name":"word","type":"int16"}],"outputs":[{"type":"uint256"}]},
		{"type":"function","name":"getPoolTickInfo","inputs":[{"name":"id","type":"bytes32"},{"name":"tick","type":"int24"}],"outputs":[{"name":"info","type":"tuple","components":[{"name":"liquidityGross","type":"uint128"},{"name":"liquidityNet","type":"int128"},{"name":"feeGrowthOutside0X128","type":"uint256"},{"name":"feeGrowthOutside1X128","type":"uint256"}]}]}
	]`)
	universalRouterABI = mustABI(`[
		{"type":"function","name":"execute","stateMutability":"payable","inputs":[{"name":"commands","type":"bytes"},{"name":"inputs","type":"bytes[]"},{"name":"deadline","type":"uint256"}]}
	]`)
)

func mustABI(definition string) abi.ABI {
	return evmabi.MustParse("sunswap", definition)
}

func pack(contract abi.ABI, method string, args ...any) (Calldata, error) {
	data, err := contract.Pack(method, args...)
	if err != nil {
		return Calldata{}, fmt.Errorf("encode %s: %w", method, err)
	}
	return Calldata{Method: contract.Methods[method].Sig, Data: data}, nil
}

// unpack decodes a call's return data into its outputs.
func unpack(contract abi.ABI, method string, data []byte) ([]any, error) {
	values, err := contract.Unpack(method, data)
	if err != nil {
		return nil, fmt.Errorf("decode %s result: %w", method, err)
	}
	return values, nil
}

// Approve is ERC20 approve(spender, amount).
func Approve(spender common.Address, amount *big.Int) (Calldata, error) {
	return pack(erc20ABI, "approve", spender, amount)
}

// Allowance is ERC20 allowance(owner, spender).
func Allowance(owner, spender common.Address) (Calldata, error) {
	return pack(erc20ABI, "allowance", owner, spender)
}

// DecodeAllowance reads the answer to Allowance.
func DecodeAllowance(data []byte) (*big.Int, error) {
	values, err := unpack(erc20ABI, "allowance", data)
	if err != nil {
		return nil, err
	}
	return values[0].(*big.Int), nil
}

// Permit2Approve is Permit2 approve(token, spender, amount, expiration): an
// on-chain allowance from the caller to spender, drawn through Permit2.
func Permit2Approve(token, spender common.Address, amount *big.Int, expiration uint64) (Calldata, error) {
	if amount.Sign() <= 0 || amount.Cmp(maxUint160) > 0 {
		return Calldata{}, errors.New("Permit2 amount must fit in uint160")
	}
	if expiration >= 1<<48 {
		return Calldata{}, errors.New("Permit2 expiration must fit in uint48")
	}
	return pack(permit2ABI, "approve", token, spender, amount, new(big.Int).SetUint64(expiration))
}

// Permit2Allowance is Permit2 allowance(owner, token, spender).
func Permit2Allowance(owner, token, spender common.Address) (Calldata, error) {
	return pack(permit2ABI, "allowance", owner, token, spender)
}

// DecodePermit2Allowance reads the amount and expiration out of the answer to
// Permit2Allowance.
func DecodePermit2Allowance(data []byte) (*big.Int, uint64, error) {
	values, err := unpack(permit2ABI, "allowance", data)
	if err != nil {
		return nil, 0, err
	}
	return values[0].(*big.Int), values[1].(*big.Int).Uint64(), nil
}

// V2GetAmountsOut is the router's getAmountsOut(amountIn, path).
func V2GetAmountsOut(amountIn *big.Int, path []common.Address) (Calldata, error) {
	return pack(v2RouterABI, "getAmountsOut", amountIn, path)
}

// DecodeV2AmountsOut returns the last amount of a getAmountsOut answer: what
// the end of the path receives.
func DecodeV2AmountsOut(data []byte) (*big.Int, error) {
	values, err := unpack(v2RouterABI, "getAmountsOut", data)
	if err != nil {
		return nil, err
	}
	amounts := values[0].([]*big.Int)
	if len(amounts) < 2 {
		return nil, errors.New("getAmountsOut returned fewer than two amounts")
	}
	return amounts[len(amounts)-1], nil
}

// V2SwapExactETHForTokens spends exactly the call value.
func V2SwapExactETHForTokens(minOut *big.Int, path []common.Address, to common.Address, deadline int64) (Calldata, error) {
	return pack(v2RouterABI, "swapExactETHForTokens", minOut, path, to, big.NewInt(deadline))
}

// V2SwapExactTokensForETH pays the native coin out to to.
func V2SwapExactTokensForETH(amountIn, minOut *big.Int, path []common.Address, to common.Address, deadline int64) (Calldata, error) {
	return pack(v2RouterABI, "swapExactTokensForETH", amountIn, minOut, path, to, big.NewInt(deadline))
}

// V3ExactInputSingleParams mirrors ISwapRouter.ExactInputSingleParams of the
// original (deadline-carrying) SwapRouter, which is what SunSwap V3 runs.
type V3ExactInputSingleParams struct {
	TokenIn           common.Address
	TokenOut          common.Address
	Fee               *big.Int
	Recipient         common.Address
	Deadline          *big.Int
	AmountIn          *big.Int
	AmountOutMinimum  *big.Int
	SqrtPriceLimitX96 *big.Int
}

// V3ExactInputSingle swaps through a single V3 pool.
func V3ExactInputSingle(params V3ExactInputSingleParams) (Calldata, error) {
	return pack(v3RouterABI, "exactInputSingle", params)
}

// V3UnwrapWETH9 sends the router's whole WTRX balance to recipient as TRX.
func V3UnwrapWETH9(minimum *big.Int, recipient common.Address) (Calldata, error) {
	return pack(v3RouterABI, "unwrapWETH9", minimum, recipient)
}

// V3Multicall runs the calls in order inside one transaction.
func V3Multicall(calls ...Calldata) (Calldata, error) {
	data := make([][]byte, len(calls))
	for i, call := range calls {
		data[i] = call.Data
	}
	return pack(v3RouterABI, "multicall", data)
}

// V3QuoteExactInputSingle asks the V3 quoter what one pool pays for amountIn.
func V3QuoteExactInputSingle(tokenIn, tokenOut common.Address, fee uint32, amountIn *big.Int) (Calldata, error) {
	return pack(v3QuoterABI, "quoteExactInputSingle", tokenIn, tokenOut, new(big.Int).SetUint64(uint64(fee)), amountIn, new(big.Int))
}

// DecodeV3Quote reads the answer to V3QuoteExactInputSingle.
func DecodeV3Quote(data []byte) (*big.Int, error) {
	values, err := unpack(v3QuoterABI, "quoteExactInputSingle", data)
	if err != nil {
		return nil, err
	}
	return values[0].(*big.Int), nil
}

// PoolKey identifies a SunSwap V4 pool. Its layout is SunSwap's own: the
// PancakeSwap Infinity key without the pool manager, with the tick spacing
// carried in Parameters.
type PoolKey struct {
	Currency0  common.Address
	Currency1  common.Address
	Hooks      common.Address
	Fee        uint32
	Parameters [32]byte
}

// NewPoolKey builds a hookless key for a pool of the native coin (currency0,
// the zero address) and token with the given fee and tick spacing.
func NewPoolKey(token common.Address, fee uint32, tickSpacing int) PoolKey {
	var parameters [32]byte
	// The tick spacing sits above the 16-bit hooks-registration bitmap.
	big.NewInt(int64(tickSpacing) << 16).FillBytes(parameters[:])
	return PoolKey{Currency1: token, Fee: fee, Parameters: parameters}
}

// TickSpacing reads the tick spacing back out of Parameters.
func (k PoolKey) TickSpacing() int {
	v := new(big.Int).SetBytes(k.Parameters[:])
	return int(new(big.Int).And(new(big.Int).Rsh(v, 16), big.NewInt(0xffffff)).Int64())
}

var poolKeyArgs = abi.Arguments{
	{Type: mustType("address", nil)},
	{Type: mustType("address", nil)},
	{Type: mustType("address", nil)},
	{Type: mustType("uint24", nil)},
	{Type: mustType("bytes32", nil)},
}

// ID is keccak256(abi.encode(key)), the pool's identifier in the pool manager.
func (k PoolKey) ID() ([32]byte, error) {
	encoded, err := poolKeyArgs.Pack(k.Currency0, k.Currency1, k.Hooks, new(big.Int).SetUint64(uint64(k.Fee)), k.Parameters)
	if err != nil {
		// Unreachable: every field has a fixed Go type that packs. The error
		// is returned rather than panicked on so a future field cannot turn a
		// quote into a crash.
		return [32]byte{}, fmt.Errorf("encode pool key: %w", err)
	}
	return crypto.Keccak256Hash(encoded), nil
}

func mustType(name string, components []abi.ArgumentMarshaling) abi.Type {
	t, err := abi.NewType(name, "", components)
	if err != nil {
		panic("sunswap: bad ABI type " + name + ": " + err.Error())
	}
	return t
}

// V4GetSlot0 reads a V4 pool's price, tick and fees.
func V4GetSlot0(id [32]byte) (Calldata, error) { return pack(v4PoolManagerABI, "getSlot0", id) }

// V4Slot0 is the answer to V4GetSlot0.
type V4Slot0 struct {
	SqrtPriceX96 *big.Int
	Tick         int
	ProtocolFee  uint32
	LPFee        uint32
}

// DecodeV4Slot0 reads the answer to V4GetSlot0.
func DecodeV4Slot0(data []byte) (V4Slot0, error) {
	values, err := unpack(v4PoolManagerABI, "getSlot0", data)
	if err != nil {
		return V4Slot0{}, err
	}
	return V4Slot0{
		SqrtPriceX96: values[0].(*big.Int),
		Tick:         int(values[1].(*big.Int).Int64()),
		ProtocolFee:  uint32(values[2].(*big.Int).Uint64()),
		LPFee:        uint32(values[3].(*big.Int).Uint64()),
	}, nil
}

// V4GetLiquidity reads the liquidity in range at a V4 pool's current price.
func V4GetLiquidity(id [32]byte) (Calldata, error) {
	return pack(v4PoolManagerABI, "getLiquidity", id)
}

// DecodeV4Liquidity reads the answer to V4GetLiquidity.
func DecodeV4Liquidity(data []byte) (*big.Int, error) {
	values, err := unpack(v4PoolManagerABI, "getLiquidity", data)
	if err != nil {
		return nil, err
	}
	return values[0].(*big.Int), nil
}

// V4GetPoolBitmapInfo reads one word of a V4 pool's tick bitmap.
func V4GetPoolBitmapInfo(id [32]byte, word int16) (Calldata, error) {
	return pack(v4PoolManagerABI, "getPoolBitmapInfo", id, word)
}

// DecodeV4BitmapWord reads the answer to V4GetPoolBitmapInfo.
func DecodeV4BitmapWord(data []byte) (*big.Int, error) {
	values, err := unpack(v4PoolManagerABI, "getPoolBitmapInfo", data)
	if err != nil {
		return nil, err
	}
	return values[0].(*big.Int), nil
}

// V4GetPoolTickInfo reads one tick of a V4 pool.
func V4GetPoolTickInfo(id [32]byte, tick int) (Calldata, error) {
	return pack(v4PoolManagerABI, "getPoolTickInfo", id, big.NewInt(int64(tick)))
}

// DecodeV4LiquidityNet reads the net liquidity out of the answer to
// V4GetPoolTickInfo.
func DecodeV4LiquidityNet(data []byte) (*big.Int, error) {
	values, err := unpack(v4PoolManagerABI, "getPoolTickInfo", data)
	if err != nil {
		return nil, err
	}
	// Unpack has already checked the tuple against the ABI, so the conversion
	// cannot meet a shape it does not expect.
	info := abi.ConvertType(values[0], new(v4TickInfo)).(*v4TickInfo)
	return info.LiquidityNet, nil
}

type v4TickInfo struct {
	LiquidityGross        *big.Int
	LiquidityNet          *big.Int
	FeeGrowthOutside0X128 *big.Int
	FeeGrowthOutside1X128 *big.Int
}

// V3Slot0 reads a V3 pool's price and tick.
func V3Slot0() (Calldata, error) { return pack(v3PoolABI, "slot0") }

// DecodeV3Slot0 returns the price and tick of a slot0 answer.
func DecodeV3Slot0(data []byte) (*big.Int, int, error) {
	values, err := unpack(v3PoolABI, "slot0", data)
	if err != nil {
		return nil, 0, err
	}
	return values[0].(*big.Int), int(values[1].(*big.Int).Int64()), nil
}

// V3Liquidity reads a V3 pool's in-range liquidity.
func V3Liquidity() (Calldata, error) { return pack(v3PoolABI, "liquidity") }

// DecodeV3Liquidity reads the answer to V3Liquidity.
func DecodeV3Liquidity(data []byte) (*big.Int, error) {
	values, err := unpack(v3PoolABI, "liquidity", data)
	if err != nil {
		return nil, err
	}
	return values[0].(*big.Int), nil
}

// V3TickBitmap reads one word of a V3 pool's tick bitmap.
func V3TickBitmap(word int16) (Calldata, error) { return pack(v3PoolABI, "tickBitmap", word) }

// DecodeV3BitmapWord reads the answer to V3TickBitmap.
func DecodeV3BitmapWord(data []byte) (*big.Int, error) {
	values, err := unpack(v3PoolABI, "tickBitmap", data)
	if err != nil {
		return nil, err
	}
	return values[0].(*big.Int), nil
}

// V3Ticks reads one tick of a V3 pool.
func V3Ticks(tick int) (Calldata, error) { return pack(v3PoolABI, "ticks", big.NewInt(int64(tick))) }

// DecodeV3LiquidityNet reads the net liquidity out of a ticks answer.
func DecodeV3LiquidityNet(data []byte) (*big.Int, error) {
	values, err := unpack(v3PoolABI, "ticks", data)
	if err != nil {
		return nil, err
	}
	return values[1].(*big.Int), nil
}

// Universal router commands and V4 actions, as SunSwap's router SDK numbers
// them.
const (
	commandSweep  byte = 0x04
	commandV4Swap byte = 0x12

	actionCLSwapExactInSingle byte = 0x06
	actionSettle              byte = 0x0b
	actionTake                byte = 0x0e
)

var (
	// msgSender and addressThis are the router's placeholders for "the caller"
	// and "the router itself".
	msgSender   = common.BigToAddress(big.NewInt(1))
	addressThis = common.BigToAddress(big.NewInt(2))
	// openDelta asks an action to use whatever the pool manager currently owes
	// or is owed, rather than a fixed amount.
	openDelta = new(big.Int)
)

var (
	settleArgs = abi.Arguments{{Type: mustType("address", nil)}, {Type: mustType("uint256", nil)}, {Type: mustType("bool", nil)}}
	takeArgs   = abi.Arguments{{Type: mustType("address", nil)}, {Type: mustType("address", nil)}, {Type: mustType("uint256", nil)}}
	sweepArgs  = abi.Arguments{{Type: mustType("address", nil)}, {Type: mustType("address", nil)}, {Type: mustType("uint160", nil)}}
	v4SwapArgs = abi.Arguments{{Type: mustType("bytes", nil)}, {Type: mustType("bytes[]", nil)}}
	clSwapArgs = abi.Arguments{{Type: mustType("tuple", []abi.ArgumentMarshaling{
		{Name: "poolKey", Type: "tuple", Components: []abi.ArgumentMarshaling{
			{Name: "currency0", Type: "address"},
			{Name: "currency1", Type: "address"},
			{Name: "hooks", Type: "address"},
			{Name: "fee", Type: "uint24"},
			{Name: "parameters", Type: "bytes32"},
		}},
		{Name: "zeroForOne", Type: "bool"},
		{Name: "amountIn", Type: "uint128"},
		{Name: "amountOutMinimum", Type: "uint128"},
		{Name: "hookData", Type: "bytes"},
	})}}
)

type abiPoolKey struct {
	Currency0  common.Address
	Currency1  common.Address
	Hooks      common.Address
	Fee        *big.Int
	Parameters [32]byte
}

type abiCLSwapExactInputSingleParams struct {
	PoolKey          abiPoolKey
	ZeroForOne       bool
	AmountIn         *big.Int
	AmountOutMinimum *big.Int
	HookData         []byte
}

// V4ExactInputSingle is a universal router execute() that swaps amountIn
// through one V4 pool and sends at least minOut of the other currency to the
// caller.
//
// It is the sequence SunSwap's own router SDK emits for a single-pool trade:
// settle the input (pulled from the caller through Permit2 unless it is the
// native coin, which arrives as the call value), swap the open delta, take both
// currencies to the router, and sweep the output to the caller. The minimum is
// enforced by the sweep, which reverts below it.
func V4ExactInputSingle(key PoolKey, zeroForOne bool, amountIn, minOut *big.Int, deadline int64) (Calldata, error) {
	if minOut.Cmp(maxUint160) > 0 {
		return Calldata{}, errors.New("minimum output must fit in uint160")
	}
	currencyIn, currencyOut := key.Currency0, key.Currency1
	if !zeroForOne {
		currencyIn, currencyOut = key.Currency1, key.Currency0
	}
	payerIsUser := currencyIn != (common.Address{})

	settle, err := settleArgs.Pack(currencyIn, amountIn, payerIsUser)
	if err != nil {
		return Calldata{}, fmt.Errorf("encode settle: %w", err)
	}
	// Settle is the only pack that sees a caller's amount; the ones after it
	// carry fixed placeholders and amounts settle has already accepted, so
	// their error branches cannot be reached and stay as guards.
	swap, err := clSwapArgs.Pack(abiCLSwapExactInputSingleParams{
		PoolKey: abiPoolKey{
			Currency0: key.Currency0, Currency1: key.Currency1, Hooks: key.Hooks,
			Fee: new(big.Int).SetUint64(uint64(key.Fee)), Parameters: key.Parameters,
		},
		ZeroForOne: zeroForOne, AmountIn: openDelta, AmountOutMinimum: new(big.Int),
		HookData: []byte{},
	})
	if err != nil {
		return Calldata{}, fmt.Errorf("encode swap: %w", err)
	}
	takeOut, err := takeArgs.Pack(currencyOut, addressThis, openDelta)
	if err != nil {
		return Calldata{}, fmt.Errorf("encode take: %w", err)
	}
	takeIn, err := takeArgs.Pack(currencyIn, addressThis, openDelta)
	if err != nil {
		return Calldata{}, fmt.Errorf("encode take: %w", err)
	}
	actions := []byte{actionSettle, actionCLSwapExactInSingle, actionTake, actionTake}
	v4Swap, err := v4SwapArgs.Pack(actions, [][]byte{settle, swap, takeOut, takeIn})
	if err != nil {
		return Calldata{}, fmt.Errorf("encode V4 swap: %w", err)
	}
	sweep, err := sweepArgs.Pack(currencyOut, msgSender, minOut)
	if err != nil {
		return Calldata{}, fmt.Errorf("encode sweep: %w", err)
	}
	return pack(universalRouterABI, "execute",
		[]byte{commandV4Swap, commandSweep}, [][]byte{v4Swap, sweep}, big.NewInt(deadline))
}
