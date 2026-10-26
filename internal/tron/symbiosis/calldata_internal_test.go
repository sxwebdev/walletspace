package symbiosis

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// A struct that does not follow the ABI is a bug in this package. decode must
// report it rather than leave zero values behind, on both of its paths: a
// single tuple and a list of arguments.
func TestDecodeRefusesAStructThatDoesNotFit(t *testing.T) {
	t.Parallel()
	type unrelated struct{ Nothing string }

	swap, err := octopoolABI.Pack("swap", big.NewInt(1), big.NewInt(2), big.NewInt(3), big.NewInt(4), common.Address{1}, big.NewInt(5))
	if err != nil {
		t.Fatal(err)
	}
	if err := decode(octopoolABI, "swap", swap, &unrelated{}); err == nil {
		t.Error("decode() filled a struct that does not follow the arguments")
	}

	exec, err := settlerABI.Pack("execute", allowedSlippage{MinAmountOut: big.NewInt(1)}, [][]byte{}, [32]byte{})
	if err != nil {
		t.Fatal(err)
	}
	var into struct{ Slippage struct{ Recipient string } }
	if err := decode(settlerABI, "execute", exec, &into); err == nil {
		t.Error("decode() filled a struct whose fields have other types")
	}

	route, err := gatewayABI.Pack("metaRoute", metaRouteTx{Amount: big.NewInt(1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := decode(gatewayABI, "metaRoute", route, &unrelated{}); err == nil {
		t.Error("decode() filled a struct that does not follow the tuple")
	}
}

func TestScale(t *testing.T) {
	t.Parallel()
	tests := []struct {
		v        int64
		from, to uint8
		want     string
	}{
		{1_234_567, 6, 6, "1234567"},
		{1_234_567, 6, 18, "1234567000000000000"},
		{1_234_567, 6, 4, "12345"},
		{999, 18, 6, "0"},
	}
	for _, tt := range tests {
		if got := scale(big.NewInt(tt.v), tt.from, tt.to); got.String() != tt.want {
			t.Errorf("scale(%d, %d, %d) = %s, want %s", tt.v, tt.from, tt.to, got, tt.want)
		}
	}
}

// A direct payout's minimum is the pool's minimum less the fee, both in the
// synthetic's units; the recipient is paid in the real token's. With every
// listed stable the two agree, so only a synthetic whose decimals differ can
// show that the minimum is converted.
func TestDirectPayoutConvertsToTheRealDecimals(t *testing.T) {
	t.Parallel()
	dest := destinations[42161]
	stable := dest.Stables[0]
	stable.Decimals, stable.RealDecimals = 18, 6
	recipient := common.Address{0x11}
	burn := metaBurnTx{
		StableBridgingFee: new(big.Int).Exp(big.NewInt(10), big.NewInt(17), nil), // 0.1
		SToken:            stable.Token,
		Chain2address:     recipient,
		ReceiveSide:       dest.Portal,
		OppositeBridge:    dest.Bridge,
		SyntCaller:        operators[0],
		RevertableAddress: operators[0],
		ChainID:           big.NewInt(42161),
		FinalOffset:       new(big.Int),
	}
	minPooled := new(big.Int).Mul(big.NewInt(99), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	want := Want{ChainID: 42161, Token: stable.Real, Recipient: recipient}
	venue, minOut, err := checkBurn(burn, want, dest, stable, minPooled, nil)
	if err != nil {
		t.Fatalf("checkBurn() error = %v", err)
	}
	if venue != VenueDirect || minOut.String() != "98900000" {
		t.Errorf("checkBurn() = %q, %s; want %q, 98.9 in six decimals", venue, minOut, VenueDirect)
	}
}
