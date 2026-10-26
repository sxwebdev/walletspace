package symbiosis_test

import (
	"encoding/hex"
	"encoding/json/v2"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/sxwebdev/walletspace/internal/tron/symbiosis"
)

// The quotes in testdata/quotes are real answers of the Symbiosis API to
// /crosschain/v1/swap for 100 USDT out of Tron, recorded on 2026-10-26. The
// request named a sender whose EVM form is 0x2222…2222 and a recipient of
// 0x1111…1111, so both are easy to find in the calldata.
const (
	owner = "TD5gsCwxykWsLN9aPrq2TAfNjByuZKYp4E"
	other = "TNXoiAJ3dct8Fjg4M9fkLFh9S2v9TXc32G"
)

var (
	recipient = common.HexToAddress("0x1111111111111111111111111111111111111111")
	stranger  = common.HexToAddress("0x9999999999999999999999999999999999999999")
	hundred   = big.NewInt(100_000_000)

	arbitrumUSDC = common.HexToAddress("0xaf88d065e77c8cC2239327C5EDb3A432268e5831")
	arbitrumUSDT = common.HexToAddress("0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9")
	bscUSDC      = common.HexToAddress("0x8AC76a51cc950d9822D68b83fE1Ad97B32Cd580d")
	bscUSDT      = common.HexToAddress("0x55d398326f99059fF775485246999027B3197955")

	// What the Tron gateway named as its router (metaRouterExecutorDontApprove)
	// when the quotes were recorded, and when that was.
	executor   = common.HexToAddress("0xd076ab0d4b74aef1aec7b1d4f106787e0d5f9163")
	recordedAt = time.Date(2026, 10, 26, 3, 0, 0, 0, time.UTC)

	// The Settlers the 0x registry named on each chain (ownerOf(2)) when the
	// quotes were recorded.
	settlers = map[uint64]common.Address{
		1:     common.HexToAddress("0x0cef320d0c5793ade37964e2661a033771e53cd5"),
		56:    common.HexToAddress("0x5be10189f009334aacae23d7102d64527e32bbdb"),
		42161: common.HexToAddress("0x93ea5e32e9c2be78a279be03f86b74917537df51"),
	}
)

type fixture struct {
	name    string
	chainID uint64
	token   common.Address
	venue   symbiosis.Venue
}

var fixtures = []fixture{
	{"arb_usdt", 42161, arbitrumUSDT, symbiosis.VenueOneInch},
	{"arb_eth", 42161, symbiosis.Native(), symbiosis.VenueZeroEx},
	{"bsc_usdt", 56, bscUSDT, symbiosis.VenueZeroEx},
	{"bsc_bnb", 56, symbiosis.Native(), symbiosis.VenueZeroEx},
	{"eth_eth", 1, symbiosis.Native(), symbiosis.VenueZeroEx},
}

type recorded struct {
	Tx struct {
		From             string `json:"from"`
		To               string `json:"to"`
		Data             string `json:"data"`
		Value            string `json:"value"`
		FunctionSelector string `json:"functionSelector"`
	} `json:"tx"`
	ApproveTo         string `json:"approveTo"`
	TokenAmountOutMin struct {
		Amount string `json:"amount"`
	} `json:"tokenAmountOutMin"`
}

// load reads a recorded quote and the request it answered. The request's
// floor is the quote's own minimum, which is what the calldata enforces.
func load(t *testing.T, f fixture) (symbiosis.Tx, symbiosis.Want) {
	t.Helper()
	raw, err := os.ReadFile("testdata/quotes/" + f.name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var q recorded
	if err := json.Unmarshal(raw, &q); err != nil {
		t.Fatalf("%s: %v", f.name, err)
	}
	data, err := hex.DecodeString(strings.TrimPrefix(q.Tx.Data, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	value, ok := new(big.Int).SetString(q.Tx.Value, 10)
	if !ok {
		t.Fatalf("%s: value %q", f.name, q.Tx.Value)
	}
	tx := symbiosis.Tx{
		From: q.Tx.From, To: q.Tx.To, FunctionSelector: q.Tx.FunctionSelector,
		Data: data, Value: value, ApproveTo: q.ApproveTo,
	}
	return tx, symbiosis.Want{
		Owner: owner, Amount: new(big.Int).Set(hundred), ChainID: f.chainID,
		Token: f.token, Recipient: recipient, MinOut: amount(t, q.TokenAmountOutMin.Amount),
	}
}

func fixtureNamed(t *testing.T, name string) fixture {
	t.Helper()
	for _, f := range fixtures {
		if f.name == name {
			return f
		}
	}
	t.Fatalf("no fixture %q", name)
	return fixture{}
}

func amount(t *testing.T, s string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("amount %q", s)
	}
	return v
}

// rewrite decodes a transaction, lets edit change any layer, and encodes it
// again canonically, so the only thing wrong with the result is the edit.
func rewrite(t *testing.T, tx symbiosis.Tx, edit func(*symbiosis.Layers)) symbiosis.Tx {
	t.Helper()
	layers, err := symbiosis.Decode(tx)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	edit(&layers)
	data, err := symbiosis.Encode(layers)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	tx.Data = data
	return tx
}

// factsFor is what the wallet would have read for a quote to chainID when the
// quotes were recorded.
func factsFor(chainID uint64) symbiosis.Facts {
	return symbiosis.Facts{
		Executor: executor,
		Settlers: []common.Address{settlers[chainID]},
		Now:      recordedAt,
	}
}
