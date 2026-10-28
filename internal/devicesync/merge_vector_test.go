package devicesync_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/devicesync"
)

type mergeVectors struct {
	Description string `json:"description"`
	Cases       []struct {
		Name         string                `json:"name"`
		Snapshots    []devicesync.Snapshot `json:"snapshots"`
		Merged       devicesync.Snapshot   `json:"merged"`
		Duplicates   []string              `json:"duplicates"`
		KeyConflicts []string              `json:"key_conflicts"`
	} `json:"cases"`
}

// comparable is a snapshot encoded with empty and absent lists alike, so that
// a result is compared with the vector by what it holds.
func comparable(t *testing.T, s devicesync.Snapshot) string {
	t.Helper()
	if s.Accounts == nil {
		s.Accounts = []account.Account{}
	}
	if s.DeletedAccounts == nil {
		s.DeletedAccounts = []devicesync.Tombstone{}
	}
	if s.DeletedKeys == nil {
		s.DeletedKeys = []devicesync.Tombstone{}
	}
	if s.ImportedKeys == nil {
		s.ImportedKeys = map[string][]devicesync.Key{}
	}
	return encoded(t, s)
}

// groupings are every order of the snapshots, and every way of merging a
// first part of an order before the rest — what machines that read the folder
// at different times end up computing.
func groupings(snapshots []devicesync.Snapshot) [][]devicesync.Snapshot {
	var out [][]devicesync.Snapshot
	for _, order := range permutations(snapshots) {
		out = append(out, order)
		for split := 1; split < len(order); split++ {
			head, _ := devicesync.Merge(order[:split], at(0))
			tail, _ := devicesync.Merge(order[split:], at(0))
			out = append(out,
				append([]devicesync.Snapshot{head}, order[split:]...),
				[]devicesync.Snapshot{head, tail})
		}
	}
	return out
}

func permutations(items []devicesync.Snapshot) [][]devicesync.Snapshot {
	if len(items) <= 1 {
		return [][]devicesync.Snapshot{slices.Clone(items)}
	}
	var out [][]devicesync.Snapshot
	for i := range items {
		rest := slices.Concat(items[:i], items[i+1:])
		for _, tail := range permutations(rest) {
			out = append(out, append([]devicesync.Snapshot{items[i]}, tail...))
		}
	}
	return out
}

// The merge vectors pin what a merge decides, for any implementation: ties
// of every kind, time forms, sets, escaping, key variants and tombstones.
// Their expected results were computed by a separate implementation written
// from the spec, not by this code.
func TestMergeVectors(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "merge_v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors mergeVectors
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	if len(vectors.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, tc := range vectors.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()

			for i, snap := range tc.Snapshots {
				if err := snap.Validate(); err != nil {
					t.Fatalf("snapshot %d: Validate() error = %v", i, err)
				}
			}
			want := comparable(t, tc.Merged)
			for _, inputs := range groupings(tc.Snapshots) {
				merged, report := devicesync.Merge(inputs, at(0))
				if got := comparable(t, merged); got != want {
					t.Fatalf("Merge() =\n%s\nwant\n%s", got, want)
				}
				if !slices.Equal(report.Duplicates, tc.Duplicates) && len(report.Duplicates)+len(tc.Duplicates) > 0 {
					t.Errorf("Duplicates = %v, want %v", report.Duplicates, tc.Duplicates)
				}
				if !slices.Equal(report.KeyConflicts, tc.KeyConflicts) && len(report.KeyConflicts)+len(tc.KeyConflicts) > 0 {
					t.Errorf("KeyConflicts = %v, want %v", report.KeyConflicts, tc.KeyConflicts)
				}
			}
			if again, _ := devicesync.Merge([]devicesync.Snapshot{tc.Merged}, at(0)); comparable(t, again) != want {
				t.Errorf("the vector's result is not a fixpoint of Merge")
			}
		})
	}
}
