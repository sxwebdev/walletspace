# Security audit Walletspace

Дата аудита: 2026-07-31
Ревизия: `22267c1`
Объект: весь сервис Walletspace — Go backend, embedded web UI, vault/storage,
EVM/Tron signing, RPC discovery и Node Doctor.

## Итог

В текущем виде сервис нельзя считать безопасным для хранения или перемещения
реальных средств. Найдены две критические уязвимости, каждая из которых может
привести к компрометации seed/private keys или подписанию операции, отличной от
той, которую запросил пользователь:

1. локальный HTTP API не имеет аутентификации и обходится через DNS rebinding;
2. Tron-транзакция строится недоверенной RPC-нодой и подписывается без проверки
   ее фактического содержимого.

Дополнительно подтверждены stored XSS через on-chain metadata, не связанное с
подписью подтверждение EVM fee, утечка provider credentials на fallback-ноды и
риск повторной Tron-операции после неопределенного результата broadcast.

| Severity | Количество | Допуск к релизу с реальными средствами |
| -------- | ---------: | -------------------------------------- |
| Critical |          2 | Блокирует                              |
| High     |          4 | Блокирует                              |
| Medium   |          5 | Исправить до публичного релиза         |

## Модель угроз

В аудит включены следующие реалистичные противники:

- произвольный сайт, открытый пользователем в обычном браузере;
- непривилегированный локальный процесс или sandboxed-приложение, которому
  доступен loopback, но недоступны файлы Walletspace;
- скомпрометированная, ошибочная или злонамеренная RPC/discovery-нода;
- злонамеренный ERC20/TRC20 контракт с контролируемыми metadata;
- атакующий, получивший копию encrypted backup или каталога данных;
- сетевой сбой в момент broadcast.

Полная компрометация учетной записи ОС и чтение памяти процесса не считаются
устранимыми средствами самого Walletspace. Legacy migration не оценивалась как
целевая функция: по условию проект стартует с чистого формата данных.

## Найденные уязвимости

### SEC-01 — Critical — локальный API не имеет границы авторизации и уязвим к DNS rebinding

**Доказательство.** Все чувствительные маршруты, включая unlock, seed/private
key export и отправку средств, зарегистрированы без authentication middleware в
`internal/httpapi/platform.go:80-126`. Единственная защита находится в
`internal/httpapi/server.go:166-201`:

- пустой `Sec-Fetch-Site` разрешен (`:168-173`);
- `Origin` сравнивается только с полученным от клиента `Host` (`:175-181`);
- сам `Host` не сверяется с фактическим loopback listen address;
- capability/session token отсутствует.

Bind на loopback проверяется в `internal/config/home.go:274-285`, но loopback
ограничивает сетевой маршрут, а не полномочия вызывающего кода.

**Эксплуатация.** Атакующий отдает страницу с контролируемого домена и порта
`8080`, затем меняет DNS-ответ домена на `127.0.0.1`. Последующий browser fetch
остается same-origin: `Origin` и `Host` равны домену атакующего, а
`Sec-Fetch-Site` имеет значение `same-origin`. Guard пропускает запрос к
Walletspace. Возможные действия:

- получить mnemonic (`platform.go:235-243`) и private key (`:364-380`) из уже
  разблокированного space;
- подписать перевод, staking/delegation или deploy;
- скачать encrypted backup и атаковать пароль offline;
- менять RPC/discovery settings;
- создавать spaces и нагружать CPU/диск.

Обычный локальный процесс может вызывать API еще проще: отсутствие browser
headers намеренно разрешено и закреплено тестом
`internal/httpapi/guard_test.go:48-56`.

**Что исправить.** На чистом формате рекомендуется не считать loopback
authentication boundary:

1. Генерировать при каждом запуске случайный capability token не менее 256 бит.
   Передавать его UI через URL fragment, хранить только в памяти вкладки и
   требовать, например, в `X-Walletspace-Token` на **всех** `/api/*`, включая
   GET и streaming endpoints.
2. Слушать случайный loopback port; проверять `Host` по точному allowlist
   фактически открытых IP/port. Не доверять произвольному Host header.
3. Сравнивать полный origin: scheme, hostname и port. Fetch Metadata и JSON
   Content-Type оставить как дополнительную CSRF-защиту, а не как
   аутентификацию.
4. Более сильный и удобный вариант — native shell/webview с IPC или Unix socket,
   а HTTP оставить только адаптером с capability authentication.

**Критерий приемки.** Запросы с отсутствующим/неверным token и с
`Host: attacker.example:8080` отклоняются до routing независимо от
`Origin`/`Sec-Fetch-Site`. Это покрыто интеграционным DNS-rebinding тестом.

### SEC-02 — Critical — Walletspace подписывает Tron-транзакцию, которую целиком вернула недоверенная нода

**Доказательство.** Запрос пользователя валидируется локально, но итоговый
protobuf transaction создается RPC-нодой:

- TRX/TRC20 transfer: `internal/tron/service.go:824-874` и `:159-185`;
- staking/delegation: `internal/tron/staking.go:611-735`, затем `:764-785`;
- deploy: `internal/tron/contract.go:189-219`.

После этого `submitWithSigner` сериализует `tx.Transaction.RawData`, вычисляет
digest и подписывает его без сравнения с исходным intent
(`internal/tron/service.go:803-821`). Проверка endpoint подтверждает только
заявленный `net_version` (`internal/chain/tron/adapter.go:117-166`), который
злонамеренный сервер может вернуть без участия в настоящей сети.

**Эксплуатация.** Выбранная через custom RPC или discovery нода получает запрос
«перевести 1 TRX адресу A», но возвращает unsigned transaction «перевести весь
доступный TRX адресу атакующего». Локальный signer видит только 32-byte digest и
подписывает подмененный raw data. Аналогично можно заменить TRC20 contract/data,
receiver delegation, amount, fee limit или содержимое deploy.

**Что исправить.** Приватный ключ должен подписывать только локально
сформированный и проверенный intent:

- предпочтительно собирать Tron contract/raw data локально; от RPC получать
  только head/reference block data;
- если node-assisted build временно остается, непосредственно перед подписью
  декодировать transaction и строго проверить: ровно один contract, его type,
  owner, recipient, amount, token contract/calldata, resource, fee limit,
  permission id, expiration/reference block и отсутствие дополнительных
  действий;
- signer API должен принимать typed intent или уже проверенный canonical
  transaction, а не произвольный digest от RPC;
- вычислять txid локально из того же canonical `raw_data`.

Проверка нужна отдельно для каждого вида Tron-операции. Проверка только owner
address или chain identity проблему не решает.

**Критерий приемки.** Fake RPC подменяет по одному полю и добавляет второй
contract; во всех случаях запрос отклоняется и `SignDigest` не вызывается.

### SEC-03 — High — stored DOM XSS через on-chain symbol токена

**Доказательство.** ERC20 symbol читается как произвольная строка из контракта
(`internal/chain/evm/adapter.go:254-297`), затем сохраняется без ограничения
формата (`internal/httpapi/platform.go:717-757`,
`internal/asset/store.go:91-110`). В dashboard это значение трижды вставляется
без `escapeHTML`:

`internal/httpapi/ui/views/dashboard.js:251-255`

Полученная строка разбирается как HTML через `createContextualFragment` в
`dashboard.js:290-295`. CSP отсутствует (`internal/httpapi/ui/index.html:1-23`),
общий middleware также не выставляет ее.

**Эксплуатация.** Контракт возвращает symbol вида
`</span><img src=x onerror="...">`. Пользователь добавляет адрес такого токена,
после чего payload выполняется в origin Walletspace. Скрипт может вызывать API,
экспортировать seed/keys и подписывать операции, особенно пока space unlocked.
Эта атака не требует взлома RPC — metadata контролирует сам token contract.

**Что исправить.** Не строить DOM из строк для недоверенных данных: создавать
узлы и задавать `textContent`. Дополнительно валидировать/ограничить server-side
symbol и name (разумная длина, printable Unicode), но validation не заменяет
context-aware escaping. Добавить строгую CSP как второй барьер:

`default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'`

Также нужны `X-Content-Type-Options: nosniff` и `Referrer-Policy: no-referrer`.

**Критерий приемки.** Symbol с HTML/SVG/event handler отображается буквальным
текстом; CSP запрещает inline handler; browser-тест подтверждает отсутствие
сетевого запроса payload.

### SEC-04 — High — подтвержденная EVM fee не связана с подписываемой транзакцией

**Доказательство.** Estimate получает gas/fees в
`internal/chain/evm/adapter.go:300-327`. После подтверждения UI вызывает другой
endpoint, а `Send` повторно запрашивает `EstimateGas`, nonce и fee suggestions
(`adapter.go:330-399`). Никакой approved fee/gas или hash подготовленной
транзакции в request/idempotency record нет. `suggestFees` принимает значения
RPC без верхней границы (`adapter.go:559-571`). UI показывает результат первого
estimate (`internal/httpapi/ui/views/dashboard.js:675-694`).

**Эксплуатация.** Ошибочная или злонамеренная RPC сначала возвращает нормальную
комиссию, а на `Send` — огромный priority fee/gas price. Пользователь видит и
подтверждает одно значение, но подписывает другое. Высокий tip может привести к
существенной или полной потере native balance.

**Что исправить.** Ввести поток `prepare -> approve -> sign exact bytes`:

1. Backend строит canonical unsigned transaction и возвращает все поля плюс
   одноразовый `intent_id`/hash.
2. UI показывает полный intent.
3. Sign endpoint подписывает именно сохраненный immutable transaction либо
   принимает жесткие user-approved maxima (`max_total_fee`, gas limit, fee cap,
   tip cap, nonce).
4. Любое изменение требует нового подтверждения. Добавить absolute/relative fee
   policy и отказ при аномальном ответе RPC.

**Критерий приемки.** Между prepare и sign RPC меняет fee в 100 раз; backend не
подписывает новую транзакцию и требует повторного подтверждения.

### SEC-05 — High — provider credentials отправляются посторонним fallback/discovery endpoints

**Доказательство.** Resolver объединяет custom RPC с официальными fallback
(`internal/rpcpool/resolver.go:81-93`), но headers хранятся на уровне всей сети,
а не endpoint (`resolver.go:164-177`). EVM adapter применяет одни и те же headers
к каждому кандидату (`internal/chain/evm/adapter.go:478-490`). Node Doctor делает
то же для EVM и Tron (`cmd/walletspace/main.go:109-124`).

Даже без custom URL можно сохранить headers и включить discovery, после чего
секрет уйдет всем найденным и fallback-нодам. При наличии custom URL достаточно
его временной недоступности, чтобы EVM перешел к следующему endpoint.

**Влияние.** `Authorization`, API key или bearer token попадает провайдеру, для
которого он не предназначался. Это может раскрыть account access, платную квоту
или credentials другого сервиса.

**Что исправить.** Перейти от network-wide полей к списку endpoint records:

```text
endpoint = { url, credential_ref, allowed_header_names, trust_level }
```

Credential разрешено прикладывать только при точном совпадении
scheme/hostname/port с записью. Fallback и discovery всегда стартуют без
credentials, пока пользователь явно не привязал отдельный secret к конкретному
origin. Secrets хранить через OS keychain/secret store либо как env reference.

**Критерий приемки.** Mock custom endpoint падает; fallback получает запрос без
`Authorization`/provider headers. Тот же тест нужен для Doctor.

### SEC-06 — High — неопределенный Tron broadcast превращается в рекомендацию создать новую транзакцию

**Доказательство.** `submitWithSigner` уже вычисляет digest локально, но при
любой ошибке `BroadcastTransaction` возвращает пустой txid
(`internal/tron/service.go:803-821`). HTTP layer помечает операцию failed, если
txid пуст (`internal/httpapi/platform.go:884-900` для transfer и аналогично
`:1079-1089` для staking/delegation). Для failed operation API прямо предлагает
повторить с новым idempotency key (`platform.go:1244-1252`), а UI сбрасывает key
после ошибки (`internal/httpapi/ui/views/dashboard.js:698-701`).

**Эксплуатация.** Нода приняла первую транзакцию, но ответ потерялся из-за
timeout/reset. Пользователь повторяет действие. Backend строит новый Tron raw
data с другим reference block/timestamp и подписывает вторую самостоятельную
операцию. Обе могут быть исполнены.

**Что исправить.** До сетевого вызова локально вычислить txid и durable-сохранить
signed bytes/status `broadcasting`. Transport error после отправки — это
`broadcast_unknown`, не `failed`. Затем:

- проверять txid через независимые endpoints;
- при необходимости повторно broadcast **тех же signed bytes**, что безопасно
  из-за того же txid;
- не разрешать новый build для того же business intent, пока исходный tx не
  найден, не истек или пользователь явно не подтвердил replacement.

**Критерий приемки.** Fake broadcaster принимает tx и обрывает ответ. Повторный
запрос не вызывает build/sign второй раз, а возвращает исходный локальный txid и
status `broadcast_unknown`.

### SEC-07 — Medium — env-resolved RPC secrets сохраняются в cache и попадают в ошибки

**Доказательство.** `${ENV}` в RPC URL раскрывается в runtime
(`internal/rpcpool/resolver.go:83-91`). Успешный endpoint передается в
`MarkHealthy` и целиком записывается в `cache/rpc-nodes.json`
(`resolver.go:124-143`; вызовы — `internal/chain/evm/adapter.go:522-525` и
`internal/chain/tron/adapter.go:612-618`). Если API token находится в path/query,
его раскрытое значение материализуется на диске, хотя исходный YAML содержал
только env reference.

При ручной проверке RPC полный раскрытый endpoint включается в ошибку
`internal/httpapi/platform.go:685-702`, после чего ошибка возвращается UI и при
части сценариев пишется в log (`platform.go:1477-1505`).

**Что исправить.** Cache должен хранить opaque endpoint ID либо URL без
userinfo/query/secret path. Не кешировать custom secret URLs вообще. Для ошибок
и логов использовать единый redactor, удаляющий userinfo, query и secret path
segments. Credentials отделить от URL.

### SEC-08 — Medium — unlock/create допускают online guessing и resource exhaustion

**Доказательство.** Rate limit, failed-attempt backoff и общий лимит дорогих KDF
операций отсутствуют. Один unlock использует Argon2id с 64 MiB памяти и тремя
проходами (`internal/vault/vault.go:54`, `:138-147`), причем глобальный mutex
Manager удерживается во время KDF (`internal/space/manager.go:422-443`). Это
блокирует операции всех spaces. Create также выполняет KDF дважды
(`manager.go:250-314`) и доступен через неаутентифицированный API.

HTTP server задает только `ReadHeaderTimeout`; отсутствуют `ReadTimeout`,
`WriteTimeout`, `IdleTimeout`, явный `MaxHeaderBytes` и connection/concurrency
budget (`cmd/walletspace/main.go:148`).

**Влияние.** Browser через SEC-01 или локальный sandboxed process может
последовательно угадывать пароль, постоянно занимать CPU/KDF mutex, удерживать
connections и создавать данные до заполнения диска.

**Что исправить.** После capability authentication добавить per-space
exponential cooldown с jitter, глобальный KDF semaphore, per-space locks вместо
одного глобального lock, quotas для spaces/assets/operations и server timeouts.
Ошибки unlock должны оставаться неразличимыми. Состояние rate limit не должно
позволять обойти его перезапуском без заметного user action.

### SEC-09 — Medium — экран подтверждения скрывает большую часть адреса и backend не проверяет EIP-55

**Доказательство.** `shortAddress` оставляет 9 первых и 7 последних символов
(`internal/httpapi/ui/components/ui.js:80-81`). Именно сокращенный recipient
показывается перед подписью (`internal/httpapi/ui/views/dashboard.js:681-684`).
EVM backend использует только `common.IsHexAddress`
(`internal/chain/evm/adapter.go:424-430`), поэтому mixed-case адрес с неверной
EIP-55 checksum не отклоняется.

**Влияние.** Address-poisoning/clipboard malware может подобрать похожие начало
и конец; пользователь не имеет возможности проверить отличающуюся середину на
финальном экране. Неверный mixed-case checksum не останавливает опечатку.

**Что исправить.** На confirmation показывать полный recipient, сеть, chain ID,
asset contract, amount и максимальную комиссию; визуально группировать, но не
скрывать символы. Для EVM принимать полностью lowercase адрес либо адрес с
валидной EIP-55 checksum; mixed-case с неверной checksum отклонять.

### SEC-10 — Medium — discovery response позволяет создать неограниченный fan-out goroutines

**Доказательство.** Размер discovery JSON ограничен 2 MiB, но количество URL и
длина списка не ограничены (`internal/rpcpool/resolver.go:238-260`). Parser
рекурсивно извлекает строки из произвольной структуры (`resolver.go:323-346`).
Doctor создает по goroutine на каждый endpoint и только внутри goroutine ждет
общий limiter (`internal/doctor/doctor.go:201-230`).

**Эксплуатация.** Скомпрометированный discovery service возвращает десятки
тысяч коротких URL. Каждая минутная проверка выделяет большие slices и запускает
тысячи goroutines на сеть, вызывая memory/CPU exhaustion.

**Что исправить.** Использовать строгую versioned schema; принять не более
8–16 endpoints на сеть, ограничить длину URL и количество JSON nodes до
сортировки/DNS lookup. Doctor должен применять фиксированный worker pool, не
создавая goroutine до получения slot.

### SEC-11 — Medium — export секретов не требует step-up authorization, а unlocked session может быть бессрочной

**Доказательство.** Mnemonic/private key export требуют только уже unlocked
space и не принимают текущий пароль или одноразовое подтверждение
(`internal/httpapi/platform.go:235-243`, `:364-380`; UI:
`internal/httpapi/ui/features/accounts/dialogs.js:98-124`). Auto-lock можно
установить в `0`, что полностью отключает expiration
(`internal/space/manager.go:935-944`). Минимальная длина vault password — всего
8 bytes (`manager.go:27-30`, `:520-527`).

**Влияние.** Короткий доступ к разблокированной вкладке, same-origin XSS или
локальный API client сразу получает долгоживущие master secrets. Копия backup с
человеческим восьмисимвольным паролем допускает offline guessing.

**Что исправить.** Для mnemonic/private-key export требовать повторный пароль
или короткоживущий одноразовый step-up grant с явным user gesture. Для отправки
средств — отдельное подтверждение exact intent. Не разрешать полностью отключать
auto-lock в production profile; показывать риск и установить безопасный верхний
предел. Вместо одной проверки длины применять оценку слабых/скомпрометированных
паролей и рекомендовать password manager.

## Рекомендуемая архитектура подписи

С учетом старта с чистого листа стоит унифицировать EVM и Tron вокруг одного
неизменяемого signing pipeline:

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

RPC никогда не должен выбирать получателя, amount, contract calldata или fee
после пользовательского подтверждения. RPC может предоставить chain state
(nonce, base fee, head/reference block), но эти значения должны пройти policy и
стать частью immutable prepared transaction.

## Порядок исправления

### P0 — до любых реальных средств

1. SEC-01: capability-authenticated local transport + строгий Host/origin.
2. SEC-02: локальная сборка/полная semantic verification Tron transaction.
3. SEC-03: устранить HTML sinks для on-chain data и включить CSP.
4. SEC-04: связать UI approval с exact EVM transaction и fee maxima.

### P1 — до публичного тестирования

5. SEC-05: endpoint-scoped credentials.
6. SEC-06: durable `broadcast_unknown` и повтор exact signed transaction.
7. SEC-08: rate limits, KDF/connections/quotas.
8. SEC-11: step-up для seed/private-key export.

### P2 — hardening перед release

9. SEC-07: secret-safe cache/error/log model.
10. SEC-09: полный адрес и checksum policy.
11. SEC-10: строгая discovery schema и bounded workers.

## Что уже сделано правильно

Следующие механизмы проверены и не являются findings:

- vault использует Argon2id и AES-256-GCM с random salt/nonce и AAD;
- параметры KDF/ciphertext ограничены перед выделением памяти
  (`internal/vault/vault.go:114-147`);
- файлы пишутся атомарно с `0600`, каталоги приводятся к `0700`
  (`internal/storage/storage.go:28-84`);
- private RPC dialer блокирует loopback/private/special IP и повторно проверяет
  DNS при соединении; redirects отключены (`internal/rpcpool/resolver.go:180-211`);
- EVM chain ID и Tron declared network identity проверяются перед выбором
  endpoint;
- write bodies ограничены, JSON decoder запрещает неизвестные поля;
- secret responses получают `Cache-Control: no-store`;
- provider header values не возвращаются settings API;
- EVM transaction hash вычисляется локально до broadcast и уже имеет состояние
  `broadcast_unknown` при ошибке отправки.

Эти меры полезны, но не компенсируют findings выше.

## Проверки и ограничения аудита

Выполнено:

- ручной review всех production packages и embedded UI;
- прослеживание trust boundaries от HTTP request до signer/broadcast;
- `go test ./... -count=1` — успешно вне sandbox (тестам нужен loopback bind);
- `go vet ./...` — успешно;
- `govulncheck -show verbose ./...` на 2026-07-31 — достижимых уязвимых
  символов не найдено.

`govulncheck` дополнительно сообщил о двух недостижимых advisories:

- [GO-2026-5158](https://pkg.go.dev/vuln/GO-2026-5158) для импортируемого
  `go.opentelemetry.io/otel@v1.43.0`, исправлено в `v1.44.0`, уязвимый symbol из
  Walletspace не вызывается;
- [GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932) для неиспользуемого здесь
  `golang.org/x/crypto/openpgp`; уязвимый package/symbol не вызывается.

Не выполнялись live-операции с реальными сетями и fuzzing внешних RPC responses.
После P0/P1 нужен повторный security review и adversarial integration suite с
fake browser origin, fake discovery и fake Tron/EVM RPC.
