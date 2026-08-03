package httpapi_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/httpapi"
	"github.com/sxwebdev/walletspace/internal/tron"
	"github.com/sxwebdev/walletspace/internal/wallet"
)

// Real base58 addresses, not placeholders. The handlers validate what they are
// handed before deriving a key, so a fixture that is not a valid address would
// be testing a path production never reaches — and would start failing the
// moment any other handler grew the same check.
const (
	walletAddr0   = "TEeKaYdpN6ujnpVZ1SkohE6Ru6gd9vGC2A"
	walletAddr1   = "TUwbUgKvC1RsT3qShxmZcfMpvMdbE6JPST"
	newWalletAddr = "TLutkfK9N2BaBEzUngAuaNKTC9SZu3ER1K"
	recipientAddr = "TZ4UXDV5ZhNW7fb2AMSbgfAEZ7hWsnYS2g"
	receiverAddr  = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
)

func TestListWallets(t *testing.T) {
	t.Parallel()

	srv := newServer(t, newWalletsFake(), newChainFake())

	var got struct {
		Wallets []struct {
			Index   uint32 `json:"index"`
			Address string `json:"address"`
			Label   string `json:"label"`
		} `json:"wallets"`
	}
	do(t, srv, http.MethodGet, "/api/wallets", "", http.StatusOK, &got)

	if len(got.Wallets) != 2 {
		t.Fatalf("returned %d wallets, want 2", len(got.Wallets))
	}

	if got.Wallets[0].Address != walletAddr0 || got.Wallets[1].Index != 1 {
		t.Errorf("unexpected wallets: %+v", got.Wallets)
	}
}

func TestCreateWallet(t *testing.T) {
	t.Parallel()

	wallets := newWalletsFake()
	srv := newServer(t, wallets, newChainFake())

	var created struct {
		Index   uint32 `json:"index"`
		Address string `json:"address"`
		Label   string `json:"label"`
	}
	do(t, srv, http.MethodPost, "/api/wallets", `{"label":"third"}`, http.StatusCreated, &created)

	if created.Index != 2 {
		t.Errorf("created index = %d, want 2", created.Index)
	}

	if created.Label != "third" {
		t.Errorf("created label = %q, want %q", created.Label, "third")
	}

	if n := len(wallets.List()); n != 3 {
		t.Errorf("store holds %d wallets after create, want 3", n)
	}
}

func TestCreateWalletWithEmptyBody(t *testing.T) {
	t.Parallel()

	srv := newServer(t, newWalletsFake(), newChainFake())

	// The UI may post without a body; that just means "no label".
	do(t, srv, http.MethodPost, "/api/wallets", "", http.StatusCreated, nil)
}

func TestBalancesReportPerAddressErrors(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.balances[walletAddr0] = tron.Balance{
		TRX:       decimal.RequireFromString("12.5"),
		USDT:      decimal.RequireFromString("3.25"),
		Activated: true,
	}
	chain.errs[walletAddr1] = errors.New("node unreachable")

	srv := newServer(t, newWalletsFake(), chain)

	var got struct {
		Balances []struct {
			Address string `json:"address"`
			TRX     string `json:"trx"`
			USDT    string `json:"usdt"`
			Error   string `json:"error"`
		} `json:"balances"`
	}
	do(t, srv, http.MethodGet, "/api/balances", "", http.StatusOK, &got)

	if len(got.Balances) != 2 {
		t.Fatalf("returned %d balances, want one per wallet", len(got.Balances))
	}

	// A failing address must not drop the healthy ones from the response.
	if got.Balances[0].TRX != "12.5" || got.Balances[0].USDT != "3.25" {
		t.Errorf("wallet 0 balances = %+v, want 12.5 TRX / 3.25 USDT", got.Balances[0])
	}

	if got.Balances[1].Error != "node unreachable" {
		t.Errorf("wallet 1 error = %q, want the fetch error", got.Balances[1].Error)
	}
}

func TestBalancesRefreshFlag(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	srv := newServer(t, newWalletsFake(), chain)

	do(t, srv, http.MethodGet, "/api/balances", "", http.StatusOK, nil)
	if chain.lastRefresh {
		t.Error("refresh = true without the query parameter")
	}

	do(t, srv, http.MethodGet, "/api/balances?refresh=1", "", http.StatusOK, nil)
	if !chain.lastRefresh {
		t.Error("refresh = false with ?refresh=1")
	}
}

func TestBalancesCanSelectWalletIndexes(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.balances[walletAddr1] = tron.Balance{
		TRX:       decimal.RequireFromString("7.5"),
		Activated: true,
	}
	srv := newServer(t, newWalletsFake(), chain)

	var got struct {
		Balances []struct {
			Address string `json:"address"`
			TRX     string `json:"trx"`
		} `json:"balances"`
	}
	do(t, srv, http.MethodGet, "/api/balances?index=1", "", http.StatusOK, &got)

	if len(got.Balances) != 1 || got.Balances[0].Address != walletAddr1 {
		t.Fatalf("selected balances = %+v, want only wallet 1", got.Balances)
	}
	if len(chain.balanceAddresses) != 1 || chain.balanceAddresses[0] != walletAddr1 {
		t.Errorf("chain addresses = %v, want only %s", chain.balanceAddresses, walletAddr1)
	}
}

func TestBalanceStreamFlushesEachCompletedWallet(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	releaseSecond := make(chan struct{})
	chain.balanceStream = func(ctx context.Context, addresses []string, refresh bool) <-chan tron.BalanceResult {
		out := make(chan tron.BalanceResult)
		go func() {
			defer close(out)
			select {
			case out <- tron.BalanceResult{
				Address: addresses[0],
				Balance: tron.Balance{TRX: decimal.NewFromInt(1), Activated: true},
			}:
			case <-ctx.Done():
				return
			}

			select {
			case <-releaseSecond:
			case <-ctx.Done():
				return
			}

			select {
			case out <- tron.BalanceResult{
				Address: addresses[1],
				Balance: tron.Balance{TRX: decimal.NewFromInt(2), Activated: true},
			}:
			case <-ctx.Done():
			}
		}()
		return out
	}

	srv := newServer(t, newWalletsFake(), chain)
	res, err := srv.Client().Get(srv.URL + "/api/balances/stream")
	if err != nil {
		t.Fatalf("GET balance stream: %v", err)
	}
	defer res.Body.Close()

	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/x-ndjson") {
		t.Fatalf("Content-Type = %q, want NDJSON", got)
	}

	dec := json.NewDecoder(res.Body)
	var first struct {
		Address string `json:"address"`
		TRX     string `json:"trx"`
	}
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("decode first streamed balance: %v", err)
	}
	if first.Address != walletAddr0 || first.TRX != "1" {
		t.Fatalf("first streamed balance = %+v", first)
	}

	close(releaseSecond)

	var second struct {
		Address string `json:"address"`
		TRX     string `json:"trx"`
	}
	if err := dec.Decode(&second); err != nil {
		t.Fatalf("decode second streamed balance: %v", err)
	}
	if second.Address != walletAddr1 || second.TRX != "2" {
		t.Fatalf("second streamed balance = %+v", second)
	}
}

func TestTransactionStatus(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.confirmed = true
	srv := newServer(t, newWalletsFake(), chain)
	txid := strings.Repeat("ab", 32)

	var got struct {
		Confirmed bool `json:"confirmed"`
	}
	do(t, srv, http.MethodGet, "/api/transactions/"+txid, "", http.StatusOK, &got)

	if !got.Confirmed {
		t.Error("confirmed = false, want true")
	}
	if chain.confirmedID != txid {
		t.Errorf("checked txid = %q, want %q", chain.confirmedID, txid)
	}
}

func TestSend(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	srv := newServer(t, newWalletsFake(), chain)

	var got struct {
		Txid string `json:"txid"`
	}
	do(t, srv, http.MethodPost, "/api/wallets/1/send",
		`{"asset":"usdt","to":"`+recipientAddr+`","amount":"1.5"}`, http.StatusOK, &got)

	if got.Txid != "deadbeef" {
		t.Errorf("txid = %q, want %q", got.Txid, "deadbeef")
	}

	if chain.sentFrom != walletAddr1 {
		t.Errorf("sent from %q, want the address of wallet 1", chain.sentFrom)
	}

	if chain.sentAsset != tron.AssetUSDT {
		t.Errorf("sent asset = %q, want usdt", chain.sentAsset)
	}

	if !chain.sentAmount.Equal(decimal.RequireFromString("1.5")) {
		t.Errorf("sent amount = %s, want 1.5", chain.sentAmount)
	}

	if chain.sentKey == nil {
		t.Error("send was called without a signing key")
	}
}

func TestSendRejectsBadInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		body       string
		wantStatus int
	}{
		{
			name:       "unknown asset",
			path:       "/api/wallets/0/send",
			body:       `{"asset":"btc","to":"` + recipientAddr + `","amount":"1"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "unparsable amount",
			path:       "/api/wallets/0/send",
			body:       `{"asset":"trx","to":"` + recipientAddr + `","amount":"one"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "broken json",
			path:       "/api/wallets/0/send",
			body:       `{`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "non-numeric index",
			path:       "/api/wallets/abc/send",
			body:       `{"asset":"trx","to":"` + recipientAddr + `","amount":"1"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "unknown wallet",
			path:       "/api/wallets/99/send",
			body:       `{"asset":"trx","to":"` + recipientAddr + `","amount":"1"}`,
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			chain := newChainFake()
			srv := newServer(t, newWalletsFake(), chain)

			do(t, srv, http.MethodPost, tt.path, tt.body, tt.wantStatus, nil)

			if chain.sentFrom != "" {
				t.Errorf("a transfer was attempted for invalid input: from %q", chain.sentFrom)
			}
		})
	}
}

func TestEstimate(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.estimate = tron.Estimate{
		Fee:        decimal.RequireFromString("1.268"),
		Activation: decimal.RequireFromString("1"),
	}

	srv := newServer(t, newWalletsFake(), chain)

	var got estimateBody
	do(t, srv, http.MethodPost, "/api/wallets/0/estimate",
		`{"asset":"trx","to":"`+recipientAddr+`","amount":"1"}`, http.StatusOK, &got)

	// The UI subtracts the fee from the balance for "Max", so it has to arrive
	// exactly, not rounded through a float.
	if got.Fee != "1.268" {
		t.Errorf("fee = %q, want 1.268", got.Fee)
	}

	if got.Activation != "1" {
		t.Errorf("activation = %q, want 1 — the recipient needs creating", got.Activation)
	}
}

func TestEstimatePassesTheRequestThrough(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	srv := newServer(t, newWalletsFake(), chain)

	// Wallet 1, not 0: a handler that resolves the wrong wallet, or swaps
	// sender and recipient, would otherwise go unnoticed.
	do(t, srv, http.MethodPost, "/api/wallets/1/estimate",
		`{"asset":"usdt","to":"`+recipientAddr+`","amount":"2.5"}`, http.StatusOK, nil)

	if chain.estFrom != walletAddr1 {
		t.Errorf("estimated from %q, want the address of wallet 1", chain.estFrom)
	}

	if chain.estTo != recipientAddr {
		t.Errorf("estimated to %q, want the recipient from the body", chain.estTo)
	}

	if chain.estAsset != tron.AssetUSDT {
		t.Errorf("estimated asset = %q, want usdt", chain.estAsset)
	}

	if !chain.estAmount.Equal(decimal.RequireFromString("2.5")) {
		t.Errorf("estimated amount = %s, want 2.5", chain.estAmount)
	}
}

func TestEstimateMaxAsksTheServerForTheAmount(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.spendable = decimal.RequireFromString("1979.463")
	chain.estimate = tron.Estimate{Fee: decimal.RequireFromString("0.269")}

	srv := newServer(t, newWalletsFake(), chain)

	var got estimateBody
	do(t, srv, http.MethodPost, "/api/wallets/0/estimate",
		`{"asset":"trx","to":"`+recipientAddr+`","amount":"max"}`, http.StatusOK, &got)

	if chain.spendableCalls != 1 {
		t.Errorf("Spendable called %d times, want exactly 1", chain.spendableCalls)
	}

	// The browser must not compute this itself: it would do it in a float.
	if got.Amount != "1979.463" {
		t.Errorf("amount = %q, want the server-computed 1979.463", got.Amount)
	}
}

func TestEstimateRejectsBadInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		body       string
		wantStatus int
	}{
		{
			name:       "unknown asset",
			path:       "/api/wallets/0/estimate",
			body:       `{"asset":"btc","to":"` + recipientAddr + `","amount":"1"}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "unknown wallet",
			path:       "/api/wallets/99/estimate",
			body:       `{"asset":"trx","to":"` + recipientAddr + `","amount":"1"}`,
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "unparsable amount",
			path:       "/api/wallets/0/estimate",
			body:       `{"asset":"trx","to":"` + recipientAddr + `","amount":"plenty"}`,
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			do(t, srvFor(t), http.MethodPost, tt.path, tt.body, tt.wantStatus, nil)
		})
	}
}

func srvFor(t *testing.T) *httptest.Server {
	t.Helper()

	return newServer(t, newWalletsFake(), newChainFake())
}

func TestSendMaxResolvesAmountBeforeSigning(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.spendable = decimal.RequireFromString("42.5")

	srv := newServer(t, newWalletsFake(), chain)

	do(t, srv, http.MethodPost, "/api/wallets/0/send",
		`{"asset":"trx","to":"`+recipientAddr+`","amount":"max"}`, http.StatusOK, nil)

	if chain.spendableCalls != 1 {
		t.Errorf("Spendable called %d times, want 1", chain.spendableCalls)
	}

	// Sending "max" must transfer what the server worked out, not zero.
	if !chain.sentAmount.Equal(decimal.RequireFromString("42.5")) {
		t.Errorf("sent %s, want the spendable 42.5", chain.sentAmount)
	}
}

func TestSendMaxSurfacesSpendableFailure(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.spendableErr = fmt.Errorf("%w: holds 0.1 TRX, which does not cover the fee", tron.ErrInvalidRequest)

	srv := newServer(t, newWalletsFake(), chain)

	do(t, srv, http.MethodPost, "/api/wallets/0/send",
		`{"asset":"trx","to":"`+recipientAddr+`","amount":"max"}`, http.StatusBadRequest, nil)

	if chain.sentFrom != "" {
		t.Error("a transfer was attempted although the amount could not be resolved")
	}
}

func TestEstimateMapsValidationFailuresTo400(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.estimateErr = fmt.Errorf("%w: invalid recipient address: checksum mismatch", tron.ErrInvalidRequest)

	srv := newServer(t, newWalletsFake(), chain)

	do(t, srv, http.MethodPost, "/api/wallets/0/estimate",
		`{"asset":"trx","to":"T","amount":"1"}`, http.StatusBadRequest, nil)
}

type estimateBody struct {
	Amount     string `json:"amount"`
	Fee        string `json:"fee"`
	Activation string `json:"activation"`
	Shortfall  string `json:"shortfall"`
}

func TestSendMapsValidationFailuresTo400(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	// tron.Send raises these before it ever contacts a node, so blaming the
	// upstream with a 502 would mislead both clients and log readers.
	chain.sendErr = fmt.Errorf("%w: invalid recipient address: checksum mismatch", tron.ErrInvalidRequest)

	srv := newServer(t, newWalletsFake(), chain)

	var got struct {
		Error string `json:"error"`
	}
	do(t, srv, http.MethodPost, "/api/wallets/0/send",
		`{"asset":"trx","to":"T","amount":"1"}`, http.StatusBadRequest, &got)

	if !strings.Contains(got.Error, "invalid recipient address") {
		t.Errorf("error = %q, want the validation message", got.Error)
	}
}

func TestSendMapsAmountRejectionTo400(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	// tron.Send reports a rejected amount as ErrInvalidRequest, since the
	// constructors refuse it before any RPC happens.
	chain.sendErr = fmt.Errorf("%w: invalid amount: 0.0000001 is finer than the token's 6 decimals", tron.ErrInvalidRequest)

	srv := newServer(t, newWalletsFake(), chain)

	var got struct {
		Error string `json:"error"`
	}
	do(t, srv, http.MethodPost, "/api/wallets/0/send",
		`{"asset":"usdt","to":"`+recipientAddr+`","amount":"0.0000001"}`, http.StatusBadRequest, &got)

	if !strings.Contains(got.Error, "finer than the token") {
		t.Errorf("error = %q, want the amount rejection reason", got.Error)
	}
}

func TestSendPropagatesChainFailure(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	chain.sendErr = errors.New("broadcast transaction: bandwidth is not enough")

	srv := newServer(t, newWalletsFake(), chain)

	var got struct {
		Error string `json:"error"`
	}
	do(t, srv, http.MethodPost, "/api/wallets/0/send",
		`{"asset":"trx","to":"`+recipientAddr+`","amount":"1"}`, http.StatusBadGateway, &got)

	if !strings.Contains(got.Error, "bandwidth is not enough") {
		t.Errorf("error = %q, want it to carry the chain failure", got.Error)
	}
}

func TestRename(t *testing.T) {
	t.Parallel()

	wallets := newWalletsFake()
	srv := newServer(t, wallets, newChainFake())

	var got struct {
		Index uint32 `json:"index"`
		Label string `json:"label"`
	}
	do(t, srv, http.MethodPatch, "/api/wallets/0", `{"label":"main"}`, http.StatusOK, &got)

	if got.Label != "main" {
		t.Errorf("label = %q, want %q", got.Label, "main")
	}

	stored, err := wallets.Get(0)
	if err != nil {
		t.Fatalf("Get(0) error = %v", err)
	}

	if stored.Label != "main" {
		t.Errorf("stored label = %q, want %q", stored.Label, "main")
	}

	do(t, srv, http.MethodPatch, "/api/wallets/99", `{"label":"x"}`, http.StatusNotFound, nil)
}

func TestExportPrivateKey(t *testing.T) {
	t.Parallel()

	wallets := newWalletsFake()
	srv := newServer(t, wallets, newChainFake())

	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		srv.URL+"/api/wallets/1/private-key",
		nil,
	)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST private-key: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 200 (body: %s)", res.StatusCode, raw)
	}
	if got := res.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := res.Header.Get("Pragma"); got != "no-cache" {
		t.Errorf("Pragma = %q, want no-cache", got)
	}

	var got struct {
		Address    string `json:"address"`
		PrivateKey string `json:"private_key"`
	}
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode private key response: %v", err)
	}

	wantKey := fmt.Sprintf("%064x", wallets.key.D)
	if got.Address != walletAddr1 || got.PrivateKey != wantKey {
		t.Errorf("export = %+v, want wallet 1 and its 32-byte key", got)
	}
	if len(got.PrivateKey) != 64 {
		t.Errorf("private key length = %d, want 64 hex characters", len(got.PrivateKey))
	}
	if wallets.keyCalls != 1 {
		t.Errorf("private key derivations = %d, want 1", wallets.keyCalls)
	}
}

func TestPrivateKeyCannotBeExportedWithGET(t *testing.T) {
	t.Parallel()

	srv := newServer(t, newWalletsFake(), newChainFake())
	do(t, srv, http.MethodGet, "/api/wallets/0/private-key", "", http.StatusNotFound, nil)
}

func TestInfo(t *testing.T) {
	t.Parallel()

	srv := newServer(t, newWalletsFake(), newChainFake())

	var got struct {
		Network       string `json:"network"`
		Explorer      string `json:"explorer"`
		TokenSymbol   string `json:"token_symbol"`
		TokenContract string `json:"token_contract"`
		TokenDecimals int32  `json:"token_decimals"`
	}
	do(t, srv, http.MethodGet, "/api/info", "", http.StatusOK, &got)

	if got.Network != "nile" || got.Explorer != "https://nile.tronscan.org" {
		t.Errorf("info = %+v, want the configured network and explorer", got)
	}

	if got.TokenSymbol != "USDT" || got.TokenDecimals != 6 {
		t.Errorf("token info = %+v, want USDT with 6 decimals", got)
	}
}

func TestServesUI(t *testing.T) {
	t.Parallel()

	srv := newServer(t, newWalletsFake(), newChainFake())

	res, err := srv.Client().Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET / error = %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", res.StatusCode)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body error = %v", err)
	}

	page := string(body)
	if !strings.Contains(page, "<title>Walletspace</title>") ||
		!strings.Contains(page, `<script type="module" src="/app.js"></script>`) {
		t.Error("GET / did not return the Walletspace application shell")
	}
}

// --- helpers ---

func newServer(t *testing.T, wallets httpapi.Wallets, chain httpapi.Chain) *httptest.Server {
	t.Helper()

	handler, err := httpapi.New(wallets, chain, "nile", "https://nile.tronscan.org",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("httpapi.New() error = %v", err)
	}

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return srv
}

func do(t *testing.T, srv *httptest.Server, method, path, body string, wantStatus int, out any) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("NewRequest(%s %s) error = %v", method, path, err)
	}

	// The guard requires it on every write, body or not.
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", method, path, err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body of %s %s error = %v", method, path, err)
	}

	if res.StatusCode != wantStatus {
		t.Fatalf("%s %s status = %d, want %d (body: %s)", method, path, res.StatusCode, wantStatus, raw)
	}

	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decode response of %s %s error = %v (body: %s)", method, path, err, raw)
		}
	}
}

type walletsFake struct {
	wallets []wallet.Wallet
	key     *ecdsa.PrivateKey
	// keyCalls counts derivations, so a handler that reaches the mnemonic when
	// it has nothing to sign can be caught.
	keyCalls int
}

func newWalletsFake() *walletsFake {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}

	return &walletsFake{
		wallets: []wallet.Wallet{
			{Index: 0, Address: walletAddr0, Label: "first", CreatedAt: time.Unix(0, 0).UTC()},
			{Index: 1, Address: walletAddr1, CreatedAt: time.Unix(0, 0).UTC()},
		},
		key: key,
	}
}

func (f *walletsFake) List() []wallet.Wallet { return f.wallets }

func (f *walletsFake) Get(index uint32) (wallet.Wallet, error) {
	for _, w := range f.wallets {
		if w.Index == index {
			return w, nil
		}
	}

	return wallet.Wallet{}, wallet.ErrNotFound
}

func (f *walletsFake) Create(label string) (wallet.Wallet, error) {
	w := wallet.Wallet{
		Index:     uint32(len(f.wallets)),
		Address:   newWalletAddr,
		Label:     label,
		CreatedAt: time.Unix(0, 0).UTC(),
	}
	f.wallets = append(f.wallets, w)

	return w, nil
}

func (f *walletsFake) Rename(index uint32, label string) error {
	for i := range f.wallets {
		if f.wallets[i].Index == index {
			f.wallets[i].Label = label
			return nil
		}
	}

	return wallet.ErrNotFound
}

func (f *walletsFake) PrivateKey(index uint32) (*ecdsa.PrivateKey, error) {
	f.keyCalls++

	if _, err := f.Get(index); err != nil {
		return nil, err
	}

	return f.key, nil
}

type chainFake struct {
	balances map[string]tron.Balance
	errs     map[string]error

	lastRefresh      bool
	balanceAddresses []string
	balanceStream    func(context.Context, []string, bool) <-chan tron.BalanceResult
	confirmed        bool
	confirmErr       error
	confirmedID      string

	estimate    tron.Estimate
	estimateErr error

	// Recorded so a test can catch a handler that passes the wrong wallet,
	// swaps sender and recipient, or drops the amount.
	estFrom   string
	estTo     string
	estAsset  tron.Asset
	estAmount decimal.Decimal

	spendable      decimal.Decimal
	spendableErr   error
	spendableCalls int

	shortfall    decimal.Decimal
	shortfallErr error

	sfFrom     string
	sfAsset    tron.Asset
	sfAmount   decimal.Decimal
	sfEstimate tron.Estimate

	sendErr    error
	sentFrom   string
	sentTo     string
	sentAsset  tron.Asset
	sentAmount decimal.Decimal
	sentKey    *ecdsa.PrivateKey

	resources     tron.Resources
	resourcesErr  error
	resourcesAddr string

	// One recording for every staking operation: which one ran, and with what.
	// A handler that resolves the wrong wallet, swaps the two addresses or
	// drops the resource would otherwise still return a txid.
	stakeErr   error
	opName     string
	opFrom     string
	opTo       string
	opResource tron.Resource
	opAmount   decimal.Decimal
	opKey      *ecdsa.PrivateKey

	deployed    tron.Deployed
	deployErr   error
	deployFrom  string
	deployment  tron.Deployment
	deployCalls int
	deployKey   *ecdsa.PrivateKey

	cost    tron.DeployCost
	costErr error
	costOf  tron.Deployment
}

func newChainFake() *chainFake {
	return &chainFake{
		balances: make(map[string]tron.Balance),
		errs:     make(map[string]error),
	}
}

func (f *chainFake) Token() tron.TokenInfo {
	return tron.TokenInfo{Contract: "TTokenContract", Symbol: "USDT", Decimals: 6}
}

func (f *chainFake) Balances(_ context.Context, addresses []string, refresh bool) (map[string]tron.Balance, map[string]error) {
	f.lastRefresh = refresh
	f.balanceAddresses = append([]string(nil), addresses...)

	out := make(map[string]tron.Balance, len(addresses))
	errs := make(map[string]error)

	for _, addr := range addresses {
		if err, ok := f.errs[addr]; ok {
			errs[addr] = err
			continue
		}

		if b, ok := f.balances[addr]; ok {
			out[addr] = b
			continue
		}

		out[addr] = tron.Balance{TRX: decimal.Zero, USDT: decimal.Zero, Activated: false}
	}

	return out, errs
}

func (f *chainFake) BalanceStream(ctx context.Context, addresses []string, refresh bool) <-chan tron.BalanceResult {
	if f.balanceStream != nil {
		return f.balanceStream(ctx, addresses, refresh)
	}

	out := make(chan tron.BalanceResult, len(addresses))
	balances, errs := f.Balances(ctx, addresses, refresh)

	for _, addr := range addresses {
		out <- tron.BalanceResult{Address: addr, Balance: balances[addr], Err: errs[addr]}
	}
	close(out)

	return out
}

func (f *chainFake) TransactionConfirmed(_ context.Context, txid string) (bool, error) {
	f.confirmedID = txid
	return f.confirmed, f.confirmErr
}

func (f *chainFake) Estimate(_ context.Context, from, to string, asset tron.Asset, amount decimal.Decimal) (tron.Estimate, error) {
	f.estFrom, f.estTo, f.estAsset, f.estAmount = from, to, asset, amount

	if f.estimateErr != nil {
		return tron.Estimate{}, f.estimateErr
	}

	return f.estimate, nil
}

func (f *chainFake) Spendable(_ context.Context, from, to string, asset tron.Asset) (decimal.Decimal, tron.Estimate, error) {
	f.estFrom, f.estTo, f.estAsset = from, to, asset
	f.spendableCalls++

	if f.spendableErr != nil {
		return decimal.Zero, tron.Estimate{}, f.spendableErr
	}

	return f.spendable, f.estimate, nil
}

func (f *chainFake) Shortfall(_ context.Context, from string, asset tron.Asset, amount decimal.Decimal, est tron.Estimate) (decimal.Decimal, error) {
	// Recorded like every other operation: the asset genuinely changes the
	// answer — tron.Shortfall adds the amount to the need only for TRX — so a
	// handler that passed the recipient, the wrong asset or a stale amount
	// would otherwise still return a plausible figure.
	f.sfFrom, f.sfAsset, f.sfAmount, f.sfEstimate = from, asset, amount, est

	if f.shortfallErr != nil {
		return decimal.Zero, f.shortfallErr
	}

	return f.shortfall, nil
}

func (f *chainFake) Send(_ context.Context, from, to string, asset tron.Asset, amount decimal.Decimal, key *ecdsa.PrivateKey) (string, error) {
	if f.sendErr != nil {
		return "", f.sendErr
	}

	f.sentFrom, f.sentTo, f.sentAsset, f.sentAmount, f.sentKey = from, to, asset, amount, key

	return "deadbeef", nil
}

func (f *chainFake) Resources(_ context.Context, addr string) (tron.Resources, error) {
	f.resourcesAddr = addr

	if f.resourcesErr != nil {
		return tron.Resources{}, f.resourcesErr
	}

	return f.resources, nil
}

// record captures one staking operation and stands in for the txid a node would
// return.
func (f *chainFake) record(name, from, to string, resource tron.Resource, amount decimal.Decimal, key *ecdsa.PrivateKey) (string, error) {
	if f.stakeErr != nil {
		return "", f.stakeErr
	}

	f.opName, f.opFrom, f.opTo, f.opResource, f.opAmount, f.opKey = name, from, to, resource, amount, key

	return "cafebabe", nil
}

func (f *chainFake) Stake(_ context.Context, from string, resource tron.Resource, amount decimal.Decimal, key *ecdsa.PrivateKey) (string, error) {
	return f.record("stake", from, "", resource, amount, key)
}

func (f *chainFake) Unstake(_ context.Context, from string, resource tron.Resource, amount decimal.Decimal, key *ecdsa.PrivateKey) (string, error) {
	return f.record("unstake", from, "", resource, amount, key)
}

func (f *chainFake) Delegate(_ context.Context, from, to string, resource tron.Resource, amount decimal.Decimal, key *ecdsa.PrivateKey) (string, error) {
	return f.record("delegate", from, to, resource, amount, key)
}

func (f *chainFake) Reclaim(_ context.Context, from, to string, resource tron.Resource, amount decimal.Decimal, key *ecdsa.PrivateKey) (string, error) {
	return f.record("reclaim", from, to, resource, amount, key)
}

func (f *chainFake) ReclaimAll(_ context.Context, from, to string, resource tron.Resource, key *ecdsa.PrivateKey) (string, error) {
	return f.record("reclaim-all", from, to, resource, decimal.Zero, key)
}

func (f *chainFake) WithdrawUnstaked(_ context.Context, from string, key *ecdsa.PrivateKey) (string, error) {
	return f.record("withdraw", from, "", "", decimal.Zero, key)
}

func (f *chainFake) CancelUnstakes(_ context.Context, from string, key *ecdsa.PrivateKey) (string, error) {
	return f.record("cancel", from, "", "", decimal.Zero, key)
}

func (f *chainFake) EstimateDeploy(_ context.Context, from string, d tron.Deployment) (tron.DeployCost, error) {
	f.deployFrom, f.costOf = from, d

	if f.costErr != nil {
		return tron.DeployCost{}, f.costErr
	}

	return f.cost, nil
}

func (f *chainFake) Deploy(_ context.Context, from string, d tron.Deployment, key *ecdsa.PrivateKey) (tron.Deployed, error) {
	f.deployCalls++

	if f.deployErr != nil {
		return tron.Deployed{}, f.deployErr
	}

	f.deployFrom, f.deployment, f.deployKey = from, d, key

	return f.deployed, nil
}
