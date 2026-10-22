package httpapi

import (
	"context"
	"net/http"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/chain"
	solanachain "github.com/sxwebdev/walletspace/internal/chain/solana"
	"github.com/sxwebdev/walletspace/internal/network"
)

// solanaTokenAccounts is what the Solana adapter offers for closing empty
// token accounts.
type solanaTokenAccounts interface {
	EmptyTokenAccounts(ctx context.Context, networkID, owner string) ([]solanachain.EmptyTokenAccount, error)
	EstimateClose(ctx context.Context, networkID string, req solanachain.CloseRequest) (solanachain.CloseEstimate, error)
	Close(ctx context.Context, networkID string, req solanachain.CloseRequest, key chain.Key) (chain.Transaction, error)
}

// tokenAccountDTO is one empty token account as the UI shows it.
type tokenAccountDTO struct {
	Address string `json:"address"`
	Mint    string `json:"mint"`
	// Symbol is the asset's symbol when the wallet knows the mint.
	Symbol string `json:"symbol,omitempty"`
	Rent   string `json:"rent"`
}

// closeAccountsDTO is the close request: the accounts, and on close the fee
// the estimate showed.
type closeAccountsDTO struct {
	Accounts                      []string `json:"accounts"`
	FeeModel                      string   `json:"fee_model,omitempty"`
	Fee                           string   `json:"fee,omitempty"`
	ComputeUnitLimit              uint64   `json:"compute_unit_limit,omitempty"`
	ComputeUnitPriceMicroLamports uint64   `json:"compute_unit_price_micro_lamports,omitempty"`
}

// solanaAccount resolves a route's network and account for closing token
// accounts, refusing as the Tron-only routes do: a network that is not an
// enabled Solana network with the capability is a bad request, an account
// without a Solana address on it is not found.
func (p *Platform) solanaAccount(
	w http.ResponseWriter, r *http.Request,
) (network.Network, account.Account, string, solanaTokenAccounts, bool) {
	item, err := p.networks.Get(r.PathValue("network_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return network.Network{}, account.Account{}, "", nil, false
	}
	item = p.effectiveNetwork(item)
	adapter, adapterErr := p.chains.For(network.FamilySolana)
	closer, ok := adapter.(solanaTokenAccounts)
	if item.Family != network.FamilySolana || !item.Enabled || !item.Capabilities.SolanaCloseAccounts || adapterErr != nil || !ok {
		writeError(w, http.StatusBadRequest, "closing token accounts requires an enabled Solana network that supports it")
		return network.Network{}, account.Account{}, "", nil, false
	}
	accounts, err := p.spaces.Accounts(r.PathValue("space_id"))
	if err != nil {
		p.writePlatformError(w, err)
		return network.Network{}, account.Account{}, "", nil, false
	}
	for _, itemAccount := range accounts {
		if itemAccount.ID == r.PathValue("account_id") && itemAccount.BoundTo(item.ID) {
			if address := itemAccount.Addresses[account.FamilySolana]; address != "" {
				return item, itemAccount, address, closer, true
			}
		}
	}
	writeError(w, http.StatusNotFound, "account is not available for Solana")
	return network.Network{}, account.Account{}, "", nil, false
}

// tokenAccountDTOs adds the symbols the wallet knows for the mints.
func (p *Platform) tokenAccountDTOs(item network.Network, accounts []solanachain.EmptyTokenAccount) []tokenAccountDTO {
	symbols := make(map[string]string)
	for _, asset := range p.assets.List(item) {
		if asset.Kind == "spl" {
			symbols[asset.Contract] = asset.Symbol
		}
	}
	out := make([]tokenAccountDTO, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, tokenAccountDTO{
			Address: account.Address, Mint: account.Mint, Symbol: symbols[account.Mint], Rent: account.Rent,
		})
	}
	return out
}

// emptyTokenAccounts lists the account's token accounts that can be closed.
func (p *Platform) emptyTokenAccounts(w http.ResponseWriter, r *http.Request) {
	item, _, address, closer, ok := p.solanaAccount(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	listed, err := closer.EmptyTokenAccounts(ctx, item.ID, address)
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"network_id": item.ID, "accounts": p.tokenAccountDTOs(item, listed),
		"max_per_transaction": solanachain.MaxCloseAccounts,
	})
}

// estimateCloseTokenAccounts prices closing the named accounts.
func (p *Platform) estimateCloseTokenAccounts(w http.ResponseWriter, r *http.Request) {
	item, _, address, closer, ok := p.solanaAccount(w, r)
	if !ok {
		return
	}
	var request closeAccountsDTO
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	estimate, err := closer.EstimateClose(ctx, item.ID, solanachain.CloseRequest{Owner: address, Accounts: request.Accounts})
	if err != nil {
		p.writePlatformError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"network_id": estimate.NetworkID, "accounts": p.tokenAccountDTOs(item, estimate.Accounts),
		"returned": estimate.Returned, "fee": estimate.Fee, "fee_model": estimate.FeeModel,
		"compute_unit_limit":                estimate.ComputeUnitLimit,
		"compute_unit_price_micro_lamports": estimate.ComputeUnitPriceMicroLamports,
	})
}

// closeTokenAccounts closes the named accounts at the approved fee, under the
// same rules as a transfer: the spending step-up, an idempotency key, and the
// transaction id with its expiry recorded before anything is broadcast.
func (p *Platform) closeTokenAccounts(w http.ResponseWriter, r *http.Request) {
	if !p.requireSendConfirmation(w, r) {
		return
	}
	item, accountItem, address, closer, ok := p.solanaAccount(w, r)
	if !ok {
		return
	}
	var request closeAccountsDTO
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	operationKey, ok := p.beginChainOperation(w, r, item.ID, request)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	var lastValidBlockHeight uint64
	closeRequest := solanachain.CloseRequest{
		Owner: address, Accounts: request.Accounts,
		Approved: &chain.FeeApproval{
			FeeModel: request.FeeModel, Fee: request.Fee, ComputeUnitLimit: request.ComputeUnitLimit,
			ComputeUnitPriceMicroLamports: request.ComputeUnitPriceMicroLamports,
		},
		BeforeSigning: func(height uint64) error {
			lastValidBlockHeight = height
			return nil
		},
	}
	var transaction chain.Transaction
	err := p.spaces.WithMessageSigner(ctx, r.PathValue("space_id"), accountItem.ID, item.ID, account.FamilySolana,
		func(signer chain.MessageSigner) error {
			recording := p.recordingMessageSigner(r.PathValue("space_id"), operationKey, signer,
				func() uint64 { return lastValidBlockHeight })
			var closeErr error
			transaction, closeErr = closer.Close(ctx, item.ID, closeRequest, recording)
			return closeErr
		})
	p.finishTronOperation(w, r, item.ID, operationKey, transaction.Hash, err)
}
