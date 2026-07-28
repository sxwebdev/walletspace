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
	"time"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/tronfaucet/internal/tron"
	"github.com/sxwebdev/tronfaucet/internal/wallet"
)

//go:embed ui
var uiFS embed.FS

// requestTimeout bounds every on-chain operation triggered from the UI.
const requestTimeout = 45 * time.Second

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
	Send(ctx context.Context, from, to string, asset tron.Asset, amount decimal.Decimal, key *ecdsa.PrivateKey) (string, error)
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
	mux.HandleFunc("POST /api/wallets/{index}/send", s.handleSend)
	mux.HandleFunc("PATCH /api/wallets/{index}", s.handleRename)

	return s.guard(mux), nil
}

// maxBodyBytes caps request bodies. Every payload here is a handful of short
// fields, so anything larger is a mistake or an attempt to exhaust memory.
const maxBodyBytes = 64 << 10

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
			if ct := r.Header.Get("Content-Type"); ct != "" {
				if media, _, err := mime.ParseMediaType(ct); err != nil || media != "application/json" {
					writeError(w, http.StatusUnsupportedMediaType, "expected Content-Type: application/json")
					return
				}
			}

			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
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

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	index, err := parseIndex(r.PathValue("index"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var req struct {
		Asset  string `json:"asset"`
		To     string `json:"to"`
		Amount string `json:"amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	amount, err := decimal.NewFromString(req.Amount)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid amount: "+req.Amount)
		return
	}

	var asset tron.Asset
	switch req.Asset {
	case string(tron.AssetTRX):
		asset = tron.AssetTRX
	case string(tron.AssetUSDT):
		asset = tron.AssetUSDT
	default:
		writeError(w, http.StatusBadRequest, "unknown asset: "+req.Asset)
		return
	}

	from, err := s.wallets.Get(index)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	key, err := s.wallets.PrivateKey(index)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}

	txid, err := s.chain.Send(ctx, from.Address, req.To, asset, amount, key)
	if err != nil {
		// Input the chain never saw is the caller's fault, not the node's.
		if errors.Is(err, tron.ErrInvalidRequest) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}

		s.log.Error("send failed",
			"from", from.Address,
			"to", req.To,
			"asset", asset,
			"amount", amount.String(),
			"error", err,
		)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	s.log.Info("sent", "from", from.Address, "to", req.To, "asset", asset, "amount", amount.String(), "txid", txid)
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
