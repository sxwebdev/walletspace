package solana

import (
	"encoding/binary"

	"github.com/gagliardetto/solana-go"
)

// TamperWithBuilder makes the adapter build every transfer for one more base
// unit than asked, the kind of mistake the intent check exists to catch.
func TamperWithBuilder(a *Adapter) {
	a.buildMessage = func(p *plan, blockhash solana.Hash) ([]byte, error) {
		wrong := *p
		wrong.amount++
		return wrong.build(blockhash)
	}
}

// BeforeCachePut runs hook between a balance stream's generation check and
// its cache write, with the SOL balance about to be written.
func BeforeCachePut(a *Adapter, hook func(lamports uint64)) { a.beforeCachePut = hook }

// WhenAFlightEnds runs hook when a shared token read ends, before it leaves
// the group.
func WhenAFlightEnds(a *Adapter, hook func()) { a.flightEnded = hook }

// TamperWithCloseBuilder makes the adapter send one account's rent to
// another address, the kind of mistake the intent check exists to catch.
func TamperWithCloseBuilder(a *Adapter, destination string) {
	a.buildClose = func(c *closePlan, blockhash solana.Hash) ([]byte, error) {
		wrong := *c
		instructions := []solana.Instruction{
			solana.NewInstruction(solana.ComputeBudget, nil, binary.LittleEndian.AppendUint32([]byte{2}, c.limit)),
			solana.NewInstruction(solana.ComputeBudget, nil, binary.LittleEndian.AppendUint64([]byte{3}, c.price)),
		}
		for _, account := range wrong.accounts {
			instructions = append(instructions, solana.NewInstruction(solana.TokenProgramID, solana.AccountMetaSlice{
				solana.Meta(account).WRITE(), solana.Meta(solana.MustPublicKeyFromBase58(destination)).WRITE(),
				solana.Meta(c.owner).SIGNER(),
			}, []byte{closeAccountInstruction}))
		}
		tx, err := solana.NewTransaction(instructions, blockhash, solana.TransactionPayer(c.owner))
		if err != nil {
			return nil, err
		}
		return tx.Message.MarshalBinary()
	}
}
