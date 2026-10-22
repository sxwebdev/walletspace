# Walletspace

Local multichain wallet manager: several isolated spaces, derived and imported
accounts, Tron, EVM and Solana networks in one loopback UI. Private keys are used
locally and are never sent to an RPC provider.

## Features

- Independent spaces, each with its own password and encrypted vault.
- A new BIP39 recovery phrase or recovery of an existing one, with BIP44
  derivation for Tron (`m/44'/195'/0'/0/i`) and EVM (`m/44'/60'/0'/0/i`), and
  SLIP-0010 ed25519 derivation for Solana (`m/44'/501'/i'/0'`, the path Phantom
  and Solflare use, so the same phrase shows the same addresses there).
- Explicit network bindings: a wallet exists only in the networks it was created
  or connected in, the derivation index starts at `0` separately in each network,
  and a compatible key is not duplicated.
- Import of a secp256k1 private key, or of a Solana keypair as base58 or as the
  JSON byte array `solana-keygen` writes, marked `Imported` in the UI; export of
  a private key (a Solana keypair as base58) or a recovery phrase only from an
  unlocked space.
- 20 networks at once: Tron, Ethereum, BSC, Polygon PoS, OP, Arbitrum, Robinhood
  Chain and Avalanche C-Chain, each with its testnet, Base and Kaia mainnets,
  and Solana Mainnet and Devnet.
- Native, ERC20, TRC20 and SPL balances and transfers; Tron resources, staking,
  delegation and contract deployment.
- Solana: SOL and classic SPL tokens, with USDC and USDT built in and any other
  token added by its mint; a mint needs Metaplex metadata to be added, and
  Token-2022 tokens are not supported. The fee is the base fee plus a priority
  fee at a recent price, at least 1,000 micro-lamports per compute unit; a
  recipient's token account is opened when it is missing, and its rent is shown
  before you confirm. The exact transaction is simulated before signing and the
  runtime's answer decides — a refusal is shown in plain words and nothing is
  signed. **Close empty accounts** closes the wallet's empty token accounts and
  returns their rent; frozen accounts are left alone.
- Solana swaps on mainnet, through Raydium pools only (AMM v4, CPMM and CLMM).
  Routes come from Raydium's keyless Trade API; when it is down or its route
  cannot be used, the pools are found on chain instead. The wallet builds every
  instruction itself from the pools' on-chain state, simulates the swap to learn
  exactly what it pays, and checks the transaction it signs against that plan.
  Routes have one pool, or two through SOL, USDC or USDT; slippage is up to 3%.
  Swap data powered by Raydium.
- Solana uses the public Solana RPC by default, which is rate-limited; set your
  own node in the network's settings for heavier use. Not supported on Solana
  yet: Token-2022 tokens, swaps on devnet, Gas.zip top-ups and on-chain
  transaction history.
- On-chain wrap/unwrap at 1:1 through native wrapper contracts: ETH/WETH on
  Ethereum, Base, OP and Arbitrum; BNB/WBNB on BSC mainnet and testnet. Use
  **Wrap / Unwrap** beside the wallet, choose a direction and amount (or MAX),
  estimate the fee, then sign. Both directions require native coins for gas.
  Supported pairs are defined per network; bridged tokens are not native wrappers.
- TRX ↔ USDT swaps on Tron through SunSwap V2, V3 and V4, mainnet and Nile.
  Use **Swap** beside a Tron wallet: every pool is quoted — V2 and V3 by the
  routers' own quoters, V4 by repeating the pool's arithmetic locally, since
  SunSwap publishes no V4 quoter on Tron — and the routes are ranked by what
  is left after network fees, because an approval costs several TRX. Selling
  USDT approves exactly the amount sold unless you opt into an unlimited
  approval; V4 adds a Permit2 grant to the universal router that expires
  after an hour. All calldata is built locally, the minimum you approve is
  what is signed, and each step is checked against the current price and
  simulated before signing, so a swap the pool would revert costs nothing.
- Native and token amounts with a USD estimate beside each wallet balance;
  unavailable prices and cached values are labelled, and testnets have no USD value.
- Progressive background balance loading, a mainnet USD total and a 24h price
  change from the keyless DefiLlama feed, cached for five minutes.
- Gas top-ups inside the EVM token send form: if native gas is missing, compare
  funded wallets in the space by total cost, use a direct transfer or a Gas.zip
  native-to-native refill, wait for confirmed delivery, then review and send the
  token on its existing network. Quotes budget the fee shortfall with a 10%
  reserve, include source gas and provider minimums, and show the estimated
  remainder. An exactly zero remainder cannot be guaranteed as fees change.
  Supported cross-chain funding sources: Ethereum, BSC, Polygon, OP, Arbitrum,
  Base, Avalanche and Kaia; destinations depend on current Gas.zip availability.
  Pending top-ups survive restarts and cannot be paid twice with the same plan.
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
- A Solana transaction is built locally — transfers, account closes and every
  instruction of a Raydium swap, from the pools' on-chain state — then decoded
  on its own and compared with what was asked before it is signed: programs,
  accounts, amounts, the signer and which accounts may be written. Addresses a
  swap loads from lookup tables are checked against those tables' contents on
  chain. The approved compute limit, priority price and swap minimum are what
  get signed.
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
- Every outbound request — each RPC connection, the node-discovery poll and the
  Raydium API — goes through a dialer that resolves the host itself and refuses loopback,
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
brew trust --tap https://github.com/sxwebdev/walletspace
brew tap sxwebdev/walletspace https://github.com/sxwebdev/walletspace
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

The UI is a separate frontend build that is not committed, so a binary from
`go install` serves a page saying the UI is missing. Use a release archive, the
cask, or a build from source for the full wallet.

### From source

Needs Go, Node.js 22.12+ and pnpm: the React UI in [ui/](ui) is built first
and embedded into the binary.

```sh
make build     # ./bin/walletspace with the UI, version stamped from git describe
make install   # the same into $GOBIN
make ui        # only the frontend, into internal/httpapi/ui/dist
```

Start the UI with `walletspace`. It picks a free loopback port, prints the URL
to open — including the capability token for this run — and offers to create a
space on the first run. The printed link is the only way in: without the token
every API call is refused, so open it rather than typing the address by hand.
`walletspace help` lists the commands, the `migrate` flags and the environment
overrides.

## License

MIT — see [LICENSE](LICENSE).
