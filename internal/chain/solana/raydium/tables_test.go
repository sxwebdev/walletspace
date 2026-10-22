package raydium_test

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
)

func TestLookupTables(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join("testdata", "poolkeys_api_v3.json"))
	if err != nil {
		t.Fatal(err)
	}
	q, _, last := serveKeys(t, http.StatusOK, body)
	pools := []solana.PublicKey{
		key("2QdhepnKRTLjjSqPL1PtKNwqrUkoLee5Gqs8bvZhRdMv"), key("BZtgQEyS6eXUXicYPHecYQ7PybqodXQMvkjUbP4R8mUU"),
		key("11111111111111111111111111111112"), // not a pool the API knows
	}
	got, err := q.LookupTables(t.Context(), pools)
	if err != nil {
		t.Fatal(err)
	}
	// The tables api-v3 names for the two pools (poolkeys_api_v3.json).
	want := map[solana.PublicKey]solana.PublicKey{
		pools[0]: key("47Fi43gX1EHVN1LX1Nu73MDpmZ3yooB8fwtBuxufyzX7"),
		pools[1]: key("8TM3cxoXfnNXU7XUjsAeyN8niu7dQMDf5ZB47nmrTcLD"),
	}
	if len(got) != len(want) || got[pools[0]] != want[pools[0]] || got[pools[1]] != want[pools[1]] {
		t.Fatalf("tables %v, want %v", got, want)
	}
	r := last.Load()
	if r.URL.Path != "/pools/key/ids" || r.URL.Query().Get("ids") != strings.Join([]string{pools[0].String(), pools[1].String(), pools[2].String()}, ",") {
		t.Fatalf("request %s", r.URL)
	}
}

func TestLookupTablesRefuses(t *testing.T) {
	t.Parallel()
	pool := key("2QdhepnKRTLjjSqPL1PtKNwqrUkoLee5Gqs8bvZhRdMv")
	for name, tt := range map[string]struct {
		status int
		body   string
		pools  []solana.PublicKey
		want   error
	}{
		"no pools":      {http.StatusOK, `{}`, nil, nil},
		"eleven pools":  {http.StatusOK, `{}`, make([]solana.PublicKey, 11), nil},
		"server error":  {http.StatusBadGateway, `down`, []solana.PublicKey{pool}, raydium.ErrAPI},
		"not JSON":      {http.StatusOK, `nope`, []solana.PublicKey{pool}, raydium.ErrAPI},
		"not a success": {http.StatusOK, `{"success":false}`, []solana.PublicKey{pool}, raydium.ErrAPI},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q, _, _ := serveKeys(t, tt.status, []byte(tt.body))
			_, err := q.LookupTables(t.Context(), tt.pools)
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	// Entries that are null, not canonical, not asked for, or that name the
	// pool itself are left out.
	q, _, _ := serveKeys(t, http.StatusOK, []byte(`{"success":true,"data":[null,
		{"id":" 2QdhepnKRTLjjSqPL1PtKNwqrUkoLee5Gqs8bvZhRdMv","lookupTableAccount":"47Fi43gX1EHVN1LX1Nu73MDpmZ3yooB8fwtBuxufyzX7"},
		{"id":"BZtgQEyS6eXUXicYPHecYQ7PybqodXQMvkjUbP4R8mUU","lookupTableAccount":"8TM3cxoXfnNXU7XUjsAeyN8niu7dQMDf5ZB47nmrTcLD"},
		{"id":"2QdhepnKRTLjjSqPL1PtKNwqrUkoLee5Gqs8bvZhRdMv","lookupTableAccount":"2QdhepnKRTLjjSqPL1PtKNwqrUkoLee5Gqs8bvZhRdMv"},
		{"id":"2QdhepnKRTLjjSqPL1PtKNwqrUkoLee5Gqs8bvZhRdMv","lookupTableAccount":"x"}]}`))
	got, err := q.LookupTables(t.Context(), []solana.PublicKey{pool})
	if err != nil || len(got) != 0 {
		t.Fatalf("tables %v, %v", got, err)
	}
}

func TestNewQuoterRefusesABadPoolKeysURL(t *testing.T) {
	t.Parallel()
	if _, err := raydium.NewQuoter(http.DefaultClient, "", raydium.WithPoolKeysURL("ftp://x")); err == nil {
		t.Fatal("accepted")
	}
}
