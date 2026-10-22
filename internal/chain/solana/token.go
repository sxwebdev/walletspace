package solana

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/sxwebdev/walletspace/internal/chain"
)

const (
	// mintSize is the length of a classic SPL mint account. A token account
	// (165 bytes) owned by the same program is not a mint.
	mintSize = 82
	// Offsets inside a mint: COption<Pubkey> mint authority (36 bytes), u64
	// supply, then the decimals and the initialized flag.
	mintDecimalsOffset    = 44
	mintInitializedOffset = 45

	// metadataKeyV1 is the first byte of a Metaplex token metadata account.
	metadataKeyV1 = 4
	// maxMetadataString bounds a borsh string length read from metadata. The
	// program stores names in 32 bytes and symbols in 10; anything far past
	// that is not a metadata account worth reading.
	maxMetadataString = 200
)

var (
	// ErrToken2022 refuses a mint of the Token-2022 program, which this wallet
	// does not support yet: its transfers can carry fees, hooks and
	// confidential amounts a classic transfer would get wrong.
	ErrToken2022 = errors.New("Token-2022 tokens are not supported")
	// ErrNotMint refuses an address that is not a classic SPL token mint.
	ErrNotMint = errors.New("the address is not an SPL token mint")
	// ErrNoSymbol refuses a mint without Metaplex metadata: there is no symbol
	// on chain to show for it.
	ErrNoSymbol = errors.New("the token has no on-chain metadata with a symbol")
)

// TokenMetadata reads a classic SPL mint and its Metaplex metadata in one
// request: decimals from the mint, the symbol from the metadata account.
func (a *Adapter) TokenMetadata(ctx context.Context, networkID, contract string) (string, uint8, error) {
	mint, err := solana.PublicKeyFromBase58(contract)
	if err != nil {
		return "", 0, fmt.Errorf("%w: invalid SPL mint address", chain.ErrInvalidRequest)
	}
	_, nodes, err := a.client(ctx, networkID)
	if err != nil {
		return "", 0, err
	}
	metadata, _, err := solana.FindTokenMetadataAddress(mint)
	if err != nil {
		// Not reachable in practice: it needs all 256 bump seeds to land on
		// the curve, which no mint has. Kept so a library change cannot turn
		// it into reading the wrong account.
		return "", 0, fmt.Errorf("derive token metadata address: %w", err)
	}
	keys := []solana.PublicKey{mint, metadata}
	var result *rpc.GetMultipleAccountsResult
	err = nodes.do(ctx, "getMultipleAccounts", func(client *rpc.Client) error {
		var callErr error
		result, callErr = client.GetMultipleAccountsWithOpts(ctx, keys, &rpc.GetMultipleAccountsOpts{
			Encoding: solana.EncodingBase64, Commitment: readCommitment,
		})
		if callErr == nil && len(result.Value) != len(keys) {
			callErr = fmt.Errorf("answered %d accounts for %d addresses", len(result.Value), len(keys))
		}
		return callErr
	})
	if err != nil {
		return "", 0, fmt.Errorf("read SPL mint: %w", err)
	}
	decimals, err := mintDecimals(result.Value[0])
	if err != nil {
		return "", 0, err
	}
	symbol, err := metadataSymbol(result.Value[1], mint)
	if err != nil {
		return "", 0, err
	}
	return symbol, decimals, nil
}

func mintDecimals(account *rpc.Account) (uint8, error) {
	if account == nil {
		return 0, fmt.Errorf("%w: %w: no account exists at this address", chain.ErrInvalidRequest, ErrNotMint)
	}
	switch account.Owner {
	case solana.TokenProgramID:
	case solana.Token2022ProgramID:
		return 0, fmt.Errorf("%w: %w", chain.ErrInvalidRequest, ErrToken2022)
	default:
		return 0, fmt.Errorf(
			"%w: %w: the account belongs to program %s", chain.ErrInvalidRequest, ErrNotMint, account.Owner,
		)
	}
	data := account.Data.GetBinary()
	if len(data) != mintSize {
		return 0, fmt.Errorf("%w: %w: account data is %d bytes", chain.ErrInvalidRequest, ErrNotMint, len(data))
	}
	if data[mintInitializedOffset] != 1 {
		return 0, fmt.Errorf("%w: %w: the mint is not initialized", chain.ErrInvalidRequest, ErrNotMint)
	}
	return data[mintDecimalsOffset], nil
}

// metadataSymbol reads the symbol from a Metaplex metadata account: key,
// update authority, mint, then the borsh strings name and symbol.
func metadataSymbol(account *rpc.Account, mint solana.PublicKey) (string, error) {
	if account == nil {
		return "", fmt.Errorf("%w: %w", chain.ErrInvalidRequest, ErrNoSymbol)
	}
	if account.Owner != solana.TokenMetadataProgramID {
		return "", fmt.Errorf("%w: %w: the metadata account belongs to another program", chain.ErrInvalidRequest, ErrNoSymbol)
	}
	data := account.Data.GetBinary()
	malformed := fmt.Errorf("%w: token metadata is malformed", chain.ErrInvalidRequest)
	if len(data) < 1+32+32 || data[0] != metadataKeyV1 {
		return "", malformed
	}
	if solana.PublicKeyFromBytes(data[33:65]) != mint {
		return "", fmt.Errorf("%w: token metadata names another mint", chain.ErrInvalidRequest)
	}
	rest := data[65:]
	readString := func() (string, bool) {
		if len(rest) < 4 {
			return "", false
		}
		size := binary.LittleEndian.Uint32(rest)
		if size > maxMetadataString || int(size) > len(rest)-4 {
			return "", false
		}
		value := string(rest[4 : 4+size])
		rest = rest[4+size:]
		return value, true
	}
	if _, ok := readString(); !ok {
		return "", malformed
	}
	symbol, ok := readString()
	if !ok {
		return "", malformed
	}
	// The program pads to a fixed width with NUL bytes.
	symbol = strings.TrimSpace(strings.TrimRight(symbol, "\x00"))
	if symbol == "" {
		return "", fmt.Errorf("%w: %w", chain.ErrInvalidRequest, ErrNoSymbol)
	}
	return symbol, nil
}
