// Package tron wraps the gotron client with the few operations the UI needs:
// reading TRX and TRC20 balances, and sending either asset.
package tron

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"github.com/shopspring/decimal"
	"github.com/sxwebdev/gotron/pkg/address"
	"github.com/sxwebdev/gotron/pkg/client"
	"github.com/sxwebdev/gotron/schema/pb/api"
	"github.com/sxwebdev/tronfaucet/internal/config"
	"golang.org/x/sync/errgroup"
)

const (
	// maxTokenDecimals mirrors the cap the gotron amount constructors enforce:
	// the number of decimal digits in the largest ABI uint256.
	maxTokenDecimals = 78
	// balanceTTL is how long a fetched balance is served from cache.
	balanceTTL = 15 * time.Second
	// Public TronGrid endpoints allow ~3 requests per second per IP and each
	// address costs two calls, so without an API key we stay deliberately slow.
	balanceConcurrencyPublic = 2
	balanceConcurrencyKeyed  = 8
)

// Asset identifies which currency an operation applies to.
type Asset string

const (
	AssetTRX  Asset = "trx"
	AssetUSDT Asset = "usdt"
)

// Balance is the pair of balances shown for one address.
type Balance struct {
	TRX       decimal.Decimal
	USDT      decimal.Decimal
	Activated bool
}

// TokenInfo describes the TRC20 contract configured as USDT.
type TokenInfo struct {
	Contract string
	Symbol   string
	Decimals int32
}

// Service talks to the Tron network.
type Service struct {
	client   *client.Client
	log      *slog.Logger
	token    TokenInfo
	feeLimit client.SUN
	nodes    int // number of configured nodes, used as the retry budget
	workers  int // parallel balance fetches
	cache    *ttlcache.Cache[string, Balance]
	// fetch reads one address from chain. It is a field so the caching and
	// fan-out logic in Balances can be exercised without a live node.
	fetch func(ctx context.Context, addr string) (Balance, error)
}

// retry runs call up to attempts times. Each attempt is routed to the next
// node, so a single dead endpoint does not fail the whole operation.
func retry[T any](ctx context.Context, attempts int, call func() (T, error)) (T, error) {
	var (
		zero T
		err  error
	)

	if attempts < 1 {
		attempts = 1
	}

	for range attempts {
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}

		var result T
		if result, err = call(); err == nil {
			return result, nil
		}
	}

	return zero, err
}

// New builds a client from the configuration and reads the token metadata
// once, so balances can be scaled without an extra call per request.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Service, error) {
	nodes, err := cfg.ParseNodes()
	if err != nil {
		return nil, err
	}

	nodeCfgs := make([]client.NodeConfig, 0, len(nodes))
	for _, n := range nodes {
		nc := client.NodeConfig{
			Protocol: client.ProtocolGRPC,
			Address:  n.Address,
			UseTLS:   n.TLS,
			Tier:     n.Tier,
		}
		if n.HTTP {
			nc.Protocol = client.ProtocolHTTP
			nc.UseTLS = false
		}
		if cfg.APIKey != "" {
			nc.Headers = map[string]string{"TRON-PRO-API-KEY": cfg.APIKey}
		}

		nodeCfgs = append(nodeCfgs, nc)
	}

	c, err := client.New(client.Config{
		Nodes:      nodeCfgs,
		Network:    client.Network(cfg.Network),
		Blockchain: "tron",
		Health:     client.HealthConfig{Logger: healthLogger{log}},
	})
	if err != nil {
		return nil, fmt.Errorf("create tron client: %w", err)
	}

	workers := balanceConcurrencyPublic
	if cfg.APIKey != "" {
		workers = balanceConcurrencyKeyed
	}

	feeLimit, err := client.FromTRX(decimal.NewFromInt(cfg.FeeLimitTRX))
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("fee limit of %d TRX: %w", cfg.FeeLimitTRX, err)
	}

	s := &Service{
		client:   c,
		log:      log,
		feeLimit: feeLimit,
		nodes:    len(nodeCfgs),
		workers:  workers,
		cache:    newBalanceCache(balanceTTL),
		token:    TokenInfo{Contract: cfg.USDTContract},
	}
	s.fetch = s.balance

	if err := s.loadTokenInfo(ctx); err != nil {
		c.Close()
		return nil, err
	}

	// Started only once the Service is going to be returned. Starting earlier
	// would leak the janitor goroutine on the error path above, with no handle
	// left to stop it — and Stop() is a no-op until Start has actually run, so
	// a caller could not clean it up either.
	go s.cache.Start()

	return s, nil
}

// newBalanceCache builds the balance cache.
//
// WithDisableTouchOnHit is essential: by default ttlcache extends an item's
// expiry every time it is read, so a UI that polls faster than the TTL would
// keep an address alive forever and never see a fresh balance.
func newBalanceCache(ttl time.Duration) *ttlcache.Cache[string, Balance] {
	return ttlcache.New(
		ttlcache.WithTTL[string, Balance](ttl),
		ttlcache.WithDisableTouchOnHit[string, Balance](),
	)
}

// Close stops the cache janitor and releases the underlying transports.
func (s *Service) Close() error {
	s.cache.Stop()

	return s.client.Close()
}

// Token returns the metadata of the configured TRC20 contract.
func (s *Service) Token() TokenInfo { return s.token }

func (s *Service) loadTokenInfo(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// At start-up the health checker has not probed anything yet, so the first
	// node picked may well be a dead one. Retry: every attempt is routed to the
	// next node in the tier.
	decimals, err := retry(ctx, s.nodes, func() (*big.Int, error) {
		return s.client.TRC20GetDecimals(ctx, s.token.Contract)
	})
	if err != nil {
		return fmt.Errorf("read decimals of %s: %w", s.token.Contract, err)
	}

	scale, err := validateDecimals(decimals)
	if err != nil {
		return fmt.Errorf("contract %s on %s: %w — check that USDT_CONTRACT points at a TRC20 token",
			s.token.Contract, s.client.GetNetwork(), err)
	}
	s.token.Decimals = scale

	symbol, err := retry(ctx, s.nodes, func() (string, error) {
		return s.client.TRC20GetSymbol(ctx, s.token.Contract)
	})
	if err != nil {
		return fmt.Errorf("read symbol of %s: %w", s.token.Contract, err)
	}
	s.token.Symbol = symbol

	s.log.Info("token contract resolved",
		"contract", s.token.Contract,
		"symbol", s.token.Symbol,
		"decimals", s.token.Decimals,
	)

	return nil
}

// Balances fetches the balances of every address in parallel. Per-address
// failures are returned in the errs map instead of failing the whole call.
func (s *Service) Balances(ctx context.Context, addresses []string, refresh bool) (map[string]Balance, map[string]error) {
	out := make(map[string]Balance, len(addresses))
	errs := make(map[string]error)

	var (
		mu      sync.Mutex
		pending []string
	)

	if refresh {
		pending = addresses
	} else {
		for _, addr := range addresses {
			if item := s.cache.Get(addr); item != nil {
				out[addr] = item.Value()
				continue
			}

			pending = append(pending, addr)
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.workers)

	fetched := make(map[string]Balance, len(pending))

	for _, addr := range pending {
		g.Go(func() error {
			b, err := s.fetch(gctx, addr)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[addr] = err
				return nil
			}
			fetched[addr] = b

			return nil
		})
	}

	// The goroutines never return an error; failures land in errs.
	_ = g.Wait()

	// Only freshly fetched balances are stored: writing back a cache hit would
	// restart its TTL and the entry would never go stale.
	for addr, b := range fetched {
		s.cache.Set(addr, b, ttlcache.DefaultTTL)
		out[addr] = b
	}

	return out, errs
}

// invalidate drops the cached balances of the given addresses, so the next
// read goes back to the chain. Both sides of a transfer have moved.
func (s *Service) invalidate(addresses ...string) {
	for _, addr := range addresses {
		s.cache.Delete(addr)
	}
}

// accountBalance carries the TRX balance together with whether the account
// exists on chain at all.
type accountBalance struct {
	trx       client.SUN
	activated bool
}

func (s *Service) balance(ctx context.Context, addr string) (Balance, error) {
	// An address that has never received anything simply does not exist on
	// chain yet — that is a zero balance of an unactivated account, not a
	// failure, so it must not be retried against the other nodes.
	account, err := retry(ctx, s.nodes, func() (accountBalance, error) {
		v, err := s.client.GetAccountBalance(ctx, addr)
		if errors.Is(err, client.ErrAccountNotFound) {
			return accountBalance{}, nil
		}
		if err != nil {
			return accountBalance{}, err
		}

		return accountBalance{trx: v, activated: true}, nil
	})
	if err != nil {
		return Balance{}, fmt.Errorf("trx balance: %w", err)
	}

	tokens, err := retry(ctx, s.nodes, func() (client.TokenAmount, error) {
		return s.client.TRC20ContractBalance(ctx, addr, s.token.Contract)
	})
	if err != nil {
		return Balance{}, fmt.Errorf("%s balance: %w", s.token.Symbol, err)
	}

	return Balance{
		TRX:       account.trx.TRX(),
		USDT:      tokens.Decimal(s.token.Decimals),
		Activated: account.activated,
	}, nil
}

// Send transfers TRX or the configured TRC20 token and returns the txid.
func (s *Service) Send(ctx context.Context, from, to string, asset Asset, amount decimal.Decimal, key *ecdsa.PrivateKey) (string, error) {
	if err := address.Validate(to); err != nil {
		return "", fmt.Errorf("%w: invalid recipient address: %s", ErrInvalidRequest, err)
	}

	if amount.LessThanOrEqual(decimal.Zero) {
		return "", fmt.Errorf("%w: amount must be greater than zero", ErrInvalidRequest)
	}

	var (
		tx  *api.TransactionExtention
		err error
	)

	switch asset {
	case AssetTRX:
		sun, convErr := trxAmount(amount)
		if convErr != nil {
			return "", convErr
		}

		// Building a transaction is a read-only call on the node, so it is safe
		// to route past an unhealthy endpoint. Only the broadcast below must
		// not be retried.
		tx, err = retry(ctx, s.nodes, func() (*api.TransactionExtention, error) {
			return s.client.CreateTransferTransaction(ctx, from, to, sun)
		})
		switch {
		case err == nil:
		case errors.Is(err, client.ErrInvalidTransaction):
			// The node rejects the transfer without saying why; in practice the
			// sender is empty or not activated yet.
			err = fmt.Errorf("node rejected the TRX transfer: check that %s holds enough TRX to cover the amount and the fee", from)
		default:
			err = fmt.Errorf("create TRX transfer: %w", err)
		}

	case AssetUSDT:
		tokens, convErr := tokenAmount(amount, s.token.Decimals)
		if convErr != nil {
			return "", convErr
		}

		tx, err = retry(ctx, s.nodes, func() (*api.TransactionExtention, error) {
			return s.client.TRC20Send(ctx, from, to, s.token.Contract, tokens, s.feeLimit)
		})
		if err != nil {
			err = fmt.Errorf("create %s transfer: %w", s.token.Symbol, err)
		}

	default:
		return "", fmt.Errorf("%w: unknown asset %q", ErrInvalidRequest, asset)
	}
	if err != nil {
		return "", err
	}

	if err := s.client.SignTransaction(tx.GetTransaction(), key); err != nil {
		return "", fmt.Errorf("sign transaction: %w", err)
	}

	// A rejection arrives as *client.BroadcastError, whose message always names
	// the node's response code.
	if _, err := s.client.BroadcastTransaction(ctx, tx.GetTransaction()); err != nil {
		return "", fmt.Errorf("broadcast transaction: %w", err)
	}

	s.invalidate(from, to)

	return hex.EncodeToString(tx.GetTxid()), nil
}

// healthLogger bridges the client's health checker to slog.
type healthLogger struct{ log *slog.Logger }

func (l healthLogger) Infof(format string, args ...any) {
	l.log.Info(fmt.Sprintf(format, args...))
}
