package httpapi

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/tronfaucet/internal/tron"
	"github.com/sxwebdev/tronfaucet/internal/wallet"
)

type poolResponse struct {
	Available string `json:"available"`
	Total     string `json:"total"`
}

type unstakeResponse struct {
	Resource string    `json:"resource"`
	Amount   string    `json:"amount"`
	ExpireAt time.Time `json:"expire_at"`
}

type delegationResponse struct {
	To       string `json:"to"`
	Resource string `json:"resource"`
	Amount   string `json:"amount"`
	// LockedUntil is omitted unless the delegation actually carries a lock, so
	// the UI can test for its presence instead of for a zero date.
	LockedUntil *time.Time `json:"locked_until,omitempty"`
}

type resourcesResponse struct {
	Bandwidth poolResponse `json:"bandwidth"`
	Energy    poolResponse `json:"energy"`

	StakedBandwidth string `json:"staked_bandwidth"`
	StakedEnergy    string `json:"staked_energy"`
	Unstaking       string `json:"unstaking"`
	WithdrawableNow string `json:"withdrawable_now"`

	// What one staked TRX yields right now, so the UI can price an amount in
	// resource units without a round-trip per keystroke.
	BandwidthPerTRX string `json:"bandwidth_per_trx"`
	EnergyPerTRX    string `json:"energy_per_trx"`

	// The node's own answer for how much stake may still be lent out. Shown,
	// not enforced — see tron.Resources.
	CanDelegateBandwidth string `json:"can_delegate_bandwidth"`
	CanDelegateEnergy    string `json:"can_delegate_energy"`

	UnstakeSlots int64 `json:"unstake_slots"`

	Pending     []unstakeResponse    `json:"pending"`
	Delegations []delegationResponse `json:"delegations"`
}

func toResourcesResponse(res tron.Resources) resourcesResponse {
	out := resourcesResponse{
		Bandwidth: poolResponse{
			Available: res.Bandwidth.Available.String(),
			Total:     res.Bandwidth.Total.String(),
		},
		Energy: poolResponse{
			Available: res.Energy.Available.String(),
			Total:     res.Energy.Total.String(),
		},
		StakedBandwidth:      res.StakedBandwidth.String(),
		StakedEnergy:         res.StakedEnergy.String(),
		Unstaking:            res.Unstaking.String(),
		WithdrawableNow:      res.WithdrawableNow.String(),
		BandwidthPerTRX:      res.BandwidthPerTRX.String(),
		EnergyPerTRX:         res.EnergyPerTRX.String(),
		CanDelegateBandwidth: res.CanDelegateBandwidth.String(),
		CanDelegateEnergy:    res.CanDelegateEnergy.String(),
		UnstakeSlots:         res.UnstakeSlots,
		// Built empty rather than nil so both encode as [] and the UI never has
		// to guard a null.
		Pending:     make([]unstakeResponse, 0, len(res.Pending)),
		Delegations: make([]delegationResponse, 0, len(res.Delegations)),
	}

	for _, item := range res.Pending {
		out.Pending = append(out.Pending, unstakeResponse{
			Resource: string(item.Resource),
			Amount:   item.Amount.String(),
			ExpireAt: item.ExpireAt,
		})
	}

	for _, item := range res.Delegations {
		row := delegationResponse{
			To:       item.To,
			Resource: string(item.Resource),
			Amount:   item.Amount.String(),
		}
		if !item.LockedUntil.IsZero() {
			locked := item.LockedUntil
			row.LockedUntil = &locked
		}

		out.Delegations = append(out.Delegations, row)
	}

	return out
}

func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	from, ok := s.resolveWallet(w, r)
	if !ok {
		return
	}

	res, err := s.chain.Resources(ctx, from.Address)
	if err != nil {
		if errors.Is(err, tron.ErrInvalidRequest) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		s.log.Warn("resources fetch failed", "address", from.Address, "error", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, toResourcesResponse(res))
}

// stakeRequest is the body the staking endpoints take. To is read only by the
// two operations that name a counterparty.
type stakeRequest struct {
	Resource string `json:"resource"`
	Amount   string `json:"amount"`
	To       string `json:"to"`
}

// stakeOp is a decoded staking request. amount is TRX for stake and unstake,
// and an amount of the resource itself for delegate and reclaim.
type stakeOp struct {
	from     wallet.Wallet
	resource tron.Resource
	amount   decimal.Decimal
	to       string
	// all asks to reclaim the whole delegation rather than a named amount.
	all bool
}

// resolveWallet reads the wallet index out of the path, writing the response
// itself when it names no wallet.
func (s *Server) resolveWallet(w http.ResponseWriter, r *http.Request) (wallet.Wallet, bool) {
	index, err := parseIndex(r.PathValue("index"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return wallet.Wallet{}, false
	}

	found, err := s.wallets.Get(index)
	if err != nil {
		s.writeStoreError(w, err)
		return wallet.Wallet{}, false
	}

	return found, true
}

// decodeStake parses the shared staking body. The resource name is checked here
// so an unknown one is a bad request rather than a refusal from the node.
//
// allowAll admits the "max" sentinel, which only reclaiming accepts: the amount
// is then whatever is actually delegated, which the caller cannot name exactly
// because the figure it sees has been through a rounding conversion.
func (s *Server) decodeStake(w http.ResponseWriter, r *http.Request, allowAll bool) (stakeOp, bool) {
	from, ok := s.resolveWallet(w, r)
	if !ok {
		return stakeOp{}, false
	}

	var req stakeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return stakeOp{}, false
	}

	op := stakeOp{from: from, to: req.To, all: allowAll && req.Amount == amountMax}

	switch req.Resource {
	case string(tron.ResourceBandwidth):
		op.resource = tron.ResourceBandwidth
	case string(tron.ResourceEnergy):
		op.resource = tron.ResourceEnergy
	default:
		writeError(w, http.StatusBadRequest, "unknown resource: "+req.Resource)
		return stakeOp{}, false
	}

	if !op.all {
		amount, err := decimal.NewFromString(req.Amount)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid amount: "+req.Amount)
			return stakeOp{}, false
		}
		op.amount = amount
	}

	return op, true
}

// runStake derives the signing key and performs one staking operation.
//
// The key is derived last, once everything that can be rejected already has
// been, so a malformed request never reaches the mnemonic.
func (s *Server) runStake(w http.ResponseWriter, name string, op stakeOp, call func(key *ecdsa.PrivateKey) (string, error)) {
	key, err := s.wallets.PrivateKey(op.from.Index)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	txid, err := call(key)
	if err != nil {
		// Input the chain never saw is the caller's fault, not the node's.
		if errors.Is(err, tron.ErrInvalidRequest) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		s.log.Error(name+" failed", append(op.logAttrs(), "error", err)...)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.log.Info(name, append(op.logAttrs(), "txid", txid)...)
	writeJSON(w, http.StatusOK, map[string]string{"txid": txid})
}

// logAttrs describes the operation for the log, leaving out what it does not
// have. Withdrawing, cancelling and reclaiming everything name no amount, and
// printing amount=0 for them reads as a transaction that moved nothing.
func (op stakeOp) logAttrs() []any {
	attrs := []any{"from", op.from.Address}

	if op.to != "" {
		attrs = append(attrs, "to", op.to)
	}

	if op.resource != "" {
		attrs = append(attrs, "resource", op.resource)
	}

	switch {
	case op.all:
		attrs = append(attrs, "amount", "all")
	case !op.amount.IsZero():
		attrs = append(attrs, "amount", op.amount.String())
	}

	return attrs
}

func (s *Server) handleStake(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	op, ok := s.decodeStake(w, r, false)
	if !ok {
		return
	}

	s.runStake(w, "staked", op, func(key *ecdsa.PrivateKey) (string, error) {
		return s.chain.Stake(ctx, op.from.Address, op.resource, op.amount, key)
	})
}

func (s *Server) handleUnstake(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	op, ok := s.decodeStake(w, r, false)
	if !ok {
		return
	}

	s.runStake(w, "unstaked", op, func(key *ecdsa.PrivateKey) (string, error) {
		return s.chain.Unstake(ctx, op.from.Address, op.resource, op.amount, key)
	})
}

func (s *Server) handleDelegate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	op, ok := s.decodeStake(w, r, false)
	if !ok {
		return
	}

	s.runStake(w, "delegated", op, func(key *ecdsa.PrivateKey) (string, error) {
		return s.chain.Delegate(ctx, op.from.Address, op.to, op.resource, op.amount, key)
	})
}

func (s *Server) handleReclaim(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	op, ok := s.decodeStake(w, r, true)
	if !ok {
		return
	}

	s.runStake(w, "reclaimed", op, func(key *ecdsa.PrivateKey) (string, error) {
		if op.all {
			return s.chain.ReclaimAll(ctx, op.from.Address, op.to, op.resource, key)
		}

		return s.chain.Reclaim(ctx, op.from.Address, op.to, op.resource, op.amount, key)
	})
}

// handleWithdrawUnstaked and handleCancelUnstakes take no body: both act on
// every pending unstake the account has.
func (s *Server) handleWithdrawUnstaked(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	from, ok := s.resolveWallet(w, r)
	if !ok {
		return
	}

	op := stakeOp{from: from}
	s.runStake(w, "withdrew unstaked", op, func(key *ecdsa.PrivateKey) (string, error) {
		return s.chain.WithdrawUnstaked(ctx, from.Address, key)
	})
}

func (s *Server) handleCancelUnstakes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	from, ok := s.resolveWallet(w, r)
	if !ok {
		return
	}

	op := stakeOp{from: from}
	s.runStake(w, "cancelled unstakes", op, func(key *ecdsa.PrivateKey) (string, error) {
		return s.chain.CancelUnstakes(ctx, from.Address, key)
	})
}
