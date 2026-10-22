package solana

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/sxwebdev/walletspace/internal/chain"
)

const (
	// MaxCloseAccounts is how many token accounts one transaction closes. A
	// legacy transaction is at most 1232 bytes; with one signature, the two
	// compute-budget instructions and n CloseAccount instructions it is
	// 218 + 39·n bytes, so 26 would fit. Twenty leaves room and keeps the
	// compute well under any limit (a close uses about 3,000 units).
	MaxCloseAccounts = 20

	// Offsets in a 165-byte token account past the amount: the state, and the
	// close authority (a 4-byte option tag and a key).
	closeAuthorityOffset = 129
	// closeAccountInstruction is the token program's CloseAccount.
	closeAccountInstruction = 9
)

// EmptyTokenAccount is a token account the wallet can close for its rent.
type EmptyTokenAccount struct {
	Address string `json:"address"`
	Mint    string `json:"mint"`
	// Rent is what closing it returns to the wallet, in SOL.
	Rent     string `json:"rent"`
	lamports uint64
}

// CloseRequest names the accounts to close for owner. The approval and hooks
// mean what they mean for a transfer.
type CloseRequest struct {
	Owner         string
	Accounts      []string
	Approved      *chain.FeeApproval
	BeforeSigning func(lastValidBlockHeight uint64) error
}

// CloseEstimate is what closing the accounts costs and returns.
type CloseEstimate struct {
	NetworkID                     string              `json:"network_id"`
	Accounts                      []EmptyTokenAccount `json:"accounts"`
	Returned                      string              `json:"returned"`
	Fee                           string              `json:"fee"`
	FeeModel                      string              `json:"fee_model"`
	ComputeUnitLimit              uint64              `json:"compute_unit_limit"`
	ComputeUnitPriceMicroLamports uint64              `json:"compute_unit_price_micro_lamports"`
}

// closable reports whether a classic token account owned by owner can be
// closed by it now, and why not.
func closable(account *rpc.Account, owner solana.PublicKey) error {
	if account == nil {
		return refuseState("the token account no longer exists")
	}
	if account.Owner == solana.Token2022ProgramID {
		return refuse("%w", ErrToken2022)
	}
	data := account.Data.GetBinary()
	if account.Owner != solana.TokenProgramID || len(data) != tokenAccountSize {
		return refuse("the address is not a token account")
	}
	if solana.PublicKeyFromBytes(data[32:64]) != owner {
		return refuse("the token account belongs to another wallet")
	}
	switch data[tokenStateOffset] {
	case 1:
	case 2:
		return refuseState("the token account is frozen; the wallet does not close frozen accounts")
	default:
		return refuse("the token account is not initialized")
	}
	if tag := binary.LittleEndian.Uint32(data[closeAuthorityOffset:]); tag != 0 &&
		solana.PublicKeyFromBytes(data[closeAuthorityOffset+4:closeAuthorityOffset+36]) != owner {
		return refuse("another address is the token account's close authority")
	}
	if binary.LittleEndian.Uint64(data[64:72]) != 0 {
		return refuseState("the token account holds tokens")
	}
	return nil
}

// EmptyTokenAccounts lists owner's classic SPL token accounts that hold no
// tokens and that owner can close: no close authority other than owner, and
// not frozen. The token program would close a frozen empty account (its
// CloseAccount checks only the balance and the authority), but the wallet
// leaves frozen accounts alone on purpose: whoever froze it may have a reason,
// and nothing is lost by keeping it. A wrapped-SOL account holding no wrapped
// SOL is listed; closing it returns all its lamports. Token-2022 accounts are
// not listed.
func (a *Adapter) EmptyTokenAccounts(ctx context.Context, networkID, owner string) ([]EmptyTokenAccount, error) {
	wallet, err := solana.PublicKeyFromBase58(owner)
	if err != nil {
		return nil, refuse("invalid Solana address")
	}
	_, nodes, err := a.client(ctx, networkID)
	if err != nil {
		return nil, err
	}
	var result *rpc.GetTokenAccountsResult
	err = nodes.do(ctx, "getTokenAccountsByOwner", func(client *rpc.Client) error {
		var callErr error
		result, callErr = client.GetTokenAccountsByOwner(ctx, wallet,
			&rpc.GetTokenAccountsConfig{ProgramId: new(solana.TokenProgramID)},
			&rpc.GetTokenAccountsOpts{Encoding: solana.EncodingBase64, Commitment: readCommitment},
		)
		if callErr == nil && result == nil {
			callErr = errors.New("answer has no result")
		}
		return callErr
	})
	if err != nil {
		return nil, fmt.Errorf("list token accounts: %w", err)
	}
	out := []EmptyTokenAccount{}
	for _, entry := range result.Value {
		if entry == nil {
			return nil, errors.New("list token accounts: answer lists an empty token account")
		}
		account := entry.Account
		if closable(&account, wallet) != nil {
			continue
		}
		data := account.Data.GetBinary()
		out = append(out, EmptyTokenAccount{
			Address: entry.Pubkey.String(), Mint: solana.PublicKeyFromBytes(data[0:32]).String(),
			Rent: sol(account.Lamports), lamports: account.Lamports,
		})
	}
	slices.SortFunc(out, func(x, y EmptyTokenAccount) int { return strings.Compare(x.Address, y.Address) })
	return out, nil
}

// closePlan is one close transaction.
type closePlan struct {
	owner       solana.PublicKey
	accounts    []solana.PublicKey
	listed      []EmptyTokenAccount
	balance     uint64
	rentExempt0 uint64
	basePrice   uint64
	price       uint64
	neededLimit uint32
	limit       uint32
}

func (c *closePlan) fee() uint64 { return lamportsPerSignature + priorityFee(c.limit, c.price) }

func (c *closePlan) returned() uint64 {
	var total uint64
	for _, account := range c.listed {
		total += account.lamports
	}
	return total
}

// expected is the close transaction's instructions: the compute budget, then
// one CloseAccount per account, each paying its rent to the owner and signed
// by the owner.
func (c *closePlan) expected() []expectedInstruction {
	out := []expectedInstruction{
		{program: solana.ComputeBudget, data: binary.LittleEndian.AppendUint32([]byte{2}, c.limit)},
		{program: solana.ComputeBudget, data: binary.LittleEndian.AppendUint64([]byte{3}, c.price)},
	}
	for _, account := range c.accounts {
		out = append(out, expectedInstruction{
			program: solana.TokenProgramID,
			accounts: []expectedMeta{
				{key: account, writable: true},
				{key: c.owner, writable: true},
				{key: c.owner, signer: true},
			},
			data: []byte{closeAccountInstruction},
		})
	}
	return out
}

func (c *closePlan) build(blockhash solana.Hash) ([]byte, error) {
	instructions := []solana.Instruction{
		solana.NewInstruction(solana.ComputeBudget, nil, binary.LittleEndian.AppendUint32([]byte{2}, c.limit)),
		solana.NewInstruction(solana.ComputeBudget, nil, binary.LittleEndian.AppendUint64([]byte{3}, c.price)),
	}
	for _, account := range c.accounts {
		instructions = append(instructions, solana.NewInstruction(solana.TokenProgramID, solana.AccountMetaSlice{
			solana.Meta(account).WRITE(), solana.Meta(c.owner).WRITE(), solana.Meta(c.owner).SIGNER(),
		}, []byte{closeAccountInstruction}))
	}
	tx, err := solana.NewTransaction(instructions, blockhash, solana.TransactionPayer(c.owner))
	if err != nil {
		// Not reachable: there are always instructions and a payer.
		return nil, fmt.Errorf("build Solana transaction: %w", err)
	}
	return tx.Message.MarshalBinary()
}

// prepareClose reads every account named — right now, not from a listing —
// and refuses one that cannot be closed; then sizes the compute with a probe.
func (a *Adapter) prepareClose(ctx context.Context, nodes *nodes, networkID string, req CloseRequest) (*closePlan, error) {
	owner, err := solana.PublicKeyFromBase58(req.Owner)
	if err != nil {
		return nil, refuse("invalid Solana address")
	}
	if len(req.Accounts) == 0 || len(req.Accounts) > MaxCloseAccounts {
		return nil, refuse("close between 1 and %d token accounts at a time", MaxCloseAccounts)
	}
	c := &closePlan{owner: owner}
	keys := []solana.PublicKey{owner}
	for _, address := range req.Accounts {
		account, err := solana.PublicKeyFromBase58(address)
		if err != nil {
			return nil, refuse("invalid token account address %q", address)
		}
		if account == owner || slices.Contains(c.accounts, account) {
			return nil, refuse("token account %s is listed twice or is the wallet itself", address)
		}
		c.accounts = append(c.accounts, account)
		keys = append(keys, account)
	}
	read, err := readAccounts(ctx, nodes, keys)
	if err != nil {
		return nil, err
	}
	if read[0] != nil {
		c.balance = read[0].Lamports
	}
	for i, account := range c.accounts {
		if err := closable(read[i+1], owner); err != nil {
			return nil, fmt.Errorf("token account %s: %w", account, err)
		}
		data := read[i+1].Data.GetBinary()
		c.listed = append(c.listed, EmptyTokenAccount{
			Address: account.String(), Mint: solana.PublicKeyFromBytes(data[0:32]).String(),
			Rent: sol(read[i+1].Lamports), lamports: read[i+1].Lamports,
		})
	}
	if c.rentExempt0, err = a.rentExempt(ctx, nodes, networkID, 0); err != nil {
		return nil, err
	}
	if c.basePrice, err = priorityPrice(ctx, nodes, keys); err != nil {
		return nil, err
	}
	c.price = offeredPrice(c.basePrice)
	probe := *c
	probe.limit, probe.price = simulationComputeUnitLimit, 0
	message, err := probe.build(solana.Hash{})
	if err != nil {
		return nil, err // not reachable: see build
	}
	result, err := simulate(ctx, nodes, message)
	if err != nil {
		return nil, err
	}
	if result.UnitsConsumed == nil || *result.UnitsConsumed == 0 {
		if result.Err == nil {
			return nil, errors.New("simulate the close: the node did not report the compute used")
		}
		bound := *c
		bound.limit = simulationComputeUnitLimit
		return nil, bound.explain(result, message, true)
	}
	c.neededLimit = limitFor(*result.UnitsConsumed)
	c.limit = c.neededLimit
	return c, nil
}

// explain words a runtime refusal of a close. The rent comes back only once
// the accounts are closed; the fee has to be paid from what the wallet holds
// before that, and it may not drop a rent-exempt wallet below the minimum.
func (c *closePlan) explain(result *rpc.SimulateTransactionResult, message []byte, bound bool) error {
	fee := c.fee()
	feeText := sol(fee)
	if bound {
		feeText = "of at most " + feeText
	}
	need := fee + c.rentExempt0
	add := uint64(0)
	if need > c.balance {
		add = need - c.balance
	}
	if reason, ok := result.Err.(string); ok && (reason == "InsufficientFundsForFee" || reason == "AccountNotFound") {
		return refuseState(
			"the wallet holds %s SOL and has to pay the fee %s before the rent comes back: add at least %s SOL",
			sol(c.balance), feeText, sol(add),
		)
	}
	if account, ok := rentAccount(result.Err, message); ok && account == c.owner {
		return refuseState(
			"paying the fee %s would leave %s SOL, below the %s SOL an account must keep, and the network checks the fee "+
				"before the rent comes back: add at least %s SOL",
			feeText, sol(c.balance-min(fee, c.balance)), sol(c.rentExempt0), sol(add),
		)
	}
	return refuseState("the network would reject this close: %s%s", describe(result.Err, message), lastLog(result.Logs))
}

func (c *closePlan) verdict(ctx context.Context, nodes *nodes, message []byte) error {
	result, err := simulate(ctx, nodes, message)
	if err != nil {
		return err
	}
	if result.Err != nil {
		return c.explain(result, message, false)
	}
	return nil
}

// EstimateClose prices closing the accounts: the fee at a recent priority
// price for the compute a probe used, and the rent that comes back. The
// runtime's answer on the exact transaction decides.
func (a *Adapter) EstimateClose(ctx context.Context, networkID string, req CloseRequest) (CloseEstimate, error) {
	_, nodes, err := a.client(ctx, networkID)
	if err != nil {
		return CloseEstimate{}, err
	}
	c, err := a.prepareClose(ctx, nodes, networkID, req)
	if err != nil {
		return CloseEstimate{}, err
	}
	message, err := c.build(solana.Hash{})
	if err != nil {
		return CloseEstimate{}, err // not reachable: see build
	}
	if err := c.verdict(ctx, nodes, message); err != nil {
		return CloseEstimate{}, err
	}
	return CloseEstimate{
		NetworkID: networkID, Accounts: c.listed, Returned: sol(c.returned()), Fee: sol(c.fee()), FeeModel: feeModel,
		ComputeUnitLimit: uint64(c.limit), ComputeUnitPriceMicroLamports: c.price,
	}, nil
}

// Close closes the accounts at the fee the user approved, under the same
// rules as Send: every account is read again and must still be closable, the
// approved compute and price are signed, the message is decoded and checked
// against the request, the runtime has the last word on the exact message,
// and the expiry and the transaction id are recorded before the broadcast.
// An account that received tokens since it was listed is ErrStateChanged.
func (a *Adapter) Close(ctx context.Context, networkID string, req CloseRequest, key chain.Key) (chain.Transaction, error) {
	signer, err := messageSigner(key, req.Owner)
	if err != nil {
		return chain.Transaction{}, err
	}
	approved, err := parseApproval(req.Approved)
	if err != nil {
		return chain.Transaction{}, err
	}
	if approved.createAccount {
		return chain.Transaction{}, refuse("closing token accounts opens none")
	}
	_, nodes, err := a.client(ctx, networkID)
	if err != nil {
		return chain.Transaction{}, err
	}
	c, err := a.prepareClose(ctx, nodes, networkID, req)
	if err != nil {
		return chain.Transaction{}, stateChanged(err)
	}
	switch {
	case c.neededLimit > approved.limit:
		return chain.Transaction{}, fmt.Errorf("%w: closing now needs %d compute units, %d were approved",
			chain.ErrFeeChanged, c.neededLimit, approved.limit)
	case c.basePrice > approved.price:
		return chain.Transaction{}, fmt.Errorf("%w: the priority price is now %d micro-lamports, %d was approved",
			chain.ErrFeeChanged, c.basePrice, approved.price)
	}
	c.limit, c.price = approved.limit, approved.price
	blockhash, lastValid, err := latestBlockhash(ctx, nodes)
	if err != nil {
		return chain.Transaction{}, err
	}
	message, err := a.buildClose(c, blockhash)
	if err != nil {
		return chain.Transaction{}, err
	}
	if err := verifyInstructions(message, c.owner, blockhash, c.expected()); err != nil {
		return chain.Transaction{}, err
	}
	if err := c.verdict(ctx, nodes, message); err != nil {
		return chain.Transaction{}, stateChanged(err)
	}
	return a.signAndBroadcast(ctx, networkID, nodes, message, lastValid, signer, req.BeforeSigning, nil)
}
