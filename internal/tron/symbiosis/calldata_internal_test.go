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
