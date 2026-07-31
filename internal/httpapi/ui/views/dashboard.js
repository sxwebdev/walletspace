import { listSpaces, lockSpace } from "../api/spaces.js";
import { bindAccountNetwork, listAccounts } from "../api/accounts.js";
import { doctorHealth, listNetworks, streamBalances } from "../api/networks.js";
import { getSettings, listAssets } from "../api/settings.js";
import { listPrices } from "../api/prices.js";
import { estimateTransfer, sendTransfer, transactionStatus } from "../api/transfers.js";
import { deployContract, estimateDeploy, resources, stakingOperation } from "../api/tron.js";
import {
  showDerive as openDerive,
  showExport as openExport,
  showImport as openImport,
  showRename as openAccountRename,
} from "../features/accounts/dialogs.js";
import { renderOnboarding } from "../features/onboarding/onboarding.js";
import {
  backupSpace,
  showChangePassword,
  showCreateSpace as openCreateSpace,
  showMnemonic,
  showRenameSpace as openSpaceRename,
  showUnlock as openUnlock,
} from "../features/spaces/dialogs.js";
import { navigate } from "../router.js";
import {
  balanceKey,
  currentSpace,
  state,
  update,
} from "../state/store.js";
import { escapeHTML, modal, setBusy, shortAddress, toast } from "../components/ui.js";

let routeSignal;
let balanceController;
let doctorTimer;
let lastDoctorSnapshot;
let menuDismissalBound = false;
let priceGeneration = 0;
const targetedBalanceControllers = new Set();

export async function render(root, signal) {
  routeSignal = signal;
  try {
    const [spaces, networks, settings] = await Promise.all([
      listSpaces(signal), listNetworks(signal), getSettings(signal),
    ]);
    if (!spaces.length) {
      renderOnboarding(root, signal);
      return cleanup;
    }
    const preferredSpace = state.currentSpaceID || settings.ui.last_space_id;
    const spaceID = spaces.some((item) => item.id === preferredSpace)
      ? preferredSpace
      : spaces[0].id;
    const availableNetworks = networks.filter((item) => item.enabled);
    const accountFilter = state.accountFilter === "all" || state.accountFilter === "unassigned" ||
      networks.some((item) => item.id === state.accountFilter)
      ? state.accountFilter
      : "all";
    update({
      spaces, networks, currentSpaceID: spaceID, accountFilter,
    });
    if (!availableNetworks.length) {
      root.innerHTML = `<div class="boot">
        <strong>Нет включённых сетей</strong>
        <span>Включите хотя бы одну сеть на странице настроек.</span>
        <button class="button primary" type="button" data-open-settings>Открыть настройки</button>
      </div>`;
      root.querySelector("[data-open-settings]").addEventListener("click", () => navigate("/settings"));
      return cleanup;
    }
    const [accounts, assetGroups] = await Promise.all([
      listAccounts(spaceID, signal),
      Promise.all(availableNetworks.map((network) => listAssets(network.id, signal))),
    ]);
    update({
      accounts, assets: assetGroups.flat(), balances: new Map(), balancesLoading: true,
      balanceFailures: 0,
      prices: new Map(), pricesLoading: true, pricesStale: false, pricesError: "",
    });
    renderShell(root);
    bindShell(root);
    startBalances();
    doctorTimer = window.setInterval(refreshNetworkHealth, 15000);
  } catch (cause) {
    if (cause.name !== "AbortError") {
      root.innerHTML = `<div class="boot"><strong>Walletspace не загрузился</strong><span>${escapeHTML(cause.message)}</span></div>`;
    }
  }
  return cleanup;
}

function cleanup() {
  priceGeneration += 1;
  balanceController?.abort();
  balanceController = null;
  for (const controller of targetedBalanceControllers) controller.abort();
  targetedBalanceControllers.clear();
  window.clearInterval(doctorTimer);
  doctorTimer = null;
  lastDoctorSnapshot = null;
}

function renderShell(root) {
  const space = currentSpace();
  root.innerHTML = `
    <div class="shell">
      <header class="topbar">
        <a class="brand" href="/" data-home>
          <span class="brand-mark" aria-hidden="true">W</span>
          <span class="brand-copy"><strong>Walletspace</strong><span>Local multichain vault</span></span>
        </a>
        <div class="toolbar">
          <label class="sr-only" for="space-select">Space</label>
          <select class="control" id="space-select">${spaceOptions()}</select>
          <button class="button icon" type="button" data-new-space title="Новый space">＋</button>
          <button class="button icon" type="button" data-lock title="${space.locked ? "Разблокировать" : "Заблокировать"}">${space.locked ? "🔒" : "🔓"}</button>
          <div class="menu" data-space-menu>
            <button class="button icon" type="button" data-menu-toggle aria-label="Действия space" aria-haspopup="menu" aria-expanded="false">•••</button>
            <div class="menu-popover">
              <button type="button" data-space-action="rename">Переименовать space</button>
              <button type="button" data-space-action="mnemonic">Показать recovery phrase</button>
              <button type="button" data-space-action="password">Сменить пароль</button>
              <button type="button" data-space-action="backup">Скачать encrypted backup</button>
            </div>
          </div>
        </div>
        <button class="button icon" type="button" data-settings title="Настройки">⚙</button>
      </header>
      <main class="page">
        <section class="page-heading">
          <div>
            <p class="eyebrow">Secure Space · все сети</p>
            <h1>${escapeHTML(space.name)}</h1>
            <p class="muted">${space.locked ? "Read-only · разблокируйте space для подписи и ключей" : "Vault разблокирован локально"}</p>
          </div>
          <div class="toolbar">
            <button class="button" type="button" data-refresh>Обновить</button>
            <div class="menu" data-create-menu>
              <button class="button primary" type="button" data-menu-toggle aria-haspopup="menu" aria-expanded="false">Создать ▾</button>
              <div class="menu-popover">
                <button type="button" data-derive>Новый derived account</button>
                <button type="button" data-import>Импортировать private key</button>
              </div>
            </div>
          </div>
        </section>
        <section class="summary-grid">
          <article class="summary-card"><span>Общий баланс · mainnet</span><strong data-total-usd>Считаем…</strong><small class="muted" data-market-change>Загружаем балансы и USD-котировки</small></article>
          <article class="summary-card"><span>Активы с балансом</span><strong data-assets-with-balance>—</strong><small class="muted" data-unpriced-assets>без цены: —</small></article>
          <article class="summary-card"><span>Node doctor</span><strong data-rpc-status>Checking…</strong><small class="muted" data-rpc-detail>Проверяем все сети и RPC-ноды</small><button class="doctor-details" type="button" data-doctor-details>Детали</button></article>
        </section>
        <section class="panel">
          <header class="panel-header">
            <div><h2>Кошельки</h2><span class="muted">Space: ${escapeHTML(space.name)} · балансы сразу во всех подключённых сетях</span></div>
            <label class="filter-control">
              <span class="sr-only">Фильтр сети</span>
              <select class="control" data-account-filter>${accountFilterOptions()}</select>
            </label>
          </header>
          <div class="account-list" data-account-list>${accountCards()}</div>
        </section>
      </main>
    </div>`;
  queueMicrotask(refreshNetworkHealth);
  queueMicrotask(renderPortfolioSummary);
}

function spaceOptions() {
  return state.spaces
    .map((item) => `<option value="${item.id}" ${item.id === state.currentSpaceID ? "selected" : ""}>${escapeHTML(item.name)}${item.locked ? " · locked" : ""}</option>`)
    .join("");
}

function enabledNetworks() {
  return state.networks.filter((item) => item.enabled);
}

function assetsFor(network) {
  return state.assets.filter((item) => item.network_id === network.id);
}

function accountFilterOptions() {
  return [
    `<option value="all" ${state.accountFilter === "all" ? "selected" : ""}>Все сети</option>`,
    `<option value="unassigned" ${state.accountFilter === "unassigned" ? "selected" : ""}>Нужно назначить сеть</option>`,
    ...state.networks.map((item) =>
      `<option value="${item.id}" ${state.accountFilter === item.id ? "selected" : ""}>${escapeHTML(item.name)}${item.enabled ? "" : " · disabled"}</option>`),
  ].join("");
}

function accountNetworks(account) {
  return account.network_ids || [];
}

function accountBoundTo(account, networkID) {
  return accountNetworks(account).includes(networkID);
}

function nextDerivationIndex(networkID) {
  const used = new Set();
  for (const account of state.accounts) {
    if (account.kind !== "derived" || !accountBoundTo(account, networkID) ||
        !Number.isInteger(account.index)) continue;
    used.add(account.index);
  }
  let next = 0;
  while (used.has(next)) next += 1;
  return next;
}

function filteredAccounts() {
  if (state.accountFilter === "all") return state.accounts;
  if (state.accountFilter === "unassigned") {
    return state.accounts.filter((account) => !accountNetworks(account).length);
  }
  return state.accounts.filter((account) => accountBoundTo(account, state.accountFilter));
}

function upsertAccount(updated) {
  const index = state.accounts.findIndex((item) => item.id === updated.id);
  if (index === -1) state.accounts.push(updated);
  else state.accounts[index] = updated;
}

function accountCards() {
  const accounts = filteredAccounts();
  if (!accounts.length) {
    return `<div class="empty">${state.accounts.length
      ? "В этом фильтре пока нет кошельков."
      : "Кошельков пока нет. Нажмите «Создать» и выберите сеть в модальном окне."}</div>`;
  }
  return accounts.map((account) => {
    const connectable = connectableNetworks(account);
    return `
      <article class="account-card" data-account="${account.id}">
        <div class="account-identity">
          <div class="account-title">
            <strong>${escapeHTML(account.label || `Account ${account.index ?? ""}`)}</strong>
            <span class="badge ${account.kind === "imported" ? "imported" : ""}" title="${account.kind === "imported" ? "Не восстанавливается из мнемоники space" : "Восстанавливается из мнемоники space"}">${account.kind === "imported" ? "Импортирован" : "Derived"}</span>
            <span class="badge">Space · ${escapeHTML(currentSpace().name)}</span>
          </div>
        </div>
        <div class="menu">
          <button class="button icon" type="button" data-account-menu aria-label="Действия" aria-haspopup="menu" aria-expanded="false">•••</button>
          <div class="menu-popover">
            ${connectable.length ? '<button type="button" data-action="bind">Подключить ещё одну сеть</button>' : ""}
            <button type="button" data-action="rename">Переименовать</button>
            <button type="button" data-action="export">Экспорт private key</button>
          </div>
        </div>
        <div class="account-networks">${accountNetworkRows(account)}</div>
      </article>`;
  }).join("");
}

function connectableNetworks(account) {
  return enabledNetworks().filter((network) => !accountBoundTo(account, network.id) &&
    (account.kind === "imported" || !account.family || account.family === network.family));
}

function accountNetworkRows(account) {
  const rows = accountNetworks(account).map((networkID) => {
    const network = state.networks.find((item) => item.id === networkID);
    if (!network) {
      return `<div class="wallet-network"><div><strong>${escapeHTML(networkID)}</strong><small class="muted">Сеть отсутствует в конфигурации</small></div></div>`;
    }
    const address = account.addresses[network.family] || "";
    const balances = network.enabled ? visibleAssets(account, network).map((asset) => {
      const item = state.balances.get(balanceKey(state.currentSpaceID, network.id, account.id, asset.id));
      const symbol = escapeHTML(asset.symbol);
      if (!item) return `<div class="balance"><div class="skeleton"></div><span>${symbol}</span></div>`;
      if (item.error) return `<div class="balance"><strong>—</strong><span title="${escapeHTML(item.error)}">${symbol} · ошибка</span></div>`;
      return `<div class="balance ${item.stale ? "stale" : ""}"><strong>${escapeHTML(item.amount || "0")}</strong><span>${symbol}${item.stale ? " · cached" : ""}</span></div>`;
    }).join("") : '<span class="muted">Сеть отключена в настройках</span>';
    return `<section class="wallet-network" data-network="${escapeHTML(network.id)}">
      <div class="wallet-network-identity">
        <div class="network-badges"><strong>${escapeHTML(network.name)}</strong>${network.testnet ? '<span class="badge testnet">TESTNET</span>' : ""}${network.enabled ? "" : '<span class="badge danger">OFF</span>'}</div>
        <button class="address" type="button" data-copy="${escapeHTML(address)}" title="Копировать адрес">${escapeHTML(shortAddress(address))}</button>
      </div>
      <div class="wallet-balances">${balances}</div>
      <div class="wallet-actions">
        ${network.enabled ? `<button class="button" type="button" data-action="send" data-network="${escapeHTML(network.id)}">Отправить</button>` : ""}
        ${network.enabled && network.family === "tron" ? `<button class="button icon" type="button" data-action="resources" data-network="${escapeHTML(network.id)}" title="Resources & staking">R</button><button class="button icon" type="button" data-action="deploy" data-network="${escapeHTML(network.id)}" title="Deploy contract">D</button>` : ""}
      </div>
    </section>`;
  });
  return rows.length ? rows.join("") : '<div class="notice danger">Сеть не назначена. Подключите сеть через меню wallet.</div>';
}

function visibleAssets(account, network) {
  return assetsFor(network).filter((asset) => {
    if (asset.kind === "native") return true;
    const balance = state.balances.get(
      balanceKey(state.currentSpaceID, network.id, account.id, asset.id),
    );
    return balance?.error || !isZeroAmount(balance?.amount);
  });
}

function isZeroAmount(amount) {
  return amount === undefined || /^0(?:\.0+)?$/.test(String(amount).trim());
}

function renderAccounts() {
  document.querySelector("[data-account-list]")?.replaceChildren(
    document.createRange().createContextualFragment(accountCards()),
  );
  bindAccountActions();
  renderPortfolioSummary();
}

function renderPortfolioSummary() {
  const totalElement = document.querySelector("[data-total-usd]");
  const changeElement = document.querySelector("[data-market-change]");
  const assetsElement = document.querySelector("[data-assets-with-balance]");
  const unpricedElement = document.querySelector("[data-unpriced-assets]");
  if (!totalElement || !changeElement || !assetsElement || !unpricedElement) return;

  if (state.balancesLoading) {
    totalElement.textContent = "Считаем…";
    changeElement.textContent = "Загружаем mainnet-балансы";
    assetsElement.textContent = "—";
    unpricedElement.textContent = "без цены: —";
    return;
  }

  const portfolio = calculatePortfolio();
  const failedNetworks = state.balanceFailures || 0;
  const failureSuffix = failedNetworks ? ` · ошибок сетей: ${failedNetworks}` : "";
  assetsElement.textContent = String(portfolio.assets.size);
  unpricedElement.textContent = `без цены: ${portfolio.unpriced.size}${failureSuffix}`;
  if (!portfolio.assets.size) {
    if (failedNetworks) {
      totalElement.textContent = "—";
      changeElement.textContent = `Не удалось загрузить балансы в ${failedNetworks} сетях`;
      assetsElement.textContent = "—";
      return;
    }
    totalElement.textContent = formatUSD(0);
    changeElement.textContent = "Нет mainnet-активов с балансом";
    return;
  }
  if (state.pricesLoading) {
    totalElement.textContent = "Считаем…";
    changeElement.textContent = "Загружаем USD-котировки";
    return;
  }
  if (!portfolio.priced.size) {
    totalElement.textContent = "—";
    changeElement.textContent = state.pricesError || "Для активов не найдены котировки";
    return;
  }

  totalElement.textContent = `${portfolio.unpriced.size || failedNetworks ? "≈ " : ""}${formatUSD(portfolio.current)}`;
  const cached = state.pricesStale ? " · cached quotes" : "";
  if (!portfolio.historyComplete || portfolio.previous <= 0) {
    changeElement.textContent = `24ч: недостаточно исторических котировок${cached}${failureSuffix}`;
    return;
  }
  const delta = portfolio.current - portfolio.previous;
  const percent = delta / portfolio.previous * 100;
  const sign = delta > 0 ? "+" : delta < 0 ? "−" : "";
  changeElement.textContent = `Изменение цен: ${sign}${formatUSD(Math.abs(delta))} · ${sign}${Math.abs(percent).toFixed(2)}% за 24ч${cached}${failureSuffix}`;
}

function calculatePortfolio() {
  const mainnetIDs = new Set(enabledNetworks().filter((network) => !network.testnet).map((network) => network.id));
  const result = {
    assets: new Set(), priced: new Set(), unpriced: new Set(),
    current: 0, previous: 0, historyComplete: true,
  };
  for (const holding of mainnetHoldings(mainnetIDs)) {
    result.assets.add(holding.asset.id);
    const quote = state.prices.get(holding.asset.id);
    const currentPrice = Number(quote?.current_usd);
    if (!quote || !Number.isFinite(currentPrice)) {
      result.unpriced.add(holding.asset.id);
      continue;
    }
    result.priced.add(holding.asset.id);
    result.current += holding.amount * currentPrice;
    const previousPrice = Number(quote.previous_24h_usd);
    if (!quote.has_previous_24h || !Number.isFinite(previousPrice)) {
      result.historyComplete = false;
      continue;
    }
    result.previous += holding.amount * previousPrice;
  }
  return result;
}

function mainnetHoldings(mainnetIDs = new Set(
  enabledNetworks().filter((network) => !network.testnet).map((network) => network.id),
)) {
  const holdings = [];
  for (const account of state.accounts) {
    for (const networkID of accountNetworks(account)) {
      if (!mainnetIDs.has(networkID)) continue;
      const network = state.networks.find((item) => item.id === networkID);
      for (const asset of assetsFor(network)) {
        const balance = state.balances.get(balanceKey(state.currentSpaceID, networkID, account.id, asset.id));
        if (!balance || balance.error || isZeroAmount(balance.amount)) continue;
        const amount = Number(balance.amount);
        if (Number.isFinite(amount) && amount > 0) holdings.push({ asset, amount });
      }
    }
  }
  return holdings;
}

function formatUSD(value) {
  return new Intl.NumberFormat("en-US", {
    style: "currency", currency: "USD", minimumFractionDigits: 2, maximumFractionDigits: 2,
  }).format(value);
}

async function loadPrices() {
  const generation = priceGeneration + 1;
  priceGeneration = generation;
  const assetIDs = [...new Set(mainnetHoldings().map((holding) => holding.asset.id))];
  if (!assetIDs.length) {
    update({ prices: new Map(), pricesLoading: false, pricesStale: false, pricesError: "" });
    renderPortfolioSummary();
    return;
  }
  update({ pricesLoading: true, pricesError: "" });
  renderPortfolioSummary();
  try {
    const result = await listPrices(assetIDs, routeSignal);
    if (generation !== priceGeneration) return;
    update({
      prices: new Map(result.quotes.map((quote) => [quote.asset_id, quote])),
      pricesLoading: false, pricesStale: Boolean(result.stale), pricesError: "",
    });
  } catch (cause) {
    if (cause.name === "AbortError") return;
    if (generation !== priceGeneration) return;
    update({
      prices: new Map(), pricesLoading: false, pricesStale: false,
      pricesError: "Котировки временно недоступны",
    });
  }
  renderPortfolioSummary();
}

function bindShell(root) {
  bindMenuDismissal();
  root.querySelector("[data-settings]").addEventListener("click", () => navigate("/settings"));
  root.querySelector("[data-doctor-details]").addEventListener("click", showDoctorDetails);
  root.querySelector("#space-select").addEventListener("change", switchSpace);
  root.querySelector("[data-account-filter]").addEventListener("change", (event) => {
    update({ accountFilter: event.target.value });
    renderAccounts();
  });
  root.querySelector("[data-refresh]").addEventListener("click", () => {
    startBalances(true);
  });
  root.querySelector("[data-lock]").addEventListener("click", toggleLock);
  root.querySelector("[data-new-space]").addEventListener("click", () => openCreateSpace((result) => {
    state.spaces.push(result.space);
    update({ currentSpaceID: result.space.id, accounts: result.accounts });
    rerenderShell();
  }));
  const createMenu = root.querySelector("[data-create-menu]");
  createMenu.querySelector("[data-menu-toggle]").addEventListener("click", () => toggleMenu(createMenu));
  createMenu.querySelector("[data-derive]").addEventListener("click", () => {
    closeMenus();
    openDerive(state.currentSpaceID, enabledNetworks(), nextDerivationIndex, (created) => {
      upsertAccount(created);
      renderAccounts();
      startBalances(true, [created.id]);
    });
  });
  createMenu.querySelector("[data-import]").addEventListener("click", () => {
    closeMenus();
    openImport(state.currentSpaceID, enabledNetworks(), (created) => {
      upsertAccount(created);
      toast("Ключ импортирован. Не забудьте backup.");
      renderAccounts();
      startBalances(true, [created.id]);
    });
  });
  const spaceMenu = root.querySelector("[data-space-menu]");
  spaceMenu.querySelector("[data-menu-toggle]").addEventListener("click", () => toggleMenu(spaceMenu));
  spaceMenu.querySelectorAll("[data-space-action]").forEach((button) => {
    button.addEventListener("click", () => {
      closeMenus();
      if (button.dataset.spaceAction === "rename") {
        openSpaceRename(currentSpace(), (updated) => {
          Object.assign(currentSpace(), updated);
          rerenderShell();
        });
      }
      if (button.dataset.spaceAction === "mnemonic") showMnemonic(state.currentSpaceID);
      if (button.dataset.spaceAction === "password") showChangePassword(state.currentSpaceID);
      if (button.dataset.spaceAction === "backup") backupSpace(currentSpace());
    });
  });
  bindAccountActions();
}

function bindAccountActions() {
  document.querySelectorAll("[data-account-menu]").forEach((button) => {
    button.addEventListener("click", () => toggleMenu(button.closest(".menu")));
  });
  document.querySelectorAll("[data-copy]").forEach((button) => {
    button.addEventListener("click", async () => {
      closeMenus();
      await navigator.clipboard.writeText(button.dataset.copy);
      toast("Адрес скопирован");
    });
  });
  document.querySelectorAll("[data-action]").forEach((button) => {
    button.addEventListener("click", () => {
      closeMenus();
      const account = state.accounts.find((item) => item.id === button.closest("[data-account]").dataset.account);
      const network = state.networks.find((item) => item.id === button.dataset.network);
      if (button.dataset.action === "send") showSend(account, network);
      if (button.dataset.action === "bind") showBindNetwork(account);
      if (button.dataset.action === "rename") {
        openAccountRename(state.currentSpaceID, account, (updated) => {
          Object.assign(account, updated);
          renderAccounts();
        });
      }
      if (button.dataset.action === "export") {
        openExport(state.currentSpaceID, account, account.family || "evm");
      }
      if (button.dataset.action === "resources") showResources(account, network);
      if (button.dataset.action === "deploy") showDeploy(account, network);
    });
  });
}

function bindMenuDismissal() {
  if (menuDismissalBound) return;
  menuDismissalBound = true;
  document.addEventListener("click", (event) => {
    if (!event.target.closest(".menu")) closeMenus();
  });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") closeMenus();
  });
  window.addEventListener("resize", closeMenus);
  window.addEventListener("scroll", closeMenus, true);
}

function toggleMenu(menu) {
  const shouldOpen = !menu.classList.contains("open");
  closeMenus();
  if (!shouldOpen) return;

  menu.classList.add("open");
  menu.closest(".account-card")?.classList.add("menu-active");
  menu.querySelector("[data-menu-toggle], [data-account-menu]")?.setAttribute("aria-expanded", "true");
  positionMenu(menu);
}

function positionMenu(menu) {
  const popover = menu.querySelector(".menu-popover");
  const trigger = menu.querySelector("[data-menu-toggle], [data-account-menu]");
  if (!popover || !trigger) return;

  menu.classList.remove("open-up");
  popover.style.removeProperty("--menu-max-height");
  const triggerRect = trigger.getBoundingClientRect();
  const gap = 8;
  const viewportPadding = 12;
  const spaceBelow = window.innerHeight - triggerRect.bottom - gap - viewportPadding;
  const spaceAbove = triggerRect.top - gap - viewportPadding;
  const openUp = popover.scrollHeight > spaceBelow && spaceAbove > spaceBelow;
  const available = Math.max(openUp ? spaceAbove : spaceBelow, 80);

  menu.classList.toggle("open-up", openUp);
  popover.style.setProperty("--menu-max-height", `${Math.floor(available)}px`);
}

function closeMenus() {
  document.querySelectorAll(".menu.open").forEach((menu) => {
    menu.classList.remove("open", "open-up");
    menu.closest(".account-card")?.classList.remove("menu-active");
    menu.querySelector(".menu-popover")?.style.removeProperty("--menu-max-height");
    menu.querySelector("[data-menu-toggle], [data-account-menu]")?.setAttribute("aria-expanded", "false");
  });
}

function showBindNetwork(account) {
  const candidates = connectableNetworks(account);
  if (!candidates.length) return;
  const legacy = !accountNetworks(account).length;
  modal({
    title: "Подключить сеть",
    subtitle: legacy
      ? "Это старая запись без network binding. Выберите только ту сеть, где вы действительно создавали этот wallet."
      : "Тот же key source станет доступен для балансов и операций в выбранной сети.",
    content: `<form class="form-stack" data-form>
      ${legacy ? '<div class="notice danger">Назначение определит address family старой записи. Проверьте сеть перед продолжением.</div>' : ""}
      <label class="field"><span>Сеть</span><select name="network_id">${candidates.map((network) => `<option value="${escapeHTML(network.id)}">${escapeHTML(network.name)}${network.testnet ? " · TESTNET" : ""}</option>`).join("")}</select></label>
      <div class="error-text" data-error></div>
      <button class="button primary" type="submit">Подключить wallet</button>
    </form>`,
    onMount(element, close) {
      const form = element.querySelector("[data-form]");
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        const networkID = new FormData(form).get("network_id");
        const network = candidates.find((item) => item.id === networkID);
        setBusy(form, true);
        try {
          const updated = await bindAccountNetwork(
            state.currentSpaceID, account.id, networkID,
          );
          upsertAccount(updated);
          close();
          renderAccounts();
          startBalances(true, [updated.id]);
          toast(`Wallet подключён к ${network.name}`);
        } catch (cause) {
          element.querySelector("[data-error]").textContent = cause.message;
        } finally {
          setBusy(form, false);
        }
      });
    },
  });
}

async function switchSpace(event) {
  balanceController?.abort();
  const spaceID = event.target.value;
  update({
    currentSpaceID: spaceID, accounts: [], balances: new Map(),
    balancesLoading: true, balanceFailures: 0,
  });
  try {
    const accounts = await listAccounts(spaceID, routeSignal);
    if (spaceID !== state.currentSpaceID) return;
    update({ accounts });
    renderShell(document.querySelector("#app"));
    bindShell(document.querySelector("#app"));
    startBalances();
  } catch (cause) {
    if (cause.name !== "AbortError" && spaceID === state.currentSpaceID) {
      toast(cause.message, "error");
    }
  }
}

async function refreshNetworkHealth() {
  const status = document.querySelector("[data-rpc-status]");
  const detail = document.querySelector("[data-rpc-detail]");
  try {
    const health = await doctorHealth(routeSignal);
    lastDoctorSnapshot = health;
    if (!status?.isConnected) return;
    const labels = {
      healthy: "● Healthy",
      degraded: "● Degraded",
      unavailable: "● Unavailable",
      checking: "● Checking",
    };
    status.textContent = labels[health.status] || "● Checking";
    status.style.color = health.status === "healthy"
      ? "var(--success)"
      : health.status === "checking" ? "var(--muted)" : "var(--danger)";
    detail.textContent = health.status === "checking"
      ? "Первичная проверка всех RPC-ноды"
      : `${health.healthy}/${health.total} сетей healthy · ${health.failed_nodes} нод недоступно`;
  } catch (cause) {
    if (cause.name === "AbortError" || !status?.isConnected) return;
    status.textContent = "● Unavailable";
    status.style.color = "var(--danger)";
    detail.textContent = "Doctor API недоступен";
  }
}

function showDoctorDetails() {
  const snapshot = lastDoctorSnapshot;
  if (!snapshot) {
    toast("Doctor ещё выполняет первичную проверку");
    return;
  }
  const rows = snapshot.networks.map((networkStatus) => {
    const network = state.networks.find((item) => item.id === networkStatus.network_id);
    const failed = networkStatus.nodes.filter((node) => node.status !== "healthy");
    return `<article class="doctor-row">
      <div><strong>${escapeHTML(network?.name || networkStatus.network_id)}</strong><span class="badge ${networkStatus.status === "healthy" ? "" : "danger"}">${escapeHTML(networkStatus.status)}</span></div>
      <small class="muted">${networkStatus.healthy}/${networkStatus.total} nodes healthy${failed.length
        ? ` · недоступны: ${failed.map((node) => escapeHTML(node.label)).join(", ")}`
        : ""}</small>
    </article>`;
  }).join("");
  modal({
    title: "Node Doctor",
    subtitle: "Фоновая проверка chain identity и доступности всех RPC endpoints.",
    wide: true,
    content: `<div class="doctor-list">${rows}</div>`,
  });
}

function startBalances(refresh = false, accountIDs = []) {
  if (accountIDs.length) {
    startTargetedBalances(refresh, accountIDs);
    return;
  }
  balanceController?.abort();
  for (const controller of targetedBalanceControllers) controller.abort();
  targetedBalanceControllers.clear();
  balanceController = new AbortController();
  const generation = state.balanceGeneration + 1;
  update({ balanceGeneration: generation, balancesLoading: true, balanceFailures: 0 });
  const spaceID = state.currentSpaceID;
  const selected = accountIDs.length ? new Set(accountIDs) : null;
  const requests = balanceNetworks(selected).map((network) =>
    streamNetworkBalances(network, balanceController, generation, spaceID, refresh, accountIDs, selected));
  reportBalanceFailures(requests).then((failures) => {
    if (generation !== state.balanceGeneration || spaceID !== state.currentSpaceID) return;
    update({ balancesLoading: false, balanceFailures: failures });
    renderPortfolioSummary();
    loadPrices();
  });
}

function startTargetedBalances(refresh, accountIDs) {
  const controller = new AbortController();
  targetedBalanceControllers.add(controller);
  const generation = state.balanceGeneration;
  const spaceID = state.currentSpaceID;
  const selected = new Set(accountIDs);
  const requests = balanceNetworks(selected).map((network) => streamBalances(spaceID, network.id, {
    signal: controller.signal,
    refresh,
    accountIDs,
    onValue(result) {
      if (generation !== state.balanceGeneration || spaceID !== state.currentSpaceID) return;
      if (!selected.has(result.account_id)) return;
      state.balances.set(balanceKey(spaceID, network.id, result.account_id, result.asset_id), result);
      renderAccounts();
    },
  }));
  reportBalanceFailures(requests).then((failures) => {
    targetedBalanceControllers.delete(controller);
    if (controller.signal.aborted || spaceID !== state.currentSpaceID) return;
    if (failures) {
      update({ balanceFailures: Math.max(state.balanceFailures || 0, failures) });
      renderPortfolioSummary();
    }
    loadPrices();
  });
}

function balanceNetworks(selected) {
  return enabledNetworks().filter((network) => state.accounts.some((account) =>
    accountBoundTo(account, network.id) && (!selected || selected.has(account.id))));
}

function streamNetworkBalances(network, controller, generation, spaceID, refresh, accountIDs, selected) {
  return streamBalances(spaceID, network.id, {
    signal: controller.signal,
    refresh,
    accountIDs,
    onValue(result) {
      if (generation !== state.balanceGeneration || spaceID !== state.currentSpaceID) return;
      if (selected && !selected.has(result.account_id)) return;
      state.balances.set(balanceKey(spaceID, network.id, result.account_id, result.asset_id), result);
      renderAccounts();
    },
  });
}

function reportBalanceFailures(requests) {
  return Promise.allSettled(requests).then((results) => {
    const failures = results.filter((result) =>
      result.status === "rejected" && result.reason?.name !== "AbortError");
    if (failures.length) {
      toast(`Не удалось обновить баланс в ${failures.length} сетях: ${failures[0].reason.message}`, "error");
    }
    return failures.length;
  });
}

async function toggleLock() {
  const space = currentSpace();
  if (space.locked) {
    openUnlock(space, () => {
      space.locked = false;
      rerenderShell();
    });
    return;
  }
  try {
    await lockSpace(space.id);
    space.locked = true;
    renderShell(document.querySelector("#app"));
    bindShell(document.querySelector("#app"));
    startBalances();
  } catch (cause) {
    toast(cause.message, "error");
  }
}

function rerenderShell() {
  renderShell(document.querySelector("#app"));
  bindShell(document.querySelector("#app"));
  startBalances();
}

function showSend(account, network) {
  const assets = assetsFor(network);
  modal({
    title: `Отправить · ${network.name}`,
    subtitle: `${network.testnet ? "TESTNET · " : ""}Chain ID ${network.chain_id}`,
    content: `<form class="form-stack" data-form>
      ${network.testnet ? '<div class="notice">Это testnet. Токены не имеют mainnet-стоимости.</div>' : ""}
      <label class="field"><span>Asset</span><select name="asset_id">${assets.map((asset) => `<option value="${asset.id}">${escapeHTML(asset.name || asset.symbol)} · ${escapeHTML(asset.symbol)}</option>`).join("")}</select></label>
      <label class="field"><span>Recipient</span><input name="to" required spellcheck="false" autocomplete="off"></label>
      <label class="field"><span>Amount</span><input name="amount" required inputmode="decimal" placeholder="0.0"></label>
      <div data-estimate></div>
      <div class="error-text" data-error></div>
      <button class="button primary" type="submit">Рассчитать комиссию</button>
    </form>`,
    onMount(element, close) {
      const form = element.querySelector("[data-form]");
      let confirmedBody;
      let idempotencyKey;
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        const data = new FormData(form);
        const body = {
          account_id: account.id, asset_id: data.get("asset_id"),
          to: data.get("to"), amount: data.get("amount"),
        };
        setBusy(form, true, confirmedBody ? "Подписываем…" : "Считаем…");
        form.querySelector("[data-error]").textContent = "";
        try {
          if (!confirmedBody || JSON.stringify(confirmedBody) !== JSON.stringify(body)) {
            const estimate = await estimateTransfer(state.currentSpaceID, network.id, body, routeSignal);
            confirmedBody = body;
            form.querySelector("[data-estimate]").innerHTML = `
              <div class="notice">
                <strong>Проверьте перед подписью</strong><br>
                ${escapeHTML(network.name)} · Chain ${escapeHTML(network.chain_id)}<br>
                From: <span class="mono">${escapeHTML(shortAddress(account.addresses[network.family]))}</span><br>
                To: <span class="mono">${escapeHTML(shortAddress(body.to))}</span><br>
                Amount: ${escapeHTML(body.amount)} · Max fee: ${escapeHTML(estimate.fee)} ${escapeHTML(network.native.symbol)}
              </div>`;
            setBusy(form, false);
            form.querySelector('[type="submit"]').dataset.label = "Подписать и отправить";
            form.querySelector('[type="submit"]').textContent = "Подписать и отправить";
            return;
          }
          idempotencyKey ||= crypto.randomUUID();
          const operation = await sendTransfer(
            state.currentSpaceID, network.id, body, idempotencyKey, routeSignal,
          );
          close();
          toast(`Транзакция отправлена: ${shortAddress(operation.tx_hash)}`);
          trackReceipt(state.currentSpaceID, network.id, operation.tx_hash, account.id, body.to);
        } catch (cause) {
          if (cause.status && !String(cause.message).includes("still in progress")) {
            idempotencyKey = undefined;
          }
          form.querySelector("[data-error]").textContent = cause.message;
        } finally {
          setBusy(form, false);
        }
      });
      form.addEventListener("input", () => {
        confirmedBody = null;
        idempotencyKey = undefined;
        form.querySelector("[data-estimate]").replaceChildren();
        form.querySelector('[type="submit"]').textContent = "Рассчитать комиссию";
      });
    },
  });
}

async function showResources(account, network) {
  const dialog = modal({
    title: "Tron resources",
    subtitle: `${network.name} · ${shortAddress(account.addresses.tron)}`,
    wide: true,
    content: '<div class="boot" style="min-height:220px">Загружаем staking position…</div>',
  });
  try {
    const position = await resources(state.currentSpaceID, network.id, account.id, routeSignal);
    dialog.element.querySelector("[data-content]").innerHTML = `
      <div class="summary-grid">
        <article class="summary-card"><span>Bandwidth</span><strong>${escapeHTML(position.bandwidth.available)}</strong><small class="muted">из ${escapeHTML(position.bandwidth.total)}</small></article>
        <article class="summary-card"><span>Energy</span><strong>${escapeHTML(position.energy.available)}</strong><small class="muted">из ${escapeHTML(position.energy.total)}</small></article>
        <article class="summary-card"><span>Unstaking</span><strong>${escapeHTML(position.unstaking)}</strong><small class="muted">TRX · withdrawable ${escapeHTML(position.withdrawable_now)}</small></article>
      </div>
      <form class="form-stack" data-form>
        <label class="field"><span>Операция</span><select name="action">
          <option value="stake">Stake TRX</option>
          <option value="unstake">Unstake TRX</option>
          <option value="delegate">Delegate resource</option>
          <option value="reclaim">Reclaim resource</option>
          <option value="withdraw">Withdraw matured unstakes</option>
          <option value="cancel-unstakes">Cancel all unstakes</option>
        </select></label>
        <label class="field" data-resource><span>Resource</span><select name="resource"><option value="bandwidth">Bandwidth</option><option value="energy">Energy</option></select></label>
        <label class="field" data-amount><span>Amount</span><input name="amount" inputmode="decimal" placeholder="1"></label>
        <label class="field" data-to hidden><span>Receiver</span><input name="to" spellcheck="false"></label>
        <label data-all hidden><input type="checkbox" name="all"> Reclaim all</label>
        <div class="notice">Stake/unstake amount задаётся в TRX. Delegate/reclaim amount задаётся в bandwidth/energy units.</div>
        <div class="error-text" data-error></div>
        <div class="form-actions"><button class="button primary" type="submit">Проверить и подписать</button></div>
      </form>`;
    const form = dialog.element.querySelector("[data-form]");
    const action = form.querySelector('[name="action"]');
    let operationKey;
    let operationSignature;
    const syncFields = () => {
      const delegation = action.value === "delegate" || action.value === "reclaim";
      const bodyless = action.value === "withdraw" || action.value === "cancel-unstakes";
      form.querySelector("[data-to]").hidden = !delegation;
      form.querySelector("[data-all]").hidden = action.value !== "reclaim";
      form.querySelector("[data-resource]").hidden = bodyless;
      form.querySelector("[data-amount]").hidden = bodyless;
    };
    action.addEventListener("change", syncFields);
    syncFields();
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      const data = new FormData(form);
      const actionName = data.get("action");
      setBusy(form, true, "Подписываем…");
      try {
        const operationBody = actionName === "withdraw" || actionName === "cancel-unstakes" ? {} : {
          resource: data.get("resource"), amount: data.get("amount"),
          to: data.get("to"), all: data.get("all") === "on",
        };
        const signature = JSON.stringify({ actionName, operationBody });
        if (signature !== operationSignature) {
          operationSignature = signature;
          operationKey = crypto.randomUUID();
        }
        const result = await stakingOperation(
          state.currentSpaceID, network.id, account.id, actionName,
          operationBody,
          operationKey,
          routeSignal,
        );
        dialog.close();
        toast(`Tron transaction: ${shortAddress(result.tx_id)}`);
        trackReceipt(state.currentSpaceID, network.id, result.tx_id, account.id, "");
      } catch (cause) {
        if (cause.status && !String(cause.message).includes("still in progress")) {
          operationKey = undefined;
          operationSignature = undefined;
        }
        form.querySelector("[data-error]").textContent = cause.message;
      } finally {
        setBusy(form, false);
      }
    });
  } catch (cause) {
    dialog.element.querySelector("[data-content]").innerHTML = `<div class="error-text">${escapeHTML(cause.message)}</div>`;
  }
}

function showDeploy(account, network) {
  modal({
    title: "Deploy Tron contract",
    subtitle: `${network.name} · deployment расходует энергию даже при failure`,
    wide: true,
    content: `<form class="form-stack" data-form>
      <div class="notice danger">Сначала обязательно проверьте estimate и минимальный Fee Limit.</div>
      <label class="field"><span>Contract name</span><input name="name"></label>
      <label class="field"><span>Bytecode (hex)</span><textarea name="bytecode" required spellcheck="false"></textarea></label>
      <label class="field"><span>ABI JSON</span><textarea name="abi" spellcheck="false"></textarea></label>
      <label class="field"><span>Constructor params</span><input name="constructor_params" placeholder='[{"uint256":"1000"}]'></label>
      <label class="field"><span>Fee limit, TRX</span><input name="fee_limit" value="100" inputmode="decimal" required></label>
      <label class="field"><span>Consume user resource, %</span><input type="number" name="consume_user_resource_percent" value="100" min="0" max="100"></label>
      <label class="field"><span>Origin energy limit</span><input type="number" name="origin_energy_limit" value="10000000" min="0"></label>
      <div data-estimate></div><div class="error-text" data-error></div>
      <button class="button primary" type="submit">Рассчитать deployment</button>
    </form>`,
    onMount(element, close) {
      const form = element.querySelector("[data-form]");
      let confirmed;
      let idempotencyKey;
      const body = () => {
        const data = new FormData(form);
        return {
          name: data.get("name"), bytecode: data.get("bytecode"), abi: data.get("abi"),
          constructor_params: data.get("constructor_params"), fee_limit: data.get("fee_limit"),
          consume_user_resource_percent: Number(data.get("consume_user_resource_percent")),
          origin_energy_limit: Number(data.get("origin_energy_limit")),
        };
      };
      form.addEventListener("input", () => {
        confirmed = null;
        idempotencyKey = undefined;
        form.querySelector("[data-estimate]").replaceChildren();
      });
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        const requestBody = body();
        setBusy(form, true, confirmed ? "Deploy…" : "Считаем…");
        try {
          if (!confirmed || JSON.stringify(confirmed) !== JSON.stringify(requestBody)) {
            const cost = await estimateDeploy(
              state.currentSpaceID, network.id, account.id, requestBody, routeSignal,
            );
            confirmed = requestBody;
            form.querySelector("[data-estimate]").innerHTML = `<div class="notice">
              Energy ${escapeHTML(cost.energy)} · fee ${escapeHTML(cost.fee)} TRX · minimum Fee Limit ${escapeHTML(cost.min_fee_limit)} TRX
            </div>`;
            setBusy(form, false);
            form.querySelector('[type="submit"]').dataset.label = "Подписать и deploy";
            form.querySelector('[type="submit"]').textContent = "Подписать и deploy";
            return;
          }
          idempotencyKey ||= crypto.randomUUID();
          const result = await deployContract(
            state.currentSpaceID, network.id, account.id, requestBody,
            idempotencyKey, routeSignal,
          );
          close();
          toast(result.failure ? `Deploy failed: ${result.failure}` : `Contract: ${result.address}`,
            result.failure ? "error" : "");
          startBalances(true, [account.id]);
        } catch (cause) {
          if (cause.status && !String(cause.message).includes("still in progress")) {
            idempotencyKey = undefined;
          }
          form.querySelector("[data-error]").textContent = cause.message;
        } finally {
          setBusy(form, false);
        }
      });
    },
  });
}

async function trackReceipt(spaceID, networkID, txID, senderID, recipient) {
  if (!txID) return;
  for (let attempt = 0; attempt < 60 && !routeSignal.aborted; attempt += 1) {
    await new Promise((resolve) => setTimeout(resolve, 3000));
    try {
      const status = await transactionStatus(spaceID, networkID, txID, routeSignal);
      if (status.status === "pending") continue;
      toast(status.status === "confirmed" ? "Транзакция подтверждена" : "Транзакция завершилась ошибкой",
        status.status === "confirmed" ? "" : "error");
      if (spaceID !== state.currentSpaceID) return;
      const family = state.networks.find((item) => item.id === networkID)?.family;
      const recipientAccount = state.accounts.find((item) =>
        accountBoundTo(item, networkID) &&
        item.addresses[family]?.toLowerCase() === recipient?.toLowerCase());
      startBalances(true, [senderID, recipientAccount?.id].filter(Boolean));
      return;
    } catch (cause) {
      if (cause.name === "AbortError") return;
    }
  }
}
