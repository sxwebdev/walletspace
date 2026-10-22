package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/operation"
	"github.com/sxwebdev/walletspace/internal/refuel"
	"golang.org/x/sync/errgroup"
)

type PlatformOption func(*Platform)

func WithRefuelProvider(provider refuel.Provider) PlatformOption {
	return func(p *Platform) {
		if provider != nil {
			p.refuel = provider
		}
	}
}

type refuelCandidate struct {
	account account.Account
	network network.Network
}

func nativeAsset(item network.Network) chain.Asset {
	return chain.Asset{ID: item.ID + ":native", NetworkID: item.ID, Kind: "native", Symbol: item.Native.Symbol, Decimals: item.Native.Decimals}
}

func (p *Platform) quoteRefuel(w http.ResponseWriter, r *http.Request) {
	_, item, transfer, ok := p.decodePlatformTransfer(w, r)
	if !ok {
		return
	}
	if item.Family != network.FamilyEVM || item.Testnet || transfer.Asset.Kind != "erc20" || transfer.Action != "" {
		writeError(w, http.StatusBadRequest, "gas top-ups are available for mainnet EVM token transfers")
		return
	}
	if active, found, err := p.refuelPlans.Active(r.PathValue("space_id"), item.ID, transfer.AccountID); err != nil {
		p.writePlatformError(w, err)
		return
	} else if found {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "A gas top-up is already pending for this wallet. Resume it instead of paying again.", "pending_plan_id": active.ID})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	if transfer.Amount == "max" {
		var err error
		transfer.Amount, err = p.evm.Balance(ctx, item.ID, transfer.From, transfer.Asset)
		if err != nil {
			p.writePlatformError(w, err)
			return
		}
	}
	estimate, err := p.evm.EstimateFunding(ctx, item.ID, transfer)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	shortfall, _ := decimal.NewFromString(estimate.FeeShortfall)
	if !shortfall.IsPositive() {
		writeJSON(w, http.StatusOK, map[string]any{"estimate": estimate, "quotes": []refuel.Plan{}})
		return
	}
	accounts, err := p.spaces.Accounts(r.PathValue("space_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	var candidates []refuelCandidate
	// Restrict each interactive request to 32 candidates; the user can select an
	// individual source explicitly when managing a larger collection of wallets.
	for _, sourceAccount := range accounts {
		for _, source := range p.networks.List() {
			source = p.effectiveNetwork(source)
			if !source.Enabled || source.Testnet || source.Family != network.FamilyEVM || !sourceAccount.BoundTo(source.ID) ||
				(sourceAccount.ID == transfer.AccountID && source.ID == item.ID) {
				continue
			}
			if filter := r.URL.Query().Get("source_account_id"); filter != "" && sourceAccount.ID != filter {
				continue
			}
			if filter := r.URL.Query().Get("source_network_id"); filter != "" && source.ID != filter {
				continue
			}
			id, _ := strconv.ParseInt(source.ChainID, 10, 64)
			if source.ID != item.ID && !refuel.SupportedSource(id) {
				continue
			}
			candidates = append(candidates, refuelCandidate{sourceAccount, source})
		}
	}
	totalCandidates := len(candidates)
	if len(candidates) > 32 {
		candidates = candidates[:32]
	}
	results := make([]*refuel.Plan, len(candidates))
	failures := make([]string, len(candidates))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	for i, candidate := range candidates {
		group.Go(func() error {
			plan, err := p.buildRefuelPlan(groupCtx, item, transfer, estimate, candidate)
			if err != nil {
				failures[i] = fmt.Sprintf("%s · %s: %s", candidate.account.Label, candidate.network.Name, config.RedactError(err))
				return nil
			}
			results[i] = &plan
			return nil
		})
	}
	_ = group.Wait()
	plans := make([]refuel.Plan, 0, len(results))
	notes := make([]string, 0)
	for i, plan := range results {
		if plan != nil {
			plans = append(plans, *plan)
		} else if failures[i] != "" {
			notes = append(notes, failures[i])
		}
	}
	slices.SortStableFunc(plans, func(a, b refuel.Plan) int {
		if a.TotalUSD == "" && b.TotalUSD != "" {
			return 1
		}
		if b.TotalUSD == "" && a.TotalUSD != "" {
			return -1
		}
		av, _ := decimal.NewFromString(a.TotalUSD)
		bv, _ := decimal.NewFromString(b.TotalUSD)
		return av.Cmp(bv)
	})
	saved := make([]refuel.Plan, 0, len(plans))
	for _, plan := range plans {
		stored, err := p.refuelPlans.Put(r.PathValue("space_id"), plan)
		if err != nil {
			p.writePlatformError(w, err)
			return
		}
		saved = append(saved, stored)
	}
	writeJSON(w, http.StatusOK, map[string]any{"estimate": estimate, "quotes": saved, "unavailable": notes, "sources_checked": len(candidates), "sources_total": totalCandidates})
}

func (p *Platform) buildRefuelPlan(ctx context.Context, destination network.Network, transfer chain.TransferRequest, estimate chain.TransferEstimate, source refuelCandidate) (refuel.Plan, error) {
	address := source.account.Addresses[account.FamilyEVM]
	balance, err := p.evm.Balance(ctx, source.network.ID, address, nativeAsset(source.network))
	if err != nil {
		return refuel.Plan{}, err
	}
	available, err := decimal.NewFromString(balance)
	if err != nil || !available.IsPositive() {
		return refuel.Plan{}, errors.New("no spendable native balance")
	}
	fee, _ := decimal.NewFromString(estimate.Fee)
	current, _ := decimal.NewFromString(estimate.NativeBalance)
	// Ten percent of the actual fee budget, not a fixed $1/$5 refill. Provider
	// minimums are shown separately when they dominate this small reserve.
	needed := fee.Mul(decimal.RequireFromString("1.10")).Sub(current)
	needed = needed.Shift(int32(destination.Native.Decimals)).Ceil().Shift(-int32(destination.Native.Decimals))
	var quote refuel.Quote
	if source.network.ID == destination.ID {
		priceUSD := decimal.Zero
		if snapshot, err := p.prices.Quotes(ctx, []string{source.network.NativePrice}); err == nil {
			priceUSD = snapshot.Quotes[source.network.NativePrice].Current
		}
		quote = refuel.Quote{Provider: "Direct transfer", To: transfer.From, Deposit: needed.Shift(int32(destination.Native.Decimals)).BigInt().String(), Expected: needed.Shift(int32(destination.Native.Decimals)).BigInt().String(), ExpiresAt: time.Now().Add(90 * time.Second), SourcePriceUSD: priceUSD, DepositUSD: needed.Mul(priceUSD)}
	} else {
		sourceID, _ := strconv.ParseInt(source.network.ChainID, 10, 64)
		destID, _ := strconv.ParseInt(destination.ChainID, 10, 64)
		quote, err = p.refuel.Quote(ctx, refuel.Request{SourceChain: sourceID, DestinationChain: destID, From: address, Recipient: transfer.From, Needed: needed.Shift(int32(destination.Native.Decimals)).BigInt()})
		if err != nil {
			return refuel.Plan{}, err
		}
	}
	deposit, err := decimal.NewFromString(quote.Deposit)
	if err != nil || !deposit.IsPositive() {
		return refuel.Plan{}, errors.New("invalid top-up deposit")
	}
	deposit = deposit.Shift(-int32(source.network.Native.Decimals))
	expected, err := decimal.NewFromString(quote.Expected)
	if err != nil || expected.Shift(-int32(destination.Native.Decimals)).LessThan(needed) {
		return refuel.Plan{}, errors.New("top-up cannot cover the transfer fee")
	}
	expected = expected.Shift(-int32(destination.Native.Decimals))
	sourceReq := chain.TransferRequest{AccountID: source.account.ID, From: address, To: quote.To, Amount: deposit.String(), Asset: nativeAsset(source.network), Data: common.FromHex(quote.Data)}
	sourceEstimate, err := p.evm.EstimateFunding(ctx, source.network.ID, sourceReq)
	if err != nil {
		return refuel.Plan{}, err
	}
	missing, _ := decimal.NewFromString(sourceEstimate.FeeShortfall)
	if missing.IsPositive() {
		return refuel.Plan{}, errors.New("native balance does not cover deposit and source gas")
	}
	sourceFee, _ := decimal.NewFromString(sourceEstimate.Fee)
	total := deposit.Add(sourceFee)
	totalUSD := ""
	if quote.SourcePriceUSD.IsPositive() {
		totalUSD = total.Mul(quote.SourcePriceUSD).String()
	}
	expectedFee, _ := decimal.NewFromString(estimate.EstimatedFee)
	return refuel.Plan{NetworkID: destination.ID, Target: refuel.Target{AccountID: transfer.AccountID, AssetID: transfer.Asset.ID, To: transfer.To, Amount: transfer.Amount}, Recipient: transfer.From,
		SourceNetworkID: source.network.ID, SourceAccountID: source.account.ID, SourceAddress: address, SourceLabel: source.account.Label, Quote: quote,
		SourceEstimate: sourceEstimate, TargetEstimate: estimate, Required: needed.String(), Expected: expected.String(), Remainder: current.Add(expected).Sub(expectedFee).String(), TotalSource: total.String(), TotalUSD: totalUSD}, nil
}

func (p *Platform) getRefuelPlan(w http.ResponseWriter, r *http.Request) (refuel.Plan, bool) {
	plan, err := p.refuelPlans.Get(r.PathValue("space_id"), r.PathValue("plan_id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return refuel.Plan{}, false
	}
	if plan.NetworkID != r.PathValue("network_id") {
		writeError(w, http.StatusBadRequest, "top-up belongs to another network")
		return refuel.Plan{}, false
	}
	return plan, true
}

func (p *Platform) sendRefuel(w http.ResponseWriter, r *http.Request) {
	if !p.requireSendConfirmation(w, r) {
		return
	}
	plan, ok := p.getRefuelPlan(w, r)
	if !ok {
		return
	}
	// A plan has one durable operation key. Retries, including a lost response
	// across restarts, can only return that operation and can never sign twice.
	key := "refuel-" + plan.ID
	requestHash, err := operation.RequestHash(plan.ID)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	existing, repeated, err := p.operations.Begin(r.PathValue("space_id"), key, requestHash, plan.SourceNetworkID)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	if repeated {
		if !rejectIncompleteReplay(w, existing) {
			writeJSON(w, http.StatusOK, existing)
		}
		return
	}
	fail := func(err error) {
		_ = p.refuelPlans.Mark(r.PathValue("space_id"), plan.ID, true)
		_, updateErr := p.operations.Update(r.PathValue("space_id"), key, "", operation.StatusFailed)
		if updateErr != nil {
			p.writePlatformError(w, updateErr)
		} else {
			p.writePlatformError(w, err)
		}
	}
	if !plan.Quote.ExpiresAt.After(time.Now()) {
		fail(fmt.Errorf("%w: gas quote expired; calculate again", chain.ErrFeeChanged))
		return
	}
	source, err := p.enabledNetwork(plan.SourceNetworkID)
	if err != nil {
		fail(err)
		return
	}
	if _, err := p.enabledNetwork(plan.NetworkID); err != nil {
		fail(err)
		return
	}
	if err := p.refuelPlans.Claim(r.PathValue("space_id"), plan.ID); err != nil {
		fail(err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	destination, err := p.enabledNetwork(plan.NetworkID)
	if err != nil {
		fail(err)
		return
	}
	targetAsset, found := findAsset(p.assets.List(destination), plan.Target.AssetID)
	if !found {
		fail(fmt.Errorf("%w: the token is no longer configured", chain.ErrInvalidRequest))
		return
	}
	targetNow, err := p.evm.EstimateFunding(ctx, destination.ID, chain.TransferRequest{From: plan.Recipient, To: plan.Target.To, Amount: plan.Target.Amount, Asset: targetAsset})
	if err != nil {
		fail(err)
		return
	}
	shortfall, _ := decimal.NewFromString(targetNow.FeeShortfall)
	expected, _ := decimal.NewFromString(plan.Expected)
	if !shortfall.IsPositive() || shortfall.GreaterThan(expected) {
		fail(fmt.Errorf("%w: destination gas requirement changed; calculate the token transfer again", chain.ErrFeeChanged))
		return
	}
	deposit, _ := decimal.NewFromString(plan.Quote.Deposit)
	req := chain.TransferRequest{AccountID: plan.SourceAccountID, From: plan.SourceAddress, To: plan.Quote.To, Amount: deposit.Shift(-int32(source.Native.Decimals)).String(), Asset: nativeAsset(source), Data: common.FromHex(plan.Quote.Data),
		Approved: approvalFromEstimate(plan.SourceEstimate), BeforeBroadcast: func(hash string) error {
			if !plan.Quote.ExpiresAt.After(time.Now()) {
				return fmt.Errorf("%w: gas quote expired before broadcast", chain.ErrFeeChanged)
			}
			_, err := p.operations.Update(r.PathValue("space_id"), key, hash, operation.StatusBroadcasting)
			return err
		}}
	fresh, err := p.evm.EstimateFunding(ctx, source.ID, req)
	if err != nil {
		fail(err)
		return
	}
	freshFee, _ := decimal.NewFromString(fresh.Fee)
	approvedFee, _ := decimal.NewFromString(plan.SourceEstimate.Fee)
	missing, _ := decimal.NewFromString(fresh.FeeShortfall)
	if freshFee.GreaterThan(approvedFee) || missing.IsPositive() {
		fail(fmt.Errorf("%w: source balance or fee changed; calculate again", chain.ErrFeeChanged))
		return
	}
	var transaction chain.Transaction
	err = p.spaces.WithSigner(ctx, r.PathValue("space_id"), plan.SourceAccountID, source.ID, account.FamilyEVM, func(signer chain.Signer) error {
		var err error
		transaction, err = p.evm.Send(ctx, source.ID, req, signer)
		return err
	})
	status := transaction.Status
	if err != nil {
		status = operation.StatusFailed
		if errors.Is(err, chain.ErrBroadcastUnknown) {
			status = operation.StatusBroadcastUnknown
		}
	}
	updated, updateErr := p.operations.Update(r.PathValue("space_id"), key, transaction.Hash, status)
	if updateErr != nil {
		p.writePlatformError(w, updateErr)
		return
	}
	if err != nil && status == operation.StatusFailed {
		_ = p.refuelPlans.Mark(r.PathValue("space_id"), plan.ID, true)
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, updated)
}

func approvalFromEstimate(estimate chain.TransferEstimate) *chain.FeeApproval {
	return &chain.FeeApproval{FeeModel: estimate.FeeModel, GasLimit: estimate.GasLimit, MaxFeePerGas: estimate.MaxFeePerGas, MaxPriorityFeePerGas: estimate.MaxPriorityFeePerGas}
}

func (p *Platform) refuelStatus(w http.ResponseWriter, r *http.Request) {
	plan, ok := p.getRefuelPlan(w, r)
	if !ok {
		return
	}
	op, found, err := p.operations.Get(r.PathValue("space_id"), "refuel-"+plan.ID)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, map[string]any{"status": "not_started", "plan": plan})
		return
	}
	if op.Status == operation.StatusFailed {
		writeJSON(w, http.StatusOK, map[string]any{"status": "failed", "operation": op, "plan": plan})
		return
	}
	if op.TxHash == "" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "operation": op, "plan": plan})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	sourceStatus, err := p.evm.Transaction(ctx, plan.SourceNetworkID, op.TxHash)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	state := "pending"
	if sourceStatus.Status == "failed" {
		state = "failed"
	} else if sourceStatus.Status == "confirmed" {
		if plan.SourceNetworkID == plan.NetworkID {
			state = "delivered"
		} else {
			dest, err := p.networks.Get(plan.NetworkID)
			if err != nil {
				p.writePlatformError(w, err)
				return
			}
			id, _ := strconv.ParseInt(dest.ChainID, 10, 64)
			delivery, err := p.refuel.Delivery(ctx, op.TxHash, id, plan.Recipient)
			if err != nil {
				p.writePlatformError(w, err)
				return
			}
			if delivery.Status == "cancelled" {
				state = "cancelled"
			} else if delivery.Status == "delivered" {
				minimum, _ := decimal.NewFromString(plan.Required)
				delivered, err := p.evm.NativeDelivery(ctx, plan.NetworkID, delivery.Hash, plan.Recipient, minimum.Shift(int32(dest.Native.Decimals)).BigInt())
				if err != nil {
					p.writePlatformError(w, err)
					return
				}
				if delivered {
					state = "delivered"
				}
			}
		}
	}
	if state == "delivered" || state == "failed" || state == "cancelled" {
		if err := p.refuelPlans.Mark(r.PathValue("space_id"), plan.ID, true); err != nil {
			p.writePlatformError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": state, "operation": op, "plan": plan})
}
