import {
  deriveAccount,
  exportPrivateKey,
  importAccount,
  renameAccount,
} from "../../api/accounts.js";
import { escapeHTML, modal, setBusy, toast } from "../../components/ui.js";

export function showDerive(spaceID, network, nextIndex, onCreated) {
  modal({
    title: `Новый wallet · ${network.name}`,
    subtitle: `Индекс начинается с 0 независимо для ${network.name}. Совместимый ключ в другой сети будет подключён без дубликата.`,
    content: `<form class="form-stack" data-form>
      <label class="field"><span>Label</span><input name="label" placeholder="Account ${nextIndex}"></label>
      <div class="error-text" data-error></div>
      <button class="button primary" type="submit">Создать account</button>
    </form>`,
    onMount(element, close) {
      const form = element.querySelector("[data-form]");
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        setBusy(form, true);
        try {
          const created = await deriveAccount(
            spaceID, network.id, new FormData(form).get("label"),
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

export function showImport(spaceID, network, onCreated) {
  modal({
    title: "Импорт private key",
    subtitle: `Ключ будет явно подключён к ${network.name}. Его можно подключить и к другим сетям позже.`,
    content: `<form class="form-stack" data-form autocomplete="off">
      <div class="notice">Этот account не восстанавливается из мнемоники space. Сделайте backup space или сохраните ключ отдельно.</div>
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
            spaceID, network.id, data.get("private_key"), data.get("label"),
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
