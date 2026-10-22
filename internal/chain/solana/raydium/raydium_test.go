package raydium_test

import (
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain/solana/raydium"
)

func TestKinds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		program solana.PublicKey
		kind    raydium.Kind
		name    string
	}{
		{key("675kPX9MHTjS2zt1qfr1NYHuzeLXfQM9H24wFSUt1Mp8"), raydium.KindAMMV4, "amm-v4"},
		{key("CPMMoo8L3F4NbTegBCKVNunggL7H1ZpdTHKxQB5qKP1C"), raydium.KindCPMM, "cpmm"},
		{key("CAMMCzo5YL8w4VFF8KVHrK22GGUsp5VTaW7grrKgrWqK"), raydium.KindCLMM, "clmm"},
	}
	for _, tt := range tests {
		if got := raydium.KindOf(tt.program); got != tt.kind || got.Program() != tt.program || got.String() != tt.name {
			t.Errorf("KindOf(%s) = %v (%s, %s)", tt.program, got, got.Program(), got)
		}
	}
	// Raydium's stable swap and the devnet CPMM are not swapped on.
	for _, other := range []solana.PublicKey{key("5quBtoiQqxF9Jv6KYKctB59NT3gtJD2Y65kdnB1Uev3h"), key("DRaycpLY18LhpbydsBWbVJtxpNv9oXPgjRSfpF2bWpYb"), {}} {
		if k := raydium.KindOf(other); k != raydium.KindUnknown || !k.Program().IsZero() || k.String() != "unknown" {
			t.Errorf("KindOf(%s) = %v", other, k)
		}
	}
}

func TestInstructionKeys(t *testing.T) {
	t.Parallel()
	in := raydium.Instruction{Accounts: []solana.AccountMeta{{PublicKey: wsol, IsWritable: true}, {PublicKey: usdc, IsSigner: true}}}
	if keys := in.Keys(); len(keys) != 2 || keys[0] != wsol || keys[1] != usdc {
		t.Fatalf("Keys = %v", keys)
	}
}
