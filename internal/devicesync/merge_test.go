package devicesync_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/devicesync"
)

func snapshot(edit func(*devicesync.Snapshot)) devicesync.Snapshot {
	s := devicesync.Snapshot{Version: devicesync.SnapshotVersion, ImportedKeys: map[string][]devicesync.Key{}}
	edit(&s)
	return s
}

// variant is a secp256k1 key made of one repeated byte.
func variant(b byte, imported int) devicesync.Key {
	return devicesync.Key{Curve: "secp256k1", PrivateKey: bytes.Repeat([]byte{b}, devicesync.KeyLength), ImportedAt: at(imported)}
}

// key is a reference holding one variant.
func key(b byte, imported int) []devicesync.Key {
	return []devicesync.Key{variant(b, imported)}
}

func variantIDs(keys []devicesync.Key) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = devicesync.VariantID(k.Curve, k.PrivateKey)
	}
	return out
}

func ids(items []account.Account) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.ID
	}
	return out
}

func tombIDs(items []devicesync.Tombstone) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = item.ID
	}
	return out
}

func TestMergeKeepsTheLatestCopyOfAWallet(t *testing.T) {
	t.Parallel()

	old := derived("acc_one", 0, 1)
	renamed := derived("acc_one", 0, 5)
	renamed.Label = "renamed later"
	for _, order := range [][]devicesync.Snapshot{
		{snapshot(func(s *devicesync.Snapshot) { s.Accounts = []account.Account{old} }),
			snapshot(func(s *devicesync.Snapshot) { s.Accounts = []account.Account{renamed} })},
		{snapshot(func(s *devicesync.Snapshot) { s.Accounts = []account.Account{renamed} }),
			snapshot(func(s *devicesync.Snapshot) { s.Accounts = []account.Account{old} })},
	} {
		merged, _ := devicesync.Merge(order, at(10))
		if len(merged.Accounts) != 1 || merged.Accounts[0].Label != "renamed later" {
			t.Fatalf("merged wallets = %+v, want the later rename", merged.Accounts)
		}
	}
}

func TestMergeUnitesWalletsFromSeveralMachines(t *testing.T) {
	t.Parallel()

	a := snapshot(func(s *devicesync.Snapshot) {
		s.Accounts = []account.Account{derived("acc_one", 0, 1)}
	})
	b := snapshot(func(s *devicesync.Snapshot) {
		s.Accounts = []account.Account{imported("acc_two", "key_two", "fp2", 2)}
		s.ImportedKeys["key_two"] = key(2, 0)
	})
	merged, report := devicesync.Merge([]devicesync.Snapshot{a, b}, at(10))
	if got := ids(merged.Accounts); !slices.Equal(got, []string{"acc_one", "acc_two"}) {
		t.Fatalf("merged wallets = %v", got)
	}
	if _, ok := merged.ImportedKeys["key_two"]; !ok {
		t.Fatal("the imported key from the other machine is missing")
	}
	if len(report.Duplicates) != 0 || len(report.KeyConflicts) != 0 {
		t.Errorf("report = %+v, want nothing to report", report)
	}
}

func TestADeletionWinsEverywhere(t *testing.T) {
	t.Parallel()

	// The wallet was edited after the deletion on another machine; the
	// deletion still wins, because the id is never coming back.
	edited := derived("acc_one", 0, 9)
	deleted := snapshot(func(s *devicesync.Snapshot) {
		s.DeletedAccounts = []devicesync.Tombstone{{ID: "acc_one", DeletedAt: at(3)}}
	})
	kept := snapshot(func(s *devicesync.Snapshot) { s.Accounts = []account.Account{edited} })
	merged, _ := devicesync.Merge([]devicesync.Snapshot{kept, deleted}, at(10))
	if len(merged.Accounts) != 0 {
		t.Fatalf("merged wallets = %v, want the deleted one gone", ids(merged.Accounts))
	}
	if got := tombIDs(merged.DeletedAccounts); !slices.Equal(got, []string{"acc_one"}) {
		t.Fatalf("tombstones = %v, want the deletion carried on", got)
	}

	// An old snapshot read again later does not bring it back.
	again, _ := devicesync.Merge([]devicesync.Snapshot{merged, kept}, at(20))
	if len(again.Accounts) != 0 {
		t.Fatalf("a replayed snapshot resurrected %v", ids(again.Accounts))
	}
}

func TestAKeyIsRemovedOnlyByItsTombstone(t *testing.T) {
	t.Parallel()

	withKey := snapshot(func(s *devicesync.Snapshot) {
		s.ImportedKeys["key_kept"] = key(1, 0)
		s.ImportedKeys["key_orphan"] = key(2, 0)
	})
	// No wallet references either key in the merged result, and another
	// machine has never heard of them: they stay.
	empty := snapshot(func(s *devicesync.Snapshot) {})
	merged, _ := devicesync.Merge([]devicesync.Snapshot{withKey, empty}, at(10))
	if len(merged.ImportedKeys) != 2 {
		t.Fatalf("keys = %v, want both kept", merged.ImportedKeys)
	}

	deleted := snapshot(func(s *devicesync.Snapshot) {
		s.DeletedKeys = []devicesync.Tombstone{{ID: "key_orphan", DeletedAt: at(4)}}
	})
	merged, _ = devicesync.Merge([]devicesync.Snapshot{withKey, deleted}, at(10))
	if _, ok := merged.ImportedKeys["key_orphan"]; ok {
		t.Error("a tombstoned key survived")
	}
	if _, ok := merged.ImportedKeys["key_kept"]; !ok {
		t.Error("a key without a tombstone was removed")
	}
}

// A key tombstone takes the key and nothing else. The wallet record stays in
// the merge; the space, which checks every imported wallet against its key,
// is what leaves it out.
func TestAKeyTombstoneTakesOnlyTheKey(t *testing.T) {
	t.Parallel()

	holder := snapshot(func(s *devicesync.Snapshot) {
		s.Accounts = []account.Account{imported("acc_two", "key_two", "fp2", 2)}
		s.ImportedKeys["key_two"] = key(2, 0)
	})
	deleter := snapshot(func(s *devicesync.Snapshot) {
		s.DeletedKeys = []devicesync.Tombstone{{ID: "key_two", DeletedAt: at(4)}}
	})
	merged, _ := devicesync.Merge([]devicesync.Snapshot{holder, deleter}, at(10))
	if len(merged.ImportedKeys) != 0 {
		t.Fatalf("keys = %v, want the tombstoned key gone", merged.ImportedKeys)
	}
	if got := ids(merged.Accounts); !slices.Equal(got, []string{"acc_two"}) {
		t.Fatalf("wallets = %v, want the record kept for the space to judge", got)
	}
}

func TestTheSameWalletMadeTwiceIsReported(t *testing.T) {
	t.Parallel()

	// Two machines derived index 0 before either saw the other.
	first := derived("acc_bbb", 0, 1)
	second := derived("acc_aaa", 0, 2)
	second.CreatedAt = at(1)
	// And imported the same key under two references.
	keyA := imported("acc_ka", "key_a", "same-fingerprint", 1)
	keyB := imported("acc_kb", "key_b", "same-fingerprint", 1)
	keyB.CreatedAt = at(2)

	a := snapshot(func(s *devicesync.Snapshot) {
		s.Accounts = []account.Account{first, keyA}
		s.ImportedKeys["key_a"] = key(9, 0)
	})
	b := snapshot(func(s *devicesync.Snapshot) {
		s.Accounts = []account.Account{second, keyB}
		s.ImportedKeys["key_b"] = key(9, 0)
	})
	merged, report := devicesync.Merge([]devicesync.Snapshot{b, a}, at(10))
	// Both are kept — nothing is deleted on the merge's own judgement — and
	// the newer of each pair is reported.
	if got := ids(merged.Accounts); !slices.Equal(got, []string{"acc_bbb", "acc_ka", "acc_aaa", "acc_kb"}) {
		t.Fatalf("merged wallets = %v, want all four in creation order", got)
	}
	if !slices.Equal(report.Duplicates, []string{"acc_aaa", "acc_kb"}) {
		t.Errorf("report.Duplicates = %v, want the newer of each pair", report.Duplicates)
	}
	if len(merged.DeletedAccounts) != 0 {
		t.Errorf("tombstones = %v, want none", tombIDs(merged.DeletedAccounts))
	}
	if len(merged.ImportedKeys) != 2 {
		t.Errorf("keys = %v, want both references kept", merged.ImportedKeys)
	}

	// Different indexes, families or profiles are different wallets.
	other := derived("acc_ccc", 1, 1)
	evm := derived("acc_ddd", 0, 1)
	evm.Family = account.FamilyEVM
	distinct, report := devicesync.Merge([]devicesync.Snapshot{snapshot(func(s *devicesync.Snapshot) {
		s.Accounts = []account.Account{first, other, evm}
	})}, at(10))
	if len(distinct.Accounts) != 3 || len(report.Duplicates) != 0 {
		t.Fatalf("distinct wallets folded: %v, %+v", ids(distinct.Accounts), report)
	}
}

// Two machines hold different keys under one reference. Both are kept, in
// the same order whichever was read first, and the reference is reported:
// keeping one of them would lose the other's secret.
func TestDisagreeingKeysAreAllKept(t *testing.T) {
	t.Parallel()

	a := snapshot(func(s *devicesync.Snapshot) { s.ImportedKeys["key_x"] = key(1, 5) })
	b := snapshot(func(s *devicesync.Snapshot) { s.ImportedKeys["key_x"] = key(2, 3) })
	want := variantIDs([]devicesync.Key{variant(1, 5), variant(2, 3)})
	slices.Sort(want)
	for _, order := range [][]devicesync.Snapshot{{a, b}, {b, a}} {
		merged, report := devicesync.Merge(order, at(10))
		if got := variantIDs(merged.ImportedKeys["key_x"]); !slices.Equal(got, want) {
			t.Fatalf("key_x variants = %v, want both sorted by variant id %v", got, want)
		}
		if !slices.Equal(report.KeyConflicts, []string{"key_x"}) {
			t.Errorf("report.KeyConflicts = %v", report.KeyConflicts)
		}
	}
}

// The same key read from several machines is one variant, imported when it
// was first imported anywhere — whichever copy came first, and whatever zone
// its time was written in.
func TestTheSameKeyTwiceIsOneVariant(t *testing.T) {
	t.Parallel()

	late := snapshot(func(s *devicesync.Snapshot) { s.ImportedKeys["key_x"] = key(1, 5) })
	early := snapshot(func(s *devicesync.Snapshot) {
		v := variant(1, 2)
		v.ImportedAt = v.ImportedAt.In(time.FixedZone("", -5*3600))
		s.ImportedKeys["key_x"] = []devicesync.Key{v}
	})
	for _, order := range [][]devicesync.Snapshot{{late, early}, {early, late}} {
		merged, report := devicesync.Merge(order, at(10))
		got := merged.ImportedKeys["key_x"]
		if len(got) != 1 {
			t.Fatalf("key_x = %d variants, want one", len(got))
		}
		if !got[0].ImportedAt.Equal(at(2)) || got[0].ImportedAt.Location() != time.UTC {
			t.Errorf("imported_at = %v, want the earliest, in UTC", got[0].ImportedAt)
		}
		if len(report.KeyConflicts) != 0 {
			t.Errorf("report.KeyConflicts = %v for one key", report.KeyConflicts)
		}
	}
}

func TestTheLatestNameWins(t *testing.T) {
	t.Parallel()

	a := snapshot(func(s *devicesync.Snapshot) { s.Name, s.NameUpdatedAt = "Old", at(1) })
	b := snapshot(func(s *devicesync.Snapshot) { s.Name, s.NameUpdatedAt = "New", at(2) })
	for _, order := range [][]devicesync.Snapshot{{a, b}, {b, a}} {
		if merged, _ := devicesync.Merge(order, at(10)); merged.Name != "New" {
			t.Fatalf("name = %q, want New", merged.Name)
		}
	}
}

func TestMergeReportsTheFuture(t *testing.T) {
	t.Parallel()

	ahead := snapshot(func(s *devicesync.Snapshot) {
		s.WrittenAt = at(600)
		s.Accounts = []account.Account{derived("acc_one", 0, 600)}
	})
	_, report := devicesync.Merge([]devicesync.Snapshot{ahead}, at(10))
	if report.FutureRecords != 2 {
		t.Fatalf("FutureRecords = %d, want the snapshot and the wallet", report.FutureRecords)
	}
	_, report = devicesync.Merge([]devicesync.Snapshot{ahead}, at(600))
	if report.FutureRecords != 0 {
		t.Fatalf("FutureRecords = %d with a matching clock", report.FutureRecords)
	}
}

func TestMergeDoesNotShareMemoryWithItsInputs(t *testing.T) {
	t.Parallel()

	input := snapshot(func(s *devicesync.Snapshot) {
		s.Accounts = []account.Account{imported("acc_two", "key_two", "fp2", 2)}
		s.ImportedKeys["key_two"] = key(2, 0)
	})
	merged, _ := devicesync.Merge([]devicesync.Snapshot{input}, at(10))
	input.Clear()
	input.Accounts[0].NetworkIDs[0] = "changed"
	*input.Accounts[0].ImportedAt = at(99)
	if got := merged.ImportedKeys["key_two"][0].PrivateKey; got[0] != 2 {
		t.Fatal("clearing the input wiped the merged key")
	}
	if merged.Accounts[0].NetworkIDs[0] != "eth-sepolia" || !merged.Accounts[0].ImportedAt.Equal(at(0)) {
		t.Fatal("editing the input edited the merged wallet")
	}
}

// zones are the forms one instant takes in a file from another machine.
var zones = []*time.Location{time.UTC, time.FixedZone("", 3600), time.FixedZone("", -(5*3600 + 1800))}

// randomTime is one of a few instants, written in one of a few zones.
func randomTime(r *rand.Rand, minutes int) time.Time {
	return at(r.IntN(minutes)).In(zones[r.IntN(len(zones))])
}

// randomSnapshot draws from a small pool of ids, keys and times, so that
// random snapshots collide on everything the rules have to settle: the same
// wallet with different times, deletions of wallets others still have, two
// wallets with one key, several keys under one reference, the same key with
// different import times — and one instant written in different zones, one
// network list in different orders.
func randomSnapshot(r *rand.Rand) devicesync.Snapshot {
	s := devicesync.Snapshot{
		Version: devicesync.SnapshotVersion, ImportedKeys: map[string][]devicesync.Key{},
		Name: fmt.Sprintf("name-%d", r.IntN(3)), NameUpdatedAt: randomTime(r, 4), WrittenAt: randomTime(r, 5),
	}
	addKey := func(ref string) {
		k := variant(byte(r.IntN(3)), 0)
		if r.IntN(3) == 0 {
			k.Curve = "ed25519"
		}
		k.ImportedAt = randomTime(r, 3)
		if !slices.ContainsFunc(s.ImportedKeys[ref], func(have devicesync.Key) bool {
			return have.Curve == k.Curve && bytes.Equal(have.PrivateKey, k.PrivateKey)
		}) {
			s.ImportedKeys[ref] = append(s.ImportedKeys[ref], k)
		}
	}
	used := map[string]bool{}
	for range r.IntN(6) {
		id := fmt.Sprintf("acc_%d", r.IntN(6))
		if used[id] {
			continue
		}
		used[id] = true
		var item account.Account
		if r.IntN(2) == 0 {
			item = derived(id, uint32(r.IntN(3)), r.IntN(4))
		} else {
			ref := fmt.Sprintf("key_%d", r.IntN(4))
			item = imported(id, ref, fmt.Sprintf("fp%d", r.IntN(3)), r.IntN(4))
			item.ImportedAt = new(randomTime(r, 2))
			addKey(ref)
		}
		item.CreatedAt = randomTime(r, 3)
		item.UpdatedAt = randomTime(r, 4)
		item.Label = fmt.Sprintf("label-%d", r.IntN(3))
		item.NetworkIDs = [][]string{{"a", "b"}, {"b", "a"}, {"a", "a", "b"}, {"a"}, nil}[r.IntN(5)]
		s.Accounts = append(s.Accounts, item)
	}
	for range r.IntN(3) {
		addKey(fmt.Sprintf("key_%d", r.IntN(4)))
	}
	for range r.IntN(3) {
		s.DeletedAccounts = append(s.DeletedAccounts, devicesync.Tombstone{ID: fmt.Sprintf("acc_%d", r.IntN(6)), DeletedAt: randomTime(r, 4)})
	}
	for range r.IntN(2) {
		s.DeletedKeys = append(s.DeletedKeys, devicesync.Tombstone{ID: fmt.Sprintf("key_%d", r.IntN(4)), DeletedAt: randomTime(r, 4)})
	}
	return s
}

func encoded(t *testing.T, s devicesync.Snapshot) string {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The properties that let every machine merge whatever it has read so far
// and still end where the others end: the order of the inputs does not
// matter, merging a merge with more input is merging everything, and merging
// a result again changes nothing — byte for byte, whatever zones and orders
// the inputs were written in.
func TestMergeConverges(t *testing.T) {
	t.Parallel()

	now := at(10)
	for seed := range uint64(5) {
		r := rand.New(rand.NewPCG(seed, 2))
		for round := range 2000 {
			inputs := make([]devicesync.Snapshot, 1+r.IntN(4))
			for i := range inputs {
				inputs[i] = randomSnapshot(r)
			}
			all, _ := devicesync.Merge(inputs, now)
			want := encoded(t, all)

			shuffled := slices.Clone(inputs)
			r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			if got, _ := devicesync.Merge(shuffled, now); encoded(t, got) != want {
				t.Fatalf("round %d: the order of the inputs changed the result\nshuffled: %s\nonce:     %s",
					round, encoded(t, got), want)
			}

			split := r.IntN(len(inputs) + 1)
			head, _ := devicesync.Merge(inputs[:split], now)
			if got, _ := devicesync.Merge(append([]devicesync.Snapshot{head}, inputs[split:]...), now); encoded(t, got) != want {
				t.Fatalf("round %d: merging in two steps differs from merging at once\nsteps: %s\nonce:  %s",
					round, encoded(t, got), want)
			}

			if again, _ := devicesync.Merge([]devicesync.Snapshot{all, all}, now); encoded(t, again) != want {
				t.Fatalf("round %d: merging a result with itself changed it", round)
			}

			checkNothingLost(t, round, inputs, all)
		}
	}
}

// checkNothingLost: nothing that survives was tombstoned; every variant of
// every reference without a tombstone is there, once, in variant id order,
// imported at the earliest time any input gives it.
func checkNothingLost(t *testing.T, round int, inputs []devicesync.Snapshot, all devicesync.Snapshot) {
	t.Helper()
	deadKeys := map[string]bool{}
	for _, tomb := range all.DeletedKeys {
		deadKeys[tomb.ID] = true
	}
	earliest := map[string]time.Time{}
	for _, in := range inputs {
		for ref, keys := range in.ImportedKeys {
			for _, k := range keys {
				id := ref + "/" + devicesync.VariantID(k.Curve, k.PrivateKey)
				if first, ok := earliest[id]; !ok || k.ImportedAt.Before(first) {
					earliest[id] = k.ImportedAt
				}
			}
		}
	}
	kept := 0
	for ref, keys := range all.ImportedKeys {
		if deadKeys[ref] {
			t.Fatalf("round %d: tombstoned key %s survived", round, ref)
		}
		ids := variantIDs(keys)
		if !slices.IsSorted(ids) || len(slices.Compact(slices.Clone(ids))) != len(ids) {
			t.Fatalf("round %d: variants of %s are not a sorted set: %v", round, ref, ids)
		}
		for i, k := range keys {
			first, ok := earliest[ref+"/"+ids[i]]
			if !ok || !k.ImportedAt.Equal(first) {
				t.Fatalf("round %d: variant of %s imported at %v, want the earliest %v", round, ref, k.ImportedAt, first)
			}
			kept++
		}
	}
	for id := range earliest {
		ref, _, _ := strings.Cut(id, "/")
		if !deadKeys[ref] {
			kept--
		}
	}
	if kept != 0 {
		t.Fatalf("round %d: %d key variants vanished without a tombstone", round, -kept)
	}
	dead := map[string]bool{}
	for _, tomb := range all.DeletedAccounts {
		dead[tomb.ID] = true
	}
	for _, item := range all.Accounts {
		if dead[item.ID] {
			t.Fatalf("round %d: tombstoned wallet %s survived", round, item.ID)
		}
	}
}

// A time with a zone offset out of range decodes from JSON but does not encode
// again in Go. Two copies of a wallet updated at the same instant — one of them
// so written, on disk here or by another client — tie, and the tie is settled
// by the canonical encoding, which writes every time in UTC and so has a form
// for this one too. The merge must neither panic (it runs in the sync
// goroutines, and a panic there takes the wallet down on every unlock) nor
// return something that cannot be written, and both orders must agree. A
// snapshot carrying such a record is refused as a whole before it gets here.
func TestARecordThatDoesNotEncodeIsSettledInUTC(t *testing.T) {
	t.Parallel()

	good := derived("acc_one", 0, 2)
	bad := good.Clone()
	bad.Label = "rewritten"
	bad.UpdatedAt = good.UpdatedAt.In(time.FixedZone("", 24*3600))
	if !bad.UpdatedAt.Equal(good.UpdatedAt) {
		t.Fatal("the case needs the same instant")
	}
	if _, err := json.Marshal(bad); err == nil {
		t.Fatal("the case needs a record that does not encode")
	}
	var results []string
	for _, order := range [][]account.Account{{good, bad}, {bad, good}} {
		snaps := []devicesync.Snapshot{
			snapshot(func(s *devicesync.Snapshot) { s.Accounts = order[:1] }),
			snapshot(func(s *devicesync.Snapshot) { s.Accounts = order[1:] }),
		}
		merged, _ := devicesync.Merge(snaps, at(60))
		if len(merged.Accounts) != 1 {
			t.Fatalf("merged = %+v, want one wallet", merged.Accounts)
		}
		results = append(results, encoded(t, merged))
	}
	if results[0] != results[1] {
		t.Errorf("the order chose the copy:\n%s\n%s", results[0], results[1])
	}

	refused := snapshot(func(s *devicesync.Snapshot) { s.Accounts = []account.Account{bad} })
	if err := refused.Validate(); !errors.Is(err, devicesync.ErrInvalidSnapshot) {
		t.Errorf("Validate() error = %v, want ErrInvalidSnapshot", err)
	}
}

// A name set on a machine whose clock ran a year ahead wins every rename for a
// year — after that clock is fixed, under a snapshot whose own time is right.
// It is reported like a wallet stamped so.
func TestANameFromTheFutureIsReported(t *testing.T) {
	t.Parallel()

	ahead := snapshot(func(s *devicesync.Snapshot) {
		s.WrittenAt, s.Name, s.NameUpdatedAt = at(10), "Pinned", at(365*24*60)
	})
	here := snapshot(func(s *devicesync.Snapshot) {
		s.WrittenAt, s.Name, s.NameUpdatedAt = at(11), "Renamed", at(11)
	})
	merged, report := devicesync.Merge([]devicesync.Snapshot{ahead, here}, at(12))
	if merged.Name != "Pinned" {
		t.Fatalf("name = %q; the case needs the far-future name to win", merged.Name)
	}
	if report.FutureRecords != 1 {
		t.Errorf("FutureRecords = %d, want the name reported", report.FutureRecords)
	}
}

// A wallet's times are kept in UTC, whatever zone the copy that won was
// written in: the import time too, not only the ones ties are compared by.
func TestAMergedWalletHasItsTimesInUTC(t *testing.T) {
	t.Parallel()

	zone := time.FixedZone("", 5*3600)
	item := imported("acc_two", "key_two", "fp2", 3)
	item.CreatedAt, item.UpdatedAt = item.CreatedAt.In(zone), item.UpdatedAt.In(zone)
	item.ImportedAt = new(item.ImportedAt.In(zone))
	merged, _ := devicesync.Merge([]devicesync.Snapshot{snapshot(func(s *devicesync.Snapshot) {
		s.Accounts = []account.Account{item}
	})}, at(10))
	got := merged.Accounts[0]
	for name, value := range map[string]time.Time{"created_at": got.CreatedAt, "updated_at": got.UpdatedAt, "imported_at": *got.ImportedAt} {
		if value.Location() != time.UTC {
			t.Errorf("%s = %v, want UTC", name, value)
		}
	}
}

// Every copy of key material the merge makes is in its result — where
// Snapshot.Clear reaches it — and no other: not of a key whose reference is
// deleted, not of a second copy of a key it already has.
//
// Not parallel: it swaps the package's copier.
func TestTheMergeCopiesNoSecretItDoesNotReturn(t *testing.T) {
	var copies [][]byte
	restore := devicesync.RecordSecretCopies(&copies)
	defer restore()

	inputs := []devicesync.Snapshot{
		snapshot(func(s *devicesync.Snapshot) {
			s.ImportedKeys["key_kept"] = key(1, 2)
			s.ImportedKeys["key_gone"] = key(2, 0)
		}),
		snapshot(func(s *devicesync.Snapshot) {
			s.ImportedKeys["key_kept"] = key(1, 1)
			s.ImportedKeys["key_gone"] = key(3, 0)
			s.DeletedKeys = []devicesync.Tombstone{{ID: "key_gone", DeletedAt: at(4)}}
		}),
	}
	merged, _ := devicesync.Merge(inputs, at(10))
	restore()
	var returned [][]byte
	for _, keys := range merged.ImportedKeys {
		for _, k := range keys {
			returned = append(returned, k.PrivateKey)
		}
	}
	for _, c := range copies {
		if !slices.ContainsFunc(returned, func(r []byte) bool { return &r[0] == &c[0] }) {
			t.Errorf("the merge kept a copy of key material it does not return: %x", c)
		}
	}
	if len(returned) != 1 {
		t.Errorf("returned %d keys, want the one kept", len(returned))
	}
}
