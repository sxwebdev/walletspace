package solana

import (
	"fmt"
	"testing"
)

// The book is bounded; past the bound an old record makes room, and a record
// that is only updated takes none.
func TestExpiryBookIsBounded(t *testing.T) {
	t.Parallel()

	book := newExpiryBook()
	for i := range maxExpiryRecords {
		book.record("solana-mainnet", fmt.Sprint(i), uint64(i))
	}
	book.record("solana-mainnet", "0", 7)
	if got := len(book.items); got != maxExpiryRecords {
		t.Fatalf("records after an update = %d, want %d", got, maxExpiryRecords)
	}
	if value, ok := book.lookup("solana-mainnet", "0"); !ok || value != 7 {
		t.Errorf("updated record = %d, %v", value, ok)
	}
	book.record("solana-mainnet", "new", 1)
	if got := len(book.items); got != maxExpiryRecords {
		t.Errorf("records past the bound = %d, want %d", got, maxExpiryRecords)
	}
	if _, ok := book.lookup("solana-mainnet", "new"); !ok {
		t.Error("the newest record was dropped")
	}
}
