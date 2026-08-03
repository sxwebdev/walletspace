import {
  changeSpacePassword,
  createSpace,
  downloadBackup,
  renameSpace,
  revealMnemonic,
  unlockSpace,
} from "../../api/spaces.js";
import { escapeHTML, modal, setBusy, toast } from "../../components/ui.js";

export function showUnlock(space, onUnlocked) {
  modal({
    title: `Разблокировать ${space.name}`,
    subtitle: "Пароль остаётся только в этом запросе.",
    content: `<form class="form-stack" data-form>
      <label class="field"><span>Пароль space</span><input type="password" name="password" required autocomplete="current-password"></label>
      <div class="error-text" data-error></div>
      <button class="button primary" type="submit">Разблокировать</button>
    </form>`,
    onMount(element, close) {
      const form = element.querySelector("[data-form]");
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        setBusy(form, true);
        try {
          await unlockSpace(space.id, new FormData(form).get("password"));
          form.reset();
          close();
          onUnlocked();
        } catch (cause) {
          form.querySelector("[data-error]").textContent = cause.message;
        } finally {
          setBusy(form, false);
        }
      });
    },
  });
}

export function showCreateSpace(onCreated) {
  modal({
    title: "Новый space",
    subtitle: "Отдельный vault без кошельков. Wallet можно создать позже в нужной сети.",
    content: `<form class="form-stack" data-form autocomplete="off">
      <label class="field"><span>Название</span><input name="name" placeholder="default"></label>
      <label class="field"><span>Мнемоника (необязательно)</span><textarea name="mnemonic" spellcheck="false"></textarea></label>
      <label><input type="checkbox" name="imported_only"> Imported-only, без мнемоники</label>
      <label class="field"><span>Пароль</span><input type="password" name="password" required minlength="8"></label>
      <div class="error-text" data-error></div>
      <button class="button primary" type="submit">Создать</button>
    </form>`,
    onMount(element, close) {
      const form = element.querySelector("[data-form]");
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        const data = new FormData(form);
        setBusy(form, true);
        try {
          const result = await createSpace({
            name: data.get("name"),
            mnemonic: data.get("mnemonic"),
            password: data.get("password"),
            imported_only: data.get("imported_only") === "on",
          });
          form.reset();
          close();
          if (result.mnemonic_generated) showSecret("Recovery phrase", result.mnemonic);
          onCreated(result);
        } catch (cause) {
          form.querySelector("[data-error]").textContent = cause.message;
        } finally {
          setBusy(form, false);
        }
      });
    },
  });
}

export function showRenameSpace(space, onRenamed) {
  modal({
    title: "Переименовать space",
    content: `<form class="form-stack" data-form>
      <label class="field"><span>Название</span><input name="name" value="${escapeHTML(space.name)}" required></label>
      <div class="error-text" data-error></div><button class="button primary" type="submit">Сохранить</button>
    </form>`,
    onMount(element, close) {
      const form = element.querySelector("[data-form]");
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        try {
          const updated = await renameSpace(space.id, new FormData(form).get("name"));
          close();
          onRenamed(updated);
        } catch (cause) {
          form.querySelector("[data-error]").textContent = cause.message;
        }
      });
    },
  });
}

export async function showMnemonic(spaceID) {
  try {
    showSecret("Recovery phrase", await revealMnemonic(spaceID));
  } catch (cause) {
    toast(cause.message, "error");
  }
}

export function showChangePassword(spaceID) {
  modal({
    title: "Сменить пароль space",
    subtitle: "Адреса и recovery phrase не изменятся.",
    content: `<form class="form-stack" data-form autocomplete="off">
      <label class="field"><span>Текущий пароль</span><input type="password" name="current" required></label>
      <label class="field"><span>Новый пароль</span><input type="password" name="next" minlength="8" required></label>
      <label class="field"><span>Повторите новый пароль</span><input type="password" name="confirmation" minlength="8" required></label>
      <div class="error-text" data-error></div><button class="button primary" type="submit">Перешифровать vault</button>
    </form>`,
    onMount(element, close) {
      const form = element.querySelector("[data-form]");
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        const data = new FormData(form);
        if (data.get("next") !== data.get("confirmation")) {
          form.querySelector("[data-error]").textContent = "Новые пароли не совпадают";
          return;
        }
        setBusy(form, true, "Перешифровываем…");
        try {
          await changeSpacePassword(spaceID, data.get("current"), data.get("next"));
          form.reset();
          close();
          toast("Пароль space изменён");
        } catch (cause) {
          form.querySelector("[data-error]").textContent = cause.message;
        } finally {
          setBusy(form, false);
        }
      });
    },
  });
}

export async function backupSpace(space) {
  try {
    await downloadBackup(space.id, space.name);
    toast("Encrypted backup скачан");
  } catch (cause) {
    toast(cause.message, "error");
  }
}

function showSecret(title, value) {
  const wrapper = document.createElement("div");
  wrapper.className = "form-stack";
  wrapper.innerHTML = `<div class="secret" data-secret></div><button class="button secondary" type="button" data-copy-secret>Копировать</button>`;
  wrapper.querySelector("[data-secret]").textContent = value;
  wrapper.querySelector("[data-copy-secret]").addEventListener("click", async () => {
    await navigator.clipboard.writeText(value);
    toast("Секрет скопирован");
  });
  modal({ title, content: wrapper });
}
