package raydium_test

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
)

// mathCapture is testdata/math_<kind>.json: a Trade API quote and, read
// right after it, the pool, its vaults (and config).
type mathCapture struct {
	Quote struct {
		Data struct {
			InputAmount  string `json:"inputAmount"`
			OutputAmount string `json:"outputAmount"`
		} `json:"data"`
	} `json:"quote"`
	Accounts map[string]struct {
		Owner string `json:"owner"`
		Data  string `json:"data"`
	} `json:"accounts"`
}

func (c mathCapture) account(t *testing.T, k string) (solana.PublicKey, []byte) {
	t.Helper()
	a, ok := c.Accounts[k]
	if !ok {
		t.Fatalf("capture lacks %s", k)
	}
	data, err := base64.StdEncoding.DecodeString(a.Data)
	if err != nil {
		t.Fatal(err)
	}
	return key(a.Owner), data
}

func (c mathCapture) amounts(t *testing.T) (in, out uint64) {
	t.Helper()
	in, err := strconv.ParseUint(c.Quote.Data.InputAmount, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	out, err = strconv.ParseUint(c.Quote.Data.OutputAmount, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return in, out
}

func vaultAmount(t *testing.T, c mathCapture, k string) uint64 {
	t.Helper()
	_, data := c.account(t, k)
	return binary.LittleEndian.Uint64(data[64:])
}

// deviationBps is |a − b| / b in basis points.
func deviationBps(a, b uint64) float64 {
	return math.Abs(float64(a)-float64(b)) / float64(b) * 10000
}

// The pool's own curve, fed with the vault balances read right after the
// quote, gives the API's figure for AMM v4 and CPMM routes.
func TestAmountOutAgreesWithTheTradeAPI(t *testing.T) {
	t.Parallel()
	t.Run("amm v4", func(t *testing.T) {
		t.Parallel()
		var c mathCapture
		readJSON(t, "math_ammv4.json", &c)
		p, err := raydium.DecodeAMMV4Pool(c.account(t, "58oQChx4yWmvKdwLLZzBi4ChoCc2fqCUWBkwMihLYQo2"))
		if err != nil {
			t.Fatal(err)
		}
		in, quoted := c.amounts(t)
		local, err := p.AmountOut(wsol, in, vaultAmount(t, c, "DQyrAcCrDXQ7NeoqGgDCZwBvWDcYmFCjSb9JtteuvPpz"), vaultAmount(t, c, "HLmqeL62xR1QoZ1HKKbXRrdN1p3phKpxRMb2VVopvBBz"))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("AMM v4: API %d, curve %d (%.4f bps)", quoted, local, deviationBps(quoted, local))
		if deviationBps(quoted, local) > 1 {
			t.Errorf("API %d, curve %d", quoted, local)
		}
		if err := raydium.CheckQuotedOut(local, quoted, 10); err != nil {
			t.Error(err)
		}
	})
	t.Run("cpmm", func(t *testing.T) {
		t.Parallel()
		var c mathCapture
		readJSON(t, "math_cpmm.json", &c)
		p, err := raydium.DecodeCPMMPool(c.account(t, "Q2sPHPdUWFMg7M7wwrQKLrn619cAucfRsmhVJffodSp"))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := raydium.DecodeCPMMConfig(c.account(t, p.AmmConfig.String()))
		if err != nil {
			t.Fatal(err)
		}
		in, quoted := c.amounts(t)
		local, err := p.AmountOut(cfg, wsol, in, vaultAmount(t, c, p.Token0Vault.String()), vaultAmount(t, c, p.Token1Vault.String()))
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("CPMM: API %d, curve %d (%.4f bps)", quoted, local, deviationBps(quoted, local))
		if deviationBps(quoted, local) > 1 {
			t.Errorf("API %d, curve %d", quoted, local)
		}
		if err := raydium.CheckQuotedOut(local, quoted, 10); err != nil {
			t.Error(err)
		}
	})
}

func TestCheckQuotedOut(t *testing.T) {
	t.Parallel()
	tests := []struct {
		local, quoted, max uint64
		ok                 bool
	}{
		{10000, 10000, 0, true},
		{10000, 10010, 10, true},  // exactly 10 bps above
		{10000, 9990, 10, true},   // exactly 10 bps below
		{10000, 10011, 10, false}, // just above
		{10000, 9989, 10, false},  // just below
		{0, 0, 0, true},
		{0, 1, 10000, false},
		{math.MaxUint64, math.MaxUint64 - 1, 1, true}, // no overflow
		{math.MaxUint64, 0, 9999, false},
		{math.MaxUint64, 0, 10000, true},
	}
	for _, tt := range tests {
		err := raydium.CheckQuotedOut(tt.local, tt.quoted, tt.max)
		if tt.ok != (err == nil) || (err != nil && !errors.Is(err, raydium.ErrMalformedRoute)) {
			t.Errorf("CheckQuotedOut(%d, %d, %d) = %v, want ok=%v", tt.local, tt.quoted, tt.max, err, tt.ok)
		}
	}
}
