package symbiosis

import (
	"fmt"
	"reflect"
	"slices"

	"github.com/ethereum/go-ethereum/accounts/abi"
)

// The layers of a route, exported so the black-box tests can change one field
// deep inside a real quote and have everything around it re-encoded.
type (
	MetaRouteTx        = metaRouteTx
	MetaSynthesizeTx   = metaSynthesizeTx
	MetaBurnTx         = metaBurnTx
	MulticallArgs      = multicallArgs
	OctopoolSwapArgs   = octopoolSwapArgs
	OneInchSwapArgs    = oneInchSwapArgs
	ExecArgs           = execArgs
	SettlerExecuteArgs = settlerExecuteArgs
)

// Layers is a route decoded all the way down. Exactly one of OneInch and Exec
// is set when the route ends in a swap; Settler goes with Exec.
//
// The Raw fields replace a layer's encoding outright, for tests of bytes that
// are not the call they claim to be: RawOtherSide the metaSynthesize call,
// RawPool the multicall, RawPoolCall the octopool swap inside it and RawBurn
// the metaBurnSyntheticToken call. A final
// swap set to nil keeps Burn.FinalCallData, and a nil Settler keeps Exec.Data.
type Layers struct {
	Route   MetaRouteTx
	Synth   MetaSynthesizeTx
	Pool    MulticallArgs
	Swap    OctopoolSwapArgs
	Burn    MetaBurnTx
	OneInch *OneInchSwapArgs
	// OneInchTail is what the 1inch API appended after the swap call.
	OneInchTail []byte
	Exec        *ExecArgs
	Settler     *SettlerExecuteArgs

	RawOtherSide, RawPool, RawPoolCall, RawBurn []byte
}

// Decode takes a transaction apart without judging it.
func Decode(tx Tx) (Layers, error) {
	var l Layers
	method := gatewayABI.Methods["metaRoute"]
	steps := []func() error{
		func() error { return decode(gatewayABI, "metaRoute", slices.Concat(method.ID, tx.Data), &l.Route) },
		func() error { return decode(portalABI, "metaSynthesize", l.Route.OtherSideCalldata, &l.Synth) },
		func() error { return decode(multicallABI, "multicall", l.Synth.SecondSwapCalldata, &l.Pool) },
		func() error { return decode(octopoolABI, "swap", l.Pool.Calldata[0], &l.Swap) },
		func() error { return decode(synthesisABI, "metaBurnSyntheticToken", l.Synth.FinalCalldata, &l.Burn) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return Layers{}, err
		}
	}
	switch l.Burn.FinalReceiveSide {
	case oneInchRouter:
		l.OneInch = new(OneInchSwapArgs)
		if err := decodeTail(oneInchABI, "swap", l.Burn.FinalCallData, l.OneInch, oneInchMaxTail); err != nil {
			return Layers{}, err
		}
		data := l.Burn.FinalCallData
		l.OneInchTail = slices.Clone(data[len(data)-(len(data)-4)%32:])
		return l, nil
	case allowanceHolder:
		l.Exec, l.Settler = new(ExecArgs), new(SettlerExecuteArgs)
		if err := decode(allowanceHolderABI, "exec", l.Burn.FinalCallData, l.Exec); err != nil {
			return Layers{}, err
		}
		return l, decode(settlerABI, "execute", l.Exec.Data, l.Settler)
	}
	return l, nil
}

// Encode puts the layers back together from the bottom up and returns the
// transaction's arguments.
func Encode(l Layers) ([]byte, error) {
	var err error
	if l.Settler != nil {
		if l.Exec.Data, err = pack(settlerABI, "execute", *l.Settler); err != nil {
			return nil, err
		}
	}
	if l.Exec != nil {
		if l.Burn.FinalCallData, err = pack(allowanceHolderABI, "exec", *l.Exec); err != nil {
			return nil, err
		}
	}
	if l.OneInch != nil {
		swap, err := pack(oneInchABI, "swap", *l.OneInch)
		if err != nil {
			return nil, err
		}
		l.Burn.FinalCallData = slices.Concat(swap, l.OneInchTail)
	}
	swap, err := pack(octopoolABI, "swap", l.Swap)
	if err != nil {
		return nil, err
	}
	l.Pool.Calldata = slices.Clone(l.Pool.Calldata)
	l.Pool.Calldata[0] = raw(l.RawPoolCall, swap)
	pool, err := pack(multicallABI, "multicall", l.Pool)
	if err != nil {
		return nil, err
	}
	l.Synth.SecondSwapCalldata = raw(l.RawPool, pool)
	burn, err := pack(synthesisABI, "metaBurnSyntheticToken", l.Burn)
	if err != nil {
		return nil, err
	}
	l.Synth.FinalCalldata = raw(l.RawBurn, burn)
	otherSide, err := pack(portalABI, "metaSynthesize", l.Synth)
	if err != nil {
		return nil, err
	}
	l.Route.OtherSideCalldata = raw(l.RawOtherSide, otherSide)
	data, err := pack(gatewayABI, "metaRoute", l.Route)
	if err != nil {
		return nil, err
	}
	return data[4:], nil
}

// pack encodes args, a struct laid out like the ones decode fills, as a call
// to method: a method taking one tuple gets the struct itself, any other its
// fields in order.
func pack(contract abi.ABI, method string, args any) ([]byte, error) {
	m := contract.Methods[method]
	if len(m.Inputs) == 1 {
		return contract.Pack(method, args)
	}
	v := reflect.ValueOf(args)
	if v.NumField() != len(m.Inputs) {
		return nil, fmt.Errorf("%T has %d fields for %d inputs of %s", args, v.NumField(), len(m.Inputs), m.Sig)
	}
	values := make([]any, v.NumField())
	for i := range values {
		values[i] = v.Field(i).Interface()
	}
	return contract.Pack(method, values...)
}

// raw returns the override when a test set one.
func raw(override, encoded []byte) []byte {
	if override != nil {
		return override
	}
	return encoded
}
