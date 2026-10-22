package evm_test

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/network"
)

// Inspect the signed wire transaction, not just the estimate: withdraw must
// encode the amount in calldata and must never send native value to the token.
func TestNativeConversionSignedTransaction(t *testing.T) {
	t.Parallel()
	for _, networkID := range []string{"ethereum-mainnet", "bsc-mainnet", "bsc-testnet", "base-mainnet", "optimism-mainnet", "arbitrum-mainnet"} {
		for _, action := range []string{"wrap", "unwrap"} {
			t.Run(networkID+"/"+action, func(t *testing.T) {
				t.Parallel()
				registry, err := network.Builtin()
				if err != nil {
					t.Fatal(err)
				}
				item, err := registry.Get(networkID)
				if err != nil {
					t.Fatal(err)
				}
				var raw string
				var mu sync.Mutex
				server := rpcServer(t, func(request rpcRequest) (any, error) {
					switch request.Method {
					case "eth_chainId":
						id, _ := new(big.Int).SetString(item.ChainID, 10)
						return hexutil.EncodeBig(id), nil
					case "eth_blockNumber":
						return "0x100", nil
					case "eth_getBalance":
						return "0xde0b6b3a7640000", nil // 1 native coin
					case "eth_call":
						return "0x" + fmt.Sprintf("%064x", big.NewInt(2000000000000000000)), nil // 2 wrapped coins
					case "eth_estimateGas":
						return "0x5208", nil
					case "eth_getTransactionCount":
						return "0x7", nil
					case "eth_getBlockByNumber", "eth_maxPriorityFeePerGas":
						return nil, fmt.Errorf("legacy node")
					case "eth_gasPrice":
						return "0x3b9aca00", nil
					case "eth_sendRawTransaction":
						var params []string
						if err := json.Unmarshal(request.Params, &params); err != nil {
							return nil, err
						}
						mu.Lock()
						raw = params[0]
						mu.Unlock()
						return "0xignored", nil
					default:
						return nil, fmt.Errorf("unexpected method %s", request.Method)
					}
				})
				adapter := newAdapter(t, server.URL)
				from := "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf"
				asset := chain.Asset{NetworkID: networkID, Kind: "native", Decimals: 18}
				if action == "unwrap" {
					asset.Kind = "erc20"
					asset.Contract = strings.ToUpper(item.WrappedNative.Contract)
				}
				req := chain.TransferRequest{Action: action, From: from, To: from, Asset: asset, Amount: "0.125"}
				estimate, err := adapter.EstimateTransfer(t.Context(), networkID, req)
				if err != nil {
					t.Fatal(err)
				}
				req.Approved = &chain.FeeApproval{FeeModel: estimate.FeeModel, GasLimit: estimate.GasLimit, MaxFeePerGas: estimate.MaxFeePerGas, MaxPriorityFeePerGas: estimate.MaxPriorityFeePerGas}
				result, err := adapter.Send(t.Context(), networkID, req, testSigner{keyHex: fmt.Sprintf("%064x", big.NewInt(1))})
				if err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				wire := raw
				mu.Unlock()
				signed := new(types.Transaction)
				if err := signed.UnmarshalBinary(common.FromHex(wire)); err != nil {
					t.Fatal(err)
				}
				if signed.To() == nil || *signed.To() != common.HexToAddress(item.WrappedNative.Contract) {
					t.Fatalf("destination = %v", signed.To())
				}
				units := big.NewInt(125000000000000000)
				wantValue := new(big.Int)
				wantData := common.FromHex("0xd0e30db0") // deposit()
				if action == "wrap" {
					wantValue.Set(units)
				} else {
					wantData = append(common.FromHex("0x2e1a7d4d"), common.LeftPadBytes(units.Bytes(), 32)...)
				}
				if signed.Value().Cmp(wantValue) != 0 || !bytes.Equal(signed.Data(), wantData) {
					t.Fatalf("signed value/data = %s/%x, want %s/%x", signed.Value(), signed.Data(), wantValue, wantData)
				}
				id, _ := new(big.Int).SetString(item.ChainID, 10)
				sender, err := types.Sender(types.LatestSignerForChainID(id), signed)
				if err != nil || sender != common.HexToAddress(from) || signed.ChainId().Cmp(id) != 0 || result.Hash != signed.Hash().Hex() {
					t.Fatalf("invalid signature, chain or local hash: sender %s, err %v", sender, err)
				}

				maxEstimate, err := adapter.EstimateMaxTransfer(t.Context(), networkID, req)
				if err != nil {
					t.Fatal(err)
				}
				wantMax := "2"
				if action == "wrap" {
					wantMax = "0.999979"
				}
				if maxEstimate.Amount != wantMax || maxEstimate.Fee != "0.000021" {
					t.Fatalf("MAX = %+v, want amount %s with native gas reserved", maxEstimate, wantMax)
				}
			})
		}
	}
}

func TestNativeConversionRejectsUnrelatedAssets(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, action, kind, contract, to, amount string
		decimals                                 uint8
	}{
		{name: "bridged WETH", action: "unwrap", kind: "erc20", contract: "0x4db5a66e937a9f4473fa95b1caf1d1e1d62e29ea", decimals: 18},
		{name: "native unwrap", action: "unwrap", kind: "native", decimals: 18},
		{name: "token wrap", action: "wrap", kind: "erc20", contract: "0xbb4cdb9cbd36b01bd1cbaebf2de08d9173bc095c", decimals: 18},
		{name: "other recipient", action: "wrap", kind: "native", to: "0x0000000000000000000000000000000000000001", decimals: 18},
		{name: "wrong decimals", action: "wrap", kind: "native", decimals: 6},
		{name: "unknown action", action: "swap", kind: "native", decimals: 18},
		{name: "zero amount", action: "wrap", kind: "native", decimals: 18, amount: "0"},
		{name: "negative amount", action: "wrap", kind: "native", decimals: 18, amount: "-1"},
		{name: "excess precision", action: "wrap", kind: "native", decimals: 18, amount: "0.0000000000000000001"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := rpcServer(t, func(request rpcRequest) (any, error) {
				switch request.Method {
				case "eth_chainId":
					return "0x38", nil
				case "eth_blockNumber":
					return "0x100", nil
				default:
					t.Errorf("invalid conversion reached %s", request.Method)
					return nil, fmt.Errorf("unexpected RPC")
				}
			})
			adapter := newAdapter(t, server.URL)
			from := "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf"
			to := tt.to
			if to == "" {
				to = from
			}
			amount := tt.amount
			if amount == "" {
				amount = "1"
			}
			_, err := adapter.EstimateTransfer(t.Context(), "bsc-mainnet", chain.TransferRequest{
				Action: tt.action, From: from, To: to, Amount: amount,
				Asset: chain.Asset{NetworkID: "bsc-mainnet", Kind: tt.kind, Contract: tt.contract, Decimals: tt.decimals},
			})
			if !errors.Is(err, chain.ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}
