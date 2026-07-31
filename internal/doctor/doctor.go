// Package doctor continuously verifies every configured RPC endpoint and keeps
// a redacted health snapshot for the UI.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sxwebdev/walletspace/internal/network"
	"golang.org/x/sync/errgroup"
)

type EndpointProvider interface {
	Endpoints(ctx context.Context, item network.Network) ([]string, error)
}

type VerifyFunc func(ctx context.Context, item network.Network, endpoint string) error

type Options struct {
	Interval     time.Duration
	ProbeTimeout time.Duration
	Concurrency  int
	Networks     func() []network.Network
}

type NodeStatus struct {
	Label       string    `json:"label"`
	Status      string    `json:"status"`
	LastChecked time.Time `json:"last_checked"`
}

type NetworkStatus struct {
	NetworkID   string       `json:"network_id"`
	Status      string       `json:"status"`
	Healthy     int          `json:"healthy"`
	Total       int          `json:"total"`
	LastChecked time.Time    `json:"last_checked"`
	Nodes       []NodeStatus `json:"nodes"`
}

type Snapshot struct {
	Status      string          `json:"status"`
	Healthy     int             `json:"healthy"`
	Total       int             `json:"total"`
	FailedNodes int             `json:"failed_nodes"`
	LastChecked time.Time       `json:"last_checked"`
	Networks    []NetworkStatus `json:"networks"`
}

type Doctor struct {
	networks  func() []network.Network
	endpoints EndpointProvider
	verify    VerifyFunc
	options   Options

	mu       sync.RWMutex
	snapshot Snapshot
	checking bool
	cancel   context.CancelFunc
	done     chan struct{}
	trigger  chan struct{}
}

func New(
	parent context.Context,
	networks *network.Registry,
	endpoints EndpointProvider,
	verify VerifyFunc,
	options Options,
) (*Doctor, error) {
	if networks == nil || endpoints == nil || verify == nil {
		return nil, errors.New("doctor requires networks, endpoints and verifier")
	}
	if parent == nil {
		parent = context.Background()
	}
	if options.Interval <= 0 {
		options.Interval = time.Minute
	}
	if options.ProbeTimeout <= 0 {
		options.ProbeTimeout = 8 * time.Second
	}
	if options.Concurrency <= 0 {
		options.Concurrency = 8
	}
	if options.Networks == nil {
		options.Networks = networks.List
	}
	items := enabledNetworks(options.Networks())
	initial := Snapshot{Status: "checking", Total: len(items)}
	for _, item := range items {
		initial.Networks = append(initial.Networks, NetworkStatus{
			NetworkID: item.ID,
			Status:    "checking",
		})
	}
	ctx, cancel := context.WithCancel(parent)
	d := &Doctor{
		networks: options.Networks, endpoints: endpoints, verify: verify, options: options,
		snapshot: initial, cancel: cancel, done: make(chan struct{}), trigger: make(chan struct{}, 1),
	}
	go d.loop(ctx)
	return d, nil
}

func (d *Doctor) Close() {
	if d == nil {
		return
	}
	d.cancel()
	<-d.done
}

func (d *Doctor) Refresh() {
	if d == nil {
		return
	}
	select {
	case d.trigger <- struct{}{}:
	default:
	}
}

func (d *Doctor) Snapshot() Snapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return cloneSnapshot(d.snapshot)
}

// Check performs one complete pass. It is exported for deterministic tests and
// operational tooling; the background loop uses the same path.
func (d *Doctor) Check(ctx context.Context) {
	d.mu.Lock()
	if d.checking {
		d.mu.Unlock()
		return
	}
	d.checking = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.checking = false
		d.mu.Unlock()
	}()

	items := enabledNetworks(d.networks())
	results := make([]NetworkStatus, len(items))
	limiter := make(chan struct{}, d.options.Concurrency)
	group, groupCtx := errgroup.WithContext(ctx)
	for i, item := range items {
		i, item := i, item
		group.Go(func() error {
			results[i] = d.checkNetwork(groupCtx, item, limiter)
			return nil
		})
	}
	_ = group.Wait()

	snapshot := summarize(results)
	d.mu.Lock()
	d.snapshot = snapshot
	d.mu.Unlock()
}

func (d *Doctor) loop(ctx context.Context) {
	defer close(d.done)
	d.Check(ctx)
	ticker := time.NewTicker(d.options.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			d.Check(ctx)
		case <-d.trigger:
			d.Check(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (d *Doctor) checkNetwork(
	ctx context.Context,
	item network.Network,
	limiter chan struct{},
) NetworkStatus {
	checkedAt := time.Now().UTC()
	result := NetworkStatus{NetworkID: item.ID, Status: "unavailable", LastChecked: checkedAt}
	if !acquire(ctx, limiter) {
		return result
	}
	endpoints, err := d.endpoints.Endpoints(ctx, item)
	release(limiter)
	if err != nil || len(endpoints) == 0 {
		return result
	}
	result.Total = len(endpoints)
	result.Nodes = make([]NodeStatus, len(endpoints))
	for i := range result.Nodes {
		result.Nodes[i] = NodeStatus{
			Label: endpointLabel(i), Status: "unhealthy", LastChecked: checkedAt,
		}
	}
	group, groupCtx := errgroup.WithContext(ctx)
	for i, endpoint := range endpoints {
		i, endpoint := i, endpoint
		group.Go(func() error {
			if !acquire(groupCtx, limiter) {
				return nil
			}
			defer release(limiter)
			probeCtx, cancel := context.WithTimeout(groupCtx, d.options.ProbeTimeout)
			err := d.verify(probeCtx, item, endpoint)
			cancel()
			status := "healthy"
			if err != nil {
				status = "unhealthy"
			}
			result.Nodes[i] = NodeStatus{
				Label:  endpointLabel(i),
				Status: status, LastChecked: checkedAt,
			}
			return nil
		})
	}
	_ = group.Wait()
	for _, node := range result.Nodes {
		if node.Status == "healthy" {
			result.Healthy++
		}
	}
	switch {
	case result.Healthy == result.Total:
		result.Status = "healthy"
	case result.Healthy > 0:
		result.Status = "degraded"
	}
	return result
}

func summarize(networks []NetworkStatus) Snapshot {
	sort.Slice(networks, func(i, j int) bool {
		return networks[i].NetworkID < networks[j].NetworkID
	})
	result := Snapshot{Status: "healthy", Total: len(networks), Networks: networks}
	for _, item := range networks {
		if item.Status == "healthy" {
			result.Healthy++
		}
		result.FailedNodes += item.Total - item.Healthy
		if item.LastChecked.After(result.LastChecked) {
			result.LastChecked = item.LastChecked
		}
	}
	switch {
	case len(networks) == 0:
		result.Status = "unavailable"
	case result.Healthy == len(networks):
		result.Status = "healthy"
	case result.Healthy == 0:
		result.Status = "unavailable"
	default:
		result.Status = "degraded"
	}
	return result
}

func enabledNetworks(items []network.Network) []network.Network {
	out := make([]network.Network, 0, len(items))
	for _, item := range items {
		if item.Enabled {
			out = append(out, item)
		}
	}
	return out
}

func endpointLabel(index int) string {
	return fmt.Sprintf("RPC node %d", index+1)
}

func acquire(ctx context.Context, limiter chan struct{}) bool {
	select {
	case limiter <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func release(limiter chan struct{}) {
	<-limiter
}

func cloneSnapshot(value Snapshot) Snapshot {
	value.Networks = append([]NetworkStatus(nil), value.Networks...)
	for i := range value.Networks {
		value.Networks[i].Nodes = append([]NodeStatus(nil), value.Networks[i].Nodes...)
	}
	return value
}
