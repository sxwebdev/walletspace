package solana

import (
	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
	"github.com/sxwebdev/walletspace/internal/network"
)

// TamperWithSwap makes the adapter sign a swap message built from edited
// instructions and lookup tables — the kind of mistake, or interference, the
// swap check exists to catch. The check itself still compares against the
// plan and the tables' contents on chain.
func TamperWithSwap(
	a *Adapter,
	edit func([]raydium.Instruction, map[solana.PublicKey]solana.PublicKeySlice) ([]raydium.Instruction, map[solana.PublicKey]solana.PublicKeySlice),
) {
	a.swaps.mu.Lock()
	defer a.swaps.mu.Unlock()
	a.swaps.buildFinal = func(p *swapPlan, blockhash solana.Hash) ([]byte, error) {
		instructions, _, err := p.instructions(stageFinal)
		if err != nil {
			return nil, err
		}
		tables := make(map[solana.PublicKey]solana.PublicKeySlice, len(p.tables))
		for table, content := range p.tables {
			tables[table] = append(solana.PublicKeySlice(nil), content...)
		}
		instructions, tables = edit(instructions, tables)
		message, _, err := buildMessage(p.user, blockhash, instructions, tables)
		return message, err
	}
}

// RaydiumQuoter is the quoter the adapter swaps with on item, built on first
// use.
func RaydiumQuoter(a *Adapter, item network.Network) (*raydium.Quoter, error) {
	return a.raydiumQuoter(item)
}
