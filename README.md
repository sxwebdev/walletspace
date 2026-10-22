# Walletspace

Secure self-hosted wallet for Tron, EVM and Solana. Your keys stay on your
machine: encrypted with Argon2id + AES-256-GCM, unlocked only in memory, and
every transaction is signed locally. Runs on your computer, works in your
browser.

## Install

### macOS (Homebrew)

```sh
brew trust --tap https://github.com/sxwebdev/walletspace
brew tap sxwebdev/walletspace https://github.com/sxwebdev/walletspace
brew install --cask walletspace
```

**macOS / Linux (binary)** — download an archive from
[Releases](https://github.com/sxwebdev/walletspace/releases), unpack it and put
`walletspace` somewhere on your `PATH`.

Windows is not supported.

## Run

```sh
walletspace
```

It starts on a random local port and opens the browser. If the browser does not
open, copy the link printed in the terminal — it contains an access token, so
typing just the address will not work. On the first run, create a space (a
password-protected wallet set) and either generate a new recovery phrase or
restore an existing one.

Stop it with `Ctrl+C`.

| Command                          | What it does                 |
| -------------------------------- | ---------------------------- |
| `walletspace`                    | start the UI                 |
| `walletspace version`            | print the version            |
| `walletspace migrate --from DIR` | import an old data directory |
| `walletspace help`               | all commands and flags       |

## Data and settings

Everything lives in `~/.walletspace` (change it with `WALLETSPACE_HOME`):
encrypted vaults, `config.yaml` and `networks.yaml`. Settings — RPC nodes,
auto-lock, etc. — are easiest to change on the **Settings** page in the UI.

Other environment variables:

- `WALLETSPACE_ADDR` — listen address, e.g. `127.0.0.1:8080` (loopback only)
- `WALLETSPACE_OPEN_BROWSER` — `false` to not open the browser

Uninstalling does not delete `~/.walletspace`. **Write down every recovery
phrase before removing it** — nothing else can restore the wallets.

## How keys are protected

- Each space is a separate vault encrypted with **AES-256-GCM**. The key is
  derived from the space password with **Argon2id** (64 MiB, 3 passes) and a
  random salt; every write uses a fresh random nonce.
- Recovery phrases, BIP39 passphrases and imported private keys are never
  written to disk unencrypted. Files are `0600`, directories `0700`.
- Keys are decrypted only in memory while a space is unlocked. The space locks
  itself after an idle period (15 min by default, 1 min - 24 h).
- Sending funds, revealing a phrase or key, and downloading a backup ask for
  the password again.
- Transactions are signed locally; RPC nodes only receive signed transactions.
- The UI listens on `127.0.0.1` only, and every API call needs a token that is
  generated anew on each start.

## What it can do

- Several independent spaces, each with its own password
- HD wallets from a BIP39 phrase (same addresses as TronLink, MetaMask, Phantom)
  and imported private keys
- 20 networks: Tron, Ethereum, BSC, Polygon, OP, Arbitrum, Base, Avalanche, Kaia,
  Robinhood Chain, Solana, plus testnets
- Native and token balances (TRC20, ERC20, SPL) with USD prices; send and receive
- Swaps: TRX ↔ USDT on SunSwap, Solana tokens on Raydium
- ETH/WETH and BNB/WBNB wrap/unwrap
- Tron staking, energy/bandwidth delegation, contract deployment
- Gas top-ups from your other wallets when a token send lacks gas

## Development

```sh
make start   # run from source (build the UI first with `make ui`)
make test
make lint
```

## License

MIT — see [LICENSE](LICENSE).
