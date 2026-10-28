# Device sync — agreed rev 2 specification

Written: 2026-10-26 against `bc12eb8`. Revised: 2026-10-27 after design and
implementation reviews.

Scope: keeping the spaces of one person current across several desktop
machines, and recovering every space — imported keys included — on a new
machine from its recovery phrase, its BIP39 passphrase if set, and a current
copy of its files in the sync folder.

Revision 1 and rev 2 — the changes agreed after the reviews of 2026-10-27 —
are implemented and tested in the working tree, not committed or released. The
marker _(rev 2)_ identifies the rev 2 requirements, and
[implementation status](#implementation-status) lists them. Revision
numbers here describe the design; the unreleased wire format remains version 1.

## Goals, in priority order

1. **Preserve access to funds.** Keep every imported key until the person
   deletes its wallet, and make it recoverable after
   loss of a laptop once a current external copy exists. Before sync, imported
   keys lived only in that machine's vault. An unsent change or a lost or
   rolled-back external copy remains a recovery limit; the phrase cannot
   regenerate independently imported keys.
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
  possibly its own relay; the format must not care which transport carries it,
  and everything a second implementation needs to reproduce is specified here
  and pinned by test vectors.
- Maintainable by one person: a small versioned format, a pure merge function
  with tests, no server to run.

## What was agreed

| Decision                                                                                | Why                                                                                                                                                                                                                                 |
| --------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| The recovery phrase and the BIP39 passphrase never leave the machine                    | They recover the HD wallets, and the sync key is derived from them — they cannot be protected by a key derived from themselves                                                                                                      |
| Imported keys **do** travel, encrypted under a key derived from the seed                | So that the phrase recovers the whole space. Someone holding the phrase already owns the HD funds; the file in the cloud is unreadable without it                                                                                   |
| One recovery secret per space: the phrase (with its BIP39 passphrase, if it has one)    | Every new space gets a phrase; older spaces without one get it by an explicit action. Without it there is nothing to recover them from                                                                                              |
| The sync key is derived from the seed, not from the password                            | The phrase's entropy (128 bits for 12 words, 256 for 24) instead of a password; nothing in the cloud can be brute-forced; a password change is local and needs no coordination; the same seed finds the same folder                 |
| One key, one folder per space                                                           | Spaces stay independent: learning one space's password or phrase reveals nothing — not even metadata — about the others                                                                                                             |
| One file per device, never a shared file                                                | Every file has one writer, so the sync provider never has a conflict to resolve and nothing is lost silently. Merging is done by Walletspace, not by iCloud                                                                         |
| Merge with tombstones; deletions are explicit records that never expire                 | A replayed old snapshot cannot resurrect a deleted wallet **as long as one participant of the merge holds the tombstone**. See [Rollback](#rollback) for what this does not cover                                                   |
| The password stays local to each machine                                                | Nothing password-encrypted goes to the cloud; the same model as MetaMask                                                                                                                                                            |
| A new machine is seeded by an export file (AirDrop, USB, `scp`) or by the phrase        | Both keep secrets off the cloud. The export file is exactly as sensitive as `~/.walletspace`, no more                                                                                                                               |
| Sync is opt-in and off by default                                                       | The README promise "your keys stay on your machine" becomes "encrypted with a key that only your recovery phrase produces" — the user has to choose that                                                                            |
| Imported-only spaces stop existing as a kind (owner, 2026-10-26)                        | A new space always gets an ordinary phrase; a space made for imported keys simply derives no wallets from it. One code path instead of two                                                                                          |
| Existing spaces without a phrase get one by an explicit action (owner, 2026-10-26)      | Space settings → "Create a recovery phrase". A space copied to several machines gets its phrase on one and the same phrase entered on the others                                                                                    |
| Enabling sync needs no password step-up                                                 | It writes only ciphertext under seed-derived keys; pointing the folder somewhere readable gives a token holder nothing they can decrypt                                                                                             |
| _(rev 2)_ Sync state is delivered from a local outbox, not at lock                      | Locking must stay fast and must not depend on the folder. The sealed snapshot is prepared on every change; locking only clears keys                                                                                                 |
| _(rev 2)_ Delivery is tracked by revisions, not a flag                                  | A flag can be cleared by a write that finished after a newer change. Revisions say exactly which state reached which stage                                                                                                          |
| _(rev 2)_ A key reference holds a set of variants; each wallet picks the one it matches | Two machines may hold different keys under one reference. Keeping only one loses a secret; keeping "our own" breaks convergence. A canonical set is the same everywhere, and a wallet signs only with the key of its address        |
| Deleting an imported wallet deletes its key, everywhere (owner, 2026-10-27)             | That is what the person asks for, and a key kept after its wallet — a leaked one, say — would stay in every vault and in the cloud. Every import gets a reference of its own, so no other wallet loses its key                      |
| _(rev 2)_ The device id is bound to the machine and the data folder                     | A copied or restored `~/.walletspace` must not make two installations write the same file                                                                                                                                           |
| _(rev 2)_ Key tombstones are kept inside the vault (owner, 2026-10-27)                  | A plaintext tombstone added to a backup file in transit would delete a key on every machine. Inside the vault it is authenticated by the password; account tombstones stay plaintext, since without the key they only hide a wallet |
| _(rev 2)_ Record comparison is part of the protocol                                     | Ties are settled by a canonical encoding any implementation can reproduce, not by whatever one language's JSON library emits                                                                                                        |

### Rejected along the way

| Rejected                                                                       | Reason                                                                                                                                                                                                                                              |
| ------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| One shared sync key for the whole installation + pairing code                  | Links every space together: one leaked password exposes the metadata of all of them. The code also has to be typed on every machine (1Password's Secret Key model)                                                                                  |
| Keying the sync files from the space password                                  | Makes every file in the cloud an offline password-guessing target; forces a password-change protocol between devices                                                                                                                                |
| A password-encrypted copy of the vault in the cloud ("variant B")              | Same brute-force exposure. Its only benefit — restore without the phrase — is not one we want to promise; the phrase on paper stays mandatory                                                                                                       |
| A shared `space.json` in the synced folder                                     | iCloud resolves conflicts silently; one device's changes vanish                                                                                                                                                                                     |
| Trying an entered password against every space in the cloud                    | Only needed when secrets in the cloud are password-keyed. With seed-derived keys the device computes the folder name                                                                                                                                |
| Syncing `config.yaml`, networks, assets, operation stores                      | Device-local by nature. This leaves a real gap for transfers whose outcome is unknown — see [Sending from two machines](#sending-from-two-machines) — which syncing these files would not close either                                              |
| Automatically folding duplicate wallets                                        | Whether the older wallet is still alive depends on what each machine has read; a fold decided on part of the picture deletes a wallet the whole picture keeps. Found by the merge property test                                                     |
| Anti-rollback counters in the snapshots                                        | A counter in the same folder rolls back with it. Detecting a rollback needs trusted knowledge of a newer state, which only a surviving device has. See [Rollback](#rollback)                                                                        |
| Flushing to the folder when a space locks                                      | Locking would wait on folder I/O and fail with it. Replaced by the outbox _(rev 2)_                                                                                                                                                                 |
| "Keep this machine's key" on a key conflict                                    | Keeps both secrets only if the other machine survives, and leaves the reference meaning different keys on different machines. Replaced by key variants _(rev 2)_                                                                                    |
| Keeping a key after its wallet is deleted, with a separate "delete key" action | A deleted key would stay in every vault and in the folder unless the person found a second action, and the key count would only grow toward the snapshot limit. Walletspace never puts two wallets on one reference, so there is nothing to protect |
| Detecting a cloned install by "my tag, not my file"                            | The provider can hand back an older version of one's own file, and a clone has the same ids and keys: the signal is ambiguous. Replaced by the machine-bound device id _(rev 2)_                                                                    |

## Threat model

| Adversary                                                | Gets                                                                                                               | Does not get                                                                                      |
| -------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------- |
| The sync provider, or anyone reading the synced folder   | Number of spaces, number of devices per space, write times, approximate sizes                                      | Names, addresses, labels, keys — everything is ciphertext under a 256-bit key                     |
| A thief with a locked laptop                             | The same as before sync: encrypted vaults, plaintext wallet list; _(rev 2)_ the outbox, ciphertext like the folder | The sync key — it is never written to disk                                                        |
| Someone who knows one space's password                   | That space, as before                                                                                              | Anything about other spaces, in the cloud or locally                                              |
| Someone who holds a space's phrase and the cloud folder  | That space's HD wallets (as before) **and its imported keys** — this is the one new exposure                       | Other spaces                                                                                      |
| Someone who holds a space's phrase but not the folder    | That space's HD wallets, as before                                                                                 | Its imported keys                                                                                 |
| Someone who can write to the synced folder, without seed | Can plant, delete or roll back files. Planted files fail authentication; deleted or rolled-back ones see below     | Cannot read anything, cannot inject a wallet or a key                                             |
| Someone who can alter a backup file or `space.json`      | Can add or remove plaintext wallet records and account tombstones                                                  | _(rev 2)_ Cannot delete a key: key tombstones are inside the password-encrypted vault             |
| Someone who can write to the folder **and** has the seed | Can plant wallets and keys of their own; every wallet is verified against the seed and its key before it is kept   | Cannot replace a key a wallet here uses; cannot make a wallet sign with a key that is not its own |

The sync key is an HKDF output of the seed. It cannot be reversed to the seed:
a leaked sync key exposes a space's metadata and imported keys, never its phrase
or HD wallets.

### Rollback

AEAD authenticates a file; it does not say whether the file is the latest one
([RFC 5116 §1.2](https://www.rfc-editor.org/rfc/rfc5116.html#section-1.2)
leaves replay protection to the protocol). What protects against old
files is the merge: tombstones are permanent, so an old snapshot loses to any
participant that holds the newer state. A surviving `space.json` protects only
the updates and tombstones that machine has already observed. This does **not**
cover a machine that starts from nothing:

- **A deleted wallet can come back.** The laptop that deleted it is lost, and
  the folder — rolled back by the provider, by a restore, or by someone with
  write access — holds snapshots from before the deletion. The restore adopts
  the wallet again. It is the person's own wallet, verified against their seed,
  not one an attacker chose; the harm is that a deliberately removed address
  is offered again.
- **Newer imported keys can be missing.** The same rolled-back folder simply
  does not contain keys imported after its snapshots. This is the dangerous
  half: the restore looks complete and is not.

Neither is detectable from the folder alone: counters and timestamps in it
roll back with it. v1 states the limit instead of hiding it: recovery
completeness depends on the folder being current, the UI shows when each space
was last written to the folder and whether changes are still unsent _(rev 2)_,
and the export file and the paper phrase remain the backups of record.

## Layout

Local:

```
~/.walletspace/
  spaces/spc_<id>/space.json      ← the only state the wallet runs on; (rev 2) carries local_revision;
                                    key tombstones live inside its vault (rev 2)
  spaces/spc_<id>/sync.json       ← public: cached space tag; (rev 2) device id and tag, delivery revisions
  spaces/spc_<id>/outbox.wsync    ← (rev 2) the next snapshot to deliver, sealed
  spaces/spc_<id>/space.json.prev ← the space before a merge that removed something
  device.json                     ← device id; (rev 2) plus the machine binding
  config.yaml                     ← sync: enabled, folder path
```

Synced folder (the user picks it; iCloud Drive is suggested on macOS):

```
<folder>/v1/<space_tag>/<device_tag>.wsync
```

- `space_tag` = the first 16 bytes of `HMAC-SHA256(K_tag, "space")`, lowercase
  hex (32 characters). Every device holding the seed computes the same tag;
  nobody else can relate it to a space.
- `device_tag` = the first 16 bytes of
  `HMAC-SHA256(K_tag, "device:" + device_id + ":" + local space id)`, lowercase
  hex. A device gets a different tag in every space, so the folder does not
  reveal which spaces share a machine. The local space id is in it because one
  machine may hold the same phrase twice (two local spaces); each is then its
  own writer.
- The tags are not secret — they are folder names — so caching them in
  `sync.json` is safe. The cache lets a locked startup recognise its folders
  without any key.
- Walletspace owns one snapshot path per space: its own
  `<device_tag>.wsync`. Atomic replacement may use and rename a temporary file
  it owns; it never overwrites, renames or deletes another writer's snapshot.

### Device identity _(rev 2)_

`device.json` holds the random `device_id` and a `binding`:
`SHA-256(machine id ‖ absolute path of the data folder)`, where the machine id
is `IOPlatformUUID` on macOS and `/etc/machine-id` on Linux. At start, a binding
that does not match means the installation was moved or copied: a new
`device_id` is made, and with it new device tags, so the copy becomes a writer
of its own. The path is part of the binding because two data folders on one
machine (`WALLETSPACE_HOME`) holding the same imported space would otherwise
share a tag.

The machine id is `ioreg`'s `IOPlatformUUID` on macOS (no cgo) and
`/etc/machine-id`, then `/var/lib/dbus/machine-id`, elsewhere. When it cannot
be read, the stored id is kept unchanged; a new installation then gets an id
with no binding, adopted at the first start that can read the machine id. A
`device.json` from before rev 2 adopts the current binding and keeps its id.
The path is used as written, without resolving symlinks, so renaming the home
folder or moving `WALLETSPACE_HOME` also makes a new writer — harmless, the
old file simply stops changing.

A new `device_id` discards the outbox and the delivery records: they belong to
the old device tag. The next unlock of each space seals its state under the new
tag.

**Not supported:** a disk or virtual-machine clone that keeps the same machine
id _and_ the same data path. Both copies keep one device id and write the same
files; each overwrites the other's view, and whatever one machine changed
between the other's writes may be lost from the folder until it writes again.
The README documents this limit and the UI must do so too. Before using the
clone, stop Walletspace, remove only its `device.json`, then restart and unlock
its spaces to establish new writer tags and outboxes. Prefer the space bundle
for ordinary transfers; it does not copy the installation identity.

## Cryptography

```
seed   = BIP39 seed (mnemonic + BIP39 passphrase)
K_root = HKDF-SHA256(ikm = seed, salt = "", info = "walletspace/sync/v1/root")
K_enc  = HKDF-SHA256(ikm = K_root, info = "enc")      32 bytes
K_tag  = HKDF-SHA256(ikm = K_root, info = "tag")      32 bytes
```

- The keys are 256-bit; their strength is the phrase's: 128 bits for a
  12-word phrase, 256 for the 24 words Walletspace generates, plus whatever the
  BIP39 passphrase adds.
- Envelope: `"WSYN" ‖ version (1 byte) ‖ space_tag (32) ‖ device_tag (32) ‖
nonce (24) ‖ ciphertext`. Everything before the nonce is the AEAD's associated
  data. Each tag is its 32-byte lowercase ASCII hex representation. The reader
  checks the authenticated tags against the requested path, so moving a file
  does not turn it into another space's or device's snapshot.
- AEAD: XChaCha20-Poly1305, a fresh random 24-byte nonce per write. The key is
  shared by several writers, which is what the 192-bit nonce is for.
- **Key lifetime.** `K_*` are derived at unlock, kept in the space session,
  cleared at lock. They are never written to disk. A space reads the folder and
  merges only while unlocked; _(rev 2)_ delivering its already sealed outbox
  needs no key and happens locked or unlocked.

## File contents

Plaintext inside the envelope, JSON. Each device writes its **full view** of the
space — a snapshot, not a log. Merging snapshots is idempotent and order-free.

```json
{
  "version": 1,
  "written_at": "2026-10-26T12:00:00Z",
  "name": "Main",
  "name_updated_at": "2026-10-26T11:00:00Z",
  "accounts": [],
  "deleted_accounts": [{ "id": "acc_…", "deleted_at": "…" }],
  "imported_keys": {
    "key_…": [{ "curve": "secp256k1", "private_key": "…", "imported_at": "…" }]
  },
  "deleted_keys": [{ "id": "key_…", "deleted_at": "…" }]
}
```

_(rev 2)_ `imported_keys` maps a reference to a **list of variants**, sorted by
variant id (below). Revision 1 maps it to a single key. Sync is not released,
so the format changes in place, keeping `version: 1`, and the golden vector
must be updated and independently checked before release. Existing local vaults
and bundle imports with one key per reference must be accepted as singleton
variant sets without changing their account IDs, references or key material.

The JSON above is a shape example with placeholder IDs and key bytes. Actual
`private_key` values use standard padded base64 of the normalised key bytes;
account records follow the canonical-field rules below. Variant IDs are
recomputed from decoded key material, not trusted as external labels.

A reader refuses a snapshot _(rev 2)_ with a curve other than `secp256k1` or
`ed25519`, a key that is not 32 bytes, a reference with an empty variant list,
more than 512 wallets or 512 key references, more than 1024 variants in all, or a time that does not encode back as written
and in UTC. Files written by the first sync build, with one key per reference,
are unreadable to rev 2 and count as unreadable until that machine updates;
sync was never released, so there is no reader for both shapes. The vault takes
the list shape too: once a rev 2 build rewrites a vault, Walletspace 0.3.2 and
earlier cannot unlock it, so a downgrade, or a backup file carried to an older
install, does not open. Release notes must say so.

Not in the file: the mnemonic, the BIP39 passphrase, the vault container, the
password or anything derived from it.

Size padding to fixed buckets (hide the wallet count from the provider) is a
later nicety, not part of v1.

## Merge

One pure function in `internal/devicesync`: `Merge(snapshots, now) (Snapshot,
Report)`. No I/O; `now` only feeds the clock-skew warning. Its result does not
depend on the order of the snapshots or on how they were grouped into earlier
merges — a property test checks that on random inputs, and _(rev 2)_ shared
merge vectors pin it for other implementations.

The rules depend only on sets that only grow — the copies of each wallet, the
variants of each key, the tombstones — which is what makes the result
independent of grouping. A rule that drops a record on a judgement ("this
wallet has no key", "these two are duplicates") breaks that, and was found to
by the property test twice; such judgements belong to the space, after the
merge.

- **A deletion wins.** A wallet or key reference with a tombstone anywhere is
  gone, whatever any snapshot says about it, because ids are never reused:
  creating a wallet again makes a new id. Tombstones do not expire. A size or
  count limit must fail delivery explicitly while preserving local state;
  sending only the newest tombstones would silently weaken deletion history.
- **Wallets** are matched by `id`. The copy with the latest `updated_at` wins
  whole; equal times are settled by comparing the records' canonical encodings
  (below), and the greater wins.
- **Duplicates are reported, not folded.** Two machines that derive the same
  index or import the same key before seeing each other produce two wallets for
  one address. Both are kept, the newer is reported, the person deletes one.
- **Keys** _(rev 2)_. A reference holds the union of every variant any snapshot
  has for it. A variant is identified by
  `SHA-256(curve ‖ 0x00 ‖ normalised private key)`, lowercase hex. `curve` is
  the UTF-8 string `secp256k1` or `ed25519`; secp256k1 keys are 32
  big-endian bytes, ed25519 keys are their 32-byte seed — the forms the vault
  stores. Equal variants with different `imported_at` keep the earliest. The
  list is sorted by variant id. A key tombstone removes the reference with all
  its variants; nothing else removes a variant — not being unreferenced, not
  disagreeing with another copy. **Losing an imported key is the one failure
  this design exists to prevent.**
- **Which key a wallet uses** _(rev 2)_: the variant of its reference that
  passes the full imported-key check — its curve, its fingerprint and every
  address the wallet records; a fingerprint alone is not enough. Signing uses
  that variant, never simply the first one. A wallet with no passing variant is
  dropped by the space's verification. A reference no wallet uses keeps all
  its variants.
- **Name**: latest `name_updated_at` wins; equal times are settled by the
  greater name, compared as UTF-8 bytes. A default name has no time, so a
  restored space takes the name its other machines gave it. Names are limited
  to 256 characters where they are set.
- **Mnemonic** is not merged; a different seed is a different folder. The same
  phrase restored into two spaces means the same space, and the two merge.
- **Verification before save.** Every wallet the merge brings in or changes is
  checked against the seed (derived wallets) or its key (imported wallets).
  What fails is dropped, logged and shown; it never reaches disk and never
  blocks the unlock. The seed is derived once per pass.
- **Clocks.** Timestamps are wall-clock; a badly wrong clock can win a merge.
  Accepted for v1; a far-future timestamp is reported.

### Deleting a wallet

`DeleteAccount` removes the wallet and writes its account tombstone. For an
imported wallet it also deletes the key: the key leaves the vault, and its
reference gets a key tombstone that removes it on every machine, all its
variants included _(rev 2)_. Deleting the key is what deleting an imported
wallet means; there is no separate action. Key tombstones are kept inside
the vault _(rev 2)_, so only someone with the password or the phrase can make
one. Importing the same key again makes a new
reference and a new wallet id, and a stale snapshot cannot undo the tombstone
while a participant of the merge holds it.

Every import gets a reference of its own; importing a key the space already
has adds networks to the existing wallet. So a reference belongs to one wallet.
Two wallets on one reference can only come from a snapshot Walletspace did not
write, which takes the seed. _(rev 2)_ For that case deletion keeps the key
while another wallet of the space still uses the reference, so the remaining
wallet does not stop the space from unlocking. Nothing more is promised for it.

### Canonical encoding _(rev 2)_

Used to settle account ties; the envelope layout is not affected. Both the
winning record and its comparison object use these normalisations:

- Always include `id`, `label`, `kind`, `addresses`, `created_at`, `updated_at`.
  The empty address map is `{}`. Omit empty optional strings (`family`,
  `derivation_profile`, `key_ref`, `fingerprint`).
- Omit absent/null `index` and `imported_at`. Preserve a present `index: 0`:
  it is a valid derivation index. Indices are integers in the uint32 range.
- Represent each present timestamp in UTC as RFC 3339, with up to nine
  fractional digits and trailing zeros removed. An integral second has no
  fractional part; `.500Z` becomes `.5Z`. Compare timestamps by instant,
  before comparing encodings on a tie.
- Treat `network_ids` as a set: deduplicate, sort ascending by UTF-8 bytes,
  omit the field when empty. Preserve strings without Unicode normalisation.

Encode that object using [RFC 8785 (JCS)](https://www.rfc-editor.org/rfc/rfc8785.html),
including its recursive property ordering and string escaping. A language's
default JSON output is not the protocol. Compare the resulting UTF-8 bytes
lexicographically; the greater record wins. Equal bytes represent the same
normalised record, so input order cannot choose a different stored form.

For names, a timestamp tie uses the greater name by UTF-8 bytes. For identical
key variants, `imported_at` is the earliest instant. Output variants are sorted
by variant ID, tombstones by ID (the latest deletion time for a repeated ID),
and accounts by creation instant then ID. `written_at` is delivery metadata;
it does not choose records and is set only when sealing a new own snapshot.
Every time in a merge result is in UTC — wallet records, the name time,
tombstones and key import times — so the first pass after an update may
rewrite `space.json` once. Merge vectors must cover these choices, including equivalent timestamp and
set encodings, index zero, map order, non-ASCII text and escaping.

## Delivery _(rev 2)_

A change reaches the folder through a local outbox, so that locking never waits
on the folder and a change made just before locking is not left behind.

- **Revision.** `space.json` carries `local_revision`, raised in the same write
  as the change itself, including a change a merge brings in. A pass that
  changes nothing leaves it alone. It is local bookkeeping and never decides a
  merge.
- **Outbox.** After every change while the space is unlocked and sync is on,
  the space seals its snapshot — the same envelope as in the folder — into
  `spaces/<id>/outbox.wsync` and then records its revision as
  `outbox_revision` in `sync.json`. A crash between the two leaves the record
  older than the file, which only understates.
- **Sending** copies the outbox to this machine's file in the folder. It needs
  no key, so it runs locked or unlocked: after changes (debounced), on every
  sync tick for every space, at startup and, bounded at 2 s, on shutdown. After
  a successful write, `written_revision` becomes the outbox's revision. Sends
  and passes run one at a time, and a send reads the outbox and its revision
  together, so an older outbox never replaces a newer file; a failed send
  stays queued and shows as an error.
- **This machine's own file is never overwritten unmerged.** A home restored
  from a backup on the same machine keeps its device id, and its outbox can be
  older than its own file in the folder, which may hold keys imported since.
  So, locked, the outbox replaces only a missing own file, an identical one,
  or this machine's own last write, recognised by the SHA-256 recorded in
  `sync.json` (`written_sha256`) at every write; any other own file waits for
  an unlock, and the status says so (`merge_first`). Unlocked, every pass reads
  the own file — dataless ones included — and merges it like any other before
  writing; while it cannot be read (evicted), it counts as pending, is asked
  for, and is not overwritten. An own file that authenticates but that this
  build cannot read — written by a newer Walletspace — is never overwritten:
  this machine's changes to that space wait, and the status asks the person
  to update. One that does not authenticate is damaged and is replaced after
  an unlock. A rolled-back own file is sent again after the next unlock's
  merge. The status names the reason (`merge_first_reason`: `not_merged`,
  `not_downloaded`, `newer_version`, `merge_failed`).
- **Locking** only clears the keys; the outbox is sent while locked.
- **Upload.** Whether iCloud has uploaded the file cannot be read without cgo,
  so upload is "unknown" for every provider.
- **Changing the folder**: `written_revision` counts only for the folder it was
  recorded with, so a new folder shows unsent changes at once.

`sync.json` holds, besides the cached space tag: the installation's
`device_id` and this space's device tag (to recognise an outbox sealed by a
copy), `outbox_revision`, `folder`, `written_revision`, `written_sha256`, the
time of the last write, and whether the space has no phrase (such a space has no delivery state
at all). All of it is public.

A folder that does not answer the start-up check keeps sync on: changes are
still sealed into outboxes and shown unsent with the reason, and the check is
retried every 30 s; when it passes, delivery resumes without a restart.

The status shows one of:

| State          | When                                                              |
| -------------- | ----------------------------------------------------------------- |
| Unsent changes | `written_revision < local_revision`, or written to another folder |
| Upload unknown | Written to this folder; the provider cannot be asked              |

The outbox is re-sealed by the next pass when it is missing or older than the
space, so a crash between saving the space and sealing the outbox needs no
further edit.

If `outbox_revision < local_revision` while the space is locked, the status
says the space has unsent changes and needs an unlock to prepare them. That
follows a crash right after a change, a rename of a locked space, a backup
import, sync turned on for spaces not opened since, or an outbox discarded
after a new device id.

## Lifecycle

- **Enable** in Settings: folder path (macOS suggestion
  `~/Library/Mobile Documents/com~apple~CloudDocs/Walletspace`), on/off for the
  installation. The folder's parent must exist: Walletspace creates only the
  last element, and never recreates a folder that disappeared (iCloud turned
  off, a volume unmounted) — the status shows an error instead.
- **Unlock**: derive keys → compute tags → read every file under the space's
  tag → authenticate, decode, merge, verify → save `space.json` → _(rev 2)_
  seal the outbox and send it. The unlock waits up to 3 s
  for the initial pass. A slow or incomplete pass continues in the background;
  the UI must not present a timed-out or partial restore as complete. _(rev 2)_
  The create and unlock answers carry `sync: {finished, pending, missing}`,
  and the UI says "not everything has arrived yet" instead of plain success.
- **While unlocked**: write after every change (debounced); read the folder
  every ~30 s. Polling rather than
  fsnotify: iCloud and Dropbox deliver events unreliably, and the folder is
  small.
- **Lock**: clear keys. _(rev 2)_ The outbox keeps what has not been sent and
  is sent while locked.
- **Startup**: local state as before; _(rev 2)_ pending outboxes are sent; the
  folder's space tags are compared with the cached ones. A space here that has
  not synced here yet cannot be told apart from one that is missing — its tag
  comes from the seed — so the count of missing spaces leaves room for those
  and reports them separately ("unlock these first").
- **Files not downloaded yet** _(rev 2)_. A file the provider has evicted
  (`.<name>.icloud` on iCloud) is not here, and seeing the placeholder again
  does not bring it. On macOS Walletspace asks iCloud to download every evicted
  file of a space it is syncing, and reports them as downloading. Where that
  request is not available, or keeps failing, the status says files are missing
  and tells the person to keep the folder downloaded (Finder → "Keep
  Downloaded"). A restore that is waiting for files says so instead of looking
  finished.
- **Status**: `GET /api/sync` and Settings → Backup & sync expose the delivery
  states above _(rev 2)_ and, separately, files read, pending, unreadable or
  dropped. The UI must not say that a phrase alone restores imported keys; use
  the three requirements in the recovery section below.

## Moving to a new machine and recovering

**Export file** (`.wsbackup`): the space files, bundled, each behind its
password step-up in the UI; `walletspace export` from the terminal without one,
since the local user can read `~/.walletspace` anyway. **Import**: "Import from
a backup file" on the start screen, or `walletspace import FILE`. Spaces land
locked, with the same ids, and join the same sync folders at first unlock. The
file is as sensitive as `~/.walletspace` and should be deleted after import.

**Phrase**: "Restore a space" with the phrase and the BIP39 passphrase if the
space has one, and a new password. The first unlock derives the sync key, finds
the folder and adopts wallets, labels, networks and imported keys.

**Dead laptop.** A full recovery needs all three of:

1. the recovery phrase;
2. the BIP39 passphrase, if the space has one;
3. a current copy of the space's files in the folder.

The third is where it can fail: a file written to the folder is not yet a file
in the cloud. iCloud may be offline, paused or full, and the file then exists
only on the laptop that died. The status shows unsent changes, and says that
upload to the cloud is unknown once a change is written _(rev 2)_: Walletspace
cannot see whether iCloud has uploaded it. A rolled-back folder is covered in
[Rollback](#rollback).

**A space made without a phrase on several machines**: create its phrase on one
machine and enter that phrase on the others. A new phrase on each would give
each copy a folder of its own. _(rev 2)_ A phrase that already belongs to
another space on this machine is refused: the two would merge for good. The
check sees open spaces and locked ones whose tag this build has cached (every
unlock caches it); a space never unlocked by this build cannot be checked
without its password.

**Password change**: local to the machine. `space.json.prev` is resealed under
the new password, so the old one opens nothing. A change that races a sync pass
starts over; after three tries it fails with "space changed, try again".

## Sending from two machines

Sync carries the space, not the wallet's operations: the record of transfers,
their fee approvals and especially their outcome lives on the machine that sent
them.

- **Two machines sending at once** submit transactions independently; this
  protocol does not reserve nonces or coordinate spending. On EVM networks
  two machines can pick the same pending nonce (the nonce lock is per process),
  leading to a replacement or rejection. Distinct valid transactions can also
  both execute; sync provides no cross-device payment deduplication.
- **The real risk is a repeated payment.** A transfer whose outcome was unknown
  on machine A (`broadcast_unknown`) may already be on chain. Machine B knows
  nothing about it, and repeating the transfer from B may pay twice. Sync does not
  prevent this, and syncing the operation store would not either: A's record
  only says the outcome is unknown. Before repeating a transfer from another
  machine, check the address on the explorer. The README documents this; the
  send screen's unknown-outcome message must explicitly cover retrying from
  another computer too.

## Transport abstraction

Everything above is a set of opaque blobs addressed by `(space_tag,
device_tag)`. The folder is one store; a future relay or client is another, and
sees exactly what iCloud sees: tags and ciphertext.

```go
type Store interface {
    Spaces(ctx) ([]string, error)          // space tags present
    List(ctx, spaceTag) ([]Entry, error)   // device tags, modification times, evicted placeholders
    Get(ctx, spaceTag, deviceTag) ([]byte, error)
    Put(ctx, spaceTag, deviceTag, blob) error
}
```

Required vectors for implementations, in `internal/devicesync/testdata/`:

- `envelope_v1.json` — phrase, passphrase, device, nonce and snapshot → tags
  and envelope. The revision 1 vector was checked independently. Rev 2 must
  update it for variant lists and repeat that independent check of the BIP39
  seed, HKDF, tags and XChaCha20-Poly1305.
- _(rev 2)_ `merge_v1.json` — sets of snapshots → merged snapshot, with ties
  of every kind, key variants and tombstones.

## Risks and mitigations

| Risk                                                       | Mitigation                                                                                                                                                      |
| ---------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| A merge bug drops an imported key                          | Tombstone-only removal; key variants keep every secret _(rev 2)_; property and vector tests; `space.json.prev` before every merge that removes a wallet or key  |
| A change never reaches the folder                          | Outbox and revisions _(rev 2)_; status shows unsent changes until the revision is written to this folder; upload is "unknown"                                   |
| The folder is current on disk but not in the cloud         | Delivery states _(rev 2)_; "unknown" where the provider cannot be asked                                                                                         |
| The folder is rolled back                                  | Covered while any machine holds the newer state; otherwise stated as a limit ([Rollback](#rollback)); export file and paper phrase remain the backups of record |
| iCloud evicts files                                        | Download requested on macOS _(rev 2)_; otherwise the status names the files missing and the fix                                                                 |
| A copied installation writes another machine's file        | Machine-bound device id _(rev 2)_; same-machine-id clones declared unsupported                                                                                  |
| Clock skew wins a merge                                    | Warning on far-future timestamps; accepted for v1                                                                                                               |
| A repeated payment from another machine                    | Documented; the unknown-outcome message tells the person to check the chain before repeating                                                                    |
| A new space made on one machine does not appear on another | By design — its secrets are not in the cloud until it syncs. The missing-space count tells the user; export or phrase brings it over                            |
| New crypto and merge code, one author                      | Kept small and pure; vectors; an addendum to `docs/security-audit.md`; independent reviews (2026-10-27, more needed before release)                             |

## Implementation status

Revision 1 — implemented, not released, nothing committed:

| Part                                                | State                                                  |
| --------------------------------------------------- | ------------------------------------------------------ |
| Export/import bundle, CLI and UI                    | Done, tests; live check between two instances          |
| Keys, tags, envelope, snapshot, merge, folder store | Done, tests; envelope vector checked independently     |
| Space integration, verification, status, settings   | Done, tests; restore by phrase checked live            |
| Phrase for every space, phrase for older spaces     | Done, tests                                            |
| Reviews                                             | Independent reviews on 2026-10-27; their fixes in tree |

Rev 2 — in progress, uncommitted. Each item lands with a test that fails
without it; "done" means reviewed by an independent agent until a round found
nothing:

1. _Done._ Outbox and revisions ([Delivery](#delivery-rev-2)); upload state "unknown". Tests: a change made and
   locked at once reaches the folder while locked; a crash between the change
   and the outbox shows unsent changes; an older send never replaces a newer
   file.
2. _Done._ Key variants per reference, chosen per wallet ([Merge](#merge)); singleton
   keys in existing vaults and bundles read as one variant. Tests: two variants
   under one reference converge in any order and grouping; a wallet signs with
   its own variant; the envelope vector regenerated and checked again.
3. _Done._ Deleting an imported wallet keeps the key while another wallet uses its
   reference ([Deleting a wallet](#deleting-a-wallet)).
4. _Done._ Machine-bound device id ([Device identity](#device-identity-rev-2)). Test: a
   copied data folder gets a new device tag and drops the copied outbox.
5. _Done._ Download request for evicted iCloud files, with the "Keep Downloaded"
   message where it fails (`brctl download`; macOS 14+ dataless files are
   still read, which makes any provider fetch them).
6. _Done._ Canonical encoding for ties ([Canonical encoding](#canonical-encoding-rev-2))
   and `testdata/merge_v1.json`.
7. _Done._ Tombstones are never trimmed; a space over the limit fails its pass
   with an error and keeps its local state.
8. _Done._ The send screen's unknown-outcome message warns about repeating the
   transfer from another computer.
9. _Done._ Key tombstones inside the vault ([Deleting a wallet](#deleting-a-wallet)).
   A plaintext key tombstone in `space.json` or a backup file counts for
   nothing; revision 1 was never released, so there is no migration.
10. _Done._ A phrase belonging to another local space is refused when adding a phrase.
11. _Done._ The start-up folder check has a timeout (one check per folder at a
    time, retried every 30 s), and the
    "not inside the data folder" check resolves symlinks and ignores case on macOS, bounded at 3 s
    (at most 4 checks left hanging; past that a check is refused). A folder
    that does not answer at start keeps its setting and shows sync as
    unavailable. Firmlinks and Unicode normalisation are not resolved.
12. _Done._ A synced copy of a wallet already here cannot change which key it
    is: for an imported wallet its reference, fingerprint and addresses; for a
    derived one its profile, index and curve (Tron, EVM and the old two-address
    form are one group, Solana another). Such a copy is kept out and reported.

## Where the code is

| Part                                                     | Files                                                           |
| -------------------------------------------------------- | --------------------------------------------------------------- |
| Keys, envelope, snapshot, merge, folder store, device id | `internal/devicesync/`                                          |
| Snapshot ↔ space, verification, passes, status           | `internal/space/sync.go`                                        |
| Outbox, revisions, delivery                              | `internal/space/outbox.go`                                      |
| Tombstones, name time, phrase for older spaces           | `internal/space/manager.go`, `internal/space/recovery.go`       |
| Bundle export/import                                     | `internal/space/bundle.go`, `cmd/walletspace/bundle.go`         |
| Settings and API                                         | `internal/config/home.go` (`sync:`), `internal/httpapi/sync.go` |
| UI                                                       | `ui/src/features/spaces/sync-section.tsx`, `transfer.tsx`       |

## Open questions

- Per-space opt-out of sync. v1 is installation-wide.
- Fixed-size padding of snapshots, to hide the wallet count from the provider.
- Linux has no default folder; the user picks one. Syncthing is the obvious
  recommendation there.
- iCloud's upload state needs cgo (or a new dependency) to read; rev 2 shows
  "upload unknown" for every provider. Revisit if the build ever takes cgo.
