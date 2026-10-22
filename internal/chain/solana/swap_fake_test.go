package solana_test

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
)

const (
	wsolMint          = "So11111111111111111111111111111111111111112"
	altProgram        = "AddressLookupTab1e1111111111111111111111111"
	ataProgram        = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
	swapComputeBudget = "ComputeBudget111111111111111111111111111111"
	tokenRent         = rentExemptToken
	swapUnits         = 120_000
	// Pools captured from mainnet (internal/chain/solana/raydium/testdata).
	clmmSOLUSDC  = "2QdhepnKRTLjjSqPL1PtKNwqrUkoLee5Gqs8bvZhRdMv" // CLMM, tick spacing 10
	clmmUSDCUSDT = "BZtgQEyS6eXUXicYPHecYQ7PybqodXQMvkjUbP4R8mUU" // CLMM, tick spacing 1
	ammUSDTUSDC  = "7TbGqz32RsuwXbXY7EyBCiAnMbJq1gm1wKmfjQjuwoyF" // AMM v4, coin USDT, pc USDC
	cpmmUSDCUSDT = "By3gjE18rtuqSXdjefh9oTDjyaswYrjL3mqG53tkWjYR"
)

var swapChainTime = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// fixtureData reads a captured account: testdata/swap for lookup tables,
// the raydium package's testdata for pools, configs, vaults and tick arrays.
func fixtureData(t *testing.T, address string) ledgerAccount {
	t.Helper()
	for _, dir := range []string{filepath.Join("testdata", "swap"), filepath.Join("raydium", "testdata", "accounts")} {
		raw, err := os.ReadFile(filepath.Join(dir, address+".json"))
		if err != nil {
			continue
		}
		var f struct {
			Owner    string `json:"owner"`
			Lamports uint64 `json:"lamports"`
			Data     string `json:"data"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.DecodeString(f.Data)
		if err != nil {
			t.Fatal(err)
		}
		return ledgerAccount{owner: f.Owner, lamports: f.Lamports, data: data}
	}
	t.Fatalf("no fixture for %s", address)
	return ledgerAccount{}
}

// swapChain is a fake cluster that runs the instructions a swap is built
// from: compute budget, associated account creation, SOL transfer,
// SyncNative, CloseAccount and the three Raydium swaps — each at a fixed
// rate per pool and direction, refusing an output below the minimum as the
// programs do.
type swapChain struct {
	t    *testing.T
	node *node

	mu       sync.Mutex
	accounts map[string]ledgerAccount
	// rates[pool+inputMint] is the output per unit of input.
	rates  map[string]*big.Rat
	fees   []uint64
	units  uint64
	simErr any
	// finalErr is what a simulation of a message that is not a measuring
	// one (priority price 0) answers, as if the chain moved in between.
	finalErr  any
	simulated [][]byte
	sent      [][]byte
	reads     [][]string
	sendReply *reply
	// faults[method] = n fails the n-th call of the method (1-based), and
	// every later one when n is negative (-n-th onwards).
	faults map[string]int
	calls  map[string]int
	// simAccounts, when set, replaces the accounts a simulation returns.
	simAccounts func(addresses []string) []any
	// simUnits, when set, replaces the compute a simulation reports;
	// finalUnits does so for final simulations only.
	simUnits   any
	finalUnits uint64
	// Odd answers.
	blockTimeNull bool
	simNull       bool
	blockhashZero bool
	blockHeight   uint64
	gpaOwner      string
}

// fault makes the n-th call of method fail with a final JSON-RPC error.
func (c *swapChain) fault(method string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.faults[method] = n
}

func (c *swapChain) handle(method string, h handler) {
	c.node.handle(method, func(params []json.RawMessage) reply {
		c.mu.Lock()
		c.calls[method]++
		call, n := c.calls[method], c.faults[method]
		c.mu.Unlock()
		if (n > 0 && call == n) || (n < 0 && call >= -n) {
			return reply{err: &rpcError{Code: -32602, Message: "injected fault"}}
		}
		return h(params)
	})
}

func newSwapChain(t *testing.T) *swapChain {
	t.Helper()
	c := &swapChain{
		t: t, accounts: map[string]ledgerAccount{}, rates: map[string]*big.Rat{}, units: swapUnits,
		faults: map[string]int{}, calls: map[string]int{},
	}
	for _, mint := range []string{wsolMint} {
		c.accounts[mint] = ledgerAccount{owner: tokenProgram, lamports: 1, data: mintData(9, true)}
	}
	c.accounts[usdcMint] = ledgerAccount{owner: tokenProgram, lamports: 1, data: mintData(6, true)}
	c.accounts[usdtMint] = ledgerAccount{owner: tokenProgram, lamports: 1, data: mintData(6, true)}
	c.node = newNode(t, mainnetGenesis)
	c.serve()
	return c
}

// pool loads a captured pool with what its swaps read: its bitmap extension,
// its vaults, its config.
func (c *swapChain) pool(address string, extras ...string) {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accounts[address] = fixtureData(c.t, address)
	for _, extra := range extras {
		c.accounts[extra] = fixtureData(c.t, extra)
	}
}

func (c *swapChain) rate(pool, inputMint string, num, den int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rates[pool+inputMint] = big.NewRat(num, den)
}

func (c *swapChain) set(address string, account ledgerAccount) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accounts[address] = account
}

func (c *swapChain) tokens(owner, mint string, amount uint64) {
	c.set(ata(owner, mint), ledgerAccount{owner: tokenProgram, lamports: tokenRent, data: tokenAccountData(mint, owner, amount, 1)})
}

func (c *swapChain) sentTransactions() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.sent...)
}

func (c *swapChain) serve() {
	t := c.t
	c.handle("getMultipleAccounts", func(params []json.RawMessage) reply {
		c.mu.Lock()
		defer c.mu.Unlock()
		keys := requestedKeys(t, params)
		c.reads = append(c.reads, keys)
		value := make([]any, len(keys))
		for i, address := range keys {
			if found, ok := c.accounts[address]; ok {
				value[i] = account(found.owner, found.lamports, found.data)
			}
		}
		return reply{result: withContext(value)}
	})
	c.handle("getProgramAccounts", func(params []json.RawMessage) reply {
		var program string
		var opts struct {
			DataSlice struct{ Length *uint64 } `json:"dataSlice"`
			Filters   []struct {
				DataSize uint64 `json:"dataSize"`
				Memcmp   *struct {
					Offset uint64 `json:"offset"`
					Bytes  string `json:"bytes"`
				} `json:"memcmp"`
			} `json:"filters"`
		}
		if len(params) != 2 || json.Unmarshal(params[0], &program) != nil || json.Unmarshal(params[1], &opts) != nil ||
			opts.DataSlice.Length == nil || *opts.DataSlice.Length != 0 {
			t.Errorf("getProgramAccounts params = %s", params)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		value := []any{}
	next:
		for address, found := range c.accounts {
			if found.owner != program {
				continue
			}
			for _, filter := range opts.Filters {
				if filter.DataSize != 0 && uint64(len(found.data)) != filter.DataSize {
					continue next
				}
				if filter.Memcmp != nil {
					want := solana.MustPublicKeyFromBase58(filter.Memcmp.Bytes).Bytes()
					end := filter.Memcmp.Offset + uint64(len(want))
					if end > uint64(len(found.data)) || string(found.data[filter.Memcmp.Offset:end]) != string(want) {
						continue next
					}
				}
			}
			owner := found.owner
			if c.gpaOwner != "" {
				owner = c.gpaOwner
			}
			value = append(value, map[string]any{"pubkey": address, "account": account(owner, found.lamports, nil)})
		}
		return reply{result: value}
	})
	c.handle("getMinimumBalanceForRentExemption", func(params []json.RawMessage) reply {
		switch string(params[0]) {
		case "0":
			return reply{result: rentExempt0}
		case "165":
			return reply{result: tokenRent}
		}
		t.Errorf("rent asked for size %s", params[0])
		return reply{err: &rpcError{Code: -32602, Message: "unexpected size"}}
	})
	c.handle("getRecentPrioritizationFees", func([]json.RawMessage) reply {
		c.mu.Lock()
		defer c.mu.Unlock()
		value := []map[string]any{}
		for i, fee := range c.fees {
			value = append(value, map[string]any{"slot": i + 1, "prioritizationFee": fee})
		}
		return reply{result: value}
	})
	c.handle("getSlot", func([]json.RawMessage) reply { return reply{result: 400_000_000} })
	c.handle("getBlockTime", func([]json.RawMessage) reply {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.blockTimeNull {
			return reply{result: nil}
		}
		return reply{result: swapChainTime.Unix()}
	})
	c.handle("getSignatureStatuses", func([]json.RawMessage) reply { return reply{result: withContext([]any{nil})} })
	c.handle("getBlockHeight", func([]json.RawMessage) reply {
		c.mu.Lock()
		defer c.mu.Unlock()
		return reply{result: c.blockHeight}
	})
	c.handle("getLatestBlockhash", func([]json.RawMessage) reply {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.blockhashZero {
			return reply{result: withContext(map[string]any{"blockhash": testBlockhash, "lastValidBlockHeight": 0})}
		}
		return reply{result: withContext(map[string]any{"blockhash": testBlockhash, "lastValidBlockHeight": testLastValid})}
	})
	c.handle("simulateTransaction", func(params []json.RawMessage) reply {
		var encoded string
		var opts struct {
			SigVerify bool `json:"sigVerify"`
			Replace   bool `json:"replaceRecentBlockhash"`
			Accounts  *struct {
				Encoding  string   `json:"encoding"`
				Addresses []string `json:"addresses"`
			} `json:"accounts"`
		}
		if len(params) != 2 || json.Unmarshal(params[0], &encoded) != nil || json.Unmarshal(params[1], &opts) != nil ||
			opts.SigVerify || !opts.Replace {
			t.Errorf("simulateTransaction params = %s", params)
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Errorf("simulated transaction is not base64: %v", err)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.simulated = append(c.simulated, raw)
		if c.simNull {
			return reply{result: withContext(nil)}
		}
		state, failure, logs := c.execute(raw)
		units := c.units
		if failure == nil && c.simErr != nil {
			failure = c.simErr
		}
		if failure == nil && c.finalErr != nil && !measuring(t, raw, c.accounts) {
			failure = c.finalErr
		}
		var reported any = units
		if c.simUnits != nil {
			reported = c.simUnits
		}
		if c.finalUnits != 0 && !measuring(t, raw, c.accounts) {
			reported = c.finalUnits
		}
		// A program that runs out of the transaction's compute limit fails
		// the way Agave reports it (agave c744af01,
		// program-runtime/src/invoke_context.rs: an EbpfError other than a
		// syscall error — ExceededMaxInstructions, "exceeded CUs meter at
		// BPF instruction" — is logged as "Program <id> failed: <error>" and
		// becomes InstructionError::ProgramFailedToComplete), with all the
		// limit consumed.
		if need, ok := reported.(uint64); ok && failure == nil {
			if limit, index, program := computeLimit(t, raw, c.accounts); need > limit {
				failure = map[string]any{"InstructionError": []any{index, "ProgramFailedToComplete"}}
				logs = []string{
					fmt.Sprintf("Program %s consumed %d of %d compute units", program, limit, limit),
					fmt.Sprintf("Program %s failed: exceeded CUs meter at BPF instruction #4242", program),
				}
				reported = limit
			}
		}
		result := map[string]any{"err": failure, "logs": logs, "unitsConsumed": reported}
		if opts.Accounts != nil {
			if opts.Accounts.Encoding != "base64" {
				t.Errorf("simulation accounts encoding %q", opts.Accounts.Encoding)
			}
			var after []any
			for _, address := range opts.Accounts.Addresses {
				if found, ok := state[address]; ok && failure == nil {
					after = append(after, account(found.owner, found.lamports, found.data))
				} else {
					after = append(after, nil)
				}
			}
			if c.simAccounts != nil {
				after = c.simAccounts(opts.Accounts.Addresses)
			}
			result["accounts"] = after
		}
		return reply{result: withContext(result)}
	})
	c.handle("sendTransaction", func(params []json.RawMessage) reply {
		var encoded string
		if len(params) != 2 || json.Unmarshal(params[0], &encoded) != nil {
			t.Errorf("sendTransaction params = %s", params)
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Errorf("sent transaction is not base64: %v", err)
		}
		c.mu.Lock()
		c.sent = append(c.sent, raw)
		answer := c.sendReply
		c.mu.Unlock()
		if answer != nil {
			return *answer
		}
		return reply{result: solana.SignatureFromBytes(raw[1:65]).String()}
	})
}

// decodeTransaction reads a signed legacy or v0 transaction and resolves
// its lookups against the tables in accounts.
func decodeTransaction(t *testing.T, raw []byte, accounts map[string]ledgerAccount) (*solana.Transaction, []solana.PublicKey) {
	t.Helper()
	tx, err := solana.TransactionFromBytes(raw)
	if err != nil {
		t.Fatalf("decode transaction: %v", err)
	}
	if tx.Message.IsVersioned() && len(tx.Message.AddressTableLookups) > 0 {
		tables := map[solana.PublicKey]solana.PublicKeySlice{}
		for _, lookup := range tx.Message.AddressTableLookups {
			found, ok := accounts[lookup.AccountKey.String()]
			if !ok || found.owner != altProgram {
				t.Fatalf("lookup table %s is not on the fake chain", lookup.AccountKey)
			}
			var content solana.PublicKeySlice
			for i := 56; i+32 <= len(found.data); i += 32 {
				content = append(content, solana.PublicKeyFromBytes(found.data[i:i+32]))
			}
			tables[lookup.AccountKey] = content
		}
		if err := tx.Message.SetAddressTables(tables); err != nil {
			t.Fatal(err)
		}
		if err := tx.Message.ResolveLookups(); err != nil {
			t.Fatal(err)
		}
	}
	keys, err := tx.Message.GetAllKeys()
	if err != nil {
		t.Fatal(err)
	}
	return tx, keys
}

// measuring reports whether a simulated message is one of the swap's
// measuring simulations, which offer no priority price.
func measuring(t *testing.T, raw []byte, accounts map[string]ledgerAccount) bool {
	tx, keys := decodeTransaction(t, raw, accounts)
	for _, in := range tx.Message.Instructions {
		data := []byte(in.Data)
		if keys[in.ProgramIDIndex].String() == swapComputeBudget && data[0] == 3 {
			return binary.LittleEndian.Uint64(data[1:]) == 0
		}
	}
	return false
}

// computeLimit reads a message's compute-unit limit and the index and
// program of its first Raydium swap, where the fake runs out of compute.
func computeLimit(t *testing.T, raw []byte, accounts map[string]ledgerAccount) (uint64, int, string) {
	tx, keys := decodeTransaction(t, raw, accounts)
	limit, index, program := uint64(200_000), 0, ""
	for i, in := range tx.Message.Instructions {
		data := []byte(in.Data)
		id := keys[in.ProgramIDIndex]
		if id.String() == swapComputeBudget && data[0] == 2 {
			limit = uint64(binary.LittleEndian.Uint32(data[1:]))
		}
		if program == "" && raydium.KindOf(id) != raydium.KindUnknown {
			index, program = i, id.String()
		}
	}
	return limit, index, program
}

func tokenAmount(data []byte) uint64 { return binary.LittleEndian.Uint64(data[64:72]) }

func setTokenAmount(account ledgerAccount, amount uint64) ledgerAccount {
	data := append([]byte(nil), account.data...)
	binary.LittleEndian.PutUint64(data[64:72], amount)
	account.data = data
	return account
}

func slippageCode(program solana.PublicKey) int {
	switch raydium.KindOf(program) {
	case raydium.KindAMMV4:
		return 30
	case raydium.KindCPMM:
		return 6005
	}
	return 6018
}

func instructionError(index int, code int) any {
	return map[string]any{"InstructionError": []any{index, map[string]any{"Custom": code}}}
}

// execute runs a transaction on a copy of the accounts and returns the copy.
func (c *swapChain) execute(raw []byte) (map[string]ledgerAccount, any, []string) {
	t := c.t
	tx, keys := decodeTransaction(t, raw, c.accounts)
	state := make(map[string]ledgerAccount, len(c.accounts))
	for k, v := range c.accounts {
		state[k] = v
	}
	var limit, price uint64
	for _, in := range tx.Message.Instructions {
		data := []byte(in.Data)
		if keys[in.ProgramIDIndex].String() == swapComputeBudget {
			switch data[0] {
			case 2:
				limit = uint64(binary.LittleEndian.Uint32(data[1:]))
			case 3:
				price = binary.LittleEndian.Uint64(data[1:])
			}
		}
	}
	payer := keys[0].String()
	fee := 5000 + (limit*price+999_999)/1_000_000
	if state[payer].lamports < fee {
		return nil, "InsufficientFundsForFee", nil
	}
	p := state[payer]
	p.lamports -= fee
	state[payer] = p
	for index, in := range tx.Message.Instructions {
		program, data := keys[in.ProgramIDIndex], []byte(in.Data)
		accounts := make([]solana.PublicKey, len(in.Accounts))
		for i, a := range in.Accounts {
			accounts[i] = keys[a]
		}
		switch program.String() {
		case swapComputeBudget:
		case ataProgram:
			target, owner, mint := accounts[1].String(), accounts[2].String(), accounts[3].String()
			if accounts[1] != solana.MustPublicKeyFromBase58(ata(owner, mint)) {
				return nil, instructionError(index, 3), nil
			}
			if _, exists := state[target]; exists {
				continue
			}
			p := state[payer]
			if p.lamports < tokenRent {
				return nil, instructionError(index, 1), nil
			}
			p.lamports -= tokenRent
			state[payer] = p
			state[target] = ledgerAccount{owner: tokenProgram, lamports: tokenRent, data: tokenAccountData(mint, owner, 0, 1)}
		case systemProgram:
			from, to := accounts[0].String(), accounts[1].String()
			amount := binary.LittleEndian.Uint64(data[4:])
			f, d := state[from], state[to]
			if f.lamports < amount {
				return nil, instructionError(index, 1), nil
			}
			f.lamports -= amount
			d.lamports += amount
			state[from], state[to] = f, d
		case tokenProgram:
			switch data[0] {
			case 17: // SyncNative
				target := state[accounts[0].String()]
				state[accounts[0].String()] = setTokenAmount(target, target.lamports-tokenRent)
			case 9: // CloseAccount
				target, destination := state[accounts[0].String()], state[accounts[1].String()]
				destination.lamports += target.lamports
				state[accounts[1].String()] = destination
				delete(state, accounts[0].String())
			default:
				t.Errorf("unexpected token instruction %d", data[0])
			}
		default:
			if failure, logs := c.swap(state, index, program, accounts, data); failure != nil {
				return nil, failure, logs
			}
		}
	}
	return state, nil, []string{"Program log: ok"}
}

func (c *swapChain) swap(state map[string]ledgerAccount, index int, program solana.PublicKey, accounts []solana.PublicKey, data []byte) (any, []string) {
	t := c.t
	var pool, source, destination solana.PublicKey
	var amount, minOut uint64
	switch raydium.KindOf(program) {
	case raydium.KindAMMV4:
		s, err := raydium.DecodeAMMV4Swap(program, accounts, data)
		if err != nil {
			t.Errorf("AMM v4 swap: %v", err)
			return instructionError(index, 0), nil
		}
		pool, source, destination, amount, minOut = s.Pool, s.UserSource, s.UserDestination, s.AmountIn, s.MinAmountOut
	case raydium.KindCPMM:
		s, err := raydium.DecodeCPMMSwap(program, accounts, data)
		if err != nil {
			t.Errorf("CPMM swap: %v", err)
			return instructionError(index, 0), nil
		}
		pool, source, destination, amount, minOut = s.Pool, s.InputTokenAccount, s.OutputTokenAccount, s.AmountIn, s.MinAmountOut
	case raydium.KindCLMM:
		s, err := raydium.DecodeCLMMSwap(program, accounts, data)
		if err != nil {
			t.Errorf("CLMM swap: %v", err)
			return instructionError(index, 0), nil
		}
		pool, source, destination, amount, minOut = s.Pool, s.InputTokenAccount, s.OutputTokenAccount, s.Amount, s.OtherAmountThreshold
	default:
		t.Errorf("unexpected program %s", program)
		return instructionError(index, 0), nil
	}
	from, ok := state[source.String()]
	if !ok || tokenAmount(from.data) < amount {
		return instructionError(index, 1), []string{"Program log: insufficient funds"}
	}
	inputMint := solana.PublicKeyFromBytes(from.data[:32]).String()
	rate, ok := c.rates[pool.String()+inputMint]
	if !ok {
		t.Errorf("no rate for pool %s selling %s", pool, inputMint)
		return instructionError(index, 0), nil
	}
	out := new(big.Int).Mul(big.NewInt(0).SetUint64(amount), rate.Num())
	out.Quo(out, rate.Denom())
	if out.Uint64() < minOut {
		return instructionError(index, slippageCode(program)), []string{"Program log: Error: too little output"}
	}
	to, ok := state[destination.String()]
	if !ok || to.owner != tokenProgram {
		return instructionError(index, 3012), []string{"Program log: AccountNotInitialized"}
	}
	state[source.String()] = setTokenAmount(from, tokenAmount(from.data)-amount)
	state[destination.String()] = setTokenAmount(to, tokenAmount(to.data)+out.Uint64())
	return nil, nil
}

// fakeRaydium is the Trade API and the pool-keys API.
type fakeRaydium struct {
	t      *testing.T
	server *httptest.Server

	mu         sync.Mutex
	quote      []byte // the body of a quote
	status     int
	header     http.Header
	keys       []byte
	keysStatus int
	quotes     int
	keyAsks    int
}

func newFakeRaydium(t *testing.T) *fakeRaydium {
	t.Helper()
	f := &fakeRaydium{t: t, status: http.StatusOK}
	keys, err := os.ReadFile(filepath.Join("raydium", "testdata", "poolkeys_api_v3.json"))
	if err != nil {
		t.Fatal(err)
	}
	f.keys = keys
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for k, v := range f.header {
			w.Header()[k] = v
		}
		switch {
		case r.URL.Path == "/compute/swap-base-in":
			f.quotes++
			w.WriteHeader(f.status)
			_, _ = w.Write(f.quote)
		case r.URL.Path == "/pools/key/ids":
			f.keyAsks++
			if f.keysStatus != 0 {
				w.WriteHeader(f.keysStatus)
				return
			}
			_, _ = w.Write(f.keys)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeRaydium) answer(status int, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.quote = status, body
}

func (f *fakeRaydium) counts() (quotes, keys int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.quotes, f.keyAsks
}

func (f *fakeRaydium) quoter(t *testing.T) *raydium.Quoter {
	t.Helper()
	q, err := raydium.NewQuoter(f.server.Client(), f.server.URL, raydium.WithPoolKeysURL(f.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	return q
}

type hopFixture struct {
	pool, in, out string
	remaining     []string
}

// quoteBody is a Trade API answer for a route.
func quoteBody(in, out string, amount, outAmount uint64, slippage int, hops ...hopFixture) []byte {
	var plan []map[string]any
	for _, hop := range hops {
		plan = append(plan, map[string]any{
			"poolId": hop.pool, "inputMint": hop.in, "outputMint": hop.out, "feeMint": hop.in, "feeRate": 1,
			"feeAmount": "1", "remainingAccounts": append([]string{}, hop.remaining...),
		})
	}
	floor := new(big.Int).Mul(new(big.Int).SetUint64(outAmount), big.NewInt(int64(10000-slippage)))
	floor.Quo(floor, big.NewInt(10000))
	body, _ := json.Marshal(map[string]any{
		"id": "test", "success": true, "version": "V1",
		"data": map[string]any{
			"swapType": "BaseIn", "inputMint": in, "inputAmount": big.NewInt(0).SetUint64(amount).String(),
			"outputMint": out, "outputAmount": big.NewInt(0).SetUint64(outAmount).String(),
			"otherAmountThreshold": floor.String(), "slippageBps": slippage, "priceImpactPct": 0.01,
			"referrerAmount": "0", "routePlan": plan,
		},
	})
	return body
}
