package evm_test

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/big"
	"sync/atomic"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/sxwebdev/walletspace/internal/chain"
)

func TestFundingEstimateSimulatesTokenWithoutChangingTokenState(t *testing.T) {
	t.Parallel()
	var simulations atomic.Int64
	server := rpcServer(t, func(request rpcRequest) (any, error) {
		switch request.Method {
		case "eth_chainId":
			return "0x2019", nil
		case "eth_blockNumber":
			return "0x100", nil
		case "eth_getBlockByNumber", "eth_maxPriorityFeePerGas":
			return nil, fmt.Errorf("legacy node")
		case "eth_gasPrice":
			return "0x5d21dba00", nil // 25 gwei
		case "eth_getBalance":
			return "0x0", nil
		case "eth_estimateGas":
			var args []jsontext.Value
			if err := json.Unmarshal(request.Params, &args); err != nil {
				return nil, err
			}
			if len(args) == 1 {
				return nil, fmt.Errorf("insufficient funds")
			}
			simulations.Add(1)
			if len(args) != 3 {
				t.Errorf("simulation arguments = %s", request.Params)
			}
			var overrides map[string]map[string]string
			if err := json.Unmarshal(args[2], &overrides); err != nil {
				return nil, err
			}
			if len(overrides) != 1 {
				t.Errorf("simulation changed %d accounts", len(overrides))
			}
			for _, state := range overrides {
				if len(state) != 1 || state["balance"] == "" {
					t.Errorf("simulation changed token storage: %v", state)
				}
			}
			return "0xea60", nil // 60,000 gas
		default:
			return nil, fmt.Errorf("unexpected method %s", request.Method)
		}
	})
	adapter := newAdapter(t, server.URL)
	estimate, err := adapter.EstimateFunding(t.Context(), "kaia-mainnet", chain.TransferRequest{
		From: "0x1111111111111111111111111111111111111111", To: "0x2222222222222222222222222222222222222222", Amount: "10",
		Asset: chain.Asset{NetworkID: "kaia-mainnet", Kind: "erc20", Decimals: 6, Contract: "0x3333333333333333333333333333333333333333"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if estimate.NativeBalance != "0" || estimate.FeeShortfall != "0.0015" || estimate.Fee != "0.0015" || estimate.GasLimit != 60000 || simulations.Load() != 1 {
		t.Fatalf("estimate = %+v; simulations %d", estimate, simulations.Load())
	}
}

func TestRefuelRecordsBeforeBroadcastAndPreservesDepositData(t *testing.T) {
	t.Parallel()
	var recorded atomic.Bool
	var broadcasts atomic.Int64
	server := rpcServer(t, func(request rpcRequest) (any, error) {
		switch request.Method {
		case "eth_chainId":
			return "0x1", nil
		case "eth_blockNumber":
			return "0x100", nil
		case "eth_estimateGas":
			return "0x5208", nil
		case "eth_getTransactionCount":
			return "0x1", nil
		case "eth_getBlockByNumber", "eth_maxPriorityFeePerGas":
			return nil, fmt.Errorf("legacy node")
		case "eth_gasPrice":
			return "0x3b9aca00", nil
		case "eth_sendRawTransaction":
			var params []string
			if err := json.Unmarshal(request.Params, &params); err != nil {
				return nil, err
			}
			tx := new(types.Transaction)
			if err := tx.UnmarshalBinary(common.FromHex(params[0])); err != nil {
				return nil, err
			}
			if !bytes.Equal(tx.Data(), []byte{1, 0, 33}) || tx.To() == nil || *tx.To() != common.HexToAddress("0x391E7C679d29bD940d63be94AD22A25d25b5A604") || tx.Value().Cmp(big.NewInt(10000000000000)) != 0 {
				t.Errorf("signed deposit = %v", tx)
			}
			if !recorded.Load() {
				t.Error("broadcast before durable record")
			}
			broadcasts.Add(1)
			return "0xignored", nil
		default:
			return nil, fmt.Errorf("unexpected method %s", request.Method)
		}
	})
	adapter := newAdapter(t, server.URL)
	req := chain.TransferRequest{From: "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf", To: "0x391E7C679d29bD940d63be94AD22A25d25b5A604", Amount: "0.00001", Data: []byte{1, 0, 33}, Asset: chain.Asset{NetworkID: "ethereum-mainnet", Kind: "native", Decimals: 18}, Approved: legacyApproval("1000000000")}
	stop := errors.New("storage failed")
	req.BeforeBroadcast = func(string) error { return stop }
	if _, err := adapter.Send(t.Context(), "ethereum-mainnet", req, testSigner{keyHex: fmt.Sprintf("%064x", big.NewInt(1))}); !errors.Is(err, stop) {
		t.Fatalf("record failure = %v", err)
	}
	if broadcasts.Load() != 0 {
		t.Fatal("transaction broadcast after storage failure")
	}
	req.BeforeBroadcast = func(hash string) error {
		if len(hash) != 66 {
			t.Errorf("hash = %s", hash)
		}
		recorded.Store(true)
		return nil
	}
	if _, err := adapter.Send(t.Context(), "ethereum-mainnet", req, testSigner{keyHex: fmt.Sprintf("%064x", big.NewInt(1))}); err != nil {
		t.Fatal(err)
	}
	if broadcasts.Load() != 1 {
		t.Fatalf("broadcasts = %d", broadcasts.Load())
	}
}

func TestFundingIncludesRollupDataAndOperatorFees(t *testing.T) {
	t.Parallel()
	server := rpcServer(t, func(request rpcRequest) (any, error) {
		switch request.Method {
		case "eth_chainId":
			return "0x2105", nil
		case "eth_blockNumber":
			return "0x100", nil
		case "eth_estimateGas":
			return "0x5208", nil
		case "eth_getBlockByNumber", "eth_maxPriorityFeePerGas":
			return nil, fmt.Errorf("legacy node")
		case "eth_gasPrice":
			return "0x3b9aca00", nil
		case "eth_getBalance":
			return "0x0", nil
		case "eth_call":
			return "0x" + fmt.Sprintf("%064x", 1000000000000), nil
		default:
			return nil, fmt.Errorf("unexpected method %s", request.Method)
		}
	})
	adapter := newAdapter(t, server.URL)
	got, err := adapter.EstimateFunding(t.Context(), "base-mainnet", chain.TransferRequest{
		From: "0x1111111111111111111111111111111111111111", To: "0x2222222222222222222222222222222222222222", Amount: "1",
		Asset: chain.Asset{NetworkID: "base-mainnet", Kind: "erc20", Contract: "0x3333333333333333333333333333333333333333", Decimals: 6},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Fee != "0.000023" || got.AdditionalFee != "0.000002" || got.FeeShortfall != "0.000023" {
		t.Fatalf("rollup budget = %+v", got)
	}
}

func TestNativeDeliveryRequiresRealPaymentToTheTokenOwner(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, to        string
		value, chainID  int64
		pending, failed bool
		want            bool
		wantErr         error
	}{
		{name: "confirmed", to: "0x2222222222222222222222222222222222222222", value: 2000, chainID: 8217, want: true},
		{name: "different recipient", to: "0x3333333333333333333333333333333333333333", value: 2000, chainID: 8217, wantErr: chain.ErrInvalidRequest},
		{name: "underpaid", to: "0x2222222222222222222222222222222222222222", value: 999, chainID: 8217, wantErr: chain.ErrInvalidRequest},
		{name: "wrong network", to: "0x2222222222222222222222222222222222222222", value: 2000, chainID: 1, wantErr: chain.ErrInvalidRequest},
		{name: "pending", to: "0x2222222222222222222222222222222222222222", value: 2000, chainID: 8217, pending: true},
		{name: "failed", to: "0x2222222222222222222222222222222222222222", value: 2000, chainID: 8217, failed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			key, err := crypto.HexToECDSA(fmt.Sprintf("%064x", 1))
			if err != nil {
				t.Fatal(err)
			}
			tx, err := types.SignTx(types.NewTx(&types.LegacyTx{Nonce: 1, To: new(common.HexToAddress(tt.to)), Value: big.NewInt(tt.value), Gas: 21000, GasPrice: big.NewInt(1)}), types.LatestSignerForChainID(big.NewInt(tt.chainID)), key)
			if err != nil {
				t.Fatal(err)
			}
			receiptStatus := uint64(1)
			if tt.failed {
				receiptStatus = 0
			}
			server := rpcServer(t, func(request rpcRequest) (any, error) {
				switch request.Method {
				case "eth_chainId":
					return "0x2019", nil
				case "eth_blockNumber":
					return "0x100", nil
				case "eth_getTransactionReceipt":
					if tt.pending {
						return nil, nil
					}
					return &types.Receipt{Status: receiptStatus, TxHash: tx.Hash(), CumulativeGasUsed: 21000, GasUsed: 21000, Logs: []*types.Log{}, BlockHash: common.HexToHash("0x1234"), BlockNumber: big.NewInt(1)}, nil
				case "eth_getTransactionByHash":
					data, err := tx.MarshalJSON()
					if err != nil {
						return nil, err
					}
					var result map[string]any
					if err := json.Unmarshal(data, &result); err != nil {
						return nil, err
					}
					result["blockNumber"] = "0x1"
					return result, nil
				default:
					return nil, fmt.Errorf("unexpected method %s", request.Method)
				}
			})
			adapter := newAdapter(t, server.URL)
			delivered, err := adapter.NativeDelivery(t.Context(), "kaia-mainnet", tx.Hash().Hex(), "0x2222222222222222222222222222222222222222", big.NewInt(1000))
			if delivered != tt.want || !errors.Is(err, tt.wantErr) {
				t.Fatalf("delivery = %t, %v; want %t, %v", delivered, err, tt.want, tt.wantErr)
			}
		})
	}
}
