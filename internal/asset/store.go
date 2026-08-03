// Package asset stores user-curated token metadata by network.
package asset

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/network"
	"github.com/sxwebdev/walletspace/internal/storage"
)

const schemaVersion = 1

type file struct {
	SchemaVersion int                    `json:"schema_version"`
	Assets        map[string]chain.Asset `json:"assets"`
}

type Store struct {
	home   string
	mu     sync.RWMutex
	custom map[string]chain.Asset
}

func New(home string) (*Store, error) {
	store := &Store{home: home, custom: make(map[string]chain.Asset)}
	data, err := os.ReadFile(filepath.Join(home, "assets.json"))
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read assets: %w", err)
	}
	var current file
	if err := json.Unmarshal(data, &current); err != nil {
		return nil, fmt.Errorf("decode assets: %w", err)
	}
	if current.SchemaVersion != schemaVersion {
		return nil, fmt.Errorf("unsupported assets schema version %d", current.SchemaVersion)
	}
	for id, token := range current.Assets {
		if strings.TrimSpace(token.Name) == "" {
			token.Name = token.Symbol
			current.Assets[id] = token
		}
	}
	store.custom = current.Assets
	return store, nil
}

func (s *Store) List(item network.Network) []chain.Asset {
	byID := make(map[string]chain.Asset)
	for _, token := range defaults(item) {
		byID[token.ID] = token
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, token := range s.custom {
		if token.NetworkID == item.ID {
			byID[token.ID] = token
		}
	}
	out := make([]chain.Asset, 0, len(byID))
	for _, token := range byID {
		out = append(out, token)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			if out[i].Kind == "native" {
				return true
			}
			if out[j].Kind == "native" {
				return false
			}
		}
		if out[i].Name == out[j].Name {
			return out[i].Symbol < out[j].Symbol
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (s *Store) Add(token chain.Asset) error {
	if token.ID == "" || token.NetworkID == "" || token.Contract == "" || token.Symbol == "" {
		return errors.New("token metadata is incomplete")
	}
	if token.Kind != "erc20" && token.Kind != "trc20" {
		return errors.New("only ERC20 and TRC20 assets can be configured")
	}
	if strings.TrimSpace(token.Name) == "" {
		token.Name = token.Symbol
	}
	token.Configured = true
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(s.custom)
	next[token.ID] = token
	if err := s.saveLocked(next); err != nil {
		return err
	}
	s.custom = next
	return nil
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.custom[id]; !ok {
		return errors.New("configured asset not found")
	}
	next := clone(s.custom)
	delete(next, id)
	if err := s.saveLocked(next); err != nil {
		return err
	}
	s.custom = next
	return nil
}

func (s *Store) saveLocked(assets map[string]chain.Asset) error {
	data, err := json.MarshalIndent(file{SchemaVersion: schemaVersion, Assets: assets}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode assets: %w", err)
	}
	return storage.AtomicWrite(filepath.Join(s.home, "assets.json"), append(data, '\n'))
}

func defaults(item network.Network) []chain.Asset {
	out := []chain.Asset{{
		ID: item.ID + ":native", NetworkID: item.ID, Kind: "native",
		Name: nativeAssetName(item.Native.Symbol), Symbol: item.Native.Symbol,
		Decimals: item.Native.Decimals,
	}}
	for _, token := range builtinTokens[item.ID] {
		out = append(out, chain.Asset{
			ID: ID(item.ID, token.kind, token.contract), NetworkID: item.ID, Kind: token.kind,
			Name: token.name, Symbol: token.symbol, Decimals: token.decimals,
			Contract: token.contract,
		})
	}
	return out
}

type builtinToken struct {
	kind     string
	name     string
	symbol   string
	contract string
	decimals uint8
}

func erc20(name, symbol, contract string, decimals uint8) builtinToken {
	return builtinToken{kind: "erc20", name: name, symbol: symbol, contract: contract, decimals: decimals}
}

func trc20(name, symbol, contract string, decimals uint8) builtinToken {
	return builtinToken{kind: "trc20", name: name, symbol: symbol, contract: contract, decimals: decimals}
}

var builtinTokens = map[string][]builtinToken{
	"tron-mainnet": {
		trc20("Tether USD", "USDT", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", 6),
	},
	"tron-nile": {
		trc20("Tether USD", "USDT", "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf", 6),
	},
	"bsc-mainnet": {
		erc20("Tether USD", "USDT", "0x55d398326f99059ff775485246999027b3197955", 18),
		erc20("USD Coin", "USDC", "0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d", 18),
		erc20("Binance Bitcoin", "BTCB", "0x7130d2a12b9bcbfae4f2634d864a1ee1ce3ead9c", 18),
		erc20("Wormhole Wrapped Ether", "WETH", "0x4db5a66e937a9f4473fa95b1caf1d1e1d62e29ea", 18),
		erc20("Binance-Peg XRP", "XRP", "0x1d2f0da169ceb9fc7b3144628db156f3f6c60dbe", 18),
		erc20("Binance-Peg Dogecoin", "DOGE", "0xba2ae424d960c26247dd6c32edc70b295c744c43", 8),
		erc20("Binance-Peg POL", "POL", "0xcc42724c6683b7e57334c4e856f4c9965ed682bd", 18),
		erc20("Binance-Peg Solana", "SOL", "0x570a5d26f7765ecb712c0924e4de545b89fd43df", 18),
		erc20("Binance-Peg Chainlink", "LINK", "0xf8a0bf9cf54bb92f17374d9e9a321e6a111a51bd", 18),
		erc20("Binance-Peg Shiba Inu", "SHIB", "0x2859e4544c4bb03966803b044a93563bd2d0dd4d", 18),
		erc20("Binance-Peg Uniswap", "UNI", "0xbf5140a22578168fd562dccf235e5d43a02ce9b1", 18),
		erc20("Binance-Peg Dai", "DAI", "0x1af3f329e8be154074d8769d1ffa4ee058b1dbc3", 18),
		erc20("Binance-Peg Ethereum", "ETH", "0x2170ed0880ac9a755fd29b2688956bd959f933f8", 18),
	},
	"ethereum-mainnet": {
		erc20("Tether USD", "USDT", "0xdac17f958d2ee523a2206206994597c13d831ec7", 6),
		erc20("USD Coin", "USDC", "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", 6),
		erc20("Wrapped Bitcoin", "WBTC", "0x2260fac5e5542a773aa44fbcfedf7c193bc2c599", 8),
		erc20("Wrapped Ether", "WETH", "0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2", 18),
		erc20("Wrapped XRP", "WXRP", "0x39fbbabf11738317a448031930706cd3e612e1b9", 18),
		erc20("POL", "POL", "0x455e53cbb86018ac2b8092fdcd39d8444affc3f6", 18),
		erc20("Chainlink", "LINK", "0x514910771af9ca656af840dff83e8264ecf986ca", 18),
		erc20("Shiba Inu", "SHIB", "0x95ad61b0a150d79219dcf64e1e6cc01f0b64c4ce", 18),
		erc20("Uniswap", "UNI", "0x1f9840a85d5af5bf1d1762f925bdaddc4201f984", 18),
		erc20("Dai", "DAI", "0x6b175474e89094c44da98b954eedeac495271d0f", 18),
	},
	"polygon-mainnet": {
		erc20("Tether USD", "USDT", "0xc2132d05d31c914a87c6611c10748aeb04b58e8f", 6),
		erc20("USD Coin", "USDC", "0x3c499c542cef5e3811e1192ce70d8cc03d5c3359", 6),
		erc20("BNB", "BNB", "0x3ba4c387f786bfee076a58914f5bd38d668b42c3", 18),
		erc20("Wrapped BNB", "WBNB", "0xecdcb5b88f8e3c15f95c720c51c71c9e2080525d", 18),
		erc20("Wrapped Bitcoin", "WBTC", "0x1bfd67037b42cf73acf2047067bd4f2c47d9bfd6", 8),
		erc20("Wrapped Ether", "WETH", "0x7ceb23fd6bc0add59e62ac25578270cff1b9f619", 18),
		erc20("Wormhole Wrapped Solana", "SOL", "0xd93f7e271cb87c23aaa73edc008a79646d1f9912", 9),
	},
	"optimism-mainnet": {
		erc20("Tether USD", "USDT", "0x94b008aa00579c1307b0ef2c499ad98a8ce58e58", 6),
		erc20("USD Coin", "USDC", "0x0b2c639c533813f4aa9d7837caf62653d097ff85", 6),
		erc20("Wrapped Bitcoin", "WBTC", "0x68f180fcce6836688e9084f035309e29bf0a2095", 8),
	},
	"base-mainnet": {
		erc20("USD Coin", "USDC", "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913", 6),
		erc20("Coinbase Wrapped Bitcoin", "cbBTC", "0x0555e30da8f98308edb960aa94c0db47230d2b9c", 8),
		erc20("Wrapped Ether", "WETH", "0x4200000000000000000000000000000000000006", 18),
		erc20("Bridged Tether USD", "USDT", "0xfde4c96c8593536e31f229ea8f37b2ada2699bb2", 6),
	},
	"arbitrum-mainnet": {
		erc20("USD Coin", "USDC", "0xaf88d065e77c8cc2239327c5edb3a432268e5831", 6),
		erc20("Wrapped Bitcoin", "WBTC", "0x2f2a2543b76a4166549f7aab2e75bef0aefc5b0f", 8),
		erc20("Wrapped Ether", "WETH", "0x82af49447d8a07e3bd95bd0d56f35241523fbab1", 18),
		erc20("Tether USD", "USDT", "0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9", 6),
		erc20("Arbitrum", "ARB", "0x912ce59144191c1204e64559fe8253a0e49e6548", 18),
	},
	"avalanche-mainnet": {
		erc20("USD Coin", "USDC", "0xB97EF9Ef8734C71904D8002F8b6Bc66Dd9c48a6E", 6),
		erc20("Tether USD", "USDT", "0x9702230A8Ea53601f5cD2dc00fDBc13d4dF4A8c7", 6),
	},
	"robinhood-mainnet": {
		erc20("Robinhood Wrapped ETH", "WETH", "0x0Bd7D308f8E1639FAb988df18A8011f41EAcAD73", 18),
	},
}

func nativeAssetName(symbol string) string {
	switch symbol {
	case "ETH":
		return "Ether"
	case "BNB", "tBNB":
		return "BNB"
	case "POL":
		return "POL"
	case "AVAX":
		return "Avalanche"
	case "TRX":
		return "TRON"
	default:
		return symbol
	}
}

func ID(networkID, kind, contract string) string {
	if kind == "erc20" {
		contract = strings.ToLower(contract)
	}
	return networkID + ":" + kind + ":" + contract
}

func clone(source map[string]chain.Asset) map[string]chain.Asset {
	out := make(map[string]chain.Asset, len(source))
	for id, item := range source {
		out[id] = item
	}
	return out
}
