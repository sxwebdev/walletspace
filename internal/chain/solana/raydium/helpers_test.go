package raydium_test

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/gagliardetto/solana-go"
)

// fixtureAccount is an account captured from mainnet with getMultipleAccounts
// (testdata/accounts/<key>.json; source, slot and time are in each file).
type fixtureAccount struct {
	Source   string `json:"source"`
	Captured string `json:"captured"`
	Slot     uint64 `json:"slot"`
	Pubkey   string `json:"pubkey"`
	Owner    string `json:"owner"`
	Lamports uint64 `json:"lamports"`
	Data     string `json:"data"`
}

type account struct {
	key   solana.PublicKey
	owner solana.PublicKey
	data  []byte
}

func loadAccount(t *testing.T, key string) account {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "accounts", key+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var f fixtureAccount
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(f.Data)
	if err != nil {
		t.Fatal(err)
	}
	if f.Pubkey != key {
		t.Fatalf("fixture %s holds %s", key, f.Pubkey)
	}
	return account{key: solana.MustPublicKeyFromBase58(key), owner: solana.MustPublicKeyFromBase58(f.Owner), data: data}
}

// tokenAccount reads a captured SPL token account the way the token program
// lays it out: mint at 0, authority at 32, amount at 64.
func tokenAccount(t *testing.T, key string) (mint, authority solana.PublicKey, amount uint64) {
	t.Helper()
	a := loadAccount(t, key)
	if a.owner != solana.TokenProgramID || len(a.data) != 165 {
		t.Fatalf("%s is not a token account", key)
	}
	return solana.PublicKeyFromBytes(a.data[:32]), solana.PublicKeyFromBytes(a.data[32:64]), binary.LittleEndian.Uint64(a.data[64:])
}

// apiPoolKeys is the part of api-v3.raydium.io/pools/key/ids used as an
// independent statement of each pool's accounts
// (testdata/poolkeys_api_v3.json, captured 2026-10-22).
type apiPoolKeys struct {
	ProgramID string `json:"programId"`
	ID        string `json:"id"`
	MintA     struct {
		Address   string `json:"address"`
		ProgramID string `json:"programId"`
		Decimals  int    `json:"decimals"`
	} `json:"mintA"`
	MintB struct {
		Address   string `json:"address"`
		ProgramID string `json:"programId"`
		Decimals  int    `json:"decimals"`
	} `json:"mintB"`
	OpenTime string `json:"openTime"`
	Vault    struct {
		A string `json:"A"`
		B string `json:"B"`
	} `json:"vault"`
	Authority    string `json:"authority"`
	OpenOrders   string `json:"openOrders"`
	TargetOrders string `json:"targetOrders"`
	MintLp       struct {
		Address string `json:"address"`
	} `json:"mintLp"`
	MarketProgramID string `json:"marketProgramId"`
	MarketID        string `json:"marketId"`
	Config          struct {
		ID              string `json:"id"`
		Index           int    `json:"index"`
		ProtocolFeeRate uint64 `json:"protocolFeeRate"`
		TradeFeeRate    uint64 `json:"tradeFeeRate"`
		FundFeeRate     uint64 `json:"fundFeeRate"`
		CreatorFeeRate  uint64 `json:"creatorFeeRate"`
		TickSpacing     int    `json:"tickSpacing"`
		CreatePoolFee   string `json:"createPoolFee"`
	} `json:"config"`
	ObservationID   string `json:"observationId"`
	ExBitmapAccount string `json:"exBitmapAccount"`
}

func poolKeys(t *testing.T, id string) apiPoolKeys {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "poolkeys_api_v3.json"))
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Data []apiPoolKeys `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp, json.RejectUnknownMembers(false)); err != nil {
		t.Fatal(err)
	}
	for _, p := range resp.Data {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("pool %s not in poolkeys_api_v3.json", id)
	return apiPoolKeys{}
}

func key(s string) solana.PublicKey { return solana.MustPublicKeyFromBase58(s) }

var (
	wsol    = key("So11111111111111111111111111111111111111112")
	usdc    = key("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
	usdt    = key("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB")
	useless = key("Dz9mQ9NzkBcCsuGPFJ3r1bS4wgqKMHBPiVuniW8Mbonk")
)

func readJSON(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}
