package httpapi

import (
	"context"
	"net/http"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/chain"
	tronchain "github.com/sxwebdev/walletspace/internal/chain/tron"
)

type tronSwapQuoteRequest struct {
	// In is the asset sold: "trx" or "usdt".
	In        string `json:"in"`
	Amount    string `json:"amount"`
	Unlimited bool   `json:"unlimited"`
}

// tronSwapRequest is one step of a swap. Everything the signature commits to
// is in it — amount, minimum, route, approval scope — so the idempotency key
// binds the step to exactly what the user saw.
type tronSwapRequest struct {
	In        string              `json:"in"`
	Amount    string              `json:"amount"`
	MinOut    string              `json:"min_out"`
	Route     tronchain.SwapRoute `json:"route"`
	Step      string              `json:"step"`
	Unlimited bool                `json:"unlimited"`
}

func swapAsset(value string) (tronchain.Asset, bool) {
	switch tronchain.Asset(value) {
	case tronchain.AssetTRX, tronchain.AssetUSDT:
		return tronchain.Asset(value), true
	default:
		return "", false
	}
}

func (p *Platform) tronSwapQuote(w http.ResponseWriter, r *http.Request) {
	var request tronSwapQuoteRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	in, ok := swapAsset(request.In)
	if !ok {
		writeError(w, http.StatusBadRequest, `in must be "trx" or "usdt"`)
		return
	}
	amount, err := decimal.NewFromString(request.Amount)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid amount")
		return
	}
	item, _, address, ok := p.tronAccount(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	quotes, err := p.tron.QuoteSwap(ctx, item.ID, address, tronchain.SwapQuoteRequest{
		In: in, Amount: amount, Unlimited: request.Unlimited,
	})
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, quotes)
}

func (p *Platform) tronSwap(w http.ResponseWriter, r *http.Request) {
	if !p.requireSendConfirmation(w, r) {
		return
	}
	var request tronSwapRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	in, ok := swapAsset(request.In)
	if !ok {
		writeError(w, http.StatusBadRequest, `in must be "trx" or "usdt"`)
		return
	}
	amount, err := decimal.NewFromString(request.Amount)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid amount")
		return
	}
	minOut, err := decimal.NewFromString(request.MinOut)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid min_out")
		return
	}
	switch tronchain.SwapStepKind(request.Step) {
	case tronchain.SwapStepApprove, tronchain.SwapStepPermit2, tronchain.SwapStepSwap:
	default:
		writeError(w, http.StatusBadRequest, `step must be "approve", "permit2" or "swap"`)
		return
	}
	item, accountItem, address, ok := p.tronAccount(w, r)
	if !ok {
		return
	}
	operationKey, ok := p.beginChainOperation(w, r, item.ID, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	var txID string
	err = p.spaces.WithSigner(ctx, r.PathValue("space_id"), accountItem.ID, item.ID, account.FamilyTron,
		func(signer chain.Signer) error {
			signer = p.recordingTronSigner(r.PathValue("space_id"), operationKey, signer)
			var swapErr error
			txID, swapErr = p.tron.Swap(ctx, item.ID, address, tronchain.SwapRequest{
				In: in, Amount: amount, MinOut: minOut, Route: request.Route,
				Step: tronchain.SwapStepKind(request.Step), Unlimited: request.Unlimited,
			}, signer)
			return swapErr
		})
	p.finishTronOperation(w, r, item.ID, operationKey, txID, err)
}
