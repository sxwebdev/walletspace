import { listSpaces, lockSpace } from "../services/spaces.js";
import { bindAccountNetwork, listAccounts } from "../services/accounts.js";
import { doctorHealth, listNetworks, streamBalances } from "../services/networks.js";
import { getSettings, listAssets } from "../services/settings.js";
import { listPrices } from "../services/prices.js";
import { estimateTransfer, sendTransfer, transactionStatus } from "../services/transfers.js";
import { deployContract, estimateDeploy, resources, stakingOperation } from "../services/tron.js";
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
import { addressGroups, escapeHTML, modal, setBusy, shortAddress, toast } from "../components/ui.js";

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
        <strong>No enabled networks</strong>
        <span>Enable at least one network on the settings page.</span>
        <button class="button primary" type="button" data-open-settings>Open the settings</button>
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
      root.innerHTML = `<div class="boot"><strong>Walletspace did not load</strong><span>${escapeHTML(cause.message)}</span></div>`;
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
          <button class="button icon" type="button" data-new-space title="New space">＋</button>
          <button class="button icon" type="button" data-lock title="${space.locked ? "Unlock" : "Lock"}">${space.locked ? "🔒" : "🔓"}</button>
          <div class="menu" data-space-menu>
            <button class="button icon" type="button" data-menu-toggle aria-label="Space actions" aria-haspopup="menu" aria-expanded="false">•••</button>
            <div class="menu-popover">
              <button type="button" data-space-action="rename">Rename space</button>
              <button type="button" data-space-action="mnemonic">Show the recovery phrase</button>
              <button type="button" data-space-action="password">Change the password</button>
              <button type="button" data-space-action="backup">Download an encrypted backup</button>
            </div>
          </div>
        </div>
        <button class="button icon" type="button" data-settings title="Settings">⚙</button>
      </header>
      <main class="page">
        <section class="page-heading">
          <div>
            <p class="eyebrow">Secure Space · every network</p>
            <h1>${escapeHTML(space.name)}</h1>
            <p class="muted">${space.locked ? "Read-only · unlock the space for signatures and keys" : "The vault is unlocked locally"}</p>
          </div>
          <div class="toolbar">
            <button class="button" type="button" data-refresh>Refresh</button>
            <div class="menu" data-create-menu>
              <button class="button primary" type="button" data-menu-toggle aria-haspopup="menu" aria-expanded="false">Create ▾</button>
              <div class="menu-popover">
                <button type="button" data-derive>New derived account</button>
                <button type="button" data-import>Import a private key</button>
              </div>
            </div>
          </div>
        </section>
        <section class="summary-grid">
          <article class="summary-card"><span>Total balance · mainnet</span><strong data-total-usd>Calculating…</strong><small class="muted" data-market-change>Loading balances and USD quotes</small></article>
          <article class="summary-card"><span>Wallets with a balance</span><strong data-funded-wallets>—</strong><small class="muted" data-holding-detail>positions: — · mainnet without a price: —</small></article>
          <article class="summary-card"><span>Node doctor</span><strong data-rpc-status>Checking…</strong><small class="muted" data-rpc-detail>Checking every network and RPC node</small><button class="doctor-details" type="button" data-doctor-details>Details</button></article>
        </section>
        <section class="panel">
          <header class="panel-header">
            <div><h2>Wallets</h2><span class="muted">Space: ${escapeHTML(space.name)} · balances across every connected network at once</span></div>
            <label class="filter-control">
              <span class="sr-only">Network filter</span>
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
    .map((item) => `<option value="${escapeHTML(item.id)}" ${item.id === state.currentSpaceID ? "selected" : ""}>${escapeHTML(item.name)}${item.locked ? " · locked" : ""}</option>`)
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
    `<option value="all" ${state.accountFilter === "all" ? "selected" : ""}>Every network</option>`,
    `<option value="unassigned" ${state.accountFilter === "unassigned" ? "selected" : ""}>Needs a network</option>`,
    ...state.networks.map((item) =>
      `<option value="${escapeHTML(item.id)}" ${state.accountFilter === item.id ? "selected" : ""}>${escapeHTML(item.name)}${item.enabled ? "" : " · disabled"}</option>`),
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
      ? "No wallets match this filter yet."
      : "No wallets yet. Press “Create” and choose a network in the dialog."}</div>`;
  }
  return accounts.map((account) => {
    const connectable = connectableNetworks(account);
    return `
      <article class="account-card" data-account="${escapeHTML(account.id)}">
        <div class="account-identity">
          <div class="account-title">
            <strong>${escapeHTML(account.label || `Account ${account.index ?? ""}`)}</strong>
            <span class="badge ${account.kind === "imported" ? "imported" : ""}" title="${account.kind === "imported" ? "Cannot be recovered from the mnemonic of the space" : "Recoverable from the mnemonic of the space"}">${account.kind === "imported" ? "Imported" : "Derived"}</span>
            <span class="badge">Space · ${escapeHTML(currentSpace().name)}</span>
          </div>
        </div>
        <div class="menu">
          <button class="button icon" type="button" data-account-menu aria-label="Actions" aria-haspopup="menu" aria-expanded="false">•••</button>
          <div class="menu-popover">
            ${connectable.length ? '<button type="button" data-action="bind">Connect another network</button>' : ""}
            <button type="button" data-action="rename">Rename</button>
            <button type="button" data-action="export">Export the private key</button>
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
      return `<div class="wallet-network"><div><strong>${escapeHTML(networkID)}</strong><small class="muted">This network is missing from the configuration</small></div></div>`;
    }
    const address = account.addresses[network.family] || "";
    const balances = network.enabled ? visibleAssets(account, network).map((asset) => {
      const item = state.balances.get(balanceKey(state.currentSpaceID, network.id, account.id, asset.id));
      const symbol = escapeHTML(asset.symbol);
      if (!item) return `<div class="balance"><div class="skeleton"></div><span>${symbol}</span></div>`;
      if (item.error) return `<div class="balance"><strong>—</strong><span title="${escapeHTML(item.error)}">${symbol} · error</span></div>`;
      return `<div class="balance ${item.stale ? "stale" : ""}"><strong>${escapeHTML(item.amount || "0")}</strong><span>${symbol}${item.stale ? " · cached" : ""}</span></div>`;
    }).join("") : '<span class="muted">This network is disabled in the settings</span>';
    return `<section class="wallet-network" data-network="${escapeHTML(network.id)}">
      <div class="wallet-network-identity">
        <div class="network-badges"><strong>${escapeHTML(network.name)}</strong>${network.testnet ? '<span class="badge testnet">TESTNET</span>' : ""}${network.enabled ? "" : '<span class="badge danger">OFF</span>'}</div>
        <button class="address" type="button" data-copy="${escapeHTML(address)}" title="Copy the address">${escapeHTML(shortAddress(address))}</button>
      </div>
      <div class="wallet-balances">${balances}</div>
      <div class="wallet-actions">
        ${network.enabled ? `<button class="button" type="button" data-action="send" data-network="${escapeHTML(network.id)}">Send</button>` : ""}
        ${network.enabled && network.family === "tron" ? `<button class="button icon" type="button" data-action="resources" data-network="${escapeHTML(network.id)}" title="Resources & staking">R</button><button class="button icon" type="button" data-action="deploy" data-network="${escapeHTML(network.id)}" title="Deploy contract">D</button>` : ""}
      </div>
    </section>`;
  });
  return rows.length ? rows.join("") : '<div class="notice danger">No network assigned. Connect one from the wallet menu.</div>';
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
  const fundedElement = document.querySelector("[data-funded-wallets]");
  const holdingDetailElement = document.querySelector("[data-holding-detail]");
  if (!totalElement || !changeElement || !fundedElement || !holdingDetailElement) return;

  if (state.balancesLoading) {
    totalElement.textContent = "Calculating…";
    changeElement.textContent = "Loading mainnet balances";
    fundedElement.textContent = "—";
    holdingDetailElement.textContent = "positions: — · mainnet without a price: —";
    return;
  }

  const portfolio = calculatePortfolio();
  const holdings = holdingsWithBalance();
  const fundedWallets = new Set(holdings.map((holding) => holding.account.id));
  const testnetPositions = holdings.filter((holding) => holding.network.testnet).length;
  const failedNetworks = state.balanceFailures || 0;
  const failureSuffix = failedNetworks ? ` · failed networks: ${failedNetworks}` : "";
  fundedElement.textContent = failedNetworks
    ? fundedWallets.size ? `≥ ${fundedWallets.size}` : "—"
    : String(fundedWallets.size);
  holdingDetailElement.textContent = `positions: ${holdings.length} · mainnet without a price: ${portfolio.unpriced.size}${failureSuffix}`;
  if (!portfolio.assets.size) {
    if (failedNetworks) {
      totalElement.textContent = "—";
      changeElement.textContent = `Balances could not be loaded in ${failedNetworks} networks`;
      return;
    }
    totalElement.textContent = formatUSD(0);
    changeElement.textContent = testnetPositions
      ? "Testnet balances are not part of the USD total"
      : "No mainnet assets with a balance";
    return;
  }
  if (state.pricesLoading) {
    totalElement.textContent = "Calculating…";
    changeElement.textContent = "Loading USD quotes";
    return;
  }
  if (!portfolio.priced.size) {
    totalElement.textContent = "—";
    changeElement.textContent = state.pricesError || "No quotes found for these assets";
    return;
  }

  totalElement.textContent = `${portfolio.unpriced.size || failedNetworks ? "≈ " : ""}${formatUSD(portfolio.current)}`;
  const cached = state.pricesStale ? " · cached quotes" : "";
  if (!portfolio.historyComplete || portfolio.previous <= 0) {
    changeElement.textContent = `24h: not enough historical quotes${cached}${failureSuffix}`;
    return;
  }
  const delta = portfolio.current - portfolio.previous;
  const percent = delta / portfolio.previous * 100;
  const sign = delta > 0 ? "+" : delta < 0 ? "−" : "";
  changeElement.textContent = `Price change: ${sign}${formatUSD(Math.abs(delta))} · ${sign}${Math.abs(percent).toFixed(2)}% over 24h${cached}${failureSuffix}`;
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
  return holdingsWithBalance(mainnetIDs);
}

function holdingsWithBalance(networkIDs = new Set(
  enabledNetworks().map((network) => network.id),
)) {
  const holdings = [];
  for (const account of state.accounts) {
    for (const networkID of accountNetworks(account)) {
      if (!networkIDs.has(networkID)) continue;
      const network = state.networks.find((item) => item.id === networkID);
      for (const asset of assetsFor(network)) {
        const balance = state.balances.get(balanceKey(state.currentSpaceID, networkID, account.id, asset.id));
        if (!balance || balance.error || isZeroAmount(balance.amount)) continue;
        const amount = Number(balance.amount);
        if (Number.isFinite(amount) && amount > 0) holdings.push({ account, network, asset, amount });
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
      pricesError: "Quotes are temporarily unavailable",
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
      toast("Key imported. Do not forget the backup.");
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
      toast("Address copied");
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
    title: "Connect a network",
    subtitle: legacy
      ? "This is an old record with no network binding. Choose only the network this wallet was actually created in."
      : "The same key source becomes available for balances and operations in the chosen network.",
    content: `<form class="form-stack" data-form>
      ${legacy ? '<div class="notice danger">The assignment fixes the address family of the old record. Check the network before continuing.</div>' : ""}
      <label class="field"><span>Network</span><select name="network_id">${candidates.map((network) => `<option value="${escapeHTML(network.id)}">${escapeHTML(network.name)}${network.testnet ? " · TESTNET" : ""}</option>`).join("")}</select></label>
      <div class="error-text" data-error></div>
      <button class="button primary" type="submit">Connect the wallet</button>
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
          toast(`Wallet connected to ${network.name}`);
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
      ? "First pass over every RPC node"
      : `${health.healthy}/${health.total} networks healthy · ${health.failed_nodes} nodes unreachable`;
  } catch (cause) {
    if (cause.name === "AbortError" || !status?.isConnected) return;
    status.textContent = "● Unavailable";
    status.style.color = "var(--danger)";
    detail.textContent = "The doctor API is unreachable";
  }
}

function showDoctorDetails() {
  const snapshot = lastDoctorSnapshot;
  if (!snapshot) {
    toast("The doctor is still running its first pass");
    return;
  }
  const rows = snapshot.networks.map((networkStatus) => {
    const network = state.networks.find((item) => item.id === networkStatus.network_id);
    const failed = networkStatus.nodes.filter((node) => node.status !== "healthy");
    return `<article class="doctor-row">
      <div><strong>${escapeHTML(network?.name || networkStatus.network_id)}</strong><span class="badge ${networkStatus.status === "healthy" ? "" : "danger"}">${escapeHTML(networkStatus.status)}</span></div>
      <small class="muted">${networkStatus.healthy}/${networkStatus.total} nodes healthy${failed.length
        ? ` · unreachable: ${failed.map((node) => escapeHTML(node.label)).join(", ")}`
        : ""}</small>
    </article>`;
  }).join("");
  modal({
    title: "Node Doctor",
    subtitle: "A background check of the chain identity and the reachability of every RPC endpoint.",
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
      toast(`Balances could not be refreshed in ${failures.length} networks: ${failures[0].reason.message}`, "error");
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
    title: `Send · ${network.name}`,
    subtitle: `${network.testnet ? "TESTNET · " : ""}Chain ID ${network.chain_id}`,
    content: `<form class="form-stack" data-form>
      ${network.testnet ? '<div class="notice">This is a testnet. The tokens have no mainnet value.</div>' : ""}
      <label class="field"><span>Asset</span><select name="asset_id">${assets.map((asset) => `<option value="${escapeHTML(asset.id)}">${escapeHTML(asset.name || asset.symbol)} · ${escapeHTML(asset.symbol)}</option>`).join("")}</select></label>
      <label class="field"><span>Recipient</span><input name="to" required spellcheck="false" autocomplete="off"></label>
      <div class="field"><label for="send-amount">Amount</label><div class="input-action"><input id="send-amount" name="amount" required inputmode="decimal" placeholder="0.0"><button class="button" type="button" data-max>MAX</button></div></div>
      <div data-estimate></div>
      <div class="error-text" data-error></div>
      <button class="button primary" type="submit" data-estimate-button>Estimate the fee</button>
      <button class="button danger" type="button" data-sign hidden disabled>Sign and send</button>
    </form>`,
    onMount(element, close) {
      const form = element.querySelector("[data-form]");
      const maxButton = form.querySelector("[data-max]");
      const estimateButton = form.querySelector("[data-estimate-button]");
      const signButton = form.querySelector("[data-sign]");
      const errorBox = form.querySelector("[data-error]");
      let approved;
      let idempotencyKey;
      let armTimer;
      // Every in-flight estimate carries the generation it was started in. A
      // slow node answering after the user has typed something else must not
      // write that stale answer back into the form or arm the signing button
      // for it, so any response from an older generation is dropped.
      let generation = 0;

      const disarm = () => {
        approved = undefined;
        idempotencyKey = undefined;
        clearTimeout(armTimer);
        signButton.hidden = true;
        signButton.disabled = true;
        estimateButton.hidden = false;
        form.querySelector("[data-estimate]").replaceChildren();
      };

      const feeRow = (estimate) => {
        if (!estimate.gas_limit) return "";
        return `Gas limit: ${escapeHTML(String(estimate.gas_limit))} · Max fee/gas: ${escapeHTML(estimate.max_fee_per_gas)} wei<br>
            Priority fee: ${escapeHTML(estimate.max_priority_fee_per_gas)} wei · Pricing: ${escapeHTML(estimate.fee_model)}<br>`;
      };

      const arm = (body, estimate, asset) => {
        approved = { body, estimate };
        form.querySelector("[data-estimate]").innerHTML = `
          <div class="notice">
            <strong>Check this before signing</strong><br>
            ${escapeHTML(network.name)} · Chain ${escapeHTML(network.chain_id)}<br>
            Asset: ${escapeHTML(asset?.symbol || "")}${asset?.contract ? ` · <span class="mono">${addressGroups(asset.contract)}</span>` : ""}<br>
            From: <span class="mono address-full">${addressGroups(account.addresses[network.family])}</span><br>
            To: <span class="mono address-full">${addressGroups(body.to)}</span><br>
            Amount: ${escapeHTML(body.amount)} ${escapeHTML(asset?.symbol || "")}<br>
            ${feeRow(estimate)}Max fee: ${escapeHTML(estimate.fee)} ${escapeHTML(network.native.symbol)}
          </div>`;
        estimateButton.hidden = true;
        signButton.hidden = false;
        // Armed a beat late on purpose. The panel appears and the control goes
        // live in the same tick otherwise, so a held Enter or an impatient
        // second click signs a transfer that was never actually read.
        signButton.disabled = true;
        clearTimeout(armTimer);
        armTimer = setTimeout(() => {
          signButton.disabled = false;
        }, 600);
      };

      const transferBody = (amount = new FormData(form).get("amount")) => {
        const data = new FormData(form);
        return {
          account_id: account.id, asset_id: data.get("asset_id"),
          to: data.get("to"), amount,
        };
      };
      const selectedAsset = () =>
        assets.find((item) => item.id === new FormData(form).get("asset_id"));

      maxButton.addEventListener("click", async () => {
        const mine = ++generation;
        const requestBody = transferBody("max");
        errorBox.textContent = "";
        if (!String(requestBody.to).trim()) {
          errorBox.textContent = "Enter the recipient first — the fee depends on it";
          return;
        }
        disarm();
        maxButton.disabled = true;
        maxButton.textContent = "…";
        try {
          const estimate = await estimateTransfer(
            state.currentSpaceID, network.id, requestBody, routeSignal,
          );
          // Anything the user did while the node was thinking wins. Writing the
          // spendable balance over an amount they typed in the meantime, and
          // then arming the signing button for it, is how MAX turns into "send
          // everything" without anyone asking for it.
          if (mine !== generation) return;
          form.querySelector('[name="amount"]').value = estimate.amount;
          arm({ ...requestBody, amount: estimate.amount }, estimate, selectedAsset());
        } catch (cause) {
          if (mine === generation) errorBox.textContent = cause.message;
        } finally {
          maxButton.disabled = false;
          maxButton.textContent = "MAX";
        }
      });

      // Enter in a text field submits the form, so the submit handler must only
      // ever price the transfer. Signing lives on its own type="button" control
      // that implicit submission cannot reach.
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        const mine = ++generation;
        const body = transferBody();
        const asset = selectedAsset();
        disarm();
        setBusy(form, true, "Calculating…");
        errorBox.textContent = "";
        try {
          const estimate = await estimateTransfer(state.currentSpaceID, network.id, body, routeSignal);
          if (mine !== generation) return;
          arm(body, estimate, asset);
        } catch (cause) {
          if (mine === generation) errorBox.textContent = cause.message;
        } finally {
          setBusy(form, false);
        }
      });

      signButton.addEventListener("click", async () => {
        if (!approved) return;
        const { body, estimate } = approved;
        signButton.disabled = true;
        signButton.textContent = "Signing…";
        errorBox.textContent = "";
        try {
          idempotencyKey ||= crypto.randomUUID();
          // The approved fee travels with the request. The backend signs these
          // numbers rather than re-asking the node, so what is committed to is
          // what the panel above showed.
          const operation = await sendTransfer(
            state.currentSpaceID, network.id, {
              ...body,
              fee_model: estimate.fee_model,
              gas_limit: estimate.gas_limit,
              max_fee_per_gas: estimate.max_fee_per_gas,
              max_priority_fee_per_gas: estimate.max_priority_fee_per_gas,
            }, idempotencyKey, routeSignal,
          );
          close();
          announceBroadcast(operation, operation.tx_hash, "Transaction sent");
          trackReceipt(state.currentSpaceID, network.id, operation.tx_hash, account.id, body.to);
        } catch (cause) {
          if (retryIsSafe(cause)) idempotencyKey = undefined;
          // The dialog may already be gone from the screen, so the error goes
          // to a toast as well: a send that failed must never look like one
          // that quietly succeeded.
          errorBox.textContent = cause.message;
          toast(cause.message, "error");
          if (cause.status === 409) disarm();
        } finally {
          signButton.textContent = "Sign and send";
          signButton.disabled = !approved;
        }
      });

      form.addEventListener("input", () => {
        generation++;
        disarm();
      });
    },
  });
}

function stakingRequestBody(action, data) {
  if (action === "stake" || action === "unstake") {
    return { resource: data.get("resource"), amount: data.get("amount") };
  }
  if (action === "delegate") {
    return {
      resource: data.get("resource"), amount: data.get("amount"), to: data.get("to"),
    };
  }
  if (action === "reclaim") {
    return {
      resource: data.get("resource"), amount: data.get("amount"),
      to: data.get("to"), all: data.get("all") === "on",
    };
  }
  return {};
}

async function showResources(account, network) {
  const dialog = modal({
    title: "Tron resources",
    subtitle: `${network.name} · ${shortAddress(account.addresses.tron)}`,
    wide: true,
    content: '<div class="boot boot-inline">Loading the staking position…</div>',
  });
  try {
    const position = await resources(state.currentSpaceID, network.id, account.id, routeSignal);
    dialog.element.querySelector("[data-content]").innerHTML = `
      <div class="summary-grid">
        <article class="summary-card"><span>Bandwidth</span><strong>${escapeHTML(position.bandwidth.available)}</strong><small class="muted">of ${escapeHTML(position.bandwidth.total)}</small></article>
        <article class="summary-card"><span>Energy</span><strong>${escapeHTML(position.energy.available)}</strong><small class="muted">of ${escapeHTML(position.energy.total)}</small></article>
        <article class="summary-card"><span>Unstaking</span><strong>${escapeHTML(position.unstaking)}</strong><small class="muted">TRX · withdrawable ${escapeHTML(position.withdrawable_now)}</small></article>
      </div>
      <form class="form-stack" data-form>
        <label class="field"><span>Operation</span><select name="action">
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
        <div class="notice">A stake/unstake amount is given in TRX. A delegate/reclaim amount is given in bandwidth/energy units.</div>
        <div class="error-text" data-error></div>
        <div class="form-actions"><button class="button primary" type="submit">Sign and submit</button></div>
      </form>`;
    const form = dialog.element.querySelector("[data-form]");
    const action = form.querySelector('[name="action"]');
    const all = form.querySelector('[name="all"]');
    let operationKey;
    let operationSignature;
    const syncFields = () => {
      const delegation = action.value === "delegate" || action.value === "reclaim";
      const bodyless = action.value === "withdraw" || action.value === "cancel-unstakes";
      const reclaimAll = action.value === "reclaim" && all.checked;
      const receiver = form.querySelector("[data-to]");
      const amount = form.querySelector("[data-amount]");
      receiver.hidden = !delegation;
      receiver.querySelector("input").required = delegation;
      form.querySelector("[data-all]").hidden = action.value !== "reclaim";
      form.querySelector("[data-resource]").hidden = bodyless;
      amount.hidden = bodyless || reclaimAll;
      amount.querySelector("input").required = !bodyless && !reclaimAll;
    };
    action.addEventListener("change", syncFields);
    all.addEventListener("change", syncFields);
    syncFields();
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      const data = new FormData(form);
      const actionName = data.get("action");
      setBusy(form, true, "Signing…");
      try {
        const operationBody = stakingRequestBody(actionName, data);
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
        announceBroadcast(result, result.tx_id, "Tron transaction");
        trackReceipt(state.currentSpaceID, network.id, result.tx_id, account.id, "");
      } catch (cause) {
        if (retryIsSafe(cause)) {
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
    subtitle: `${network.name} · a deployment spends energy even when it fails`,
    wide: true,
    content: `<form class="form-stack" data-form>
      <div class="notice danger">Always check the estimate and the minimum fee limit first.</div>
      <label class="field"><span>Contract name</span><input name="name"></label>
      <label class="field"><span>Bytecode (hex)</span><textarea name="bytecode" required spellcheck="false"></textarea></label>
      <label class="field"><span>ABI JSON</span><textarea name="abi" spellcheck="false"></textarea></label>
      <label class="field"><span>Constructor params</span><input name="constructor_params" placeholder='[{"uint256":"1000"}]'></label>
      <label class="field"><span>Fee limit, TRX</span><input name="fee_limit" value="100" inputmode="decimal" required></label>
      <label class="field"><span>Consume user resource, %</span><input type="number" name="consume_user_resource_percent" value="100" min="0" max="100"></label>
      <label class="field"><span>Origin energy limit</span><input type="number" name="origin_energy_limit" value="10000000" min="0"></label>
      <div data-estimate></div><div class="error-text" data-error></div>
      <button class="button primary" type="submit">Estimate the deployment</button>
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
        // An edit invalidates the estimate, so the next click estimates again.
        // Leaving the label on "Sign and deploy" claimed the opposite.
        form.querySelector('[type="submit"]').textContent = "Estimate the deployment";
      });
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        const requestBody = body();
        setBusy(form, true, confirmed ? "Deploy…" : "Calculating…");
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
            form.querySelector('[type="submit"]').dataset.label = "Sign and deploy";
            form.querySelector('[type="submit"]').textContent = "Sign and deploy";
            return;
          }
          idempotencyKey ||= crypto.randomUUID();
          const result = await deployContract(
            state.currentSpaceID, network.id, account.id, requestBody,
            idempotencyKey, routeSignal,
          );
          close();
          if (inFlightStatuses.has(result.status)) {
            announceBroadcast(result, result.tx_id, "The deployment");
          } else {
            toast(result.failure ? `Deploy failed: ${result.failure}` : `Contract: ${result.address}`,
              result.failure ? "error" : "");
          }
          startBalances(true, [account.id]);
        } catch (cause) {
          if (retryIsSafe(cause)) idempotencyKey = undefined;
          form.querySelector("[data-error]").textContent = cause.message;
        } finally {
          setBusy(form, false);
        }
      });
    },
  });
}

// A transaction whose broadcast was not confirmed either way. The backend has
// signed it and sent it, and the node's answer never arrived — so it may be on
// chain. The one thing that must not happen next is signing a replacement, and
// the wording says so rather than reading like an ordinary send.
const inFlightStatuses = new Set(["broadcast_unknown", "broadcasting"]);

// Dropping the idempotency key means the next attempt builds and signs a new
// transaction, so it is only ever safe once the backend has said the last one
// provably never reached a node — which it says in exactly one way, by refusing
// the replay and asking for a new key.
//
// This is an allowlist on purpose. Keeping the key is always safe: the backend
// recognises the retry as the same request and answers with what it already
// knows. Dropping it on anything the backend did not explicitly clear is how a
// transfer that may already be on chain gets signed a second time.
function retryIsSafe(cause) {
  return Boolean(cause.status) && String(cause.message).includes("new idempotency key");
}

function announceBroadcast(result, txID, label) {
  if (inFlightStatuses.has(result?.status)) {
    toast(
      `${label} was sent but the node did not confirm it. Check ${shortAddress(txID)} in the ` +
      "explorer before trying again — sending a second one could move the funds twice.",
      "error",
    );
    return;
  }
  toast(`${label}: ${shortAddress(txID)}`);
}

async function trackReceipt(spaceID, networkID, txID, senderID, recipient) {
  if (!txID) return;
  for (let attempt = 0; attempt < 60 && !routeSignal.aborted; attempt += 1) {
    await new Promise((resolve) => setTimeout(resolve, 3000));
    try {
      const status = await transactionStatus(spaceID, networkID, txID, routeSignal);
      if (status.status === "pending") continue;
      toast(status.status === "confirmed" ? "Transaction confirmed" : "The transaction failed",
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
