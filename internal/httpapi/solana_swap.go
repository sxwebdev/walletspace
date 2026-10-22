package httpapi

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/chain"
	solanachain "github.com/sxwebdev/walletspace/internal/chain/solana"
	"github.com/sxwebdev/walletspace/internal/network"
)

// solanaSwapper is what the Solana adapter offers for swaps. It is reached
// through the family's adapter, behind the network's solana_swap capability.
type solanaSwapper interface {
	QuoteSwap(ctx context.Context, networkID string, req solanachain.SwapQuoteRequest) (solanachain.SwapQuote, error)
	Swap(ctx context.Context, networkID string, req solanachain.SwapRequest, key chain.Key) (chain.Transaction, error)
}

// solanaSwapQuoteRequest asks what selling Amount of one asset for another
// would return.
type solanaSwapQuoteRequest struct {
	InAssetID   string `json:"in_asset_id"`
	OutAssetID  string `json:"out_asset_id"`
	Amount      string `json:"amount"`
	SlippageBps uint16 `json:"slippage_bps"`
}

// solanaSwapRequest is a swap the user approved: the quote's request, its
// minimum, route and fee, echoed back. Everything the signature commits to is
// in it, so the idempotency key binds the swap to exactly what was shown.
type solanaSwapRequest struct {
	InAssetID   string                `json:"in_asset_id"`
	OutAssetID  string                `json:"out_asset_id"`
	Amount      string                `json:"amount"`
	SlippageBps uint16                `json:"slippage_bps"`
	MinOut      string                `json:"min_out"`
	Route       solanachain.SwapRoute `json:"route"`

	ComputeUnitLimit              uint64 `json:"compute_unit_limit"`
	ComputeUnitPriceMicroLamports uint64 `json:"compute_unit_price_micro_lamports"`
	TokenAccountsCreated          int    `json:"token_accounts_created"`
	TokenAccountRent              string `json:"token_account_rent,omitempty"`
	Fee                           string `json:"fee"`
}

// solanaSwapAccount resolves a swap request's network, account, address,
// assets and the adapter that swaps.
func (p *Platform) solanaSwapAccount(
	w http.ResponseWriter, r *http.Request, inID, outID string,
) (network.Network, account.Account, string, chain.Asset, chain.Asset, solanaSwapper, bool) {
	fail := func() (network.Network, account.Account, string, chain.Asset, chain.Asset, solanaSwapper, bool) {
		return network.Network{}, account.Account{}, "", chain.Asset{}, chain.Asset{}, nil, false
	}
	item, err := p.networks.Get(r.PathValue("network_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return fail()
	}
	item = p.effectiveNetwork(item)
	if item.Family != network.FamilySolana || !item.Enabled || !item.Capabilities.SolanaSwap {
		writeError(w, http.StatusBadRequest, "swaps need an enabled Solana network that offers them")
		return fail()
	}
	adapter, err := p.chains.For(network.FamilySolana)
	if err != nil {
		p.writePlatformError(w, err)
		return fail()
	}
	swapper, ok := adapter.(solanaSwapper)
	if !ok {
		writeError(w, http.StatusNotImplemented, "the Solana adapter does not swap")
		return fail()
	}
	assets := p.assets.List(item)
	in, inOK := findAsset(assets, inID)
	out, outOK := findAsset(assets, outID)
	if !inOK || !outOK {
		writeError(w, http.StatusBadRequest, "asset does not belong to this network")
		return fail()
	}
	accounts, err := p.spaces.Accounts(r.PathValue("space_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return fail()
	}
	for _, candidate := range accounts {
		if candidate.ID == r.PathValue("account_id") && candidate.BoundTo(item.ID) {
			if address := candidate.Addresses[account.FamilySolana]; address != "" {
				return item, candidate, address, in, out, swapper, true
			}
		}
	}
	writeError(w, http.StatusNotFound, "account is not available for Solana")
	return fail()
}

func (p *Platform) solanaSwapQuote(w http.ResponseWriter, r *http.Request) {
	var request solanaSwapQuoteRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item, _, address, in, out, swapper, ok := p.solanaSwapAccount(w, r, request.InAssetID, request.OutAssetID)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	quote, err := swapper.QuoteSwap(ctx, item.ID, solanachain.SwapQuoteRequest{
		From: address, In: in, Out: out, Amount: request.Amount, SlippageBps: request.SlippageBps,
	})
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, quote)
}

func (p *Platform) solanaSwap(w http.ResponseWriter, r *http.Request) {
	if !p.requireSendConfirmation(w, r) {
		return
	}
	var request solanaSwapRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	item, accountItem, address, in, out, swapper, ok := p.solanaSwapAccount(w, r, request.InAssetID, request.OutAssetID)
	if !ok {
		return
	}
	operationKey, ok := p.beginChainOperation(w, r, item.ID, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	spaceID := r.PathValue("space_id")
	// The swap says its expiry height before signing; the recording signer
	// writes it with the transaction id.
	var lastValidBlockHeight uint64
	var transaction chain.Transaction
	err := p.spaces.WithKey(ctx, spaceID, accountItem.ID, item.ID, account.FamilySolana, func(key chain.Key) error {
		signer, ok := key.(chain.MessageSigner)
		if !ok {
			return fmt.Errorf("%w: Solana transactions are signed as messages", chain.ErrSignerMismatch)
		}
		recording := p.recordingMessageSigner(spaceID, operationKey, signer, func() uint64 { return lastValidBlockHeight })
		var swapErr error
		transaction, swapErr = swapper.Swap(ctx, item.ID, solanachain.SwapRequest{
			From: address, In: in, Out: out, Amount: request.Amount, SlippageBps: request.SlippageBps,
			MinOut: request.MinOut, Route: request.Route,
			Approved: solanachain.SwapApproval{
				ComputeUnitLimit: request.ComputeUnitLimit, ComputeUnitPriceMicroLamports: request.ComputeUnitPriceMicroLamports,
				TokenAccountsCreated: request.TokenAccountsCreated, TokenAccountRent: request.TokenAccountRent, Fee: request.Fee,
			},
			BeforeSigning: func(height uint64) error {
				lastValidBlockHeight = height
				return nil
			},
		}, recording)
		return swapErr
	})
	p.finishTronOperation(w, r, item.ID, operationKey, transaction.Hash, err)
}
