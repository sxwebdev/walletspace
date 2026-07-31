import { listSpaces, lockSpace } from "../api/spaces.js";
import { listAccounts } from "../api/accounts.js";
import { listNetworks, networkHealth, streamBalances } from "../api/networks.js";
import { getSettings, listAssets } from "../api/settings.js";
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
  currentNetwork,
  currentSpace,
  state,
  update,
} from "../state/store.js";
import { escapeHTML, modal, setBusy, shortAddress, toast } from "../components/ui.js";

let routeSignal;
let balanceController;
let menuDismissalBound = false;
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
    const preferredNetwork = state.currentNetworkID || settings.ui.last_network_id || "tron-mainnet";
    const spaceID = spaces.some((item) => item.id === preferredSpace)
      ? preferredSpace
      : spaces[0].id;
    const networkID = networks.some((item) => item.id === preferredNetwork && item.enabled)
      ? preferredNetwork
      : networks.find((item) => item.enabled)?.id;
    update({ spaces, networks, currentSpaceID: spaceID, currentNetworkID: networkID });
    if (!networkID) {
      root.innerHTML = `<div class="boot">
        <strong>Нет включённых сетей</strong>
        <span>Включите хотя бы одну сеть на странице настроек.</span>
        <button class="button primary" type="button" data-open-settings>Открыть настройки</button>
      </div>`;
      root.querySelector("[data-open-settings]").addEventListener("click", () => navigate("/settings"));
      return cleanup;
    }
    const [accounts, assets] = await Promise.all([
      listAccounts(spaceID, signal), listAssets(networkID, signal),
    ]);
    update({ accounts, assets });
    renderShell(root);
    bindShell(root);
    startBalances();
  } catch (cause) {
    if (cause.name !== "AbortError") {
      root.innerHTML = `<div class="boot"><strong>Walletspace не загрузился</strong><span>${escapeHTML(cause.message)}</span></div>`;
    }
  }
  return cleanup;
}

function cleanup() {
  balanceController?.abort();
  balanceController = null;
  for (const controller of targetedBalanceControllers) controller.abort();
  targetedBalanceControllers.clear();
}

function renderShell(root) {
  const space = currentSpace();
  const network = currentNetwork();
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
        <div class="toolbar">
          <label class="sr-only" for="network-select">Network</label>
          <select class="control" id="network-select">${networkOptions()}</select>
          ${network.testnet ? '<span class="badge testnet">TESTNET</span>' : ""}
          <button class="button icon" type="button" data-settings title="Настройки">⚙</button>
        </div>
      </header>
      <main class="page">
        <section class="page-heading">
          <div>
            <p class="eyebrow">${escapeHTML(network.name)} · Chain ${escapeHTML(network.chain_id)}</p>
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
          <article class="summary-card"><span>Accounts</span><strong data-account-count>${state.accounts.length}</strong></article>
          <article class="summary-card"><span>Network</span><strong>${escapeHTML(network.native.symbol)}</strong><small class="muted">${network.testnet ? "Test network" : "Main network"}</small></article>
          <article class="summary-card"><span>RPC status</span><strong data-rpc-status>Checking…</strong><small class="muted" data-rpc-detail>Проверяем chain identity и свежий блок</small></article>
        </section>
        <section class="panel">
          <header class="panel-header">
            <div><h2>Аккаунты</h2><span class="muted">Баланс обновляется по строкам в фоне</span></div>
            <span class="badge">${escapeHTML(network.family.toUpperCase())}</span>
          </header>
          <div class="account-list" data-account-list>${accountCards()}</div>
        </section>
      </main>
    </div>`;
  queueMicrotask(refreshNetworkHealth);
}

function spaceOptions() {
  return state.spaces
    .map((item) => `<option value="${item.id}" ${item.id === state.currentSpaceID ? "selected" : ""}>${escapeHTML(item.name)}${item.locked ? " · locked" : ""}</option>`)
    .join("");
}

function networkOptions() {
  const groups = new Map();
  for (const item of state.networks.filter((item) => item.enabled)) {
    const label = item.id.split("-")[0].replace(/^./, (char) => char.toUpperCase());
    if (!groups.has(label)) groups.set(label, []);
    groups.get(label).push(item);
  }
  return [...groups.entries()]
    .map(([label, items]) => `<optgroup label="${escapeHTML(label)}">${items
      .map((item) => `<option value="${item.id}" ${item.id === state.currentNetworkID ? "selected" : ""}>${escapeHTML(item.short_name)}${item.testnet ? " · testnet" : ""}</option>`)
      .join("")}</optgroup>`)
    .join("");
}

function assetsFor(network) {
  return state.assets.filter((item) => item.network_id === network.id);
}

function accountCards() {
  const network = currentNetwork();
  const family = network.family;
  const assets = assetsFor(network);
  if (!state.accounts.length) return '<div class="empty">В этом space пока нет аккаунтов.</div>';
  return state.accounts.map((account) => {
    const address = account.addresses[family] || "";
    const balances = assets.map((asset) => {
      const item = state.balances.get(balanceKey(state.currentSpaceID, network.id, account.id, asset.id));
      if (!item) return `<div class="balance"><div class="skeleton"></div><span>${asset.symbol}</span></div>`;
      if (item.error) return `<div class="balance"><strong>—</strong><span title="${escapeHTML(item.error)}">${asset.symbol} · ошибка</span></div>`;
      return `<div class="balance ${item.stale ? "stale" : ""}"><strong>${escapeHTML(item.amount || "0")}</strong><span>${asset.symbol}${item.stale ? " · cached" : ""}</span></div>`;
    }).join("");
    return `
      <article class="account-card" data-account="${account.id}">
        <div class="account-identity">
          <div class="account-title">
            <strong>${escapeHTML(account.label || `Account ${account.index ?? ""}`)}</strong>
            <span class="badge ${account.kind === "imported" ? "imported" : ""}" title="${account.kind === "imported" ? "Не восстанавливается из мнемоники space" : "Восстанавливается из мнемоники space"}">${account.kind === "imported" ? "Импортирован" : "Derived"}</span>
          </div>
          <button class="address" type="button" data-copy="${escapeHTML(address)}" title="Копировать адрес">${escapeHTML(shortAddress(address))}</button>
        </div>
        <div class="toolbar">${balances}</div>
        <div class="menu">
          <button class="button icon" type="button" data-account-menu aria-label="Действия" aria-haspopup="menu" aria-expanded="false">•••</button>
          <div class="menu-popover">
            <button type="button" data-action="send">Отправить</button>
            ${network.family === "tron" ? '<button type="button" data-action="resources">Resources & staking</button><button type="button" data-action="deploy">Deploy contract</button>' : ""}
            <button type="button" data-action="rename">Переименовать</button>
            <button type="button" data-action="export">Экспорт private key</button>
            <button type="button" data-copy="${escapeHTML(address)}">Копировать адрес</button>
          </div>
        </div>
      </article>`;
  }).join("");
}

function renderAccounts() {
  const count = document.querySelector("[data-account-count]");
  if (count) count.textContent = String(state.accounts.length);
  document.querySelector("[data-account-list]")?.replaceChildren(
    document.createRange().createContextualFragment(accountCards()),
  );
  bindAccountActions();
}

function bindShell(root) {
  bindMenuDismissal();
  root.querySelector("[data-settings]").addEventListener("click", () => navigate("/settings"));
  root.querySelector("#space-select").addEventListener("change", switchSpace);
  root.querySelector("#network-select").addEventListener("change", switchNetwork);
  root.querySelector("[data-refresh]").addEventListener("click", () => startBalances(true));
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
    openDerive(state.currentSpaceID, state.accounts.length, (created) => {
      state.accounts.push(created);
      renderAccounts();
      startBalances(true, [created.id]);
    });
  });
  createMenu.querySelector("[data-import]").addEventListener("click", () => {
    closeMenus();
    openImport(state.currentSpaceID, (created) => {
      state.accounts.push(created);
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
      if (button.dataset.action === "send") showSend(account);
      if (button.dataset.action === "rename") {
        openAccountRename(state.currentSpaceID, account, (updated) => {
          Object.assign(account, updated);
          renderAccounts();
        });
      }
      if (button.dataset.action === "export") {
        openExport(state.currentSpaceID, account, currentNetwork().family);
      }
      if (button.dataset.action === "resources") showResources(account);
      if (button.dataset.action === "deploy") showDeploy(account);
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

async function switchSpace(event) {
  balanceController?.abort();
  const spaceID = event.target.value;
  update({ currentSpaceID: spaceID, accounts: [] });
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

async function switchNetwork(event) {
  balanceController?.abort();
  const networkID = event.target.value;
  update({ currentNetworkID: networkID });
  try {
    const assets = await listAssets(networkID, routeSignal);
    if (networkID !== state.currentNetworkID) return;
    update({ assets });
  } catch (cause) {
    if (cause.name !== "AbortError" && networkID === state.currentNetworkID) {
      toast(cause.message, "error");
    }
    if (networkID !== state.currentNetworkID) return;
  }
  renderShell(document.querySelector("#app"));
  bindShell(document.querySelector("#app"));
  startBalances();
}

async function refreshNetworkHealth() {
  const networkID = state.currentNetworkID;
  const status = document.querySelector("[data-rpc-status]");
  const detail = document.querySelector("[data-rpc-detail]");
  try {
    const health = await networkHealth(networkID, routeSignal);
    if (networkID !== state.currentNetworkID || !status?.isConnected) return;
    status.textContent = health.status === "healthy" ? "● Healthy" : "● Unavailable";
    status.style.color = health.status === "healthy" ? "var(--success)" : "var(--danger)";
    detail.textContent = health.status === "healthy"
      ? "Chain identity подтверждён"
      : "Проверьте RPC в Settings";
  } catch (cause) {
    if (cause.name === "AbortError" || networkID !== state.currentNetworkID || !status?.isConnected) return;
    status.textContent = "● Unavailable";
    status.style.color = "var(--danger)";
    detail.textContent = "Проверьте RPC в Settings";
  }
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
  update({ balanceGeneration: generation });
  const networkID = state.currentNetworkID;
  const spaceID = state.currentSpaceID;
  const selected = accountIDs.length ? new Set(accountIDs) : null;
  streamBalances(spaceID, networkID, {
    signal: balanceController.signal,
    refresh,
    accountIDs,
    onValue(result) {
      if (generation !== state.balanceGeneration || networkID !== state.currentNetworkID) return;
      if (selected && !selected.has(result.account_id)) return;
      state.balances.set(balanceKey(spaceID, networkID, result.account_id, result.asset_id), result);
      renderAccounts();
    },
  }).catch((cause) => {
    if (cause.name !== "AbortError") toast(`Баланс: ${cause.message}`, "error");
  });
}

function startTargetedBalances(refresh, accountIDs) {
  const controller = new AbortController();
  targetedBalanceControllers.add(controller);
  const generation = state.balanceGeneration;
  const networkID = state.currentNetworkID;
  const spaceID = state.currentSpaceID;
  const selected = new Set(accountIDs);
  streamBalances(spaceID, networkID, {
    signal: controller.signal,
    refresh,
    accountIDs,
    onValue(result) {
      if (generation !== state.balanceGeneration || networkID !== state.currentNetworkID) return;
      if (!selected.has(result.account_id)) return;
      state.balances.set(balanceKey(spaceID, networkID, result.account_id, result.asset_id), result);
      renderAccounts();
    },
  }).catch((cause) => {
    if (cause.name !== "AbortError") toast(`Баланс: ${cause.message}`, "error");
  }).finally(() => targetedBalanceControllers.delete(controller));
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

function showSend(account) {
  const network = currentNetwork();
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

async function showResources(account) {
  const network = currentNetwork();
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

function showDeploy(account) {
  const network = currentNetwork();
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
      if (spaceID !== state.currentSpaceID || networkID !== state.currentNetworkID) return;
      const family = state.networks.find((item) => item.id === networkID)?.family;
      const recipientAccount = state.accounts.find((item) =>
        item.addresses[family]?.toLowerCase() === recipient.toLowerCase());
      startBalances(true, [senderID, recipientAccount?.id].filter(Boolean));
      return;
    } catch (cause) {
      if (cause.name === "AbortError") return;
    }
  }
}
