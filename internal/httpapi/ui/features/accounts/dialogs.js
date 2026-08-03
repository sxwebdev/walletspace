import {
  deriveAccount,
  exportPrivateKey,
  importAccount,
  renameAccount,
} from "../../api/accounts.js";
import { escapeHTML, modal, setBusy, toast } from "../../components/ui.js";

export function showDerive(spaceID, networks, nextIndex, onCreated) {
  const initialNetwork = networks[0];
  modal({
    title: "Новый wallet",
    subtitle: "Сеть выбирается для этого wallet. Derivation index считается с нуля независимо в каждой сети.",
    content: `<form class="form-stack" data-form>
      <label class="field"><span>Сеть</span><select name="network_id" required>${networkOptions(networks)}</select></label>
      <label class="field"><span>Label</span><input name="label" placeholder="Account ${nextIndex(initialNetwork.id)}"></label>
      <small class="hint" data-index-hint>Следующий derivation index: ${nextIndex(initialNetwork.id)}</small>
      <div class="error-text" data-error></div>
      <button class="button primary" type="submit">Создать account</button>
    </form>`,
    onMount(element, close) {
      const form = element.querySelector("[data-form]");
      const networkSelect = form.querySelector('[name="network_id"]');
      networkSelect.addEventListener("change", () => {
        const index = nextIndex(networkSelect.value);
        form.querySelector('[name="label"]').placeholder = `Account ${index}`;
        form.querySelector("[data-index-hint]").textContent = `Следующий derivation index: ${index}`;
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
    title: "Импорт private key",
    subtitle: "Выберите сеть, в которой этот wallet должен быть доступен.",
    content: `<form class="form-stack" data-form autocomplete="off">
      <div class="notice">Этот account не восстанавливается из мнемоники space. Сделайте backup space или сохраните ключ отдельно.</div>
      <label class="field"><span>Сеть</span><select name="network_id" required>${networkOptions(networks)}</select></label>
      <label class="field"><span>Private key</span><input type="password" name="private_key" required autocomplete="new-password" spellcheck="false"></label>
      <label class="field"><span>Label</span><input name="label" placeholder="Imported account"></label>
      <div class="error-text" data-error></div>
      <button class="button primary" type="submit">Импортировать</button>
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
    title: "Переименовать account",
    content: `<form class="form-stack" data-form>
      <label class="field"><span>Label</span><input name="label" value="${escapeHTML(account.label || "")}"></label>
      <div class="error-text" data-error></div>
      <button class="button primary" type="submit">Сохранить</button>
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
    title: "Экспорт private key",
    subtitle: account.kind === "imported"
      ? "Для импортированного account ключ общий в Tron и EVM."
      : `Derived wallet использует BIP44 family ${account.family?.toUpperCase() || "legacy"}.`,
    content: `<form class="form-stack" data-form>
      <div class="notice danger">Private key даёт полный контроль над средствами. Не показывайте его никому.</div>
      <label class="field"><span>Address family</span><select name="family">
        ${families.map((family) => `<option value="${family}" ${defaultFamily === family ? "selected" : ""}>${family === "tron" ? "Tron" : "EVM"}</option>`).join("")}
      </select></label>
      <div class="error-text" data-error></div>
      <button class="button danger" type="submit">Показать ключ</button>
    </form>`,
    onMount(element) {
      const form = element.querySelector("[data-form]");
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        setBusy(form, true, "Получаем…");
        try {
          const key = await exportPrivateKey(
            spaceID, account.id, new FormData(form).get("family"),
          );
          form.replaceWith(secretBlock(key));
        } catch (cause) {
          form.querySelector("[data-error]").textContent = cause.message;
        } finally {
          setBusy(form, false);
        }
      });
    },
  });
}

function secretBlock(value) {
  const wrapper = document.createElement("div");
  wrapper.className = "form-stack";
  wrapper.innerHTML = `<div class="secret" data-secret></div><button class="button secondary" type="button" data-copy-secret>Копировать</button>`;
  wrapper.querySelector("[data-secret]").textContent = value;
  wrapper.querySelector("[data-copy-secret]").addEventListener("click", async () => {
    await navigator.clipboard.writeText(value);
    toast("Private key скопирован");
  });
  return wrapper;
}
