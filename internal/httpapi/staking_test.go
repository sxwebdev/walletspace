package httpapi_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/tron"
)

// The fee is paid in TRX whatever asset is moving, so the dialog has to say
// when the sender cannot cover it — the node otherwise answers only after the
// transfer has been signed and broadcast.
func TestEstimateReportsTheShortfall(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.estimate = tron.Estimate{Fee: decimal.RequireFromString("27.3")}
	chain.shortfall = decimal.RequireFromString("27.3")

	srv := newServer(t, newWalletsFake(), chain)

	var got estimateBody
	do(t, srv, http.MethodPost, "/api/wallets/0/estimate",
		`{"asset":"usdt","to":"`+recipientAddr+`","amount":"10"}`, http.StatusOK, &got)

	if got.Shortfall != "27.3" {
		t.Errorf("shortfall = %q, want 27.3", got.Shortfall)
	}

	// The sender, not the recipient, and the asset as asked: tron.Shortfall adds
	// the amount to the need only for TRX, so a handler that hard-coded either
	// would still answer plausibly here while getting a TRX send badly wrong.
	if chain.sfFrom != walletAddr0 || chain.sfAsset != tron.AssetUSDT {
		t.Errorf("shortfall asked for %q/%q, want %q/usdt", chain.sfFrom, chain.sfAsset, walletAddr0)
	}

	if !chain.sfAmount.Equal(decimal.NewFromInt(10)) || !chain.sfEstimate.Fee.Equal(chain.estimate.Fee) {
		t.Errorf("shortfall priced %s against fee %s, want 10 against the estimate's own fee",
			chain.sfAmount, chain.sfEstimate.Fee)
	}
}

// A balance the service could not read must not fail an estimate that was
// priced successfully: the shortfall only adds a warning to it.
func TestEstimateSurvivesAFailedShortfallCheck(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.estimate = tron.Estimate{Fee: decimal.RequireFromString("1.1")}
	chain.shortfallErr = errors.New("node unreachable")

	srv := newServer(t, newWalletsFake(), chain)

	var got estimateBody
	do(t, srv, http.MethodPost, "/api/wallets/0/estimate",
		`{"asset":"trx","to":"`+recipientAddr+`","amount":"1"}`, http.StatusOK, &got)

	if got.Fee != "1.1" {
		t.Errorf("fee = %q, want the estimate to survive", got.Fee)
	}

	if got.Shortfall != "0" {
		t.Errorf("shortfall = %q, want 0 when it could not be worked out", got.Shortfall)
	}
}

func TestResources(t *testing.T) {
	t.Parallel()

	locked := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	chain := newChainFake()
	chain.resources = tron.Resources{
		Bandwidth:       tron.Pool{Available: decimal.NewFromInt(1245), Total: decimal.NewFromInt(1800)},
		Energy:          tron.Pool{Available: decimal.Zero, Total: decimal.Zero},
		StakedBandwidth: decimal.RequireFromString("100.5"),
		StakedEnergy:    decimal.NewFromInt(500),
		Unstaking:       decimal.NewFromInt(50),
		WithdrawableNow: decimal.NewFromInt(20),
		BandwidthPerTRX: decimal.RequireFromString("0.63189936"),
		EnergyPerTRX:    decimal.RequireFromString("73.814562"),
		// Far below the stake, which is the normal case for an account that has
		// been spending: consumed resource cannot be lent out.
		CanDelegateBandwidth: decimal.RequireFromString("308.062077"),
		CanDelegateEnergy:    decimal.NewFromInt(500),
		UnstakeSlots:         31,
		Pending: []tron.Unstake{
			{Resource: tron.ResourceBandwidth, Amount: decimal.NewFromInt(50), ExpireAt: locked},
		},
		Delegations: []tron.Delegation{
			{To: receiverAddr, Resource: tron.ResourceEnergy, Amount: decimal.NewFromInt(200)},
			{To: "TLocked", Resource: tron.ResourceBandwidth, Amount: decimal.NewFromInt(7), LockedUntil: locked},
		},
	}

	srv := newServer(t, newWalletsFake(), chain)

	var got resourcesBody
	do(t, srv, http.MethodGet, "/api/wallets/1/resources", "", http.StatusOK, &got)

	if chain.resourcesAddr != walletAddr1 {
		t.Errorf("read resources of %q, want the address of wallet 1", chain.resourcesAddr)
	}

	// Amounts travel as strings: the UI puts them straight back into a request,
	// and a float round-trip would quietly change them.
	if got.StakedBandwidth != "100.5" {
		t.Errorf("staked_bandwidth = %q, want 100.5", got.StakedBandwidth)
	}

	if got.Bandwidth.Available != "1245" || got.Bandwidth.Total != "1800" {
		t.Errorf("bandwidth pool = %+v, want 1245 of 1800", got.Bandwidth)
	}

	// The UI multiplies these to preview what an amount buys, so they have to
	// arrive at full precision rather than rounded to something displayable.
	if got.EnergyPerTRX != "73.814562" || got.BandwidthPerTRX != "0.63189936" {
		t.Errorf("rates = %q / %q, want them verbatim", got.BandwidthPerTRX, got.EnergyPerTRX)
	}

	// The dialog offers the whole stake without this, and the chain answers
	// "delegateBalance must be less than or equal to available
	// FreezeBandwidthV2 balance" — which names no number.
	if got.CanDelegateBandwidth != "308.062077" {
		t.Errorf("can_delegate_bandwidth = %q, want the node's 308.062077", got.CanDelegateBandwidth)
	}

	if got.UnstakeSlots != 31 {
		t.Errorf("unstake_slots = %d, want 31", got.UnstakeSlots)
	}

	if len(got.Pending) != 1 || got.Pending[0].Amount != "50" {
		t.Errorf("pending = %+v, want the single 50 TRX entry", got.Pending)
	}

	if len(got.Delegations) != 2 {
		t.Fatalf("got %d delegations, want 2", len(got.Delegations))
	}

	// The UI offers "Вернуть" only when there is no lock, so an unlocked
	// delegation must not carry a date at all.
	if got.Delegations[0].LockedUntil != nil {
		t.Errorf("unlocked delegation reported locked_until = %v", got.Delegations[0].LockedUntil)
	}

	if got.Delegations[1].LockedUntil == nil || !got.Delegations[1].LockedUntil.Equal(locked) {
		t.Errorf("locked delegation lost its lock: %+v", got.Delegations[1])
	}
}

// The UI iterates both lists without a guard, so they have to encode as arrays
// even when the account has nothing staked at all.
func TestResourcesEncodesEmptyListsAsArrays(t *testing.T) {
	t.Parallel()

	srv := newServer(t, newWalletsFake(), newChainFake())

	var raw map[string]any
	do(t, srv, http.MethodGet, "/api/wallets/0/resources", "", http.StatusOK, &raw)

	for _, key := range []string{"pending", "delegations"} {
		if _, ok := raw[key].([]any); !ok {
			t.Errorf("%s = %#v, want an empty array", key, raw[key])
		}
	}
}

func TestStakingOperationsPassTheRequestThrough(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		path     string
		body     string
		wantOp   string
		wantTo   string
		wantRes  tron.Resource
		wantAmnt string
	}{
		{
			name:     "stake",
			path:     "/api/wallets/1/stake",
			body:     `{"resource":"energy","amount":"100.5"}`,
			wantOp:   "stake",
			wantRes:  tron.ResourceEnergy,
			wantAmnt: "100.5",
		},
		{
			name:     "unstake",
			path:     "/api/wallets/1/unstake",
			body:     `{"resource":"bandwidth","amount":"7"}`,
			wantOp:   "unstake",
			wantRes:  tron.ResourceBandwidth,
			wantAmnt: "7",
		},
		{
			// Delegation counts in the resource itself, not in TRX: the
			// amount goes through untouched and the service converts it.
			name:     "delegate",
			path:     "/api/wallets/1/delegate",
			body:     `{"resource":"energy","amount":"14762.9124","to":"` + receiverAddr + `"}`,
			wantOp:   "delegate",
			wantTo:   receiverAddr,
			wantRes:  tron.ResourceEnergy,
			wantAmnt: "14762.9124",
		},
		{
			name:     "reclaim",
			path:     "/api/wallets/1/reclaim",
			body:     `{"resource":"energy","amount":"14762.9124","to":"` + receiverAddr + `"}`,
			wantOp:   "reclaim",
			wantTo:   receiverAddr,
			wantRes:  tron.ResourceEnergy,
			wantAmnt: "14762.9124",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			chain := newChainFake()
			srv := newServer(t, newWalletsFake(), chain)

			var got struct {
				Txid string `json:"txid"`
			}
			do(t, srv, http.MethodPost, tt.path, tt.body, http.StatusOK, &got)

			if got.Txid != "cafebabe" {
				t.Errorf("txid = %q, want the one the chain returned", got.Txid)
			}

			if chain.opName != tt.wantOp {
				t.Errorf("ran %q, want %q", chain.opName, tt.wantOp)
			}

			// Wallet 1, not 0: a handler that resolves the wrong wallet or puts
			// the receiver where the sender belongs would otherwise pass.
			if chain.opFrom != walletAddr1 {
				t.Errorf("from = %q, want the address of wallet 1", chain.opFrom)
			}

			if chain.opTo != tt.wantTo {
				t.Errorf("to = %q, want %q", chain.opTo, tt.wantTo)
			}

			if chain.opResource != tt.wantRes {
				t.Errorf("resource = %q, want %q", chain.opResource, tt.wantRes)
			}

			if !chain.opAmount.Equal(decimal.RequireFromString(tt.wantAmnt)) {
				t.Errorf("amount = %s, want %s", chain.opAmount, tt.wantAmnt)
			}

			if chain.opKey == nil {
				t.Error("the operation was signed with no key")
			}
		})
	}
}

// Neither takes a body: both act on every pending unstake the account has.
func TestWithdrawAndCancelNeedNoBody(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"withdraw", "cancel-unstakes"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			chain := newChainFake()
			srv := newServer(t, newWalletsFake(), chain)

			do(t, srv, http.MethodPost, "/api/wallets/1/"+path, "", http.StatusOK, nil)

			if chain.opFrom != walletAddr1 {
				t.Errorf("from = %q, want the address of wallet 1", chain.opFrom)
			}

			if chain.opKey == nil {
				t.Error("the operation was signed with no key")
			}
		})
	}
}

// The resource figure the UI shows is a TRX amount converted at the current
// rate, so converting it back rounds up past what is actually delegated and the
// chain refuses it. "max" asks the service to use the staked amount verbatim.
func TestReclaimMaxTakesTheWholeDelegation(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	srv := newServer(t, newWalletsFake(), chain)

	do(t, srv, http.MethodPost, "/api/wallets/1/reclaim",
		`{"resource":"energy","amount":"max","to":"`+receiverAddr+`"}`, http.StatusOK, nil)

	if chain.opName != "reclaim-all" {
		t.Errorf("ran %q, want reclaim-all", chain.opName)
	}

	if chain.opTo != receiverAddr || chain.opResource != tron.ResourceEnergy {
		t.Errorf("reclaimed %s from %q, want energy from TReceiver", chain.opResource, chain.opTo)
	}
}

// Only reclaiming has a "take all of it" meaning; anywhere else the sentinel is
// simply an amount that will not parse.
func TestOnlyReclaimAcceptsMax(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"stake", "unstake", "delegate"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			chain := newChainFake()
			srv := newServer(t, newWalletsFake(), chain)

			do(t, srv, http.MethodPost, "/api/wallets/0/"+path,
				`{"resource":"energy","amount":"max","to":"`+receiverAddr+`"}`, http.StatusBadRequest, nil)

			if chain.opName != "" {
				t.Errorf("%q was attempted for an unparsable amount", chain.opName)
			}
		})
	}
}

func TestStakingRejectsBadInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		body       string
		wantStatus int
	}{
		{
			name:       "unknown resource",
			path:       "/api/wallets/0/stake",
			body:       `{"resource":"cpu","amount":"1"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "missing resource",
			path:       "/api/wallets/0/stake",
			body:       `{"amount":"1"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "unparsable amount",
			path:       "/api/wallets/0/unstake",
			body:       `{"resource":"energy","amount":"lots"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "broken json",
			path:       "/api/wallets/0/delegate",
			body:       `{`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "non-numeric index",
			path:       "/api/wallets/abc/stake",
			body:       `{"resource":"energy","amount":"1"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "unknown wallet",
			path:       "/api/wallets/99/stake",
			body:       `{"resource":"energy","amount":"1"}`,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "unknown wallet on withdraw",
			path:       "/api/wallets/99/withdraw",
			body:       "",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			chain := newChainFake()
			srv := newServer(t, newWalletsFake(), chain)

			do(t, srv, http.MethodPost, tt.path, tt.body, tt.wantStatus, nil)

			if chain.opName != "" {
				t.Errorf("%q was attempted for invalid input", chain.opName)
			}
		})
	}
}

func TestStakingMapsValidationFailuresTo400(t *testing.T) {
	t.Parallel()

	wallets := newWalletsFake()
	chain := newChainFake()
	// Nothing about this needs a node, so blaming the upstream with a 502 would
	// mislead both clients and log readers.
	srv := newServer(t, wallets, chain)

	var got struct {
		Error string `json:"error"`
	}
	do(t, srv, http.MethodPost, "/api/wallets/0/delegate",
		`{"resource":"energy","amount":"1","to":"`+walletAddr0+`"}`, http.StatusBadRequest, &got)

	if !strings.Contains(got.Error, "delegate to itself") {
		t.Errorf("error = %q, want the validation message", got.Error)
	}

	// Refused by the handler, before either the chain or the mnemonic is
	// reached: the chain fake is left unarmed on purpose, so an answer that
	// came from it would show up as the wrong error message.
	if chain.opName != "" {
		t.Errorf("%q ran for a delegation to self", chain.opName)
	}

	if wallets.keyCalls != 0 {
		t.Errorf("the private key was derived %d times for a request that cannot succeed", wallets.keyCalls)
	}
}

func TestStakingPropagatesChainFailure(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.stakeErr = errors.New("broadcast transaction: bandwidth is not enough")

	srv := newServer(t, newWalletsFake(), chain)

	var got struct {
		Error string `json:"error"`
	}
	do(t, srv, http.MethodPost, "/api/wallets/0/stake",
		`{"resource":"energy","amount":"1"}`, http.StatusBadGateway, &got)

	if !strings.Contains(got.Error, "bandwidth is not enough") {
		t.Errorf("error = %q, want it to carry the chain failure", got.Error)
	}
}

func TestResourcesPropagatesChainFailure(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.resourcesErr = errors.New("read stake: node unreachable")

	srv := newServer(t, newWalletsFake(), chain)

	do(t, srv, http.MethodGet, "/api/wallets/0/resources", "", http.StatusBadGateway, nil)
}

type poolBody struct {
	Available string `json:"available"`
	Total     string `json:"total"`
}

type resourcesBody struct {
	Bandwidth poolBody `json:"bandwidth"`
	Energy    poolBody `json:"energy"`

	StakedBandwidth string `json:"staked_bandwidth"`
	StakedEnergy    string `json:"staked_energy"`
	Unstaking       string `json:"unstaking"`
	WithdrawableNow string `json:"withdrawable_now"`

	BandwidthPerTRX string `json:"bandwidth_per_trx"`
	EnergyPerTRX    string `json:"energy_per_trx"`

	CanDelegateBandwidth string `json:"can_delegate_bandwidth"`
	CanDelegateEnergy    string `json:"can_delegate_energy"`

	UnstakeSlots int64 `json:"unstake_slots"`

	Pending []struct {
		Resource string    `json:"resource"`
		Amount   string    `json:"amount"`
		ExpireAt time.Time `json:"expire_at"`
	} `json:"pending"`

	Delegations []struct {
		To          string     `json:"to"`
		Resource    string     `json:"resource"`
		Amount      string     `json:"amount"`
		LockedUntil *time.Time `json:"locked_until"`
	} `json:"delegations"`
}
