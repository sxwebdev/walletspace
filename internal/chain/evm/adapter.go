// Package evm implements one adapter for every configured EVM network.
package evm

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/shopspring/decimal"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/operation"
	"golang.org/x/sync/errgroup"
)

const erc20ABIJSON = `[
  {"constant":true,"inputs":[{"name":"owner","type":"address"}],"name":"balanceOf","outputs":[{"name":"","type":"uint256"}],"type":"function"},
  {"constant":true,"inputs":[],"name":"symbol","outputs":[{"name":"","type":"string"}],"type":"function"},
  {"constant":true,"inputs":[],"name":"decimals","outputs":[{"name":"","type":"uint8"}],"type":"function"},
  {"constant":false,"inputs":[{"name":"to","type":"address"},{"name":"amount","type":"uint256"}],"name":"transfer","outputs":[{"name":"","type":"bool"}],"type":"function"}
]`

type EndpointProvider interface {
	Endpoints(ctx context.Context, item network.Network) ([]string, error)
}

type StaticEndpoints struct{}

func (StaticEndpoints) Endpoints(_ context.Context, item network.Network) ([]string, error) {
	return append([]string(nil), item.RPCFallbacks...), nil
}

// HeaderProvider resolves the credentials that belong to one endpoint. The
// endpoint is part of the question: a provider key configured for a custom URL
// must not travel to the official fallback the resolver moves on to.
type HeaderProvider interface {
	Headers(item network.Network, endpoint string) (http.Header, error)
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

type Adapter struct {
	registry  *network.Registry
	endpoints EndpointProvider
	erc20     abi.ABI

	mu      sync.Mutex
	clients map[string]*ethclient.Client
	nonces  map[string]*sync.Mutex
	version map[string]uint64
}

func New(registry *network.Registry, endpoints EndpointProvider) (*Adapter, error) {
	if registry == nil {
		return nil, errors.New("network registry is required")
	}
	if endpoints == nil {
		endpoints = StaticEndpoints{}
	}
	parsed, err := abi.JSON(strings.NewReader(erc20ABIJSON))
	if err != nil {
		return nil, fmt.Errorf("parse ERC20 ABI: %w", err)
	}
	return &Adapter{
		registry: registry, endpoints: endpoints, erc20: parsed,
		clients: make(map[string]*ethclient.Client), nonces: make(map[string]*sync.Mutex),
		version: make(map[string]uint64),
	}, nil
}

func (a *Adapter) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, client := range a.clients {
		client.Close()
		delete(a.clients, id)
	}
}

func (a *Adapter) Invalidate(networkID string) {
	a.mu.Lock()
	client := a.clients[networkID]
	delete(a.clients, networkID)
	a.version[networkID]++
	a.mu.Unlock()
	if client != nil {
		// Requests that already captured the old immutable client snapshot are
		// allowed to finish. New requests immediately build a verified client
		// from the updated settings.
		time.AfterFunc(time.Minute, client.Close)
	}
	if invalidator, ok := a.endpoints.(EndpointInvalidator); ok {
		invalidator.Invalidate(networkID)
	}
}

func VerifyEndpoint(
	ctx context.Context,
	item network.Network,
	endpoint string,
	headers http.Header,
	httpClient *http.Client,
) error {
	if item.Family != network.FamilyEVM {
		return errors.New("network is not EVM")
	}
	options := []rpc.ClientOption{rpc.WithHeaders(headers)}
	if httpClient != nil {
		options = append(options, rpc.WithHTTPClient(httpClient))
	}
	rpcClient, err := rpc.DialOptions(ctx, endpoint, options...)
	if err != nil {
		return fmt.Errorf("connect RPC: %w", err)
	}
	client := ethclient.NewClient(rpcClient)
	defer client.Close()
	got, err := client.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("read eth_chainId: %w", err)
	}
	want, _ := new(big.Int).SetString(item.ChainID, 10)
	if got.Cmp(want) != 0 {
		return fmt.Errorf("RPC chain id is %s, expected %s", got, want)
	}
	if _, err := client.BlockNumber(ctx); err != nil {
		return fmt.Errorf("read eth_blockNumber: %w", err)
	}
	return nil
}

func (a *Adapter) VerifyEndpoint(
	ctx context.Context, item network.Network, endpoint string, headers http.Header,
) error {
	var client *http.Client
	if provider, ok := a.endpoints.(HTTPClientProvider); ok {
		client = provider.HTTPClient(item)
	}
	return VerifyEndpoint(ctx, item, endpoint, headers, client)
}

func (a *Adapter) Health(ctx context.Context, networkID string) error {
	_, client, err := a.client(ctx, networkID)
	if err != nil {
		return err
	}
	_, err = client.BlockNumber(ctx)
	if err != nil {
		a.Invalidate(networkID)
	}
	return err
}

func (a *Adapter) BalanceStream(
	ctx context.Context,
	networkID string,
	accounts []chain.AccountAddress,
	assets []chain.Asset,
) <-chan chain.Balance {
	out := make(chan chain.Balance)
	go func() {
		defer close(out)
		group, groupCtx := errgroup.WithContext(ctx)
		group.SetLimit(8)
		for _, holder := range accounts {
			for _, asset := range assets {
				group.Go(func() error {
					amount, err := a.Balance(groupCtx, networkID, holder.Address, asset)
					result := chain.Balance{AccountID: holder.AccountID, AssetID: asset.ID}
					if err != nil {
						result.Error = err.Error()
					} else {
						result.Amount = amount
					}
					select {
					case out <- result:
						return nil
					case <-groupCtx.Done():
						return groupCtx.Err()
					}
				})
			}
		}
		_ = group.Wait()
	}()
	return out
}

func (a *Adapter) Balance(ctx context.Context, networkID, holder string, asset chain.Asset) (string, error) {
	item, client, err := a.client(ctx, networkID)
	if err != nil {
		return "", err
	}
	if !common.IsHexAddress(holder) {
		return "", fmt.Errorf("%w: invalid EVM address", chain.ErrInvalidRequest)
	}
	address := common.HexToAddress(holder)
	var units *big.Int
	switch asset.Kind {
	case "native":
		units, err = client.BalanceAt(ctx, address, nil)
	case "erc20":
		if asset.NetworkID != item.ID || !common.IsHexAddress(asset.Contract) {
			return "", fmt.Errorf("%w: invalid ERC20 asset", chain.ErrInvalidRequest)
		}
		input, packErr := a.erc20.Pack("balanceOf", address)
		if packErr != nil {
			return "", packErr
		}
		result, callErr := client.CallContract(ctx, ethereum.CallMsg{
			To: new(common.HexToAddress(asset.Contract)), Data: input,
		}, nil)
		if callErr != nil {
			return "", callErr
		}
		values, unpackErr := a.erc20.Unpack("balanceOf", result)
		if unpackErr != nil || len(values) != 1 {
			return "", fmt.Errorf("decode ERC20 balance: %w", unpackErr)
		}
		var ok bool
		units, ok = values[0].(*big.Int)
		if !ok {
			return "", errors.New("ERC20 balance has an unexpected type")
		}
	default:
		return "", fmt.Errorf("%w: unsupported asset kind %q", chain.ErrInvalidRequest, asset.Kind)
	}
	if err != nil {
		return "", err
	}
	return decimal.NewFromBigInt(units, -int32(asset.Decimals)).String(), nil
}

func (a *Adapter) TokenMetadata(ctx context.Context, networkID, contract string) (string, uint8, error) {
	_, client, err := a.client(ctx, networkID)
	if err != nil {
		return "", 0, err
	}
	if !common.IsHexAddress(contract) {
		return "", 0, fmt.Errorf("%w: invalid ERC20 contract", chain.ErrInvalidRequest)
	}
	address := common.HexToAddress(contract)
	code, err := client.CodeAt(ctx, address, nil)
	if err != nil {
		return "", 0, fmt.Errorf("read contract code: %w", err)
	}
	if len(code) == 0 {
		return "", 0, fmt.Errorf("%w: address has no contract code", chain.ErrInvalidRequest)
	}
	call := func(method string) ([]any, error) {
		input, err := a.erc20.Pack(method)
		if err != nil {
			return nil, err
		}
		result, err := client.CallContract(ctx, ethereum.CallMsg{To: &address, Data: input}, nil)
		if err != nil {
			return nil, err
		}
		return a.erc20.Unpack(method, result)
	}
	symbolValues, err := call("symbol")
	if err != nil || len(symbolValues) != 1 {
		return "", 0, fmt.Errorf("read ERC20 symbol: %w", err)
	}
	symbol, ok := symbolValues[0].(string)
	if !ok || strings.TrimSpace(symbol) == "" {
		return "", 0, errors.New("ERC20 symbol has an unexpected type")
	}
	decimalValues, err := call("decimals")
	if err != nil || len(decimalValues) != 1 {
		return "", 0, fmt.Errorf("read ERC20 decimals: %w", err)
	}
	decimals, ok := decimalValues[0].(uint8)
	if !ok {
		return "", 0, errors.New("ERC20 decimals have an unexpected type")
	}
	return symbol, decimals, nil
}

func (a *Adapter) EstimateTransfer(
	ctx context.Context,
	networkID string,
	req chain.TransferRequest,
) (chain.TransferEstimate, error) {
	item, client, err := a.client(ctx, networkID)
	if err != nil {
		return chain.TransferEstimate{}, err
	}
	call, _, err := a.transferCall(item, req)
	if err != nil {
		return chain.TransferEstimate{}, err
	}
	gasLimit, err := client.EstimateGas(ctx, call)
	if err != nil {
		return chain.TransferEstimate{}, fmt.Errorf("estimate gas: %w", err)
	}
	fees, err := suggestFees(ctx, client)
	if err != nil {
		return chain.TransferEstimate{}, err
	}
	feeUnits := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), fees.maxFee())
	return chain.TransferEstimate{
		NetworkID: networkID, Amount: req.Amount,
		Fee:      decimal.NewFromBigInt(feeUnits, -int32(item.Native.Decimals)).String(),
		GasLimit: gasLimit, FeeModel: fees.model,
		MaxFeePerGas: fees.feeCap.String(), MaxPriorityFeePerGas: fees.tipCap.String(),
	}, nil
}

// EstimateMaxTransfer returns the largest currently sendable amount. Token
// fees are paid in the native asset, while a native transfer must reserve its
// maximum gas fee before exposing the amount to the caller.
func (a *Adapter) EstimateMaxTransfer(
	ctx context.Context,
	networkID string,
	req chain.TransferRequest,
) (chain.TransferEstimate, error) {
	balanceValue, err := a.Balance(ctx, networkID, req.From, req.Asset)
	if err != nil {
		return chain.TransferEstimate{}, err
	}
	balance, err := decimal.NewFromString(balanceValue)
	if err != nil || !balance.IsPositive() {
		return chain.TransferEstimate{}, fmt.Errorf(
			"%w: account holds no %s", chain.ErrInvalidRequest, req.Asset.Symbol,
		)
	}
	if req.Asset.Kind != "native" {
		req.Amount = balance.String()
		estimate, estimateErr := a.EstimateTransfer(ctx, networkID, req)
		if estimateErr != nil {
			return chain.TransferEstimate{}, estimateErr
		}
		item, itemErr := a.registry.Get(networkID)
		if itemErr != nil {
			return chain.TransferEstimate{}, itemErr
		}
		nativeBalanceValue, balanceErr := a.Balance(ctx, networkID, req.From, chain.Asset{
			ID: networkID + ":native", NetworkID: networkID, Kind: "native",
			Symbol: item.Native.Symbol, Decimals: item.Native.Decimals,
		})
		if balanceErr != nil {
			return chain.TransferEstimate{}, balanceErr
		}
		nativeBalance, nativeErr := decimal.NewFromString(nativeBalanceValue)
		fee, feeErr := decimal.NewFromString(estimate.Fee)
		if nativeErr != nil || feeErr != nil {
			return chain.TransferEstimate{}, errors.New("EVM estimate returned an invalid balance or fee")
		}
		if nativeBalance.LessThan(fee) {
			return chain.TransferEstimate{}, fmt.Errorf(
				"%w: %s balance %s does not cover the maximum fee %s",
				chain.ErrInvalidRequest, item.Native.Symbol, nativeBalance, fee,
			)
		}
		return estimate, nil
	}

	reservedFee := decimal.Zero
	candidate := decimal.New(1, -int32(req.Asset.Decimals))
	for range 4 {
		req.Amount = candidate.String()
		estimate, estimateErr := a.EstimateTransfer(ctx, networkID, req)
		if estimateErr != nil {
			return chain.TransferEstimate{}, estimateErr
		}
		fee, parseErr := decimal.NewFromString(estimate.Fee)
		if parseErr != nil || fee.IsNegative() {
			return chain.TransferEstimate{}, errors.New("EVM estimate returned an invalid fee")
		}
		if fee.GreaterThan(reservedFee) {
			reservedFee = fee
		}
		next := balance.Sub(reservedFee)
		if !next.IsPositive() {
			return chain.TransferEstimate{}, fmt.Errorf(
				"%w: balance %s does not cover the maximum fee %s",
				chain.ErrInvalidRequest, balance, reservedFee,
			)
		}
		if next.Equal(candidate) {
			estimate.Amount = candidate.String()
			estimate.Fee = reservedFee.String()
			return estimate, nil
		}
		candidate = next
	}
	return chain.TransferEstimate{}, errors.New("EVM maximum fee did not stabilize; retry")
}

func (a *Adapter) Send(
	ctx context.Context,
	networkID string,
	req chain.TransferRequest,
	signer chain.Signer,
) (chain.Transaction, error) {
	if signer == nil || signer.Family() != chain.FamilyEVM {
		return chain.Transaction{}, errors.New("EVM signer is required")
	}
	item, client, err := a.client(ctx, networkID)
	if err != nil {
		return chain.Transaction{}, err
	}
	publicKey, err := publicKey(signer.PublicKey())
	if err != nil {
		return chain.Transaction{}, err
	}
	from := crypto.PubkeyToAddress(*publicKey)
	if !common.IsHexAddress(req.From) || from != common.HexToAddress(req.From) {
		return chain.Transaction{}, fmt.Errorf("%w: signer does not match sender", chain.ErrInvalidRequest)
	}
	lock := a.nonceLock(networkID, from)
	lock.Lock()
	defer lock.Unlock()

	call, amountUnits, err := a.transferCall(item, req)
	if err != nil {
		return chain.Transaction{}, err
	}
	gasLimit, err := client.EstimateGas(ctx, call)
	if err != nil {
		return chain.Transaction{}, fmt.Errorf("estimate gas: %w", err)
	}
	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return chain.Transaction{}, fmt.Errorf("read pending nonce: %w", err)
	}
	current, err := suggestFees(ctx, client)
	if err != nil {
		return chain.Transaction{}, err
	}
	// What gets signed is what the user approved, never the answer the node
	// just gave. The fresh numbers are consulted only to notice that the
	// approval has gone stale, in which case the user has to see the new ones.
	fees, gasLimit, err := approvedFees(req.Approved, current, gasLimit)
	if err != nil {
		return chain.Transaction{}, err
	}
	chainID, _ := new(big.Int).SetString(item.ChainID, 10)
	var tx *types.Transaction
	if fees.model == "eip1559" {
		tx = types.NewTx(&types.DynamicFeeTx{
			ChainID: chainID, Nonce: nonce, GasTipCap: fees.tipCap, GasFeeCap: fees.feeCap,
			Gas: gasLimit, To: call.To, Value: amountUnits, Data: call.Data,
		})
	} else {
		tx = types.NewTx(&types.LegacyTx{
			Nonce: nonce, GasPrice: fees.feeCap, Gas: gasLimit, To: call.To,
			Value: amountUnits, Data: call.Data,
		})
	}
	txSigner := types.LatestSignerForChainID(chainID)
	signature, err := signer.SignDigest(ctx, txSigner.Hash(tx).Bytes())
	if err != nil {
		return chain.Transaction{}, err
	}
	signed, err := tx.WithSignature(txSigner, signature)
	clear(signature)
	if err != nil {
		return chain.Transaction{}, fmt.Errorf("attach EVM signature: %w", err)
	}
	hash := signed.Hash().Hex()
	if err := client.SendTransaction(ctx, signed); err != nil {
		if rejected(err) {
			return chain.Transaction{
					NetworkID: networkID, Hash: hash, Status: operation.StatusFailed,
				},
				fmt.Errorf("%w: broadcast %s: %w", chain.ErrInvalidRequest, hash, err)
		}
		return chain.Transaction{
				NetworkID: networkID, Hash: hash, Status: operation.StatusBroadcastUnknown,
			},
			fmt.Errorf("broadcast %s: %w", hash, errors.Join(chain.ErrBroadcastUnknown, err))
	}
	return chain.Transaction{NetworkID: networkID, Hash: hash, Status: operation.StatusPending}, nil
}

// rejected reports that the node answered and turned the transaction down, as
// opposed to never answering at all.
//
// The distinction decides what the caller may do next, and it used to be
// missing here: every send error became broadcast_unknown, so a flat
// "insufficient funds for gas" — where nothing reached the chain and a corrected
// retry is exactly right — was reported as a transaction that might be on chain
// and must not be replaced. A JSON-RPC error object only exists because the node
// produced one; a timeout or a reset connection arrives as anything but.
func rejected(err error) bool {
	var rpcErr rpc.Error
	return errors.As(err, &rpcErr)
}

func (a *Adapter) Transaction(ctx context.Context, networkID, hash string) (chain.Transaction, error) {
	_, client, err := a.client(ctx, networkID)
	if err != nil {
		return chain.Transaction{}, err
	}
	if len(hash) != 66 || !strings.HasPrefix(hash, "0x") || len(common.FromHex(hash)) != 32 {
		return chain.Transaction{}, fmt.Errorf("%w: invalid transaction hash", chain.ErrInvalidRequest)
	}
	receipt, err := client.TransactionReceipt(ctx, common.HexToHash(hash))
	if errors.Is(err, ethereum.NotFound) {
		return chain.Transaction{NetworkID: networkID, Hash: hash, Status: "pending"}, nil
	}
	if err != nil {
		return chain.Transaction{}, err
	}
	status := "confirmed"
	if receipt.Status != types.ReceiptStatusSuccessful {
		status = "failed"
	}
	return chain.Transaction{NetworkID: networkID, Hash: hash, Status: status}, nil
}

// parseRecipient accepts the two forms of an EVM address a person can be
// expected to hold, and rejects the one that means something went wrong.
//
// common.IsHexAddress only checks length and hex digits, so it says yes to an
// address whose EIP-55 checksum is broken. That checksum exists precisely to
// catch a mistyped or tampered character, and every wallet and explorer emits
// the mixed-case form — so a mixed-case address that fails it is not a
// stylistic variation, it is a corrupted address, and sending to it burns the
// funds. An address in a single case carries no checksum to verify and is the
// canonical pre-EIP-55 form, so it is taken as given.
func parseRecipient(value string) (common.Address, error) {
	if !common.IsHexAddress(value) {
		return common.Address{}, fmt.Errorf("%w: invalid EVM address", chain.ErrInvalidRequest)
	}
	digits := strings.TrimPrefix(strings.TrimPrefix(value, "0x"), "0X")
	if digits == strings.ToLower(digits) || digits == strings.ToUpper(digits) {
		return common.HexToAddress(value), nil
	}
	// ValidChecksum compares the original spelling against Hex(), which always
	// emits a lower-case "0x". So the prefix has to be normalised first, or a
	// correctly checksummed address written bare or with "0X" — the form several
	// explorers and CSV exports produce — would be reported as corrupted.
	mixed, err := common.NewMixedcaseAddressFromString("0x" + digits)
	if err != nil || !mixed.ValidChecksum() {
		return common.Address{}, fmt.Errorf(
			"%w: the address checksum does not match, so at least one character is wrong; "+
				"paste it again, or enter it in lower case to skip the check",
			chain.ErrInvalidRequest,
		)
	}

	return mixed.Address(), nil
}

func (a *Adapter) transferCall(item network.Network, req chain.TransferRequest) (ethereum.CallMsg, *big.Int, error) {
	if req.Asset.NetworkID != item.ID {
		return ethereum.CallMsg{}, nil, fmt.Errorf("%w: asset belongs to another network", chain.ErrInvalidRequest)
	}
	if !common.IsHexAddress(req.From) {
		return ethereum.CallMsg{}, nil, fmt.Errorf("%w: invalid EVM address", chain.ErrInvalidRequest)
	}
	// The recipient is the one address a person types or pastes, so it is the
	// one worth checking properly rather than for hex-ness alone.
	to, err := parseRecipient(req.To)
	if err != nil {
		return ethereum.CallMsg{}, nil, err
	}
	units, err := decimalUnits(req.Amount, req.Asset.Decimals)
	if err != nil {
		return ethereum.CallMsg{}, nil, err
	}
	from := common.HexToAddress(req.From)
	switch req.Asset.Kind {
	case "native":
		return ethereum.CallMsg{From: from, To: &to, Value: units}, units, nil
	case "erc20":
		if !common.IsHexAddress(req.Asset.Contract) {
			return ethereum.CallMsg{}, nil, fmt.Errorf("%w: invalid token contract", chain.ErrInvalidRequest)
		}
		data, err := a.erc20.Pack("transfer", to, units)
		if err != nil {
			return ethereum.CallMsg{}, nil, err
		}
		contract := common.HexToAddress(req.Asset.Contract)
		return ethereum.CallMsg{From: from, To: &contract, Value: new(big.Int), Data: data}, new(big.Int), nil
	default:
		return ethereum.CallMsg{}, nil, fmt.Errorf("%w: unsupported asset kind", chain.ErrInvalidRequest)
	}
}

func (a *Adapter) client(ctx context.Context, networkID string) (network.Network, *ethclient.Client, error) {
	item, err := a.registry.Get(networkID)
	if err != nil {
		return network.Network{}, nil, err
	}
	if item.Family != network.FamilyEVM {
		return network.Network{}, nil, fmt.Errorf("%w: %s is not EVM", chain.ErrInvalidRequest, networkID)
	}
	for {
		a.mu.Lock()
		cached := a.clients[networkID]
		version := a.version[networkID]
		a.mu.Unlock()
		if cached != nil {
			return item, cached, nil
		}
		endpoints, err := a.endpoints.Endpoints(ctx, item)
		if err != nil {
			return network.Network{}, nil, err
		}
		chainID, _ := new(big.Int).SetString(item.ChainID, 10)
		var failures []error
		invalidated := false
		for _, endpoint := range endpoints {
			var headers http.Header
			if provider, ok := a.endpoints.(HeaderProvider); ok {
				headers, err = provider.Headers(item, endpoint)
				if err != nil {
					return network.Network{}, nil, err
				}
			}
			options := []rpc.ClientOption{rpc.WithHeaders(headers)}
			if provider, ok := a.endpoints.(HTTPClientProvider); ok {
				options = append(options, rpc.WithHTTPClient(provider.HTTPClient(item)))
			}
			rpcClient, err := rpc.DialOptions(ctx, endpoint, options...)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			client := ethclient.NewClient(rpcClient)
			gotChainID, err := client.ChainID(ctx)
			if err != nil || gotChainID.Cmp(chainID) != 0 {
				client.Close()
				if err == nil {
					err = fmt.Errorf("RPC chain id is %s, expected %s", gotChainID, chainID)
				}
				failures = append(failures, err)
				continue
			}
			if _, err := client.BlockNumber(ctx); err != nil {
				client.Close()
				failures = append(failures, fmt.Errorf("read latest block: %w", err))
				continue
			}
			a.mu.Lock()
			if a.version[networkID] != version {
				a.mu.Unlock()
				client.Close()
				invalidated = true
				break
			}
			if existing := a.clients[networkID]; existing != nil {
				a.mu.Unlock()
				client.Close()
				return item, existing, nil
			}
			a.clients[networkID] = client
			if reporter, ok := a.endpoints.(EndpointReporter); ok {
				reporter.MarkHealthy(item, endpoint)
			}
			a.mu.Unlock()
			return item, client, nil
		}
		if invalidated {
			if err := ctx.Err(); err != nil {
				return network.Network{}, nil, err
			}
			continue
		}
		return network.Network{}, nil, fmt.Errorf("no verified RPC for %s: %w", networkID, errors.Join(failures...))
	}
}

func (a *Adapter) nonceLock(networkID string, address common.Address) *sync.Mutex {
	key := networkID + ":" + address.Hex()
	a.mu.Lock()
	defer a.mu.Unlock()
	lock := a.nonces[key]
	if lock == nil {
		lock = new(sync.Mutex)
		a.nonces[key] = lock
	}
	return lock
}

type fees struct {
	model  string
	tipCap *big.Int
	feeCap *big.Int
}

func (f fees) maxFee() *big.Int { return new(big.Int).Set(f.feeCap) }

// maxSuggestedGasPriceWei is the highest per-gas price this wallet will take
// from a node without calling it nonsense: 10,000 gwei.
//
// Both inputs to an EIP-1559 estimate come from the node, so a hostile or
// broken one can name any price it likes and the whole native balance goes to
// the miner as a tip. Real congestion on the supported chains peaks two orders
// of magnitude below this, so the ceiling only ever catches an answer that was
// never plausible.
var maxSuggestedGasPriceWei = new(big.Int).SetUint64(10_000 * params.GWei)

func suggestFees(ctx context.Context, client *ethclient.Client) (fees, error) {
	header, headerErr := client.HeaderByNumber(ctx, nil)
	tip, tipErr := client.SuggestGasTipCap(ctx)
	if headerErr == nil && tipErr == nil && header.BaseFee != nil {
		feeCap := new(big.Int).Mul(header.BaseFee, big.NewInt(2))
		feeCap.Add(feeCap, tip)
		result := fees{model: "eip1559", tipCap: tip, feeCap: feeCap}
		return result, result.sane()
	}
	price, err := client.SuggestGasPrice(ctx)
	if err != nil {
		return fees{}, fmt.Errorf("suggest gas price: %w", err)
	}
	result := fees{model: "legacy", tipCap: new(big.Int), feeCap: price}
	return result, result.sane()
}

func (f fees) sane() error {
	if f.feeCap == nil || f.tipCap == nil || f.feeCap.Sign() < 0 || f.tipCap.Sign() < 0 {
		return fmt.Errorf("%w: RPC returned a negative gas price", chain.ErrInvalidRequest)
	}
	if f.feeCap.Cmp(maxSuggestedGasPriceWei) > 0 || f.tipCap.Cmp(maxSuggestedGasPriceWei) > 0 {
		return fmt.Errorf(
			"%w: RPC suggested %s wei per gas, above the %s wei ceiling",
			chain.ErrInvalidRequest, f.feeCap, maxSuggestedGasPriceWei,
		)
	}

	return nil
}

// approvedFees turns the user's approval into the values that will actually be
// signed, refusing when the network has moved past what they agreed to.
//
// Signing the approved caps is what makes the confirmation meaningful: the
// sender cannot be charged above them whatever the node says next. The staleness
// checks are about the transaction still being useful — a gas limit below what
// the call now needs would run out of gas and burn the fee, and a fee cap under
// the current base fee would simply never be mined.
func approvedFees(approval *chain.FeeApproval, current fees, currentGas uint64) (fees, uint64, error) {
	if approval == nil {
		return fees{}, 0, fmt.Errorf(
			"%w: this transfer was not confirmed against a fee estimate", chain.ErrInvalidRequest,
		)
	}
	if approval.FeeModel != current.model {
		return fees{}, 0, fmt.Errorf(
			"%w: the network switched from %s to %s pricing",
			chain.ErrFeeChanged, approval.FeeModel, current.model,
		)
	}
	feeCap, ok := new(big.Int).SetString(approval.MaxFeePerGas, 10)
	if !ok || feeCap.Sign() < 0 {
		return fees{}, 0, fmt.Errorf("%w: approved max fee per gas is not a number", chain.ErrInvalidRequest)
	}
	tipCap, ok := new(big.Int).SetString(approval.MaxPriorityFeePerGas, 10)
	if !ok || tipCap.Sign() < 0 {
		return fees{}, 0, fmt.Errorf("%w: approved priority fee is not a number", chain.ErrInvalidRequest)
	}
	if approval.GasLimit == 0 {
		return fees{}, 0, fmt.Errorf("%w: approved gas limit is zero", chain.ErrInvalidRequest)
	}
	approved := fees{model: approval.FeeModel, tipCap: tipCap, feeCap: feeCap}
	// An approval is user input on its way to a signature, so it is bounded by
	// the same ceiling as anything a node suggests.
	if err := approved.sane(); err != nil {
		return fees{}, 0, err
	}
	if tipCap.Cmp(feeCap) > 0 {
		return fees{}, 0, fmt.Errorf(
			"%w: approved priority fee is above the approved max fee", chain.ErrInvalidRequest,
		)
	}
	if currentGas > approval.GasLimit {
		return fees{}, 0, fmt.Errorf(
			"%w: this transfer now needs %d gas, %d was approved",
			chain.ErrFeeChanged, currentGas, approval.GasLimit,
		)
	}
	if current.feeCap.Cmp(feeCap) > 0 {
		return fees{}, 0, fmt.Errorf(
			"%w: the network now asks %s wei per gas, %s was approved",
			chain.ErrFeeChanged, current.feeCap, feeCap,
		)
	}

	return approved, approval.GasLimit, nil
}

func decimalUnits(value string, decimals uint8) (*big.Int, error) {
	amount, err := decimal.NewFromString(value)
	if err != nil || amount.Sign() <= 0 {
		return nil, fmt.Errorf("%w: amount must be positive decimal", chain.ErrInvalidRequest)
	}
	scaled := amount.Shift(int32(decimals))
	if !scaled.Equal(scaled.Truncate(0)) {
		return nil, fmt.Errorf("%w: amount has more than %d decimals", chain.ErrInvalidRequest, decimals)
	}
	return scaled.BigInt(), nil
}

//go:fix inline
func ptr[T any](value T) *T { return new(value) }

func publicKey(raw []byte) (*ecdsa.PublicKey, error) {
	key, err := crypto.UnmarshalPubkey(raw)
	if err != nil {
		return nil, errors.New("signer returned an invalid public key")
	}
	return key, nil
}
