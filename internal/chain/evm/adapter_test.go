package evm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
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
	amount, err := adapter.Balance(
		t.Context(), "ethereum-mainnet",
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

func TestEstimateMaxTransferReservesNativeFee(t *testing.T) {
	t.Parallel()

	server := rpcServer(t, func(request rpcRequest) (any, error) {
		switch request.Method {
		case "eth_chainId":
			return "0x1", nil
		case "eth_blockNumber":
			return "0x100", nil
		case "eth_getBalance":
			return "0xde0b6b3a7640000", nil // 1 ETH
		case "eth_estimateGas":
			return "0x5208", nil // 21,000 gas
		case "eth_getBlockByNumber", "eth_maxPriorityFeePerGas":
			return nil, fmt.Errorf("legacy node")
		case "eth_gasPrice":
			return "0x3b9aca00", nil // 1 gwei
		default:
			return nil, fmt.Errorf("unexpected method %s", request.Method)
		}
	})
	adapter := newAdapter(t, server.URL)
	estimate, err := adapter.EstimateMaxTransfer(t.Context(), "ethereum-mainnet", chain.TransferRequest{
		AccountID: "acc_test",
		From:      "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf",
		To:        "0x000000000000000000000000000000000000dEaD",
		Amount:    "max",
		Asset: chain.Asset{
			ID: "ethereum-mainnet:native", NetworkID: "ethereum-mainnet",
			Kind: "native", Decimals: 18,
		},
	})
	if err != nil {
		t.Fatalf("EstimateMaxTransfer() error = %v", err)
	}
	if estimate.Amount != "0.999979" || estimate.Fee != "0.000021" {
		t.Fatalf("EstimateMaxTransfer() = %+v", estimate)
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
	_, err := adapter.Balance(
		t.Context(), "ethereum-mainnet",
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
		Amount:   "0.1",
		Asset:    chain.Asset{ID: "ethereum-mainnet:native", NetworkID: "ethereum-mainnet", Kind: "native", Decimals: 18},
		Approved: legacyApproval("1000000000"),
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

// A broadcast whose answer never came back may well be on chain, so the local
// hash comes back with it and the caller is told not to replace it.
func TestSendReturnsLocalHashWhenBroadcastResultIsUnknown(t *testing.T) {
	t.Parallel()

	// The connection is cut without a reply, which is what a timeout or a reset
	// looks like from here. A JSON-RPC error object would be the node answering,
	// which is the opposite case — see the test below.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if request.Method == "eth_sendRawTransaction" {
			hijacked, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			hijacked.Close()
			return
		}
		result, err := sendFixture(request)
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		if err != nil {
			response["error"] = map[string]any{"code": -32601, "message": err.Error()}
		} else {
			response["result"] = result
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)

	tx, err := sendFixtureTransfer(t, server.URL)
	if err == nil {
		t.Fatal("Send() error = nil, want ambiguous broadcast error")
	}
	if tx.Status != "broadcast_unknown" || len(tx.Hash) != 66 {
		t.Errorf("transaction = %+v", tx)
	}
	if !strings.Contains(err.Error(), tx.Hash) {
		t.Errorf("Send() error = %q, want local hash %q", err, tx.Hash)
	}
	// The sentinel is what the HTTP layer branches on to decide that a retry
	// must not build a second transaction, so the status field alone is not
	// enough — the error has to carry it too.
	if !errors.Is(err, chain.ErrBroadcastUnknown) {
		t.Errorf("Send() error = %v, does not wrap ErrBroadcastUnknown", err)
	}
}

// A node that answers with a JSON-RPC error has refused the transaction, so
// nothing reached the chain. Reporting that as broadcast_unknown would warn the
// user off a corrected retry and leave the record in flight for ever.
func TestSendReportsANodeRejectionAsFailed(t *testing.T) {
	t.Parallel()

	server := rpcServer(t, func(request rpcRequest) (any, error) {
		if request.Method == "eth_sendRawTransaction" {
			return nil, fmt.Errorf("insufficient funds for gas * price + value")
		}
		return sendFixture(request)
	})

	tx, err := sendFixtureTransfer(t, server.URL)
	if err == nil {
		t.Fatal("Send() error = nil, want a rejection")
	}
	if errors.Is(err, chain.ErrBroadcastUnknown) {
		t.Errorf("Send() error = %v, a flat rejection must not read as an unknown broadcast", err)
	}
	if !errors.Is(err, chain.ErrInvalidRequest) {
		t.Errorf("Send() error = %v, does not wrap ErrInvalidRequest", err)
	}
	if tx.Status != "failed" {
		t.Errorf("transaction = %+v, want status failed", tx)
	}
}

// sendFixture answers everything a native transfer needs except the broadcast.
func sendFixture(request rpcRequest) (any, error) {
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
	default:
		return nil, fmt.Errorf("unexpected method %s", request.Method)
	}
}

func sendFixtureTransfer(t *testing.T, endpoint string) (chain.Transaction, error) {
	t.Helper()
	adapter := newAdapter(t, endpoint)
	return adapter.Send(t.Context(), "ethereum-mainnet", chain.TransferRequest{
		AccountID: "acc_test",
		From:      "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf",
		To:        "0x000000000000000000000000000000000000dEaD",
		Amount:    "0.1",
		Asset:     chain.Asset{ID: "ethereum-mainnet:native", NetworkID: "ethereum-mainnet", Kind: "native", Decimals: 18},
		Approved:  legacyApproval("1000000000"),
	}, testSigner{keyHex: fmt.Sprintf("%064x", big.NewInt(1))})
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
		amount, err := adapter.Balance(
			t.Context(), "ethereum-mainnet",
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

// legacyApproval is the confirmation the UI carries back from an estimate on a
// pre-1559 node: 21,000 gas at the quoted price.
func legacyApproval(gasPriceWei string) *chain.FeeApproval {
	return &chain.FeeApproval{
		FeeModel: "legacy", GasLimit: 21_000,
		MaxFeePerGas: gasPriceWei, MaxPriorityFeePerGas: "0",
	}
}

// feeSwingServer answers the first gas-price query with one value and every
// later one with another — a node that quotes a normal fee for the estimate and
// something else once the user has agreed to it.
func feeSwingServer(t *testing.T, first, rest string, onSend func()) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var quoted bool
	return rpcServer(t, func(request rpcRequest) (any, error) {
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
			mu.Lock()
			defer mu.Unlock()
			if !quoted {
				quoted = true
				return first, nil
			}
			return rest, nil
		case "eth_sendRawTransaction":
			onSend()
			return "0xignored-by-client", nil
		default:
			return nil, fmt.Errorf("unexpected method %s", request.Method)
		}
	})
}

// The whole point of the confirmation screen is that the transaction signed is
// the one that was displayed. A node that quotes 1 gwei for the estimate and
// 100 gwei once the user has pressed the button must not get a signature.
func TestSendRefusesAFeeRaisedAfterApproval(t *testing.T) {
	t.Parallel()

	var broadcast bool
	server := feeSwingServer(t, "0x3b9aca00", "0x174876e800", func() { broadcast = true }) // 1 gwei -> 100 gwei
	adapter := newAdapter(t, server.URL)
	request := chain.TransferRequest{
		AccountID: "acc_test",
		From:      "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf",
		To:        "0x000000000000000000000000000000000000dEaD",
		Amount:    "0.1",
		Asset:     chain.Asset{ID: "ethereum-mainnet:native", NetworkID: "ethereum-mainnet", Kind: "native", Decimals: 18},
	}

	// The real sequence: price it, show it, then sign. The node quotes 1 gwei
	// for the screen and 100 gwei once the user has agreed.
	estimate, err := adapter.EstimateTransfer(t.Context(), "ethereum-mainnet", request)
	if err != nil {
		t.Fatalf("EstimateTransfer() error = %v", err)
	}
	if estimate.MaxFeePerGas != "1000000000" {
		t.Fatalf("estimate quoted %s wei per gas, want the first answer", estimate.MaxFeePerGas)
	}
	request.Approved = &chain.FeeApproval{
		FeeModel: estimate.FeeModel, GasLimit: estimate.GasLimit,
		MaxFeePerGas: estimate.MaxFeePerGas, MaxPriorityFeePerGas: estimate.MaxPriorityFeePerGas,
	}

	_, err = adapter.Send(t.Context(), "ethereum-mainnet", request,
		testSigner{keyHex: fmt.Sprintf("%064x", big.NewInt(1))})

	if !errors.Is(err, chain.ErrFeeChanged) {
		t.Fatalf("Send() error = %v, want ErrFeeChanged", err)
	}
	if broadcast {
		t.Error("a transaction was broadcast at a fee the user never approved")
	}
}

// The reverse case has to keep working: when the network is cheaper than the
// ceiling, the approved cap is still what is signed, and 1559 refunds the rest.
func TestSendSignsTheApprovedCapWhenTheNetworkIsCheaper(t *testing.T) {
	t.Parallel()

	var raw string
	var mu sync.Mutex
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
			return "0x3b9aca00", nil // 1 gwei, below the 5 gwei approved
		case "eth_sendRawTransaction":
			var params []string
			if err := json.Unmarshal(request.Params, &params); err != nil {
				return nil, err
			}
			mu.Lock()
			raw = params[0]
			mu.Unlock()
			return "0xignored-by-client", nil
		default:
			return nil, fmt.Errorf("unexpected method %s", request.Method)
		}
	})
	adapter := newAdapter(t, server.URL)

	if _, err := adapter.Send(t.Context(), "ethereum-mainnet", chain.TransferRequest{
		AccountID: "acc_test",
		From:      "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf",
		To:        "0x000000000000000000000000000000000000dEaD",
		Amount:    "0.1",
		Asset:     chain.Asset{ID: "ethereum-mainnet:native", NetworkID: "ethereum-mainnet", Kind: "native", Decimals: 18},
		Approved:  legacyApproval("5000000000"),
	}, testSigner{keyHex: fmt.Sprintf("%064x", big.NewInt(1))}); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	decoded, err := hexutil.Decode(raw)
	if err != nil {
		t.Fatalf("decode raw transaction: %v", err)
	}
	var signed types.Transaction
	if err := signed.UnmarshalBinary(decoded); err != nil {
		t.Fatalf("unmarshal transaction: %v", err)
	}
	if got := signed.GasPrice().String(); got != "5000000000" {
		t.Errorf("signed gas price = %s, want the approved 5000000000", got)
	}
	if signed.Gas() != 21_000 {
		t.Errorf("signed gas limit = %d, want the approved 21000", signed.Gas())
	}
}

// A gas limit that has outgrown the approval would run out of gas and burn the
// fee, so it needs a fresh confirmation rather than a hopeful signature.
func TestSendRefusesWhenTheCallNowNeedsMoreGas(t *testing.T) {
	t.Parallel()

	var broadcast bool
	server := rpcServer(t, func(request rpcRequest) (any, error) {
		switch request.Method {
		case "eth_chainId":
			return "0x1", nil
		case "eth_blockNumber":
			return "0x100", nil
		case "eth_estimateGas":
			return "0xf4240", nil // 1,000,000 gas, far above the approved 21,000
		case "eth_getTransactionCount":
			return "0x7", nil
		case "eth_getBlockByNumber", "eth_maxPriorityFeePerGas":
			return nil, fmt.Errorf("legacy node")
		case "eth_gasPrice":
			return "0x3b9aca00", nil
		case "eth_sendRawTransaction":
			broadcast = true
			return "0xignored-by-client", nil
		default:
			return nil, fmt.Errorf("unexpected method %s", request.Method)
		}
	})
	adapter := newAdapter(t, server.URL)

	_, err := adapter.Send(t.Context(), "ethereum-mainnet", chain.TransferRequest{
		AccountID: "acc_test",
		From:      "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf",
		To:        "0x000000000000000000000000000000000000dEaD",
		Amount:    "0.1",
		Asset:     chain.Asset{ID: "ethereum-mainnet:native", NetworkID: "ethereum-mainnet", Kind: "native", Decimals: 18},
		Approved:  legacyApproval("1000000000"),
	}, testSigner{keyHex: fmt.Sprintf("%064x", big.NewInt(1))})

	if !errors.Is(err, chain.ErrFeeChanged) {
		t.Fatalf("Send() error = %v, want ErrFeeChanged", err)
	}
	if broadcast {
		t.Error("a transaction was broadcast with a gas limit the user never approved")
	}
}

// An unconfirmed transfer is the pre-fix behaviour — re-ask the node and sign
// whatever it says. It has to be impossible to reach, not merely unused.
func TestSendRefusesATransferWithNoApproval(t *testing.T) {
	t.Parallel()

	var broadcast bool
	server := feeSwingServer(t, "0x3b9aca00", "0x3b9aca00", func() { broadcast = true })
	adapter := newAdapter(t, server.URL)

	_, err := adapter.Send(t.Context(), "ethereum-mainnet", chain.TransferRequest{
		AccountID: "acc_test",
		From:      "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf",
		To:        "0x000000000000000000000000000000000000dEaD",
		Amount:    "0.1",
		Asset:     chain.Asset{ID: "ethereum-mainnet:native", NetworkID: "ethereum-mainnet", Kind: "native", Decimals: 18},
	}, testSigner{keyHex: fmt.Sprintf("%064x", big.NewInt(1))})

	if !errors.Is(err, chain.ErrInvalidRequest) {
		t.Fatalf("Send() error = %v, want ErrInvalidRequest", err)
	}
	if broadcast {
		t.Error("an unconfirmed transfer was broadcast")
	}
}

// A node naming an absurd price is refused before it can reach a confirmation
// screen, so the user is never asked to approve it in the first place.
func TestEstimateRefusesAnAbsurdGasPrice(t *testing.T) {
	t.Parallel()

	server := rpcServer(t, func(request rpcRequest) (any, error) {
		switch request.Method {
		case "eth_chainId":
			return "0x1", nil
		case "eth_blockNumber":
			return "0x100", nil
		case "eth_estimateGas":
			return "0x5208", nil
		case "eth_getBlockByNumber", "eth_maxPriorityFeePerGas":
			return nil, fmt.Errorf("legacy node")
		case "eth_gasPrice":
			return "0xd3c21bcecceda1000000", nil // 1e24 wei per gas
		default:
			return nil, fmt.Errorf("unexpected method %s", request.Method)
		}
	})
	adapter := newAdapter(t, server.URL)

	_, err := adapter.EstimateTransfer(t.Context(), "ethereum-mainnet", chain.TransferRequest{
		AccountID: "acc_test",
		From:      "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf",
		To:        "0x000000000000000000000000000000000000dEaD",
		Amount:    "0.1",
		Asset:     chain.Asset{ID: "ethereum-mainnet:native", NetworkID: "ethereum-mainnet", Kind: "native", Decimals: 18},
	})
	if !errors.Is(err, chain.ErrInvalidRequest) {
		t.Fatalf("EstimateTransfer() error = %v, want the absurd price refused", err)
	}
}

// IsHexAddress checks length and hex digits only, so it accepts an address
// whose EIP-55 checksum is broken — which is exactly what a mistyped or
// tampered character produces. Sending there burns the funds.
func TestTransferRejectsABrokenChecksum(t *testing.T) {
	t.Parallel()

	// Valid EIP-55, and the same address with one character's case flipped.
	const valid = "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"
	const broken = "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1Beaed"

	tests := []struct {
		name    string
		to      string
		wantErr bool
	}{
		{name: "valid eip-55", to: valid},
		{name: "all lower case carries no checksum", to: strings.ToLower(valid)},
		{name: "broken checksum", to: broken, wantErr: true},
		// ValidChecksum compares against Hex(), which always emits a lower-case
		// "0x", so these two spellings of a correct address were being reported
		// as corrupted. Several explorers and CSV exports produce them.
		{name: "valid eip-55 without the prefix", to: valid[2:]},
		{name: "valid eip-55 with an upper-case prefix", to: "0X" + valid[2:]},
		{name: "broken checksum without the prefix", to: broken[2:], wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := rpcServer(t, func(request rpcRequest) (any, error) {
				switch request.Method {
				case "eth_chainId":
					return "0x1", nil
				case "eth_blockNumber":
					return "0x100", nil
				case "eth_estimateGas":
					return "0x5208", nil
				case "eth_getBlockByNumber", "eth_maxPriorityFeePerGas":
					return nil, fmt.Errorf("legacy node")
				case "eth_gasPrice":
					return "0x3b9aca00", nil
				default:
					return nil, fmt.Errorf("unexpected method %s", request.Method)
				}
			})
			adapter := newAdapter(t, server.URL)

			_, err := adapter.EstimateTransfer(t.Context(), "ethereum-mainnet", chain.TransferRequest{
				AccountID: "acc_test",
				From:      "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf",
				To:        tt.to,
				Amount:    "0.1",
				Asset: chain.Asset{
					ID: "ethereum-mainnet:native", NetworkID: "ethereum-mainnet",
					Kind: "native", Decimals: 18,
				},
			})
			if tt.wantErr {
				if !errors.Is(err, chain.ErrInvalidRequest) {
					t.Fatalf("EstimateTransfer() error = %v, want the bad checksum refused", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("EstimateTransfer() error = %v", err)
			}
		})
	}
}

// scopedEndpoints is the shape rpcpool.Resolver presents: a list of candidates
// and credentials resolved for one of them at a time.
type scopedEndpoints struct {
	endpoints []string
	secretFor string

	mu    sync.Mutex
	asked []string
}

func (s *scopedEndpoints) Endpoints(context.Context, network.Network) ([]string, error) {
	return append([]string(nil), s.endpoints...), nil
}

func (s *scopedEndpoints) Headers(_ network.Network, endpoint string) (http.Header, error) {
	s.mu.Lock()
	s.asked = append(s.asked, endpoint)
	s.mu.Unlock()
	if endpoint != s.secretFor {
		return nil, nil
	}
	return http.Header{"Authorization": []string{"Bearer secret"}}, nil
}

// The adapter walks its candidates until one answers. Resolving credentials per
// network rather than per endpoint meant the provider key configured for the
// first was replayed to whatever it fell through to — another provider, or a
// node discovery had suggested.
func TestProviderCredentialsDoNotFollowTheFallback(t *testing.T) {
	t.Parallel()

	var paidAuth, fallbackAuth string
	var mu sync.Mutex
	record := func(target *string, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		*target = r.Header.Get("Authorization")
	}

	paid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(&paidAuth, r)
		http.Error(w, "payment required", http.StatusPaymentRequired)
	}))
	t.Cleanup(paid.Close)

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(&fallbackAuth, r)
		var request rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		result := any("0x1")
		if request.Method == "eth_blockNumber" {
			result = "0x2a"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": request.ID, "result": result,
		})
	}))
	t.Cleanup(fallback.Close)

	provider := &scopedEndpoints{
		endpoints: []string{paid.URL, fallback.URL}, secretFor: paid.URL,
	}
	registry, err := network.Builtin()
	if err != nil {
		t.Fatalf("network.Builtin() error = %v", err)
	}
	adapter, err := evm.New(registry, provider)
	if err != nil {
		t.Fatalf("evm.New() error = %v", err)
	}
	t.Cleanup(adapter.Close)

	if err := adapter.Health(t.Context(), "ethereum-mainnet"); err != nil {
		t.Fatalf("Health() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if paidAuth != "Bearer secret" {
		t.Errorf("configured endpoint Authorization = %q, want the credential", paidAuth)
	}
	if fallbackAuth != "" {
		t.Errorf("fallback received Authorization = %q, want none", fallbackAuth)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.asked) != 2 ||
		provider.asked[0] != paid.URL || provider.asked[1] != fallback.URL {
		t.Errorf("credentials were not resolved per endpoint: %#v", provider.asked)
	}
}
