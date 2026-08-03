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

> **Not ready for real funds.** The audit in
> [docs/security-audit.md](docs/security-audit.md) lists two unfixed Critical
> findings: the local API has no authorization boundary and is reachable from a
> web page through DNS rebinding, and a Tron transaction is built by the RPC node
> and signed without any check of its contents. Use testnets until both are
> closed.

- The API binds to loopback only; a non-loopback bind is rejected. Loopback
  constrains the network route, not the authority of the caller — there is no
  capability token yet, so any local process can call the API.
- Vaults use Argon2id and AES-256-GCM. The mnemonic, the BIP39 passphrase and
  imported keys are never written to disk in the clear. Files are created with
  `0600`, directories with `0700`.
- Mutating browser requests are checked against Origin and Sec-Fetch-Site and
  must carry a JSON content type; secret responses get `Cache-Control: no-store`.
  These are CSRF hardening, not authentication, and the audit's SEC-01 shows how
  they are bypassed.
- A space locks itself after a configurable idle period and has to be unlocked
  before a key export, a signature or an import.
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

Start the UI with `walletspace`. It opens on <http://127.0.0.1:8080> by default
and offers to create a space on the first run. `walletspace help` lists the
commands, the `migrate` flags and the environment overrides.

## License

MIT — see [LICENSE](LICENSE).
