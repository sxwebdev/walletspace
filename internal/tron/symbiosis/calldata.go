package symbiosis

import (
	"bytes"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/sxwebdev/walletspace/internal/evmabi"
)

// The ABIs below are the slices of each contract a route calls. The Symbiosis
// structs are copied from MetaRouteStructs.sol in symbiosis-finance/core-contracts;
// the component names matter, because go-ethereum maps them onto the fields of
// the structs that follow.
var (
	gatewayABI = mustABI(`[
		{"type":"function","name":"metaRouterExecutorDontApprove","outputs":[{"type":"address"}]},
		{"type":"function","name":"metaRoute","stateMutability":"payable","inputs":[{"name":"t","type":"tuple","components":[
			{"name":"firstSwapCalldata","type":"bytes"},{"name":"secondSwapCalldata","type":"bytes"},
			{"name":"approvedTokens","type":"address[]"},{"name":"firstDexRouter","type":"address"},
			{"name":"secondDexRouter","type":"address"},{"name":"amount","type":"uint256"},
			{"name":"nativeIn","type":"bool"},{"name":"relayRecipient","type":"address"},
			{"name":"otherSideCalldata","type":"bytes"}]}]}
	]`)
	portalABI = mustABI(`[
		{"type":"function","name":"metaSynthesize","inputs":[{"name":"t","type":"tuple","components":[
			{"name":"stableBridgingFee","type":"uint256"},{"name":"amount","type":"uint256"},
			{"name":"rtoken","type":"address"},{"name":"chain2address","type":"address"},
			{"name":"receiveSide","type":"address"},{"name":"oppositeBridge","type":"address"},
			{"name":"syntCaller","type":"address"},{"name":"chainID","type":"uint256"},
			{"name":"swapTokens","type":"address[]"},{"name":"secondDexRouter","type":"address"},
			{"name":"secondSwapCalldata","type":"bytes"},{"name":"finalReceiveSide","type":"address"},
			{"name":"finalCalldata","type":"bytes"},{"name":"finalOffset","type":"uint256"},
			{"name":"revertableAddress","type":"address"},{"name":"clientID","type":"bytes32"}]}]}
	]`)
	synthesisABI = mustABI(`[
		{"type":"function","name":"metaBurnSyntheticToken","inputs":[{"name":"t","type":"tuple","components":[
			{"name":"stableBridgingFee","type":"uint256"},{"name":"amount","type":"uint256"},
			{"name":"crossChainID","type":"bytes32"},{"name":"syntCaller","type":"address"},
			{"name":"finalReceiveSide","type":"address"},{"name":"sToken","type":"address"},
			{"name":"finalCallData","type":"bytes"},{"name":"finalOffset","type":"uint256"},
			{"name":"chain2address","type":"address"},{"name":"receiveSide","type":"address"},
			{"name":"oppositeBridge","type":"address"},{"name":"revertableAddress","type":"address"},
			{"name":"chainID","type":"uint256"},{"name":"clientID","type":"bytes32"}]}]}
	]`)
	multicallABI = mustABI(`[
		{"type":"function","name":"multicall","inputs":[{"name":"amountIn","type":"uint256"},{"name":"calldata","type":"bytes[]"},
			{"name":"receiveSides","type":"address[]"},{"name":"path","type":"address[]"},
			{"name":"offset","type":"uint256[]"},{"name":"to","type":"address"}]}
	]`)
	octopoolABI = mustABI(`[
		{"type":"function","name":"swap","inputs":[{"name":"fromID","type":"uint256"},{"name":"toID","type":"uint256"},
			{"name":"fromAmount","type":"uint256"},{"name":"minToAmount","type":"uint256"},
			{"name":"to","type":"address"},{"name":"deadline","type":"uint256"}]}
	]`)
	oneInchABI = mustABI(`[
		{"type":"function","name":"swap","stateMutability":"payable","inputs":[{"name":"executor","type":"address"},
			{"name":"desc","type":"tuple","components":[
				{"name":"srcToken","type":"address"},{"name":"dstToken","type":"address"},
				{"name":"srcReceiver","type":"address"},{"name":"dstReceiver","type":"address"},
				{"name":"amount","type":"uint256"},{"name":"minReturnAmount","type":"uint256"},
				{"name":"flags","type":"uint256"}]},
			{"name":"data","type":"bytes"}]}
	]`)
	allowanceHolderABI = mustABI(`[
		{"type":"function","name":"exec","stateMutability":"payable","inputs":[{"name":"operator","type":"address"},
			{"name":"token","type":"address"},{"name":"amount","type":"uint256"},
			{"name":"target","type":"address"},{"name":"data","type":"bytes"}]}
	]`)
	settlerABI = mustABI(`[
		{"type":"function","name":"execute","stateMutability":"payable","inputs":[
			{"name":"slippage","type":"tuple","components":[
				{"name":"recipient","type":"address"},{"name":"buyToken","type":"address"},
				{"name":"minAmountOut","type":"uint256"}]},
			{"name":"actions","type":"bytes[]"},{"name":"zid","type":"bytes32"}]}
	]`)
	deployerABI = mustABI(`[
		{"type":"function","name":"ownerOf","inputs":[{"name":"feature","type":"uint256"}],"outputs":[{"type":"address"}]},
		{"type":"function","name":"prev","inputs":[{"name":"feature","type":"uint128"}],"outputs":[{"type":"address"}]}
	]`)
)

func mustABI(definition string) abi.ABI {
	return evmabi.MustParse("symbiosis", definition)
}

type metaRouteTx struct {
	FirstSwapCalldata  []byte
	SecondSwapCalldata []byte
	ApprovedTokens     []common.Address
	FirstDexRouter     common.Address
	SecondDexRouter    common.Address
	Amount             *big.Int
	NativeIn           bool
	RelayRecipient     common.Address
	OtherSideCalldata  []byte
}

type metaSynthesizeTx struct {
	StableBridgingFee  *big.Int
	Amount             *big.Int
	Rtoken             common.Address
	Chain2address      common.Address
	ReceiveSide        common.Address
	OppositeBridge     common.Address
	SyntCaller         common.Address
	ChainID            *big.Int
	SwapTokens         []common.Address
	SecondDexRouter    common.Address
	SecondSwapCalldata []byte
	FinalReceiveSide   common.Address
	FinalCalldata      []byte
	FinalOffset        *big.Int
	RevertableAddress  common.Address
	ClientID           [32]byte
}

type metaBurnTx struct {
	StableBridgingFee *big.Int
	Amount            *big.Int
	CrossChainID      [32]byte
	SyntCaller        common.Address
	FinalReceiveSide  common.Address
	SToken            common.Address
	FinalCallData     []byte
	FinalOffset       *big.Int
	Chain2address     common.Address
	ReceiveSide       common.Address
	OppositeBridge    common.Address
	RevertableAddress common.Address
	ChainID           *big.Int
	ClientID          [32]byte
}

type multicallArgs struct {
	AmountIn     *big.Int
	Calldata     [][]byte
	ReceiveSides []common.Address
	Path         []common.Address
	Offset       []*big.Int
	To           common.Address
}

type octopoolSwapArgs struct {
	FromID      *big.Int
	ToID        *big.Int
	FromAmount  *big.Int
	MinToAmount *big.Int
	To          common.Address
	Deadline    *big.Int
}

type oneInchDesc struct {
	SrcToken        common.Address
	DstToken        common.Address
	SrcReceiver     common.Address
	DstReceiver     common.Address
	Amount          *big.Int
	MinReturnAmount *big.Int
	Flags           *big.Int
}

type oneInchSwapArgs struct {
	Executor common.Address
	Desc     oneInchDesc
	Data     []byte
}

type execArgs struct {
	Operator common.Address
	Token    common.Address
	Amount   *big.Int
	Target   common.Address
	Data     []byte
}

type allowedSlippage struct {
	Recipient    common.Address
	BuyToken     common.Address
	MinAmountOut *big.Int
}

type settlerExecuteArgs struct {
	Slippage allowedSlippage
	Actions  [][]byte
	Zid      [32]byte
}

// decode unpacks a call to method into args, a pointer to a struct whose
// fields are named after the method's inputs — or, for a method taking a
// single tuple, after the tuple's components. It accepts only the canonical
// encoding: the bytes must be exactly what packing the decoded arguments
// produces.
//
// That is not pedantry. The contracts along a route patch the amount they
// actually hold into these calls at fixed byte offsets, which land on the
// amount field only when every dynamic argument sits where the canonical
// encoding puts it. A call with its tuple moved elsewhere decodes to the same
// values, and then has its recipient or its minimum overwritten on chain.
func decode(contract abi.ABI, method string, data []byte, args any) error {
	return decodeTail(contract, method, data, args, 0)
}

// decodeTail is decode for a call that may carry up to maxTail bytes after its
// canonical encoding. Bytes past the arguments move no offset and the ABI
// decoder of the called contract never reads them, so a tail is harmless where
// the contract does not look at its raw calldata; it is still bounded, so it
// cannot hide a word.
func decodeTail(contract abi.ABI, method string, data []byte, args any, maxTail int) error {
	m := contract.Methods[method]
	if len(data) < 4 || !bytes.Equal(data[:4], m.ID) {
		return fmt.Errorf("%w: expected a call to %s, got %d bytes starting %x",
			ErrUnexpected, m.Sig, len(data), data[:min(4, len(data))])
	}
	values, err := m.Inputs.Unpack(data[4:])
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrMalformed, m.Sig, err)
	}
	repacked, err := m.Inputs.Pack(values...)
	tail := len(data) - 4 - len(repacked)
	if err != nil || tail < 0 || tail > maxTail || !bytes.Equal(repacked, data[4:4+len(repacked)]) {
		return fmt.Errorf("%w: %s is not canonically encoded", ErrMalformed, m.Sig)
	}
	if len(values) == 1 {
		return convert(m.Sig, values[0], args)
	}
	if err := m.Inputs.Copy(args, values); err != nil {
		return fmt.Errorf("symbiosis: %s into %T: %w", m.Sig, args, err)
	}
	return nil
}

// convert copies a decoded tuple into a struct by field name. Arguments.Copy
// would put a single argument into the struct's first field instead, and
// ConvertType reports a field that does not fit by panicking.
func convert(sig string, tuple, args any) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("symbiosis: %s into %T: %v", sig, args, r)
		}
	}()
	abi.ConvertType(tuple, args)
	return nil
}
