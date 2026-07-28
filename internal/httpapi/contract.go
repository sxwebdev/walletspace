package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/tron"
	"github.com/sxwebdev/walletspace/internal/wallet"
)

// deployRequest is the body /deploy takes. The two limits are chain settings
// rather than money, so they travel as numbers; the fee limit is TRX and stays
// a string all the way to decimal.
type deployRequest struct {
	Name                       string `json:"name"`
	ABI                        string `json:"abi"`
	Bytecode                   string `json:"bytecode"`
	ConstructorParams          string `json:"constructor_params"`
	FeeLimit                   string `json:"fee_limit"`
	ConsumeUserResourcePercent int64  `json:"consume_user_resource_percent"`
	OriginEnergyLimit          int64  `json:"origin_energy_limit"`
}

// deployResponse reports where the contract landed and what the receipt said.
//
// The cost fields are omitted until the receipt arrives rather than sent as
// zeros, which would read as a deployment that cost nothing.
type deployResponse struct {
	TxID      string `json:"txid"`
	Address   string `json:"address"`
	Confirmed bool   `json:"confirmed"`
	// Failure is the VM's verdict when the deployment was mined and refused.
	// Empty means the contract is at Address.
	Failure    string `json:"failure,omitempty"`
	EnergyUsed int64  `json:"energy_used,omitempty"`
	Fee        string `json:"fee,omitempty"`
}

// deployCostResponse is what a deployment will cost. Every figure is a string:
// the resource counts are exact integers the browser would otherwise round, and
// the rest is money.
type deployCostResponse struct {
	Energy    string `json:"energy"`
	Bandwidth string `json:"bandwidth"`
	Fee       string `json:"fee"`
	// MinFeeLimit is the smallest fee limit that covers the energy, which is
	// not the same figure as Fee — see tron.DeployCost. "0" means the node did
	// not report an energy price, so it is unknown rather than unrestricted.
	MinFeeLimit string `json:"min_fee_limit"`
	Shortfall   string `json:"shortfall"`
}

// decodeDeployment parses the body both deployment endpoints take and resolves
// the wallet, writing the response itself when anything is wrong.
func (s *Server) decodeDeployment(w http.ResponseWriter, r *http.Request) (wallet.Wallet, tron.Deployment, bool) {
	from, ok := s.resolveWallet(w, r)
	if !ok {
		return wallet.Wallet{}, tron.Deployment{}, false
	}

	var req deployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return wallet.Wallet{}, tron.Deployment{}, false
	}

	feeLimit, err := decimal.NewFromString(req.FeeLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid fee limit: "+req.FeeLimit)
		return wallet.Wallet{}, tron.Deployment{}, false
	}

	return from, tron.Deployment{
		Name:                       req.Name,
		ABI:                        req.ABI,
		Bytecode:                   req.Bytecode,
		ConstructorParams:          req.ConstructorParams,
		FeeLimit:                   feeLimit,
		ConsumeUserResourcePercent: req.ConsumeUserResourcePercent,
		OriginEnergyLimit:          req.OriginEnergyLimit,
	}, true
}

// handleDeployEstimate prices a deployment. It signs nothing and never reaches
// the mnemonic — the whole point is to answer before anything is committed to.
func (s *Server) handleDeployEstimate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	from, deployment, ok := s.decodeDeployment(w, r)
	if !ok {
		return
	}

	cost, err := s.chain.EstimateDeploy(ctx, from.Address, deployment)
	if err != nil {
		// A constructor that reverts is the caller's contract, not the node's
		// fault, and the service classifies it as such.
		if errors.Is(err, tron.ErrInvalidRequest) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		s.log.Warn("deploy estimate failed", "from", from.Address, "error", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, deployCostResponse{
		Energy:      cost.Energy.String(),
		Bandwidth:   cost.Bandwidth.String(),
		Fee:         cost.Fee.String(),
		MinFeeLimit: cost.MinFeeLimit.String(),
		Shortfall:   cost.Shortfall.String(),
	})
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	// Its own budget: a deployment is built, broadcast, and then waited on for
	// the receipt, which no other operation here does.
	ctx, cancel := context.WithTimeout(r.Context(), deployTimeout)
	defer cancel()

	from, deployment, ok := s.decodeDeployment(w, r)
	if !ok {
		return
	}

	// Checked here rather than being left to the service: a deployment carries
	// half a megabyte of caller-supplied bytecode, and refusing it a step later
	// would mean deriving a key from the mnemonic for a request that was never
	// going anywhere.
	if err := deployment.Validate(from.Address); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	key, err := s.wallets.PrivateKey(from.Index)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	out, err := s.chain.Deploy(ctx, from.Address, deployment, key)
	if err != nil {
		if errors.Is(err, tron.ErrInvalidRequest) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		s.log.Error("deploy failed", "from", from.Address, "name", deployment.Name, "error", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	// A deployment the VM refused is still an answer, not an error: the
	// transaction is on chain, the fee is spent, and the caller needs the txid.
	// It is reported in the body so nothing about it is lost to a status code.
	res := deployResponse{
		TxID:      out.TxID,
		Address:   out.Address,
		Confirmed: out.Confirmed,
		Failure:   out.Failure,
	}
	if out.Confirmed {
		res.EnergyUsed = out.EnergyUsed
		res.Fee = out.Fee.String()
	}

	s.log.Info("deployed",
		"from", from.Address,
		"address", out.Address,
		"txid", out.TxID,
		"confirmed", out.Confirmed,
		"failure", out.Failure,
	)
	writeJSON(w, http.StatusOK, res)
}
