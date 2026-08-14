package tron

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/sxwebdev/gotron/pkg/client"
	"github.com/sxwebdev/gotron/pkg/tronutils"
	"github.com/sxwebdev/gotron/schema/pb/core"
	"google.golang.org/protobuf/proto"
)

// Intent is the operation the user asked for, expressed in the same terms the
// chain uses.
//
// It exists so that the bytes about to be signed can be compared against
// something. Tron transactions are assembled by an RPC node, and a node is not
// trusted: it sees "send 1 TRX to A" and is free to answer with "send
// everything to B". The signer only ever sees a 32-byte digest, so unless the
// raw data is decoded and checked against a locally held intent first, nothing
// in the pipeline can tell those two apart.
type Intent struct {
	Kind         IntentKind
	Owner        string
	To           string
	Amount       int64
	Contract     string
	Data         []byte
	Resource     core.ResourceCode
	FeeLimit     int64
	PermissionID int32

	// Deployment-only. Name, the resource split and the energy ceiling all
	// decide who pays to run the contract afterwards, so a node must not be
	// able to change them on the way past.
	Name              string
	ConsumePercent    int64
	OriginEnergyLimit int64
}

// IntentKind names the one contract a transaction is allowed to carry.
type IntentKind string

const (
	IntentTransferTRX      IntentKind = "transfer_trx"
	IntentTriggerContract  IntentKind = "trigger_contract"
	IntentStake            IntentKind = "stake"
	IntentUnstake          IntentKind = "unstake"
	IntentDelegate         IntentKind = "delegate"
	IntentReclaim          IntentKind = "reclaim"
	IntentWithdrawUnstaked IntentKind = "withdraw_unstaked"
	IntentCancelUnstakes   IntentKind = "cancel_unstakes"
	IntentDeploy           IntentKind = "deploy"
)

var contractTypeByKind = map[IntentKind]core.Transaction_Contract_ContractType{
	IntentTransferTRX:      core.Transaction_Contract_TransferContract,
	IntentTriggerContract:  core.Transaction_Contract_TriggerSmartContract,
	IntentStake:            core.Transaction_Contract_FreezeBalanceV2Contract,
	IntentUnstake:          core.Transaction_Contract_UnfreezeBalanceV2Contract,
	IntentDelegate:         core.Transaction_Contract_DelegateResourceContract,
	IntentReclaim:          core.Transaction_Contract_UnDelegateResourceContract,
	IntentWithdrawUnstaked: core.Transaction_Contract_WithdrawExpireUnfreezeContract,
	IntentCancelUnstakes:   core.Transaction_Contract_CancelAllUnfreezeV2Contract,
	IntentDeploy:           core.Transaction_Contract_CreateSmartContract,
}

// ErrIntentMismatch means the transaction about to be signed is not the
// transaction that was asked for. It is always a refusal to sign.
var ErrIntentMismatch = fmt.Errorf("%w: transaction does not match the requested operation", ErrInvalidRequest)

func mismatch(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrIntentMismatch, fmt.Sprintf(format, args...))
}

// Every field of raw_data is the node's to choose and is covered by the
// signature exactly as the contract is, so Verify has to account for all of
// them and not only the ones an Intent has an opinion about. What "all of them"
// means is settled by reading the generated TransactionRaw, never by a count
// written into a comment: this one used to say "four fields", and data and
// ref_block_num went unread for as long as it did.
//
// The one worth an attacker's time is the expiration. Tron accepts up to 24
// hours; a node that sets it there, has a correct transfer signed and then
// answers with a transport error walks away with a valid signed transfer it can
// broadcast whenever it suits — after the account is topped up, or after the
// user has given up and moved the funds. The wallet's side of that exchange is
// honest about what it knows: the operation is recorded as a broadcast whose
// outcome is unknown, and no second transaction is built. It just has no idea
// the first one stays spendable for a day.
//
// Every build here is followed by a signature in the same request, so the
// window the wallet needs is minutes.
const (
	// maxTransactionLifetime is how far ahead an expiration may sit. A node
	// builds with sixty seconds by default; this leaves room for a slow build
	// and a skewed clock while cutting the protocol's ceiling from a day.
	maxTransactionLifetime = 10 * time.Minute
	// maxClockSkew is how far a node's clock may run ahead of ours before its
	// answer stops being believable.
	maxClockSkew = 2 * time.Minute
	// A reference block is named three times over: two bytes of its height,
	// eight of its hash, and the height in full — a field java-tron leaves
	// empty. None of the three can be judged without asking the chain, but a
	// value of the wrong shape, or two of them that disagree, is a malformed
	// transaction whatever the chain would say.
	refBlockBytesLen = 2
	refBlockHashLen  = 8
)

// Verify decodes raw_data and refuses anything that is not exactly this intent.
//
// Every field a node could profitably change is compared, not just the obvious
// ones: swapping the recipient is the loud attack, but raising a fee limit,
// switching the resource being delegated, signing under a different permission
// id, appending a second contract to an otherwise correct transaction, padding
// one with a memo the user is billed bandwidth for, or leaving it valid for a
// day all spend the user's funds just as effectively.
func (i Intent) Verify(tx *core.Transaction) error {
	return i.verify(tx, time.Now())
}

// verify takes the clock as an argument so the header bounds can be tested
// without waiting for one.
func (i Intent) verify(tx *core.Transaction, now time.Time) error {
	raw := tx.GetRawData()
	if raw == nil {
		return mismatch("transaction carries no raw data")
	}
	// Before any comparison below can mean "this is the whole transaction", the
	// transaction has to be one this build can read in full. Unmarshalling keeps
	// fields it does not recognise rather than failing on them; they are signed
	// and broadcast with everything else while escaping every check here, so a
	// node speaking a dialect this build does not know — or a protocol version
	// newer than it — can attach meaning to bytes nobody reads. That is
	// unmarshalParameter's reasoning applied to the levels above the parameter,
	// and it is repeated at each of them: the transaction here, the contract
	// entry below, the envelope around the parameter in unmarshalParameter.
	if err := rejectUnknown("transaction", raw); err != nil {
		return err
	}
	if err := verifyHeader(raw, now); err != nil {
		return err
	}
	// One intent is one contract. Tron allows a list, and a node that appends a
	// second entry to a correct first one gets both signed by the same
	// signature.
	if len(raw.GetContract()) != 1 {
		return mismatch("expected exactly one contract, got %d", len(raw.GetContract()))
	}
	if len(raw.GetScripts()) != 0 {
		return mismatch("transaction carries unexpected scripts")
	}
	if len(raw.GetAuths()) != 0 {
		return mismatch("transaction carries unexpected authorities")
	}
	// The memo. An Intent has no concept of one, so the honest assertion is the
	// one the two checks above make: there is none. It is the cheapest field on
	// the transaction to abuse — arbitrary bytes of the node's choosing, covered
	// by the signature, billed to the user as bandwidth and published on chain
	// under the user's address permanently. fee_limit is no defence: it is zero
	// for a plain TRX transfer and caps energy rather than bandwidth, so a
	// hundred kilobytes rides along on a correct one-TRX transfer and burns the
	// user's TRX once the free allowance is gone.
	if len(raw.GetData()) != 0 {
		return mismatch(
			"transaction carries an unexpected memo of %d bytes", len(raw.GetData()),
		)
	}
	if raw.GetFeeLimit() != i.FeeLimit {
		return mismatch("fee limit is %d, expected %d", raw.GetFeeLimit(), i.FeeLimit)
	}

	entry := raw.GetContract()[0]
	if err := rejectUnknown("contract", entry); err != nil {
		return err
	}
	expectedType, ok := contractTypeByKind[i.Kind]
	if !ok {
		return mismatch("unknown operation kind %q", i.Kind)
	}
	if entry.GetType() != expectedType {
		return mismatch("contract type is %s, expected %s", entry.GetType(), expectedType)
	}
	// A non-zero permission id signs under a multi-signature permission rather
	// than the owner's own key, which is a different authorisation entirely.
	if entry.GetPermissionId() != i.PermissionID {
		return mismatch("permission id is %d, expected %d", entry.GetPermissionId(), i.PermissionID)
	}
	if len(entry.GetProvider()) != 0 || len(entry.GetContractName()) != 0 {
		return mismatch("contract carries unexpected provider or name")
	}

	return i.verifyParameter(entry)
}

// verifyHeader bounds the fields that say when raw_data was built and against
// which block.
//
// The reference block is checked for shape and internal agreement only: picking
// the block a transaction hangs off is the node's job, and judging the choice
// would mean trusting the same node to describe the head of the chain. What the
// wallet can judge on its own is time, and that is where the leverage is — see
// the comment on maxTransactionLifetime.
func verifyHeader(raw *core.TransactionRaw, now time.Time) error {
	if len(raw.GetRefBlockBytes()) != refBlockBytesLen {
		return mismatch(
			"reference block height is %d bytes, expected %d",
			len(raw.GetRefBlockBytes()), refBlockBytesLen,
		)
	}
	if len(raw.GetRefBlockHash()) != refBlockHashLen {
		return mismatch(
			"reference block hash is %d bytes, expected %d",
			len(raw.GetRefBlockHash()), refBlockHashLen,
		)
	}
	// The height of that same block, written out in full. Which block a node
	// picked cannot be judged here — that would mean asking the same node to
	// describe the head of the chain — but two fields naming one block can be
	// held against each other without asking anyone. java-tron fills in only the
	// two bytes and leaves this at zero, so zero has to keep passing or the
	// wallet would refuse every transaction a real node builds; a node that does
	// write the height has to write one that agrees with the bytes beside it.
	if num := raw.GetRefBlockNum(); num != 0 {
		if num < 0 {
			return mismatch("reference block height %d is negative", num)
		}
		if uint16(num) != binary.BigEndian.Uint16(raw.GetRefBlockBytes()) {
			return mismatch(
				"reference block height %d does not name the block in ref_block_bytes", num,
			)
		}
	}

	// The transaction was built moments ago, in this request. A timestamp that
	// is not around now means the node answered with something it prepared
	// earlier, and the expiration below is measured from it.
	built := time.UnixMilli(raw.GetTimestamp())
	if raw.GetTimestamp() <= 0 ||
		built.After(now.Add(maxClockSkew)) ||
		built.Before(now.Add(-maxTransactionLifetime)) {
		return mismatch(
			"transaction is timestamped %s, which is not the time it was asked for",
			built.UTC().Format(time.RFC3339),
		)
	}

	expires := time.UnixMilli(raw.GetExpiration())
	// Lenient by a skew on purpose. An expiration that has passed is not an
	// attack — a slow node produces the same thing — and the cost of refusing
	// it is only a wasted idempotency key on a transaction that could never be
	// included. The cost of being strict is a wallet that signs nothing at all
	// on a machine whose clock runs a minute fast, where every transaction a
	// node builds looks expired and every one of them is in fact valid.
	if expires.Before(now.Add(-maxClockSkew)) {
		return mismatch(
			"transaction expired at %s, before it could be signed",
			expires.UTC().Format(time.RFC3339),
		)
	}
	if expires.After(now.Add(maxTransactionLifetime)) {
		return mismatch(
			"transaction stays valid until %s, more than %s from now",
			expires.UTC().Format(time.RFC3339), maxTransactionLifetime,
		)
	}
	if !expires.After(built) {
		return mismatch("transaction expires before it was built")
	}

	return nil
}

func (i Intent) verifyParameter(entry *core.Transaction_Contract) error {
	switch i.Kind {
	case IntentTransferTRX:
		var value core.TransferContract
		if err := unmarshalParameter(entry, &value); err != nil {
			return err
		}
		if err := i.sameAddress("owner", i.Owner, value.GetOwnerAddress()); err != nil {
			return err
		}
		if err := i.sameAddress("recipient", i.To, value.GetToAddress()); err != nil {
			return err
		}
		if value.GetAmount() != i.Amount {
			return mismatch("amount is %d SUN, expected %d", value.GetAmount(), i.Amount)
		}

	case IntentTriggerContract:
		var value core.TriggerSmartContract
		if err := unmarshalParameter(entry, &value); err != nil {
			return err
		}
		if err := i.sameAddress("owner", i.Owner, value.GetOwnerAddress()); err != nil {
			return err
		}
		if err := i.sameAddress("token contract", i.Contract, value.GetContractAddress()); err != nil {
			return err
		}
		// The calldata is the whole of what the contract will do, so it is
		// compared byte for byte rather than parsed and spot-checked.
		if !bytes.Equal(value.GetData(), i.Data) {
			return mismatch("call data does not match the requested transfer")
		}
		// A TRC20 transfer sends no TRX and no TRC10 alongside itself.
		if value.GetCallValue() != 0 {
			return mismatch("contract call attaches %d SUN, expected none", value.GetCallValue())
		}
		if value.GetCallTokenValue() != 0 || value.GetTokenId() != 0 {
			return mismatch("contract call attaches a TRC10 token, expected none")
		}

	case IntentStake:
		var value core.FreezeBalanceV2Contract
		if err := unmarshalParameter(entry, &value); err != nil {
			return err
		}
		if err := i.sameAddress("owner", i.Owner, value.GetOwnerAddress()); err != nil {
			return err
		}
		if value.GetFrozenBalance() != i.Amount {
			return mismatch("stake is %d SUN, expected %d", value.GetFrozenBalance(), i.Amount)
		}
		if value.GetResource() != i.Resource {
			return mismatch("resource is %s, expected %s", value.GetResource(), i.Resource)
		}

	case IntentUnstake:
		var value core.UnfreezeBalanceV2Contract
		if err := unmarshalParameter(entry, &value); err != nil {
			return err
		}
		if err := i.sameAddress("owner", i.Owner, value.GetOwnerAddress()); err != nil {
			return err
		}
		if value.GetUnfreezeBalance() != i.Amount {
			return mismatch("unstake is %d SUN, expected %d", value.GetUnfreezeBalance(), i.Amount)
		}
		if value.GetResource() != i.Resource {
			return mismatch("resource is %s, expected %s", value.GetResource(), i.Resource)
		}

	case IntentDelegate:
		var value core.DelegateResourceContract
		if err := unmarshalParameter(entry, &value); err != nil {
			return err
		}
		if err := i.sameAddress("owner", i.Owner, value.GetOwnerAddress()); err != nil {
			return err
		}
		if err := i.sameAddress("receiver", i.To, value.GetReceiverAddress()); err != nil {
			return err
		}
		if value.GetBalance() != i.Amount {
			return mismatch("delegation is %d SUN, expected %d", value.GetBalance(), i.Amount)
		}
		if value.GetResource() != i.Resource {
			return mismatch("resource is %s, expected %s", value.GetResource(), i.Resource)
		}
		// A lock would make the delegation unrecoverable for its duration.
		if value.GetLock() || value.GetLockPeriod() != 0 {
			return mismatch("delegation is locked, expected it to stay reclaimable")
		}

	case IntentReclaim:
		var value core.UnDelegateResourceContract
		if err := unmarshalParameter(entry, &value); err != nil {
			return err
		}
		if err := i.sameAddress("owner", i.Owner, value.GetOwnerAddress()); err != nil {
			return err
		}
		if err := i.sameAddress("receiver", i.To, value.GetReceiverAddress()); err != nil {
			return err
		}
		if value.GetBalance() != i.Amount {
			return mismatch("reclaim is %d SUN, expected %d", value.GetBalance(), i.Amount)
		}
		if value.GetResource() != i.Resource {
			return mismatch("resource is %s, expected %s", value.GetResource(), i.Resource)
		}

	case IntentWithdrawUnstaked:
		var value core.WithdrawExpireUnfreezeContract
		if err := unmarshalParameter(entry, &value); err != nil {
			return err
		}
		if err := i.sameAddress("owner", i.Owner, value.GetOwnerAddress()); err != nil {
			return err
		}

	case IntentCancelUnstakes:
		var value core.CancelAllUnfreezeV2Contract
		if err := unmarshalParameter(entry, &value); err != nil {
			return err
		}
		if err := i.sameAddress("owner", i.Owner, value.GetOwnerAddress()); err != nil {
			return err
		}

	case IntentDeploy:
		var value core.CreateSmartContract
		if err := unmarshalParameter(entry, &value); err != nil {
			return err
		}
		if err := i.sameAddress("owner", i.Owner, value.GetOwnerAddress()); err != nil {
			return err
		}
		contract := value.GetNewContract()
		if contract == nil {
			return mismatch("deployment carries no contract")
		}
		if err := i.sameAddress("contract owner", i.Owner, contract.GetOriginAddress()); err != nil {
			return err
		}
		// The bytecode is the contract. gotron appends the encoded constructor
		// arguments to what was submitted, so the submitted code has to be a
		// prefix of what is about to be signed — anything else deploys
		// something the user never wrote.
		if !bytes.HasPrefix(contract.GetBytecode(), i.Data) {
			return mismatch("deployment bytecode does not start with the submitted contract")
		}
		if contract.GetName() != i.Name {
			return mismatch("contract name is %q, expected %q", contract.GetName(), i.Name)
		}
		// These two decide who pays for every later call. Raising the user
		// share to 100 or dropping the energy ceiling to zero costs the
		// deployer nothing now and everything afterwards.
		if contract.GetConsumeUserResourcePercent() != i.ConsumePercent {
			return mismatch(
				"user resource share is %d%%, expected %d%%",
				contract.GetConsumeUserResourcePercent(), i.ConsumePercent,
			)
		}
		if contract.GetOriginEnergyLimit() != i.OriginEnergyLimit {
			return mismatch(
				"origin energy limit is %d, expected %d",
				contract.GetOriginEnergyLimit(), i.OriginEnergyLimit,
			)
		}
		// A deployment sends no value of any kind alongside the code.
		if contract.GetCallValue() != 0 {
			return mismatch("deployment attaches %d SUN, expected none", contract.GetCallValue())
		}
		if value.GetCallTokenValue() != 0 || value.GetTokenId() != 0 {
			return mismatch("deployment attaches a TRC10 token, expected none")
		}

	default:
		return mismatch("unknown operation kind %q", i.Kind)
	}

	return nil
}

// unmarshalParameter decodes the contract payload into its concrete type.
//
// UnmarshalTo checks the Any type URL, so a parameter claiming to be one
// contract while carrying another is rejected here rather than silently
// decoding to a zero value that then passes every field comparison.
func unmarshalParameter(entry *core.Transaction_Contract, target proto.Message) error {
	parameter := entry.GetParameter()
	if parameter == nil {
		return mismatch("contract carries no parameter")
	}
	// The Any is a message in its own right, so it has room for fields beside
	// the type URL and the payload — room a node can fill with bytes the user
	// signs, pays bandwidth for and publishes, and that nothing else here would
	// read. Reflection stops at the payload, which is an opaque byte string to
	// it, so this is a level of its own and not the check below in disguise.
	if err := rejectUnknown("contract parameter envelope", parameter); err != nil {
		return err
	}
	if err := parameter.UnmarshalTo(target); err != nil {
		return mismatch("contract parameter is not a %s: %v", entry.GetType(), err)
	}
	// Unmarshalling keeps fields this build does not recognise rather than
	// failing on them. They would be covered by the signature while escaping
	// every comparison verifyParameter makes, and a later protocol version could
	// give them meaning.
	return rejectUnknown("contract parameter", target)
}

// rejectUnknown refuses a message carrying protobuf fields this build cannot
// name.
//
// Every message a transaction is built from goes through it, because every one
// of them is hashed into the digest the key signs: bytes that survive decoding
// without being understood are bytes the user endorses, pays bandwidth for and
// publishes on chain, having never been looked at by anything in this file.
func rejectUnknown(what string, message proto.Message) error {
	if unknown := message.ProtoReflect().GetUnknown(); len(unknown) != 0 {
		return mismatch("%s carries %d bytes this build does not understand", what, len(unknown))
	}

	return nil
}

// trc20Intent describes a TRC20 transfer: a contract call whose calldata is
// rebuilt locally so the recipient and the amount inside it can be compared.
func trc20Intent(from, to, contract string, amount client.TokenAmount, feeLimit client.SUN) (Intent, error) {
	data, err := trc20TransferData(to, amount.TokenUnits())
	if err != nil {
		return Intent{}, err
	}

	return Intent{
		Kind: IntentTriggerContract, Owner: from, To: to,
		Contract: contract, Data: data, FeeLimit: int64(feeLimit),
	}, nil
}

// transactionDigest returns the bytes to sign and the transaction id.
//
// On Tron they are the same value: the txid is sha256 over the canonical
// raw_data. Deriving it here rather than reading api.TransactionExtention.Txid
// keeps the recorded identifier tied to the bytes that were actually signed. A
// node reporting an unrelated txid would otherwise have that value persisted
// and shown, leaving the real transaction untracked and unfindable.
func transactionDigest(tx *core.Transaction) ([32]byte, string, error) {
	rawData, err := proto.Marshal(tx.GetRawData())
	if err != nil {
		return [32]byte{}, "", fmt.Errorf("encode transaction for signing: %w", err)
	}
	digest := sha256.Sum256(rawData)

	return digest, hex.EncodeToString(digest[:]), nil
}

// resourceIntent describes a stake, unstake, delegation or reclaim: an amount
// of frozen TRX and the resource it backs.
func resourceIntent(
	kind IntentKind, from, to string, sun client.SUN, resource client.ResourceType,
) Intent {
	return Intent{
		Kind: kind, Owner: from, To: to,
		Amount: int64(sun), Resource: resource.ToProto(),
	}
}

// ownerIntent describes the two operations that carry nothing but the account
// they act on: withdrawing expired unstakes and cancelling pending ones.
func ownerIntent(kind IntentKind, from string) Intent {
	return Intent{Kind: kind, Owner: from}
}

// deployIntent describes a contract deployment.
//
// The bytecode is decoded from the hex the caller submitted so it can be
// compared against the transaction as a byte prefix — gotron appends the
// encoded constructor arguments to it before sending.
func deployIntent(from string, request client.DeployContractRequest) (Intent, error) {
	code, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(request.Bytecode, "0x"), "0X"))
	if err != nil {
		return Intent{}, fmt.Errorf("%w: contract bytecode is not hex: %v", ErrInvalidRequest, err)
	}

	return Intent{
		Kind: IntentDeploy, Owner: from, Data: code,
		Name:              request.Name,
		FeeLimit:          int64(request.FeeLimit),
		ConsumePercent:    request.ConsumeUserResourcePercent,
		OriginEnergyLimit: request.OriginEnergyLimit,
	}, nil
}

// trc20TransferSelector is the first four bytes of keccak256("transfer(address,uint256)").
var trc20TransferSelector = []byte{0xa9, 0x05, 0x9c, 0xbb}

// trc20TransferData rebuilds the calldata for a TRC20 transfer locally.
//
// The encoding is fully determined by the recipient and the amount, so it can
// be produced here and compared byte for byte against what the node put in the
// transaction. That comparison is what stops a node from keeping the contract
// address the user chose while quietly rewriting who gets paid and how much —
// the part of a TRC20 transfer that lives entirely inside opaque calldata.
func trc20TransferData(to string, units *big.Int) ([]byte, error) {
	raw, err := tronutils.DecodeCheck(to)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid recipient address: %v", ErrInvalidRequest, err)
	}
	// A Tron address is 21 bytes: a 0x41 network prefix and the 20-byte body
	// the EVM-compatible ABI expects.
	if len(raw) != 21 {
		return nil, fmt.Errorf("%w: recipient address is %d bytes, expected 21", ErrInvalidRequest, len(raw))
	}
	if units == nil || units.Sign() <= 0 {
		return nil, fmt.Errorf("%w: amount must be greater than zero", ErrInvalidRequest)
	}
	if units.BitLen() > 256 {
		return nil, fmt.Errorf("%w: amount does not fit in uint256", ErrInvalidRequest)
	}

	data := make([]byte, 0, 4+32+32)
	data = append(data, trc20TransferSelector...)
	padded := make([]byte, 32)
	copy(padded[12:], raw[1:])
	data = append(data, padded...)
	amount := make([]byte, 32)
	units.FillBytes(amount)

	return append(data, amount...), nil
}

func (i Intent) sameAddress(field, expected string, actual []byte) error {
	if expected == "" {
		return mismatch("%s was not part of the request", field)
	}
	want, err := tronutils.DecodeCheck(expected)
	if err != nil {
		return mismatch("%s %q is not a Tron address: %v", field, expected, err)
	}
	if !bytes.Equal(want, actual) {
		return mismatch("%s is %s, expected %s", field, tronutils.EncodeCheck(actual), expected)
	}

	return nil
}
