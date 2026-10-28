# Walletspace

Secure self-hosted wallet for Tron, EVM and Solana. Keys are encrypted locally
with Argon2id + AES-256-GCM, unlocked in memory, and transactions are signed
locally. Runs on your computer, works in your browser. Optional sync shares
encrypted wallet metadata and imported keys through a folder such as iCloud
Drive. Its encryption key comes from each space's recovery phrase and BIP39
passphrase, if set; the phrase and passphrase are never sent.

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

| Command                          | What it does                  |
| -------------------------------- | ----------------------------- |
| `walletspace`                    | start the UI                  |
| `walletspace version`            | print the version             |
| `walletspace migrate --from DIR` | import an old data directory  |
| `walletspace export --out FILE`  | write spaces to a backup file |
| `walletspace import FILE`        | add the spaces from a file    |
| `walletspace help`               | all commands and flags        |

## Data and settings

Everything lives in `~/.walletspace` (change it with `WALLETSPACE_HOME`):
encrypted vaults, `config.yaml`, `networks.yaml` and `device.json` (the
installation's sync identity). Settings — RPC nodes, auto-lock, sync, etc. —
are easiest to change on the **Settings** page in the UI.

Other environment variables:

- `WALLETSPACE_ADDR` — listen address, e.g. `127.0.0.1:8080` (loopback only)
- `WALLETSPACE_OPEN_BROWSER` — `false` to not open the browser

Uninstalling does not delete `~/.walletspace`. **Before removing the data,
preserve every recovery phrase and its BIP39 passphrase, if set, and keep a
current encrypted backup of imported keys.** A phrase cannot regenerate
independently imported keys without that backup or current sync files.

## Several computers

**Moving spaces to another computer.** Settings → Backup & sync → _Move spaces
to another computer_ writes one `.wsbackup` file with the selected spaces (each
asks for its password). Carry it directly — AirDrop, a USB stick, `scp` — and
open it on the other computer with _Import from a backup file_ on the start
screen. Spaces arrive locked and open with the passwords they had. Delete the
file on both computers afterwards: it is as sensitive as `~/.walletspace`.

**Sync.** Settings → Backup & sync → _Sync between computers_, with the same
folder on every computer (iCloud Drive is filled in on a Mac; Dropbox or a
Syncthing folder work the same way). Each space writes one file there,
encrypted with a key derived from its recovery phrase and optional BIP39
passphrase: wallets, labels, network bindings and imported private keys.
The phrase, BIP39 passphrase and local password are never sent. A space reads
and merges changes from the folder while it is unlocked; its own changes are
queued as an encrypted file and sent even after it locks. Deleting a wallet deletes it on
every computer, an imported wallet together with its key.

On a new computer a space comes back whole, imported keys included, from three
things: its recovery phrase, its BIP39 passphrase if it has one, and a current
copy of its files in the folder. The last one is the one to watch: a file
written to the folder is in the cloud only once iCloud (or Dropbox, or
Syncthing) has copied it, and a folder restored to an older state lacks what
was added since. Keep the phrase on paper and, for a space that matters, an
export file as well. A transfer whose outcome was unknown on one computer may
already be on chain: check the explorer before repeating it from another.
Details: [docs/device-sync.md](docs/device-sync.md).

A copy of `~/.walletspace` on another computer becomes a sync writer of its
own. A disk or virtual-machine clone of the same computer does not: both copies
would write the same sync file. Before using such a clone, quit Walletspace on
it, delete `device.json` in its data folder and start it again. To move spaces,
prefer the backup file above.

## How keys are protected

- Each space is a separate vault encrypted with **AES-256-GCM**. The key is
  derived from the space password with **Argon2id** (64 MiB, 3 passes) and a
  random salt; every write uses a fresh random nonce.
- Recovery phrases, BIP39 passphrases and imported private keys are never
  written to disk unencrypted. Files are `0600`, directories `0700`.
- Every space has a recovery phrase. Spaces made without one by older versions
  can create one in the space's settings.
- Sync is off by default. The sync folder holds ciphertext encrypted with a
  256-bit key derived from the space's seed, rather than its local password.
  Security depends on the recovery secret: a 12-word BIP39 phrase has 128 bits
  of entropy; the generated 24-word phrase has 256 bits. The encryption key is
  held only in memory while the space is unlocked. The phrase and BIP39
  passphrase are never sent.
- Keys are decrypted only in memory while a space is unlocked. The space locks
  itself after an idle period (15 min by default, 1 min - 24 h).
- Sending funds, revealing a phrase or key, and downloading a backup ask for
  the password again.
- Transactions are signed locally; RPC nodes only receive signed transactions.
- The UI listens on `127.0.0.1` only, and every API call needs a token that is
  generated anew on each start.

## What it can do

- Several independent spaces, each with its own password
- Sync between your computers through iCloud Drive, Dropbox or Syncthing, and
  a backup file to move spaces directly
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
