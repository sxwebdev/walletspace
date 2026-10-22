package integration

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/gagliardetto/solana-go"
)

const (
	solanaMainnet  = "solana-mainnet"
	solanaGenesis  = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
	solanaUSDC     = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	solanaHash     = "9zrUHnA1nCByPksy3aL8tQ47vqdaG2vnFs4HrxgcZj4F"
	solanaValidTo  = 777_000
	tokenProgramID = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	systemID       = "11111111111111111111111111111111"
)

type solanaAccount struct {
	owner    string
	lamports uint64
	data     []byte
}

// fakeSolana is a Solana cluster in one process: accounts, a simulation that
// always succeeds, and every transaction it was sent.
type fakeSolana struct {
	server *httptest.Server

	mu       sync.Mutex
	accounts map[string]solanaAccount
	sent     [][]byte
	// height is the finalized block height getBlockHeight reports.
	height uint64
}

func newFakeSolana(t *testing.T) *fakeSolana {
	t.Helper()
	node := &fakeSolana{accounts: map[string]solanaAccount{}, height: 1}
	node.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call rpcCall
		if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var params []json.RawMessage
		_ = json.Unmarshal(call.Params, &params)
		result := node.answer(t, call.Method, params)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
	t.Cleanup(node.server.Close)
	return node
}

func context1(value any) map[string]any {
	return map[string]any{"context": map[string]any{"slot": 1}, "value": value}
}

func (n *fakeSolana) answer(t *testing.T, method string, params []json.RawMessage) any {
	n.mu.Lock()
	defer n.mu.Unlock()
	switch method {
	case "getGenesisHash":
		return solanaGenesis
	case "getMultipleAccounts":
		var keys []string
		_ = json.Unmarshal(params[0], &keys)
		value := make([]any, len(keys))
		for i, key := range keys {
			if found, ok := n.accounts[key]; ok {
				value[i] = map[string]any{
					"lamports": found.lamports, "owner": found.owner, "executable": false, "rentEpoch": 0,
					"data": []string{base64.StdEncoding.EncodeToString(found.data), "base64"}, "space": len(found.data),
				}
			}
		}
		return context1(value)
	case "getMinimumBalanceForRentExemption":
		if string(params[0]) == "165" {
			return 1_488_440
		}
		return 650_240
	case "getRecentPrioritizationFees":
		return []map[string]any{{"slot": 1, "prioritizationFee": 20_000}}
	case "simulateTransaction":
		return context1(map[string]any{"err": nil, "logs": []string{}, "unitsConsumed": 1200})
	case "getLatestBlockhash":
		return context1(map[string]any{"blockhash": solanaHash, "lastValidBlockHeight": solanaValidTo})
	case "sendTransaction":
		var encoded string
		_ = json.Unmarshal(params[0], &encoded)
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(raw) < 65 {
			t.Errorf("sent transaction is not base64: %v", err)
			return nil
		}
		n.sent = append(n.sent, raw)
		return solana.SignatureFromBytes(raw[1:65]).String()
	case "getSignatureStatuses":
		return context1([]any{nil})
	case "getBlockHeight":
		return n.height
	}
	t.Errorf("unexpected Solana call %s", method)
	return nil
}

func (n *fakeSolana) set(address string, account solanaAccount) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.accounts[address] = account
}

func (n *fakeSolana) sentTransactions() [][]byte {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.sent)
}

// useNodeFor points one network at a node, as useNode does for Ethereum.
func (w *wallet) useNodeFor(networkID, url string) {
	w.t.Helper()
	allowed := w.call(http.MethodPatch, "/api/settings/node-discovery", map[string]any{
		"enabled": false, "url": "", "refresh_interval": "30m",
		"request_timeout": "5s", "allow_insecure_rpc": true,
	}, header("If-Match", `"`+w.revision()+`"`))
	if allowed.status != http.StatusOK {
		w.t.Fatalf("PATCH node-discovery = %d %s", allowed.status, allowed.text())
	}
	saved := w.call(http.MethodPut, "/api/settings/networks/"+networkID, map[string]any{
		"enabled": true, "endpoints": []map[string]any{{"url": url}},
	}, header("If-Match", `"`+w.revision()+`"`))
	if saved.status != http.StatusOK {
		w.t.Fatalf("PUT network settings = %d %s", saved.status, saved.text())
	}
}

// solanaWallet is a running wallet with one Solana account pointed at node.
func solanaWallet(t *testing.T, node *fakeSolana) (*wallet, string, string, string) {
	t.Helper()
	wallet := start(t)
	spaceID := wallet.createSpace()
	derived := wallet.call(http.MethodPost, "/api/spaces/"+spaceID+"/accounts/derive", map[string]any{"network_id": solanaMainnet})
	if derived.status != http.StatusCreated {
		t.Fatalf("derive Solana account = %d %s", derived.status, derived.text())
	}
	var created struct {
		ID        string            `json:"id"`
		Addresses map[string]string `json:"addresses"`
	}
	if err := json.Unmarshal(derived.body, &created); err != nil || created.Addresses["solana"] == "" {
		t.Fatalf("derived account = %s", derived.text())
	}
	wallet.useNodeFor(solanaMainnet, node.server.URL)
	wallet.confirmSend(spaceID)
	return wallet, spaceID, created.ID, created.Addresses["solana"]
}

// sendSolana estimates, echoes the estimate as the approval and sends.
func sendSolana(t *testing.T, wallet *wallet, spaceID string, body map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	base := "/api/spaces/" + spaceID + "/networks/" + solanaMainnet
	estimated := wallet.call(http.MethodPost, base+"/transfers/estimate", body)
	if estimated.status != http.StatusOK {
		t.Fatalf("estimate = %d %s", estimated.status, estimated.text())
	}
	shown := estimated.json(t)
	approved := maps(body)
	for _, field := range []string{
		"fee_model", "fee", "compute_unit_limit", "compute_unit_price_micro_lamports",
		"creates_token_account", "token_account_rent",
	} {
		if value, ok := shown[field]; ok {
			approved[field] = value
		}
	}
	sent := wallet.call(http.MethodPost, base+"/transfers", approved, header("Idempotency-Key", "solana-send-1"))
	if sent.status != http.StatusAccepted {
		t.Fatalf("send = %d %s", sent.status, sent.text())
	}
	return shown, sent.json(t)
}

func maps(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for k, v := range value {
		out[k] = v
	}
	return out
}

// checkSigned decodes the one transaction the node received and checks its
// ed25519 signature over the exact message bytes that were broadcast, by the
// account's own key.
func checkSigned(t *testing.T, node *fakeSolana, from string) (*solana.Transaction, string) {
	t.Helper()
	sent := node.sentTransactions()
	if len(sent) != 1 {
		t.Fatalf("transactions sent = %d, want 1", len(sent))
	}
	raw := sent[0]
	if raw[0] != 1 {
		t.Fatalf("signature count = %d, want 1", raw[0])
	}
	if !ed25519.Verify(solana.MustPublicKeyFromBase58(from).Bytes(), raw[65:], raw[1:65]) {
		t.Error("the signature does not verify over the broadcast message with the account's key")
	}
	tx, err := solana.TransactionFromBytes(raw)
	if err != nil {
		t.Fatalf("decode sent transaction: %v", err)
	}
	if tx.Message.AccountKeys[0].String() != from || tx.Message.RecentBlockhash.String() != solanaHash {
		t.Errorf("message = %+v, want the account as payer and the fetched blockhash", tx.Message)
	}
	return tx, solana.SignatureFromBytes(raw[1:65]).String()
}

func TestASOLTransferEndToEnd(t *testing.T) {
	t.Parallel()

	node := newFakeSolana(t)
	wallet, spaceID, accountID, from := solanaWallet(t, node)
	node.set(from, solanaAccount{owner: systemID, lamports: 2_000_000_000})
	to := solana.PublicKeyFromBytes(append(make([]byte, 31), 3)).String()

	shown, sent := sendSolana(t, wallet, spaceID, map[string]any{
		"account_id": accountID, "asset_id": solanaMainnet + ":native", "to": to, "amount": "0.5",
	})
	if shown["fee_model"] != "solana" || shown["compute_unit_limit"] == nil {
		t.Errorf("estimate = %v, want a Solana fee", shown)
	}
	tx, txID := checkSigned(t, node, from)
	instruction := tx.Message.Instructions[len(tx.Message.Instructions)-1]
	want := binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint32(nil, 2), 500_000_000)
	if tx.Message.AccountKeys[instruction.ProgramIDIndex].String() != systemID || string(instruction.Data) != string(want) {
		t.Errorf("transfer instruction = %+v, want 0.5 SOL through the system program", instruction)
	}
	if sent["tx_hash"] != txID || sent["status"] != "pending" {
		t.Errorf("operation = %v, want pending %s", sent, txID)
	}
	if sent["last_valid_block_height"] != float64(solanaValidTo) {
		t.Errorf("operation = %v, want the expiry height %d recorded", sent, solanaValidTo)
	}

	// The status follows the expiry once the finalized chain is past it.
	status := wallet.call(http.MethodGet, "/api/spaces/"+spaceID+"/networks/"+solanaMainnet+"/transactions/"+txID, nil)
	if status.status != http.StatusOK || status.json(t)["status"] != "pending" {
		t.Errorf("status before expiry = %d %s", status.status, status.text())
	}
	node.mu.Lock()
	node.height = solanaValidTo + 1000
	node.mu.Unlock()
	status = wallet.call(http.MethodGet, "/api/spaces/"+spaceID+"/networks/"+solanaMainnet+"/transactions/"+txID, nil)
	if status.status != http.StatusOK || status.json(t)["status"] != "failed" {
		t.Errorf("status after expiry = %d %s, want failed", status.status, status.text())
	}
}

func TestAUSDCTransferThatOpensTheRecipientAccountEndToEnd(t *testing.T) {
	t.Parallel()

	node := newFakeSolana(t)
	wallet, spaceID, accountID, from := solanaWallet(t, node)
	node.set(from, solanaAccount{owner: systemID, lamports: 100_000_000})
	mint := make([]byte, 82)
	mint[44], mint[45] = 6, 1
	node.set(solanaUSDC, solanaAccount{owner: tokenProgramID, lamports: 1, data: mint})
	source, _, _ := solana.FindAssociatedTokenAddress(solana.MustPublicKeyFromBase58(from), solana.MustPublicKeyFromBase58(solanaUSDC))
	account := make([]byte, 165)
	copy(account[0:32], solana.MustPublicKeyFromBase58(solanaUSDC).Bytes())
	copy(account[32:64], solana.MustPublicKeyFromBase58(from).Bytes())
	binary.LittleEndian.PutUint64(account[64:72], 9_000_000)
	account[108] = 1
	node.set(source.String(), solanaAccount{owner: tokenProgramID, lamports: 1, data: account})
	to := solana.MustPublicKeyFromBase58("9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM")

	shown, sent := sendSolana(t, wallet, spaceID, map[string]any{
		"account_id": accountID, "asset_id": solanaMainnet + ":spl:" + solanaUSDC, "to": to.String(), "amount": "2.5",
	})
	if shown["creates_token_account"] != true || shown["token_account_rent"] != "0.00148844" {
		t.Errorf("estimate = %v, want the recipient account's rent shown", shown)
	}
	tx, txID := checkSigned(t, node, from)
	destination, _, _ := solana.FindAssociatedTokenAddress(to, solana.MustPublicKeyFromBase58(solanaUSDC))
	programs := make([]string, 0, len(tx.Message.Instructions))
	for _, instruction := range tx.Message.Instructions {
		programs = append(programs, tx.Message.AccountKeys[instruction.ProgramIDIndex].String())
	}
	wantPrograms := []string{
		"ComputeBudget111111111111111111111111111111", "ComputeBudget111111111111111111111111111111",
		"ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL", tokenProgramID,
	}
	if !slices.Equal(programs, wantPrograms) {
		t.Errorf("programs = %v, want %v", programs, wantPrograms)
	}
	transfer := tx.Message.Instructions[3]
	if got := tx.Message.AccountKeys[transfer.Accounts[2]]; got != destination {
		t.Errorf("transfer destination = %s, want the recipient's associated account %s", got, destination)
	}
	if sent["tx_hash"] != txID {
		t.Errorf("operation = %v, want %s", sent, txID)
	}
}
