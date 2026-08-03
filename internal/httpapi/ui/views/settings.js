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
      root.innerHTML = `<div class="boot"><strong>The settings did not load</strong><span>${escapeHTML(cause.message)}</span></div>`;
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
          <div><p class="eyebrow">Typed configuration · revision ${escapeHTML(settings.revision.slice(0, 8))}</p><h1>Settings</h1><p class="muted">Every file-backed setting is edited here. Secret provider headers are never returned by the API.</p></div>
        </section>
        <div class="settings-layout">
          <nav class="settings-nav" aria-label="Sections">
            <a href="#general">General</a>
            <a href="#security">Security</a>
            <a href="#discovery">Node Discovery</a>
            <a href="#networks">Networks and RPC</a>
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
    <h2>Assets</h2><p class="muted">The contract is verified against the chosen network; the symbol and the decimals are read on-chain before saving.</p>
    <form data-assets>
      <label class="field"><span>Network</span><select name="network_id">${networks.map((item, index) => `<option value="${escapeHTML(item.id)}" ${index === 0 ? "selected" : ""}>${escapeHTML(item.name)}</option>`).join("")}</select></label>
      <label class="field"><span>ERC20 / TRC20 contract</span><input name="contract" required spellcheck="false"></label>
      <div class="error-text" data-error></div>
      <div class="form-actions"><button class="button primary" type="submit">Verify and add</button></div>
    </form>
    <div class="network-overrides gap-above-md" data-assets-list>${assetRows()}</div>
  </section>`;
}

function assetRows() {
  return assets.map((item) => `<div class="network-row">
    <div><strong>${escapeHTML(item.name || item.symbol)}</strong><br><span class="muted">${escapeHTML(item.symbol)} · ${escapeHTML(item.kind)}</span></div>
    <span class="address">${escapeHTML(item.contract || "Native asset")}</span>
    ${item.configured ? `<button class="button" type="button" data-delete-asset="${escapeHTML(item.id)}">Delete</button>` : '<span class="badge">Built-in</span>'}
  </div>`).join("");
}

function generalCard() {
  return `<section class="settings-card" id="general">
    <h2>General</h2><p class="muted">A changed listen address is saved now and takes effect after a restart.</p>
    <form data-general>
      <label class="field"><span>UI address</span><input name="addr" value="${escapeHTML(settings.server.addr)}" required></label>
      <label><input type="checkbox" name="open_browser" ${settings.server.open_browser ? "checked" : ""}> Open a browser on start</label>
      <label class="field"><span>Default space</span><select name="last_space_id"><option value="">Not set</option>${spaces.map((item) => `<option value="${escapeHTML(item.id)}" ${settings.ui.last_space_id === item.id ? "selected" : ""}>${escapeHTML(item.name)}</option>`).join("")}</select></label>
      <div class="error-text" data-error></div>
      <div class="form-actions"><button class="button primary" type="submit">Save general</button></div>
    </form>
  </section>`;
}

function securityCard() {
  return `<section class="settings-card" id="security">
    <h2>Security</h2><p class="muted">Auto-lock is counted separately for every space.</p>
    <form data-security>
      <label class="field"><span>Auto-lock timeout</span><input name="auto_lock" value="${escapeHTML(settings.security.auto_lock)}" placeholder="15m" required><small class="hint">Go duration: 5m, 15m, 2h. Between 1m and 24h — auto-lock is what takes the decrypted seed back out of memory, so it cannot be switched off.</small></label>
      <div class="error-text" data-error></div>
      <div class="form-actions"><button class="button primary" type="submit">Save security</button></div>
    </form>
  </section>`;
}

function discoveryCard() {
  const value = settings.node_discovery;
  return `<section class="settings-card" id="discovery">
    <h2>Node Discovery</h2><p class="muted">Discovery supplies candidates. The chain ID and the safety of the address are verified locally.</p>
    <form data-discovery>
      <label><input type="checkbox" name="enabled" ${value.enabled ? "checked" : ""}> Use Node Discovery</label>
      <label class="field"><span>Service URL</span><input name="url" value="${escapeHTML(value.url)}" placeholder="https://discovery.example"><small class="hint">The URL is stored in config.yaml and is only required while Discovery is enabled.</small></label>
      <label class="field"><span>Refresh interval</span><input name="refresh_interval" value="${escapeHTML(value.refresh_interval)}" required></label>
      <label class="field"><span>Request timeout</span><input name="request_timeout" value="${escapeHTML(value.request_timeout)}" required></label>
      <label><input type="checkbox" name="allow_insecure_rpc" ${value.allow_insecure_rpc ? "checked" : ""}> Allow HTTP RPC <span class="badge testnet">not recommended</span></label>
      <div class="error-text" data-error></div>
      <div class="form-actions"><button class="button primary" type="submit">Save discovery</button></div>
    </form>
  </section>`;
}

function networksCard() {
  return `<section class="settings-card" id="networks">
    <h2>Networks and RPC</h2><p class="muted">One HTTPS RPC URL per line. An empty override uses discovery and the official fallbacks.</p>
    <div class="network-overrides">
      ${networks.map((item) => {
        const override = overrides[item.id] || {};
        const enabled = override.enabled ?? item.enabled;
        const urls = (override.endpoints || []).map((endpoint) => endpoint.url);
        return `<form class="network-row" data-network="${escapeHTML(item.id)}">
          <div><strong>${escapeHTML(item.name)}</strong><br><span class="muted">${escapeHTML(item.family.toUpperCase())} · ${escapeHTML(item.chain_id)}</span></div>
          <label class="field"><span class="sr-only">RPC URLs</span><textarea name="rpc_urls" rows="2" placeholder="${escapeHTML(item.rpc_fallbacks[0] || "")}">${escapeHTML(urls.join("\n"))}</textarea></label>
          <div class="form-stack">
            <label><input type="checkbox" name="enabled" ${enabled ? "checked" : ""}> Enabled</label>
            <button class="button secondary" type="submit">Apply</button>
            ${overrides[item.id] ? '<button class="button" type="button" data-reset>Reset</button>' : ""}
          </div>
          <details class="span-full">
            <summary>Advanced override</summary>
            <div class="form-stack gap-above">
              <label class="field"><span>Discovery</span><select name="discovery_enabled">
                <option value="">Per global policy</option>
                <option value="true" ${override.discovery_enabled === true ? "selected" : ""}>Enabled</option>
                <option value="false" ${override.discovery_enabled === false ? "selected" : ""}>Disabled</option>
              </select></label>
              <label class="field"><span>Explorer address template</span><input name="explorer_address" value="${escapeHTML(override.explorer?.address || "")}" placeholder="${escapeHTML(item.explorer.address)}"></label>
              <label class="field"><span>Explorer transaction template</span><input name="explorer_tx" value="${escapeHTML(override.explorer?.tx || "")}" placeholder="${escapeHTML(item.explorer.tx)}"></label>
              <label class="field"><span>Explorer block template</span><input name="explorer_block" value="${escapeHTML(override.explorer?.block || "")}" placeholder="${escapeHTML(item.explorer.block)}"></label>
              <div class="form-stack" data-credentials>${credentialRows(urls, storedCredentials(override))}</div>
            </div>
          </details>
          <div class="error-text span-full" data-error></div>
        </form>`;
      }).join("")}
    </div>
  </section>`;
}

function storedCredentials(override) {
  return new Set((override.endpoints || []).filter((endpoint) => endpoint.has_headers)
    .map((endpoint) => endpoint.url));
}

// One credential block per URL, not one per network. A provider key is sent
// only to the endpoint it sits under: the rest of the list, the official
// fallbacks and anything discovery suggests never see it.
function credentialRows(urls, stored) {
  if (!urls.length) {
    return '<p class="muted">Add an RPC URL above to attach a provider credential to it.</p>';
  }
  return urls.map((url, index) => `<fieldset class="endpoint-credential">
    <legend>${escapeHTML(url)}</legend>
    <label class="field"><span>Provider header name</span><input name="header_name_${index}" placeholder="Authorization"></label>
    <label class="field"><span>Provider header secret</span><input type="password" name="header_value_${index}" placeholder="${stored.has(url) ? "A secret is stored for this endpoint; an empty field leaves it alone" : "Optional"}" autocomplete="new-password"></label>
    ${stored.has(url) ? `<label><input type="checkbox" name="clear_headers_${index}"> Delete the stored credential</label>` : ""}
  </fieldset>`).join("");
}

function urlsOf(form) {
  return String(form.elements.rpc_urls.value).split(/\s+/).filter(Boolean);
}

// The credential blocks are keyed by URL, so they have to follow the textarea
// rather than the state that was last saved. Without this a secret typed after
// an edit would be attached to the endpoint that used to be on that line.
function renderCredentialRows(form) {
  const container = form.querySelector("[data-credentials]");
  const next = credentialRows(urlsOf(form), storedCredentials(overrides[form.dataset.network] || {}));
  if (container.dataset.rendered !== next) {
    container.innerHTML = next;
    container.dataset.rendered = next;
  }
}

function bindCredentialRows(form) {
  form.elements.rpc_urls.addEventListener("input", () => renderCredentialRows(form));
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
    bindCredentialRows(form);
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
        toast("Asset deleted");
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
    const result = await saveNetwork(form.dataset.network, {
      enabled: data.get("enabled") === "on",
      endpoints: urlsOf(form).map((url, index) => endpointBody(url, data, index)),
      discovery_enabled: discoveryValue === "" ? null : discoveryValue === "true",
      explorer: explorerAddress || explorerTX || explorerBlock ? {
        address: explorerAddress,
        tx: explorerTX,
        block: explorerBlock,
      } : null,
    }, networkRevision);
    overrides = result.networks;
    networkRevision = result.revision;
    settings.revision = result.revision;
    // A credential that was just saved is now stored, so the block for it has to
    // switch to "already stored" and offer the delete checkbox.
    form.querySelectorAll('[name^="header_value_"]').forEach((input) => {
      input.value = "";
    });
    renderCredentialRows(form);
  });
}

// An omitted headers field leaves whatever is stored for that endpoint alone;
// an empty object deletes it. The browser is never given the stored secret, so
// it cannot send one back, and "unchanged" has to be expressible without it.
function endpointBody(url, data, index) {
  const name = String(data.get(`header_name_${index}`) || "").trim();
  const value = String(data.get(`header_value_${index}`) || "");
  if (data.get(`clear_headers_${index}`) === "on") return { url, headers: {} };
  if (name && value) return { url, headers: { [name]: value } };
  return { url };
}

async function resetNetwork(form) {
  await saveForm(form, async () => {
    const result = await deleteNetwork(form.dataset.network, networkRevision);
    overrides = result.networks;
    networkRevision = result.revision;
    settings.revision = result.revision;
    form.querySelector('[name="rpc_urls"]').value = "";
    renderCredentialRows(form);
    form.querySelector("[data-reset]")?.remove();
  });
}

async function saveForm(form, action) {
  const error = form.querySelector("[data-error]");
  error.textContent = "";
  setBusy(form, true);
  try {
    await action();
    toast("Settings saved");
  } catch (cause) {
    error.textContent = cause.status === 412
      ? "The settings changed in another tab. Reload the page and try again."
      : cause.message;
  } finally {
    setBusy(form, false);
  }
}
