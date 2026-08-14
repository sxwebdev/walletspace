# Security audit Walletspace

Audit date: 2026-07-31
Revision: `22267c1`
Last verified: 2026-08-10 against `8709f93`
Scope: the whole Walletspace service — Go backend, embedded web UI,
vault/storage, EVM/Tron signing, RPC discovery and Node Doctor.

## Remediation status

Audit written 2026-07-31 against `22267c1`; remediation landed 2026-08-03 in
`d3b015b`. **Re-verified against the source on 2026-08-10 at `8709f93`** — every
line of the table below was read against the code that is in the tree today
rather than carried over from the last update. What that pass added is recorded
under "Verification pass, 2026-08-10", including three items it opened. Those
three, and everything else that did not need a live network, were then fixed;
see "Second remediation, 2026-08-10".

Everything below the status section is the audit exactly as it was written,
including the parts later found to be wrong — the table and the per-finding
"Fixed" notes are what has happened since. Findings raised after the audit are
numbered BE-n and described after it.

All eleven original findings are closed and remain closed. What is **not** done
is listed once, here:

| Open item                                                 | Where      | Why it is still open                                                               |
| --------------------------------------------------------- | ---------- | ---------------------------------------------------------------------------------- |
| Build Tron `raw_data` locally                             | SEC-02     | Needs testnet validation. Narrowed by BE-3, which bounds the header a node chooses |
| Re-broadcast the stored signed bytes                      | SEC-06     | Signed bytes are not persisted; needs testnet validation                           |
| A follow-up review by someone who did not write the fixes | audit-wide | Not started. The adversarial suite it was paired with now exists                   |
| Settings that weaken the wallet with the token alone      | BE-7       | Needs a decision: a step-up runs into BE-6's problem, restart-only costs a feature  |
| Deadlines measured on a clock that can move backwards     | BE-8       | Recorded rather than closed; not reachable by the threat model's adversaries       |

**A follow-up review by someone other than the author of the fixes has still not
been done**, and it is now the only item on the list that needs no network. The
adversarial integration suite the audit asked for alongside it landed on
2026-08-10 in `internal/integration`; the 2026-08-10 verification pass was a
check of the recorded claims against the code, not that review.

### Defects found reviewing the remediation itself

An internal review of the changes above turned up fourteen defects, several of
them in the new code rather than the old. All are fixed; the ones that undid a
finding are worth naming, because they are how a fix can look complete and not
be.

- **SEC-06 was inert.** Every Tron operation wrapper ended with
  `return "", err`, throwing away the transaction id `submitWithSigner` had
  just computed — so the adapter's `txID != "" && errors.Is(…)` branch could
  never be true, and a lost broadcast still became `failed`. Compounding it,
  `sendTransfer` classified on `transaction.Hash` rather than on the error, and
  the UI's `retryIsSafe` guarded against a phrase the server never sends. Three
  separate places, each of which alone was enough to restore the original bug.
  The UI check is now an allowlist: the key is dropped only when the backend
  explicitly asks for a new one.
- **SEC-08 had an unthrottled twin.** `confirmPasswordLocked` — the step-up on
  the seed and private-key exports — ran no cooldown, recorded no failure and
  took no KDF slot, so the export endpoints were an unlimited guessing oracle
  beside a throttled unlock. It also held the manager's write mutex across the
  derivation, which is the exact lock hold the finding was about. Both paths now
  share one counter.
- **UI-3 cleared nothing.** `wipe()` began by cancelling the pending clipboard
  clear, so closing the dialog — the common case — left the recovery phrase in
  the clipboard for good. It now clears immediately instead.
- **SEC-07 broke status mapping.** `RedactError` returned a bare `errors.New`,
  cutting the chain, so once `verifyRPCs` wrapped it the sentinels in
  `writePlatformError` no longer matched. It now redacts only the message and
  keeps the cause reachable through `Unwrap`.
- **The capability token was written to the log** and passed in the browser
  helper's argv, where `/proc/<pid>/cmdline` is world-readable on Linux. The log
  line is gone and the URL now goes through a 0600 file.

Also fixed: EVM made no rejection-versus-silence distinction, so a flat
`insufficient funds` was reported as possibly on chain; `parseRecipient`
rejected valid EIP-55 addresses written without `0x`; `Resolver.Headers`
swallowed `${ENV}` expansion errors and sent requests unauthenticated instead;
`lockSpace` grew a mutex per invented space id; and `LoopbackAccess` would have
403'd its own URL on port 80.

The "Verified in" column names where the fix lives. Paths are from the
repository root and name the function rather than a line: a line number in a
document is wrong the first time anyone edits the file above it, and this table
was already pointing at unrelated code by the end of the day it was written.

| Finding | Status                                          | Verified in                                                                                       | Note                                                                                                                                                                                                                                                                                        |
| ------- | ----------------------------------------------- | ------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| SEC-01  | **Closed**                                      | `internal/httpapi/server.go` (`Access.guard`, `LoopbackAccess`), `cmd/walletspace/main.go`                                    | Capability token on every `/api/` route, random loopback port, `Host` checked against the address actually opened, full-origin comparison. `NewPlatform` refuses to build a handler without a token and a host list                                                                         |
| SEC-02  | **Closed for signing; local construction open** | `internal/tron/intent.go` (`Intent.Verify`, `verifyHeader`, `rejectUnknown`), `internal/tron/service.go` (`submitWithSigner`)                                                | raw_data is decoded and compared field by field against a local intent before any signature, the header is bounded against the local clock (BE-3), and the txid is computed from the signed bytes. The transaction is still _assembled_ by the node — see SEC-02 below                      |
| SEC-03  | **Closed**                                      | `internal/httpapi/server.go` (`contentSecurityPolicy`, `securityHeaders`), `internal/asset/store.go` (`validateLabel`)                                       | The escaping half was already done before this work began; CSP, `nosniff`, `Referrer-Policy`, `X-Frame-Options` and server-side metadata bounds were added                                                                                                                                  |
| SEC-04  | **Closed**                                      | `internal/chain/evm/adapter.go` (`approvedFees`, `fees.sane`)                                                            | The confirmed fee is signed; a network that moves past it forces re-confirmation; absurd node quotes are refused at both ends                                                                                                                                                               |
| SEC-05  | **Closed**                                      | `internal/rpcpool/resolver.go` (`Resolver.Headers`), `internal/chain/tron/adapter.go`                                    | `networks.yaml` schema 2 binds credentials to a single endpoint; the resolver, both adapters and the Doctor resolve them per endpoint. Existing files migrate on start                                                                                                                      |
| SEC-06  | **Closed; re-broadcast open**                   | `internal/operation/status.go`, `internal/httpapi/platform.go` (`finishTronOperation`, `rejectIncompleteReplay`)                                  | Already correct for EVM before this work. Tron now keeps the locally computed txid, distinguishes a node's rejection from a lost answer, and records the transaction id before it signs. Re-broadcast of the stored bytes is not implemented — see below                                    |
| SEC-07  | **Closed**                                      | `internal/config/home.go` (`RedactURL`, `RedactError`), `internal/rpcpool/resolver.go` (`MarkHealthy`, `cacheableEndpoint`)                                           | Single redactor on errors and logs; secret-bearing endpoints are no longer cached, with a provenance check and a shape check behind it                                                                                                                                                      |
| SEC-08  | **Closed**                                      | `internal/space/throttle.go`, `internal/space/quota.go`, `internal/vault/vault.go`, `cmd/walletspace/main.go` | Server timeouts, an exponential unlock cooldown that survives restart, a global KDF semaphore, derivations moved off the global lock with per-space serialisation, quotas on spaces/accounts/assets/operations, and KDF bounds tightened at both ends                                       |
| SEC-09  | **Closed**                                      | `internal/httpapi/ui/components/ui.js` (`addressGroups`), `internal/chain/evm/adapter.go` (`parseRecipient`)                                     | Full grouped address on the confirmation screen; EIP-55 enforced on the recipient                                                                                                                                                                                                           |
| SEC-10  | **Closed**                                      | `internal/rpcpool/resolver.go` (`extractURLs` and its bounds), `internal/doctor/doctor.go`                                   | Discovery bounded by count, URL length, node count and depth; Doctor holds a limiter slot before it starts a goroutine                                                                                                                                                                      |
| SEC-11  | **Closed**                                      | `internal/space/manager.go` (`confirmPassword`, `ConfirmSend`), `internal/httpapi/ui/components/ui.js` (`secretBlock`)                                                  | Password step-up on both exports and on the backup (BE-4), a separate confirmation before funds move, auto-lock cannot be disabled and is bounded (1 min – 24 h), a 12-character floor plus a common-password and repeated-character check, revealed secrets expire and clear the clipboard |

Corrections to the audit as written, found while verifying it:

- **SEC-03** described `dashboard.js` interpolating the token symbol without
  `escapeHTML`. That was already false at the time of writing — every display
  sink was escaped. The open part was the missing CSP and the absent
  server-side bounds on the symbol.
- **SEC-06** described the finding as open across the board. It was already
  correct for EVM: the hash was computed locally and `broadcast_unknown`
  existed end to end. Only Tron was affected.
- **SEC-11** stated that any API call, including background balance polling,
  refreshes the idle timer. It does not: the balance path reads accounts
  through `Get`, which never touches `lastUsed`. Only deliberate actions refresh
  it, which is the intended behaviour — five of them at the time, six since the
  spending confirmation joined them, and the spending *gate* deliberately not
  among them: it needs nothing but the token, so refreshing there would let a
  caller hold a space open by polling a check it cannot pass.
- **"What is already right"** credits the private dialer with blocking loopback
  and private IPs on every RPC connection. That was true for EVM and false for
  Tron — see BE-2.

### Verification pass, 2026-08-10

A re-read of the whole tree at `8709f93` against the claims recorded above. Each
line of the table was traced to the code that implements it; nothing recorded as
fixed was found undone. Alongside it:

- `go vet ./...` — clean. `go test ./... -count=1` — every package passes,
  including the regression tests each fix is named after.
- `govulncheck ./...` — no reachable vulnerable symbols. One advisory remains in
  a required module, [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932) for
  the unused `golang.org/x/crypto/openpgp`, with no fixed version. The
  OpenTelemetry advisory the audit listed is gone: the dependency has moved
  past the release that fixed it.
- One change landed after the remediation and is not described anywhere above.
  `8709f93` narrowed the Origin and Fetch Metadata checks to `/api/` routes,
  because the launcher's `file://` redirect makes the first navigation
  cross-site and the guard was refusing to serve the page that then presents the
  token. Reviewed as part of this pass and found sound: the `Host` check still
  runs on every request, so DNS rebinding is unaffected; the relaxed paths serve
  only the static UI, which is not secret and carries no authority; the JSON
  content-type requirement still applies to every write whatever the path; and
  the API itself is unchanged. The same commit moved the UI's own modules out of
  `/api/`, which they had been sharing with the protected namespace — a browser
  cannot put a header on a module import, so the token check was 401'ing the
  wallet's own scripts.

Three items the pass opened are recorded as BE-3, BE-4 and BE-5. None of them
reopens a closed finding.

### Second remediation, 2026-08-10

Everything the verification pass opened, plus the two standing items that needed
no network, was fixed the same day. What landed:

- **BE-3, BE-4 and BE-5 are closed.** Each is described in its own section
  below, with the fix.
- **The adversarial integration suite exists**, in `internal/integration`. It
  runs the assembled wallet over a real loopback socket against fake nodes, and
  it is an ordinary test package rather than one behind a build tag, so `make
  test` and CI run it like everything else. Twelve scenarios: a rebinding page
  aimed at a listening port; the same page refused on its Origin instead;
  a local process with no token, and one with a token a byte short; the
  launcher's cross-site bootstrap loading the UI while the API stays shut to it;
  the step-up on both exports and on the backup; guessing throttled across
  unlock and the exports together; spending refused until it is confirmed, and
  the window dying with the session; the step-up following the config file
  rather than a default; a node that quotes one fee for the screen and another
  for the signature; a broadcast accepted and then cut off, followed by a retry
  that must not sign twice; and a node that says no, where a retry is the right
  advice.
- **`govulncheck` runs in CI**, as `make vuln`. It fails on a reachable
  vulnerability and passes on an advisory nothing calls, which is the only
  version of the check that will not be trained away.
- **Spending is a step-up of its own** — the last item of SEC-11's "what to
  fix", which asked for a separate confirmation of the exact intent before funds
  move. See "Spending confirmation" below.

One live bug turned up while fixing BE-4: the browser's backup download went out
on a bare `fetch` that never attached the capability token, so the guard had
been turning away the wallet's own request since the token shipped. Every call
now builds its headers through one function.

### Defects found reviewing the second remediation

The first remediation's own review found fourteen defects in the fixes. This
one was reviewed the same way, and the rate did not improve: ten, three of them
serious, and four of them in the tests rather than the code. They are recorded
here because the pattern is the point — a fix is not finished when it is
written.

- **The spending step-up could be switched off by the caller it defends
  against.** The worst of them, and its own finding: BE-6 below.
- **The discovery-URL rule stopped the wallet from starting.** BE-5's fix put
  the check in `ValidateHomeConfig`, which runs against the file already on
  disk, so a `config.yaml` naming a private discovery host — legal until that
  day, and the normal state for anyone who had used a LAN service — made the
  process exit before opening its port. The UI that would fix the value is
  served by the process that would not start. It was written thirty lines below
  the comment explaining why auto-lock is clamped rather than rejected for
  exactly this reason. Stored values are now repaired on load and refused only
  on the way in; the URL is dropped and discovery switched off, because an
  address has no nearest legal value the way a duration does. The same
  treatment was extended to the discovery timings, where a stored `0s` had the
  identical effect.
- **The password prompt destroyed the screen it was asking about.** `modal()`
  replaced the dialog on screen, so the confirmation asked for a password with
  the recipient, the amount and the fee no longer visible — while its own text
  promised the transfer would go ahead "exactly as it was shown". The dialogs
  underneath went on writing failures into a node no longer in the document, so
  a cancelled or failed staking or deploy reported nothing at all. Dialogs are
  a stack now: the prompt opens above, the screen beneath stays readable and
  inert, and Escape closes only the top one.
- **Dismissing the prompt mid-derivation opened the window anyway.** The
  password check takes a noticeable moment, and closing the dialog during it
  told the user nothing was confirmed while the server went on to open a
  five-minute window. The browser now aborts the request and reports what the
  request actually did, and the server reads the request context once — after
  the password is proven, before the grant is written — so the two answers
  cannot differ.
- **A grant outlived the password that bought it.** `ChangePassword` refreshed
  the session in place rather than replacing it, so a window opened under the
  old password survived the change.
- **The gate could answer for a session the auto-lock had already expired.**
  It read the map without sweeping, and the background sweep runs at most once
  a minute.
- **Every 401 was reported as a lost capability token.** A mistyped space
  password told the user their tab had lost its token and to relaunch the
  wallet from the printed URL, which also hid the cooldown message after
  repeated attempts.
- **A revealed secret was never wiped when its dialog closed.** `secretBlock`
  listens for a `secret-dismissed` event that nothing in the codebase
  dispatched, so the comment claiming that closing the dialog does not leave
  the secret alive had been false since it was written; only the 90-second
  timer cleared it. Found while reviewing the modal stack, and unrelated to it.
- **Four tests passed with the thing they tested removed.** The
  DNS-rebinding test sent an `Origin` header as well, and the guard refuses a
  foreign origin with the same status — so it passed with the `Host` check
  deleted outright. The throttle test locked the space first, so "space is
  locked" satisfied every assertion about the cooldown. Two more asserted only
  that a call was refused, with no positive control, so a renamed route or a
  malformed request body would have satisfied them — and one of them was in
  fact sending the wrong field name. Each now proves the mechanism it names,
  and the counterfactual was checked by removing the mechanism and watching the
  test go red.
- **A double spend could not be distinguished from a re-send.** The fake node
  answered every nonce query with the same number, so two independently built
  transfers signed byte-identical bytes and the assertion counting distinct
  broadcasts collapsed them into one.

### Spending confirmation

Before this, the wallet's own rules did not line up. Revealing the recovery
phrase asks for the password, exporting a private key asks for the password, and
downloading the backup now does too — while _spending_ the funds those secrets
control asked for nothing beyond an unlocked space. So anything that reached the
API of an unlocked wallet, an injected script or another local process holding
the token, could not steal the keys but could move everything they protect, one
transfer at a time. The confirmation screen is in the browser, and a script that
is already in the browser is not stopped by it.

Sending, staking, delegating, withdrawing and deploying now go through
`RequireSendConfirmation`. The password opens a window — five minutes by
default, between one minute and an hour — and every operation inside it goes
through without asking again. The grant lives on the unlocked session, so
locking the space by hand or by the idle timer takes it with it, and it is never
written to disk.

`ConfirmSend` goes through the same `confirmPassword` path as the exports, which
puts it behind the same cooldown, the same KDF semaphore and the same per-space
lock. A new password check that skipped those would be the SEC-08 "unthrottled
twin" defect a second time.

The refusal carries `code: "send_confirmation_required"` rather than only a
message, and it is raised through the shared error mapper rather than by hand,
so the code survives wherever the check is made. The UI retries the identical
request — same idempotency key, same approved fee — once the password is
accepted, and asks for it in a dialog stacked over the transfer summary rather
than in place of it: a password given for numbers the user can no longer see is
not a confirmation of anything. A retry that re-priced or re-keyed would mean
confirming one transfer and signing another.

The window is bounded at both ends of its life. `ConfirmSend` reads the request
context after the password is proven and before the grant is written, so a
browser that has given up — the dialog dismissed mid-derivation — cannot be
told nothing was confirmed while a window quietly stands open. Whether the
attempt counted against the cooldown is settled before that, on the password
alone, or hanging up would be a way to guess for free. Changing the space
password replaces the session rather than refreshing it, so a grant does not
outlive the secret that bought it, and both the gate and `ConfirmSend` run the
idle sweep first, so neither can answer for a session the auto-lock has already
passed.

The setting is stored as a pointer in the YAML so that a file written before it
existed reads as "not stated" and takes the default rather than as a deliberate
no — and it is the one security setting the API will not write at all. See
BE-6.

Covered by `TestSpendingNeedsItsOwnConfirmation`,
`TestChangingThePasswordClosesTheSpendingWindow`,
`TestTheSpendingGateSeesTheIdleDeadline`,
`TestConfirmingASpendCountsAsUsingTheSpace`,
`TestAnAbandonedConfirmationOpensNoWindow` and, end to end,
`TestSpendingNeedsThePasswordAndTheWindowDiesWithTheSession` and
`TestTheSpendingStepUpFollowsTheConfigFile`.

### SEC-02, what remains

The signing barrier is closed: a node cannot get a signature over anything
other than the operation that was asked for, and 17 substitution cases across
three operation kinds are covered by tests, two of which also assert that
`SignDigest` is never reached (the count was recorded as 23 before the
2026-08-10 pass, which is the number of assertions in the file). What is _not_
done is building `raw_data` locally, which would take the node out of the
construction path altogether. That work needs live-network validation before it
can be trusted — an incorrectly assembled reference block or fee limit produces
transactions that fail to broadcast — so it was deliberately left for a change
that can be tested against a testnet.

What a node still chooses, and the wallet cannot check without asking that same
node, is the reference block. Everything else in the header is now bounded — see
BE-3.

### BE-1 — High — comma smuggling in the Tron node list (fixed)

Not in the original audit. Every Tron endpoint was validated and probed as a
single URL string, then joined with `,` and re-split into a _list_ of nodes. A
comma is legal in a URL path, so a discovery service returning
`https://attacker.example/rpc,grpc://127.0.0.1:50051` passed every check and
then supplied a second, entirely unchecked plaintext gRPC node — bypassing the
private-IP dialer the audit lists under "What is already right", and reaching
the same code path that builds the transaction SEC-02 signs.

Fixed by passing a parsed node list instead of a delimited string, and by
rejecting `,` and `|` in both validation gates.

### BE-2 — Medium — Tron traffic bypasses the guarded dialer (fixed)

Not in the original audit, and it contradicts one of its "already right"
claims. `Resolver.HTTPClient` — the client that re-resolves DNS at dial time
and refuses non-public addresses — is passed only to the Tron verification
probe. The service carrying real traffic is built with node configs whose
`HTTPClient` and `DialOptions` are nil, so gotron falls back to its own client
and to `grpc.NewClient`, neither of which filters addresses. EVM does this
correctly.

Fixed by attaching the transport to the node itself: `config.Node` now carries
the guarded `HTTPClient` for HTTP nodes and a `grpc.WithContextDialer` built
from the same dialer for gRPC nodes, and the Tron adapter fills both in for
every endpoint it verifies.

The dialer itself also had a gap, found while fixing this. `publicIP` relied on
the `net.IP` predicates, and Go's `To4` decodes only the IPv4-mapped form — so
`::127.0.0.1`, `64:ff9b::7f00:1` (NAT64), `2002:a9fe:a9fe::` (6to4) and Teredo
all read as ordinary global unicast. Each is now refused by CIDR.

### BE-3 — Medium — the Tron transaction header is not part of the verified intent (fixed)

Found by the 2026-08-10 pass. `Intent.Verify` in `internal/tron/intent.go`
checks the contract list, the contract type, the fee limit, the permission id
and every field inside the contract parameter. It does not look at the four fields of
`raw_data` that sit _outside_ the contract — `expiration`, `timestamp`,
`ref_block_bytes` and `ref_block_hash` — and nothing else in the pipeline does
either: `Expiration` appears in the production tree only in a test fixture. All
four are chosen by the node and covered by the signature.

The audit asked for exactly this. SEC-02's "what to fix" lists "the
expiration/reference block" among the fields to check strictly, and the
remediation stopped at the contract.

**Exploitation.** A Tron node accepts an expiration up to 24 hours past the
reference block; the default the wallet would expect is a minute. A malicious
node returns a correct transfer with the expiration set to its maximum, gets it
signed, and then does not broadcast it — answering with a transport error, which
is now honestly reported as `broadcast_unknown`. It holds a valid signed
transfer for a day and fires it at a moment of its choosing: after the account
is topped up, or after the user has given up and moved the funds. The signature
is over the operation the user asked for, so nothing downstream objects. The
mirror image is a denial: an expiration already in the past, or a reference
block from an unrelated fork, produces a transaction that can never be included
while looking like an ordinary network failure.

**Fixed.** `Intent.Verify` now checks the header before it looks at the
contract, in `verifyHeader`, so every operation kind inherits it at once rather
than each signing path remembering to ask.

An expiration may sit at most ten minutes ahead. A node builds with sixty
seconds by default, so that is a wide margin on the useful value and two orders
of magnitude off the protocol's twenty-four hours — which is the whole of the
attack. The timestamp has to be around now: no further ahead than a two-minute
clock skew, and no older than the ten-minute ceiling. Expiration must also come
after the timestamp, and the reference block fields must be the right size,
which is all that can be said about them without asking the same node to
describe the head of the chain.

The expiry check is deliberately the lenient one. An expiration that has already
passed is not an attack — a slow node produces the same thing — and it is
allowed a skew's grace, because being strict there would mean a wallet on a
machine whose clock runs a minute fast signs nothing at all while every
transaction it refuses is perfectly valid on chain.

**And the header was not the whole of it.** Reviewing this fix turned up that
`raw_data` has two more node-chosen fields nobody looked at: `data`, the memo,
and `ref_block_num`. The memo is the cheaper attack of the two and the worse
one — arbitrary bytes of the node's choosing, covered by the signature, billed
to the user as bandwidth (Tron charges per serialised byte and burns TRX once
the free allowance is gone, so a hundred kilobytes of someone else's payload is
paid for out of a one-TRX transfer) and published on chain under the user's
address for good. `fee_limit`, the only cost bound `Verify` checked, is zero on
a plain transfer and caps energy rather than bandwidth.

A memo is now refused outright, as `scripts` and `auths` already were: an
`Intent` has no concept of one, so the honest assertion is that there is none.
`ref_block_num` is held against the `ref_block_bytes` beside it — two fields
naming one block can be checked against each other without asking the node to
describe the chain, which is the strongest thing sayable locally — and zero
keeps passing, because java-tron writes only the two bytes and requiring
otherwise would refuse every transaction a real node builds.

The same review also applied `unmarshalParameter`'s reasoning — that a field
this build cannot name is a field the user signs without anything having read
it — to the levels above the parameter: the transaction, the contract entry,
and the `Any` envelope around the parameter. The envelope is a message in its
own right with room for fields beside the type URL and the payload, and
reflection stops at the payload's opaque bytes, so it is a door of its own and
not the check below in disguise.

Covered by `TestVerifyRejectsAHostileTransactionHeader` (twelve cases: a day, an
hour, expired, no expiration, built last week, built in the future, no
timestamp, expiring before it was built, both reference-block fields, and the
two int64 extremes, which have to be refused by the bounds rather than by
whatever `time.UnixMilli` does when it overflows), by
`TestVerifyRejectsBytesNoIntentAsksFor` (the memo, a hundred-kilobyte memo, a
reference height naming another block, a negative one chosen so that only the
sign check can refuse it, and an unknown field at each of the three levels),
`TestVerifyAcceptsTheHeaderARealNodeWrites` for the skew a real node produces,
and `TestALongLivedTransactionNeverReachesTheKey` for the acceptance criterion
that matters: `SignDigest` is not called.

### BE-4 — Low — the encrypted backup downloads without the password (fixed)

Found by the 2026-08-10 pass. `POST /api/spaces/{id}/backup` returns the whole
space file, vault container included (`Manager.Backup`). It takes no
password, and unlike the mnemonic and private-key exports it does not require
the space to be unlocked — the manager reads the file it already holds.

This is not a break of SEC-01 or SEC-11: the capability token is required, and
those findings are about what an unauthenticated caller or a brief look at an
unlocked tab can do. It is the one master-secret-bearing endpoint left without
the step-up SEC-11 added to its neighbours, and the audit's own SEC-01
exploitation list names "download an encrypted backup and attack the password
offline" as one of the outcomes worth preventing. Argon2id at 64 MiB × 3 and a
12-character floor make that grind expensive rather than impossible.

**Fixed.** `Backup` takes a password and runs it through the same
`confirmPassword` path the exports use, which also puts the endpoint behind the
shared unlock cooldown and the KDF semaphore. The dialog asks for it the way the
recovery phrase does.

Fixing it surfaced a live bug: `downloadBackup` in the UI used a bare `fetch`
rather than the API client, so it never carried the capability token and the
guard had been answering the wallet's own download with a 401 ever since the
token shipped. The client now exposes a `download` helper that builds its
headers the same way every other call does.

Covered by `TestEncryptedBackupCanBeRestoredAndUnlocked`, extended with the
no-password and wrong-password cases, and end to end by
`TestTheBackupNeedsThePasswordEvenWhileUnlocked`.

### BE-5 — Low — the discovery request itself does not use the guarded dialer (fixed)

Found by the 2026-08-10 pass. Every RPC connection goes through
`Resolver.DialContext`, which re-resolves the host and refuses non-public
addresses. The discovery request does not: `Resolver.client` in
`internal/rpcpool/resolver.go` was built with a plain `net.Dialer` and
`http.ProxyFromEnvironment`. Validation of the discovery URL requires HTTPS, a
host and no userinfo (`ValidateHomeConfig`) — it did not require the host to
resolve to a public address.

The endpoints discovery _returns_ are still filtered by `safeDynamicEndpoint`,
so this does not reach the signing path. What it means is narrower: the setting
can point the wallet at a loopback or private-network service, and changing it
needs the capability token, which is the boundary that already governs
everything else. Recorded because it is the one outbound request the wallet
makes on a schedule that no address filter applies to, which is not what the
rest of the networking code leads a reader to expect.

**Fixed.** The discovery client now dials through `Resolver.DialContext`, built
per dial rather than captured once so that changing the insecure-RPC setting
takes effect on a client that lives for the whole process. `Proxy` is nil, as it
already was for RPC: a proxy would be dialled in place of the host, which is the
same exemption by another route.

Saving a discovery URL that names a loopback, private, link-local or `.local`
host is refused unless insecure RPC is explicitly allowed — the switch the
dialer honours for nodes. The check is a literal-and-suffix test rather than a
lookup, because a validation function has no business making a DNS query; a name
that resolves privately is still refused, by the dialer, at connect time.

Covered by `TestDiscoveryItselfGoesThroughTheGuardedDialer`, which asserts both
that a loopback discovery service is never reached and that it is reached once
private addresses are allowed — so the refusal is the dialer's and not something
else being broken — and by `TestLocalDiscoveryURLsAreRefused`.

### BE-6 — High — the spending step-up could be switched off with the token alone (fixed)

Found reviewing the second remediation, and it made the step-up worth nothing
against the caller it was built for. `PATCH /api/settings/security` required
only the capability token, and the handler pushed the result straight into the
live manager. The sequence, run end to end against the real server: a transfer
is refused with 403 `send_confirmation_required`; a PATCH carrying
`confirm_sends: false` returns 200; the identical transfer returns 202 and the
node records a broadcast. No password anywhere.

A password could not be demanded here. The setting is global and belongs to no
space, so there is no password to ask for — and "the password of any unlocked
space" is no barrier at all, because a caller holding the token can create a
space of its own and know its password.

**Fixed** by taking the field out of the API's reach. `confirm_sends` is read
from `config.yaml` at start and is refused in either direction by the settings
handler, which names the file and the restart in its answer; a PATCH that
echoes the stored value back is accepted, because the form posts the whole
block. The field is listed in `restart_required`, and the settings page renders
it as state rather than as a control. Turning the step-up off is therefore
something the person at the keyboard does to a file, which is the one thing a
caller with the token cannot reach.

The same reasoning does not extend to `send_grant_ttl`, which stays editable
within its one-minute-to-one-hour bounds: the worst a caller can do with it is
widen a window that still opens only on a real password. That is a smaller
version of the same shape, and it is recorded rather than closed.

Covered by `TestTheSpendingStepUpCannotBeSwitchedOffThroughTheAPI`.

### BE-7 — Medium — the RPC a wallet trusts can be replaced with the token alone (open)

Found reviewing the second remediation, alongside BE-6, and left open because
the answer is a design decision rather than a patch.

`PUT /api/settings/networks/{id}`, `DELETE …/override` and
`PATCH /api/settings/node-discovery` all take nothing but the capability token.
A caller holding it can drop the user's pinned endpoints, point every network
at a node it controls, and set `allow_insecure_rpc`. Endpoint verification is
no obstacle: it probes the endpoint that was just supplied, and the attacker's
node answers correctly.

The keys stay safe — a Tron transaction is verified against a local intent
before signing, an EVM fee is bound to the approval, and the guarded dialer
still applies — so this is not a path to spending. What it reaches is
everything the wallet *believes*: balances, fees, the numbers on the
confirmation screen, and whether a broadcast is reported as having happened. A
user reading a screen fed by an attacker's node is being lied to about their
own funds, which is worth closing even though nothing can be taken directly.

The obvious fix — a step-up on network settings — runs into BE-6's problem in
a milder form: these settings are global too. Making them restart-only would
end the ability to add an RPC endpoint from the UI, which is a real feature. It
needs a decision about which of the two costs to pay.

Two smaller members of the same family, both recorded and neither closed:
`auto_lock` can be pushed to twenty-four hours with the token alone, taking a
decrypted seed from fifteen minutes in memory to a day, and `send_grant_ttl` to
an hour as described in BE-6.

### BE-8 — Low — every deadline rides a clock that can be moved backwards (open)

Found reviewing the second remediation. `Manager.now` is
`time.Now().UTC()`, and `.UTC()` strips the monotonic reading, so the auto-lock
deadline and the spending window are both measured against a wall clock. A
system clock moved backwards — by an operator, by NTP after a long drift —
extends both. The unlock cooldown has an explicit guard against exactly this,
added with SEC-08 and visible in `throttle.go`; sessions and grants have none.

Not a remote attack: moving the machine's clock is not something the threat
model's adversaries can do. It is recorded because the guard already exists one
file away, which means the omission is an inconsistency rather than a
judgement.

## Outcome

As it stands, the service cannot be considered safe for holding or moving real
funds. Two critical vulnerabilities were found, each of which can lead to a
compromise of the seed or private keys, or to signing an operation other than
the one the user asked for:

1. the local HTTP API has no authentication and is reachable through DNS
   rebinding;
2. a Tron transaction is built by an untrusted RPC node and signed without any
   check of its actual contents.

In addition, the audit confirmed stored XSS through on-chain metadata, an EVM
fee confirmation that is not bound to the signature, a leak of provider
credentials to fallback nodes, and the risk of a repeated Tron operation after
an indeterminate broadcast.

| Severity | Count | Admissible for a release with real funds |
| -------- | ----: | ---------------------------------------- |
| Critical |     2 | Blocks                                   |
| High     |     4 | Blocks                                   |
| Medium   |     5 | Fix before a public release              |

## Threat model

The audit includes the following realistic adversaries:

- an arbitrary website the user opens in an ordinary browser;
- an unprivileged local process or sandboxed application that can reach
  loopback but cannot reach Walletspace's files;
- a compromised, faulty or malicious RPC/discovery node;
- a malicious ERC20/TRC20 contract with attacker-controlled metadata;
- an attacker who obtained a copy of an encrypted backup or of the data
  directory;
- a network failure at the moment of a broadcast.

A full compromise of the OS account and reading the process memory are not
considered fixable by Walletspace itself. Legacy migration was not assessed as
a target function: by assumption the project starts from a clean data format.

## Findings

### SEC-01 — Critical — the local API has no authorization boundary and is vulnerable to DNS rebinding

**Evidence.** Every sensitive route, including unlock, seed/private key export
and sending funds, is registered without authentication middleware in
`internal/httpapi/platform.go:80-126`. The only protection lives in
`internal/httpapi/server.go:166-201`:

- an empty `Sec-Fetch-Site` is allowed (`:168-173`);
- `Origin` is only compared against the client-supplied `Host` (`:175-181`);
- `Host` itself is never checked against the actual loopback listen address;
- there is no capability/session token.

The loopback bind is enforced in `internal/config/home.go:274-285`, but loopback
constrains the network route, not the authority of the calling code.

**Exploitation.** The attacker serves a page from a domain they control on port
`8080`, then changes the domain's DNS answer to `127.0.0.1`. The subsequent
browser fetch stays same-origin: `Origin` and `Host` both equal the attacker's
domain, and `Sec-Fetch-Site` is `same-origin`. The guard lets the request
through to Walletspace. Possible actions:

- read the mnemonic (`platform.go:235-243`) and a private key (`:364-380`) out of
  an already unlocked space;
- sign a transfer, a staking/delegation operation or a deploy;
- download an encrypted backup and attack the password offline;
- change RPC/discovery settings;
- create spaces and load the CPU and the disk.

An ordinary local process can call the API even more easily: the absence of
browser headers is deliberately allowed and pinned by a test,
`internal/httpapi/guard_test.go:48-56`.

**What to fix.** On a clean format, loopback should not be treated as an
authentication boundary:

1. Generate a random capability token of at least 256 bits on every start. Hand
   it to the UI through the URL fragment, keep it in the tab's memory only, and
   require it — for example in `X-Walletspace-Token` — on **all** `/api/*`
   routes, including GET and streaming endpoints.
2. Listen on a random loopback port; check `Host` against an exact allowlist of
   the IP/port actually opened. Do not trust an arbitrary Host header.
3. Compare the full origin: scheme, hostname and port. Keep Fetch Metadata and
   the JSON content type as additional CSRF protection, not as authentication.
4. A stronger and more convenient option is a native shell/webview with IPC or a
   Unix socket, leaving HTTP as nothing but an adapter with capability
   authentication.

**Acceptance criterion.** Requests with a missing or wrong token, and requests
carrying `Host: attacker.example:8080`, are rejected before routing regardless
of `Origin`/`Sec-Fetch-Site`. This is covered by an integration DNS-rebinding
test.

### SEC-02 — Critical — Walletspace signs a Tron transaction returned in full by an untrusted node

**Evidence.** The user's request is validated locally, but the resulting
protobuf transaction is created by the RPC node:

- TRX/TRC20 transfer: `internal/tron/service.go:824-874` and `:159-185`;
- staking/delegation: `internal/tron/staking.go:611-735`, then `:764-785`;
- deploy: `internal/tron/contract.go:189-219`.

After that, `submitWithSigner` serialises `tx.Transaction.RawData`, computes the
digest and signs it without comparing anything to the original intent
(`internal/tron/service.go:803-821`). The endpoint check only confirms the
declared `net_version` (`internal/chain/tron/adapter.go:117-166`), which a
malicious server can return without taking part in the real network.

**Exploitation.** A node chosen through a custom RPC or through discovery
receives the request "transfer 1 TRX to address A" but returns an unsigned
transaction "transfer the entire available TRX to the attacker's address". The
local signer sees only a 32-byte digest and signs the substituted raw data. The
TRC20 contract/data, the delegation receiver, the amount, the fee limit or the
contents of a deploy can be replaced in the same way.

**What to fix.** The private key must only ever sign an intent that was built
and verified locally:

- preferably build the Tron contract/raw data locally, taking only
  head/reference block data from RPC;
- if a node-assisted build stays for now, decode the transaction immediately
  before signing and check strictly: exactly one contract, its type, the owner,
  the recipient, the amount, the token contract/calldata, the resource, the fee
  limit, the permission id, the expiration/reference block, and the absence of
  any extra action;
- the signer API must accept a typed intent or an already verified canonical
  transaction, not an arbitrary digest coming from RPC;
- compute the txid locally from the same canonical `raw_data`.

The check is needed separately for every kind of Tron operation. Checking only
the owner address or the chain identity does not solve the problem.

**Acceptance criterion.** A fake RPC substitutes one field at a time and adds a
second contract; in every case the request is rejected and `SignDigest` is never
called.

### SEC-03 — High — stored DOM XSS through an on-chain token symbol

**Evidence.** The ERC20 symbol is read as an arbitrary string from the contract
(`internal/chain/evm/adapter.go:254-297`), then stored with no format constraint
(`internal/httpapi/platform.go:717-757`, `internal/asset/store.go:91-110`). In
the dashboard that value is interpolated three times without `escapeHTML`:

`internal/httpapi/ui/views/dashboard.js:251-255`

The resulting string is parsed as HTML through `createContextualFragment` in
`dashboard.js:290-295`. There is no CSP
(`internal/httpapi/ui/index.html:1-23`), and the shared middleware does not set
one either.

**Exploitation.** The contract returns a symbol along the lines of
`</span><img src=x onerror="...">`. The user adds the address of such a token,
after which the payload executes in Walletspace's origin. The script can call
the API, export the seed and keys and sign operations, especially while a space
is unlocked. This attack requires no compromised RPC — the metadata is
controlled by the token contract itself.

**What to fix.** Do not build DOM out of strings for untrusted data: create
nodes and set `textContent`. Additionally validate and bound the symbol and the
name server-side (a sane length, printable Unicode), but validation does not
replace context-aware escaping. Add a strict CSP as a second barrier:

`default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'`

`X-Content-Type-Options: nosniff` and `Referrer-Policy: no-referrer` are needed
as well.

**Acceptance criterion.** A symbol containing HTML/SVG/an event handler is
displayed as literal text; the CSP forbids an inline handler; a browser test
confirms that the payload issues no network request.

### SEC-04 — High — the confirmed EVM fee is not bound to the transaction being signed

**Evidence.** The estimate obtains gas/fees in
`internal/chain/evm/adapter.go:300-327`. After the confirmation the UI calls a
different endpoint, and `Send` requests `EstimateGas`, the nonce and the fee
suggestions again (`adapter.go:330-399`). Neither an approved fee/gas nor a hash
of the prepared transaction appears in the request or in the idempotency record.
`suggestFees` accepts the RPC values with no upper bound
(`adapter.go:559-571`). The UI shows the result of the first estimate
(`internal/httpapi/ui/views/dashboard.js:675-694`).

**Exploitation.** A faulty or malicious RPC first returns a normal fee, then on
`Send` returns an enormous priority fee/gas price. The user sees and confirms
one value but signs another. A high tip can cost a substantial part of the
native balance, or all of it.

**What to fix.** Introduce a `prepare -> approve -> sign exact bytes` flow:

1. The backend builds a canonical unsigned transaction and returns every field
   plus a single-use `intent_id`/hash.
2. The UI shows the full intent.
3. The sign endpoint signs exactly the stored immutable transaction, or accepts
   hard user-approved maxima (`max_total_fee`, gas limit, fee cap, tip cap,
   nonce).
4. Any change requires a new confirmation. Add an absolute/relative fee policy
   and refuse an anomalous RPC answer.

**Acceptance criterion.** Between prepare and sign, RPC changes the fee by a
factor of 100; the backend does not sign the new transaction and requires
another confirmation.

### SEC-05 — High — provider credentials are sent to unrelated fallback/discovery endpoints

**Evidence.** The resolver merges custom RPC with the official fallbacks
(`internal/rpcpool/resolver.go:81-93`), but the headers are stored per network
rather than per endpoint (`resolver.go:164-177`). The EVM adapter applies the
same headers to every candidate (`internal/chain/evm/adapter.go:478-490`). Node
Doctor does the same for EVM and Tron (`cmd/walletspace/main.go:109-124`).

Even with no custom URL, headers can be saved and discovery enabled, after
which the secret goes to every discovered and fallback node. With a custom URL
present, a moment of unavailability is enough for EVM to move on to the next
endpoint.

**Impact.** An `Authorization` header, an API key or a bearer token reaches a
provider it was never meant for. That can expose account access, a paid quota,
or the credentials of another service.

**What to fix.** Move from network-wide fields to a list of endpoint records:

```text
endpoint = { url, credential_ref, allowed_header_names, trust_level }
```

A credential may only be attached on an exact scheme/hostname/port match with
the record. Fallback and discovery always start without credentials until the
user has explicitly bound a separate secret to a specific origin. Keep secrets
in the OS keychain/secret store, or as an env reference.

**Acceptance criterion.** A mock custom endpoint fails; the fallback receives
the request without `Authorization`/provider headers. The same test is needed for
the Doctor.

**Fixed.** `networks.yaml` moved to schema 2: `rpc_urls` plus a detached
`headers` map became a list of endpoints, each carrying its own credentials.
`Resolver.Headers` now takes the endpoint as an argument and matches it on
scheme, host, port, path and query — so a fallback, a discovered node, or even
another path on the same host gets nothing. Both adapters resolve per candidate
inside their endpoint loop, the Doctor closure resolves per endpoint it probes,
and `verifyRPCs` probes each endpoint with only its own credentials.

Existing files migrate on start, in memory and then in place. A schema 1 file
records nothing about which endpoint its headers belonged to, so the migration
takes the narrowest reading available: the first custom URL, the one the secret
was typed next to. Everything after it starts clean.

Two details were needed to make the API workable. Credentials are still
redacted on the way out, so the browser cannot send one back — an absent
`headers` field therefore means "leave what is stored alone" and an empty object
means "delete it", and saving a network carries forward per endpoint. And an
endpoint listed twice is rejected, because "which credentials does this URL get"
has to have one answer.

Covered by `TestHeadersAreScopedToTheEndpointTheyBelongTo`,
`TestProviderCredentialsDoNotFollowTheFallback` (a failing paid endpoint, an
answering fallback, asserting both what was sent and what was not),
`TestNodeConfigsKeepCredentialsOnTheirOwnNode`,
`TestNetworksFileMigratesHeadersOntoTheFirstEndpoint` and
`TestSavingANetworkKeepsCredentialsItWasNotGiven`.

### SEC-06 — High — an indeterminate Tron broadcast turns into advice to build a new transaction

**Evidence.** `submitWithSigner` already computes the digest locally, but on any
`BroadcastTransaction` error it returns an empty txid
(`internal/tron/service.go:803-821`). The HTTP layer marks the operation failed
when the txid is empty (`internal/httpapi/platform.go:884-900` for a transfer
and likewise `:1079-1089` for staking/delegation). For a failed operation the
API openly suggests retrying with a new idempotency key
(`platform.go:1244-1252`), and the UI resets the key after an error
(`internal/httpapi/ui/views/dashboard.js:698-701`).

**Exploitation.** The node accepted the first transaction, but the answer was
lost to a timeout or a reset. The user repeats the action. The backend builds new
Tron raw data with a different reference block/timestamp and signs a second,
independent operation. Both can execute.

**What to fix.** Compute the txid locally before the network call and durably
store the signed bytes with status `broadcasting`. A transport error after
sending is `broadcast_unknown`, not `failed`. After that:

- check the txid through independent endpoints;
- re-broadcast **the same signed bytes** when necessary, which is safe because
  the txid is the same;
- do not allow a new build for the same business intent until the original
  transaction has been found, has expired, or the user has explicitly confirmed
  a replacement.

**Acceptance criterion.** A fake broadcaster accepts the transaction and cuts the
answer off. A repeated request does not build or sign a second time; it returns
the original local txid with status `broadcast_unknown`.

**Fixed.** Four changes, and the classification is the load-bearing one.

`broadcastError` splits a node's refusal from silence. A `*client.BroadcastError`
exists only because a node answered and turned the transaction down, so nothing
was accepted and a fresh attempt is safe. Anything else — a timeout, a reset, a
closed stream — carries `chain.ErrBroadcastUnknown` and travels with the txid
that SEC-02 already computes from the signed bytes. `DUP_TRANSACTION_ERROR` is
treated as success: it is what a re-broadcast of the same transaction looks like
from the node's side.

The Tron adapter stopped discarding that txid, which is what turned a lost
answer into `failed`. All five signing handlers now go through one path that
records `broadcast_unknown` with 202 and a warning, and reserves `failed` for
operations that provably never reached a node.

The transaction id is written down _before_ the signature. A Tron txid is
sha256 of the raw data, which is exactly the digest handed to the signer — so a
wrapper around the signer can persist it with status `broadcasting` while there
is still nothing on the wire. If the wallet dies between the signature and the
node's answer, the record still names the transaction. This works only because
Tron's digest is its transaction id; the EVM equivalent stays the hash computed
just before the send.

`rejectIncompleteReplay` now branches on whether a transaction may exist rather
than on whether an id happens to be recorded, and an unrecognised status is
treated as in-flight. The UI stops resetting the idempotency key on any error,
keeps it whenever the transaction may be on chain, and says so instead of
reporting an ordinary send.

Folded in from the sub-threshold list: `Begin` normalised the idempotency key and
`Update` did not, so a key padded with `\v`, `\f` or U+00A0 — none of which
net/textproto strips — was reserved under one name and looked up under another.
It surfaced only _after_ the transaction was signed and broadcast: "operation
not found", the txid lost, the record stuck at `building`, every retry a
permanent 409. Both now call `operation.NormalizeKey`, and `Update` no longer
erases a known txid when passed an empty one.

**Not done: re-broadcasting the stored signed bytes.** The fix above stops a
second transaction from being built, which is the harm in this finding.
Re-sending the original bytes would additionally let the transfer complete
rather than vanish, and is worth doing — but it needs the signed bytes persisted
per operation and a policy for a Tron transaction that has passed its
expiration, and neither can be validated without a live network.

Covered by `TestBroadcastErrorSeparatesRejectionFromSilence`,
`TestReplayOnlyInvitesANewTransactionWhenTheOldOneCannotExist`,
`TestSendReturnsLocalHashWhenBroadcastResultIsUnknown`,
`TestKeyNormalizationSurvivesTheRoundTrip` and
`TestUpdateKeepsAKnownTransactionID`.

### SEC-07 — Medium — env-resolved RPC secrets are written to the cache and leak into errors

**Evidence.** `${ENV}` inside an RPC URL is expanded at runtime
(`internal/rpcpool/resolver.go:83-91`). The successful endpoint is passed to
`MarkHealthy` and written in full into `cache/rpc-nodes.json`
(`resolver.go:124-143`; the calls are `internal/chain/evm/adapter.go:522-525`
and `internal/chain/tron/adapter.go:612-618`). If an API token sits in the path
or the query, its expanded value materialises on disk even though the original
YAML held nothing but an env reference.

During a manual RPC check the fully expanded endpoint is included in the error
at `internal/httpapi/platform.go:685-702`; the error then goes to the UI and, in
some scenarios, into the log (`platform.go:1477-1505`).

**What to fix.** The cache must store an opaque endpoint ID, or a URL stripped of
userinfo, query and secret path. Do not cache custom secret URLs at all. For
errors and logs, use a single redactor that removes userinfo, the query and
secret path segments. Keep credentials out of the URL.

### SEC-08 — Medium — unlock/create allow online guessing and resource exhaustion

**Evidence.** There is no rate limit, no failed-attempt backoff and no overall
limit on expensive KDF operations. A single unlock uses Argon2id with 64 MiB of
memory and three passes (`internal/vault/vault.go:54`, `:138-147`), and the
Manager's global mutex is held for the duration of the KDF
(`internal/space/manager.go:422-443`). That blocks operations across every
space. Create runs the KDF twice (`manager.go:250-314`) and is reachable through
the unauthenticated API.

The HTTP server sets only `ReadHeaderTimeout`; `ReadTimeout`, `WriteTimeout`,
`IdleTimeout`, an explicit `MaxHeaderBytes` and a connection/concurrency budget
are all absent (`cmd/walletspace/main.go:148`).

**Impact.** A browser going through SEC-01, or a local sandboxed process, can
guess the password one attempt after another, occupy the CPU and the KDF mutex
indefinitely, hold connections open and create data until the disk fills.

**What to fix.** Behind capability authentication, add a per-space exponential
cooldown with jitter, a global KDF semaphore, per-space locks instead of one
global lock, quotas for spaces/assets/operations, and server timeouts. Unlock
errors must stay indistinguishable. The rate-limit state must not be bypassable
by a restart without a visible user action.

**Fixed.** Server timeouts and `MaxHeaderBytes` were set earlier, with SEC-01.
The rest:

Failed unlocks earn an exponential cooldown, doubling from two seconds to a
fifteen-minute ceiling after three free attempts, jittered by up to a quarter of
each wait. The jitter is what stops an attacker from sleeping exactly long
enough and keeping a steady rate. The counter lives in `unlock.json` beside the
space, because restarting the wallet is not difficult for anyone who can already
reach the API. While the cooldown holds, the _correct_ password is refused too —
otherwise the throttle would confirm a guess without the attacker having to
wait — and `ErrTooManyAttempts` says nothing about the password.

Derivations moved off the manager's mutex. Holding it across a 64 MiB Argon2
pass meant one unlock froze every other space, the space list and the auto-lock
sweep; `ChangePassword` did it three times in a row and was the longest lock
hold in the process. The mutex is now held only for map work, and a per-space
lock keeps two attempts on one space in order — which is also what stops a
password change from swapping the container out from under an unlock midway
through deriving a key for the old one. A semaphore caps concurrent derivations
at min(NumCPU/2, 4), so a burst cannot put half a gigabyte of Argon2 in flight.

Quotas: 64 spaces, 512 accounts per space, 256 configured assets, 512 operation
records. The space quota is checked _before_ the two derivations in `Create`, so
a caller at the ceiling cannot spend 128 MiB of Argon2 per request discovering
it. The operations file previously grew for the life of a space and is read and
rewritten in full on every operation; it is now pruned oldest-first, and a
record for a transaction that is not resolved is never dropped to make room —
if nothing is droppable the new operation is refused, because being unable to
start a transfer is recoverable and forgetting one in flight is not.

KDF bounds are now enforced at both ends and on the way out as well as in.
Ceilings came down from 1 GiB × 10 to 256 MiB × 10, so a doctored header cannot
turn one unlock attempt into a gigabyte allocation. Floors went up from "any
non-zero" to 32 MiB × 2, and `Seal` re-validates the parameters it inherited
from the container it opened — that is the path by which a weakened header would
otherwise have been adopted and written back permanently.

Covered by `TestFailedUnlocksEarnAGrowingCooldown` (including the restart and
the correct-password case), `TestUnlockDelayGrowsWithJitterAndStopsAtTheCeiling`,
`TestASlowUnlockDoesNotBlockOtherSpaces`, `TestSpaceAndAccountQuotas`,
`TestConfiguredAssetsAreCapped`, the three `TestPrune*` cases,
`TestKDFParametersAreBoundedInBothDirections` and
`TestAWeakenedHeaderIsNeverOpenedOrResealed`.

### SEC-09 — Medium — the confirmation screen hides most of the address and the backend does not check EIP-55

**Evidence.** `shortAddress` keeps the first 9 and the last 7 characters
(`internal/httpapi/ui/components/ui.js:80-81`). It is precisely that shortened
recipient which is shown before signing
(`internal/httpapi/ui/views/dashboard.js:681-684`). The EVM backend uses only
`common.IsHexAddress` (`internal/chain/evm/adapter.go:424-430`), so a mixed-case
address with a wrong EIP-55 checksum is not rejected.

**Impact.** Address-poisoning or clipboard malware can match a similar beginning
and end; the user has no way to check the differing middle on the final screen.
A wrong mixed-case checksum does not stop a typo.

**What to fix.** On the confirmation, show the full recipient, the network, the
chain ID, the asset contract, the amount and the maximum fee; group the
characters visually, but do not hide any. For EVM, accept a fully lowercase
address or one with a valid EIP-55 checksum; reject mixed case with a wrong
checksum.

### SEC-10 — Medium — a discovery response can create an unbounded goroutine fan-out

**Evidence.** The size of the discovery JSON is capped at 2 MiB, but neither the
number of URLs nor the length of the list is bounded
(`internal/rpcpool/resolver.go:238-260`). The parser recursively pulls strings
out of an arbitrary structure (`resolver.go:323-346`). The Doctor creates one
goroutine per endpoint and only waits on the shared limiter inside the goroutine
(`internal/doctor/doctor.go:201-230`).

**Exploitation.** A compromised discovery service returns tens of thousands of
short URLs. Every one-minute check allocates large slices and starts thousands
of goroutines per network, causing memory and CPU exhaustion.

**What to fix.** Use a strict versioned schema; accept no more than 8–16
endpoints per network and bound the URL length and the number of JSON nodes
before sorting or any DNS lookup. The Doctor must use a fixed worker pool and
not create a goroutine before it holds a slot.

### SEC-11 — Medium — exporting secrets needs no step-up authorization, and an unlocked session can last forever

**Evidence.** Exporting the mnemonic or a private key requires nothing but an
already unlocked space, and accepts neither the current password nor a one-time
confirmation (`internal/httpapi/platform.go:235-243`, `:364-380`; UI:
`internal/httpapi/ui/features/accounts/dialogs.js:98-124`). Auto-lock can be set
to `0`, which disables expiration entirely
(`internal/space/manager.go:935-944`). The minimum vault password length is only
8 bytes (`manager.go:27-30`, `:520-527`).

**Impact.** Brief access to an unlocked tab, a same-origin XSS or a local API
client immediately yields long-lived master secrets. A copy of a backup with a
human eight-character password is open to offline guessing.

**What to fix.** For a mnemonic or private-key export, require the password
again, or a short-lived one-time step-up grant tied to an explicit user gesture.
For sending funds, require a separate confirmation of the exact intent. Do not
allow auto-lock to be switched off entirely in a production profile; show the
risk and impose a safe upper bound. Instead of a single length check, score weak
and breached passwords and recommend a password manager.

## Recommended signing architecture

Given that the project starts from a clean slate, EVM and Tron are worth
unifying around a single immutable signing pipeline:

```text
User input
  -> typed Intent
  -> local deterministic Builder
  -> Policy Validator
  -> immutable PreparedTransaction + hash
  -> UI confirmation of every material field
  -> step-up/capability authorization
  -> Signer signs exact canonical bytes
  -> durable txid + signed bytes
  -> Broadcaster
  -> confirmed | rejected | broadcast_unknown
```

RPC must never choose the recipient, the amount, the contract calldata or the
fee after the user has confirmed. RPC may supply chain state (nonce, base fee,
head/reference block), but those values have to pass policy and become part of
the immutable prepared transaction.

## Fix order

### P0 — before any real funds

1. SEC-01: a capability-authenticated local transport plus a strict Host/origin
   check.
2. SEC-02: local construction, or full semantic verification, of a Tron
   transaction.
3. SEC-03: remove the HTML sinks for on-chain data and enable a CSP.
4. SEC-04: bind the UI approval to the exact EVM transaction and to fee maxima.

### P1 — before public testing

5. SEC-05: endpoint-scoped credentials.
6. SEC-06: durable `broadcast_unknown` and a retry of the exact signed
   transaction.
7. SEC-08: rate limits, KDF/connection/quota budgets.
8. SEC-11: step-up for a seed or private-key export.

### P2 — hardening before release

9. SEC-07: a secret-safe cache/error/log model.
10. SEC-09: the full address and a checksum policy.
11. SEC-10: a strict discovery schema and bounded workers.

## What is already right

The following mechanisms were checked and are not findings:

- the vault uses Argon2id and AES-256-GCM with a random salt/nonce and AAD;
- the KDF/ciphertext parameters are bounded before any allocation
  (`internal/vault/vault.go:114-147`);
- files are written atomically with `0600`, directories are forced to `0700`
  (`internal/storage/storage.go:28-84`);
- the private RPC dialer blocks loopback, private and special IPs and re-checks
  DNS on connect; redirects are disabled
  (`internal/rpcpool/resolver.go:180-211`);
- the EVM chain ID and the declared Tron network identity are verified before an
  endpoint is chosen;
- write bodies are bounded and the JSON decoder rejects unknown fields;
- secret responses get `Cache-Control: no-store`;
- provider header values are not returned by the settings API;
- the EVM transaction hash is computed locally before the broadcast and already
  has a `broadcast_unknown` state on a send error.

These measures are useful, but they do not compensate for the findings above.

## Checks and limits of this audit

Performed:

- a manual review of every production package and of the embedded UI;
- tracing the trust boundaries from the HTTP request to the signer and the
  broadcast;
- `go test ./... -count=1` — passes outside the sandbox (the tests need a
  loopback bind);
- `go vet ./...` — passes;
- `govulncheck -show verbose ./...` on 2026-07-31 — no reachable vulnerable
  symbols found.

`govulncheck` additionally reported two unreachable advisories:

- [GO-2026-5158](https://pkg.go.dev/vuln/GO-2026-5158) for the imported
  `go.opentelemetry.io/otel@v1.43.0`, fixed in `v1.44.0`; the vulnerable symbol
  is never called from Walletspace;
- [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932) for
  `golang.org/x/crypto/openpgp`, which is unused here; the vulnerable
  package/symbol is never called.

No live operations against real networks and no fuzzing of external RPC
responses were carried out. After P0/P1, another security review is needed,
together with an adversarial integration suite using a fake browser origin, a
fake discovery service and fake Tron/EVM RPC. _The suite exists as of
2026-08-10, in `internal/integration`; the review does not._

Re-run on 2026-08-10 at `8709f93`: `go vet ./...` clean, the full test suite
green across every package, `govulncheck ./...` with no reachable vulnerable
symbols. GO-2026-5158 no longer applies: `go.opentelemetry.io/otel` has moved
past the release that fixed it. GO-2026-5932 stands, unreachable, with no fixed version: the package
is unmaintained by upstream policy rather than carrying a specific bug, and
nothing here imports it.

## Remaining work

Every item on the 2026-08-10 plan that could be done offline is done, and so is
everything the review of it found except two that are decisions rather than
patches. What is left:

### Needs a decision

1. **Settings that weaken the wallet with the token alone** — BE-7. The RPC a
   wallet trusts, the auto-lock, the length of the spending window. Each has
   the shape BE-6 had, and BE-6's answer — take it out of the API — costs a
   feature here that it did not cost there.
2. **Deadlines on a clock that can move backwards** — BE-8. Small, contained,
   and already solved one file away for the unlock cooldown.

### Needs a testnet

1. **Build `raw_data` locally** — SEC-02's remaining half. Take only the head
   block from RPC, assemble the contract and the header here, and keep
   `Intent.Verify` as an assertion over what was built rather than as the only
   barrier. An incorrectly assembled reference block produces transactions that
   simply fail to broadcast, and no offline test can tell the difference. BE-3
   has narrowed what the node still chooses to the reference block itself.
2. **Persist the signed bytes and re-broadcast them** — SEC-06's remaining half.
   Store the serialised signed transaction with the operation record, and on a
   `broadcast_unknown` retry send _those bytes_ to a different endpoint rather
   than building anything. Needs a policy for a transaction that has passed its
   expiration, which the BE-3 bounds now make knowable. Note that persisting
   signed bytes means a file that authorises a transfer to anyone who reads it:
   `0600` and the data directory lock are the only things guarding it, and that
   trade is part of what a testnet run should be used to think through.

### Needs a person

1. **A follow-up review by someone who did not write the fixes.** The
   remediation's own internal review found fourteen defects, five of which
   partly undid the finding they were meant to close, and the 2026-08-10 pass
   found three more. That rate is the argument for this step, not the
   complexity of any single fix. The adversarial suite it was meant to
   accompany now exists, so the review has something to run.
