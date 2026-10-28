package devicesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sxwebdev/walletspace/internal/storage"
)

// Entry is one machine's file in a space's folder.
type Entry struct {
	DeviceTag string
	Modified  time.Time
	// Evicted is a file the provider knows of but has not downloaded: iCloud
	// leaves a ".<name>.icloud" placeholder in its place. It is not an error;
	// it is "not here yet", and reading it finds nothing. A store that is also
	// a Downloader can be asked to bring it.
	Evicted bool
	// Dataless is a file a File Provider folder (macOS 14 and later: iCloud
	// Drive, Dropbox, Google Drive, OneDrive) keeps under its own name with
	// no data behind it. Unlike an evicted file it can be read: reading makes
	// the provider fetch it, and blocks until the fetch ends. A Downloader can
	// be asked for it ahead of the read.
	Dataless bool
}

// Store is where sync files live: opaque blobs addressed by a space tag and a
// device tag. A shared folder is one store; a relay of our own would be
// another, and would see exactly what the folder's provider sees.
type Store interface {
	// Spaces lists the space tags present.
	Spaces(ctx context.Context) ([]string, error)
	// List lists the files of one space. An unknown space has none.
	List(ctx context.Context, spaceTag string) ([]Entry, error)
	Get(ctx context.Context, spaceTag, deviceTag string) ([]byte, error)
	Put(ctx context.Context, spaceTag, deviceTag string, blob []byte) error
}

// ErrNotDownloaded reports a file the provider has not brought down yet.
var ErrNotDownloaded = errors.New("sync file is not downloaded yet")

const (
	folderVersion = "v1"
	fileSuffix    = ".wsync"
)

// FolderStore keeps sync files in a directory that something else — iCloud
// Drive, Dropbox, Syncthing — copies between machines:
//
//	<root>/v1/<space tag>/<device tag>.wsync
//
// It writes only its own files, whole and atomically, and never deletes or
// renames anything. Everything else in the folder is ignored.
type FolderStore struct {
	root string
	// reads and writes hold a slot for every call in flight, abandoned ones
	// included (see whileWanted). A folder that stops answering then costs a
	// few stuck threads, not one more per pass, and a write given up on can
	// never land after a newer one. The write slot belongs to the root, not
	// to the store: the wallet makes a new store whenever the settings are
	// saved, and a write abandoned through the old one must still keep out
	// every write through the new one until it has landed.
	reads  chan struct{}
	writes chan struct{}

	// What the store does to the file system and the provider, replaceable in
	// tests.
	readFile        func(path string) ([]byte, error)
	writeFile       func(path string, blob []byte) error
	dataless        func(fs.FileInfo) bool
	run             func(ctx context.Context, name string, args ...string) ([]byte, error)
	downloadTimeout time.Duration
	slotWait        time.Duration
}

// readsInFlight bounds the reads one store waits on at once.
const readsInFlight = 4

// ErrNotAnswering reports a folder whose earlier calls have not returned yet.
var ErrNotAnswering = errors.New("the sync folder is not answering")

// slotWait is how long a call waits for a slot held by an earlier one. Every
// space of the installation writes into one folder, so a write often finds
// another one under way; a healthy folder ends it well within this, and one
// that has not by then is not answering.
const slotWait = 5 * time.Second

// NewFolderStore returns a store rooted at an absolute path. The directory
// must exist when something is written: it belongs to whatever copies it, and
// only what is under it is made here.
func NewFolderStore(root string) (*FolderStore, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("sync folder %q is not an absolute path", root)
	}
	root = filepath.Clean(root)
	return &FolderStore{
		root:            root,
		reads:           make(chan struct{}, readsInFlight),
		writes:          writeSlot(root),
		readFile:        read,
		writeFile:       storage.AtomicWrite,
		dataless:        isDataless,
		run:             runCommand,
		downloadTimeout: downloadTimeout,
		slotWait:        slotWait,
	}, nil
}

// writeSlots holds the one write slot of each folder root, for the whole
// process. The key is the cleaned path as given: resolving symbolic links
// would mean asking the folder, which may be the one that is stuck.
var writeSlots struct {
	sync.Mutex
	byRoot map[string]chan struct{}
}

func writeSlot(root string) chan struct{} {
	writeSlots.Lock()
	defer writeSlots.Unlock()
	if writeSlots.byRoot == nil {
		writeSlots.byRoot = make(map[string]chan struct{})
	}
	slot, ok := writeSlots.byRoot[root]
	if !ok {
		slot = make(chan struct{}, 1)
		writeSlots.byRoot[root] = slot
	}
	return slot
}

func (f *FolderStore) Root() string { return f.root }

func (f *FolderStore) spaceDir(spaceTag string) (string, error) {
	if !ValidTag(spaceTag) {
		return "", errors.New("invalid sync tag")
	}
	return filepath.Join(f.root, folderVersion, spaceTag), nil
}

// whileWanted runs a file-system call and gives up waiting for it when ctx
// ends. The call itself cannot be interrupted: reading a file the provider has
// not downloaded starts the download and blocks until it is done, and a
// volume that stopped answering blocks for good. Checking ctx only before the
// call would leave whoever cancelled — turning sync off, quitting — waiting
// on the folder anyway. The abandoned call finishes, or not, on its own,
// holding one of slots until it does. A call that finds every slot held
// waits up to wait for one to free — the holders may just be busy — and past
// that the folder is not answering and nothing more is asked of it.
func whileWanted[T any](ctx context.Context, slots chan struct{}, wait time.Duration, call func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	select {
	case slots <- struct{}{}:
	default:
		if wait <= 0 {
			return zero, ErrNotAnswering
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-timer.C:
			return zero, ErrNotAnswering
		}
	}
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		value, err := call()
		// Free the slot before answering: a caller that makes its next call
		// as soon as this one returns must find the slot free, or one write
		// after another reads as a folder that is not answering.
		<-slots
		done <- result{value, err}
	}()
	select {
	case r := <-done:
		return r.value, r.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

func (f *FolderStore) Spaces(ctx context.Context) ([]string, error) {
	return whileWanted(ctx, f.reads, f.slotWait, f.spaces)
}

func (f *FolderStore) spaces() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(f.root, folderVersion))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() && ValidTag(entry.Name()) {
			out = append(out, entry.Name())
		}
	}
	return out, nil
}

func (f *FolderStore) List(ctx context.Context, spaceTag string) ([]Entry, error) {
	dir, err := f.spaceDir(spaceTag)
	if err != nil {
		return nil, err
	}
	return whileWanted(ctx, f.reads, f.slotWait, func() ([]Entry, error) { return list(dir, f.dataless) })
}

func list(dir string, dataless func(fs.FileInfo) bool) ([]Entry, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, entry := range entries {
		name := entry.Name()
		if placeholder, ok := strings.CutPrefix(name, "."); ok {
			if base, ok := strings.CutSuffix(placeholder, fileSuffix+".icloud"); ok && ValidTag(base) {
				out = append(out, Entry{DeviceTag: base, Evicted: true})
			}
			continue
		}
		base, ok := strings.CutSuffix(name, fileSuffix)
		if !ok || !ValidTag(base) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			// Gone between the listing and the stat; the next pass sees it.
			continue
		}
		out = append(out, Entry{DeviceTag: base, Modified: info.ModTime(), Dataless: dataless(info)})
	}
	return out, nil
}

func (f *FolderStore) path(spaceTag, deviceTag string) (string, error) {
	dir, err := f.spaceDir(spaceTag)
	if err != nil {
		return "", err
	}
	if !ValidTag(deviceTag) {
		return "", errors.New("invalid sync tag")
	}
	return filepath.Join(dir, deviceTag+fileSuffix), nil
}

func (f *FolderStore) Get(ctx context.Context, spaceTag, deviceTag string) ([]byte, error) {
	path, err := f.path(spaceTag, deviceTag)
	if err != nil {
		return nil, err
	}
	return whileWanted(ctx, f.reads, f.slotWait, func() ([]byte, error) { return f.readFile(path) })
}

func read(path string) ([]byte, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotDownloaded
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	// Read at most one byte past the ceiling, so an oversized file is refused
	// by Open without being loaded whole.
	return io.ReadAll(io.LimitReader(file, MaxEnvelopeBytes+1))
}

func (f *FolderStore) Put(ctx context.Context, spaceTag, deviceTag string, blob []byte) error {
	path, err := f.path(spaceTag, deviceTag)
	if err != nil {
		return err
	}
	// A write given up on may still land: it was started while the store was
	// wanted, and it replaces the file whole either way. Its slot keeps the
	// next write out until it has.
	_, err = whileWanted(ctx, f.writes, f.slotWait, func() (struct{}, error) { return struct{}{}, f.write(path, blob) })
	return err
}

// write makes what is missing under the root, never the root itself. A root
// that is gone is a provider that went away — an unlinked Dropbox, an
// unmounted volume — and recreating it would leave every later write in a
// plain local folder while the pass reports it written.
func (f *FolderStore) write(path string, blob []byte) error {
	if info, err := os.Stat(f.root); err != nil || !info.IsDir() {
		return fmt.Errorf("the sync folder %s is not there", f.root)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return f.writeFile(path, blob)
}

// deviceFile is device.json.
type deviceFile struct {
	DeviceID string `json:"device_id"`
	// Binding ties the id to one machine and one data folder: hex
	// SHA-256(machine id ‖ absolute data-folder path). Empty when it was
	// made where the machine id could not be read.
	Binding string `json:"binding,omitempty"`
}

// deviceMu keeps two callers from each making a new id and one of them
// running with the id the other overwrote.
var deviceMu sync.Mutex

// DeviceID returns this installation's device id, creating it on first use.
// It names the machine to nothing but its own sync files — and only through
// a tag that differs per space — so a random value is all it needs to be.
//
// The id is bound to the machine and the data folder: a copy of the folder on
// another machine, or at another path on this one, gets an id of its own and
// with it a file of its own in each space's folder, instead of writing over
// the original's. See "Device identity" in docs/device-sync.md.
func DeviceID(home string) (string, error) {
	return deviceID(home, machineID)
}

func deviceID(home string, machine func() (string, error)) (string, error) {
	deviceMu.Lock()
	defer deviceMu.Unlock()
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", fmt.Errorf("locate the data folder: %w", err)
	}
	path := filepath.Join(abs, "device.json")
	var stored deviceFile
	data, err := os.ReadFile(path)
	found := err == nil
	switch {
	case found:
		if err := json.Unmarshal(data, &stored); err != nil {
			return "", fmt.Errorf("decode device.json: %w", err)
		}
		if !storage.ValidID(stored.DeviceID, "dev_") {
			return "", errors.New("device.json holds an invalid device id")
		}
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("read device.json: %w", err)
	}
	// A machine whose id cannot be read keeps the id it has, whatever its
	// binding: there is nothing to compare it with.
	binding := ""
	if id, err := machine(); err == nil && id != "" {
		binding = bindingOf(id, abs)
	}
	switch {
	case found && (binding == "" || stored.Binding == binding):
		return stored.DeviceID, nil
	case found && stored.Binding == "":
		// Made before ids were bound, or where the machine id could not be
		// read: this is its machine and folder from now on.
		stored.Binding = binding
	default:
		// New, or moved: a copy of another installation's folder.
		stored.DeviceID, err = storage.NewID("dev_")
		if err != nil {
			return "", err
		}
		stored.Binding = binding
	}
	data, err = json.Marshal(stored)
	if err != nil {
		return "", err
	}
	if err := storage.AtomicWrite(path, append(data, '\n')); err != nil {
		return "", err
	}
	return stored.DeviceID, nil
}

// bindingOf is the binding of a data folder at path on the machine with id
// machine.
func bindingOf(machine, path string) string {
	sum := sha256.Sum256([]byte(machine + path))
	return hex.EncodeToString(sum[:])
}

// SuggestedFolder is the iCloud Drive folder sync defaults to on a Mac where
// iCloud Drive is on, or "" where it is not.
func SuggestedFolder() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	drive := filepath.Join(home, "Library", "Mobile Documents", "com~apple~CloudDocs")
	if info, err := os.Stat(drive); err != nil || !info.IsDir() {
		return ""
	}
	return filepath.Join(drive, "Walletspace")
}
