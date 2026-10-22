// Package network defines the immutable built-in network registry.
package network

import (
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"sort"
	"strings"
)

type Family string

const (
	FamilyTron Family = "tron"
	FamilyEVM  Family = "evm"
)

type NativeAsset struct {
	Symbol   string `json:"symbol" yaml:"symbol"`
	Decimals uint8  `json:"decimals" yaml:"decimals"`
}

type Explorer struct {
	Address string `json:"address" yaml:"address"`
	Tx      string `json:"tx" yaml:"tx"`
	Block   string `json:"block" yaml:"block"`
}

type Capabilities struct {
	NativeTransfer bool `json:"native_transfer"`
	TokenTransfer  bool `json:"token_transfer"`
	TronResources  bool `json:"tron_resources"`
	TronDeploy     bool `json:"tron_deploy"`
}

// WrappedNative describes a WETH9-compatible wrapper for this network’s native coin.
// Contracts are explicit: a bridged token with the same symbol is not a wrapper.
type WrappedNative struct {
	Symbol   string `json:"symbol"`
	Contract string `json:"contract"`
}

type Network struct {
	ID            string         `json:"id" yaml:"id"`
	WrappedNative *WrappedNative `json:"wrapped_native,omitempty" yaml:"-"`
	Name          string         `json:"name" yaml:"name"`
	ShortName     string         `json:"short_name" yaml:"short_name"`
	Family        Family         `json:"family" yaml:"family"`
	ChainID       string         `json:"chain_id" yaml:"chain_id"`
	Native        NativeAsset    `json:"native" yaml:"native"`
	Testnet       bool           `json:"testnet" yaml:"testnet"`
	Enabled       bool           `json:"enabled" yaml:"enabled"`
	Explorer      Explorer       `json:"explorer" yaml:"explorer"`
	RPCFallbacks  []string       `json:"rpc_fallbacks" yaml:"rpc_fallbacks"`
	Capabilities  Capabilities   `json:"capabilities" yaml:"capabilities"`
	PriceChain    string         `json:"-" yaml:"-"`
	NativePrice   string         `json:"-" yaml:"-"`
}

var ErrUnknownNetwork = errors.New("unknown network")

type Registry struct {
	byID map[string]Network
}

func Builtin() (*Registry, error) {
	networks := builtinNetworks()
	r := &Registry{byID: make(map[string]Network, len(networks))}
	for _, item := range networks {
		if err := Validate(item); err != nil {
			return nil, fmt.Errorf("built-in network %s: %w", item.ID, err)
		}
		if _, exists := r.byID[item.ID]; exists {
			return nil, fmt.Errorf("duplicate built-in network %q", item.ID)
		}
		r.byID[item.ID] = item
	}
	return r, nil
}

func (r *Registry) Get(id string) (Network, error) {
	item, ok := r.byID[id]
	if !ok {
		return Network{}, fmt.Errorf("%w: %s", ErrUnknownNetwork, id)
	}
	return clone(item), nil
}

func (r *Registry) List() []Network {
	out := make([]Network, 0, len(r.byID))
	for _, item := range r.byID {
		out = append(out, clone(item))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func Validate(item Network) error {
	if item.ID == "" || item.Name == "" || item.ShortName == "" {
		return errors.New("id, name and short name are required")
	}
	if item.Family != FamilyTron && item.Family != FamilyEVM {
		return fmt.Errorf("unsupported family %q", item.Family)
	}
	if item.Native.Symbol == "" || item.Native.Decimals == 0 {
		return errors.New("native asset is incomplete")
	}
	if item.Family == FamilyEVM {
		n, ok := new(big.Int).SetString(item.ChainID, 10)
		if !ok || n.Sign() <= 0 {
			return fmt.Errorf("invalid EVM chain id %q", item.ChainID)
		}
	} else if item.ChainID != "mainnet" && item.ChainID != "nile" {
		return fmt.Errorf("invalid Tron network identity %q", item.ChainID)
	}
	if !item.Testnet && item.NativePrice == "" {
		return errors.New("mainnet native price identifier is required")
	}
	for _, endpoint := range item.RPCFallbacks {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("fallback RPC must be an HTTPS URL: %q", endpoint)
		}
	}
	return nil
}

func clone(item Network) Network {
	if item.WrappedNative != nil {
		item.WrappedNative = new(*item.WrappedNative)
	}
	item.RPCFallbacks = append([]string(nil), item.RPCFallbacks...)
	return item
}

func explorer(base string) Explorer {
	base = strings.TrimSuffix(base, "/")
	return Explorer{
		Address: base + "/address/{address}",
		Tx:      base + "/tx/{tx}",
		Block:   base + "/block/{block}",
	}
}

func evm(id, name, shortName, chainID, symbol, rpc, explorerBase string, testnet bool) Network {
	return Network{
		ID: id, Name: name, ShortName: shortName, Family: FamilyEVM, ChainID: chainID,
		Native: NativeAsset{Symbol: symbol, Decimals: 18}, Testnet: testnet, Enabled: true,
		Explorer: explorer(explorerBase), RPCFallbacks: []string{rpc},
		Capabilities: Capabilities{NativeTransfer: true, TokenTransfer: true},
		PriceChain:   priceChains[id], NativePrice: nativePrices[id],
		WrappedNative: wrappedNatives[id],
	}
}

func builtinNetworks() []Network {
	tronCapabilities := Capabilities{
		NativeTransfer: true, TokenTransfer: true, TronResources: true, TronDeploy: true,
	}
	return []Network{
		{
			ID: "tron-mainnet", Name: "Tron Mainnet", ShortName: "Mainnet",
			Family: FamilyTron, ChainID: "mainnet", Native: NativeAsset{Symbol: "TRX", Decimals: 6},
			Enabled: true, Explorer: explorer("https://tronscan.org"),
			RPCFallbacks: []string{"https://tron-rpc.publicnode.com"}, Capabilities: tronCapabilities,
			PriceChain: "tron", NativePrice: "coingecko:tron",
		},
		{
			ID: "tron-nile", Name: "Tron Nile", ShortName: "Nile",
			Family: FamilyTron, ChainID: "nile", Native: NativeAsset{Symbol: "TRX", Decimals: 6},
			Testnet: true, Enabled: true, Explorer: explorer("https://nile.tronscan.org"),
			RPCFallbacks: []string{"https://nile.trongrid.io"}, Capabilities: tronCapabilities,
		},
		evm("ethereum-mainnet", "Ethereum Mainnet", "Mainnet", "1", "ETH", "https://ethereum-rpc.publicnode.com", "https://etherscan.io", false),
		evm("ethereum-sepolia", "Ethereum Sepolia", "Sepolia", "11155111", "ETH", "https://ethereum-sepolia-rpc.publicnode.com", "https://sepolia.etherscan.io", true),
		evm("bsc-mainnet", "BNB Smart Chain", "Mainnet", "56", "BNB", "https://bsc-rpc.publicnode.com", "https://bscscan.com", false),
		evm("bsc-testnet", "BNB Smart Chain Testnet", "Testnet", "97", "tBNB", "https://bsc-testnet-rpc.publicnode.com", "https://testnet.bscscan.com", true),
		evm("polygon-mainnet", "Polygon PoS", "Mainnet", "137", "POL", "https://polygon-bor-rpc.publicnode.com", "https://polygonscan.com", false),
		evm("polygon-amoy", "Polygon Amoy", "Amoy", "80002", "POL", "https://polygon-amoy-bor-rpc.publicnode.com", "https://amoy.polygonscan.com", true),
		evm("optimism-mainnet", "OP Mainnet", "Mainnet", "10", "ETH", "https://optimism-rpc.publicnode.com", "https://optimistic.etherscan.io", false),
		evm("optimism-sepolia", "OP Sepolia", "Sepolia", "11155420", "ETH", "https://optimism-sepolia-rpc.publicnode.com", "https://sepolia-optimism.etherscan.io", true),
		evm("arbitrum-mainnet", "Arbitrum One", "Mainnet", "42161", "ETH", "https://arbitrum-one-rpc.publicnode.com", "https://arbiscan.io", false),
		evm("arbitrum-sepolia", "Arbitrum Sepolia", "Sepolia", "421614", "ETH", "https://arbitrum-sepolia-rpc.publicnode.com", "https://sepolia.arbiscan.io", true),
		evm("base-mainnet", "Base", "Mainnet", "8453", "ETH", "https://mainnet.base.org", "https://basescan.org", false),
		evm("kaia-mainnet", "Kaia Mainnet", "Mainnet", "8217", "KAIA", "https://public-en.node.kaia.io", "https://kaiascan.io", false),
		evm("robinhood-mainnet", "Robinhood Chain", "Mainnet", "4663", "ETH", "https://rpc.mainnet.chain.robinhood.com", "https://explorer.chain.robinhood.com", false),
		evm("robinhood-testnet", "Robinhood Chain Testnet", "Testnet", "46630", "ETH", "https://rpc.testnet.chain.robinhood.com", "https://explorer.testnet.chain.robinhood.com", true),
		evm("avalanche-mainnet", "Avalanche C-Chain", "Mainnet", "43114", "AVAX", "https://api.avax.network/ext/bc/C/rpc", "https://snowtrace.io", false),
		evm("avalanche-fuji", "Avalanche Fuji C-Chain", "Fuji", "43113", "AVAX", "https://api.avax-test.network/ext/bc/C/rpc", "https://testnet.snowtrace.io", true),
	}
}

var priceChains = map[string]string{
	"ethereum-mainnet":  "ethereum",
	"bsc-mainnet":       "bsc",
	"polygon-mainnet":   "polygon",
	"optimism-mainnet":  "optimism",
	"arbitrum-mainnet":  "arbitrum",
	"base-mainnet":      "base",
	"kaia-mainnet":      "klaytn",
	"avalanche-mainnet": "avax",
}

var nativePrices = map[string]string{
	"ethereum-mainnet":  "coingecko:ethereum",
	"bsc-mainnet":       "coingecko:binancecoin",
	"polygon-mainnet":   "coingecko:polygon-ecosystem-token",
	"optimism-mainnet":  "coingecko:ethereum",
	"arbitrum-mainnet":  "coingecko:ethereum",
	"base-mainnet":      "coingecko:ethereum",
	"kaia-mainnet":      "coingecko:kaia",
	"robinhood-mainnet": "coingecko:ethereum",
	"avalanche-mainnet": "coingecko:avalanche-2",
}

// Only native-backed wrappers using deposit()/withdraw(uint256) belong here.
var wrappedNatives = map[string]*WrappedNative{
	"ethereum-mainnet": {Symbol: "WETH", Contract: "0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2"},
	"bsc-mainnet":      {Symbol: "WBNB", Contract: "0xbb4cdb9cbd36b01bd1cbaebf2de08d9173bc095c"},
	"bsc-testnet":      {Symbol: "WBNB", Contract: "0xae13d989dac2f0debff460ac112a837c89baa7cd"},
	"base-mainnet":     {Symbol: "WETH", Contract: "0x4200000000000000000000000000000000000006"},
	"optimism-mainnet": {Symbol: "WETH", Contract: "0x4200000000000000000000000000000000000006"},
	"arbitrum-mainnet": {Symbol: "WETH", Contract: "0x82af49447d8a07e3bd95bd0d56f35241523fbab1"},
}
