package account_test

import (
	"errors"
	"testing"

	"github.com/sxwebdev/walletspace/internal/account"
)

const testMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

func TestDerivedAddressesUseFamilyPaths(t *testing.T) {
	t.Parallel()

	got, err := account.DerivedAddresses(testMnemonic, "", 0)
	if err != nil {
		t.Fatalf("DerivedAddresses() error = %v", err)
	}
	if got[account.FamilyTron] != "TUEZSdKsoDHQMeZwihtdoBiN46zxhGWYdH" {
		t.Errorf("Tron address = %q", got[account.FamilyTron])
	}
	if got[account.FamilyEVM] != "0x9858EfFD232B4033E47d90003D41EC34EcaEda94" {
		t.Errorf("EVM address = %q", got[account.FamilyEVM])
	}
	if got[account.FamilyTron] == got[account.FamilyEVM] {
		t.Error("families unexpectedly produced the same address")
	}
}

func TestImportOneKeyProducesBothAddresses(t *testing.T) {
	t.Parallel()

	key, raw, err := account.ParsePrivateKey("0x0000000000000000000000000000000000000000000000000000000000000001")
	if err != nil {
		t.Fatalf("ParsePrivateKey() error = %v", err)
	}
	defer clear(raw)
	got, err := account.ImportedAddresses(key)
	if err != nil {
		t.Fatalf("ImportedAddresses() error = %v", err)
	}
	if got[account.FamilyEVM] != "0x7E5F4552091A69125d5DfCb7b8C2659029395Bdf" {
		t.Errorf("EVM address = %q", got[account.FamilyEVM])
	}
	if got[account.FamilyTron] != "TMVQGm1qAQYVdetCeGRRkTWYYrLXuHK2HC" {
		t.Errorf("Tron address = %q", got[account.FamilyTron])
	}
}

func TestParsePrivateKeyRejectsInvalidScalars(t *testing.T) {
	t.Parallel()

	tests := []string{
		"",
		"1",
		"zz00000000000000000000000000000000000000000000000000000000000000",
		"0000000000000000000000000000000000000000000000000000000000000000",
		"fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141",
	}
	for _, value := range tests {
		value := value
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			_, _, err := account.ParsePrivateKey(value)
			if !errors.Is(err, account.ErrInvalidPrivateKey) {
				t.Fatalf("ParsePrivateKey() error = %v, want ErrInvalidPrivateKey", err)
			}
		})
	}
}
