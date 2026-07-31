package tron_test

import (
	"io"
	"log/slog"
	"os"
	"testing"

	tronchain "github.com/sxwebdev/walletspace/internal/chain/tron"
	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/rpcpool"
)

func TestMainnetHealthThroughResolverLive(t *testing.T) {
	if os.Getenv("WALLETSPACE_LIVE_TRON_TESTS") != "1" {
		t.Skip("set WALLETSPACE_LIVE_TRON_TESTS=1 to use public Tron services")
	}

	settings, err := config.NewHomeManager(t.TempDir())
	if err != nil {
		t.Fatalf("config.NewHomeManager() error = %v", err)
	}
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	adapter, err := tronchain.New(
		t.Context(),
		registry,
		settings,
		rpcpool.New(settings),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("tron.New() error = %v", err)
	}
	t.Cleanup(adapter.Close)

	if err := adapter.Health(t.Context(), "tron-mainnet"); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
}
