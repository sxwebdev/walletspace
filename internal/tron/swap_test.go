package tron

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
	"github.com/sxwebdev/gotron/pkg/tronutils"
	"github.com/sxwebdev/gotron/schema/pb/core"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/config"
	"github.com/sxwebdev/walletspace/internal/tron/sunswap"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// swapOwner is the wallet the fake node is asked about.
const swapOwner = "TNXoiAJ3dct8Fjg4M9fkLFh9S2v9TXc32G"

// fakeSwapNode is a Tron node over HTTP that knows just enough of a chain for
// a swap: the token, the routers' prices, two allowances, and what a contract
// call costs. It builds transactions the way java-tron does — from the request,
// with its own header — and can be told to lie about one.
type fakeSwapNode struct {
	t          *testing.T
	deployment sunswap.Deployment
	addrs      sunswap.Addresses

	mu sync.Mutex
	// allowance is the token allowance from swapOwner, by spender.
	allowance map[common.Address]*big.Int
	// permit2Amount and permit2Expiry are swapOwner's grant to the universal
	// router.
	permit2Amount *big.Int
	permit2Expiry uint64
	// v2Rate and v3Rate are what the V2 router and the V3 quoter pay per unit
	// sold, in thousandths.
	v2Rate, v3Rate int64
	// revertSwaps makes every simulated swap fail.
	revertSwaps bool
	// tamper rewrites the call inside a transaction the node builds.
	tamper func(*core.TriggerSmartContract)
	// broadcast holds every transaction the wallet sent.
	broadcast []*core.Transaction
	// builds counts transactions the node was asked to build.
	builds int
	// down makes every call after start-up fail at the transport.
	down bool
}

func newFakeSwapNode(t *testing.T) *fakeSwapNode {
	t.Helper()
	deployment, ok := sunswap.ForNetwork(config.NetworkNile)
	if !ok {
		t.Fatal("no Nile deployment")
	}
	addrs, err := deployment.Addresses()
	if err != nil {
		t.Fatal(err)
	}
	return &fakeSwapNode{
		t: t, deployment: deployment, addrs: addrs,
		allowance: make(map[common.Address]*big.Int), permit2Amount: new(big.Int),
		v2Rate: 330, v3Rate: 331,
	}
}

// service starts the node and a Service talking to it.
func (f *fakeSwapNode) service() *Service {
	f.t.Helper()
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(server.Close)
	cfg := &config.Config{Network: config.NetworkNile, USDTContract: f.deployment.Token, FeeLimitTRX: 50}
	s, err := New(f.t.Context(), cfg, []config.Node{{Address: server.URL, HTTP: true}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		f.t.Fatalf("New() error = %v", err)
	}
	f.t.Cleanup(func() { _ = s.Close() })
	if !s.SwapAvailable() {
		f.t.Fatal("swaps are not available against the fake node")
	}
	return s
}

func (f *fakeSwapNode) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	down := f.down
	f.mu.Unlock()
	if down && r.URL.Path != "/wallet/getnowblock" {
		http.Error(w, "node is down", http.StatusInternalServerError)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, v)
	}
	switch r.URL.Path {
	case "/wallet/getnowblock":
		reply(map[string]any{"blockID": hex.EncodeToString(make([]byte, 32)),
			"block_header": map[string]any{"raw_data": map[string]any{"number": 100, "timestamp": time.Now().UnixMilli()}}})
	case "/wallet/getchainparameters":
		reply(map[string]any{"chainParameter": []map[string]any{
			{"key": "getEnergyFee", "value": 100}, {"key": "getTransactionFee", "value": 1000},
		}})
	case "/wallet/getaccountresource":
		reply(map[string]any{"freeNetLimit": 600})
	case "/wallet/getaccount":
		reply(map[string]any{"address": "41" + hex.EncodeToString(f.owner().Bytes()), "balance": 5_000_000_000})
	case "/wallet/triggerconstantcontract":
		f.constant(req, reply)
	case "/wallet/triggersmartcontract":
		f.build(req, reply)
	case "/wallet/broadcasthex":
		raw, _ := hex.DecodeString(req["transaction"].(string))
		var tx core.Transaction
		if err := proto.Unmarshal(raw, &tx); err != nil {
			f.t.Errorf("broadcast of undecodable bytes: %v", err)
		}
		f.mu.Lock()
		f.broadcast = append(f.broadcast, &tx)
		f.mu.Unlock()
		reply(map[string]any{"result": true})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeSwapNode) owner() common.Address {
	a, _ := sunswap.EVMAddress(swapOwner)
	return a
}

func hexAddr(v any) common.Address {
	raw, _ := hex.DecodeString(v.(string))
	return common.BytesToAddress(raw[1:])
}

func word(v *big.Int) []byte { return common.LeftPadBytes(v.Bytes(), 32) }

// constant answers a simulated call.
func (f *fakeSwapNode) constant(req map[string]any, reply func(any)) {
	data, _ := hex.DecodeString(req["data"].(string))
	contract := hexAddr(req["contract_address"])
	ok := func(energy int64, result []byte) {
		reply(map[string]any{"result": map[string]any{"result": true}, "energy_used": energy,
			"constant_result": []string{hex.EncodeToString(result)}})
	}
	revert := func() {
		reply(map[string]any{"result": map[string]any{"result": true, "message": "REVERT opcode executed"}, "energy_used": 1000})
	}
	selector := hex.EncodeToString(data[:4])
	args := data[4:]
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.addrs
	switch {
	case contract == a.Token && selector == "313ce567": // decimals()
		ok(0, word(big.NewInt(6)))
	case contract == a.Token && selector == "95d89b41": // symbol()
		ok(0, append(append(word(big.NewInt(32)), word(big.NewInt(4))...), common.RightPadBytes([]byte("USDT"), 32)...))
	case contract == a.Token && selector == "70a08231": // balanceOf
		ok(0, word(big.NewInt(1_000_000_000)))
	case contract == a.Token && selector == "dd62ed3e": // allowance(owner, spender)
		allowance := f.allowance[common.BytesToAddress(args[44:64])]
		if allowance == nil {
			allowance = new(big.Int)
		}
		ok(0, word(allowance))
	case contract == a.Token && selector == "095ea7b3": // approve
		ok(99_764, word(big.NewInt(1)))
	case contract == a.Permit2 && selector == "927da105": // allowance(owner, token, spender)
		ok(0, append(append(word(f.permit2Amount), word(new(big.Int).SetUint64(f.permit2Expiry))...), word(new(big.Int))...))
	case contract == a.Permit2 && selector == "87517c45": // approve
		ok(23_380, nil)
	case contract == a.V2Router && selector == "d06ca61f": // getAmountsOut(amountIn, path)
		in := new(big.Int).SetBytes(args[:32])
		out := new(big.Int).Div(new(big.Int).Mul(in, big.NewInt(f.v2Rate)), big.NewInt(1000))
		ok(0, append(append(append(word(big.NewInt(32)), word(big.NewInt(2))...), word(in)...), word(out)...))
	case contract == a.V3Quoter && selector == "f7729d43": // quoteExactInputSingle
		fee := new(big.Int).SetBytes(args[64:96]).Int64()
		if fee != 500 {
			revert()
			return
		}
		in := new(big.Int).SetBytes(args[96:128])
		ok(0, word(new(big.Int).Div(new(big.Int).Mul(in, big.NewInt(f.v3Rate)), big.NewInt(1000))))
	case contract == a.V4PoolManager:
		f.poolManager(data, ok)
	case contract == a.V2Router || contract == a.V3Router || contract == a.UniversalRouter:
		if f.revertSwaps {
			revert()
			return
		}
		ok(150_000, nil)
	default:
		f.t.Errorf("unexpected constant call %s on %s", selector, sunswap.TronAddress(contract))
		revert()
	}
}

// v4PoolID is the one V4 pool on the fake chain: TRX/USDT at 0.05%. It holds
// one range of liquidity around the mainnet price, so no tick is ever crossed.
var v4PoolID = func() [32]byte {
	deployment, _ := sunswap.ForNetwork(config.NetworkNile)
	addrs, _ := deployment.Addresses()
	id, _ := sunswap.NewPoolKey(addrs.Token, 500, 10).ID()
	return id
}()

func (f *fakeSwapNode) poolManager(data []byte, ok func(int64, []byte)) {
	var id [32]byte
	copy(id[:], data[4:36])
	selector := func(call sunswap.Calldata, _ error) string { return hex.EncodeToString(call.Data[:4]) }
	got := hex.EncodeToString(data[:4])
	if id != v4PoolID {
		// Any other pool does not exist: every slot reads as zero.
		ok(0, make([]byte, 4*32))
		return
	}
	switch got {
	case selector(sunswap.V4GetSlot0(id)):
		price, _ := new(big.Int).SetString("45553216903016974817197371656", 10)
		tick := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(-11070))
		ok(0, append(append(append(word(price), word(tick)...), word(new(big.Int))...), word(big.NewInt(500))...))
	case selector(sunswap.V4GetLiquidity(id)):
		ok(0, word(big.NewInt(1_000_000_000_000_000_000)))
	case selector(sunswap.V4GetPoolBitmapInfo(id, 0)):
		ok(0, word(new(big.Int)))
	default:
		f.t.Errorf("unexpected pool manager call %s", got)
		ok(0, nil)
	}
}

// build assembles a transaction from the request, as a node does.
func (f *fakeSwapNode) build(req map[string]any, reply func(any)) {
	owner, _ := tronutils.DecodeCheck(req["owner_address"].(string))
	contract, _ := tronutils.DecodeCheck(req["contract_address"].(string))
	data, _ := hex.DecodeString(req["data"].(string))
	call := &core.TriggerSmartContract{OwnerAddress: owner, ContractAddress: contract, Data: data}
	if value, ok := req["call_value"].(float64); ok {
		call.CallValue = int64(value)
	}
	f.mu.Lock()
	f.builds++
	if f.tamper != nil {
		f.tamper(call)
	}
	f.mu.Unlock()
	parameter, err := anypb.New(call)
	if err != nil {
		f.t.Fatal(err)
	}
	now := time.Now()
	raw, err := proto.Marshal(&core.TransactionRaw{
		RefBlockBytes: []byte{0, 1}, RefBlockHash: make([]byte, 8),
		Expiration: now.Add(time.Minute).UnixMilli(), Timestamp: now.UnixMilli(),
		Contract: []*core.Transaction_Contract{{Type: core.Transaction_Contract_TriggerSmartContract, Parameter: parameter}},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	reply(map[string]any{"result": map[string]any{"result": true},
		"transaction": map[string]any{"txID": hex.EncodeToString(sum[:]), "raw_data_hex": hex.EncodeToString(raw)}})
}

// sent decodes the one transaction the wallet broadcast.
func (f *fakeSwapNode) sent(t *testing.T) (*core.TriggerSmartContract, int64) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.broadcast) != 1 {
		t.Fatalf("broadcast %d transactions, want 1", len(f.broadcast))
	}
	raw := f.broadcast[0].GetRawData()
	var call core.TriggerSmartContract
	if err := raw.GetContract()[0].GetParameter().UnmarshalTo(&call); err != nil {
		t.Fatal(err)
	}
	return &call, raw.GetFeeLimit()
}

func (f *fakeSwapNode) broadcasts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.broadcast)
}

var (
	v2Route = SwapRoute{Protocol: SwapV2, Fee: 3000}
	v3Route = SwapRoute{Protocol: SwapV3, Fee: 500}
	v4Route = SwapRoute{Protocol: SwapV4, Fee: 500, TickSpacing: 10}
)

// Selling TRX sends the TRX along with the call. The amount is part of what is
// signed: exactly what the user sold, to the router the route names, with the
// calldata this wallet encoded.
func TestSwapSellsExactlyTheTRXItWasAskedTo(t *testing.T) {
	t.Parallel()
	node := newFakeSwapNode(t)
	s := node.service()
	signer := &countingSigner{}

	txid, err := s.SwapWithSigner(t.Context(), swapOwner, SwapRequest{
		In: AssetTRX, Amount: decimal.RequireFromString("1000"), MinOut: decimal.RequireFromString("329"),
		Route: v2Route, Step: SwapStepSwap,
	}, signer)
	if err != nil {
		t.Fatalf("SwapWithSigner() error = %v", err)
	}
	if signer.calls != 1 || txid == "" {
		t.Fatalf("signed %d times, txid %q", signer.calls, txid)
	}
	call, feeLimit := node.sent(t)
	if call.GetCallValue() != 1_000_000_000 {
		t.Errorf("call value = %d SUN, want 1000 TRX", call.GetCallValue())
	}
	if got := common.BytesToAddress(call.GetContractAddress()[1:]); got != node.addrs.V2Router {
		t.Errorf("called %s, want the V2 router", sunswap.TronAddress(got))
	}
	want, err := sunswap.V2SwapExactETHForTokens(big.NewInt(329_000_000), []common.Address{node.addrs.WTRX, node.addrs.Token}, node.owner(), 0)
	if err != nil {
		t.Fatal(err)
	}
	// Everything but the deadline, the fourth word of the head.
	got := bytes.Clone(call.GetData())
	if len(got) != len(want.Data) {
		t.Fatalf("calldata is %d bytes, want %d", len(got), len(want.Data))
	}
	deadline := new(big.Int).SetBytes(got[4+96 : 4+128]).Int64()
	if until := time.Until(time.Unix(deadline, 0)); until < 2*time.Minute || until > 4*time.Minute {
		t.Errorf("deadline is %s away, want about three minutes", until)
	}
	clear(got[4+96 : 4+128])
	if !bytes.Equal(got, want.Data) {
		t.Error("calldata is not the swap that was asked for")
	}
	if feeLimit != 50_000_000 {
		t.Errorf("fee limit = %d, want the configured 50 TRX", feeLimit)
	}
}

// A node that builds the swap with more TRX attached than the user sold is
// caught before the key is touched, and nothing is sent.
func TestSwapRefusesANodeThatAttachesMoreTRX(t *testing.T) {
	t.Parallel()
	node := newFakeSwapNode(t)
	node.tamper = func(call *core.TriggerSmartContract) { call.CallValue += 1_000_000 }
	s := node.service()
	signer := &countingSigner{}

	_, err := s.SwapWithSigner(t.Context(), swapOwner, SwapRequest{
		In: AssetTRX, Amount: decimal.RequireFromString("1000"), MinOut: decimal.RequireFromString("329"),
		Route: v3Route, Step: SwapStepSwap,
	}, signer)
	if !errors.Is(err, ErrIntentMismatch) {
		t.Fatalf("error = %v, want ErrIntentMismatch", err)
	}
	if signer.calls != 0 || node.broadcasts() != 0 {
		t.Errorf("signed %d, broadcast %d after a tampered build", signer.calls, node.broadcasts())
	}
}

// Selling USDT on V4 takes three transactions, and each request may only sign
// the one the chain is ready for: no swap before its approvals are on chain, no
// second approval once one is.
func TestSwapStepsFollowTheAllowancesOnChain(t *testing.T) {
	t.Parallel()
	node := newFakeSwapNode(t)
	s := node.service()
	// The fake pool pays about 302.3 TRX for 100 USDT.
	request := func(step SwapStepKind) SwapRequest {
		return SwapRequest{
			In: AssetUSDT, Amount: decimal.RequireFromString("100"), MinOut: decimal.RequireFromString("300"),
			Route: v4Route, Step: step,
		}
	}
	// The UI skips a step the chain says is already in place and waits on
	// one that is not confirmed yet, so the two refusals have to be told apart.
	refused := func(step SwapStepKind, inPlace bool) {
		t.Helper()
		signer := &countingSigner{}
		_, err := s.SwapWithSigner(t.Context(), swapOwner, request(step), signer)
		if !errors.Is(err, chain.ErrStateChanged) {
			t.Fatalf("%s: error = %v, want chain.ErrStateChanged", step, err)
		}
		if got := strings.Contains(err.Error(), "already in place"); got != inPlace {
			t.Fatalf("%s: %q, want already-in-place %v", step, err, inPlace)
		}
		if signer.calls != 0 {
			t.Fatalf("%s was signed out of order", step)
		}
	}
	signed := func(step SwapStepKind) *core.TriggerSmartContract {
		t.Helper()
		before := node.broadcasts()
		if _, err := s.SwapWithSigner(t.Context(), swapOwner, request(step), &countingSigner{}); err != nil {
			t.Fatalf("%s: error = %v", step, err)
		}
		if node.broadcasts() != before+1 {
			t.Fatalf("%s was not broadcast", step)
		}
		node.mu.Lock()
		defer node.mu.Unlock()
		var call core.TriggerSmartContract
		if err := node.broadcast[len(node.broadcast)-1].GetRawData().GetContract()[0].GetParameter().UnmarshalTo(&call); err != nil {
			t.Fatal(err)
		}
		return &call
	}

	refused(SwapStepSwap, false)
	refused(SwapStepPermit2, false)
	approve := signed(SwapStepApprove)
	want, _ := sunswap.Approve(node.addrs.Permit2, big.NewInt(100_000_000))
	if !bytes.Equal(approve.GetData(), want.Data) || common.BytesToAddress(approve.GetContractAddress()[1:]) != node.addrs.Token {
		t.Error("the approval is not exactly 100 USDT to Permit2")
	}

	// The approval lands.
	node.mu.Lock()
	node.allowance[node.addrs.Permit2] = big.NewInt(100_000_000)
	node.mu.Unlock()
	refused(SwapStepApprove, true)
	grant := signed(SwapStepPermit2)
	if common.BytesToAddress(grant.GetContractAddress()[1:]) != node.addrs.Permit2 {
		t.Error("the Permit2 grant does not go to Permit2")
	}
	// Exactly the amount, to the universal router, expiring within the hour.
	args := grant.GetData()[4:]
	if common.BytesToAddress(args[44:64]) != node.addrs.UniversalRouter ||
		new(big.Int).SetBytes(args[64:96]).Int64() != 100_000_000 {
		t.Error("the grant is not exactly 100 USDT to the universal router")
	}
	expiry := time.Unix(new(big.Int).SetBytes(args[96:128]).Int64(), 0)
	if until := time.Until(expiry); until < 50*time.Minute || until > 70*time.Minute {
		t.Errorf("the grant expires in %s, want about an hour", until)
	}

	// The grant lands, but one that expires before the swap could run is as
	// good as none.
	node.mu.Lock()
	node.permit2Amount, node.permit2Expiry = big.NewInt(100_000_000), uint64(time.Now().Add(time.Minute).Unix())
	node.mu.Unlock()
	refused(SwapStepSwap, false)
	node.mu.Lock()
	node.permit2Expiry = uint64(time.Now().Add(time.Hour).Unix())
	node.mu.Unlock()
	refused(SwapStepPermit2, true)
	refused(SwapStepApprove, true)
	swap := signed(SwapStepSwap)
	if common.BytesToAddress(swap.GetContractAddress()[1:]) != node.addrs.UniversalRouter || swap.GetCallValue() != 0 {
		t.Error("the swap is not a plain call to the universal router")
	}
}

// An unlimited approval is a choice the user makes; it changes what the
// approval signs and nothing else.
func TestUnlimitedApprovalSignsTheMaximum(t *testing.T) {
	t.Parallel()
	node := newFakeSwapNode(t)
	s := node.service()
	_, err := s.SwapWithSigner(t.Context(), swapOwner, SwapRequest{
		In: AssetUSDT, Amount: decimal.RequireFromString("100"), MinOut: decimal.RequireFromString("1"),
		Route: v2Route, Step: SwapStepApprove, Unlimited: true,
	}, &countingSigner{})
	if err != nil {
		t.Fatal(err)
	}
	call, _ := node.sent(t)
	want, _ := sunswap.Approve(node.addrs.V2Router, maxUint256)
	if !bytes.Equal(call.GetData(), want.Data) {
		t.Error("an unlimited approval did not sign the maximum to the V2 router")
	}
}

// The minimum the user approved is checked against the pool right before
// signing, in both directions: a pool that now pays less would revert the
// swap and burn its fee, and a minimum far below the pool's price no longer
// protects the trade.
func TestSwapRefusesAMinimumThatNoLongerFitsThePrice(t *testing.T) {
	t.Parallel()
	node := newFakeSwapNode(t)
	s := node.service()
	// The V2 pool pays 330 USDT for 1000 TRX.
	for _, tc := range []struct {
		name   string
		minOut string
	}{
		{"above what the pool pays", "330.000001"},
		{"more than 5% below it", "313.4"},
	} {
		signer := &countingSigner{}
		_, err := s.SwapWithSigner(t.Context(), swapOwner, SwapRequest{
			In: AssetTRX, Amount: decimal.RequireFromString("1000"), MinOut: decimal.RequireFromString(tc.minOut),
			Route: v2Route, Step: SwapStepSwap,
		}, signer)
		if !errors.Is(err, chain.ErrStateChanged) {
			t.Errorf("%s: error = %v, want chain.ErrStateChanged", tc.name, err)
		}
		if signer.calls != 0 {
			t.Errorf("%s: signed", tc.name)
		}
	}
	// The edges themselves are accepted.
	for _, minOut := range []string{"330", "313.5"} {
		if _, err := s.SwapWithSigner(t.Context(), swapOwner, SwapRequest{
			In: AssetTRX, Amount: decimal.RequireFromString("1000"), MinOut: decimal.RequireFromString(minOut),
			Route: v2Route, Step: SwapStepSwap,
		}, &countingSigner{}); err != nil {
			t.Errorf("minimum %s: error = %v", minOut, err)
		}
	}
}

// A swap the chain would revert is refused while it is still a simulation.
func TestSwapRefusesWhatTheSimulationReverts(t *testing.T) {
	t.Parallel()
	node := newFakeSwapNode(t)
	node.revertSwaps = true
	s := node.service()
	signer := &countingSigner{}
	_, err := s.SwapWithSigner(t.Context(), swapOwner, SwapRequest{
		In: AssetTRX, Amount: decimal.RequireFromString("1000"), MinOut: decimal.RequireFromString("329"),
		Route: v2Route, Step: SwapStepSwap,
	}, signer)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}
	if signer.calls != 0 || node.builds != 0 {
		t.Errorf("signed %d, built %d after a failed simulation", signer.calls, node.builds)
	}
}

func TestSwapRefusesWhatItCannotRoute(t *testing.T) {
	t.Parallel()
	node := newFakeSwapNode(t)
	s := node.service()
	base := SwapRequest{
		In: AssetTRX, Amount: decimal.RequireFromString("1"), MinOut: decimal.RequireFromString("0.3"),
		Route: v2Route, Step: SwapStepSwap,
	}
	tests := []struct {
		name   string
		change func(*SwapRequest)
		signer chain.Signer
	}{
		{"unknown route", func(r *SwapRequest) { r.Route = SwapRoute{Protocol: SwapV3, Fee: 777} }, &countingSigner{}},
		{"unknown protocol", func(r *SwapRequest) { r.Route = SwapRoute{Protocol: "v9", Fee: 3000} }, &countingSigner{}},
		{"zero minimum", func(r *SwapRequest) { r.MinOut = decimal.Zero }, &countingSigner{}},
		{"zero amount", func(r *SwapRequest) { r.Amount = decimal.Zero }, &countingSigner{}},
		{"unknown asset", func(r *SwapRequest) { r.In = "btc" }, &countingSigner{}},
		{"too many decimals", func(r *SwapRequest) { r.Amount = decimal.RequireFromString("1.0000001") }, &countingSigner{}},
		{"no signer", func(*SwapRequest) {}, nil},
	}
	for _, tt := range tests {
		request := base
		tt.change(&request)
		if _, err := s.SwapWithSigner(t.Context(), swapOwner, request, tt.signer); err == nil {
			t.Errorf("%s: accepted", tt.name)
		}
	}
	if node.broadcasts() != 0 {
		t.Errorf("broadcast %d refused swaps", node.broadcasts())
	}
}

// The route with the best price is not the best deal when it still needs an
// approval: ranking is by what is left after fees.
func TestQuoteSwapRanksRoutesByValueAfterFees(t *testing.T) {
	t.Parallel()
	node := newFakeSwapNode(t)
	// V3 pays a little more, but only V2 is already approved.
	node.v2Rate, node.v3Rate = 3000, 3010
	node.allowance[node.addrs.V2Router] = maxUint256
	s := node.service()

	quotes, err := s.QuoteSwap(t.Context(), swapOwner, SwapQuoteRequest{In: AssetUSDT, Amount: decimal.RequireFromString("100")})
	if err != nil {
		t.Fatalf("QuoteSwap() error = %v", err)
	}
	if len(quotes.Quotes) < 2 {
		t.Fatalf("quotes = %+v", quotes)
	}
	best := quotes.Quotes[0]
	if best.Route != v2Route {
		t.Fatalf("best route = %+v, want V2: the better prices elsewhere do not pay for their approvals", best.Route)
	}
	if len(best.Steps) != 1 || best.Steps[0].Kind != SwapStepSwap {
		t.Errorf("best steps = %+v, want the swap alone", best.Steps)
	}
	i := slices.IndexFunc(quotes.Quotes, func(q SwapQuote) bool { return q.Route == v3Route })
	if i < 0 {
		t.Fatalf("no V3 quote in %+v", quotes.Quotes)
	}
	v3 := quotes.Quotes[i]
	if !v3.AmountOut.GreaterThan(best.AmountOut) {
		t.Errorf("V3 pays %s, not more than V2's %s", v3.AmountOut, best.AmountOut)
	}
	if len(v3.Steps) != 2 || v3.Steps[0].Kind != SwapStepApprove {
		t.Fatalf("V3 steps = %+v, want an approval and the swap", v3.Steps)
	}
	// The approval's energy was measured; the swap behind it could not be.
	if !v3.Steps[0].Measured || v3.Steps[1].Measured {
		t.Errorf("measured flags: %+v", v3.Steps)
	}
	// 99 764 energy at 100 SUN, plus the bytes beyond the free allowance.
	if fee := v3.Steps[0].Fee; fee.LessThan(decimal.RequireFromString("9.97")) || fee.GreaterThan(decimal.RequireFromString("10.5")) {
		t.Errorf("approval fee = %s TRX, want about 10", fee)
	}
	// V4 needs both approvals.
	if i := slices.IndexFunc(quotes.Quotes, func(q SwapQuote) bool { return q.Route == v4Route }); i < 0 ||
		len(quotes.Quotes[i].Steps) != 3 || quotes.Quotes[i].Steps[1].Kind != SwapStepPermit2 {
		t.Errorf("V4 quote missing or without its Permit2 step: %+v", quotes.Quotes)
	}
	// A V3 fee tier the quoter refuses, and the V4 pools that do not exist,
	// are reported rather than dropped silently.
	if !slices.ContainsFunc(quotes.Unavailable, func(s string) bool { return bytes.HasPrefix([]byte(s), []byte("v3 1%")) }) {
		t.Errorf("unavailable = %v, want the 1%% V3 tier listed", quotes.Unavailable)
	}
	for i := 1; i < len(quotes.Quotes); i++ {
		if quotes.Quotes[i].Value.GreaterThan(quotes.Quotes[i-1].Value) {
			t.Errorf("quote %d outranks the one above it", i)
		}
	}
	if !quotes.Shortfall.IsZero() {
		t.Errorf("shortfall = %s for an account holding 5000 TRX", quotes.Shortfall)
	}
}

// Selling more TRX than the account holds is reported as a shortfall that
// includes the amount itself.
func TestQuoteSwapReportsTheTRXShortfall(t *testing.T) {
	t.Parallel()
	node := newFakeSwapNode(t)
	s := node.service()
	quotes, err := s.QuoteSwap(t.Context(), swapOwner, SwapQuoteRequest{In: AssetTRX, Amount: decimal.RequireFromString("6000")})
	if err != nil {
		t.Fatal(err)
	}
	if len(quotes.Quotes) == 0 {
		t.Fatal("no quotes")
	}
	want := decimal.RequireFromString("6000").Add(quotes.Quotes[0].Fee).Sub(decimal.NewFromInt(5000))
	if !quotes.Shortfall.Equal(want) {
		t.Errorf("shortfall = %s, want %s", quotes.Shortfall, want)
	}
}

func TestFeeLabel(t *testing.T) {
	t.Parallel()
	for fee, want := range map[uint32]string{500: "0.05%", 3000: "0.3%", 10000: "1%", 100: "0.01%"} {
		if got := feeLabel(fee); got != want {
			t.Errorf("feeLabel(%d) = %s, want %s", fee, got, want)
		}
	}
}

// A node that stops answering fails the quote and the swap outright; nothing is
// signed on the strength of reads that never came back.
func TestSwapFailsWhenTheNodeStopsAnswering(t *testing.T) {
	t.Parallel()
	node := newFakeSwapNode(t)
	s := node.service()
	node.mu.Lock()
	node.down = true
	node.mu.Unlock()

	if _, err := s.QuoteSwap(t.Context(), swapOwner, SwapQuoteRequest{In: AssetTRX, Amount: decimal.RequireFromString("10")}); err == nil {
		t.Error("QuoteSwap() succeeded against a dead node")
	}
	signer := &countingSigner{}
	_, err := s.SwapWithSigner(t.Context(), swapOwner, SwapRequest{
		In: AssetTRX, Amount: decimal.RequireFromString("10"), MinOut: decimal.RequireFromString("3.2"),
		Route: v2Route, Step: SwapStepSwap,
	}, signer)
	if err == nil || errors.Is(err, ErrInvalidRequest) {
		t.Errorf("SwapWithSigner() error = %v, want an upstream failure", err)
	}
	if signer.calls != 0 {
		t.Error("signed against a dead node")
	}
}

// Swapping is only offered where SunSwap trades the token the wallet shows
// as USDT.
func TestSwapsNeedADeploymentForTheConfiguredToken(t *testing.T) {
	t.Parallel()
	nile, _ := sunswap.ForNetwork(config.NetworkNile)
	if newSwapper(config.NetworkNile, nile.Token) == nil {
		t.Error("no swapper for Nile USDT")
	}
	if newSwapper(config.NetworkNile, "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t") != nil {
		t.Error("a swapper for a token SunSwap on Nile does not trade")
	}
	if newSwapper("shasta", nile.Token) != nil {
		t.Error("a swapper on a network without SunSwap")
	}
	s := &Service{}
	if _, err := s.QuoteSwap(t.Context(), swapOwner, SwapQuoteRequest{In: AssetTRX, Amount: decimal.NewFromInt(1)}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("QuoteSwap() without SunSwap: error = %v, want ErrInvalidRequest", err)
	}
}
