package solana_test

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/sxwebdev/walletspace/internal/chain"
	solanachain "github.com/sxwebdev/walletspace/internal/chain/solana"
)

// mintData is a classic SPL mint account.
func mintData(decimals uint8, initialized bool) []byte {
	data := make([]byte, 82)
	data[44] = decimals
	if initialized {
		data[45] = 1
	}
	return data
}

func borshString(value string, width int) []byte {
	out := binary.LittleEndian.AppendUint32(nil, uint32(width))
	padded := make([]byte, width)
	copy(padded, value)
	return append(out, padded...)
}

// metadataData is a Metaplex metadata account with the program's padding.
func metadataData(mint, name, symbol string) []byte {
	data := []byte{4}
	data = append(data, make([]byte, 32)...)
	data = append(data, solana.MustPublicKeyFromBase58(mint).Bytes()...)
	data = append(data, borshString(name, 32)...)
	data = append(data, borshString(symbol, 10)...)
	return append(data, borshString("https://example.com", 200)...)
}

// metadataNode answers the mint and metadata read for one mint.
func metadataNode(t *testing.T, mint string, mintAccount, metadataAccount any) *node {
	t.Helper()
	n := newNode(t, mainnetGenesis)
	pda, _, err := solana.FindTokenMetadataAddress(solana.MustPublicKeyFromBase58(mint))
	if err != nil {
		t.Fatalf("FindTokenMetadataAddress() error = %v", err)
	}
	n.handle("getMultipleAccounts", func(params []json.RawMessage) reply {
		if keys := requestedKeys(t, params); !slices.Equal(keys, []string{mint, pda.String()}) {
			t.Errorf("requested %v, want the mint and its metadata account", keys)
		}
		return reply{result: withContext([]any{mintAccount, metadataAccount})}
	})
	return n
}

func TestTokenMetadataReadsDecimalsAndSymbol(t *testing.T) {
	t.Parallel()

	n := metadataNode(t, usdcMint,
		account(tokenProgram, 1, mintData(6, true)),
		account(metaplex, 1, metadataData(usdcMint, "USD Coin", "USDC")),
	)
	adapter, _ := single(t, n)
	symbol, decimals, err := adapter.TokenMetadata(t.Context(), "solana-mainnet", usdcMint)
	if err != nil {
		t.Fatalf("TokenMetadata() error = %v", err)
	}
	if symbol != "USDC" || decimals != 6 {
		t.Errorf("TokenMetadata() = %q, %d, want USDC, 6", symbol, decimals)
	}
	if got := n.count("getMultipleAccounts"); got != 1 {
		t.Errorf("getMultipleAccounts calls = %d, want one for mint and metadata", got)
	}
	if got := adapter.TokenKind(); got != "spl" {
		t.Errorf("TokenKind() = %q, want spl", got)
	}
}

func TestTokenMetadataRefusesWhatIsNotAClassicMint(t *testing.T) {
	t.Parallel()

	metadata := account(metaplex, 1, metadataData(usdcMint, "USD Coin", "USDC"))
	shortSymbol := metadataData(usdcMint, "USD Coin", "USDC")
	truncated := shortSymbol[:1+32+32+4+32+2]
	// A name far past the program's 32 bytes, fully present in the data.
	longName := metadataData(usdcMint, "", "USDC")
	longName = append(append(append([]byte{}, longName[:65]...), borshString(strings.Repeat("n", 300), 300)...), longName[65+4+32:]...)
	huge := metadataData(usdcMint, "x", "y")
	binary.LittleEndian.PutUint32(huge[65:], 1<<30)
	tests := []struct {
		name     string
		mint     any
		metadata any
		wantIs   error
		wantErr  string
	}{
		{name: "Token-2022 mint", mint: account(token2022, 1, make([]byte, 234)), metadata: metadata, wantIs: solanachain.ErrToken2022},
		{name: "no account", mint: nil, metadata: metadata, wantIs: solanachain.ErrNotMint, wantErr: "no account"},
		{name: "wallet address", mint: account(systemProgram, 1, nil), metadata: metadata, wantIs: solanachain.ErrNotMint, wantErr: "program 1111"},
		{name: "token account", mint: account(tokenProgram, 1, make([]byte, 165)), metadata: metadata, wantIs: solanachain.ErrNotMint, wantErr: "165 bytes"},
		{name: "uninitialized mint", mint: account(tokenProgram, 1, mintData(6, false)), metadata: metadata, wantIs: solanachain.ErrNotMint, wantErr: "not initialized"},
		{name: "no metadata", mint: account(tokenProgram, 1, mintData(6, true)), metadata: nil, wantIs: solanachain.ErrNoSymbol},
		{name: "metadata of another program", mint: account(tokenProgram, 1, mintData(6, true)), metadata: account(tokenProgram, 1, metadataData(usdcMint, "a", "b")), wantIs: solanachain.ErrNoSymbol, wantErr: "another program"},
		{name: "metadata of another mint", mint: account(tokenProgram, 1, mintData(6, true)), metadata: account(metaplex, 1, metadataData(usdtMint, "a", "b")), wantErr: "another mint"},
		{name: "wrong metadata key", mint: account(tokenProgram, 1, mintData(6, true)), metadata: account(metaplex, 1, append([]byte{5}, metadataData(usdcMint, "a", "b")[1:]...)), wantErr: "malformed"},
		{name: "truncated symbol", mint: account(tokenProgram, 1, mintData(6, true)), metadata: account(metaplex, 1, truncated), wantErr: "malformed"},
		{name: "name past the bound", mint: account(tokenProgram, 1, mintData(6, true)), metadata: account(metaplex, 1, longName), wantErr: "malformed"},
		{name: "huge name length", mint: account(tokenProgram, 1, mintData(6, true)), metadata: account(metaplex, 1, huge), wantErr: "malformed"},
		{name: "short metadata", mint: account(tokenProgram, 1, mintData(6, true)), metadata: account(metaplex, 1, []byte{4, 1, 2}), wantErr: "malformed"},
		{name: "blank symbol", mint: account(tokenProgram, 1, mintData(6, true)), metadata: account(metaplex, 1, metadataData(usdcMint, "Name", "   ")), wantIs: solanachain.ErrNoSymbol},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			adapter, _ := single(t, metadataNode(t, usdcMint, tt.mint, tt.metadata))
			_, _, err := adapter.TokenMetadata(t.Context(), "solana-mainnet", usdcMint)
			if err == nil {
				t.Fatal("TokenMetadata() accepted it")
			}
			if !errors.Is(err, chain.ErrInvalidRequest) {
				t.Errorf("error = %v, want ErrInvalidRequest so the API answers 400", err)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error = %v, want %v", err, tt.wantIs)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestTokenMetadataTrimsThePaddingOnly(t *testing.T) {
	t.Parallel()

	n := metadataNode(t, usdtMint,
		account(tokenProgram, 1, mintData(9, true)),
		account(metaplex, 1, metadataData(usdtMint, "Tether", " USD₮ ")),
	)
	adapter, _ := single(t, n)
	symbol, decimals, err := adapter.TokenMetadata(t.Context(), "solana-mainnet", usdtMint)
	if err != nil || symbol != "USD₮" || decimals != 9 {
		t.Errorf("TokenMetadata() = %q, %d, %v, want USD₮, 9", symbol, decimals, err)
	}
}

func TestTokenMetadataRefusesAnInvalidMintWithoutAsking(t *testing.T) {
	t.Parallel()

	n := newNode(t, mainnetGenesis)
	adapter, _ := single(t, n)
	for _, contract := range []string{"", "0xdac17f958d2ee523a2206206994597c13d831ec7", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t", usdcMint + "1", "0OIl"} {
		if _, _, err := adapter.TokenMetadata(t.Context(), "solana-mainnet", contract); !errors.Is(err, chain.ErrInvalidRequest) {
			t.Errorf("TokenMetadata(%q) error = %v, want ErrInvalidRequest", contract, err)
		}
	}
	if got := n.total(); got != 0 {
		t.Errorf("RPC calls = %d, want none", got)
	}
}

func TestTokenMetadataReportsANodeFailure(t *testing.T) {
	t.Parallel()

	n := newNode(t, mainnetGenesis)
	n.handle("getMultipleAccounts", func([]json.RawMessage) reply {
		return reply{result: withContext([]any{nil})}
	})
	adapter, _ := single(t, n)
	_, _, err := adapter.TokenMetadata(t.Context(), "solana-mainnet", usdcMint)
	if err == nil || !strings.Contains(err.Error(), "answered 1 accounts for 2") {
		t.Errorf("TokenMetadata() error = %v, want a miscount error", err)
	}
	if _, _, err := adapter.TokenMetadata(t.Context(), "tron-mainnet", usdcMint); !errors.Is(err, chain.ErrInvalidRequest) {
		t.Errorf("TokenMetadata(tron) error = %v, want ErrInvalidRequest", err)
	}
}
