package httpapi_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/tron"
)

// Comparable, so the whole estimate can be asserted in one go and a field that
// silently stops being sent shows up as an empty string rather than passing.
type deployCostBody struct {
	Energy      string `json:"energy"`
	Bandwidth   string `json:"bandwidth"`
	Fee         string `json:"fee"`
	MinFeeLimit string `json:"min_fee_limit"`
	Shortfall   string `json:"shortfall"`
}

func TestDeploy(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.deployed = tron.Deployed{
		TxID:       "beefcafe",
		Address:    "TContract",
		Confirmed:  true,
		EnergyUsed: 812_345,
		Fee:        decimal.RequireFromString("178.5"),
	}

	srv := newServer(t, newWalletsFake(), chain)

	var got struct {
		TxID       string `json:"txid"`
		Address    string `json:"address"`
		Confirmed  bool   `json:"confirmed"`
		Failure    string `json:"failure"`
		EnergyUsed int64  `json:"energy_used"`
		Fee        string `json:"fee"`
	}
	do(t, srv, http.MethodPost, "/api/wallets/1/deploy", `{
		"name": "Token",
		"abi": "[]",
		"bytecode": "0x6080",
		"constructor_params": "[{\"uint256\":\"7\"}]",
		"fee_limit": "1000",
		"consume_user_resource_percent": 60,
		"origin_energy_limit": 10000000
	}`, http.StatusOK, &got)

	if got.TxID != "beefcafe" || got.Address != "TContract" {
		t.Errorf("response = %+v, want the txid and address the chain returned", got)
	}

	if !got.Confirmed || got.EnergyUsed != 812_345 || got.Fee != "178.5" {
		t.Errorf("cost = %+v, want what the receipt reported", got)
	}

	// The wallet in the path is the one that must sign, and every field has to
	// arrive intact: a deployment cannot be re-read from the chain to check.
	if chain.deployFrom != walletAddr1 {
		t.Errorf("deployed from %q, want the wallet named in the path", chain.deployFrom)
	}

	want := tron.Deployment{
		Name:                       "Token",
		ABI:                        "[]",
		Bytecode:                   "0x6080",
		ConstructorParams:          `[{"uint256":"7"}]`,
		FeeLimit:                   decimal.NewFromInt(1000),
		ConsumeUserResourcePercent: 60,
		OriginEnergyLimit:          10_000_000,
	}
	if !chain.deployment.FeeLimit.Equal(want.FeeLimit) {
		t.Errorf("fee limit = %s, want %s", chain.deployment.FeeLimit, want.FeeLimit)
	}

	chain.deployment.FeeLimit, want.FeeLimit = decimal.Zero, decimal.Zero
	if chain.deployment != want {
		t.Errorf("deployment = %+v, want %+v", chain.deployment, want)
	}

	if chain.deployKey == nil {
		t.Error("the deployment was signed with no key")
	}
}

func TestDeployEstimate(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.cost = tron.DeployCost{
		Energy:      decimal.NewFromInt(1_372_886),
		Bandwidth:   decimal.NewFromInt(1247),
		Fee:         decimal.RequireFromString("12.5"),
		MinFeeLimit: decimal.RequireFromString("137.2886"),
		Shortfall:   decimal.RequireFromString("2.5"),
	}

	srv := newServer(t, newWalletsFake(), chain)

	var got deployCostBody
	do(t, srv, http.MethodPost, "/api/wallets/1/deploy-estimate",
		`{"bytecode":"6080","fee_limit":"1000","origin_energy_limit":1}`, http.StatusOK, &got)

	want := deployCostBody{
		Energy: "1372886", Bandwidth: "1247",
		Fee: "12.5", MinFeeLimit: "137.2886", Shortfall: "2.5",
	}
	if got != want {
		t.Errorf("estimate = %+v, want %+v", got, want)
	}

	if chain.deployFrom != walletAddr1 {
		t.Errorf("priced for %q, want the wallet named in the path", chain.deployFrom)
	}

	if chain.costOf.Bytecode != "6080" {
		t.Errorf("priced bytecode %q, want the one in the body", chain.costOf.Bytecode)
	}
}

// Pricing signs nothing, so it must not touch the mnemonic — the whole point of
// asking first is that nothing has been committed to yet.
func TestDeployEstimateNeverDerivesAKey(t *testing.T) {
	t.Parallel()

	wallets := newWalletsFake()
	srv := newServer(t, wallets, newChainFake())

	do(t, srv, http.MethodPost, "/api/wallets/0/deploy-estimate",
		`{"bytecode":"6080","fee_limit":"1000","origin_energy_limit":1}`, http.StatusOK, nil)

	if wallets.keyCalls != 0 {
		t.Errorf("the private key was derived %d times for an estimate", wallets.keyCalls)
	}
}

// A constructor that reverts is the caller's contract, not the node's fault.
// The service marks it, and a 502 here would be retried against every node for
// code that reverts identically on all of them.
func TestDeployEstimateReportsARevertAsABadRequest(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.costErr = fmt.Errorf("%w: contract call failed: REVERT opcode executed", tron.ErrInvalidRequest)

	srv := newServer(t, newWalletsFake(), chain)
	do(t, srv, http.MethodPost, "/api/wallets/0/deploy-estimate",
		`{"bytecode":"6080","fee_limit":"1000","origin_energy_limit":1}`, http.StatusBadRequest, nil)
}

// Pricing carries the same contract as the deployment, so it needs the same
// room for it.
func TestDeployEstimateAcceptsABodyLargerThanTheDefaultCap(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	srv := newServer(t, newWalletsFake(), chain)

	body := `{"bytecode":"` + strings.Repeat("60", 50<<10) + `","fee_limit":"1000","origin_energy_limit":1}`
	do(t, srv, http.MethodPost, "/api/wallets/0/deploy-estimate", body, http.StatusOK, nil)

	if chain.costOf.Bytecode == "" {
		t.Error("the oversized body never reached the chain service")
	}
}

// A deployment the VM refused is still an answer: the transaction is on chain
// and the fee is spent, so the txid has to reach the caller. Reporting it as an
// error status would lose it.
func TestDeployReportsAFailedDeploymentAsAnAnswer(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.deployed = tron.Deployed{
		TxID:      "beefcafe",
		Address:   "TContract",
		Confirmed: true,
		Failure:   "OUT_OF_ENERGY",
	}

	srv := newServer(t, newWalletsFake(), chain)

	var got struct {
		TxID    string `json:"txid"`
		Failure string `json:"failure"`
	}
	do(t, srv, http.MethodPost, "/api/wallets/0/deploy",
		`{"bytecode":"6080","fee_limit":"1000","origin_energy_limit":1}`, http.StatusOK, &got)

	if got.TxID != "beefcafe" || got.Failure != "OUT_OF_ENERGY" {
		t.Errorf("response = %+v, want the txid alongside the failure", got)
	}
}

// An unconfirmed deployment must not report a cost of zero, which reads as one
// that was free.
func TestDeployOmitsTheCostUntilItIsKnown(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.deployed = tron.Deployed{TxID: "beefcafe", Address: "TContract"}

	srv := newServer(t, newWalletsFake(), chain)

	var got map[string]any
	do(t, srv, http.MethodPost, "/api/wallets/0/deploy",
		`{"bytecode":"6080","fee_limit":"1000","origin_energy_limit":1}`, http.StatusOK, &got)

	if got["confirmed"] != false {
		t.Errorf("confirmed = %v, want false", got["confirmed"])
	}

	for _, field := range []string{"fee", "energy_used", "failure"} {
		if _, ok := got[field]; ok {
			t.Errorf("%q is present without a receipt: %v", field, got[field])
		}
	}
}

func TestDeployRejectsBadRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		body       string
		chainErr   error
		wantStatus int
	}{
		{
			name:       "a wallet that does not exist",
			path:       "/api/wallets/9/deploy",
			body:       `{"bytecode":"6080","fee_limit":"1000","origin_energy_limit":1}`,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "a body that is not JSON",
			path:       "/api/wallets/0/deploy",
			body:       `{`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "a fee limit that is not a number",
			path:       "/api/wallets/0/deploy",
			body:       `{"bytecode":"6080","fee_limit":"lots","origin_energy_limit":1}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			// What the service answers for anything the chain would refuse
			// outright — empty bytecode, an unparseable ABI, no fee limit.
			name:       "input the chain would refuse",
			path:       "/api/wallets/0/deploy",
			body:       `{"bytecode":"","fee_limit":"1000","origin_energy_limit":1}`,
			chainErr:   fmt.Errorf("%w: bytecode is required", tron.ErrInvalidRequest),
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "a node that could not build it",
			path:       "/api/wallets/0/deploy",
			body:       `{"bytecode":"6080","fee_limit":"1000","origin_energy_limit":1}`,
			chainErr:   errors.New("no healthy nodes"),
			wantStatus: http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			chain := newChainFake()
			chain.deployErr = tt.chainErr

			srv := newServer(t, newWalletsFake(), chain)
			do(t, srv, http.MethodPost, tt.path, tt.body, tt.wantStatus, nil)
		})
	}
}

// Bytecode travels as hex, so a contract is twice its own size on the wire, and
// the ABI rides along with it. The default cap would refuse a perfectly
// ordinary contract.
func TestDeployAcceptsABodyLargerThanTheDefaultCap(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.deployed = tron.Deployed{TxID: "beefcafe", Address: "TContract"}

	srv := newServer(t, newWalletsFake(), chain)

	// 100 KB of hex: past the 64 KB every other endpoint is held to, and about
	// what a contract near the chain's own size limit comes to.
	body := `{"bytecode":"` + strings.Repeat("60", 50<<10) + `","fee_limit":"1000","origin_energy_limit":1}`

	do(t, srv, http.MethodPost, "/api/wallets/0/deploy", body, http.StatusOK, nil)

	if chain.deployCalls != 1 {
		t.Errorf("the deployment ran %d times, want once", chain.deployCalls)
	}
}

// The larger cap is the deploy endpoints' alone: every other write is a handful
// of short fields, and an unauthenticated POST must not be able to park half a
// megabyte in memory before the address on it is even looked at.
//
// The path is matched by suffix rather than by substring, so a route that only
// contains the word has to stay on the default — otherwise the cap is one
// invented path away from being lifted everywhere.
func TestDeployBodyLimitDoesNotLeakToOtherEndpoints(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("A", 100<<10)

	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "send",
			path: "/api/wallets/0/send",
			body: `{"asset":"trx","amount":"1","to":"` + huge + `"}`,
		},
		{
			// Not a route, so the status is beside the point — what matters is
			// that the body was capped at 64 KB on the way in.
			name: "a path that merely contains the word",
			path: "/api/wallets/0/deploy-anything",
			body: `{"bytecode":"` + huge + `"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			chain := newChainFake()
			srv := newServer(t, newWalletsFake(), chain)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				srv.URL+tt.path, strings.NewReader(tt.body))
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			req.Header.Set("Content-Type", "application/json")

			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("POST %s error = %v", tt.path, err)
			}
			defer res.Body.Close()

			if res.StatusCode == http.StatusOK {
				t.Fatalf("a 100 KB body was accepted by %s", tt.path)
			}

			if chain.sentFrom != "" || chain.deployCalls != 0 {
				t.Error("an oversized body still reached the chain service")
			}
		})
	}
}

// And the cap itself has to bite: without this, raising maxDeployBodyBytes to
// anything at all — or dropping the reader — leaves the suite green while the
// comment justifying the constant no longer holds.
func TestDeployRefusesABodyOverTheRaisedCap(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"deploy", "deploy-estimate"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			chain := newChainFake()
			srv := newServer(t, newWalletsFake(), chain)

			// Comfortably past 512 KB.
			body := `{"bytecode":"` + strings.Repeat("60", 400<<10) + `","fee_limit":"1000","origin_energy_limit":1}`

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				srv.URL+"/api/wallets/0/"+path, strings.NewReader(body))
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			req.Header.Set("Content-Type", "application/json")

			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("POST %s error = %v", path, err)
			}
			defer res.Body.Close()

			if res.StatusCode == http.StatusOK {
				t.Fatalf("an 800 KB body was accepted by /%s", path)
			}

			if chain.deployCalls != 0 || chain.costOf.Bytecode != "" {
				t.Error("an oversized deployment still reached the chain service")
			}
		})
	}
}
