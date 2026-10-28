package account_test

import (
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
)

func TestCloneSharesNothingWithTheOriginal(t *testing.T) {
	t.Parallel()

	imported := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	original := account.Account{
		ID: "acc_one", NetworkIDs: []string{"tron"},
		Addresses: map[account.Family]string{account.FamilyTron: "T1"},
		Index:     new(uint32(3)), ImportedAt: new(imported),
	}
	clone := original.Clone()
	clone.NetworkIDs[0] = "changed"
	clone.Addresses[account.FamilyTron] = "changed"
	*clone.Index = 9
	*clone.ImportedAt = imported.Add(time.Hour)
	if original.NetworkIDs[0] != "tron" || original.Addresses[account.FamilyTron] != "T1" ||
		*original.Index != 3 || !original.ImportedAt.Equal(imported) {
		t.Fatalf("changing the clone changed the original: %+v", original)
	}

	// An account without addresses still clones to one with a map, so the
	// copy encodes the way the space has always written it.
	if empty := (account.Account{}).Clone(); empty.Addresses == nil || empty.Index != nil || empty.ImportedAt != nil {
		t.Fatalf("Clone(empty) = %+v", empty)
	}
}

// The seed variants are the phrase variants with the PBKDF2 taken out; they
// must agree address for address.
func TestDerivingFromASeedMatchesDerivingFromThePhrase(t *testing.T) {
	t.Parallel()

	seed, err := account.Seed(testMnemonic, "TREZOR")
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range []account.Family{account.FamilyTron, account.FamilyEVM, account.FamilySolana} {
		for index := range uint32(3) {
			want, err := account.DerivedAddress(testMnemonic, "TREZOR", family, index)
			if err != nil {
				t.Fatal(err)
			}
			got, err := account.DerivedAddressFromSeed(seed, family, index)
			if err != nil || got != want {
				t.Fatalf("%s/%d from seed = %q, %v; want %q", family, index, got, err, want)
			}
		}
	}
	want, err := account.DerivedAddresses(testMnemonic, "TREZOR", 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := account.DerivedAddressesFromSeed(seed, 1)
	if err != nil || got[account.FamilyTron] != want[account.FamilyTron] || got[account.FamilyEVM] != want[account.FamilyEVM] {
		t.Fatalf("DerivedAddressesFromSeed = %v, %v; want %v", got, err, want)
	}
	if _, err := account.Seed("not a phrase", ""); err == nil {
		t.Error("Seed(invalid) succeeded")
	}
}
