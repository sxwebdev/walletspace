// Package httpapi serves the JSON API and the embedded single-page UI.
package httpapi

import (
	"context"
	"crypto/ecdsa"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/tron"
	"github.com/sxwebdev/walletspace/internal/wallet"
)

//go:embed ui
var uiFS embed.FS

const (
	// requestTimeout bounds every on-chain operation triggered from the UI.
	requestTimeout = 45 * time.Second
	// deployTimeout covers a deployment, which waits for its receipt on top of
	// building and broadcasting: a contract that ran out of energy is only
	// visible once the transaction is in a block.
	deployTimeout = 90 * time.Second
)

// Wallets is the wallet storage the API operates on.
type Wallets interface {
	List() []wallet.Wallet
	Get(index uint32) (wallet.Wallet, error)
	Create(label string) (wallet.Wallet, error)
	Rename(index uint32, label string) error
	PrivateKey(index uint32) (*ecdsa.PrivateKey, error)
}

// Chain is the subset of the Tron service the API needs.
type Chain interface {
	Token() tron.TokenInfo
	Balances(ctx context.Context, addresses []string, refresh bool) (map[string]tron.Balance, map[string]error)
	Estimate(ctx context.Context, from, to string, asset tron.Asset, amount decimal.Decimal) (tron.Estimate, error)
	Spendable(ctx context.Context, from, to string, asset tron.Asset) (decimal.Decimal, tron.Estimate, error)
	Shortfall(ctx context.Context, from string, asset tron.Asset, amount decimal.Decimal, est tron.Estimate) (decimal.Decimal, error)
	Send(ctx context.Context, from, to string, asset tron.Asset, amount decimal.Decimal, key *ecdsa.PrivateKey) (string, error)

	Resources(ctx context.Context, addr string) (tron.Resources, error)
	Stake(ctx context.Context, from string, resource tron.Resource, amount decimal.Decimal, key *ecdsa.PrivateKey) (string, error)
	Unstake(ctx context.Context, from string, resource tron.Resource, amount decimal.Decimal, key *ecdsa.PrivateKey) (string, error)
	Delegate(ctx context.Context, from, to string, resource tron.Resource, amount decimal.Decimal, key *ecdsa.PrivateKey) (string, error)
	Reclaim(ctx context.Context, from, to string, resource tron.Resource, amount decimal.Decimal, key *ecdsa.PrivateKey) (string, error)
	ReclaimAll(ctx context.Context, from, to string, resource tron.Resource, key *ecdsa.PrivateKey) (string, error)
	WithdrawUnstaked(ctx context.Context, from string, key *ecdsa.PrivateKey) (string, error)
	CancelUnstakes(ctx context.Context, from string, key *ecdsa.PrivateKey) (string, error)

	EstimateDeploy(ctx context.Context, from string, d tron.Deployment) (tron.DeployCost, error)
	Deploy(ctx context.Context, from string, d tron.Deployment, key *ecdsa.PrivateKey) (tron.Deployed, error)
}

// Server wires the storage and the chain service into an http.Handler.
type Server struct {
	wallets  Wallets
	chain    Chain
	log      *slog.Logger
	network  string
	explorer string
}

// New returns a handler serving both the UI and the API.
func New(wallets Wallets, chain Chain, network, explorer string, log *slog.Logger) (http.Handler, error) {
	s := &Server{
		wallets:  wallets,
		chain:    chain,
		log:      log,
		network:  network,
		explorer: explorer,
	}

	ui, err := fs.Sub(uiFS, "ui")
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(ui))
	mux.HandleFunc("GET /api/info", s.handleInfo)
	mux.HandleFunc("GET /api/wallets", s.handleList)
	mux.HandleFunc("POST /api/wallets", s.handleCreate)
	mux.HandleFunc("GET /api/balances", s.handleBalances)
	mux.HandleFunc("POST /api/wallets/{index}/estimate", s.handleEstimate)
	mux.HandleFunc("POST /api/wallets/{index}/send", s.handleSend)
	mux.HandleFunc("PATCH /api/wallets/{index}", s.handleRename)
	mux.HandleFunc("GET /api/wallets/{index}/resources", s.handleResources)
	mux.HandleFunc("POST /api/wallets/{index}/stake", s.handleStake)
	mux.HandleFunc("POST /api/wallets/{index}/unstake", s.handleUnstake)
	mux.HandleFunc("POST /api/wallets/{index}/delegate", s.handleDelegate)
	mux.HandleFunc("POST /api/wallets/{index}/reclaim", s.handleReclaim)
	mux.HandleFunc("POST /api/wallets/{index}/withdraw", s.handleWithdrawUnstaked)
	mux.HandleFunc("POST /api/wallets/{index}/cancel-unstakes", s.handleCancelUnstakes)
	mux.HandleFunc("POST "+deployPath, s.handleDeploy)
	mux.HandleFunc("POST "+deployEstimatePath, s.handleDeployEstimate)

	return s.guard(mux), nil
}

const (
	// maxBodyBytes caps request bodies. Every payload here is a handful of short
	// fields, so anything larger is a mistake or an attempt to exhaust memory.
	maxBodyBytes = 64 << 10
	// maxDeployBodyBytes caps the one payload that is not: a contract travels as
	// hex, so it is twice its own size, and the ABI rides along with it. The
	// chain's own limit on deployed code is a few tens of kilobytes.
	maxDeployBodyBytes = 512 << 10

	// The two deployment routes. Both carry the contract itself, so both need
	// the larger body limit; they share the prefix that bodyLimit matches on.
	deployPath         = "/api/wallets/{index}/deploy"
	deployEstimatePath = deployPath + "-estimate"
)

// bodyLimit is how much of a request body will be read. Only a deployment needs
// more than the default, and giving every endpoint its allowance would mean an
// unauthenticated POST could park half a megabyte of nonsense in memory before
// the address on it is even looked at.
func bodyLimit(path string) int64 {
	// Whole final segments, not a substring: matching "/deploy" anywhere would
	// hand the raised limit to "…/deploy-anything" and to any later route that
	// merely embeds the word. These two are the routes registered above.
	if strings.HasSuffix(path, "/deploy") || strings.HasSuffix(path, "/deploy-estimate") {
		return maxDeployBodyBytes
	}

	return maxBodyBytes
}

// guard protects the unauthenticated local API from requests a browser makes on
// behalf of another site.
//
// The service holds spendable keys and has no login, so a page the user happens
// to visit must not be able to drive it. Two checks do the work:
//
//   - Sec-Fetch-Site, sent by current browsers, must say the request came from
//     this origin or from no page at all.
//   - Any Origin header must match the Host we were reached on.
//
// Requiring Content-Type: application/json on writes is deliberate too: it
// makes a cross-site fetch non-simple, so the browser must preflight it, and
// the preflight fails because no CORS headers are returned.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
		default:
			writeError(w, http.StatusForbidden, "cross-site requests are not allowed")
			return
		}

		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host {
				writeError(w, http.StatusForbidden, "cross-origin requests are not allowed")
				return
			}
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// Required even from a request with no body at all. A cross-site
			// POST that sets no Content-Type is a CORS simple request, which a
			// browser sends without asking permission first; demanding JSON is
			// what forces the preflight that then fails. Accepting a missing
			// header would leave the two bodyless endpoints — withdraw and
			// cancel-unstakes, both of which move staked TRX — guarded by the
			// Sec-Fetch-Site and Origin checks alone.
			media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil || media != "application/json" {
				writeError(w, http.StatusUnsupportedMediaType, "expected Content-Type: application/json")
				return
			}

			r.Body = http.MaxBytesReader(w, r.Body, bodyLimit(r.URL.Path))
		}

		next.ServeHTTP(w, r)
	})
}

type infoResponse struct {
	Network       string `json:"network"`
	Explorer      string `json:"explorer"`
	TokenSymbol   string `json:"token_symbol"`
	TokenContract string `json:"token_contract"`
	TokenDecimals int32  `json:"token_decimals"`
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	token := s.chain.Token()

	writeJSON(w, http.StatusOK, infoResponse{
		Network:       s.network,
		Explorer:      s.explorer,
		TokenSymbol:   token.Symbol,
		TokenContract: token.Contract,
		TokenDecimals: token.Decimals,
	})
}

type walletResponse struct {
	Index     uint32    `json:"index"`
	Address   string    `json:"address"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
}

func toWalletResponse(w wallet.Wallet) walletResponse {
	return walletResponse{Index: w.Index, Address: w.Address, Label: w.Label, CreatedAt: w.CreatedAt}
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	list := s.wallets.List()

	out := make([]walletResponse, 0, len(list))
	for _, item := range list {
		out = append(out, toWalletResponse(item))
	}

	writeJSON(w, http.StatusOK, map[string]any{"wallets": out})
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Label string `json:"label"`
	}

	// An empty body is fine: it just means "no label".
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	}

	created, err := s.wallets.Create(req.Label)
	if err != nil {
		s.log.Error("create wallet", "error", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.log.Info("wallet created", "index", created.Index, "address", created.Address)
	writeJSON(w, http.StatusCreated, toWalletResponse(created))
}

func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	index, err := parseIndex(r.PathValue("index"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var req struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if err := s.wallets.Rename(index, req.Label); err != nil {
		s.writeStoreError(w, err)
		return
	}

	updated, err := s.wallets.Get(index)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, toWalletResponse(updated))
}

type balanceResponse struct {
	Address   string `json:"address"`
	TRX       string `json:"trx,omitempty"`
	USDT      string `json:"usdt,omitempty"`
	Activated bool   `json:"activated"`
	Error     string `json:"error,omitempty"`
}

func (s *Server) handleBalances(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	list := s.wallets.List()
	addresses := make([]string, 0, len(list))
	for _, item := range list {
		addresses = append(addresses, item.Address)
	}

	refresh := r.URL.Query().Get("refresh") == "1"
	balances, errs := s.chain.Balances(ctx, addresses, refresh)

	out := make([]balanceResponse, 0, len(addresses))
	for _, addr := range addresses {
		if err, ok := errs[addr]; ok {
			s.log.Warn("balance fetch failed", "address", addr, "error", err)
			out = append(out, balanceResponse{Address: addr, Error: err.Error()})
			continue
		}

		b := balances[addr]
		out = append(out, balanceResponse{
			Address:   addr,
			TRX:       b.TRX.String(),
			USDT:      b.USDT.String(),
			Activated: b.Activated,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"balances": out})
}

// amountMax asks the server to work out the largest sendable amount itself.
// The arithmetic stays in decimal on this side rather than going through a
// browser float, and the fee is priced on a probe the node will actually build.
const amountMax = "max"

// transferRequest is the body both /estimate and /send take.
type transferRequest struct {
	Asset  string `json:"asset"`
	To     string `json:"to"`
	Amount string `json:"amount"`
}

// transfer is a decoded transfer request: which wallet sends what to whom.
type transfer struct {
	index  uint32
	from   wallet.Wallet
	to     string
	asset  tron.Asset
	amount decimal.Decimal
	// max is set when the caller asked for the largest sendable amount instead
	// of naming one; amount is then filled in by the server.
	max bool
}

// decodeTransfer parses the shared body and resolves the sending wallet,
// writing the response itself when anything is wrong.
func (s *Server) decodeTransfer(w http.ResponseWriter, r *http.Request) (transfer, bool) {
	from, ok := s.resolveWallet(w, r)
	if !ok {
		return transfer{}, false
	}

	var req transferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return transfer{}, false
	}

	t := transfer{index: from.Index, from: from, to: req.To, max: req.Amount == amountMax}

	if !t.max {
		amount, err := decimal.NewFromString(req.Amount)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid amount: "+req.Amount)
			return transfer{}, false
		}
		t.amount = amount
	}

	switch req.Asset {
	case string(tron.AssetTRX):
		t.asset = tron.AssetTRX
	case string(tron.AssetUSDT):
		t.asset = tron.AssetUSDT
	default:
		writeError(w, http.StatusBadRequest, "unknown asset: "+req.Asset)
		return transfer{}, false
	}

	return t, true
}

// resolveAmount fills in the amount when the caller asked for "max".
func (s *Server) resolveAmount(ctx context.Context, t *transfer) (tron.Estimate, error) {
	if !t.max {
		est, err := s.chain.Estimate(ctx, t.from.Address, t.to, t.asset, t.amount)
		return est, err
	}

	spendable, est, err := s.chain.Spendable(ctx, t.from.Address, t.to, t.asset)
	if err != nil {
		return tron.Estimate{}, err
	}
	t.amount = spendable

	return est, nil
}

type estimateResponse struct {
	// Amount echoes what was priced, which is the point of the "max" mode.
	Amount     string `json:"amount"`
	Fee        string `json:"fee"`
	Activation string `json:"activation"`
	// Shortfall is the TRX the sender is missing, "0" when the balance covers
	// the cost. It is advisory: the send is still attempted if asked for.
	Shortfall string `json:"shortfall"`
}

func (s *Server) handleEstimate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	t, ok := s.decodeTransfer(w, r)
	if !ok {
		return
	}

	est, err := s.resolveAmount(ctx, &t)
	if err != nil {
		if errors.Is(err, tron.ErrInvalidRequest) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		s.log.Warn("estimate failed",
			"from", t.from.Address, "to", t.to, "asset", t.asset, "amount", t.amount.String(), "error", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	// A balance the service could not read is not worth failing a priced
	// estimate over — the figure only adds a warning to it.
	shortfall, err := s.chain.Shortfall(ctx, t.from.Address, t.asset, t.amount, est)
	if err != nil {
		s.log.Warn("shortfall check failed", "from", t.from.Address, "error", err)
		shortfall = decimal.Zero
	}

	writeJSON(w, http.StatusOK, estimateResponse{
		Amount:     t.amount.String(),
		Fee:        est.Fee.String(),
		Activation: est.Activation.String(),
		Shortfall:  shortfall.String(),
	})
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	t, ok := s.decodeTransfer(w, r)
	if !ok {
		return
	}

	if t.max {
		if _, err := s.resolveAmount(ctx, &t); err != nil {
			if errors.Is(err, tron.ErrInvalidRequest) {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}

			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
	}

	from, asset, amount, to := t.from, t.asset, t.amount, t.to

	key, err := s.wallets.PrivateKey(t.index)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	txid, err := s.chain.Send(ctx, from.Address, to, asset, amount, key)
	if err != nil {
		// Input the chain never saw is the caller's fault, not the node's.
		if errors.Is(err, tron.ErrInvalidRequest) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		s.log.Error("send failed",
			"from", from.Address,
			"to", to,
			"asset", asset,
			"amount", amount.String(),
			"error", err,
		)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.log.Info("sent", "from", from.Address, "to", to, "asset", asset, "amount", amount.String(), "txid", txid)
	writeJSON(w, http.StatusOK, map[string]string{"txid": txid})
}

func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, wallet.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	s.log.Error("wallet store", "error", err)
	writeError(w, http.StatusInternalServerError, err.Error())
}

func parseIndex(raw string) (uint32, error) {
	index, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, errors.New("invalid wallet index: " + raw)
	}

	return uint32(index), nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line is already out; nothing left but to note it.
		slog.Default().Debug("write response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
