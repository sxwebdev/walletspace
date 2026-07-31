// Package tron adapts the existing, tested Tron service to explicit network IDs.
package tron

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/network"
	legacy "github.com/sxwebdev/walletspace/internal/tron"
	"golang.org/x/sync/singleflight"
)

type Settings interface {
	NetworkOverride(id string) (config.NetworkOverride, bool)
}

type EndpointProvider interface {
	Endpoints(ctx context.Context, item network.Network) ([]string, error)
}

type EndpointReporter interface {
	MarkHealthy(item network.Network, endpoint string)
}

type EndpointInvalidator interface {
	Invalidate(networkID string)
}

type HTTPClientProvider interface {
	HTTPClient(item network.Network) *http.Client
}

type Resource = legacy.Resource
type Resources = legacy.Resources
type Deployment = legacy.Deployment
type DeployCost = legacy.DeployCost
type Deployed = legacy.Deployed

const (
	ResourceBandwidth = legacy.ResourceBandwidth
	ResourceEnergy    = legacy.ResourceEnergy
)

type Adapter struct {
	ctx       context.Context
	registry  *network.Registry
	settings  Settings
	endpoints EndpointProvider
	log       *slog.Logger

	mu       sync.Mutex
	services map[string]*legacy.Service
	readyAt  map[string]time.Time
	version  map[string]uint64
	init     singleflight.Group
}

func New(
	ctx context.Context,
	registry *network.Registry,
	settings Settings,
	endpoints EndpointProvider,
	log *slog.Logger,
) (*Adapter, error) {
	if registry == nil {
		return nil, errors.New("network registry is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Adapter{
		ctx: ctx, registry: registry, settings: settings, endpoints: endpoints, log: log,
		services: make(map[string]*legacy.Service), readyAt: make(map[string]time.Time),
		version: make(map[string]uint64),
	}, nil
}

func (a *Adapter) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, service := range a.services {
		service.Close()
		delete(a.services, id)
		delete(a.readyAt, id)
	}
}

func (a *Adapter) Invalidate(networkID string) {
	a.mu.Lock()
	service := a.services[networkID]
	delete(a.services, networkID)
	delete(a.readyAt, networkID)
	a.version[networkID]++
	a.mu.Unlock()
	if service != nil {
		// Keep the previous service alive for operations that obtained it before
		// the settings swap. New calls already see an empty cache slot.
		time.AfterFunc(time.Minute, func() { _ = service.Close() })
	}
	if invalidator, ok := a.endpoints.(EndpointInvalidator); ok {
		invalidator.Invalidate(networkID)
	}
}

func VerifyEndpoint(
	ctx context.Context,
	item network.Network,
	endpoint string,
	apiKey string,
	httpClient *http.Client,
) error {
	if item.Family != network.FamilyTron {
		return errors.New("network is not Tron")
	}
	expectedIdentity := "0xcd8690dc"
	if item.ChainID == config.NetworkMainnet {
		expectedIdentity = "0x2b6653dc"
	}
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "net_version", "params": []any{}, "id": 1,
	})
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, strings.TrimSuffix(endpoint, "/")+"/jsonrpc", bytes.NewReader(payload),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		request.Header.Set("TRON-PRO-API-KEY", apiKey)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("read Tron net_version: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Tron net_version returned HTTP %d", response.StatusCode)
	}
	var identity struct {
		Result string `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&identity); err != nil {
		return fmt.Errorf("decode Tron net_version: %w", err)
	}
	if identity.Result != expectedIdentity {
		return fmt.Errorf(
			"Tron genesis identity is %s, expected %s", identity.Result, expectedIdentity,
		)
	}
	return nil
}

// ProbeEndpoint verifies both Tron chain identity and a recent head block.
// Adapter initialization intentionally uses the lighter VerifyEndpoint to stay
// within public-provider startup rate limits; the background Doctor uses this
// complete probe on its slower cadence.
func ProbeEndpoint(
	ctx context.Context,
	item network.Network,
	endpoint string,
	apiKey string,
	httpClient *http.Client,
) error {
	if err := VerifyEndpoint(ctx, item, endpoint, apiKey, httpClient); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		strings.TrimSuffix(endpoint, "/")+"/wallet/getnowblock",
		bytes.NewReader([]byte("{}")),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		request.Header.Set("TRON-PRO-API-KEY", apiKey)
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("read Tron head block: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Tron head block returned HTTP %d", response.StatusCode)
	}
	var block struct {
		BlockHeader struct {
			RawData struct {
				Number    int64 `json:"number"`
				Timestamp int64 `json:"timestamp"`
			} `json:"raw_data"`
		} `json:"block_header"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&block); err != nil {
		return fmt.Errorf("decode Tron head block: %w", err)
	}
	if block.BlockHeader.RawData.Number <= 0 || block.BlockHeader.RawData.Timestamp <= 0 {
		return errors.New("Tron head block is incomplete")
	}
	age := time.Since(time.UnixMilli(block.BlockHeader.RawData.Timestamp))
	if age < -2*time.Minute || age > 5*time.Minute {
		return fmt.Errorf("Tron head block is stale: age %s", age.Round(time.Second))
	}
	return nil
}

func (a *Adapter) VerifyEndpoint(
	ctx context.Context,
	item network.Network,
	endpoint string,
	apiKey string,
) error {
	var client *http.Client
	if provider, ok := a.endpoints.(HTTPClientProvider); ok {
		client = provider.HTTPClient(item)
	}
	return VerifyEndpoint(ctx, item, endpoint, apiKey, client)
}

func (a *Adapter) Health(ctx context.Context, networkID string) error {
	a.mu.Lock()
	service := a.services[networkID]
	readyAt := a.readyAt[networkID]
	a.mu.Unlock()
	if service == nil {
		// Creating a service already proves the endpoint identity and performs
		// two on-chain metadata reads. Avoid an immediate fourth request to
		// public TronGrid, whose unauthenticated limit is three requests/s.
		_, err := a.service(networkID)
		return err
	}
	if time.Since(readyAt) < 5*time.Second {
		return nil
	}
	if err := service.Health(ctx); err != nil {
		a.Invalidate(networkID)
		return err
	}
	return nil
}

func (a *Adapter) BalanceStream(
	ctx context.Context,
	networkID string,
	accounts []chain.AccountAddress,
	assets []chain.Asset,
	refresh bool,
) <-chan chain.Balance {
	out := make(chan chain.Balance)
	go func() {
		defer close(out)
		service, err := a.service(networkID)
		if err != nil {
			for _, holder := range accounts {
				for _, asset := range assets {
					select {
					case out <- chain.Balance{AccountID: holder.AccountID, AssetID: asset.ID, Error: err.Error()}:
					case <-ctx.Done():
						return
					}
				}
			}
			return
		}
		byAddress := make(map[string]string, len(accounts))
		addresses := make([]string, 0, len(accounts))
		for _, holder := range accounts {
			byAddress[holder.Address] = holder.AccountID
			addresses = append(addresses, holder.Address)
		}
		for result := range service.BalanceStream(ctx, addresses, refresh) {
			for _, asset := range assets {
				balance := chain.Balance{
					AccountID: byAddress[result.Address], AssetID: asset.ID, Stale: result.Stale,
				}
				if result.Err != nil {
					balance.Error = result.Err.Error()
				} else {
					switch asset.Kind {
					case "native":
						balance.Amount = result.Balance.TRX.String()
					case "trc20":
						if asset.Contract == service.Token().Contract {
							balance.Amount = result.Balance.USDT.String()
						} else {
							value, err := service.TokenBalance(ctx, result.Address, asset.Contract, asset.Decimals)
							if err != nil {
								balance.Error = err.Error()
							} else {
								balance.Amount = value.String()
							}
						}
					default:
						balance.Error = "unsupported Tron asset"
					}
				}
				select {
				case out <- balance:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

func (a *Adapter) EstimateTransfer(
	ctx context.Context,
	networkID string,
	req chain.TransferRequest,
) (chain.TransferEstimate, error) {
	service, err := a.service(networkID)
	if err != nil {
		return chain.TransferEstimate{}, err
	}
	asset, amount, err := tronTransfer(req, networkID)
	if err != nil {
		return chain.TransferEstimate{}, err
	}
	var estimate legacy.Estimate
	if req.Asset.Kind == "trc20" && req.Asset.Contract != service.Token().Contract {
		estimate, err = service.EstimateToken(
			ctx, req.From, req.To, req.Asset.Contract, req.Asset.Decimals, amount,
		)
	} else {
		estimate, err = service.Estimate(ctx, req.From, req.To, asset, amount)
	}
	if err != nil {
		return chain.TransferEstimate{}, err
	}
	return chain.TransferEstimate{
		NetworkID: networkID, Amount: req.Amount, Fee: estimate.Fee.String(), FeeModel: "tron-resources",
	}, nil
}

func (a *Adapter) Send(
	ctx context.Context,
	networkID string,
	req chain.TransferRequest,
	signer chain.Signer,
) (chain.Transaction, error) {
	service, err := a.service(networkID)
	if err != nil {
		return chain.Transaction{}, err
	}
	asset, amount, err := tronTransfer(req, networkID)
	if err != nil {
		return chain.Transaction{}, err
	}
	var txID string
	if req.Asset.Kind == "trc20" && req.Asset.Contract != service.Token().Contract {
		txID, err = service.SendTokenWithSigner(
			ctx, req.From, req.To, req.Asset.Contract, req.Asset.Decimals, amount, signer,
		)
	} else {
		txID, err = service.SendWithSigner(ctx, req.From, req.To, asset, amount, signer)
	}
	if err != nil {
		return chain.Transaction{}, err
	}
	return chain.Transaction{NetworkID: networkID, Hash: txID, Status: "pending"}, nil
}

func (a *Adapter) Transaction(ctx context.Context, networkID, txID string) (chain.Transaction, error) {
	service, err := a.service(networkID)
	if err != nil {
		return chain.Transaction{}, err
	}
	confirmed, err := service.TransactionConfirmed(ctx, txID)
	if err != nil {
		return chain.Transaction{}, err
	}
	status := "pending"
	if confirmed {
		status = "confirmed"
	}
	return chain.Transaction{NetworkID: networkID, Hash: txID, Status: status}, nil
}

func (a *Adapter) TokenMetadata(ctx context.Context, networkID, contract string) (string, uint8, error) {
	service, err := a.service(networkID)
	if err != nil {
		return "", 0, err
	}
	return service.TokenMetadata(ctx, contract)
}

func (a *Adapter) Resources(ctx context.Context, networkID, address string) (Resources, error) {
	service, err := a.service(networkID)
	if err != nil {
		return Resources{}, err
	}
	return service.Resources(ctx, address)
}

func (a *Adapter) Stake(
	ctx context.Context,
	networkID, from string,
	resource Resource,
	amount decimal.Decimal,
	signer chain.Signer,
) (string, error) {
	service, err := a.service(networkID)
	if err != nil {
		return "", err
	}
	return service.StakeWithSigner(ctx, from, resource, amount, signer)
}

func (a *Adapter) Unstake(
	ctx context.Context,
	networkID, from string,
	resource Resource,
	amount decimal.Decimal,
	signer chain.Signer,
) (string, error) {
	service, err := a.service(networkID)
	if err != nil {
		return "", err
	}
	return service.UnstakeWithSigner(ctx, from, resource, amount, signer)
}

func (a *Adapter) Delegate(
	ctx context.Context,
	networkID, from, to string,
	resource Resource,
	amount decimal.Decimal,
	signer chain.Signer,
) (string, error) {
	service, err := a.service(networkID)
	if err != nil {
		return "", err
	}
	return service.DelegateWithSigner(ctx, from, to, resource, amount, signer)
}

func (a *Adapter) Reclaim(
	ctx context.Context,
	networkID, from, to string,
	resource Resource,
	amount decimal.Decimal,
	all bool,
	signer chain.Signer,
) (string, error) {
	service, err := a.service(networkID)
	if err != nil {
		return "", err
	}
	if all {
		return service.ReclaimAllWithSigner(ctx, from, to, resource, signer)
	}
	return service.ReclaimWithSigner(ctx, from, to, resource, amount, signer)
}

func (a *Adapter) Withdraw(
	ctx context.Context,
	networkID, from string,
	cancel bool,
	signer chain.Signer,
) (string, error) {
	service, err := a.service(networkID)
	if err != nil {
		return "", err
	}
	if cancel {
		return service.CancelUnstakesWithSigner(ctx, from, signer)
	}
	return service.WithdrawUnstakedWithSigner(ctx, from, signer)
}

func (a *Adapter) EstimateDeploy(
	ctx context.Context, networkID, from string, deployment Deployment,
) (DeployCost, error) {
	service, err := a.service(networkID)
	if err != nil {
		return DeployCost{}, err
	}
	return service.EstimateDeploy(ctx, from, deployment)
}

func (a *Adapter) Deploy(
	ctx context.Context,
	networkID, from string,
	deployment Deployment,
	signer chain.Signer,
) (Deployed, error) {
	service, err := a.service(networkID)
	if err != nil {
		return Deployed{}, err
	}
	return service.DeployWithSigner(ctx, from, deployment, signer)
}

func (a *Adapter) service(networkID string) (*legacy.Service, error) {
	value, err, _ := a.init.Do(networkID, func() (any, error) {
		return a.initService(networkID)
	})
	if err != nil {
		return nil, err
	}
	return value.(*legacy.Service), nil
}

func (a *Adapter) initService(networkID string) (*legacy.Service, error) {
	item, err := a.registry.Get(networkID)
	if err != nil {
		return nil, err
	}
	if item.Family != network.FamilyTron {
		return nil, fmt.Errorf("%w: %s is not Tron", chain.ErrInvalidRequest, networkID)
	}
	for {
		a.mu.Lock()
		existing := a.services[networkID]
		version := a.version[networkID]
		a.mu.Unlock()
		if existing != nil {
			return existing, nil
		}
		cfg := &config.Config{
			Network: item.ChainID, FeeLimitTRX: 50,
		}
		if item.ChainID == config.NetworkMainnet {
			cfg.USDTContract = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
		} else {
			cfg.USDTContract = "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf"
		}
		var endpoints []string
		if a.settings != nil {
			if override, ok := a.settings.NetworkOverride(networkID); ok {
				for _, endpoint := range override.RPCURLs {
					value, expandErr := config.ExpandValue(endpoint)
					if expandErr != nil {
						return nil, expandErr
					}
					endpoints = append(endpoints, value)
				}
				cfg.APIKey, err = config.ExpandValue(override.Headers["TRON-PRO-API-KEY"])
				if err != nil {
					return nil, err
				}
			}
		}
		if len(endpoints) == 0 && a.endpoints != nil {
			endpoints, err = a.endpoints.Endpoints(a.ctx, item)
			if err != nil {
				return nil, err
			}
		}
		if len(endpoints) > 0 {
			valid := make([]string, 0, len(endpoints))
			var failures []error
			var httpClient *http.Client
			if provider, ok := a.endpoints.(HTTPClientProvider); ok {
				httpClient = provider.HTTPClient(item)
			}
			for _, endpoint := range endpoints {
				checkCtx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
				verifyErr := VerifyEndpoint(checkCtx, item, endpoint, cfg.APIKey, httpClient)
				cancel()
				if verifyErr != nil {
					failures = append(failures, verifyErr)
					continue
				}
				valid = append(valid, endpoint)
			}
			if len(valid) == 0 {
				return nil, fmt.Errorf("no verified Tron RPC for %s: %w", networkID, errors.Join(failures...))
			}
			cfg.Nodes = strings.Join(valid, ",")
		}
		service, err := legacy.New(a.ctx, cfg, a.log.With("network_id", networkID))
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		if a.version[networkID] != version {
			a.mu.Unlock()
			_ = service.Close()
			if err := a.ctx.Err(); err != nil {
				return nil, err
			}
			continue
		}
		if existing := a.services[networkID]; existing != nil {
			a.mu.Unlock()
			_ = service.Close()
			return existing, nil
		}
		a.services[networkID] = service
		a.readyAt[networkID] = time.Now()
		if reporter, ok := a.endpoints.(EndpointReporter); ok {
			for _, endpoint := range strings.Split(cfg.Nodes, ",") {
				if endpoint != "" {
					reporter.MarkHealthy(item, endpoint)
				}
			}
		}
		a.mu.Unlock()
		return service, nil
	}
}

func tronTransfer(req chain.TransferRequest, networkID string) (legacy.Asset, decimal.Decimal, error) {
	if req.Asset.NetworkID != networkID {
		return "", decimal.Zero, fmt.Errorf("%w: asset belongs to another network", chain.ErrInvalidRequest)
	}
	amount, err := decimal.NewFromString(req.Amount)
	if err != nil || amount.Sign() <= 0 {
		return "", decimal.Zero, fmt.Errorf("%w: amount must be positive decimal", chain.ErrInvalidRequest)
	}
	switch req.Asset.Kind {
	case "native":
		return legacy.AssetTRX, amount, nil
	case "trc20":
		return legacy.AssetUSDT, amount, nil
	default:
		return "", decimal.Zero, fmt.Errorf("%w: unsupported Tron asset", chain.ErrInvalidRequest)
	}
}
