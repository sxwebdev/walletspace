import { listNetworks } from "../api/networks.js";
import { listSpaces } from "../api/spaces.js";
import {
  deleteNetwork,
  addAsset,
  getNetworkSettings,
  getSettings,
  saveDiscovery,
  saveGeneral,
  saveNetwork,
  saveSecurity,
  listAssets,
  removeAsset,
} from "../api/settings.js";
import { navigate } from "../router.js";
import { escapeHTML, setBusy, toast } from "../components/ui.js";

let settings;
let networks;
let spaces;
let overrides;
let networkRevision;
let assets = [];

export async function render(root, signal) {
  try {
    [settings, networks, spaces, { networks: overrides, revision: networkRevision }] = await Promise.all([
      getSettings(signal),
      listNetworks(signal),
      listSpaces(signal),
      getNetworkSettings(signal),
    ]);
    assets = await listAssets(networks[0].id, signal);
    renderPage(root);
    bindPage(root);
  } catch (cause) {
    if (cause.name !== "AbortError") {
      root.innerHTML = `<div class="boot"><strong>Настройки не загрузились</strong><span>${escapeHTML(cause.message)}</span></div>`;
    }
  }
}

function renderPage(root) {
  root.innerHTML = `
    <div class="shell">
      <header class="topbar">
        <a class="brand" href="/" data-back>
          <span class="brand-mark" aria-hidden="true">W</span>
          <span class="brand-copy"><strong>Walletspace</strong><span>Settings</span></span>
        </a>
        <div></div>
        <button class="button" type="button" data-back>← Dashboard</button>
      </header>
      <main class="page">
        <section class="page-heading">
          <div><p class="eyebrow">Typed configuration · revision ${escapeHTML(settings.revision.slice(0, 8))}</p><h1>Настройки</h1><p class="muted">Вся file-backed конфигурация редактируется здесь. Секретные provider headers никогда не возвращаются API.</p></div>
        </section>
        <div class="settings-layout">
          <nav class="settings-nav" aria-label="Разделы">
            <a href="#general">Общие</a>
            <a href="#security">Безопасность</a>
            <a href="#discovery">Node Discovery</a>
            <a href="#networks">Сети и RPC</a>
            <a href="#assets">Assets</a>
          </nav>
          <div class="settings-content">
            ${generalCard()}
            ${securityCard()}
            ${discoveryCard()}
            ${networksCard()}
            ${assetsCard()}
          </div>
        </div>
      </main>
    </div>`;
}

function assetsCard() {
  return `<section class="settings-card" id="assets">
    <h2>Assets</h2><p class="muted">Контракт проверяется выбранной сетью; symbol и decimals читаются on-chain до сохранения.</p>
    <form data-assets>
      <label class="field"><span>Network</span><select name="network_id">${networks.map((item, index) => `<option value="${item.id}" ${index === 0 ? "selected" : ""}>${escapeHTML(item.name)}</option>`).join("")}</select></label>
      <label class="field"><span>ERC20 / TRC20 contract</span><input name="contract" required spellcheck="false"></label>
      <div class="error-text" data-error></div>
      <div class="form-actions"><button class="button primary" type="submit">Проверить и добавить</button></div>
    </form>
    <div class="network-overrides" data-assets-list style="margin-top:16px">${assetRows()}</div>
  </section>`;
}

function assetRows() {
  return assets.map((item) => `<div class="network-row">
    <div><strong>${escapeHTML(item.name || item.symbol)}</strong><br><span class="muted">${escapeHTML(item.symbol)} · ${escapeHTML(item.kind)}</span></div>
    <span class="address">${escapeHTML(item.contract || "Native asset")}</span>
    ${item.configured ? `<button class="button" type="button" data-delete-asset="${escapeHTML(item.id)}">Удалить</button>` : '<span class="badge">Built-in</span>'}
  </div>`).join("");
}

function generalCard() {
  return `<section class="settings-card" id="general">
    <h2>Общие</h2><p class="muted">Изменение listen address сохранится сейчас и применится после restart.</p>
    <form data-general>
      <label class="field"><span>UI address</span><input name="addr" value="${escapeHTML(settings.server.addr)}" required></label>
      <label><input type="checkbox" name="open_browser" ${settings.server.open_browser ? "checked" : ""}> Открывать браузер при запуске</label>
      <label class="field"><span>Default space</span><select name="last_space_id"><option value="">Не задан</option>${spaces.map((item) => `<option value="${item.id}" ${settings.ui.last_space_id === item.id ? "selected" : ""}>${escapeHTML(item.name)}</option>`).join("")}</select></label>
      <div class="error-text" data-error></div>
      <div class="form-actions"><button class="button primary" type="submit">Сохранить общие</button></div>
    </form>
  </section>`;
}

function securityCard() {
  return `<section class="settings-card" id="security">
    <h2>Безопасность</h2><p class="muted">Auto-lock считается отдельно для каждого space.</p>
    <form data-security>
      <label class="field"><span>Auto-lock timeout</span><input name="auto_lock" value="${escapeHTML(settings.security.auto_lock)}" placeholder="15m" required><small class="hint">Go duration: 30s, 15m, 2h. Значение 0 отключает auto-lock.</small></label>
      <div class="error-text" data-error></div>
      <div class="form-actions"><button class="button primary" type="submit">Сохранить безопасность</button></div>
    </form>
  </section>`;
}

function discoveryCard() {
  const value = settings.node_discovery;
  return `<section class="settings-card" id="discovery">
    <h2>Node Discovery</h2><p class="muted">Discovery предоставляет кандидатов. Chain ID и безопасность адреса проверяются локально.</p>
    <form data-discovery>
      <label><input type="checkbox" name="enabled" ${value.enabled ? "checked" : ""}> Использовать Node Discovery</label>
      <label class="field"><span>Service URL</span><input name="url" value="${escapeHTML(value.url)}" placeholder="https://discovery.example"><small class="hint">URL хранится в config.yaml и обязателен только когда Discovery включён.</small></label>
      <label class="field"><span>Refresh interval</span><input name="refresh_interval" value="${escapeHTML(value.refresh_interval)}" required></label>
      <label class="field"><span>Request timeout</span><input name="request_timeout" value="${escapeHTML(value.request_timeout)}" required></label>
      <label><input type="checkbox" name="allow_insecure_rpc" ${value.allow_insecure_rpc ? "checked" : ""}> Разрешить HTTP RPC <span class="badge testnet">не рекомендуется</span></label>
      <div class="error-text" data-error></div>
      <div class="form-actions"><button class="button primary" type="submit">Сохранить discovery</button></div>
    </form>
  </section>`;
}

function networksCard() {
  return `<section class="settings-card" id="networks">
    <h2>Сети и RPC</h2><p class="muted">Одна HTTPS RPC URL на строку. Пустой override использует discovery и official fallback.</p>
    <div class="network-overrides">
      ${networks.map((item) => {
        const override = overrides[item.id] || {};
        const enabled = override.enabled ?? item.enabled;
        return `<form class="network-row" data-network="${item.id}">
          <div><strong>${escapeHTML(item.name)}</strong><br><span class="muted">${escapeHTML(item.family.toUpperCase())} · ${escapeHTML(item.chain_id)}</span></div>
          <label class="field"><span class="sr-only">RPC URLs</span><textarea name="rpc_urls" rows="2" placeholder="${escapeHTML(item.rpc_fallbacks[0] || "")}">${escapeHTML((override.rpc_urls || []).join("\n"))}</textarea></label>
          <div class="form-stack">
            <label><input type="checkbox" name="enabled" ${enabled ? "checked" : ""}> Enabled</label>
            <button class="button secondary" type="submit">Применить</button>
            ${overrides[item.id] ? '<button class="button" type="button" data-reset>Сбросить</button>' : ""}
          </div>
          <details style="grid-column:1/-1">
            <summary>Advanced override</summary>
            <div class="form-stack" style="margin-top:12px">
              <label class="field"><span>Discovery</span><select name="discovery_enabled">
                <option value="">По global policy</option>
                <option value="true" ${override.discovery_enabled === true ? "selected" : ""}>Enabled</option>
                <option value="false" ${override.discovery_enabled === false ? "selected" : ""}>Disabled</option>
              </select></label>
              <label class="field"><span>Explorer address template</span><input name="explorer_address" value="${escapeHTML(override.explorer?.address || "")}" placeholder="${escapeHTML(item.explorer.address)}"></label>
              <label class="field"><span>Explorer transaction template</span><input name="explorer_tx" value="${escapeHTML(override.explorer?.tx || "")}" placeholder="${escapeHTML(item.explorer.tx)}"></label>
              <label class="field"><span>Explorer block template</span><input name="explorer_block" value="${escapeHTML(override.explorer?.block || "")}" placeholder="${escapeHTML(item.explorer.block)}"></label>
              <label class="field"><span>Provider header name</span><input name="header_name" placeholder="Authorization"></label>
              <label class="field"><span>Provider header secret</span><input type="password" name="header_value" placeholder="${override.has_headers ? "Секрет уже сохранён; пустое поле не меняет его" : "Необязательно"}" autocomplete="new-password"></label>
              ${override.has_headers ? '<label><input type="checkbox" name="clear_headers"> Удалить сохранённые provider headers</label>' : ""}
            </div>
          </details>
          <div class="error-text" data-error style="grid-column:1/-1"></div>
        </form>`;
      }).join("")}
    </div>
  </section>`;
}

function bindPage(root) {
  root.querySelectorAll("[data-back]").forEach((button) => button.addEventListener("click", (event) => {
    event.preventDefault();
    navigate("/");
  }));
  root.querySelector("[data-general]").addEventListener("submit", submitGeneral);
  root.querySelector("[data-security]").addEventListener("submit", submitSecurity);
  const discoveryForm = root.querySelector("[data-discovery]");
  const syncDiscoveryRequired = () => {
    discoveryForm.elements.url.required = discoveryForm.elements.enabled.checked;
  };
  discoveryForm.elements.enabled.addEventListener("change", syncDiscoveryRequired);
  syncDiscoveryRequired();
  discoveryForm.addEventListener("submit", submitDiscovery);
  root.querySelectorAll("[data-network]").forEach((form) => {
    form.addEventListener("submit", submitNetwork);
    form.querySelector("[data-reset]")?.addEventListener("click", () => resetNetwork(form));
  });
  const assetsForm = root.querySelector("[data-assets]");
  assetsForm.addEventListener("submit", submitAsset);
  assetsForm.querySelector('[name="network_id"]').addEventListener("change", async (event) => {
    assets = await listAssets(event.target.value);
    renderAssetRows();
  });
  bindAssetDeletes();
}

function renderAssetRows() {
  document.querySelector("[data-assets-list]").innerHTML = assetRows();
  bindAssetDeletes();
}

function bindAssetDeletes() {
  document.querySelectorAll("[data-delete-asset]").forEach((button) => {
    button.addEventListener("click", async () => {
      try {
        await removeAsset(button.dataset.deleteAsset);
        assets = assets.filter((item) => item.id !== button.dataset.deleteAsset);
        renderAssetRows();
        toast("Asset удалён");
      } catch (cause) {
        toast(cause.message, "error");
      }
    });
  });
}

async function submitAsset(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const data = new FormData(form);
  await saveForm(form, async () => {
    const created = await addAsset(data.get("network_id"), data.get("contract"));
    assets.push(created);
    form.querySelector('[name="contract"]').value = "";
    renderAssetRows();
  });
}

async function submitGeneral(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const data = new FormData(form);
  await saveForm(form, async () => {
    settings = await saveGeneral({
      server: { addr: data.get("addr"), open_browser: data.get("open_browser") === "on" },
      ui: { last_space_id: data.get("last_space_id") },
    }, settings.revision);
    networkRevision = settings.revision;
  });
}

async function submitSecurity(event) {
  event.preventDefault();
  const form = event.currentTarget;
  await saveForm(form, async () => {
    settings = await saveSecurity(
      { auto_lock: new FormData(form).get("auto_lock") },
      settings.revision,
    );
    networkRevision = settings.revision;
  });
}

async function submitDiscovery(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const data = new FormData(form);
  await saveForm(form, async () => {
    settings = await saveDiscovery({
      enabled: data.get("enabled") === "on",
      url: data.get("url"),
      refresh_interval: data.get("refresh_interval"),
      request_timeout: data.get("request_timeout"),
      allow_insecure_rpc: data.get("allow_insecure_rpc") === "on",
    }, settings.revision);
    networkRevision = settings.revision;
  });
}

async function submitNetwork(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const data = new FormData(form);
  await saveForm(form, async () => {
    const discoveryValue = data.get("discovery_enabled");
    const explorerAddress = data.get("explorer_address");
    const explorerTX = data.get("explorer_tx");
    const explorerBlock = data.get("explorer_block");
    const headerName = String(data.get("header_name") || "").trim();
    const headerValue = String(data.get("header_value") || "");
    const result = await saveNetwork(form.dataset.network, {
      enabled: data.get("enabled") === "on",
      rpc_urls: String(data.get("rpc_urls")).split(/\s+/).filter(Boolean),
      discovery_enabled: discoveryValue === "" ? null : discoveryValue === "true",
      explorer: explorerAddress || explorerTX || explorerBlock ? {
        address: explorerAddress,
        tx: explorerTX,
        block: explorerBlock,
      } : null,
      provider_headers: headerName && headerValue ? { [headerName]: headerValue } : {},
      clear_headers: data.get("clear_headers") === "on",
    }, networkRevision);
    overrides = result.networks;
    networkRevision = result.revision;
    settings.revision = result.revision;
  });
}

async function resetNetwork(form) {
  await saveForm(form, async () => {
    const result = await deleteNetwork(form.dataset.network, networkRevision);
    overrides = result.networks;
    networkRevision = result.revision;
    settings.revision = result.revision;
    form.querySelector('[name="rpc_urls"]').value = "";
    form.querySelector("[data-reset]")?.remove();
  });
}

async function saveForm(form, action) {
  const error = form.querySelector("[data-error]");
  error.textContent = "";
  setBusy(form, true);
  try {
    await action();
    toast("Настройки сохранены");
  } catch (cause) {
    error.textContent = cause.status === 412
      ? "Настройки изменились в другой вкладке. Обновите страницу и повторите."
      : cause.message;
  } finally {
    setBusy(form, false);
  }
}
