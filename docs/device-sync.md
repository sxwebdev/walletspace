# Device sync — design and plan

Status: agreed design, **not implemented**
Written: 2026-10-26 against `bc12eb8`
Scope: keeping the spaces of one person current across several desktop
machines, and recovering every space — imported keys included — from its
recovery phrase alone.

## Goals, in priority order

1. **Funds cannot be lost.** A dead laptop must never take funds with it. Today
   HD wallets come back from the phrase, imported private keys do not: they
   live only in the space vault on that machine. This is the gap the design
   closes.
2. **Funds cannot be stolen.** Nothing that leaves the machine may be weaker
   than what already sits in `~/.walletspace`. In particular, nothing in a
   cloud folder may be attackable by guessing a password offline.
3. **Sit at another machine and see every space current.** Wallets, labels,
   network bindings and imported keys made on one machine appear on the others.
4. **Convenience last.** Where convenience and the first two goals collide, the
   first two win.

Constraints:

- Desktop only (macOS, Linux). No mobile client for now.
- Transport today is a folder synced by iCloud Drive. Dropbox and Syncthing
  must work unchanged. Later, Walletspace will ship its own clients and
  possibly its own relay; the format must not care which transport carries it.
- Maintainable by one person: a small versioned format, a pure merge function
  with tests, no server to run.

## What was agreed

| Decision                                                                           | Why                                                                                                                                                                                                   |
| ---------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| The recovery phrase never leaves the machine                                       | It is the one secret that recovers the HD wallets, and it is what the sync key is derived from — it cannot be protected by a key derived from itself                                                  |
| Imported keys **do** travel, encrypted under a key derived from the seed           | So that the phrase recovers the whole space. Someone holding the phrase already owns the HD funds; the file in the cloud is unreadable without it                                                     |
| One recovery secret per space: the phrase                                          | Imported-only spaces get a generated phrase at creation that serves as the space's master secret. Without it there is nothing to recover them from                                                    |
| The sync key is derived from the seed, not from the password                       | 256 bits of entropy instead of a password; nothing in the cloud can be brute-forced; a password change is local and needs no coordination; the same seed finds the same folder, so no trial-and-error |
| One key, one folder per space                                                      | Spaces stay independent: learning one space's password or phrase reveals nothing — not even metadata — about the others                                                                               |
| One file per device, never a shared file                                           | Every file has one writer, so the sync provider never has a conflict to resolve and nothing is lost silently. Merging is done by Walletspace, not by iCloud                                           |
| Merge with tombstones; deletions are explicit records that never expire            | A replayed old snapshot cannot resurrect a deleted wallet or key, and no rollback counters are needed                                                                                                 |
| The password stays local to each machine                                           | Nothing password-encrypted goes to the cloud; the same model as MetaMask                                                                                                                              |
| A new machine is seeded by an export file (AirDrop, USB, `scp`) or by the phrase   | Both keep secrets off the cloud. The export file is exactly as sensitive as `~/.walletspace`, no more                                                                                                 |
| Sync is opt-in and off by default                                                  | The README promise "your keys stay on your machine" becomes "encrypted with a key that only your recovery phrase produces" — the user has to choose that                                              |
| Imported-only spaces stop existing as a kind (owner, 2026-10-26)                   | A new space always gets an ordinary phrase; a space made for imported keys simply derives no wallets from it. One code path instead of two                                                            |
| Existing spaces without a phrase get one by an explicit action (owner, 2026-10-26) | Space settings → "Create a recovery phrase", shown and confirmed like a new space's. Until then the space does not sync, and the UI says so                                                           |
| Enabling sync needs no password step-up                                            | It writes only ciphertext under seed-derived keys; pointing the folder somewhere readable gives a token holder nothing they can decrypt                                                               |

### Rejected along the way

| Rejected                                                          | Reason                                                                                                                                                             |
| ----------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| One shared sync key for the whole installation + pairing code     | Links every space together: one leaked password exposes the metadata of all of them. The code also has to be typed on every machine (1Password's Secret Key model) |
| Keying the sync files from the space password                     | Makes every file in the cloud an offline password-guessing target; forces a password-change protocol between devices                                               |
| A password-encrypted copy of the vault in the cloud ("variant B") | Same brute-force exposure. Its only benefit — restore without the phrase — is not one we want to promise; the phrase on paper stays mandatory                      |
| A shared `space.json` in the synced folder                        | iCloud resolves conflicts silently; one device's changes vanish                                                                                                    |
| Trying an entered password against every space in the cloud       | Only needed when secrets in the cloud are password-keyed. With seed-derived keys the device computes the folder name                                               |
| Syncing `config.yaml`, networks, assets, operation stores         | Device-local by nature; nonces come from the chain anyway                                                                                                          |
| Anti-rollback generation counters                                 | Not needed: snapshots merge by timestamps and tombstones are permanent, so an old snapshot loses to newer data                                                     |

## Threat model

| Adversary                                               | Gets                                                                                               | Does not get                                                                  |
| ------------------------------------------------------- | -------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| The sync provider, or anyone reading the synced folder  | Number of spaces, number of devices per space, write times, approximate sizes                      | Names, addresses, labels, keys — everything is ciphertext under a 256-bit key |
| A thief with a locked laptop                            | The same as today: encrypted vaults, plaintext wallet list                                         | The sync key — it is never written to disk                                    |
| Someone who knows one space's password                  | That space, as today                                                                               | Anything about other spaces, in the cloud or locally                          |
| Someone who holds a space's phrase and the cloud folder | That space's HD wallets (as today) **and its imported keys** — this is the one new exposure        | Other spaces                                                                  |
| Someone who holds a space's phrase but not the folder   | That space's HD wallets, as today                                                                  | Its imported keys                                                             |
| Someone who can write to the synced folder              | Can plant files; they fail authentication (AEAD) or verification (`verifyPayload`) and are dropped | Cannot alter, inject or resurrect a wallet                                    |

The sync key is an HKDF output of the seed. It cannot be reversed to the seed:
a leaked sync key exposes a space's metadata and imported keys, never its phrase
or HD wallets.

## Layout

Local, unchanged plus one file per space:

```
~/.walletspace/
  spaces/spc_<id>/space.json      ← as today; the only state the wallet runs on
  spaces/spc_<id>/sync.json       ← public: cached space tag, device tag, last sync time
  device.json                     ← random device id, generated once per install
  config.yaml                     ← sync: enabled, folder path
```

Synced folder (the user picks it; iCloud Drive is detected by default on macOS):

```
<folder>/v1/<space_tag>/<device_tag>.wsync
```

- `space_tag` = hex of `HMAC-SHA256(K_tag, "space")`, 32 characters. Every
  device holding the seed computes the same tag; nobody else can relate it to a
  space.
- `device_tag` = hex of `HMAC-SHA256(K_tag, "device:" + device_id + ":" +
local space id)`. The same physical device gets a different tag in every
  space, so the folder does not reveal which spaces share a machine. The local
  space id is in it because one machine may hold the same phrase twice (two
  local spaces); each is then its own writer instead of both overwriting one
  file.
- The tags are not secret — they are folder names — so caching them in
  `sync.json` is safe. The cache lets a locked startup say "there are N spaces
  in the sync folder that are not on this machine" without any key.
- Walletspace writes exactly one file in the whole tree: its own
  `<device_tag>.wsync` per space. Everything else it only reads.

## Cryptography

```
seed   = BIP39 seed (mnemonic + passphrase) — already computed for derivation
K_root = HKDF-SHA256(ikm = seed, salt = "", info = "walletspace/sync/v1/root")
K_enc  = HKDF-SHA256(ikm = K_root, info = "enc")      32 bytes
K_tag  = HKDF-SHA256(ikm = K_root, info = "tag")      32 bytes
```

- Envelope: `header || nonce || ciphertext`. Header is `magic "WSYN", version
u8, space_tag, device_tag`; it is the AEAD's associated data, so a file cannot
  be moved under another tag or presented as another device's.
- AEAD: XChaCha20-Poly1305 (`golang.org/x/crypto/chacha20poly1305`, already a
  dependency), fresh random 24-byte nonce per write. The key is shared by
  several writers, which is exactly what the 192-bit nonce is for.
- Imported-only spaces take the same path: their generated phrase is their seed.
- **Key lifetime.** `K_*` are derived at unlock, kept in the space session next
  to the vault session key, cleared at lock. They are never written to disk.
  Consequently a space syncs only while it is unlocked; a locked space runs on
  its local copy and catches up at the next unlock.

## File contents

Plaintext inside the envelope, JSON. Each device writes its **full view** of the
space — a snapshot, not a log. Merging snapshots is idempotent and order-free,
which keeps the merge small and testable.

```json
{
  "version": 1,
  "written_at": "2026-10-26T12:00:00Z",
  "space": { "name": "Main", "name_updated_at": "…" },
  "accounts": [ { …account.Account as in space.json, with updated_at… } ],
  "deleted_accounts": [ { "id": "acc_…", "deleted_at": "…" } ],
  "imported_keys": { "key_…": { "curve": "…", "private_key": "…", "imported_at": "…" } },
  "deleted_keys": [ { "key_ref": "key_…", "deleted_at": "…" } ]
}
```

Not in the file: the mnemonic, the BIP39 passphrase, the vault container, the
password or anything derived from it.

Size padding to fixed buckets (hide the wallet count from the provider) is a
later nicety, not part of v1.

## Merge

One pure function in `internal/sync`: `Merge(local State, remote []Snapshot)
(State, Report)`. No I/O, no clock, fully covered by tests.

- **Accounts** are matched by `ID`. The record with the latest `UpdatedAt`
  wins. A tombstone with `DeletedAt` later than every `UpdatedAt` deletes the
  account; tombstones are kept forever (≈100 bytes each).
- **Dedupe on restore.** A space restored from its phrase on a new machine
  creates no accounts of its own; it adopts the list from the folder. If the
  user derived wallets before the first sync, derived accounts are also matched
  by `(family, derivation profile, index)` and imported accounts by `(family,
address)`, keeping the older `ID`.
- **Imported keys** are matched by `KeyRef`: union, tombstones delete.
  **A key is removed only by an explicit tombstone** — never because no account
  references it, never by a merge heuristic. This is the single most important
  rule in the design: the whole point is that imported keys stop being
  losable.
- **Name**: latest `NameUpdatedAt` wins.
- **Mnemonic** is not merged; a different seed yields a different folder, so a
  conflict cannot occur. The same phrase restored into two spaces (even with
  different passwords) therefore means the same space, and the two merge —
  this is the intended semantics, as with Trezor (same seed, same wallet).
- **Verification before save.** The merged result must pass `verifyPayload`
  (derived addresses recomputed from the seed; imported accounts must have a
  matching key). Records that fail are dropped, logged and surfaced in the UI;
  they never block the unlock and never reach disk.
- **Clocks.** Timestamps are wall-clock and a badly wrong clock can win a
  merge. Accepted for v1; a remote `written_at` far in the future is reported
  as a warning.

## Lifecycle

- **Enable** in Settings: folder path (macOS default
  `~/Library/Mobile Documents/com~apple~CloudDocs/Walletspace`), on/off for the
  installation. A space participates whenever it is unlocked.
- **Unlock**: derive keys → compute tags → read every `*.wsync` under the
  space's tag → authenticate, decode, merge, verify → save `space.json` → write
  own snapshot.
- **While unlocked**: write own snapshot after every local change (debounced);
  poll the folder every ~30 s. Polling rather than fsnotify: iCloud and Dropbox
  deliver events unreliably, and the folder is tiny.
- **Lock**: clear keys, stop.
- **Startup, all locked**: local state as today, plus the count of unknown
  space tags in the folder ("2 spaces in iCloud are not on this machine").
- **iCloud specifics**: an evicted file (`.icloud` placeholder) or a read error
  is treated as "not here yet" and retried later; never fatal. Walletspace
  never deletes or renames anything in the folder.
- **Doctor** gains a check: folder reachable, writable, own file readable back.

## Moving to a new machine and recovering

**Export file** (`.wsbackup`): the space files as `Backup` returns them today,
bundled. From the UI with the same password step-up as `Backup` (one password
entry covers every selected space it opens); from the terminal as
`walletspace export FILE` without a step-up, since the local user can read
`~/.walletspace` anyway. **Import**: UI "Import from file" on the start screen,
or `walletspace import FILE`. Spaces land locked, with the same IDs, so they
join the same sync folders at first unlock. The file is as sensitive as
`~/.walletspace` and should be deleted after import.

**Phrase**: "Restore a space" with the phrase (and BIP39 passphrase), a new
password. The first unlock derives the sync key, finds the folder and adopts
wallets, labels, networks and imported keys.

**Dead laptop**: the phrase restores everything, provided the space synced at
least once after the last import. The UI shows per space when its snapshot was
last written, so "never synced" is visible, not silent.

**Password change**: local to the machine. Other machines keep their own
password; nothing to coordinate.

## Transport abstraction

Everything above is a set of opaque blobs addressed by `(space_tag,
device_tag)`. The folder is one store; a future Walletspace relay or client is
another, and sees exactly what iCloud sees: tags and ciphertext.

```go
type Store interface {
    List(ctx, spaceTag) ([]Entry, error)   // device tags + modification times
    Get(ctx, spaceTag, deviceTag) ([]byte, error)
    Put(ctx, spaceTag, deviceTag, blob) error
}
```

Format and merge live in `internal/sync` with golden test vectors (fixed seed,
fixed inputs, expected envelope and merge output) so that any later client, in
any language, is tested against the same vectors.

## Risks and mitigations

| Risk                                                       | Mitigation                                                                                                                                                                                     |
| ---------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| A merge bug drops an imported key                          | Tombstone-only deletion; golden and property tests (merge is commutative, associative, idempotent); `space.json` is kept as `space.json.prev` before every merge that removes a key or account |
| Secrets in the cloud for the first time                    | Only imported keys, only under a seed-derived key; opt-in; README and Settings say so plainly                                                                                                  |
| iCloud evicts or lags files                                | Read failures are non-fatal; the UI shows last-written times per device                                                                                                                        |
| Clock skew wins a merge                                    | Warning on far-future timestamps; accepted for v1                                                                                                                                              |
| A new space made on one machine does not appear on another | By design — its secrets are not in the cloud. The unknown-tag count tells the user; export or phrase brings it over                                                                            |
| Same phrase, two spaces                                    | Intended: they merge. Creating or restoring a space whose folder already exists offers to adopt it                                                                                             |
| New crypto and merge code, one author, no audit            | Keep `internal/sync` small and pure; vectors; an addendum to `docs/security-audit.md` once implemented; a second pair of eyes on the merge before release                                      |

## Implementation plan

Stages are independent deliverables; each lands on its own.

1. **Export / import bundle.** Standalone value, no cryptography changes.
   `Manager.ExportBundle` / `ImportBundle`; `POST /api/spaces/export`,
   `POST /api/spaces/import`; start-screen import and Settings export in the
   UI; `walletspace export|import` in `cmd/walletspace`.
2. **`internal/sync`: format, envelope, merge.** Pure, with golden vectors and
   property tests. Changes in `internal/space`: `DeletedAccounts` and
   `DeletedKeys` tombstones and `NameUpdatedAt` on `File`; `DeleteAccount`
   records tombstones; imported-only spaces generate a phrase at creation
   (`Create` currently refuses one) and the UI shows it to be written down.
3. **Folder store and integration.** `Store` for a folder; polling; sync on
   unlock, write after change, stop on lock; `sync.json` tag cache;
   `space.json.prev` safety copy; Doctor check.
4. **Settings and UI.** Enable/disable, folder path with iCloud detection,
   per-space last-written times, unknown-space count, dropped-record warnings.
5. **Docs.** README "How keys are protected" and "Data and settings"; this
   document marked implemented with the revision; `docs/security-audit.md`
   addendum covering the new surface.

## Open questions

- Per-space opt-out of sync, or installation-wide only? (v1: installation-wide.)
- Fixed-size padding of snapshots — worth the complexity?
- Wording when an imported-only space shows its generated phrase: it must be
  clear that this phrase recovers the space but derives no wallets.
- Linux has no default folder; the user picks one. Should Syncthing be the
  documented default there?
