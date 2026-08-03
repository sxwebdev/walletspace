package doctor_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/doctor"
	"github.com/sxwebdev/walletspace/internal/network"
)

type endpointsFake struct{}

func (endpointsFake) Endpoints(_ context.Context, item network.Network) ([]string, error) {
	if item.ID == "bsc-mainnet" {
		return []string{"not a url"}, nil
	}
	if item.ID == "polygon-mainnet" {
		return []string{"https://polygon-mainnet.example:8545/rpc/private-token"}, nil
	}
	return []string{"https://tenant-secret@" + item.ID + ".example/rpc/private-token"}, nil
}

type endpointsFunc func(context.Context, network.Network) ([]string, error)

func (fn endpointsFunc) Endpoints(ctx context.Context, item network.Network) ([]string, error) {
	return fn(ctx, item)
}

func TestDoctorChecksEveryNetworkAndReportsRecovery(t *testing.T) {
	t.Parallel()

	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	var failEthereum atomic.Bool
	failEthereum.Store(true)
	checked := make(chan struct{}, len(registry.List())*2)
	nodeDoctor, err := doctor.New(
		t.Context(), registry, endpointsFake{},
		func(_ context.Context, item network.Network, _ string) error {
			checked <- struct{}{}
			if item.ID == "ethereum-mainnet" && failEthereum.Load() {
				return context.DeadlineExceeded
			}
			return nil
		},
		doctor.Options{Interval: time.Hour, ProbeTimeout: time.Second},
	)
	if err != nil {
		t.Fatalf("doctor.New() error = %v", err)
	}
	t.Cleanup(nodeDoctor.Close)

	waitForChecks(t, checked, len(registry.List()))
	snapshot := waitForStatus(t, nodeDoctor, "degraded")
	if snapshot.Status != "degraded" || snapshot.Healthy != snapshot.Total-1 ||
		snapshot.FailedNodes != 1 {
		t.Fatalf("degraded snapshot = %+v", snapshot)
	}
	ethereum := findNetwork(t, snapshot, "ethereum-mainnet")
	if ethereum.Status != "unavailable" || len(ethereum.Nodes) != 1 {
		t.Fatalf("Ethereum status = %+v", ethereum)
	}
	if ethereum.Nodes[0].Label != "RPC node 1" {
		t.Fatalf("node label leaked endpoint details: %q", ethereum.Nodes[0].Label)
	}

	failEthereum.Store(false)
	nodeDoctor.Refresh()
	waitForChecks(t, checked, len(registry.List()))
	recovered := waitForStatus(t, nodeDoctor, "healthy")
	if recovered.Status != "healthy" || recovered.Healthy != recovered.Total ||
		recovered.FailedNodes != 0 {
		t.Fatalf("recovered snapshot = %+v", recovered)
	}
	if got := findNetwork(t, recovered, "bsc-mainnet").Nodes[0].Label; got != "RPC node 1" {
		t.Fatalf("invalid endpoint label = %q", got)
	}
	if got := findNetwork(t, recovered, "polygon-mainnet").Nodes[0].Label; got != "RPC node 1" {
		t.Fatalf("provider endpoint was not redacted: %q", got)
	}
}

func TestDoctorValidatesDependenciesAndHandlesNoEnabledNetworks(t *testing.T) {
	t.Parallel()

	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	tests := []struct {
		name      string
		registry  *network.Registry
		endpoints doctor.EndpointProvider
		verify    doctor.VerifyFunc
	}{
		{name: "registry", endpoints: endpointsFake{}, verify: func(context.Context, network.Network, string) error { return nil }},
		{name: "endpoints", registry: registry, verify: func(context.Context, network.Network, string) error { return nil }},
		{name: "verifier", registry: registry, endpoints: endpointsFake{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := doctor.New(
				t.Context(), test.registry, test.endpoints, test.verify, doctor.Options{},
			); err == nil {
				t.Fatal("doctor.New() error = nil")
			}
		})
	}

	nodeDoctor, err := doctor.New(
		t.Context(), registry, endpointsFake{},
		func(context.Context, network.Network, string) error {
			t.Fatal("verifier called for disabled network")
			return nil
		},
		doctor.Options{
			Networks: func() []network.Network {
				items := registry.List()
				for i := range items {
					items[i].Enabled = false
				}
				return items
			},
		},
	)
	if err != nil {
		t.Fatalf("doctor.New() error = %v", err)
	}
	t.Cleanup(nodeDoctor.Close)
	waitForStatus(t, nodeDoctor, "unavailable")

	var nilDoctor *doctor.Doctor
	nilDoctor.Refresh()
	nilDoctor.Close()
}

func TestDoctorHandlesPartialNetworkAndOverlappingChecks(t *testing.T) {
	t.Parallel()

	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	ethereum, err := registry.Get("ethereum-mainnet")
	if err != nil {
		t.Fatalf("registry.Get() error = %v", err)
	}
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	nodeDoctor, err := doctor.New(
		t.Context(), registry,
		endpointsFunc(func(context.Context, network.Network) ([]string, error) {
			return []string{"https://healthy.example", "https://failed.example"}, nil
		}),
		func(_ context.Context, _ network.Network, endpoint string) error {
			started <- struct{}{}
			<-release
			if endpoint == "https://failed.example" {
				return context.DeadlineExceeded
			}
			return nil
		},
		doctor.Options{
			Interval: time.Hour,
			Networks: func() []network.Network { return []network.Network{ethereum} },
		},
	)
	if err != nil {
		t.Fatalf("doctor.New() error = %v", err)
	}
	t.Cleanup(nodeDoctor.Close)
	waitForChecks(t, started, 2)
	// A caller arriving during a running background pass must not start a
	// duplicate set of probes.
	nodeDoctor.Check(t.Context())
	close(release)

	snapshot := waitForStatus(t, nodeDoctor, "unavailable")
	status := findNetwork(t, snapshot, ethereum.ID)
	if status.Status != "degraded" || status.Healthy != 1 || status.Total != 2 {
		t.Fatalf("partial network status = %+v", status)
	}
}

func TestDoctorDoesNotMutateNetworksFromProvider(t *testing.T) {
	t.Parallel()

	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	disabled, err := registry.Get("ethereum-mainnet")
	if err != nil {
		t.Fatalf("registry.Get(Ethereum) error = %v", err)
	}
	disabled.Enabled = false
	enabled, err := registry.Get("bsc-mainnet")
	if err != nil {
		t.Fatalf("registry.Get(BSC) error = %v", err)
	}
	items := []network.Network{disabled, enabled}
	wantFirstID, wantSecondID := items[0].ID, items[1].ID

	nodeDoctor, err := doctor.New(
		t.Context(), registry, endpointsFake{},
		func(context.Context, network.Network, string) error { return nil },
		doctor.Options{
			Interval: time.Hour,
			Networks: func() []network.Network { return items },
		},
	)
	if err != nil {
		t.Fatalf("doctor.New() error = %v", err)
	}
	t.Cleanup(nodeDoctor.Close)
	if items[0].ID != wantFirstID || items[1].ID != wantSecondID || items[0].Enabled {
		t.Fatalf("Doctor mutated provider slice: %+v", items)
	}
	waitForStatus(t, nodeDoctor, "healthy")
}

func TestDoctorHandlesEndpointResolutionErrorsAndTicker(t *testing.T) {
	t.Parallel()

	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	ethereum, err := registry.Get("ethereum-mainnet")
	if err != nil {
		t.Fatalf("registry.Get() error = %v", err)
	}
	checks := make(chan struct{}, 2)
	nodeDoctor, err := doctor.New(
		nil, registry,
		endpointsFunc(func(context.Context, network.Network) ([]string, error) {
			checks <- struct{}{}
			return nil, context.DeadlineExceeded
		}),
		func(context.Context, network.Network, string) error { return nil },
		doctor.Options{
			Interval: 2 * time.Millisecond,
			Networks: func() []network.Network { return []network.Network{ethereum} },
		},
	)
	if err != nil {
		t.Fatalf("doctor.New() error = %v", err)
	}
	t.Cleanup(nodeDoctor.Close)
	waitForChecks(t, checks, 2)
	snapshot := waitForStatus(t, nodeDoctor, "unavailable")
	status := findNetwork(t, snapshot, ethereum.ID)
	if status.Total != 0 || status.Status != "unavailable" {
		t.Fatalf("endpoint resolution status = %+v", status)
	}
}

func TestDoctorConcurrencyIsGlobalAcrossNetworksAndNodes(t *testing.T) {
	t.Parallel()

	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	items := registry.List()[:3]
	const endpointsPerNetwork = 4
	started := make(chan struct{}, len(items)*endpointsPerNetwork)
	release := make(chan struct{})
	var inFlight, maximum atomic.Int64
	nodeDoctor, err := doctor.New(
		t.Context(), registry,
		endpointsFunc(func(context.Context, network.Network) ([]string, error) {
			return []string{"rpc-1", "rpc-2", "rpc-3", "rpc-4"}, nil
		}),
		func(context.Context, network.Network, string) error {
			current := inFlight.Add(1)
			defer inFlight.Add(-1)
			for {
				observed := maximum.Load()
				if current <= observed || maximum.CompareAndSwap(observed, current) {
					break
				}
			}
			started <- struct{}{}
			<-release
			return nil
		},
		doctor.Options{
			Interval: time.Hour, Concurrency: 2,
			Networks: func() []network.Network { return items },
		},
	)
	if err != nil {
		t.Fatalf("doctor.New() error = %v", err)
	}
	t.Cleanup(nodeDoctor.Close)
	waitForChecks(t, started, 2)
	close(release)
	waitForStatus(t, nodeDoctor, "healthy")
	if got := maximum.Load(); got != 2 {
		t.Fatalf("maximum concurrent probes = %d, want 2", got)
	}
}

func waitForStatus(t *testing.T, nodeDoctor *doctor.Doctor, status string) doctor.Snapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot := nodeDoctor.Snapshot()
		if snapshot.Status == status {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("doctor status = %q, want %q", snapshot.Status, status)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForChecks(t *testing.T, checked <-chan struct{}, count int) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	t.Cleanup(func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	})
	for range count {
		select {
		case <-checked:
		case <-timer.C:
			t.Fatalf("doctor checked fewer than %d networks", count)
		}
	}
}

func findNetwork(t *testing.T, snapshot doctor.Snapshot, id string) doctor.NetworkStatus {
	t.Helper()
	for _, item := range snapshot.Networks {
		if item.NetworkID == id {
			return item
		}
	}
	t.Fatalf("network %q not found", id)
	return doctor.NetworkStatus{}
}
