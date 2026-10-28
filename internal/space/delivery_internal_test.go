package space

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sxwebdev/walletspace/internal/devicesync"
	"github.com/sxwebdev/walletspace/internal/vault"
)

const (
	deliveryPhrase   = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	deliveryPassword = "delivery test password"
)

// memStore is a folder in memory. A Put it is told to hold waits until it is
// let go, and says when it has started waiting.
type memStore struct {
	root  string
	mu    sync.Mutex
	files map[string][]byte
	hold  chan struct{}
	held  chan struct{}
}

func newMemStore(root string) *memStore {
	return &memStore{root: root, files: map[string][]byte{}, held: make(chan struct{}, 1)}
}

func (s *memStore) Root() string { return s.root }

func (s *memStore) Spaces(context.Context) ([]string, error) { return nil, nil }

func (s *memStore) List(_ context.Context, spaceTag string) ([]devicesync.Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []devicesync.Entry
	for key := range s.files {
		if tag, device, _ := strings.Cut(key, "/"); tag == spaceTag {
			out = append(out, devicesync.Entry{DeviceTag: device})
		}
	}
	return out, nil
}

func (s *memStore) Get(_ context.Context, spaceTag, deviceTag string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.files[spaceTag+"/"+deviceTag]
	if !ok {
		return nil, devicesync.ErrNotDownloaded
	}
	return bytes.Clone(data), nil
}

func (s *memStore) Put(ctx context.Context, spaceTag, deviceTag string, blob []byte) error {
	s.mu.Lock()
	hold := s.hold
	s.hold = nil
	s.mu.Unlock()
	if hold != nil {
		s.held <- struct{}{}
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[spaceTag+"/"+deviceTag] = bytes.Clone(blob)
	return nil
}

// holdNextPut makes the next Put wait for release.
func (s *memStore) holdNextPut() (release func()) {
	gate := make(chan struct{})
	s.mu.Lock()
	s.hold = gate
	s.mu.Unlock()
	return func() { close(gate) }
}

func (s *memStore) waitHeld(t *testing.T) {
	t.Helper()
	select {
	case <-s.held:
	case <-time.After(5 * time.Second):
		t.Fatal("no write reached the folder")
	}
}

func (s *memStore) file(spaceTag, deviceTag string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.files[spaceTag+"/"+deviceTag]
}

func deliveryManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir(), 15*time.Minute, vault.Params{Time: 2, MemoryKiB: 32 * 1024, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func deliveryTags(t *testing.T, deviceID, spaceID string) (string, string) {
	t.Helper()
	keys := deriveSyncKeys(&Payload{Mnemonic: []byte(deliveryPhrase)})
	t.Cleanup(keys.Clear)
	return keys.SpaceTag(), keys.DeviceTag(deviceID, spaceID)
}

// Sends run one at a time, each with the outbox as it is when it starts.
// A send held up in the folder while a newer change is saved acknowledges
// only the revision it carried; and a newer send asked for meanwhile waits
// for it, so that the older file cannot land over the newer one.
func TestAnOlderSendNeitherReplacesANewerFileNorAcknowledgesANewerRevision(t *testing.T) {
	t.Parallel()

	m := deliveryManager(t)
	store := newMemStore("/folder")
	quietSync(m, store, "dev_order")
	created, err := m.Create(CreateRequest{Mnemonic: deliveryPhrase, Password: deliveryPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	spaceTag, deviceTag := deliveryTags(t, "dev_order", id)
	send := func() <-chan error {
		result := make(chan error, 1)
		go func() {
			_, err := m.SyncNow(t.Context(), id)
			result <- err
		}()
		return result
	}
	rename := func(name string) []byte {
		t.Helper()
		if _, err := m.Rename(id, name); err != nil {
			t.Fatal(err)
		}
		outbox, err := os.ReadFile(m.outboxPath(id))
		if err != nil {
			t.Fatal(err)
		}
		return outbox
	}

	// Revision 2 is held in the folder's write while revision 3 is saved.
	rename("two")
	release := store.holdNextPut()
	older := send()
	store.waitHeld(t)
	rename("three")
	release()
	if err := <-older; err != nil {
		t.Fatal(err)
	}
	status := m.syncStatusOf(id)
	if status.WrittenRevision != 2 || status.LocalRevision != 3 || status.Delivery != DeliveryUnsent {
		t.Fatalf("status = %+v, want revision 2 written and 3 unsent", status)
	}

	// Revision 3's send is held while revision 4 is saved and sent.
	release = store.holdNextPut()
	older = send()
	store.waitHeld(t)
	newest := rename("four")
	newer := send()
	// Were the sends not one at a time, the newer one would be written now:
	// only the held write waits.
	time.Sleep(200 * time.Millisecond)
	release()
	for _, result := range []<-chan error{older, newer} {
		if err := <-result; err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(store.file(spaceTag, deviceTag), newest) {
		t.Fatal("an older send replaced the newer file in the folder")
	}
	if status := m.syncStatusOf(id); status.WrittenRevision != 4 || status.Delivery != DeliveryUploadUnknown {
		t.Fatalf("status = %+v, want revision 4 written", status)
	}
}

// What was written to one folder says nothing about another: moving sync to
// a new folder shows every space's change unsent — at once, before any send
// — until it is written there.
func TestChangingTheFolderResetsWhatWasWritten(t *testing.T) {
	t.Parallel()

	m := deliveryManager(t)
	quietSync(m, newMemStore("/old"), "dev_move")
	created, err := m.Create(CreateRequest{Mnemonic: deliveryPhrase, Password: deliveryPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	if status := m.syncStatusOf(id); status.Delivery != DeliveryUploadUnknown {
		t.Fatalf("status in the first folder = %+v, want written", status)
	}
	if err := m.Lock(id); err != nil {
		t.Fatal(err)
	}

	moved := newMemStore("/new")
	quietSync(m, moved, "dev_move")
	status := m.syncStatusOf(id)
	if status.Delivery != DeliveryUnsent || status.WrittenRevision != 0 || !status.LastWrite.IsZero() || status.NeedsUnlock {
		t.Fatalf("status in the new folder = %+v, want unsent and ready to send", status)
	}
	if status, err := m.SyncNow(t.Context(), id); err != nil || status.Delivery != DeliveryUploadUnknown {
		t.Fatalf("status %+v, error %v; want written to the new folder", status, err)
	}
	spaceTag, deviceTag := deliveryTags(t, "dev_move", id)
	outbox, err := os.ReadFile(m.outboxPath(id))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(moved.file(spaceTag, deviceTag), outbox) {
		t.Fatal("the new folder does not hold the outbox")
	}
}

// stuckStore is a folder that never answers a listing.
type stuckStore struct{ *memStore }

func (stuckStore) List(ctx context.Context, _ string) ([]devicesync.Entry, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A pass asked for by hand has the loop's bound: a folder that does not
// answer ends it with a timeout, even when the caller would wait forever.
func TestSyncNowGivesUpOnAFolderThatDoesNotAnswer(t *testing.T) {
	t.Parallel()

	m := deliveryManager(t)
	m.syncTimeout = 100 * time.Millisecond
	created, err := m.Create(CreateRequest{Mnemonic: deliveryPhrase, Password: deliveryPassword})
	if err != nil {
		t.Fatal(err)
	}
	quietSync(m, stuckStore{newMemStore("/stuck")}, "dev_stuck")
	result := make(chan error, 1)
	go func() {
		_, err := m.SyncNow(t.Context(), created.Space.ID)
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("SyncNow() error = %v, want its timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SyncNow waited on a folder that does not answer")
	}
	if status := m.syncStatusOf(created.Space.ID); status.State != SyncError {
		t.Fatalf("status = %+v, want the timeout reported", status)
	}
}

// downloadMem is a memStore whose files under evicted are not downloaded,
// and whose provider answers download requests with download.
type downloadMem struct {
	*memStore
	evicted  map[string]bool
	download func(ctx context.Context, deviceTag string) error
	mu       sync.Mutex
	asked    int
}

func (d *downloadMem) List(ctx context.Context, spaceTag string) ([]devicesync.Entry, error) {
	entries, err := d.memStore.List(ctx, spaceTag)
	for i := range entries {
		entries[i].Evicted = d.evicted[entries[i].DeviceTag]
	}
	return entries, err
}

func (d *downloadMem) Download(ctx context.Context, _, deviceTag string) error {
	d.mu.Lock()
	d.asked++
	d.mu.Unlock()
	return d.download(ctx, deviceTag)
}

func (d *downloadMem) askedCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.asked
}

// downloadSpace is an open space syncing, with no loop, through a folder
// that also holds n other machines' files, none of them downloaded.
func downloadSpace(t *testing.T, n int, download func(context.Context, string) error) (*Manager, *syncer, *downloadMem, string) {
	t.Helper()
	m := deliveryManager(t)
	created, err := m.Create(CreateRequest{Mnemonic: deliveryPhrase, Password: deliveryPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	spaceTag, _ := deliveryTags(t, "dev_dl", id)
	store := &downloadMem{memStore: newMemStore("/folder"), evicted: map[string]bool{}, download: download}
	for i := range n {
		tag := strings.Repeat(string(rune('a'+i)), 32)
		store.files[spaceTag+"/"+tag] = nil
		store.evicted[tag] = true
	}
	return m, quietSync(m, store, "dev_dl"), store, id
}

// A provider that is offline answers no request: each one runs out its own
// time. Every file must still be counted, round after round, until all of
// them are reported missing — not only the first few a round had time for.
func TestSlowFailingRequestsStillMakeEveryFileMissing(t *testing.T) {
	t.Parallel()

	m, s, _, id := downloadSpace(t, 4, func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	})
	m.downloadTimeout = 20 * time.Millisecond
	m.syncTimeout = 200 * time.Millisecond
	for range downloadAttempts {
		if _, err := m.SyncNow(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		s.requests.Wait()
	}
	if status := m.syncStatusOf(id); status.Missing != 4 || status.MissingReason != MissingRequestsFailed {
		t.Fatalf("status = %+v, want all four files missing after %d failed rounds", status, downloadAttempts)
	}
}

// A file is reported missing on its third failed request in a row — not
// before, and not only at the next pass after it: the round that failed it
// says so.
func TestAFileIsMissingAfterExactlyThreeFailedRequests(t *testing.T) {
	t.Parallel()

	m, s, _, id := downloadSpace(t, 1, func(context.Context, string) error { return errors.New("no such item") })
	for round := 1; round <= downloadAttempts; round++ {
		if _, err := m.SyncNow(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		s.requests.Wait()
		want := 0
		if round == downloadAttempts {
			want = 1
		}
		if status := m.syncStatusOf(id); status.Missing != want || status.Pending != 1 {
			t.Fatalf("after %d failed request(s): status = %+v, want %d missing", round, status, want)
		}
	}
}

// One round of requests per space at a time: a pass that finds the earlier
// round still waiting on the provider does not ask again.
func TestASpaceHasOneRoundOfRequestsAtATime(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	m, _, store, id := downloadSpace(t, 1, func(ctx context.Context, _ string) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	defer close(release)
	for range 2 {
		if _, err := m.SyncNow(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for store.askedCount() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := store.askedCount(); n != 1 {
		t.Fatalf("the provider was asked %d times while the first request was waiting", n)
	}
}

// Turning sync off waits for the requests in flight: a request for the old
// folder does not outlive it.
func TestTurningSyncOffWaitsForDownloadRequests(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	returned := false
	m, _, store, id := downloadSpace(t, 1, func(ctx context.Context, _ string) error {
		<-ctx.Done()
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		returned = true
		mu.Unlock()
		return ctx.Err()
	})
	if _, err := m.SyncNow(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for store.askedCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	m.SetSync(nil, "", nil)
	mu.Lock()
	defer mu.Unlock()
	if !returned {
		t.Fatal("sync was turned off with a download request still running")
	}
}

// blockingList is a memStore whose next listing waits to be let go.
type blockingList struct {
	*memStore
	entered chan struct{}
	release chan struct{}
}

func (b blockingList) List(ctx context.Context, spaceTag string) ([]devicesync.Entry, error) {
	select {
	case b.entered <- struct{}{}:
		select {
		case <-b.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	default:
	}
	return b.memStore.List(ctx, spaceTag)
}

// The outbox is prepared before the folder is read: a folder that takes its
// time must not leave the change unsealed if the space locks meanwhile.
func TestTheOutboxIsPreparedBeforeTheFolderIsRead(t *testing.T) {
	t.Parallel()

	m := deliveryManager(t)
	// Made with sync off: saved, and not in any outbox.
	created, err := m.Create(CreateRequest{Mnemonic: deliveryPhrase, Password: deliveryPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	store := blockingList{memStore: newMemStore("/slow"), entered: make(chan struct{}), release: make(chan struct{})}
	quietSync(m, store, "dev_slow")
	result := make(chan error, 1)
	go func() {
		_, err := m.SyncNow(t.Context(), id)
		result <- err
	}()
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the pass never read the folder")
	}
	status := m.syncStatusOf(id)
	close(store.release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if status.OutboxRevision != status.LocalRevision || status.NeedsUnlock {
		t.Fatalf("status while the folder was read = %+v, want the outbox already sealed", status)
	}
}

// Quitting sends what is left only between passes and sends, like any
// send: a pass's write held in the folder finishes first, and the newer
// outbox goes after it rather than under it.
func TestTheSendOnQuittingWaitsForTheSendInFlight(t *testing.T) {
	t.Parallel()

	m := deliveryManager(t)
	store := newMemStore("/folder")
	quietSync(m, store, "dev_quit")
	created, err := m.Create(CreateRequest{Mnemonic: deliveryPhrase, Password: deliveryPassword})
	if err != nil {
		t.Fatal(err)
	}
	id := created.Space.ID
	spaceTag, deviceTag := deliveryTags(t, "dev_quit", id)
	if _, err := m.Rename(id, "two"); err != nil {
		t.Fatal(err)
	}
	release := store.holdNextPut()
	pass := make(chan error, 1)
	go func() {
		_, err := m.SyncNow(t.Context(), id)
		pass <- err
	}()
	store.waitHeld(t)
	if _, err := m.Rename(id, "three"); err != nil {
		t.Fatal(err)
	}
	newest, err := os.ReadFile(m.outboxPath(id))
	if err != nil {
		t.Fatal(err)
	}
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		m.flushSync(5 * time.Second)
	}()
	// Were the send on quitting not one of the sends taken in turn, it
	// would write now: only the held write waits.
	time.Sleep(200 * time.Millisecond)
	release()
	if err := <-pass; err != nil {
		t.Fatal(err)
	}
	<-flushed
	if !bytes.Equal(store.file(spaceTag, deviceTag), newest) {
		t.Fatal("the send in flight landed over the newer one sent on quitting")
	}
}
