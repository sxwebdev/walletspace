package httpapi_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The API has no authentication and holds spendable keys, so a page the user
// happens to visit must not be able to drive it through their browser.
func TestGuardRejectsCrossSiteWrites(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		headers    map[string]string
		wantStatus int
	}{
		{
			name:       "cross-site fetch",
			headers:    map[string]string{"Sec-Fetch-Site": "cross-site", "Content-Type": "application/json"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "same-site but different origin",
			headers:    map[string]string{"Sec-Fetch-Site": "same-site", "Content-Type": "application/json"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "foreign Origin header",
			headers:    map[string]string{"Origin": "https://evil.example", "Content-Type": "application/json"},
			wantStatus: http.StatusForbidden,
		},
		{
			// text/plain is what a form-less cross-site fetch uses to dodge the
			// CORS preflight; refusing it forces the preflight to happen.
			name:       "simple-request content type",
			headers:    map[string]string{"Content-Type": "text/plain;charset=UTF-8"},
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name:       "form content type",
			headers:    map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
			wantStatus: http.StatusUnsupportedMediaType,
		},
		{
			name:       "same-origin json is allowed through",
			headers:    map[string]string{"Sec-Fetch-Site": "same-origin", "Content-Type": "application/json"},
			wantStatus: http.StatusCreated,
		},
		{
			name:       "no browser headers at all (curl) is allowed",
			headers:    map[string]string{"Content-Type": "application/json"},
			wantStatus: http.StatusCreated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			chain := newChainFake()
			srv := newServer(t, newWalletsFake(), chain)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				srv.URL+"/api/wallets", strings.NewReader(`{"label":"x"}`))
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}

			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("POST /api/wallets error = %v", err)
			}
			defer res.Body.Close()

			if res.StatusCode != tt.wantStatus {
				body, _ := io.ReadAll(res.Body)
				t.Errorf("status = %d, want %d (body: %s)", res.StatusCode, tt.wantStatus, body)
			}
		})
	}
}

func TestGuardAllowsSameOriginSend(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	srv := newServer(t, newWalletsFake(), chain)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/api/wallets/0/send", strings.NewReader(`{"asset":"trx","to":"TRecipient","amount":"1"}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", srv.URL)
	req.Header.Set("Sec-Fetch-Site", "same-origin")

	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("send error = %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 200 (body: %s)", res.StatusCode, body)
	}

	if chain.sentFrom == "" {
		t.Error("the transfer never reached the chain service")
	}
}

func TestGuardRejectsOversizedBody(t *testing.T) {
	t.Parallel()

	chain := newChainFake()
	srv := newServer(t, newWalletsFake(), chain)

	// An unbounded body would be accumulated whole before the address is even
	// validated, which is enough to OOM a laptop-hosted faucet.
	huge := `{"asset":"trx","amount":"1","to":"` + strings.Repeat("A", 1<<20) + `"}`

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/api/wallets/0/send", strings.NewReader(huge))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("send error = %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusOK {
		t.Fatal("an oversized body was accepted")
	}

	if chain.sentFrom != "" {
		t.Error("an oversized body still reached the chain service")
	}
}

func TestGuardLeavesReadsAlone(t *testing.T) {
	t.Parallel()

	srv := newServer(t, newWalletsFake(), newChainFake())

	// Reads leak nothing an attacker cannot already learn, and blocking them
	// would break the UI's own no-header navigations.
	do(t, srv, http.MethodGet, "/api/wallets", "", http.StatusOK, nil)
}
