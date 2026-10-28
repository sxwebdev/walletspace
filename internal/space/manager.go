// Package space implements encrypted wallet collections and their lock sessions.
package space

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/sxwebdev/walletspace/internal/account"
	"github.com/sxwebdev/walletspace/internal/chain"
	"github.com/sxwebdev/walletspace/internal/devicesync"
	"github.com/sxwebdev/walletspace/internal/storage"
	"github.com/sxwebdev/walletspace/internal/vault"
)

const (
	SchemaVersion = 1
	// A vault password faces an offline attack on a stolen backup, where the
	// only real defence is length.
	minPasswordLength = 12
)

var (
	ErrNotFound         = errors.New("space not found")
	ErrLocked           = errors.New("space is locked")
	ErrDuplicateKey     = errors.New("private key is already imported")
	ErrNoSeed           = errors.New("space does not have a mnemonic")
	ErrHasSeed          = errors.New("space already has a recovery phrase")
	ErrSpaceChanged     = errors.New("the space changed while the password was being checked; try again")
	ErrAccountNotFound  = errors.New("account not found")
	ErrNetworkBinding   = errors.New("account is not available for this network")
	ErrFirstSpaceExists = errors.New("the first space was already created")
	ErrWeakPassword     = errors.New("space password is too weak")
	// ErrPasswordRequired marks a step-up that was not attempted at all, as
	// opposed to one that was attempted with the wrong password.
	ErrPasswordRequired = errors.New("space password is required")
	// ErrLastNetwork refuses to leave a wallet with no network: an empty
	// binding list is what a record from before bindings looks like.
	ErrLastNetwork = errors.New("the wallet's last network cannot be disconnected; delete the wallet instead")
)

type KeyEntry struct {
	Curve      string    `json:"curve"`
	PrivateKey []byte    `json:"private_key"`
	ImportedAt time.Time `json:"imported_at"`
}

// KeyVariants are the keys a reference holds. It is one key unless machines
// that sync the space disagreed about the reference; each wallet uses the
// variant that makes its address (see keyFor). Kept sorted by
// devicesync.VariantID, one entry per variant.
type KeyVariants []KeyEntry

// UnmarshalJSON reads a list, or the single key a vault written before key
// variants holds under each reference. The key's bytes, its reference and the
// wallets on it stay as they were.
func (v *KeyVariants) UnmarshalJSON(data []byte) error {
	if trimmed := bytes.TrimLeft(data, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '{' {
		var single KeyEntry
		if err := json.Unmarshal(data, &single); err != nil {
			clear(single.PrivateKey)
			return err
		}
		*v = KeyVariants{single}
		return nil
	}
	var list []KeyEntry
	if err := json.Unmarshal(data, &list); err != nil {
		for _, entry := range list {
			clear(entry.PrivateKey)
		}
		return err
	}
	*v = list
	return nil
}

type Payload struct {
	Version         int                    `json:"version"`
	Mnemonic        []byte                 `json:"mnemonic,omitempty"`
	BIP39Passphrase []byte                 `json:"bip39_passphrase,omitempty"`
	ImportedKeys    map[string]KeyVariants `json:"imported_keys"`
	// DeletedKeys are the tombstones of deleted key references. They live in
	// the vault, under the password, because a key tombstone deletes a secret
	// on every machine: one planted in a plaintext file — a bundle changed in
	// transit — must not be able to do that.
	DeletedKeys []devicesync.Tombstone `json:"deleted_keys,omitempty"`
	CreatedAt   time.Time              `json:"created_at"`
}

type File struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// NameUpdatedAt is when the name was last set. Sync settles two renames
	// by it; UpdatedAt moves on every change and cannot.
	NameUpdatedAt time.Time         `json:"name_updated_at,omitzero"`
	Accounts      []account.Account `json:"accounts"`
	Vault         vault.Container   `json:"vault"`
	// Tombstones of deleted wallets, kept for good so that a deletion reaches
	// every machine the space syncs to and an old snapshot read later cannot
	// bring the record back. Ids are never reused.
	//
	// Key tombstones are not here: they live in the vault (Payload.DeletedKeys).
	// A "deleted_keys" list an unreleased build left in space.json is not read
	// at all — in plaintext, anything that can write the file could have put
	// it there — and goes at the next write of the file.
	DeletedAccounts []devicesync.Tombstone `json:"deleted_accounts,omitempty"`
	// LocalRevision counts the changes made to the space here, a merge's
	// included; it is raised in the same write as the change. It is this
	// machine's bookkeeping of what has reached the sync folder (see sync.go)
	// and never decides a merge. A file from before it reads as 0.
	LocalRevision uint64 `json:"local_revision,omitempty"`
}

type Summary struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Locked bool   `json:"locked"`
	Seeded bool   `json:"seeded"`
	// HasPhrase says whether the vault holds a recovery phrase. Only an open
	// session can tell, so a locked space always reports false.
	HasPhrase    bool      `json:"has_phrase"`
	AccountCount int       `json:"account_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// CreateRequest describes a new space. Every new space has a recovery phrase:
// the given one, or a generated one. A space kept for imported keys simply
// derives no wallets from it — the phrase is still what restores the space,
// keys included, on another machine.
type CreateRequest struct {
	Name            string
	Mnemonic        string
	BIP39Passphrase string
	Password        string
	ExpectEmpty     bool
}

type CreateResult struct {
	Space             Summary
	Accounts          []account.Account
	Mnemonic          string
	MnemonicGenerated bool
	// Sync is what the space's first sync found, or nil when sync is off or
	// the space has no phrase.
	Sync *FirstSync
}

type ImportResult struct {
	Account account.Account
}

type LegacyAccount struct {
	Index       uint32
	Label       string
	TronAddress string
}

type session struct {
	payload  Payload
	key      *vault.SessionKey
	lastUsed time.Time
	// syncKeys are derived from the seed when the session opens and cleared
	// with it; a space without a phrase has none and does not sync.
	syncKeys *devicesync.Keys
	// sendGrant is when the password last entered to authorise spending stops
	// counting. It lives on the session rather than beside it so that anything
	// that ends the session takes the grant with it: locking the space by hand,
	// the idle timer, and a password change, which replaces the session rather
	// than refreshing it for exactly this reason.
	sendGrant time.Time
}

type Manager struct {
	home     string
	params   vault.Params
	autoLock time.Duration
	now      func() time.Time

	// confirmSends and sendGrantTTL are the spending step-up: whether moving
	// funds asks for the password, and how long one answer lasts. Guarded by mu
	// like the maps, because settings change while the wallet runs.
	confirmSends bool
	sendGrantTTL time.Duration

	mu       sync.RWMutex
	files    map[string]File
	sessions map[string]*session
	stop     chan struct{}
	done     chan struct{}
	reset    chan struct{}

	// spaceLocks serialises the password-checking operations per space, and
	// kdfSlots bounds how many derivations run at once across all of them.
	// Neither can be mu: mu is released across a derivation precisely so that
	// one unlock does not freeze every other space for the length of an Argon2
	// run. See throttle.go.
	spaceLocks sync.Map
	// unknownSpaceLock stands in for ids that are not space ids, so a request
	// naming one cannot add a permanent entry to spaceLocks.
	unknownSpaceLock sync.Mutex
	kdfSlots         chan struct{}

	// syncMu guards the sync wiring and what it reports. It may be taken while
	// holding mu, never the other way round. syncRun keeps passes from
	// overlapping. See sync.go.
	syncMu     sync.Mutex
	sync       *syncer
	syncStatus map[string]SyncStatus
	// sendErrors is why the last send of a space failed, by space id.
	sendErrors map[string]string
	// downloadFailures counts download requests that failed in a row, by
	// space id and device tag.
	downloadFailures map[string]int
	// mergeFirst holds the spaces whose file in the folder was left alone to
	// be merged first, and why (see deliver).
	mergeFirst map[string]string
	// syncUnavailable says why sync is turned on but not running.
	syncUnavailable string
	syncRun         sync.Mutex
	// syncTimeout bounds one pass over one space (syncRunTimeout), and
	// downloadTimeout one download request (downloadRequestTimeout); both
	// are shorter in tests.
	syncTimeout     time.Duration
	downloadTimeout time.Duration
}

func NewManager(home string, autoLock time.Duration, params vault.Params) (*Manager, error) {
	if err := storage.EnsureHome(home); err != nil {
		return nil, err
	}
	if params == (vault.Params{}) {
		params = vault.DefaultParams
	}
	m := &Manager{
		home: home, params: params, autoLock: autoLock, now: func() time.Time { return time.Now().UTC() },
		files: make(map[string]File), sessions: make(map[string]*session),
		stop: make(chan struct{}), done: make(chan struct{}), reset: make(chan struct{}, 1),
		kdfSlots:   newKDFSlots(),
		syncStatus: make(map[string]SyncStatus), sendErrors: make(map[string]string), mergeFirst: make(map[string]string),
		downloadFailures: make(map[string]int), syncTimeout: syncRunTimeout,
		downloadTimeout: downloadRequestTimeout,
		// On until told otherwise. A manager built without settings is one in a
		// test or an early start-up, and the safe reading of "not configured
		// yet" is the one that asks.
		confirmSends: true, sendGrantTTL: defaultSendGrantTTL,
	}
	if err := m.scan(); err != nil {
		return nil, err
	}
	go m.lockLoop()
	return m, nil
}

func (m *Manager) Close() {
	// What the spaces prepared and did not send yet goes to the folder now if
	// the folder answers at once; the outbox keeps it for the next start if
	// it does not.
	m.flushSync(syncFlushWait)
	m.SetSync(nil, "", nil)
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	<-m.done
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.sessions {
		m.clearSessionLocked(id)
	}
}

func (m *Manager) SetAutoLock(duration time.Duration) error {
	// Zero used to mean "never expire", which left a decrypted seed in memory
	// for the lifetime of the process.
	if duration <= 0 {
		return errors.New("auto-lock cannot be disabled")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.autoLock = duration
	m.expireLocked()
	select {
	case m.reset <- struct{}{}:
	default:
	}
	return nil
}

func (m *Manager) scan() error {
	entries, err := os.ReadDir(filepath.Join(m.home, "spaces"))
	if err != nil {
		return fmt.Errorf("scan spaces: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !storage.ValidID(entry.Name(), "spc_") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.home, "spaces", entry.Name(), "space.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read space %s: %w", entry.Name(), err)
		}
		var file File
		if err := json.Unmarshal(data, &file); err != nil {
			return fmt.Errorf("decode space %s: %w", entry.Name(), err)
		}
		if err := validateFile(file, entry.Name()); err != nil {
			return err
		}
		m.files[file.ID] = file
	}
	return nil
}

func validateFile(file File, directoryID string) error {
	if file.SchemaVersion != SchemaVersion {
		return fmt.Errorf("space %s uses unsupported schema version %d", directoryID, file.SchemaVersion)
	}
	if file.ID != directoryID || !storage.ValidID(file.ID, "spc_") {
		return fmt.Errorf("space directory and file id do not match")
	}
	if strings.TrimSpace(file.Name) == "" {
		return fmt.Errorf("space %s has an empty name", file.ID)
	}
	seen := make(map[string]struct{}, len(file.Accounts))
	for _, item := range file.Accounts {
		if !storage.ValidID(item.ID, "acc_") {
			return fmt.Errorf("space %s has invalid account id %q", file.ID, item.ID)
		}
		if _, exists := seen[item.ID]; exists {
			return fmt.Errorf("space %s has duplicate account id %q", file.ID, item.ID)
		}
		seen[item.ID] = struct{}{}
	}
	for _, tomb := range file.DeletedAccounts {
		if !storage.ValidID(tomb.ID, "acc_") {
			return fmt.Errorf("space %s has invalid deleted wallet id %q", file.ID, tomb.ID)
		}
	}
	return nil
}

func (m *Manager) List() []Summary {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	out := make([]Summary, 0, len(m.files))
	for _, file := range m.files {
		out = append(out, m.summaryLocked(file))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (m *Manager) Get(id string) (Summary, []account.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	return m.getLocked(id)
}

func (m *Manager) getLocked(id string) (Summary, []account.Account, error) {
	file, ok := m.files[id]
	if !ok {
		return Summary{}, nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return m.summaryLocked(file), cloneAccounts(file.Accounts), nil
}

func (m *Manager) Create(req CreateRequest) (CreateResult, error) {
	if err := validateNewPassword(req.Password); err != nil {
		return CreateResult{}, err
	}
	name, nameSetAt := strings.TrimSpace(req.Name), m.now()
	if err := validateName(name); err != nil {
		return CreateResult{}, err
	}
	if name == "" {
		// A default name has no time of its own, so that a space restored on
		// another machine takes the name its other machines gave it.
		name, nameSetAt = defaultName, time.Time{}
	}
	mnemonic := account.NormalizeMnemonic(req.Mnemonic)
	generated := false
	if mnemonic == "" {
		var err error
		mnemonic, err = account.GenerateMnemonic()
		if err != nil {
			return CreateResult{}, err
		}
		generated = true
	} else if err := account.ValidateMnemonic(mnemonic); err != nil {
		return CreateResult{}, err
	}

	now := m.now()
	id, err := newID("spc_")
	if err != nil {
		return CreateResult{}, err
	}
	payload := Payload{
		Version: 1, Mnemonic: []byte(mnemonic), BIP39Passphrase: []byte(req.BIP39Passphrase),
		ImportedKeys: make(map[string]KeyVariants), CreatedAt: now,
	}
	// Checked before the two derivations rather than after, so a caller that has
	// already hit the ceiling cannot spend 128 MiB of Argon2 per request finding
	// that out.
	if err := m.checkSpaceQuota(); err != nil {
		clearPayload(&payload)
		return CreateResult{}, err
	}
	file := File{SchemaVersion: SchemaVersion, ID: id, Name: name, CreatedAt: now, UpdatedAt: now, NameUpdatedAt: nameSetAt}
	releaseKDF := m.acquireKDF()
	container, err := vault.SealJSON(req.Password, payload, aad(id), m.params)
	releaseKDF()
	if err != nil {
		clearPayload(&payload)
		return CreateResult{}, err
	}
	file.Vault = container
	releaseKDF = m.acquireKDF()
	_, sessionKey, err := vault.Unlock(req.Password, container, aad(id))
	releaseKDF()
	if err != nil {
		clearPayload(&payload)
		return CreateResult{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.spaceQuotaLocked(); err != nil {
		sessionKey.Clear()
		clearPayload(&payload)
		return CreateResult{}, err
	}
	if req.ExpectEmpty && len(m.files) != 0 {
		sessionKey.Clear()
		clearPayload(&payload)
		return CreateResult{}, ErrFirstSpaceExists
	}
	if err := m.saveLocked(&file); err != nil {
		sessionKey.Clear()
		clearPayload(&payload)
		return CreateResult{}, err
	}
	m.files[id] = file
	m.sessions[id] = newSession(payload, sessionKey, now)
	m.rememberSpaceTagLocked(id)
	result := CreateResult{
		Space: m.summaryLocked(file), Accounts: cloneAccounts(file.Accounts),
		Mnemonic: mnemonic, MnemonicGenerated: generated,
	}
	m.mu.Unlock()
	// A space restored from its phrase finds what its other machines wrote.
	result.Sync = m.syncAfterUnlock(id)
	m.mu.Lock()
	if _, accounts, err := m.getLocked(id); err == nil {
		result.Accounts = accounts
		result.Space = m.summaryLocked(m.files[id])
	}
	return result, nil
}

// ImportLegacy verifies every legacy Tron address and publishes one complete
// encrypted space. The legacy directory is never changed.
func (m *Manager) ImportLegacy(req CreateRequest, legacy []LegacyAccount) (CreateResult, error) {
	if err := validateNewPassword(req.Password); err != nil {
		return CreateResult{}, err
	}
	mnemonic := account.NormalizeMnemonic(req.Mnemonic)
	verified, err := validateLegacy(mnemonic, req.BIP39Passphrase, legacy)
	if err != nil {
		return CreateResult{}, err
	}
	name, nameSetAt := strings.TrimSpace(req.Name), m.now()
	if name == "" {
		name, nameSetAt = defaultName, time.Time{}
	}
	now := m.now()
	id, err := newID("spc_")
	if err != nil {
		return CreateResult{}, err
	}
	payload := Payload{
		Version: 1, Mnemonic: []byte(mnemonic), BIP39Passphrase: []byte(req.BIP39Passphrase),
		ImportedKeys: make(map[string]KeyVariants), CreatedAt: now,
	}
	file := File{
		SchemaVersion: SchemaVersion, ID: id, Name: name, CreatedAt: now, UpdatedAt: now, NameUpdatedAt: nameSetAt,
		Accounts: make([]account.Account, 0, len(legacy)),
	}
	for _, old := range legacy {
		addresses := verified[old.Index]
		accountID, err := newID("acc_")
		if err != nil {
			clearPayload(&payload)
			return CreateResult{}, err
		}
		index := old.Index
		file.Accounts = append(file.Accounts, account.Account{
			ID: accountID, Label: old.Label, Kind: account.KindDerived, Addresses: addresses,
			Index: &index, DerivationProfile: "bip44-v1", CreatedAt: now, UpdatedAt: now,
		})
	}
	sort.Slice(file.Accounts, func(i, j int) bool { return *file.Accounts[i].Index < *file.Accounts[j].Index })
	container, err := vault.SealJSON(req.Password, payload, aad(id), m.params)
	if err != nil {
		clearPayload(&payload)
		return CreateResult{}, err
	}
	file.Vault = container
	_, sessionKey, err := vault.Unlock(req.Password, container, aad(id))
	if err != nil {
		clearPayload(&payload)
		return CreateResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.saveLocked(&file); err != nil {
		sessionKey.Clear()
		clearPayload(&payload)
		return CreateResult{}, err
	}
	m.files[id] = file
	m.sessions[id] = newSession(payload, sessionKey, now)
	m.rememberSpaceTagLocked(id)
	return CreateResult{
		Space: m.summaryLocked(file), Accounts: cloneAccounts(file.Accounts),
	}, nil
}

// ValidateLegacy performs the complete, read-only part of legacy migration.
// It is used by the CLI dry-run before a target home or vault is created.
func ValidateLegacy(mnemonic, passphrase string, legacy []LegacyAccount) error {
	_, err := validateLegacy(account.NormalizeMnemonic(mnemonic), passphrase, legacy)
	return err
}

func validateLegacy(
	mnemonic,
	passphrase string,
	legacy []LegacyAccount,
) (map[uint32]map[account.Family]string, error) {
	if err := account.ValidateMnemonic(mnemonic); err != nil {
		return nil, err
	}
	if len(legacy) == 0 {
		return nil, errors.New("legacy wallet list is empty")
	}
	verified := make(map[uint32]map[account.Family]string, len(legacy))
	for _, old := range legacy {
		if _, exists := verified[old.Index]; exists {
			return nil, fmt.Errorf("duplicate legacy derivation index %d", old.Index)
		}
		addresses, err := account.DerivedAddresses(mnemonic, passphrase, old.Index)
		if err != nil {
			return nil, err
		}
		if addresses[account.FamilyTron] != old.TronAddress {
			return nil, fmt.Errorf(
				"legacy address mismatch at index %d: derived %s, file contains %s",
				old.Index, addresses[account.FamilyTron], old.TronAddress,
			)
		}
		verified[old.Index] = addresses
	}
	return verified, nil
}

// Unlock derives the vault key and opens a session.
//
// The derivation is the expensive part and runs outside the manager's mutex:
// holding it across a 64 MiB Argon2 pass made one unlock block every other
// space, the space list and the auto-lock sweep. The per-space lock keeps two
// attempts on the same space in order, and it is also what stops a password
// change from replacing the container midway through this derivation.
func (m *Manager) Unlock(id, password string) error {
	_, err := m.UnlockAndSync(id, password)
	return err
}

// UnlockAndSync is Unlock, and reports what the space's first sync found
// (nil when sync is off or the space has no phrase).
func (m *Manager) UnlockAndSync(id, password string) (*FirstSync, error) {
	unlockSpace := m.lockSpace(id)
	defer unlockSpace()
	// A space that is already open can be resealed by a sync pass while this
	// derives; see retryWhileStale.
	if err := retryWhileStale(func() (bool, error) { return m.unlockOnce(id, password) }); err != nil {
		return nil, err
	}
	return m.syncAfterUnlock(id), nil
}

func (m *Manager) unlockOnce(id, password string) (stale bool, err error) {
	m.mu.RLock()
	file, ok := m.files[id]
	m.mu.RUnlock()
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err := m.checkUnlockCooldown(id); err != nil {
		return false, err
	}

	releaseKDF := m.acquireKDF()
	var payload Payload
	sessionKey, err := vault.UnlockJSON(password, file.Vault, aad(id), &payload)
	releaseKDF()
	if err != nil {
		if errors.Is(err, vault.ErrInvalidPassword) {
			m.recordUnlockFailure(id)
		}
		return false, err
	}
	if err := verifyPayload(file, payload); err != nil {
		sessionKey.Clear()
		clearPayload(&payload)
		return false, err
	}
	normalizePayload(&payload)
	m.clearUnlockFailures(id)

	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.files[id]; !ok || current.Vault != file.Vault {
		sessionKey.Clear()
		clearPayload(&payload)
		if !ok {
			return false, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return true, nil
	}
	m.clearSessionLocked(id)
	m.sessions[id] = newSession(payload, sessionKey, m.now())
	m.rememberSpaceTagLocked(id)
	return false, nil
}

func (m *Manager) Lock(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	m.clearSessionLocked(id)
	return nil
}

func (m *Manager) Rename(id, name string) (Summary, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Summary{}, errors.New("space name is required")
	}
	if err := validateName(name); err != nil {
		return Summary{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	file, ok := m.files[id]
	if !ok {
		return Summary{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	file.Name = name
	file.UpdatedAt = m.now()
	file.NameUpdatedAt = file.UpdatedAt
	if err := m.saveLocked(&file); err != nil {
		return Summary{}, err
	}
	m.files[id] = file
	return m.summaryLocked(file), nil
}

// defaultName is what a space is called when it is given no name. It carries
// no time of its own (see snapshotOf).
const defaultName = "default"

// validateName bounds a space name by what a sync snapshot carries, so that a
// name accepted here can always be synced.
func validateName(name string) error {
	if utf8.RuneCountInString(name) > devicesync.MaxNameRunes {
		return fmt.Errorf("space name is invalid: longer than %d characters", devicesync.MaxNameRunes)
	}
	return nil
}

// ChangePassword re-encrypts the vault under a new password.
//
// Three derivations happen here — opening the old container, sealing the new
// one and re-deriving the live session key — and all three run outside the
// manager's mutex. Under the old arrangement this was the single longest lock
// hold in the process. The per-space lock is what keeps the container this
// reads from being the one it writes back over.
func (m *Manager) ChangePassword(id, currentPassword, newPassword string) error {
	if err := validateNewPassword(newPassword); err != nil {
		return err
	}
	unlockSpace := m.lockSpace(id)
	defer unlockSpace()
	return retryWhileStale(func() (bool, error) {
		return m.changePasswordOnce(id, currentPassword, newPassword)
	})
}

// staleAttempts bounds how often a password check starts over because a sync
// pass rewrote the vault while it was deriving. A pass runs at most every few
// hundred milliseconds per change; a few attempts is plenty.
const staleAttempts = 3

// retryWhileStale runs attempt until it reports a vault that did not change
// under it.
//
// Unlock and ChangePassword read the vault, spend seconds of Argon2 outside
// the manager's lock, and then build a session or a new container from what
// they read. The per-space lock keeps other password paths out, but not a
// sync pass: it reseals the vault with adopted keys under the session key,
// without that lock. Writing back what was read would drop those keys while
// space.json keeps their wallets — the space would no longer unlock. So the
// attempt checks under the lock that the vault is still the one it read, and
// starts over if it is not.
func retryWhileStale(attempt func() (stale bool, err error)) error {
	for range staleAttempts {
		stale, err := attempt()
		if err != nil || !stale {
			return err
		}
	}
	return ErrSpaceChanged
}

func (m *Manager) changePasswordOnce(id, currentPassword, newPassword string) (stale bool, err error) {
	m.mu.RLock()
	file, ok := m.files[id]
	m.mu.RUnlock()
	if !ok {
		return false, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	read := file.Vault
	if err := m.checkUnlockCooldown(id); err != nil {
		return false, err
	}

	releaseKDF := m.acquireKDF()
	var payload Payload
	err = vault.OpenJSON(currentPassword, file.Vault, aad(id), &payload)
	releaseKDF()
	if err != nil {
		if errors.Is(err, vault.ErrInvalidPassword) {
			m.recordUnlockFailure(id)
		}
		return false, err
	}
	defer clearPayload(&payload)
	if err := verifyPayload(file, payload); err != nil {
		return false, err
	}
	normalizePayload(&payload)
	m.clearUnlockFailures(id)

	releaseKDF = m.acquireKDF()
	container, err := vault.SealJSON(newPassword, payload, aad(id), m.params)
	releaseKDF()
	if err != nil {
		return false, err
	}

	// An open session has to keep working under the new password, which means a
	// third derivation. It happens here rather than under the mutex, and only
	// when there is a session to refresh. The per-space lock is what makes the
	// check outside the mutex sound: nothing else can open or close a session
	// for this space while this call is in progress.
	var refreshed Payload
	var sessionKey *vault.SessionKey
	m.mu.RLock()
	hadSession := m.sessions[id] != nil
	m.mu.RUnlock()
	if hadSession {
		releaseKDF = m.acquireKDF()
		sessionKey, err = vault.UnlockJSON(newPassword, container, aad(id), &refreshed)
		releaseKDF()
		if err != nil {
			clearPayload(&refreshed)
			return false, err
		}
	}
	previous := m.resealPrevious(id, currentPassword, newPassword)

	m.mu.Lock()
	defer m.mu.Unlock()
	file, ok = m.files[id]
	if !ok {
		sessionKey.Clear()
		clearPayload(&refreshed)
		return false, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if file.Vault != read {
		// A sync pass changed the vault since it was read; see retryWhileStale.
		sessionKey.Clear()
		clearPayload(&refreshed)
		return true, nil
	}
	file.Vault = container
	file.UpdatedAt = m.now()
	// The same wallets, keys and phrase under another password: nothing sync
	// carries changed, so the revision stays and nothing is sent.
	if err := m.storeFileLocked(file); err != nil {
		sessionKey.Clear()
		clearPayload(&refreshed)
		return false, err
	}
	m.files[id] = file
	// The space is saved under the new password; a copy that failed to follow
	// is left as it was rather than failing a change that has happened.
	if previous != nil {
		_ = storage.AtomicWrite(m.previousPath(id), previous)
	}
	// The session is replaced rather than refreshed in place. Swapping the key
	// and the payload left everything else on it standing, including a spending
	// grant opened under the password that was just replaced — so changing the
	// password did not end the window it had bought, and a transfer went through
	// on the strength of a secret that no longer opens the vault. Unlock is safe
	// from this only because it allocates a fresh session; this now does the
	// same on purpose.
	if active := m.sessions[id]; active != nil && sessionKey != nil {
		m.clearSessionLocked(id)
		m.sessions[id] = newSession(refreshed, sessionKey, m.now())
	} else {
		sessionKey.Clear()
		clearPayload(&refreshed)
	}
	return false, nil
}

// commonPasswords are the ones that turn up first in every cracking wordlist.
//
// This is not a breach corpus and cannot be: shipping one would mean shipping
// megabytes, and checking against a hosted one would send a hash of the user's
// password off the machine, which this wallet will not do. It catches the
// handful that an offline attack on a stolen backup tries in its first second.
var commonPasswords = map[string]struct{}{
	"password": {}, "password1": {}, "password123": {}, "passw0rd": {},
	"12345678": {}, "123456789": {}, "1234567890": {}, "qwertyuiop": {},
	"letmein": {}, "welcome1": {}, "iloveyou": {}, "admin123": {},
	"walletspace": {}, "changeme": {}, "secret123": {}, "trustno1": {},
	"correcthorsebatterystaple": {},
}

// validateNewPassword bounds how weak a vault password may be.
//
// The password is the only thing between a copy of the encrypted backup and the
// seed inside it, and an offline attacker gets unlimited attempts at it. Length
// is what buys time against that, so it carries most of the weight here — a
// passphrase of several words beats a short string with punctuation in it.
func validateNewPassword(password string) error {
	if password == "" {
		return errors.New("space password is required")
	}
	if utf8.RuneCountInString(password) < minPasswordLength {
		return fmt.Errorf(
			"%w: use at least %d characters — a few unrelated words are easier to remember "+
				"and much harder to guess than a short password, and a password manager is better still",
			ErrWeakPassword, minPasswordLength,
		)
	}
	folded := strings.ToLower(strings.TrimSpace(password))
	if _, common := commonPasswords[folded]; common {
		return fmt.Errorf("%w: this is one of the first passwords an attacker tries", ErrWeakPassword)
	}
	if isSingleRepeatedRune(password) {
		return fmt.Errorf("%w: it is the same character repeated", ErrWeakPassword)
	}

	return nil
}

func isSingleRepeatedRune(value string) bool {
	var first rune
	for i, r := range value {
		if i == 0 {
			first = r
			continue
		}
		if r != first {
			return false
		}
	}

	return true
}

func (m *Manager) Derive(
	id, networkID string,
	family account.Family,
	label string,
) (account.Account, error) {
	return m.DeriveNetworks(id, []string{networkID}, family, label)
}

// DeriveNetworks creates one wallet on several networks of a family at once.
// Its index is the lowest one no derived wallet uses on any of them, so the
// new wallet is new on every network it is created for.
func (m *Manager) DeriveNetworks(
	id string, networkIDs []string,
	family account.Family,
	label string,
) (account.Account, error) {
	networkIDs, err := networkList(networkIDs)
	if err != nil {
		return account.Account{}, err
	}
	if _, err := account.CurveOf(family); err != nil {
		return account.Account{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	file, payload, err := m.unlockedLocked(id)
	if err != nil {
		return account.Account{}, err
	}
	if len(payload.Mnemonic) == 0 {
		return account.Account{}, ErrNoSeed
	}
	used := make(map[uint32]struct{})
	for _, item := range file.Accounts {
		if item.Kind == account.KindDerived && item.Index != nil &&
			slices.ContainsFunc(networkIDs, item.BoundTo) {
			used[*item.Index] = struct{}{}
		}
	}
	var next uint32
	for {
		if _, exists := used[next]; !exists {
			break
		}
		if next == ^uint32(0) {
			return account.Account{}, errors.New("derivation index exhausted")
		}
		next++
	}
	// BIP44 has a family coin type but no standard per-network component.
	// Thus index 0 on two EVM networks is the same key. Reuse the wallet and
	// add a binding instead of persisting duplicate key identities.
	for i := range file.Accounts {
		item := &file.Accounts[i]
		if item.Kind != account.KindDerived || item.Family != family ||
			item.Index == nil || *item.Index != next {
			continue
		}
		for _, networkID := range networkIDs {
			item.NetworkIDs = appendNetwork(item.NetworkIDs, networkID)
		}
		if item.Label == "" {
			item.Label = strings.TrimSpace(label)
		}
		item.UpdatedAt = m.now()
		file.UpdatedAt = item.UpdatedAt
		if err := m.saveLocked(&file); err != nil {
			return account.Account{}, err
		}
		m.files[id] = file
		return item.Clone(), nil
	}
	address, err := account.DerivedAddress(
		string(payload.Mnemonic), string(payload.BIP39Passphrase), family, next,
	)
	if err != nil {
		return account.Account{}, err
	}
	if err := accountQuotaLocked(file); err != nil {
		return account.Account{}, err
	}
	accountID, err := newID("acc_")
	if err != nil {
		return account.Account{}, err
	}
	now := m.now()
	created := account.Account{
		ID: accountID, Label: strings.TrimSpace(label), Kind: account.KindDerived,
		Family: family, NetworkIDs: networkIDs,
		Addresses: map[account.Family]string{family: address},
		Index:     &next, DerivationProfile: "bip44-v1",
		CreatedAt: now, UpdatedAt: now,
	}
	file.Accounts = append(file.Accounts, created)
	file.UpdatedAt = now
	if err := m.saveLocked(&file); err != nil {
		return account.Account{}, err
	}
	m.files[id] = file
	return created.Clone(), nil
}

func (m *Manager) Import(
	id, networkID string,
	family account.Family,
	label, privateKey string,
) (ImportResult, error) {
	return m.ImportNetworks(id, []string{networkID}, family, label, privateKey)
}

// ImportNetworks imports a key onto several networks at once. family names
// the key's curve; the caller checks that every network takes that curve.
func (m *Manager) ImportNetworks(
	id string, networkIDs []string,
	family account.Family,
	label, privateKey string,
) (ImportResult, error) {
	networkIDs, err := networkList(networkIDs)
	if err != nil {
		return ImportResult{}, err
	}
	curve, err := account.CurveOf(family)
	if err != nil {
		return ImportResult{}, err
	}
	raw, fingerprint, addresses, err := parseImportedKey(curve, privateKey)
	if err != nil {
		return ImportResult{}, err
	}
	defer clear(raw)

	m.mu.Lock()
	defer m.mu.Unlock()
	file, payload, err := m.unlockedLocked(id)
	if err != nil {
		return ImportResult{}, err
	}
	for i := range file.Accounts {
		item := &file.Accounts[i]
		if item.Kind == account.KindImported && item.Fingerprint == fingerprint {
			if !slices.ContainsFunc(networkIDs, func(id string) bool { return !item.BoundTo(id) }) {
				return ImportResult{}, ErrDuplicateKey
			}
			for _, networkID := range networkIDs {
				item.NetworkIDs = appendNetwork(item.NetworkIDs, networkID)
			}
			if item.Label == "" {
				item.Label = strings.TrimSpace(label)
			}
			item.UpdatedAt = m.now()
			file.UpdatedAt = item.UpdatedAt
			if err := m.saveLocked(&file); err != nil {
				return ImportResult{}, err
			}
			m.files[id] = file
			return ImportResult{Account: item.Clone()}, nil
		}
	}
	if err := accountQuotaLocked(file); err != nil {
		return ImportResult{}, err
	}
	keyRef, err := newID("key_")
	if err != nil {
		return ImportResult{}, err
	}
	accountID, err := newID("acc_")
	if err != nil {
		return ImportResult{}, err
	}
	now := m.now()
	importedAt := now
	created := account.Account{
		ID: accountID, Label: strings.TrimSpace(label), Kind: account.KindImported,
		NetworkIDs: networkIDs, Addresses: addresses,
		KeyRef: keyRef, Fingerprint: fingerprint, ImportedAt: &importedAt,
		CreatedAt: now, UpdatedAt: now,
	}
	payload.ImportedKeys[keyRef] = KeyVariants{{
		Curve: string(curve), PrivateKey: append([]byte(nil), raw...), ImportedAt: now,
	}}
	file.Accounts = append(file.Accounts, created)
	file.UpdatedAt = now
	if err := m.resealAndSaveLocked(&file, payload, id); err != nil {
		delete(payload.ImportedKeys, keyRef)
		return ImportResult{}, err
	}
	m.files[id] = file
	return ImportResult{Account: created.Clone()}, nil
}

// ExportPrivateKey hands over permanent control of one account, so it asks for
// the space password again rather than trusting the open session.
func (m *Manager) ExportPrivateKey(
	id, accountID string, family account.Family, password string,
) (string, error) {
	if _, err := account.CurveOf(family); err != nil {
		return "", err
	}
	unlockSpace := m.lockSpace(id)
	defer unlockSpace()
	if err := m.confirmPassword(id, password); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	file, payload, err := m.unlockedLocked(id)
	if err != nil {
		return "", err
	}
	item, err := findAccount(file.Accounts, accountID)
	if err != nil {
		return "", err
	}
	key, err := privateKeyFor(item, payload, family)
	if err != nil {
		return "", err
	}
	defer key.clear()
	return key.export()
}

// WithSigner lends fn a secp256k1 digest signer for the account on a Tron or
// EVM network. The key never leaves the manager: it is derived or decrypted
// for the call and cleared when fn returns. A family whose keys are ed25519
// is refused with chain.ErrSignerMismatch; see WithMessageSigner.
func (m *Manager) WithSigner(
	ctx context.Context,
	id, accountID, networkID string,
	family account.Family,
	fn func(chain.Signer) error,
) error {
	if fn == nil {
		return errors.New("signer callback is required")
	}
	return m.withKey(id, accountID, networkID, family, account.CurveSecp256k1, func(key signingKey) error {
		signer := &localSigner{key: key.secp256k1, family: family}
		defer signer.clear()
		return fn(signer)
	})
}

// WithMessageSigner lends fn an ed25519 message signer for the account on a
// network whose family signs whole messages (Solana), under the same rules as
// WithSigner. A secp256k1 family is refused with chain.ErrSignerMismatch.
func (m *Manager) WithMessageSigner(
	ctx context.Context,
	id, accountID, networkID string,
	family account.Family,
	fn func(chain.MessageSigner) error,
) error {
	if fn == nil {
		return errors.New("signer callback is required")
	}
	return m.withKey(id, accountID, networkID, family, account.CurveEd25519, func(key signingKey) error {
		signer := &ed25519Signer{key: key.ed25519, family: family}
		defer signer.clear()
		return fn(signer)
	})
}

// WithKey lends fn the signer the family's curve calls for — a digest signer
// for secp256k1, a message signer for ed25519 — under the same rules as
// WithSigner. The generic send path uses it; an adapter asserts the kind of
// signer it needs.
func (m *Manager) WithKey(
	ctx context.Context,
	id, accountID, networkID string,
	family account.Family,
	fn func(chain.Key) error,
) error {
	if fn == nil {
		return errors.New("signer callback is required")
	}
	curve, err := account.CurveOf(family)
	if err != nil {
		return err
	}
	if curve == account.CurveEd25519 {
		return m.WithMessageSigner(ctx, id, accountID, networkID, family, func(signer chain.MessageSigner) error {
			return fn(signer)
		})
	}
	return m.WithSigner(ctx, id, accountID, networkID, family, func(signer chain.Signer) error {
		return fn(signer)
	})
}

// withKey resolves the account's key for family, which must be on curve want,
// and hands it to fn. The key is cleared when fn returns.
func (m *Manager) withKey(
	id, accountID, networkID string,
	family account.Family,
	want account.Curve,
	fn func(signingKey) error,
) error {
	curve, err := account.CurveOf(family)
	if err != nil {
		return err
	}
	if curve != want {
		return fmt.Errorf("%w: %s keys are %s", chain.ErrSignerMismatch, family, curve)
	}
	m.mu.Lock()
	file, payload, err := m.unlockedLocked(id)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	item, err := findAccount(file.Accounts, accountID)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	if !item.BoundTo(networkID) {
		m.mu.Unlock()
		return ErrNetworkBinding
	}
	key, err := privateKeyFor(item, payload, family)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	defer key.clear()
	return fn(key)
}

// Mnemonic reveals the seed every account in the space derives from, so it
// asks for the space password again rather than trusting the open session.
func (m *Manager) Mnemonic(id, password string) (string, error) {
	unlockSpace := m.lockSpace(id)
	defer unlockSpace()
	if err := m.confirmPassword(id, password); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, payload, err := m.unlockedLocked(id)
	if err != nil {
		return "", err
	}
	if len(payload.Mnemonic) == 0 {
		return "", ErrNoSeed
	}
	return string(payload.Mnemonic), nil
}

// defaultSendGrantTTL is what a manager uses until it is told otherwise. The
// bounds on a configured value are policy and live with the settings, exactly
// as the auto-lock range does.
const defaultSendGrantTTL = 5 * time.Minute

// ErrSendConfirmationRequired reports that spending needs the password again.
//
// It is deliberately distinct from ErrPasswordRequired: the caller is not being
// told that it sent a bad request, it is being told to ask the person at the
// keyboard and come back. The UI turns exactly this into a prompt.
var ErrSendConfirmationRequired = errors.New("confirm this transfer with the space password")

// SetSendConfirmation applies the spending step-up settings.
//
// It governs the windows opened after it, not the ones already outstanding: a
// grant is an absolute deadline fixed when the password was entered, so
// shortening the window leaves a live grant where it is. Turning the step-up
// off, meanwhile, is not reachable from the API at all — the setting is read
// from config.yaml at start, because a caller able to switch it off would have
// no need of the password it asks for.
func (m *Manager) SetSendConfirmation(enabled bool, ttl time.Duration) {
	if ttl <= 0 {
		ttl = defaultSendGrantTTL
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.confirmSends = enabled
	m.sendGrantTTL = ttl
}

// ConfirmSend checks the password and opens the spending window.
//
// It goes through the same step-up as the exports, which means the same
// throttle, the same KDF semaphore and the same per-space lock — otherwise this
// would be a new unthrottled place to guess a password, which is precisely the
// defect the export step-up shipped with.
//
// The context is the caller's, and it is consulted once: after the password has
// been proven and before the window is written. That side of the derivation is
// deliberate. The dialog can be dismissed while this call is inside a
// derivation that takes the better part of a second, and the browser then tells
// the person nothing was confirmed — so the one thing a cancellation has to
// change is whether the window exists, and the only place that can be decided
// is here. Consulting it earlier would make something else depend on when the
// caller hung up: the verdict on the password, and with it the cooldown. A
// cancelled attempt therefore counts exactly as it would have counted had
// nobody hung up — a wrong password still costs a failure, a right one still
// clears the record — because a guess that can be taken back by closing the
// connection is a guess that is free.
func (m *Manager) ConfirmSend(ctx context.Context, id, password string) (time.Time, error) {
	unlockSpace := m.lockSpace(id)
	defer unlockSpace()
	if err := m.confirmPassword(id, password); err != nil {
		return time.Time{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Under the mutex, so nothing can slip between the decision and the write.
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	// Only for a space that is open. A grant on a locked space would sit there
	// waiting to be used by whoever unlocks it next — and "open" has to mean
	// what it means to the signer, so the idle sweep runs first: the derivation
	// above happens outside the mutex and takes long enough for a session to
	// reach its deadline while it runs.
	m.expireLocked()
	active := m.sessions[id]
	if active == nil {
		return time.Time{}, fmt.Errorf("%w: %s", ErrLocked, id)
	}
	now := m.now()
	// Entering the space password is the strongest evidence of presence this
	// wallet has, so it counts as use of the session. Without it, someone who
	// unlocked fourteen minutes ago and typed the password to authorise a
	// transfer could have the session swept out from under that transfer
	// seconds later. It makes six actions refresh the idle timer rather than
	// the five that read the decrypted payload — and the sixth is a password,
	// which is a stronger claim to be present than any of the other five.
	active.lastUsed = now
	active.sendGrant = now.Add(m.sendGrantTTL)
	return active.sendGrant, nil
}

// RequireSendConfirmation is the check every signing path makes before it
// spends anything.
//
// Without it the wallet's own rules do not line up: revealing a key asks for
// the password, and sending the funds that key controls does not — so anything
// that reaches the API of an unlocked space cannot steal the seed but can move
// everything it protects, one transfer at a time.
func (m *Manager) RequireSendConfirmation(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// The idle sweep runs first, as it does in every other method that reads
	// m.sessions. The background sweep runs at most once a minute, so reading
	// the map as it stands let the gate answer "authorised" for up to that long
	// from a session whose auto-lock deadline had already passed: the caller was
	// waved through and the signing path then refused it with a locked-space
	// error the UI has no prompt for. Sweeping here turns that into the 423 the
	// UI does understand, and costs the write lock the sweep needs.
	//
	// It deliberately does not touch lastUsed. This is a check, reachable by
	// anything holding the capability token, and letting it refresh the idle
	// timer would hand that caller a way to keep a space unlocked indefinitely
	// by polling the gate it cannot pass.
	m.expireLocked()
	if !m.confirmSends {
		return nil
	}
	active := m.sessions[id]
	if active == nil {
		return fmt.Errorf("%w: %s", ErrLocked, id)
	}
	if active.sendGrant.IsZero() || !m.now().Before(active.sendGrant) {
		return ErrSendConfirmationRequired
	}
	return nil
}

// Backup hands over the space file, vault container and all, so it asks for the
// space password again — the same step-up the seed and private-key exports use.
//
// What leaves here is every secret in the space in a form that can be worked on
// at leisure and without a rate limit, which makes it worth more to an attacker
// than a single exported key. It is also the one export that does not need the
// space to be unlocked first, so before this it was the cheapest thing for a
// caller holding the capability token to take.
func (m *Manager) Backup(id, password string) ([]byte, error) {
	unlockSpace := m.lockSpace(id)
	defer unlockSpace()
	if err := m.confirmPassword(id, password); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	file, ok := m.files[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode backup: %w", err)
	}
	return append(data, '\n'), nil
}

func (m *Manager) Accounts(id string) ([]account.Account, error) {
	_, accounts, err := m.Get(id)
	return accounts, err
}

// BindNetwork explicitly makes an existing wallet available on a network.
// Derived wallets can only be bound to networks from their derivation family;
// imported secp256k1 keys can be bound to either supported family.
func (m *Manager) BindNetwork(
	id, accountID, networkID string,
	family account.Family,
) (account.Account, error) {
	if networkID == "" {
		return account.Account{}, errors.New("network is required")
	}
	if _, err := account.CurveOf(family); err != nil {
		return account.Account{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	file, ok := m.files[id]
	if !ok {
		return account.Account{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	file.Accounts = cloneAccounts(file.Accounts)
	for i := range file.Accounts {
		item := &file.Accounts[i]
		if item.ID != accountID {
			continue
		}
		if item.Kind == account.KindDerived {
			if item.Family == "" {
				// Safe migration for pre-binding records: the user chooses the
				// original network, then the obsolete other-family projection
				// is removed from this wallet.
				if item.Addresses[family] == "" {
					return account.Account{}, ErrNetworkBinding
				}
				item.Family = family
				item.Addresses = map[account.Family]string{family: item.Addresses[family]}
			} else if item.Family != family {
				return account.Account{}, ErrNetworkBinding
			}
		}
		if item.Addresses[family] == "" {
			return account.Account{}, ErrNetworkBinding
		}
		item.NetworkIDs = appendNetwork(item.NetworkIDs, networkID)
		item.UpdatedAt = m.now()
		file.UpdatedAt = item.UpdatedAt
		if err := m.saveLocked(&file); err != nil {
			return account.Account{}, err
		}
		m.files[id] = file
		return item.Clone(), nil
	}
	return account.Account{}, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
}

// UnbindNetwork takes a network off a wallet. The key, its addresses and its
// other networks stay, and the network can be connected again later.
func (m *Manager) UnbindNetwork(id, accountID, networkID string) (account.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	file, ok := m.files[id]
	if !ok {
		return account.Account{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	file.Accounts = cloneAccounts(file.Accounts)
	for i := range file.Accounts {
		item := &file.Accounts[i]
		if item.ID != accountID {
			continue
		}
		if !item.BoundTo(networkID) {
			return account.Account{}, ErrNetworkBinding
		}
		if len(item.NetworkIDs) == 1 {
			return account.Account{}, ErrLastNetwork
		}
		item.NetworkIDs = slices.DeleteFunc(item.NetworkIDs, func(id string) bool { return id == networkID })
		item.UpdatedAt = m.now()
		file.UpdatedAt = item.UpdatedAt
		if err := m.saveLocked(&file); err != nil {
			return account.Account{}, err
		}
		m.files[id] = file
		return item.Clone(), nil
	}
	return account.Account{}, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
}

func (m *Manager) RenameAccount(id, accountID, label string) (account.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	file, ok := m.files[id]
	if !ok {
		return account.Account{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	file.Accounts = cloneAccounts(file.Accounts)
	for i := range file.Accounts {
		if file.Accounts[i].ID != accountID {
			continue
		}
		file.Accounts[i].Label = strings.TrimSpace(label)
		file.Accounts[i].UpdatedAt = m.now()
		file.UpdatedAt = m.now()
		if err := m.saveLocked(&file); err != nil {
			return account.Account{}, err
		}
		m.files[id] = file
		return file.Accounts[i].Clone(), nil
	}
	return account.Account{}, fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
}

// DeleteAccount removes a wallet from the space. For an imported wallet that
// also takes its private key out of the vault — every variant of its
// reference — and leaves a key tombstone in the vault, so that the key goes on
// every machine the space syncs to. Nothing can undo that, so the space
// password is asked for again rather than trusting the open session — the
// same bar as exporting the key. A derived wallet only loses its record:
// deriving again on the same network hands back the same lowest free index.
//
// Walletspace gives every import a reference of its own, so a key belongs to
// one wallet. Two wallets on one reference can only come from a snapshot this
// wallet did not write; for that case the key stays while another wallet of
// the space still uses it, so that the space keeps unlocking.
func (m *Manager) DeleteAccount(id, accountID, password string) error {
	unlockSpace := m.lockSpace(id)
	defer unlockSpace()
	if err := m.confirmPassword(id, password); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	file, payload, err := m.unlockedLocked(id)
	if err != nil {
		return err
	}
	index := slices.IndexFunc(file.Accounts, func(item account.Account) bool { return item.ID == accountID })
	if index < 0 {
		return fmt.Errorf("%w: %s", ErrAccountNotFound, accountID)
	}
	removed := file.Accounts[index]
	file.Accounts = slices.Delete(file.Accounts, index, index+1)
	file.UpdatedAt = m.now()
	// Copies, so a failed write leaves the published file's slices as they were.
	file.DeletedAccounts = append(slices.Clone(file.DeletedAccounts),
		devicesync.Tombstone{ID: removed.ID, DeletedAt: file.UpdatedAt})

	sharedKey := slices.ContainsFunc(file.Accounts, func(item account.Account) bool {
		return item.Kind == account.KindImported && item.KeyRef == removed.KeyRef
	})
	if removed.Kind != account.KindImported || sharedKey {
		if err := m.saveLocked(&file); err != nil {
			return err
		}
		m.files[id] = file
		return nil
	}

	// The key leaves the payload before the vault is resealed, and comes back
	// if the write fails: the session must keep matching what is on disk.
	variants, hadKey := payload.ImportedKeys[removed.KeyRef]
	deleted := payload.DeletedKeys
	delete(payload.ImportedKeys, removed.KeyRef)
	payload.DeletedKeys = append(slices.Clone(deleted),
		devicesync.Tombstone{ID: removed.KeyRef, DeletedAt: file.UpdatedAt})
	if err := m.resealAndSaveLocked(&file, payload, id); err != nil {
		if hadKey {
			payload.ImportedKeys[removed.KeyRef] = variants
		}
		payload.DeletedKeys = deleted
		return err
	}
	for _, entry := range variants {
		clear(entry.PrivateKey)
	}
	m.files[id] = file
	return nil
}

// confirmPassword re-derives the vault key from a password the caller just
// supplied, without disturbing the open session.
//
// Exporting a seed or a private key hands over permanent control of the funds,
// and an unlocked space is not evidence that the person asking is the owner: a
// tab left open, a same-origin script or any local client with the capability
// token all inherit that state. Re-entering the password is the one thing none
// of them can do, and it costs a full KDF — which is the point.
//
// That also makes this a password check reachable from the API, so it goes
// through the same throttle, semaphore and per-space lock as Unlock. Leaving it
// outside them would have left an unthrottled guessing oracle beside a
// throttled one, and callers would have paid a 64 MiB derivation while holding
// the manager's write mutex.
//
// The caller must NOT hold m.mu.
func (m *Manager) confirmPassword(id, password string) error {
	if password == "" {
		return fmt.Errorf("%w: the space password is required to reveal a secret", ErrPasswordRequired)
	}
	m.mu.RLock()
	file, ok := m.files[id]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err := m.checkUnlockCooldown(id); err != nil {
		return err
	}

	releaseKDF := m.acquireKDF()
	var payload Payload
	key, err := vault.UnlockJSON(password, file.Vault, aad(id), &payload)
	releaseKDF()
	if err != nil {
		if errors.Is(err, vault.ErrInvalidPassword) {
			m.recordUnlockFailure(id)
		}
		return err
	}
	key.Clear()
	clearPayload(&payload)
	m.clearUnlockFailures(id)

	return nil
}

func (m *Manager) unlockedLocked(id string) (File, *Payload, error) {
	m.expireLocked()
	file, ok := m.files[id]
	if !ok {
		return File{}, nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	active := m.sessions[id]
	if active == nil {
		return File{}, nil, ErrLocked
	}
	active.lastUsed = m.now()
	// File is a value, but its Accounts slice and the maps inside each account
	// otherwise still alias the authoritative entry in m.files. Mutating a
	// shallow copy before an AtomicWrite failure would publish an in-memory
	// state that was never persisted.
	file.Accounts = cloneAccounts(file.Accounts)
	return file, &active.payload, nil
}

func (m *Manager) resealAndSaveLocked(file *File, payload *Payload, id string) error {
	if err := m.resealAndSaveQuietLocked(file, payload, id); err != nil {
		return err
	}
	m.markDirty(id)
	return nil
}

// resealAndSaveQuietLocked is resealAndSaveLocked for a change sync made
// itself, which must not ask for another pass.
func (m *Manager) resealAndSaveQuietLocked(file *File, payload *Payload, id string) error {
	active := m.sessions[id]
	if active == nil || active.key == nil {
		return ErrLocked
	}
	container, err := active.key.SealJSON(payload, aad(id))
	if err != nil {
		return err
	}
	file.Vault = container
	return m.writeFileLocked(file, payload)
}

// saveLocked writes a space after a change made here, and asks sync to pass
// the change on.
func (m *Manager) saveLocked(file *File) error {
	var payload *Payload
	if active := m.sessions[file.ID]; active != nil {
		payload = &active.payload
	}
	if err := m.writeFileLocked(file, payload); err != nil {
		return err
	}
	m.markDirty(file.ID)
	return nil
}

// writeFileLocked writes a change to a space: its revision is raised in the
// same write. With payload — the space is open — the change is then sealed
// into the outbox for the folder (see prepareOutboxLocked), so that it is
// ready to send even if the space locks before the next pass.
func (m *Manager) writeFileLocked(file *File, payload *Payload) error {
	previous := file.LocalRevision
	file.LocalRevision = m.files[file.ID].LocalRevision + 1
	if err := m.storeFileLocked(*file); err != nil {
		file.LocalRevision = previous
		return err
	}
	if payload != nil {
		// A phrase given to the space just now (AddRecoveryPhrase) gives it a tag.
		m.rememberSpaceTagLocked(file.ID)
		m.prepareOutboxLocked(*file, payload)
	}
	return nil
}

// storeFileLocked writes a space as it is.
func (m *Manager) storeFileLocked(file File) error {
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode space: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(m.home, "spaces", file.ID, "space.json")
	return storage.AtomicWrite(path, data)
}

func (m *Manager) summaryLocked(file File) Summary {
	active, unlocked := m.sessions[file.ID]
	hasPhrase := unlocked && len(active.payload.Mnemonic) > 0
	seeded := false
	for _, item := range file.Accounts {
		if item.Kind == account.KindDerived {
			seeded = true
			break
		}
	}
	return Summary{
		ID: file.ID, Name: file.Name, Locked: !unlocked, Seeded: seeded, HasPhrase: hasPhrase,
		AccountCount: len(file.Accounts), CreatedAt: file.CreatedAt, UpdatedAt: file.UpdatedAt,
	}
}

func (m *Manager) expireLocked() {
	if m.autoLock <= 0 {
		return
	}
	now := m.now()
	for id, active := range m.sessions {
		if now.Sub(active.lastUsed) >= m.autoLock {
			m.clearSessionLocked(id)
		}
	}
}

func (m *Manager) lockLoop() {
	defer close(m.done)
	timer := time.NewTimer(m.lockInterval())
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			m.mu.Lock()
			m.expireLocked()
			interval := m.lockIntervalLocked()
			m.mu.Unlock()
			timer.Reset(interval)
		case <-m.reset:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(m.lockInterval())
		case <-m.stop:
			return
		}
	}
}

func (m *Manager) lockInterval() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lockIntervalLocked()
}

func (m *Manager) lockIntervalLocked() time.Duration {
	if m.autoLock > 0 && m.autoLock < time.Minute {
		return m.autoLock
	}
	return time.Minute
}

func (m *Manager) clearSessionLocked(id string) {
	active := m.sessions[id]
	if active == nil {
		return
	}
	clearPayload(&active.payload)
	active.key.Clear()
	active.syncKeys.Clear()
	delete(m.sessions, id)
}

func clearPayload(payload *Payload) {
	clear(payload.Mnemonic)
	clear(payload.BIP39Passphrase)
	clearKeys(payload.ImportedKeys)
}

// clearKeys wipes every variant of every key and empties the map.
func clearKeys(keys map[string]KeyVariants) {
	for ref, variants := range keys {
		for _, entry := range variants {
			clear(entry.PrivateKey)
		}
		delete(keys, ref)
	}
}

// normalizePayload names every key's curve — entries from before ed25519 left
// it empty, and meant secp256k1 — and keeps each reference's variants as a
// sorted set, so that one key is one variant here as it is in a snapshot.
// Nothing is removed but an exact copy of a key, and a reference with no key.
func normalizePayload(payload *Payload) {
	if payload.ImportedKeys == nil {
		payload.ImportedKeys = make(map[string]KeyVariants)
	}
	for ref, variants := range payload.ImportedKeys {
		variants = normalizeVariants(variants)
		if len(variants) == 0 {
			delete(payload.ImportedKeys, ref)
			continue
		}
		payload.ImportedKeys[ref] = variants
	}
}

func normalizeVariants(variants KeyVariants) KeyVariants {
	byID := make(map[string]KeyEntry, len(variants))
	for _, entry := range variants {
		if entry.Curve == "" {
			entry.Curve = string(account.CurveSecp256k1)
		}
		id := devicesync.VariantID(entry.Curve, entry.PrivateKey)
		current, ok := byID[id]
		if !ok {
			byID[id] = entry
			continue
		}
		if entry.ImportedAt.Before(current.ImportedAt) {
			current.ImportedAt = entry.ImportedAt
			byID[id] = current
		}
		clear(entry.PrivateKey)
	}
	out := make(KeyVariants, 0, len(byID))
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		out = append(out, byID[id])
	}
	return out
}

func verifyPayload(file File, payload Payload) error {
	if payload.Version != 1 {
		return errors.New("unsupported vault payload")
	}
	var seed []byte
	if len(payload.Mnemonic) > 0 {
		var err error
		seed, err = seedOf(payload.Mnemonic, payload.BIP39Passphrase)
		if err != nil {
			return err
		}
		defer clear(seed)
	}
	for _, item := range file.Accounts {
		if err := verifyAccount(item, seed, payload.ImportedKeys); err != nil {
			return err
		}
	}
	return nil
}

// seedOf is the space's BIP39 seed. Everything that checks several wallets
// derives it once and hands it on: it is a PBKDF2 of 2048 rounds, and doing it
// per wallet made checking a large space take seconds.
var seedOf = func(mnemonic, passphrase []byte) ([]byte, error) {
	return account.Seed(string(mnemonic), string(passphrase))
}

// verifyAccount checks one wallet against the secrets it claims to stand for:
// a derived wallet's address is derived again from the phrase, an imported
// wallet's reference must hold a key that produces its address. It is what makes a
// wallet list read from a file — on disk, in a backup, from another machine —
// something the vault vouches for rather than something it was told.
//
// seed is the space's BIP39 seed, or nil for a space without a phrase.
func verifyAccount(item account.Account, seed []byte, keys map[string]KeyVariants) error {
	switch item.Kind {
	case account.KindDerived:
		if item.Index == nil || len(seed) == 0 {
			return errors.New("derived account cannot be verified")
		}
		if item.Family == "" {
			// Pre-binding records projected one index into both families.
			// They remain readable but unavailable to networks until the
			// user explicitly assigns their original network.
			addresses, err := account.DerivedAddressesFromSeed(seed, *item.Index)
			if err != nil {
				return err
			}
			if addresses[account.FamilyTron] != item.Addresses[account.FamilyTron] ||
				addresses[account.FamilyEVM] != item.Addresses[account.FamilyEVM] {
				return errors.New("derived account address does not match vault")
			}
			return nil
		}
		address, err := account.DerivedAddressFromSeed(seed, item.Family, *item.Index)
		if err != nil {
			return err
		}
		if address != item.Addresses[item.Family] {
			return errors.New("derived account address does not match vault")
		}
	case account.KindImported:
		if _, err := keyFor(item, keys); err != nil {
			return err
		}
	default:
		return errors.New("unknown account kind")
	}
	return nil
}

// keyFor is an imported wallet's key: the variant of its reference that passes
// verifyImportedKey — its curve, its fingerprint and every address it records.
// A fingerprint alone is not enough, and the first variant is not the key:
// two machines may have put different keys under one reference, and a wallet
// signs only with the key of its own address.
func keyFor(item account.Account, keys map[string]KeyVariants) (KeyEntry, error) {
	variants := keys[item.KeyRef]
	if len(variants) == 0 {
		return KeyEntry{}, errors.New("imported account key is missing")
	}
	var err error
	for _, entry := range variants {
		if err = verifyImportedKey(item, entry); err == nil {
			return entry, nil
		}
	}
	if len(variants) > 1 {
		return KeyEntry{}, fmt.Errorf("none of the %d keys under the wallet's reference is its key: %w", len(variants), err)
	}
	return KeyEntry{}, err
}

func findAccount(items []account.Account, id string) (account.Account, error) {
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return account.Account{}, fmt.Errorf("%w: %s", ErrAccountNotFound, id)
}

// signingKey is an account's private key on whichever curve its family uses.
// Exactly one field is set.
type signingKey struct {
	secp256k1 *ecdsa.PrivateKey
	ed25519   ed25519.PrivateKey
}

func (k signingKey) clear() {
	if k.secp256k1 != nil && k.secp256k1.D != nil {
		k.secp256k1.D.SetInt64(0)
	}
	clear(k.ed25519)
}

// export is the private-key export format of the key's curve: 32-byte hex
// for secp256k1, the base58 Solana keypair for ed25519.
func (k signingKey) export() (string, error) {
	switch {
	case k.secp256k1 != nil:
		keyBytes := crypto.FromECDSA(k.secp256k1)
		defer clear(keyBytes)
		return hex.EncodeToString(keyBytes), nil
	case k.ed25519 != nil:
		return account.Ed25519KeypairBase58(k.ed25519)
	default:
		return "", errors.New("signer is cleared")
	}
}

// entryCurve is the curve of an imported key. Every entry written so far
// says secp256k1; an empty value is read the same way, because before ed25519
// existed the field was informational and nothing checked it.
func entryCurve(entry KeyEntry) (account.Curve, error) {
	switch account.Curve(entry.Curve) {
	case account.CurveSecp256k1, "":
		return account.CurveSecp256k1, nil
	case account.CurveEd25519:
		return account.CurveEd25519, nil
	default:
		return "", fmt.Errorf("imported key has unknown curve %q", entry.Curve)
	}
}

// parseImportedKey reads a private key offered for import on curve and
// returns what the vault stores (the secp256k1 scalar or the ed25519 seed),
// the key's fingerprint and its addresses. The caller clears raw.
func parseImportedKey(
	curve account.Curve, value string,
) (raw []byte, fingerprint string, addresses map[account.Family]string, err error) {
	switch curve {
	case account.CurveSecp256k1:
		key, raw, err := account.ParsePrivateKey(value)
		if err != nil {
			return nil, "", nil, err
		}
		fingerprint, err := account.Fingerprint(key)
		if err != nil {
			clear(raw)
			return nil, "", nil, err
		}
		addresses, err := account.ImportedAddresses(key)
		if err != nil {
			clear(raw)
			return nil, "", nil, err
		}
		return raw, fingerprint, addresses, nil
	case account.CurveEd25519:
		key, err := account.ParseEd25519PrivateKey(value)
		if err != nil {
			return nil, "", nil, err
		}
		defer clear(key)
		fingerprint, err := account.Ed25519Fingerprint(key)
		if err != nil {
			return nil, "", nil, err
		}
		addresses, err := account.ImportedEd25519Addresses(key)
		if err != nil {
			return nil, "", nil, err
		}
		return append([]byte(nil), key.Seed()...), fingerprint, addresses, nil
	default:
		return nil, "", nil, fmt.Errorf("%w: curve %s", account.ErrUnsupportedFamily, curve)
	}
}

// verifyImportedKey checks an imported account against its vault entry on
// unlock: the key has to match the account's fingerprint and produce exactly
// the account's addresses, because those addresses are where funds are
// received. A secp256k1 key has its Tron and EVM addresses and nothing else,
// as every import has stored since the first release; an ed25519 key has its
// one Solana address.
func verifyImportedKey(item account.Account, entry KeyEntry) error {
	curve, err := entryCurve(entry)
	if err != nil {
		return err
	}
	switch curve {
	case account.CurveSecp256k1:
		key, err := crypto.ToECDSA(entry.PrivateKey)
		if err != nil {
			return errors.New("imported account key is invalid")
		}
		defer key.D.SetInt64(0)
		fingerprint, err := account.Fingerprint(key)
		if err != nil || fingerprint != item.Fingerprint {
			return errors.New("imported account fingerprint does not match vault")
		}
		addresses, err := account.ImportedAddresses(key)
		if err != nil || !maps.Equal(addresses, item.Addresses) {
			return errors.New("imported account address does not match vault")
		}
		return nil
	case account.CurveEd25519:
		if len(entry.PrivateKey) != ed25519.SeedSize {
			return errors.New("imported account key is invalid")
		}
		key := ed25519.NewKeyFromSeed(entry.PrivateKey)
		defer clear(key)
		fingerprint, err := account.Ed25519Fingerprint(key)
		if err != nil || fingerprint != item.Fingerprint {
			return errors.New("imported account fingerprint does not match vault")
		}
		addresses, err := account.ImportedEd25519Addresses(key)
		if err != nil || !maps.Equal(addresses, item.Addresses) {
			return errors.New("imported account address does not match vault")
		}
		return nil
	default:
		// Unreachable: entryCurve returns one of the two curves above.
		return fmt.Errorf("imported key has unknown curve %q", entry.Curve)
	}
}

// privateKeyFor resolves the account's key for family. A derived account is
// derived along its family's path; an imported account uses the variant of its
// reference that is its own key (keyFor), and only for a family on that key's
// curve, so an ed25519 key is never offered to Tron or EVM, nor a secp256k1
// key to Solana.
func privateKeyFor(item account.Account, payload *Payload, family account.Family) (signingKey, error) {
	curve, err := account.CurveOf(family)
	if err != nil {
		return signingKey{}, err
	}
	switch item.Kind {
	case account.KindDerived:
		if item.Index == nil {
			return signingKey{}, errors.New("derived account has no index")
		}
		// A pre-binding record has no family, only the Tron and EVM addresses
		// it was created with. Its key is offered for those and nothing else:
		// any other family's derivation would be an address the wallet never
		// showed and unlock never verified.
		if (item.Family == "" && item.Addresses[family] == "") ||
			(item.Family != "" && item.Family != family) {
			return signingKey{}, ErrNetworkBinding
		}
		mnemonic, passphrase := string(payload.Mnemonic), string(payload.BIP39Passphrase)
		switch curve {
		case account.CurveSecp256k1:
			key, err := account.DerivePrivateKey(mnemonic, passphrase, family, *item.Index)
			if err != nil {
				return signingKey{}, err
			}
			return signingKey{secp256k1: key}, nil
		case account.CurveEd25519:
			key, err := account.DeriveEd25519PrivateKey(mnemonic, passphrase, family, *item.Index)
			if err != nil {
				return signingKey{}, err
			}
			return signingKey{ed25519: key}, nil
		default:
			// Unreachable: CurveOf returns one of the two curves above.
			return signingKey{}, fmt.Errorf("%w: %s", account.ErrUnsupportedFamily, family)
		}
	case account.KindImported:
		entry, err := keyFor(item, payload.ImportedKeys)
		if err != nil {
			return signingKey{}, err
		}
		stored, err := entryCurve(entry)
		if err != nil {
			return signingKey{}, err
		}
		if stored != curve {
			return signingKey{}, ErrNetworkBinding
		}
		switch stored {
		case account.CurveSecp256k1:
			keyBytes := append([]byte(nil), entry.PrivateKey...)
			defer clear(keyBytes)
			key, err := crypto.ToECDSA(keyBytes)
			if err != nil {
				return signingKey{}, errors.New("imported key is invalid")
			}
			return signingKey{secp256k1: key}, nil
		case account.CurveEd25519:
			if len(entry.PrivateKey) != ed25519.SeedSize {
				return signingKey{}, errors.New("imported key is invalid")
			}
			return signingKey{ed25519: ed25519.NewKeyFromSeed(entry.PrivateKey)}, nil
		default:
			// Unreachable: entryCurve returns one of the two curves above.
			return signingKey{}, fmt.Errorf("imported key has unknown curve %q", entry.Curve)
		}
	default:
		return signingKey{}, errors.New("unknown account kind")
	}
}

// localSigner is the secp256k1 chain.DigestSigner.
type localSigner struct {
	key    *ecdsa.PrivateKey
	family account.Family
}

func (s *localSigner) Family() chain.Family {
	return chain.Family(s.family)
}

func (s *localSigner) PublicKey() []byte {
	if s == nil || s.key == nil {
		return nil
	}
	return crypto.FromECDSAPub(&s.key.PublicKey)
}

func (s *localSigner) SignDigest(ctx context.Context, digest []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.key == nil {
		return nil, errors.New("signer is cleared")
	}
	if len(digest) != 32 {
		return nil, errors.New("digest must be 32 bytes")
	}
	signature, err := crypto.Sign(digest, s.key)
	if err != nil {
		return nil, fmt.Errorf("sign digest: %w", err)
	}
	return signature, nil
}

func (s *localSigner) clear() {
	if s == nil || s.key == nil {
		return
	}
	s.key.D.SetInt64(0)
	s.key = nil
}

// ed25519Signer is the chain.MessageSigner.
type ed25519Signer struct {
	key    ed25519.PrivateKey
	family account.Family
}

func (s *ed25519Signer) Family() chain.Family {
	return chain.Family(s.family)
}

func (s *ed25519Signer) PublicKey() []byte {
	if s == nil || len(s.key) != ed25519.PrivateKeySize {
		return nil
	}
	return append([]byte(nil), s.key[ed25519.SeedSize:]...)
}

func (s *ed25519Signer) SignMessage(ctx context.Context, message []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || len(s.key) != ed25519.PrivateKeySize {
		return nil, errors.New("signer is cleared")
	}
	// No transaction serialises to nothing; an empty message is a caller
	// that lost what it meant to sign.
	if len(message) == 0 {
		return nil, errors.New("message is empty")
	}
	return ed25519.Sign(s.key, message), nil
}

func (s *ed25519Signer) clear() {
	clear(s.key)
	s.key = nil
}

func cloneAccounts(items []account.Account) []account.Account {
	out := make([]account.Account, len(items))
	for i := range items {
		out[i] = items[i].Clone()
	}
	return out
}

// networkList is the sorted set of networks a wallet is created on.
func networkList(ids []string) ([]string, error) {
	if len(ids) == 0 || slices.Contains(ids, "") {
		return nil, errors.New("network is required")
	}
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out), nil
}

func appendNetwork(ids []string, networkID string) []string {
	if slices.Contains(ids, networkID) {
		return ids
	}
	ids = append(ids, networkID)
	sort.Strings(ids)
	return ids
}

func aad(id string) []byte {
	return fmt.Appendf(nil, "%s:%d", id, SchemaVersion)
}

func newID(prefix string) (string, error) {
	return storage.NewID(prefix)
}
