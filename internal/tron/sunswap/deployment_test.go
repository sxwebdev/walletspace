package sunswap_test

import (
	"testing"

	"github.com/sxwebdev/walletspace/internal/tron/sunswap"
)

// Every contract a deployment names has to decode: an address that does not
// would surface only when a user tried to swap through it.
func TestEveryDeploymentDecodes(t *testing.T) {
	t.Parallel()
	for _, network := range []string{"mainnet", "nile"} {
		d, ok := sunswap.ForNetwork(network)
		if !ok {
			t.Fatalf("no deployment on %s", network)
		}
		if _, err := d.Addresses(); err != nil {
			t.Errorf("%s: %v", network, err)
		}
		if len(d.V3Fees) == 0 || len(d.V4Pools) == 0 {
			t.Errorf("%s lists no V3 or V4 pools", network)
		}
		for _, pool := range d.V4Pools {
			if pool.TickSpacing <= 0 {
				t.Errorf("%s: V4 pool %d has tick spacing %d", network, pool.Fee, pool.TickSpacing)
			}
		}
	}
	if _, ok := sunswap.ForNetwork("shasta"); ok {
		t.Error("a deployment was found for a network that has none")
	}
}

func TestAddressesRefusesABrokenDeployment(t *testing.T) {
	t.Parallel()
	d, _ := sunswap.ForNetwork("mainnet")
	d.Permit2 = "TTJxU3P8rHycAyFY4kVtGNfmnMH4ezcuM8" // one character off: bad checksum
	if _, err := d.Addresses(); err == nil {
		t.Fatal("a mistyped address decoded")
	}
}
