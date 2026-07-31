package evm_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/chain/evm"
	"github.com/sxwebdev/walletspace/internal/network"
)

type endpointStub struct{ url string }

func (e endpointStub) Endpoints(context.Context, network.Network) ([]string, error) {
	return []string{e.url}, nil
}

type switchingEndpoints struct {
	mu  sync.RWMutex
	url string
}

func (e *switchingEndpoints) Endpoints(context.Context, network.Network) ([]string, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return []string{e.url}, nil
}

func (e *switchingEndpoints) set(url string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.url = url
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func rpcServer(t *testing.T, handler func(rpcRequest) (any, error)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		result, err := handler(request)
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		if err != nil {
			response["error"] = map[string]any{"code": -32601, "message": err.Error()}
		} else {
			response["result"] = result
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	return server
}

func newAdapter(t *testing.T, endpoint string) *evm.Adapter {
	t.Helper()
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	adapter, err := evm.New(registry, endpointStub{url: endpoint})
	if err != nil {
		t.Fatalf("evm.New() error = %v", err)
	}
	t.Cleanup(adapter.Close)
	return adapter
}

func TestNativeBalanceAndChainIdentity(t *testing.T) {
	t.Parallel()

	server := rpcServer(t, func(request rpcRequest) (any, error) {
		switch request.Method {
		case "eth_chainId":
			return "0x1", nil
		case "eth_blockNumber":
			return "0x100", nil
		case "eth_getBalance":
			return "0xde0b6b3a7640000", nil
		default:
			return nil, fmt.Errorf("unexpected method %s", request.Method)
		}
	})
	adapter := newAdapter(t, server.URL)
	amount, err := adapter.Balance(t.Context(), "ethereum-mainnet",
		"0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf",
		chain.Asset{ID: "ethereum-mainnet:native", NetworkID: "ethereum-mainnet", Kind: "native", Decimals: 18},
	)
	if err != nil {
		t.Fatalf("Balance() error = %v", err)
	}
	if amount != "1" {
		t.Errorf("Balance() = %q, want 1", amount)
	}
}

func TestWrongChainIDIsRejected(t *testing.T) {
	t.Parallel()

	server := rpcServer(t, func(request rpcRequest) (any, error) {
		if request.Method == "eth_chainId" {
			return "0x38", nil
		}
		return nil, fmt.Errorf("unexpected method")
	})
	adapter := newAdapter(t, server.URL)
	_, err := adapter.Balance(t.Context(), "ethereum-mainnet",
		"0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf",
		chain.Asset{ID: "ethereum-mainnet:native", NetworkID: "ethereum-mainnet", Kind: "native", Decimals: 18},
	)
	if err == nil || !strings.Contains(err.Error(), "expected 1") {
		t.Fatalf("Balance() error = %v", err)
	}
}

type testSigner struct{ keyHex string }

func (testSigner) Family() chain.Family { return chain.FamilyEVM }
func (s testSigner) PublicKey() []byte {
	key, _ := crypto.HexToECDSA(s.keyHex)
	return crypto.FromECDSAPub(&key.PublicKey)
}
func (s testSigner) SignDigest(ctx context.Context, digest []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := crypto.HexToECDSA(s.keyHex)
	if err != nil {
		return nil, err
	}
	return crypto.Sign(digest, key)
}

func TestSendUsesPendingNonceAndReturnsLocalHash(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var rawTransaction string
	server := rpcServer(t, func(request rpcRequest) (any, error) {
		switch request.Method {
		case "eth_chainId":
			return "0x1", nil
		case "eth_blockNumber":
			return "0x100", nil
		case "eth_estimateGas":
			return "0x5208", nil
		case "eth_getTransactionCount":
			if !strings.Contains(string(request.Params), `"pending"`) {
				t.Errorf("nonce params = %s", request.Params)
			}
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
			rawTransaction = params[0]
			mu.Unlock()
			return "0xignored-by-client", nil
		default:
			return nil, fmt.Errorf("unexpected method %s", request.Method)
		}
	})
	adapter := newAdapter(t, server.URL)
	privateKey := fmt.Sprintf("%064x", big.NewInt(1))
	from := "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf"
	tx, err := adapter.Send(t.Context(), "ethereum-mainnet", chain.TransferRequest{
		AccountID: "acc_test", From: from, To: "0x000000000000000000000000000000000000dEaD",
		Amount: "0.1",
		Asset:  chain.Asset{ID: "ethereum-mainnet:native", NetworkID: "ethereum-mainnet", Kind: "native", Decimals: 18},
	}, testSigner{keyHex: privateKey})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if tx.Status != "pending" || len(tx.Hash) != 66 {
		t.Errorf("transaction = %+v", tx)
	}
	mu.Lock()
	defer mu.Unlock()
	if rawTransaction == "" {
		t.Error("signed transaction was not broadcast")
	}
}

func TestSendReturnsLocalHashWhenBroadcastResultIsUnknown(t *testing.T) {
	t.Parallel()

	server := rpcServer(t, func(request rpcRequest) (any, error) {
		switch request.Method {
		case "eth_chainId":
			return "0x1", nil
		case "eth_blockNumber":
			return "0x100", nil
		case "eth_estimateGas":
			return "0x5208", nil
		case "eth_getTransactionCount":
			return "0x7", nil
		case "eth_getBlockByNumber", "eth_maxPriorityFeePerGas":
			return nil, fmt.Errorf("legacy node")
		case "eth_gasPrice":
			return "0x3b9aca00", nil
		case "eth_sendRawTransaction":
			return nil, fmt.Errorf("upstream timeout after request")
		default:
			return nil, fmt.Errorf("unexpected method %s", request.Method)
		}
	})
	adapter := newAdapter(t, server.URL)
	privateKey := fmt.Sprintf("%064x", big.NewInt(1))
	tx, err := adapter.Send(t.Context(), "ethereum-mainnet", chain.TransferRequest{
		AccountID: "acc_test",
		From:      "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf",
		To:        "0x000000000000000000000000000000000000dEaD",
		Amount:    "0.1",
		Asset:     chain.Asset{ID: "ethereum-mainnet:native", NetworkID: "ethereum-mainnet", Kind: "native", Decimals: 18},
	}, testSigner{keyHex: privateKey})
	if err == nil {
		t.Fatal("Send() error = nil, want ambiguous broadcast error")
	}
	if tx.Status != "broadcast_unknown" || len(tx.Hash) != 66 {
		t.Errorf("transaction = %+v", tx)
	}
	if !strings.Contains(err.Error(), tx.Hash) {
		t.Errorf("Send() error = %q, want local hash %q", err, tx.Hash)
	}
}

func TestInvalidateDuringInitializationCannotPublishStaleClient(t *testing.T) {
	t.Parallel()

	blockStarted := make(chan struct{})
	releaseBlock := make(chan struct{})
	var once sync.Once
	first := rpcServer(t, func(request rpcRequest) (any, error) {
		switch request.Method {
		case "eth_chainId":
			return "0x1", nil
		case "eth_blockNumber":
			once.Do(func() { close(blockStarted) })
			select {
			case <-releaseBlock:
				return "0x100", nil
			case <-t.Context().Done():
				return nil, t.Context().Err()
			}
		case "eth_getBalance":
			return "0xde0b6b3a7640000", nil
		default:
			return nil, fmt.Errorf("unexpected method %s", request.Method)
		}
	})
	second := rpcServer(t, func(request rpcRequest) (any, error) {
		switch request.Method {
		case "eth_chainId":
			return "0x1", nil
		case "eth_blockNumber":
			return "0x101", nil
		case "eth_getBalance":
			return "0x1bc16d674ec80000", nil
		default:
			return nil, fmt.Errorf("unexpected method %s", request.Method)
		}
	})
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	endpoints := &switchingEndpoints{url: first.URL}
	adapter, err := evm.New(registry, endpoints)
	if err != nil {
		t.Fatalf("evm.New() error = %v", err)
	}
	t.Cleanup(adapter.Close)

	result := make(chan struct {
		amount string
		err    error
	}, 1)
	go func() {
		amount, err := adapter.Balance(t.Context(), "ethereum-mainnet",
			"0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf",
			chain.Asset{
				ID: "ethereum-mainnet:native", NetworkID: "ethereum-mainnet",
				Kind: "native", Decimals: 18,
			},
		)
		result <- struct {
			amount string
			err    error
		}{amount: amount, err: err}
	}()
	<-blockStarted
	endpoints.set(second.URL)
	adapter.Invalidate("ethereum-mainnet")
	close(releaseBlock)

	got := <-result
	if got.err != nil {
		t.Fatalf("Balance() error = %v", got.err)
	}
	if got.amount != "2" {
		t.Fatalf("Balance() = %q, want value from replacement RPC", got.amount)
	}
}
