package devicesync

import (
	"bytes"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
)

// futureSkew is how far ahead of this machine's clock a record may be stamped
// before the merge says so. Timestamps decide merges, so a machine whose clock
// runs far ahead wins every one of them.
const futureSkew = time.Hour

// Report says what a merge did that the person may need to know.
type Report struct {
	// Duplicates are the newer of wallets that share a key with an older one:
	// two machines created the same wallet before they saw each other. They
	// are kept; deleting them is the person's call.
	Duplicates []string
	// KeyConflicts are key references that hold more than one key: machines
	// disagreed about what the reference means. Every variant is kept.
	KeyConflicts []string
	// FutureRecords counts snapshots, names and wallets stamped well ahead of
	// now.
	FutureRecords int
}

// Merge combines snapshots of one space into one.
//
// It is a pure function of the set of snapshots: the order they come in, and
// whether one of them is already a merge of others, does not change the
// result. That is what lets every machine merge whatever it has read so far
// and still end up where the others end up. The result is in normal form —
// times in UTC, network lists as sorted sets, keys and tombstones sorted — so
// that equal results are equal byte for byte, whatever form the inputs took.
//
// The rules, in the order they apply:
//
//   - A deletion wins. A wallet or key with a tombstone anywhere is gone
//     everywhere, whatever any snapshot says about it. Ids are never reused —
//     creating a wallet again makes a new one — so a deletion never has to be
//     undone.
//   - Of the copies of one wallet, the one updated last wins whole; copies
//     updated at the same instant are settled by their canonical encoding.
//   - Two wallets with the same key — two machines made it before they saw
//     each other — are both kept and reported. Folding them automatically
//     cannot be done the same way on every machine: whether the older one is
//     still alive depends on what each has read, and a fold decided on part of
//     the picture would delete a wallet the whole picture keeps. Two entries
//     for one address lose nothing; the person deletes one.
//   - A key reference holds the union of every snapshot's variants for it,
//     less tombstoned references. A variant is removed by its reference's
//     tombstone and by nothing else — not for being unreferenced, not for
//     disagreeing with another variant. Losing an imported key is the one
//     failure this design exists to prevent.
//   - The name updated last wins.
//
// Nothing else is decided here, on purpose. Every rule above depends only on
// sets that only grow — the copies of a wallet, the variants of a key, the
// tombstones — and that is what makes the result independent of how the
// snapshots were grouped. A rule like "leave out a wallet whose key was
// deleted" looks harmless and is not: it drops the winning copy of the wallet,
// and an older copy read in a later merge would then win in its place. Such
// judgements — whether wallets derive from the seed, which variant is a
// wallet's key — belong to the space, which checks the merged result before
// keeping any of it.
func Merge(snapshots []Snapshot, now time.Time) (Snapshot, Report) {
	var report Report
	out := Snapshot{Version: SnapshotVersion, ImportedKeys: make(map[string][]Key)}

	deletedAccounts := make(map[string]time.Time)
	deletedKeys := make(map[string]time.Time)
	accounts := make(map[string]account.Account)
	variants := make(map[string]map[string]Key)
	// Key tombstones first, so that a deleted key's variants are never copied:
	// a copy of a secret is one more thing to wipe.
	for _, snap := range snapshots {
		for _, tomb := range snap.DeletedKeys {
			deletedKeys[tomb.ID] = later(deletedKeys[tomb.ID], tomb.DeletedAt)
		}
	}
	limit := now.Add(futureSkew)
	for _, snap := range snapshots {
		if snap.WrittenAt.After(limit) {
			report.FutureRecords++
		}
		// A name set under a clock far ahead keeps winning after that clock is
		// fixed and the snapshot's own time is right again: every later rename
		// is undone. It is said, like a wallet stamped so.
		if snap.NameUpdatedAt.After(limit) {
			report.FutureRecords++
		}
		if snap.NameUpdatedAt.After(out.NameUpdatedAt) ||
			(snap.NameUpdatedAt.Equal(out.NameUpdatedAt) && snap.Name > out.Name) {
			out.Name, out.NameUpdatedAt = snap.Name, snap.NameUpdatedAt.UTC()
		}
		for _, tomb := range snap.DeletedAccounts {
			deletedAccounts[tomb.ID] = later(deletedAccounts[tomb.ID], tomb.DeletedAt)
		}
		for _, item := range snap.Accounts {
			if item.UpdatedAt.After(limit) {
				report.FutureRecords++
			}
			item = NormalizeAccount(item)
			if current, ok := accounts[item.ID]; !ok || newerAccount(item, current) {
				accounts[item.ID] = item
			}
		}
		for ref, keys := range snap.ImportedKeys {
			if _, gone := deletedKeys[ref]; gone {
				continue
			}
			for _, key := range keys {
				addVariant(variants, ref, key)
			}
		}
	}

	for id, item := range accounts {
		if _, gone := deletedAccounts[id]; gone {
			continue
		}
		out.Accounts = append(out.Accounts, item)
	}
	slices.SortFunc(out.Accounts, olderAccount)
	report.Duplicates = duplicates(out.Accounts)

	for ref, set := range variants {
		keys := make([]Key, 0, len(set))
		for _, id := range slices.Sorted(maps.Keys(set)) {
			keys = append(keys, set[id])
		}
		out.ImportedKeys[ref] = keys
		if len(keys) > 1 {
			report.KeyConflicts = append(report.KeyConflicts, ref)
		}
	}
	slices.Sort(report.KeyConflicts)
	out.DeletedAccounts = tombstones(deletedAccounts)
	out.DeletedKeys = tombstones(deletedKeys)
	return out, report
}

// addVariant adds a copy of key to its reference's set. The same key seen
// twice is one variant, imported when it was first imported anywhere.
func addVariant(variants map[string]map[string]Key, ref string, key Key) {
	set := variants[ref]
	if set == nil {
		set = make(map[string]Key)
		variants[ref] = set
	}
	id := VariantID(key.Curve, key.PrivateKey)
	if current, ok := set[id]; ok {
		if key.ImportedAt.Before(current.ImportedAt) {
			current.ImportedAt = key.ImportedAt.UTC()
			set[id] = current
		}
		return
	}
	set[id] = Key{Curve: key.Curve, PrivateKey: cloneSecret(key.PrivateKey), ImportedAt: key.ImportedAt.UTC()}
}

// cloneSecret copies key material. Every copy Merge makes ends up in its
// result, where the caller wipes it with Snapshot.Clear; a test swaps this to
// check that no other copy is made.
var cloneSecret = func(secret []byte) []byte { return slices.Clone(secret) }

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// NormalizeAccount is a wallet record in the form Merge keeps and compares:
// its times in UTC, its networks a sorted set without duplicates (nil when
// empty), its address map never nil. It is a copy; item is not changed.
func NormalizeAccount(item account.Account) account.Account {
	item = item.Clone()
	item.CreatedAt = item.CreatedAt.UTC()
	item.UpdatedAt = item.UpdatedAt.UTC()
	if item.ImportedAt != nil {
		*item.ImportedAt = item.ImportedAt.UTC()
	}
	slices.Sort(item.NetworkIDs)
	item.NetworkIDs = slices.Compact(item.NetworkIDs)
	if len(item.NetworkIDs) == 0 {
		item.NetworkIDs = nil
	}
	return item
}

// newerAccount orders the copies of one wallet: the later update wins, and
// equal instants are settled by the canonical encoding, the greater winning,
// so that every machine — in any language — settles them the same way.
// Both copies are in normal form.
func newerAccount(candidate, current account.Account) bool {
	if c := candidate.UpdatedAt.Compare(current.UpdatedAt); c != 0 {
		return c > 0
	}
	return bytes.Compare(canonical(candidate), canonical(current)) > 0
}

// duplicates lists, for every key held by several wallets, all but the oldest.
// sorted is in olderAccount order.
func duplicates(sorted []account.Account) []string {
	seen := make(map[string]struct{}, len(sorted))
	var out []string
	for _, item := range sorted {
		identity := keyIdentity(item)
		if _, dup := seen[identity]; dup {
			out = append(out, item.ID)
			continue
		}
		seen[identity] = struct{}{}
	}
	slices.Sort(out)
	return out
}

// olderAccount sorts by creation, then id: the order of the merged list, and
// which of two wallets with one key counts as the original.
func olderAccount(a, b account.Account) int {
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c
	}
	return strings.Compare(a.ID, b.ID)
}

// keyIdentity names the key a wallet stands for. A derived wallet is its
// family, derivation profile and index; an imported one is its fingerprint.
func keyIdentity(item account.Account) string {
	if item.Kind == account.KindImported {
		if item.Fingerprint == "" {
			// Nothing to compare by; the wallet only equals itself.
			return "id|" + item.ID
		}
		return "imported|" + item.Fingerprint
	}
	index := ""
	if item.Index != nil {
		index = strconv.FormatUint(uint64(*item.Index), 10)
	}
	return "derived|" + string(item.Family) + "|" + item.DerivationProfile + "|" + index
}

func tombstones(set map[string]time.Time) []Tombstone {
	out := make([]Tombstone, 0, len(set))
	for _, id := range slices.Sorted(maps.Keys(set)) {
		out = append(out, Tombstone{ID: id, DeletedAt: set[id].UTC()})
	}
	return out
}
