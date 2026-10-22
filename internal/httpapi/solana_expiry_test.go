package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/chain/chaintest"
	"github.com/sxwebdev/walletspace/internal/httpapi"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/operation"
	"github.com/sxwebdev/walletspace/internal/space"
)

// expiryAdapter records the expiry heights it is handed and when, relative to
// the status lookups.
type expiryAdapter struct {
	*chaintest.Adapter
	mu     sync.Mutex
	events []string
}

func (a *expiryAdapter) RecordBlockhashExpiry(networkID, txID string, height uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, "expiry "+networkID+" "+txID+" "+string(rune('0'+height%10)))
}

func (a *expiryAdapter) Transaction(ctx context.Context, networkID, txID string) (chain.Transaction, error) {
	a.mu.Lock()
	a.events = append(a.events, "status "+txID)
	a.mu.Unlock()
	return a.Adapter.Transaction(ctx, networkID, txID)
}

// After a restart the adapter knows nothing of a transaction's expiry; the
// height persisted with the operation is handed back to it before the status
// is asked, and only for that operation's own network.
func TestTheStatusLookupRestoresThePersistedExpiry(t *testing.T) {
	t.Parallel()

	fixture := newPlatformFixture(t)
	adapter := &expiryAdapter{Adapter: &chaintest.Adapter{Kind: "spl"}}
	chains, err := chain.NewAdapters(map[network.Family]chain.Adapter{network.FamilySolana: adapter})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := network.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	created, err := fixture.spaces.Create(space.CreateRequest{Password: "test-vault-password"})
	if err != nil {
		t.Fatal(err)
	}
	spaceID := created.Space.ID
	// What the previous process left behind.
	stored := operation.New(fixture.settings.Home())
	for key, record := range map[string]struct{ network, tx string }{
		"a": {"solana-mainnet", "sig-main"}, "b": {"solana-devnet", "sig-dev"},
	} {
		if _, _, err := stored.Begin(spaceID, key, "hash-"+key, record.network); err != nil {
			t.Fatal(err)
		}
		if _, err := stored.UpdateExpiring(spaceID, key, record.tx, operation.StatusBroadcasting, 7); err != nil {
			t.Fatal(err)
		}
	}
	handler, err := httpapi.NewPlatform(
		fixture.spaces, fixture.settings, registry, operation.New(fixture.settings.Home()),
		fixture.assets, chains, fixture.evm, fixture.tron, fixture.doctor, fixture.prices,
		testAccess(), slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/api/spaces/" + spaceID + "/networks/solana-mainnet/transactions/sig-main",
		// Recorded on devnet, asked about on mainnet: not handed over.
		"/api/spaces/" + spaceID + "/networks/solana-mainnet/transactions/sig-dev",
		"/api/spaces/" + spaceID + "/networks/solana-mainnet/transactions/sig-unknown",
	} {
		if response := platformRequest(t, handler, http.MethodGet, path, nil); response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, response.Code, response.Body)
		}
	}
	want := []string{"expiry solana-mainnet sig-main 7", "status sig-main", "status sig-dev", "status sig-unknown"}
	if len(adapter.events) != len(want) {
		t.Fatalf("events = %v, want %v", adapter.events, want)
	}
	for i := range want {
		if adapter.events[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, adapter.events[i], want[i])
		}
	}
}
