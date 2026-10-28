package space

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sxwebdev/walletspace/internal/devicesync"
)

// A bundle is how spaces move to another machine without going through a
// cloud: one file, carried by AirDrop, a USB stick or scp, and deleted after.
//
// It holds the space files exactly as they are on disk — vault container and
// all — and nothing else, so it is as sensitive as ~/.walletspace and no more.
// Every secret in it stays under its space's password.
const (
	BundleFormat  = "walletspace-bundle"
	BundleVersion = 1
	// MaxBundleBytes bounds a bundle on the way in. A space at its account
	// ceiling, every wallet an imported key, is a few hundred kilobytes; the
	// space ceiling times that is well under this.
	MaxBundleBytes = 32 << 20
)

// ErrInvalidBundle reports a file that is not a bundle or a space backup, or
// one whose contents do not hold together.
var ErrInvalidBundle = errors.New("invalid backup file")

// Bundle is the exported file.
type Bundle struct {
	Format    string    `json:"format"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	Spaces    []File    `json:"spaces"`
}

// ExportItem names one space to export and the password that proves the
// caller may take it.
type ExportItem struct {
	ID       string
	Password string
}

// BundleImport reports what an import did with each space in the file.
type BundleImport struct {
	Imported []Summary      `json:"imported"`
	Skipped  []SkippedSpace `json:"skipped"`
}

type SkippedSpace struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Reasons a space in a bundle is not imported. They are part of the API: the
// UI words its message by them.
const (
	SkipExists = "exists"
	SkipQuota  = "quota"
)

// ExportBundle puts the named spaces into one bundle, each behind its own
// password step-up — the same one Backup makes, for the same reason: what
// leaves here can be attacked offline at leisure. A wrong password for any
// space fails the whole export, so a bundle never silently lacks a space the
// caller asked for.
func (m *Manager) ExportBundle(items []ExportItem) ([]byte, error) {
	if len(items) == 0 {
		return nil, errors.New("at least one space is required")
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if slices.Contains(ids, item.ID) {
			return nil, fmt.Errorf("space %s is listed twice", item.ID)
		}
		ids = append(ids, item.ID)
	}
	for _, item := range items {
		if err := m.confirmSpacePassword(item.ID, item.Password); err != nil {
			// Named the way the person sees it, since they have to know which
			// password was wrong.
			m.mu.RLock()
			name := m.files[item.ID].Name
			m.mu.RUnlock()
			if name == "" {
				return nil, err
			}
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	m.mu.RLock()
	files := make([]File, 0, len(ids))
	for _, id := range ids {
		file, ok := m.files[id]
		if !ok {
			m.mu.RUnlock()
			return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		files = append(files, file)
	}
	m.mu.RUnlock()
	return encodeBundle(files, m.now())
}

// confirmSpacePassword is confirmPassword under the space's own lock, as every
// step-up runs.
func (m *Manager) confirmSpacePassword(id, password string) error {
	unlockSpace := m.lockSpace(id)
	defer unlockSpace()
	return m.confirmPassword(id, password)
}

// ExportHome bundles spaces straight from a home directory, with no password.
//
// It is for the command line only: whoever runs it can read ~/.walletspace
// already, so asking them for passwords would protect nothing. The HTTP API
// must never reach it — a capability token is not the owner of the machine.
// An empty ids exports every space.
func ExportHome(home string, ids []string) ([]byte, error) {
	files, err := readHome(home)
	if err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		selected := make([]File, 0, len(ids))
		for _, id := range ids {
			index := slices.IndexFunc(files, func(file File) bool { return file.ID == id })
			if index < 0 {
				return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
			}
			selected = append(selected, files[index])
		}
		files = selected
	}
	if len(files) == 0 {
		return nil, errors.New("there are no spaces to export")
	}
	return encodeBundle(files, time.Now().UTC())
}

// readHome loads every space file the way the manager does at start, ordered
// by creation like the space list.
func readHome(home string) ([]File, error) {
	m := &Manager{home: home, files: make(map[string]File)}
	if err := m.scan(); err != nil {
		return nil, err
	}
	files := slices.Collect(maps.Values(m.files))
	slices.SortFunc(files, func(a, b File) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return files, nil
}

func encodeBundle(files []File, now time.Time) ([]byte, error) {
	data, err := json.MarshalIndent(Bundle{
		Format: BundleFormat, Version: BundleVersion, CreatedAt: now, Spaces: files,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode bundle: %w", err)
	}
	return append(data, '\n'), nil
}

// ParseBundle reads a bundle, or a single space file as Backup writes it, and
// checks every space in it as far as that can be done without passwords.
// Nothing in a file is trusted further than that: the wallet list is checked
// against the vault at the first unlock, as it is for a space on disk.
func ParseBundle(data []byte) ([]File, error) {
	if len(data) > MaxBundleBytes {
		return nil, fmt.Errorf("%w: larger than %d MiB", ErrInvalidBundle, MaxBundleBytes>>20)
	}
	var probe struct {
		Format *string `json:"format"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("%w: not JSON", ErrInvalidBundle)
	}
	// Decoded as leniently as scan reads space.json, so that anything this
	// wallet accepts on disk it also accepts in a file. A newer format is
	// caught by the bundle version, not by unknown fields.
	var files []File
	if probe.Format == nil {
		var file File
		if err := json.Unmarshal(data, &file); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
		}
		files = []File{file}
	} else {
		var bundle Bundle
		if err := json.Unmarshal(data, &bundle); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
		}
		if bundle.Format != BundleFormat {
			return nil, fmt.Errorf("%w: unknown format %q", ErrInvalidBundle, bundle.Format)
		}
		if bundle.Version != BundleVersion {
			return nil, fmt.Errorf("%w: unsupported version %d", ErrInvalidBundle, bundle.Version)
		}
		files = bundle.Spaces
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: it contains no spaces", ErrInvalidBundle)
	}
	if len(files) > maxSpaces {
		return nil, fmt.Errorf("%w: more than %d spaces", ErrInvalidBundle, maxSpaces)
	}
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		if err := validateFile(file, file.ID); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
		}
		if _, dup := seen[file.ID]; dup {
			return nil, fmt.Errorf("%w: space %s appears twice", ErrInvalidBundle, file.ID)
		}
		seen[file.ID] = struct{}{}
		if len(file.Accounts) > maxAccountsPerSpace {
			return nil, fmt.Errorf("%w: space %s has more than %d wallets", ErrInvalidBundle, file.ID, maxAccountsPerSpace)
		}
		if err := boundedIDs(file); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
		}
		// A time whose zone offset is out of range decodes but does not encode
		// again: the space could never be saved, and the import would stop half
		// way, after the spaces before it were written.
		if _, err := json.Marshal(file); err != nil {
			return nil, fmt.Errorf("%w: space %s: %v", ErrInvalidBundle, file.ID, err)
		}
		if err := file.Vault.Check(); err != nil {
			return nil, fmt.Errorf("%w: space %s: %v", ErrInvalidBundle, file.ID, err)
		}
	}
	return files, nil
}

// boundedIDs refuses ids longer than any this wallet makes. The space id
// becomes a directory name, and a long one fails the write half way through an
// import; the others go into sync snapshots, which refuse them, and the space
// would never sync.
func boundedIDs(file File) error {
	ids := []string{file.ID}
	for _, item := range file.Accounts {
		ids = append(ids, item.ID, item.KeyRef)
	}
	for _, tomb := range file.DeletedAccounts {
		ids = append(ids, tomb.ID)
	}
	for _, id := range ids {
		if len(id) > devicesync.MaxIDLength {
			return fmt.Errorf("space %s has an id longer than %d characters", file.ID, devicesync.MaxIDLength)
		}
	}
	return nil
}

// ImportBundle adds the spaces in a bundle to this machine, locked, with the
// ids they had. A space whose id is already here is skipped rather than
// replaced: the copy here may be newer, and nothing about a file says it is
// not. Spaces past the quota are skipped too, and reported.
//
// No password is asked for. An import adds a locked space that opens only with
// its own password — which a caller with the capability token can already do
// by creating one — and it never touches a space that is here.
func (m *Manager) ImportBundle(data []byte) (BundleImport, error) {
	files, err := ParseBundle(data)
	if err != nil {
		return BundleImport{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	result := BundleImport{Imported: []Summary{}, Skipped: []SkippedSpace{}}
	for _, file := range files {
		if _, exists := m.files[file.ID]; exists {
			result.Skipped = append(result.Skipped, SkippedSpace{ID: file.ID, Name: file.Name, Reason: SkipExists})
			continue
		}
		if m.spaceQuotaLocked() != nil {
			result.Skipped = append(result.Skipped, SkippedSpace{ID: file.ID, Name: file.Name, Reason: SkipQuota})
			continue
		}
		// A directory left behind by a failed write would otherwise be adopted
		// as a space at the next start.
		if _, err := os.Stat(filepath.Join(m.home, "spaces", file.ID, "space.json")); err == nil {
			result.Skipped = append(result.Skipped, SkippedSpace{ID: file.ID, Name: file.Name, Reason: SkipExists})
			continue
		}
		if err := m.saveLocked(&file); err != nil {
			return result, err
		}
		m.files[file.ID] = file
		result.Imported = append(result.Imported, m.summaryLocked(file))
	}
	return result, nil
}
