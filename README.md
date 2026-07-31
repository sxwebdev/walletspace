# Walletspace

Локальный multichain wallet manager: несколько изолированных spaces, derived и
imported accounts, Tron и EVM-сети в одном UI. Приватные ключи используются
только локально и никогда не отправляются RPC-провайдеру.

## Возможности

- независимые spaces с отдельным паролем и зашифрованным vault;
- создание новой BIP39 recovery phrase или восстановление существующей;
- BIP44 derivation для Tron (`m/44'/195'/0'/0/i`) и EVM
  (`m/44'/60'/0'/0/i`);
- импорт secp256k1 private key с явной меткой `Imported` в UI;
- экспорт private key и recovery phrase только из разблокированного space;
- одновременная работа с 17 сетями:
  - Tron Mainnet и Nile;
  - Ethereum Mainnet и Sepolia;
  - BSC Mainnet и Testnet;
  - Polygon PoS и Amoy;
  - OP Mainnet и OP Sepolia;
  - Arbitrum One и Arbitrum Sepolia;
  - Base Mainnet;
  - Robinhood Chain Mainnet и Testnet;
  - Avalanche C-Chain и Fuji;
- native/ERC20/TRC20 balances и transfers;
- Tron resources, staking, delegation и contract deployment;
- прогрессивная фоновая загрузка балансов без перезагрузки всей таблицы;
- typed settings UI для RPC, provider headers, explorer, discovery, assets,
  auto-lock и общих настроек.

Подробная архитектура и принятые решения описаны в
[docs/multichain-plan.md](docs/multichain-plan.md).

## Запуск

```bash
go run ./cmd/walletspace
```

По умолчанию UI открывается на <http://127.0.0.1:8080>. При первом запуске
приложение предлагает создать space. Если оставить имя и мнемонику пустыми,
будет создан `default` с новой 24-word recovery phrase.

Runtime-only overrides:

| Переменная                  | Назначение                                  |
| --------------------------- | ------------------------------------------- |
| `WALLETSPACE_HOME`          | изменить каталог данных                     |
| `WALLETSPACE_ADDR`          | изменить loopback listen address            |
| `WALLETSPACE_OPEN_BROWSER`  | включить или отключить открытие браузера    |

Persistent-конфигурация редактируется на странице `/settings`. Значения,
сохранённые как `${ENV_NAME}`, раскрываются только при использовании и
позволяют не записывать provider secret непосредственно в YAML.

## Данные

Все новые данные располагаются в `~/.walletspace`:

```text
~/.walletspace/
├── config.yaml
├── networks.yaml
├── assets.json
├── cache/
└── spaces/
    └── spc_.../
        ├── space.json
        └── operations.json
```

`space.json` атомарно объединяет публичные account metadata и зашифрованный
vault. Vault использует Argon2id и AES-256-GCM; mnemonic, BIP39 passphrase и
импортированные ключи в открытом виде на диск не записываются. Файлы создаются
с правами `0600`, каталоги — `0700`.

Обычный запуск не ищет и не читает старый `./data`. Legacy-данные переносятся
только явной командой:

```bash
go run ./cmd/walletspace migrate --from ./data

# только проверить mnemonic и все адреса, не создавая ~/.walletspace
go run ./cmd/walletspace migrate --from ./data --dry-run
```

Команда сначала проверяет соответствие каждого старого Tron-адреса mnemonic и
только затем атомарно создаёт новый space. Исходные файлы не изменяются и не
удаляются.

## RPC и сети

Для каждой операции `network_id` передаётся явно — глобальной mutable network
в backend нет. Кандидаты берутся из Node Discovery и объединяются с
официальными fallback. Перед использованием:

- URL фильтруется от небезопасных схем и SSRF;
- EVM RPC проверяется через `eth_chainId` и `eth_blockNumber`;
- Tron endpoint проверяется через `net_version` и актуальный блок;
- успешный endpoint сохраняется в короткоживущий last-known-good cache.

Custom RPC и provider headers задаются через `/settings`. API возвращает только
признак наличия headers, но не их значения.

## Безопасность

- API намеренно доступен только на loopback; non-loopback bind отклоняется.
- Изменяющие browser-запросы защищены проверками Origin/Sec-Fetch-Site и
  обязательным JSON Content-Type.
- Space автоматически блокируется после настраиваемого периода неактивности.
- Secret responses получают `Cache-Control: no-store`.
- Перед экспортом ключа, подписью или импортом space должен быть разблокирован.
- Recovery phrase и private key дают полный контроль над средствами — не
  сохраняйте их в мессенджеры, облачные заметки или скриншоты.

## Разработка

```bash
go test ./... -count=1 -race
go vet ./...
```

Frontend — native ES modules без обязательного build step; страницы, API,
state, features и components разнесены по отдельным модулям в
`internal/httpapi/ui`.
