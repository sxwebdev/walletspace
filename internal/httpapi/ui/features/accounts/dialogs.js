import {
  deriveAccount,
  exportPrivateKey,
  importAccount,
  renameAccount,
} from "../../services/accounts.js";
import { escapeHTML, modal, secretBlock, setBusy } from "../../components/ui.js";

export function showDerive(spaceID, networks, nextIndex, onCreated) {
  const initialNetwork = networks[0];
  modal({
    title: "New wallet",
    subtitle: "The network is chosen for this wallet. The derivation index starts at zero separately in every network.",
    content: `<form class="form-stack" data-form>
      <label class="field"><span>Network</span><select name="network_id" required>${networkOptions(networks)}</select></label>
      <label class="field"><span>Label</span><input name="label" placeholder="Account ${nextIndex(initialNetwork.id)}"></label>
      <small class="hint" data-index-hint>Next derivation index: ${nextIndex(initialNetwork.id)}</small>
      <div class="error-text" data-error></div>
      <button class="button primary" type="submit">Create account</button>
    </form>`,
    onMount(element, close) {
      const form = element.querySelector("[data-form]");
      const networkSelect = form.querySelector('[name="network_id"]');
      networkSelect.addEventListener("change", () => {
        const index = nextIndex(networkSelect.value);
        form.querySelector('[name="label"]').placeholder = `Account ${index}`;
        form.querySelector("[data-index-hint]").textContent = `Next derivation index: ${index}`;
      });
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        setBusy(form, true);
        try {
          const created = await deriveAccount(
            spaceID, new FormData(form).get("network_id"), new FormData(form).get("label"),
          );
          close();
          onCreated(created);
        } catch (cause) {
          form.querySelector("[data-error]").textContent = cause.message;
        } finally {
          setBusy(form, false);
        }
      });
    },
  });
}

export function showImport(spaceID, networks, onCreated) {
  modal({
    title: "Import a private key",
    subtitle: "Choose the network this wallet has to be available in.",
    content: `<form class="form-stack" data-form autocomplete="off">
      <div class="notice">This account cannot be recovered from the mnemonic of the space. Back the space up, or keep the key somewhere else.</div>
      <label class="field"><span>Network</span><select name="network_id" required>${networkOptions(networks)}</select></label>
      <label class="field"><span>Private key</span><input type="password" name="private_key" required autocomplete="new-password" spellcheck="false"></label>
      <label class="field"><span>Label</span><input name="label" placeholder="Imported account"></label>
      <div class="error-text" data-error></div>
      <button class="button primary" type="submit">Import</button>
    </form>`,
    onMount(element, close) {
      const form = element.querySelector("[data-form]");
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        const data = new FormData(form);
        setBusy(form, true);
        try {
          const created = await importAccount(
            spaceID, data.get("network_id"), data.get("private_key"), data.get("label"),
          );
          form.reset();
          close();
          onCreated(created);
        } catch (cause) {
          form.querySelector('[name="private_key"]').value = "";
          form.querySelector("[data-error]").textContent = cause.message;
        } finally {
          setBusy(form, false);
        }
      });
    },
  });
}

function networkOptions(networks) {
  return networks.map((network) =>
    `<option value="${escapeHTML(network.id)}">${escapeHTML(network.name)}${network.testnet ? " · TESTNET" : ""}</option>`
  ).join("");
}

export function showRename(spaceID, account, onRenamed) {
  modal({
    title: "Rename account",
    content: `<form class="form-stack" data-form>
      <label class="field"><span>Label</span><input name="label" value="${escapeHTML(account.label || "")}"></label>
      <div class="error-text" data-error></div>
      <button class="button primary" type="submit">Save</button>
    </form>`,
    onMount(element, close) {
      const form = element.querySelector("[data-form]");
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        try {
          const updated = await renameAccount(spaceID, account.id, new FormData(form).get("label"));
          close();
          onRenamed(updated);
        } catch (cause) {
          form.querySelector("[data-error]").textContent = cause.message;
        }
      });
    },
  });
}

export function showExport(spaceID, account, defaultFamily) {
  const families = account.kind === "derived" && account.family
    ? [account.family]
    : ["tron", "evm"];
  modal({
    title: "Export a private key",
    subtitle: account.kind === "imported"
      ? "For an imported account the key is shared between Tron and EVM."
      : `A derived wallet uses BIP44 family ${account.family?.toUpperCase() || "legacy"}.`,
    content: `<form class="form-stack" data-form>
      <div class="notice danger">A private key is full control over the funds. Do not show it to anyone.</div>
      <label class="field"><span>Address family</span><select name="family">
        ${families.map((family) => `<option value="${escapeHTML(family)}" ${defaultFamily === family ? "selected" : ""}>${family === "tron" ? "Tron" : "EVM"}</option>`).join("")}
      </select></label>
      <label class="field"><span>Space password</span><input name="password" type="password" required autocomplete="current-password"></label>
      <div class="error-text" data-error></div>
      <button class="button danger" type="submit">Reveal the key</button>
    </form>`,
    onMount(element) {
      const form = element.querySelector("[data-form]");
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        setBusy(form, true, "Fetching…");
        const data = new FormData(form);
        try {
          const key = await exportPrivateKey(
            spaceID, account.id, data.get("family"), data.get("password"),
          );
          form.replaceWith(secretBlock(key, "Private key"));
        } catch (cause) {
          form.querySelector("[data-error]").textContent = cause.message;
        } finally {
          setBusy(form, false);
          // The password must not sit in the DOM after the request, whether it
          // was accepted or refused.
          if (form.isConnected) form.reset();
        }
      });
    },
  });
}
