package sunswap_test

import (
	"bytes"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/sxwebdev/gotron/pkg/tronutils"
	"github.com/sxwebdev/walletspace/internal/tron/sunswap"
)

const (
	mainnetUSDT = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
	nileUSDT    = "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf"
	someOwner   = "TNXoiAJ3dct8Fjg4M9fkLFh9S2v9TXc32G"
)

func addr(t *testing.T, base58 string) common.Address {
	t.Helper()
	a, err := sunswap.EVMAddress(base58)
	if err != nil {
		t.Fatalf("EVMAddress(%q) error = %v", base58, err)
	}
	return a
}

// Each selector is checked against the four bytes real transactions to these
// routers carry on mainnet, so a typo in an ABI fragment cannot produce calls
// to a function that does not exist.
func TestSelectorsMatchTheDeployedRouters(t *testing.T) {
	t.Parallel()
	owner, usdt := addr(t, someOwner), addr(t, mainnetUSDT)
	one := big.NewInt(1)
	path := []common.Address{usdt, owner}
	build := func(c sunswap.Calldata, err error) sunswap.Calldata {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	tests := []struct {
		name     string
		call     sunswap.Calldata
		selector string
	}{
		{"approve", build(sunswap.Approve(owner, one)), "095ea7b3"},
		{"allowance", build(sunswap.Allowance(owner, owner)), "dd62ed3e"},
		{"Permit2 approve", build(sunswap.Permit2Approve(usdt, owner, one, 1)), "87517c45"},
		{"V2 swapExactETHForTokens", build(sunswap.V2SwapExactETHForTokens(one, path, owner, 1)), "7ff36ab5"},
		{"V2 swapExactTokensForETH", build(sunswap.V2SwapExactTokensForETH(one, one, path, owner, 1)), "18cbafe5"},
		{"V2 getAmountsOut", build(sunswap.V2GetAmountsOut(one, path)), "d06ca61f"},
		{"V3 exactInputSingle", build(sunswap.V3ExactInputSingle(sunswap.V3ExactInputSingleParams{
			Fee: one, Deadline: one, AmountIn: one, AmountOutMinimum: one, SqrtPriceLimitX96: one,
		})), "414bf389"},
		{"V3 multicall", build(sunswap.V3Multicall()), "ac9650d8"},
		{"V3 quoteExactInputSingle", build(sunswap.V3QuoteExactInputSingle(usdt, owner, 500, one)), "f7729d43"},
		{"universal router execute", build(sunswap.V4ExactInputSingle(sunswap.NewPoolKey(usdt, 500, 10), true, one, one, 1)), "3593564c"},
	}
	for _, tt := range tests {
		if got := hex.EncodeToString(tt.call.Data[:4]); got != tt.selector {
			t.Errorf("%s selector = %s, want %s", tt.name, got, tt.selector)
		}
		if !strings.Contains(tt.call.Method, "(") {
			t.Errorf("%s method %q is not a signature", tt.name, tt.call.Method)
		}
	}
}

// A V4 pool is found by its id. The ids below are the pools that exist: the
// mainnet TRX/USDT pool and the two on Nile, read from the pool managers'
// own index.
func TestPoolKeyIDsMatchTheDeployedPools(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		token       string
		fee         uint32
		tickSpacing int
		want        string
	}{
		{"mainnet 0.05%", mainnetUSDT, 500, 10, "dda1d5819853f19f3e952da5d93aa2d572d95c72a8e6e4c2acab65384fd2557e"},
		{"nile 0.05%", nileUSDT, 500, 10, "977d6ad6be3a3206f7ca881434bb8a08ecaf1abe4690eed6ee23e3e7e0ae9b6a"},
		{"nile 0.3%", nileUSDT, 3000, 1, "123bfcea96ca0b639c7cae1d3418739ed1981a9773ac3fd1b6e59ac1441246c8"},
	}
	for _, tt := range tests {
		key := sunswap.NewPoolKey(addr(t, tt.token), tt.fee, tt.tickSpacing)
		id, err := key.ID()
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(id[:]); got != tt.want {
			t.Errorf("%s: id = %s, want %s", tt.name, got, tt.want)
		}
		if key.TickSpacing() != tt.tickSpacing {
			t.Errorf("%s: TickSpacing() = %d, want %d", tt.name, key.TickSpacing(), tt.tickSpacing)
		}
	}
}

// Decoding what V4ExactInputSingle produced is the check that the minimum,
// the recipient and the payer are where the router looks for them. The bytes
// themselves were run against the mainnet router by the live tests.
func TestV4ExactInputSingleSendsTheOutputToTheCaller(t *testing.T) {
	t.Parallel()
	usdt := addr(t, mainnetUSDT)
	key := sunswap.NewPoolKey(usdt, 500, 10)
	amountIn, minOut := big.NewInt(1_000_000_000), big.NewInt(330_000_000)
	for _, trxIn := range []bool{true, false} {
		call, err := sunswap.V4ExactInputSingle(key, trxIn, amountIn, minOut, 1_800_000_000)
		if err != nil {
			t.Fatal(err)
		}
		execute := mustParse(t, `[{"type":"function","name":"execute","inputs":[{"name":"c","type":"bytes"},{"name":"i","type":"bytes[]"},{"name":"d","type":"uint256"}]}]`)
		args, err := execute.Methods["execute"].Inputs.Unpack(call.Data[4:])
		if err != nil {
			t.Fatal(err)
		}
		commands, inputs, deadline := args[0].([]byte), args[1].([][]byte), args[2].(*big.Int)
		if !bytes.Equal(commands, []byte{0x12, 0x04}) {
			t.Fatalf("commands = %x, want V4_SWAP then SWEEP", commands)
		}
		if deadline.Int64() != 1_800_000_000 {
			t.Errorf("deadline = %s", deadline)
		}

		sweep, err := abi.Arguments{{Type: mustType(t, "address")}, {Type: mustType(t, "address")}, {Type: mustType(t, "uint160")}}.Unpack(inputs[1])
		if err != nil {
			t.Fatal(err)
		}
		wantOut := common.Address{} // native TRX
		if trxIn {
			wantOut = usdt
		}
		if sweep[0].(common.Address) != wantOut {
			t.Errorf("trxIn=%v: sweeps %s, want %s", trxIn, sweep[0], wantOut)
		}
		if sweep[1].(common.Address) != common.BigToAddress(big.NewInt(1)) {
			t.Errorf("trxIn=%v: sweeps to %s, want the caller placeholder", trxIn, sweep[1])
		}
		if sweep[2].(*big.Int).Cmp(minOut) != 0 {
			t.Errorf("trxIn=%v: sweep minimum = %s, want %s", trxIn, sweep[2], minOut)
		}

		v4, err := abi.Arguments{{Type: mustType(t, "bytes")}, {Type: mustType(t, "bytes[]")}}.Unpack(inputs[0])
		if err != nil {
			t.Fatal(err)
		}
		if actions := v4[0].([]byte); !bytes.Equal(actions, []byte{0x0b, 0x06, 0x0e, 0x0e}) {
			t.Fatalf("actions = %x, want SETTLE, CL_SWAP_EXACT_IN_SINGLE, TAKE, TAKE", actions)
		}
		settle, err := abi.Arguments{{Type: mustType(t, "address")}, {Type: mustType(t, "uint256")}, {Type: mustType(t, "bool")}}.Unpack(v4[1].([][]byte)[0])
		if err != nil {
			t.Fatal(err)
		}
		if settle[1].(*big.Int).Cmp(amountIn) != 0 {
			t.Errorf("trxIn=%v: settles %s, want %s", trxIn, settle[1], amountIn)
		}
		// The native coin arrives as call value; USDT is drawn from the user.
		if payerIsUser := settle[2].(bool); payerIsUser == trxIn {
			t.Errorf("trxIn=%v: payerIsUser = %v", trxIn, payerIsUser)
		}
	}
}

func TestV4ExactInputSingleRefusesAMinimumAboveUint160(t *testing.T) {
	t.Parallel()
	tooBig := new(big.Int).Lsh(big.NewInt(1), 160)
	if _, err := sunswap.V4ExactInputSingle(sunswap.NewPoolKey(addr(t, mainnetUSDT), 500, 10), true, big.NewInt(1), tooBig, 1); err == nil {
		t.Fatal("a minimum the sweep cannot hold was accepted")
	}
}

func TestPermit2ApproveRefusesOutOfRangeArguments(t *testing.T) {
	t.Parallel()
	token, spender := addr(t, mainnetUSDT), addr(t, someOwner)
	for _, tc := range []struct {
		name       string
		amount     *big.Int
		expiration uint64
	}{
		{"zero amount", big.NewInt(0), 1},
		{"amount above uint160", new(big.Int).Lsh(big.NewInt(1), 160), 1},
		{"expiration above uint48", big.NewInt(1), 1 << 48},
	} {
		if _, err := sunswap.Permit2Approve(token, spender, tc.amount, tc.expiration); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

// Calldata reaches the node's transaction builder as bytes32 words under the
// real signature; the words have to be exactly the bytes after the selector.
func TestWordsSplitTheArgumentsAfterTheSelector(t *testing.T) {
	t.Parallel()
	call, err := sunswap.V3Multicall(
		sunswap.Calldata{Data: []byte{1, 2, 3}},
		sunswap.Calldata{Data: bytes.Repeat([]byte{0xff}, 40)},
	)
	if err != nil {
		t.Fatal(err)
	}
	words, err := call.Words()
	if err != nil {
		t.Fatal(err)
	}
	var joined []byte
	for _, word := range words {
		joined = append(joined, word[:]...)
	}
	if !bytes.Equal(joined, call.Data[4:]) {
		t.Error("words do not reassemble into the arguments")
	}
	for _, bad := range [][]byte{{1, 2}, append([]byte{1, 2, 3, 4}, make([]byte, 31)...)} {
		if _, err := (sunswap.Calldata{Data: bad}).Words(); err == nil {
			t.Errorf("Words() accepted %d bytes", len(bad))
		}
	}
}

func TestEVMAddressRoundTripsAndRefusesGarbage(t *testing.T) {
	t.Parallel()
	a := addr(t, mainnetUSDT)
	if got := sunswap.TronAddress(a); got != mainnetUSDT {
		t.Errorf("TronAddress(EVMAddress(x)) = %s, want %s", got, mainnetUSDT)
	}
	for _, bad := range []string{"", "not-an-address", "TR7NHqjeKQxGChj8NioMYvSTnMGtJHLzN" /* bad checksum */} {
		if _, err := sunswap.EVMAddress(bad); err == nil {
			t.Errorf("EVMAddress(%q) accepted", bad)
		}
	}
}

func TestDecodersReadContractAnswers(t *testing.T) {
	t.Parallel()
	word := func(v int64) []byte {
		// Two's complement over 256 bits, as the ABI encodes signed values.
		n := big.NewInt(v)
		if v < 0 {
			n.Add(n, new(big.Int).Lsh(big.NewInt(1), 256))
		}
		return common.LeftPadBytes(n.Bytes(), 32)
	}

	if got, err := sunswap.DecodeAllowance(word(42)); err != nil || got.Int64() != 42 {
		t.Errorf("DecodeAllowance() = %v, %v", got, err)
	}
	amount, expiration, err := sunswap.DecodePermit2Allowance(append(append(word(7), word(99)...), word(0)...))
	if err != nil || amount.Int64() != 7 || expiration != 99 {
		t.Errorf("DecodePermit2Allowance() = %v, %d, %v", amount, expiration, err)
	}
	if got, err := sunswap.DecodeV3Quote(word(5)); err != nil || got.Int64() != 5 {
		t.Errorf("DecodeV3Quote() = %v, %v", got, err)
	}
	// getAmountsOut answers with the whole path; the last amount is the output.
	amounts := append(append(append(word(32), word(2)...), word(10)...), word(3)...)
	if got, err := sunswap.DecodeV2AmountsOut(amounts); err != nil || got.Int64() != 3 {
		t.Errorf("DecodeV2AmountsOut() = %v, %v", got, err)
	}
	if _, err := sunswap.DecodeV2AmountsOut(append(append(word(32), word(1)...), word(10)...)); err == nil {
		t.Error("DecodeV2AmountsOut() accepted a single amount")
	}
	slot := append(append(append(word(1<<40), word(-5)...), word(100<<12)...), word(500)...)
	got, err := sunswap.DecodeV4Slot0(slot)
	if err != nil {
		t.Fatal(err)
	}
	if got.SqrtPriceX96.Int64() != 1<<40 || got.Tick != -5 || got.ProtocolFee != 100<<12 || got.LPFee != 500 {
		t.Errorf("DecodeV4Slot0() = %+v", got)
	}
	tickInfo := append(append(append(word(1), word(-3)...), word(0)...), word(0)...)
	if net, err := sunswap.DecodeV4LiquidityNet(tickInfo); err != nil || net.Int64() != -3 {
		t.Errorf("DecodeV4LiquidityNet() = %v, %v", net, err)
	}
	for _, decode := range []func([]byte) error{
		func(b []byte) error { _, err := sunswap.DecodeAllowance(b); return err },
		func(b []byte) error { _, _, err := sunswap.DecodePermit2Allowance(b); return err },
		func(b []byte) error { _, err := sunswap.DecodeV3Quote(b); return err },
		func(b []byte) error { _, err := sunswap.DecodeV2AmountsOut(b); return err },
		func(b []byte) error { _, err := sunswap.DecodeV4Slot0(b); return err },
		func(b []byte) error { _, err := sunswap.DecodeV4Liquidity(b); return err },
		func(b []byte) error { _, err := sunswap.DecodeV4BitmapWord(b); return err },
		func(b []byte) error { _, err := sunswap.DecodeV4LiquidityNet(b); return err },
		func(b []byte) error { _, _, err := sunswap.DecodeV3Slot0(b); return err },
		func(b []byte) error { _, err := sunswap.DecodeV3Liquidity(b); return err },
		func(b []byte) error { _, err := sunswap.DecodeV3BitmapWord(b); return err },
		func(b []byte) error { _, err := sunswap.DecodeV3LiquidityNet(b); return err },
	} {
		if err := decode([]byte{1}); err == nil {
			t.Error("a decoder accepted a truncated answer")
		}
	}
}

func mustParse(t *testing.T, definition string) abi.ABI {
	t.Helper()
	parsed, err := abi.JSON(strings.NewReader(definition))
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func mustType(t *testing.T, name string) abi.Type {
	t.Helper()
	ty, err := abi.NewType(name, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return ty
}

// The reads a quote makes go to the pool and the pool manager by signature;
// each has to name a function those contracts have.
func TestReadSignatures(t *testing.T) {
	t.Parallel()
	owner, usdt := addr(t, someOwner), addr(t, mainnetUSDT)
	var id [32]byte
	build := func(c sunswap.Calldata, err error) sunswap.Calldata {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	tests := []struct {
		call     sunswap.Calldata
		selector string
		method   string
	}{
		{build(sunswap.Permit2Allowance(owner, usdt, owner)), "927da105", "allowance(address,address,address)"},
		{build(sunswap.V3UnwrapWETH9(big.NewInt(1), owner)), "49404b7c", "unwrapWETH9(uint256,address)"},
		{build(sunswap.V3Slot0()), "3850c7bd", "slot0()"},
		{build(sunswap.V3Liquidity()), "1a686502", "liquidity()"},
		{build(sunswap.V3TickBitmap(-5)), "5339c296", "tickBitmap(int16)"},
		{build(sunswap.V3Ticks(-11080)), "f30dba93", "ticks(int24)"},
		{build(sunswap.V4GetSlot0(id)), "", "getSlot0(bytes32)"},
		{build(sunswap.V4GetLiquidity(id)), "", "getLiquidity(bytes32)"},
		{build(sunswap.V4GetPoolBitmapInfo(id, -5)), "", "getPoolBitmapInfo(bytes32,int16)"},
		{build(sunswap.V4GetPoolTickInfo(id, -11080)), "", "getPoolTickInfo(bytes32,int24)"},
	}
	for _, tt := range tests {
		if tt.call.Method != tt.method {
			t.Errorf("method = %q, want %q", tt.call.Method, tt.method)
		}
		if tt.selector != "" && hex.EncodeToString(tt.call.Data[:4]) != tt.selector {
			t.Errorf("%s selector = %x, want %s", tt.method, tt.call.Data[:4], tt.selector)
		}
	}
}

func TestEVMAddressRefusesAnotherNetworksPrefix(t *testing.T) {
	t.Parallel()
	raw := append([]byte{0xa0}, make([]byte, 20)...)
	if _, err := sunswap.EVMAddress(tronutils.EncodeCheck(raw)); err == nil {
		t.Fatal("an address without the 0x41 prefix was accepted")
	}
}

func TestEncodersRefuseNegativeAmounts(t *testing.T) {
	t.Parallel()
	usdt := addr(t, mainnetUSDT)
	if _, err := sunswap.V4ExactInputSingle(sunswap.NewPoolKey(usdt, 500, 10), true, big.NewInt(-1), big.NewInt(1), 1); err == nil {
		t.Error("V4ExactInputSingle accepted a negative amount")
	}
	if _, err := sunswap.V4ExactInputSingle(sunswap.NewPoolKey(usdt, 500, 10), true, big.NewInt(1), big.NewInt(-1), 1); err == nil {
		t.Error("V4ExactInputSingle accepted a negative minimum")
	}
	if _, err := sunswap.Approve(usdt, big.NewInt(-1)); err == nil {
		t.Error("Approve accepted a negative amount")
	}
}

func TestDecodersReadPoolState(t *testing.T) {
	t.Parallel()
	word := func(v int64) []byte {
		n := big.NewInt(v)
		if v < 0 {
			n.Add(n, new(big.Int).Lsh(big.NewInt(1), 256))
		}
		return common.LeftPadBytes(n.Bytes(), 32)
	}
	if v, err := sunswap.DecodeV4Liquidity(word(9)); err != nil || v.Int64() != 9 {
		t.Errorf("DecodeV4Liquidity() = %v, %v", v, err)
	}
	if v, err := sunswap.DecodeV4BitmapWord(word(8)); err != nil || v.Int64() != 8 {
		t.Errorf("DecodeV4BitmapWord() = %v, %v", v, err)
	}
	if v, err := sunswap.DecodeV3Liquidity(word(7)); err != nil || v.Int64() != 7 {
		t.Errorf("DecodeV3Liquidity() = %v, %v", v, err)
	}
	if v, err := sunswap.DecodeV3BitmapWord(word(6)); err != nil || v.Int64() != 6 {
		t.Errorf("DecodeV3BitmapWord() = %v, %v", v, err)
	}
	var slot0 []byte
	for _, v := range []int64{1 << 50, -11070, 1, 2, 3, 0, 1} {
		slot0 = append(slot0, word(v)...)
	}
	if price, tick, err := sunswap.DecodeV3Slot0(slot0); err != nil || price.Int64() != 1<<50 || tick != -11070 {
		t.Errorf("DecodeV3Slot0() = %v, %d, %v", price, tick, err)
	}
	var ticks []byte
	for _, v := range []int64{100, -42, 0, 0, 0, 0, 0, 1} {
		ticks = append(ticks, word(v)...)
	}
	if net, err := sunswap.DecodeV3LiquidityNet(ticks); err != nil || net.Int64() != -42 {
		t.Errorf("DecodeV3LiquidityNet() = %v, %v", net, err)
	}
}
