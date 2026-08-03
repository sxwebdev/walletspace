# Security audit Walletspace

Audit date: 2026-07-31
Revision: `22267c1`
Scope: the whole Walletspace service — Go backend, embedded web UI,
vault/storage, EVM/Tron signing, RPC discovery and Node Doctor.

## Remediation status

Last updated: 2026-08-03. Everything below the table is the audit exactly as it
was written, including the parts later found to be wrong — the table and the
per-finding "Fixed" notes are what has happened since. Two further findings,
BE-1 and BE-2, were raised during remediation and are described after it.

All eleven findings are closed. Two pieces of follow-up work were deliberately
left undone because neither can be validated without a live network: building
Tron transaction data locally rather than checking what a node built (SEC-02),
and re-sending the stored bytes of a broadcast whose answer was lost (SEC-06).
Both are defence in depth on top of a closed finding, not the fix itself.

**A follow-up review has not been done.** The audit asks for one after P0 and
P1, together with an adversarial integration suite driven by a fake browser
origin, a fake discovery service and fake Tron/EVM nodes. Until that exists,
the fixes below are as reviewed as the code that contained the findings was.

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

| Finding | Status | Note |
| --- | --- | --- |
| SEC-01 | **Fixed** | Capability token on every `/api/` route, random loopback port, `Host` checked against the address actually opened, full-origin comparison |
| SEC-02 | **Fixed for signing; local construction outstanding** | raw_data is decoded and compared field by field against a local intent before any signature, and the txid is computed from the signed bytes. The transaction is still *assembled* by the node — see below |
| SEC-03 | **Fixed** | The escaping half was already done before this work began; CSP, `nosniff`, `Referrer-Policy`, `X-Frame-Options` and server-side metadata bounds were added |
| SEC-04 | **Fixed** | The confirmed fee is signed; a network that moves past it forces re-confirmation; absurd node quotes are refused |
| SEC-05 | **Fixed** | `networks.yaml` schema 2 binds credentials to a single endpoint; the resolver, both adapters and the Doctor resolve them per endpoint. Existing files migrate on start |
| SEC-06 | **Fixed** | Already correct for EVM before this work. Tron now keeps the locally computed txid, distinguishes a node's rejection from a lost answer, and records the transaction id before it signs. Re-broadcast of the stored bytes is not implemented — see below |
| SEC-07 | **Fixed** | Single redactor on errors and logs; secret-bearing endpoints are no longer cached |
| SEC-08 | **Fixed** | Server timeouts, an exponential unlock cooldown that survives restart, a global KDF semaphore, derivations moved off the global lock with per-space serialisation, quotas on spaces/accounts/assets/operations, and KDF bounds tightened at both ends |
| SEC-09 | **Fixed** | Full grouped address on the confirmation screen; EIP-55 enforced on the recipient |
| SEC-10 | **Fixed** | Discovery bounded by count, URL length, node count and depth; Doctor uses a bounded worker pool |
| SEC-11 | **Fixed** | Password step-up on both exports, auto-lock cannot be disabled and is bounded, password strength scored, revealed secrets expire |

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
  through `Get`, which never touches `lastUsed`. Only five deliberate actions
  refresh it, which is the intended behaviour.
- **"What is already right"** credits the private dialer with blocking loopback
  and private IPs on every RPC connection. That was true for EVM and false for
  Tron — see BE-2.

### SEC-02, what remains

The signing barrier is closed: a node cannot get a signature over anything
other than the operation that was asked for, and 23 substitution cases are
covered by tests that also assert `SignDigest` is never reached. What is *not*
done is building `raw_data` locally, which would take the node out of the
construction path altogether. That work needs live-network validation before it
can be trusted — an incorrectly assembled reference block or fee limit produces
transactions that fail to broadcast — so it was deliberately left for a change
that can be tested against a testnet.

### BE-1 — High — comma smuggling in the Tron node list (fixed)

Not in the original audit. Every Tron endpoint was validated and probed as a
single URL string, then joined with `,` and re-split into a *list* of nodes. A
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

| Severity |     Count | Admissible for a release with real funds |
| -------- | --------: | ---------------------------------------- |
| Critical |         2 | Blocks                                   |
| High     |         4 | Blocks                                   |
| Medium   |         5 | Fix before a public release              |

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

The transaction id is written down *before* the signature. A Tron txid is
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
It surfaced only *after* the transaction was signed and broadcast: "operation
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
reach the API. While the cooldown holds, the *correct* password is refused too —
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
records. The space quota is checked *before* the two derivations in `Create`, so
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
fake discovery service and fake Tron/EVM RPC.
