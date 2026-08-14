# Walletspace

Local multichain wallet manager: several isolated spaces, derived and imported
accounts, Tron and EVM networks in one loopback UI. Private keys are used locally
and are never sent to an RPC provider.

## Features

- Independent spaces, each with its own password and encrypted vault.
- A new BIP39 recovery phrase or recovery of an existing one, with BIP44
  derivation for Tron (`m/44'/195'/0'/0/i`) and EVM (`m/44'/60'/0'/0/i`).
- Explicit network bindings: a wallet exists only in the networks it was created
  or connected in, the derivation index starts at `0` separately in each network,
  and a compatible key is not duplicated.
- Import of a secp256k1 private key, marked `Imported` in the UI; export of a
  private key or a recovery phrase only from an unlocked space.
- 17 networks at once: Tron, Ethereum, BSC, Polygon PoS, OP, Arbitrum, Robinhood
  Chain and Avalanche C-Chain, each with its testnet, plus Base mainnet.
- Native, ERC20 and TRC20 balances and transfers; Tron resources, staking,
  delegation and contract deployment.
- Progressive background balance loading, a mainnet USD total and a 24h price
  change from the keyless DefiLlama feed, cached for five minutes.
- A background Node Doctor that checks the RPC nodes of every enabled network.
- Typed settings UI for RPC, provider headers, explorer, discovery, assets,
  auto-lock and general options.

## Security

> **Still not recommended for large balances.** Seventeen of the nineteen
> findings in [docs/security-audit.md](docs/security-audit.md) — the audit and
> two review passes over the fixes — are closed. What is not is listed in one
> table at the top of that document: two items wait on a live testnet, two are
> decisions about how much the API should be allowed to weaken, and the last is
> a follow-up review by someone other than the author of the fixes. That one is
> the main reason for this warning: each review of this code so far has found
> defects in the round before it, including in fixes that looked finished. The
> adversarial integration suite the audit asked for now exists and runs on every
> push.

- The API binds to loopback and is guarded by a capability token generated on
  every start. Loopback constrains the network route, not the authority of the
  caller, so the token is what actually separates the UI this process launched
  from any other local program. It is handed to the browser in the URL fragment,
  which is never sent to a server, and required on every `/api/` route. It is
  never logged, and never passed on a command line — the browser is pointed at a
  `0600` redirect file, because a process's arguments are readable by others.
- The listener takes a random port by default, and the `Host` header is checked
  against the address actually opened. Together those close the DNS-rebinding
  path: a page on an attacker's domain arrives with the attacker's own hostname,
  whatever the DNS answer said.
- A Tron transaction is assembled with help from an RPC node, but the raw data
  is decoded and compared field by field against a locally held intent before
  anything is signed — recipient, amount, contract, calldata, resource, fee
  limit, permission id, and the number of contracts. The header is bounded
  against the local clock as well, so a node cannot have a correct transfer
  signed and then keep it valid for a day to broadcast when it suits. The
  transaction id is computed from the bytes that were signed rather than read
  back from the node.
- The EVM fee the user confirms is what gets signed. The sender does not re-ask
  the node at signing time; if the network has moved past the approved ceiling
  the transfer is refused and has to be confirmed again.
- A transaction id is written down before the transaction is signed, and a
  broadcast whose answer never came back is reported as exactly that rather than
  as a failure. Only an operation that provably never reached a node is allowed
  to be replaced — otherwise a retry would sign a second transfer while the
  first was still in flight.
- Vaults use Argon2id and AES-256-GCM. The mnemonic, the BIP39 passphrase and
  imported keys are never written to disk in the clear. Files are created with
  `0600`, directories with `0700`.
- Mutating browser requests are checked against Origin and Sec-Fetch-Site and
  must carry a JSON content type; secret responses get `Cache-Control: no-store`.
  These are CSRF hardening on top of the token, not a substitute for it.
- A strict Content-Security-Policy, `nosniff` and `no-referrer` are set on every
  response, and on-chain token metadata is escaped at every point it reaches the
  DOM.
- A space locks itself after a configurable idle period — between one minute and
  a day, and it cannot be switched off. Revealing a recovery phrase or a private
  key, and downloading the encrypted backup, ask for the space password again
  even while the space is unlocked, and what is revealed hides itself
  afterwards.
- Moving funds asks for the password too. Unlocking a space says who was at the
  keyboard when it was opened; it is not by itself authority to spend, because
  anything on the machine that can reach the wallet inherits an open space. One
  password covers the next five minutes by default, and locking the space ends
  the window early. Turning it off is a `config.yaml` edit and a restart, not a
  switch in the UI — a caller that could throw that switch would have no need to
  answer the prompt.
- Wrong passwords earn a growing, jittered cooldown that survives a restart, and
  while it holds even the right password is refused, so the wait cannot be used
  to confirm a guess. Concurrent key derivations are capped, and unlocking one
  space no longer blocks the rest of the wallet.
- A provider credential belongs to the one RPC endpoint it was configured for.
  The resolver falls through to the official fallbacks when a node stops
  answering, and node discovery can add more; none of them are sent a secret
  that was typed next to a different URL.
- Every outbound request — each RPC connection and the node-discovery poll —
  goes through a dialer that resolves the host itself and refuses loopback,
  private and special-use addresses, including the IPv6 spellings of them, so
  the answer that was checked is the one that is dialled. No proxy is consulted,
  because a proxy would be dialled in place of the host.
- Only public identifiers of assets with a non-zero balance leave the machine for
  the price feed — never addresses, balances or space identifiers.
- A recovery phrase or a private key is full control over the funds: keep them
  out of messengers, cloud notes and screenshots.

## Install

### Homebrew (macOS)

```sh
brew tap sxwebdev/walletspace https://github.com/sxwebdev/walletspace
brew trust --tap sxwebdev/walletspace
brew install --cask walletspace
```

The cask lives in this repository instead of a separate `homebrew-*` one, which is
why the tap needs a URL. Homebrew loads nothing from a tap it has not been told to
trust, hence the second line. `brew upgrade --cask walletspace` installs newer
versions; the release workflow updates the cask as soon as a tag is built.

The binaries carry no Developer ID signature — the project has no Apple
certificate — and Gatekeeper refuses to run a quarantined file, so the cask
removes the quarantine flag Homebrew attaches to every download. Trusting the tap
is the point where that is agreed to; build from source instead if you would
rather not.

`brew uninstall --cask walletspace` leaves `~/.walletspace` alone: it holds the
encrypted vaults, and the cask deliberately has no `zap` stanza so that Homebrew
cannot throw them away. Remove that directory by hand, and only once every
recovery phrase is written down.

### Prebuilt binary

Archives for macOS and Linux (amd64 and arm64) are attached to every
[release](https://github.com/sxwebdev/walletspace/releases). On Linux this is the
only prebuilt option, since Homebrew casks are macOS-only. Windows is not
supported: the data directory lock is implemented for unix alone.

### go install

```sh
go install github.com/sxwebdev/walletspace/cmd/walletspace@latest
```

### From source

```sh
make build     # ./bin/walletspace, version stamped from git describe
make install   # the same into $GOBIN
```

Start the UI with `walletspace`. It picks a free loopback port, prints the URL
to open — including the capability token for this run — and offers to create a
space on the first run. The printed link is the only way in: without the token
every API call is refused, so open it rather than typing the address by hand.
`walletspace help` lists the commands, the `migrate` flags and the environment
overrides.

## License

MIT — see [LICENSE](LICENSE).
