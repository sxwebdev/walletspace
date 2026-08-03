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
    title: `Unlock ${space.name}`,
    subtitle: "The password stays inside this request only.",
    content: `<form class="form-stack" data-form>
      <label class="field"><span>Space password</span><input type="password" name="password" required autocomplete="current-password"></label>
      <div class="error-text" data-error></div>
      <button class="button primary" type="submit">Unlock</button>
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
    title: "New space",
    subtitle: "A separate vault with no wallets. A wallet can be created later, in the network you need.",
    content: `<form class="form-stack" data-form autocomplete="off">
      <label class="field"><span>Name</span><input name="name" placeholder="default"></label>
      <label class="field"><span>Mnemonic (optional)</span><textarea name="mnemonic" spellcheck="false"></textarea></label>
      <label><input type="checkbox" name="imported_only"> Imported-only, without a mnemonic</label>
      <label class="field"><span>Password</span><input type="password" name="password" required minlength="8"></label>
      <div class="error-text" data-error></div>
      <button class="button primary" type="submit">Create</button>
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
    title: "Rename space",
    content: `<form class="form-stack" data-form>
      <label class="field"><span>Name</span><input name="name" value="${escapeHTML(space.name)}" required></label>
      <div class="error-text" data-error></div><button class="button primary" type="submit">Save</button>
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
    title: "Change the space password",
    subtitle: "The addresses and the recovery phrase stay the same.",
    content: `<form class="form-stack" data-form autocomplete="off">
      <label class="field"><span>Current password</span><input type="password" name="current" required></label>
      <label class="field"><span>New password</span><input type="password" name="next" minlength="8" required></label>
      <label class="field"><span>Repeat the new password</span><input type="password" name="confirmation" minlength="8" required></label>
      <div class="error-text" data-error></div><button class="button primary" type="submit">Re-encrypt the vault</button>
    </form>`,
    onMount(element, close) {
      const form = element.querySelector("[data-form]");
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        const data = new FormData(form);
        if (data.get("next") !== data.get("confirmation")) {
          form.querySelector("[data-error]").textContent = "The new passwords do not match";
          return;
        }
        setBusy(form, true, "Re-encrypting…");
        try {
          await changeSpacePassword(spaceID, data.get("current"), data.get("next"));
          form.reset();
          close();
          toast("Space password changed");
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
    toast("Encrypted backup downloaded");
  } catch (cause) {
    toast(cause.message, "error");
  }
}

function showSecret(title, value) {
  const wrapper = document.createElement("div");
  wrapper.className = "form-stack";
  wrapper.innerHTML = `<div class="secret" data-secret></div><button class="button secondary" type="button" data-copy-secret>Copy</button>`;
  wrapper.querySelector("[data-secret]").textContent = value;
  wrapper.querySelector("[data-copy-secret]").addEventListener("click", async () => {
    await navigator.clipboard.writeText(value);
    toast("Secret copied");
  });
  modal({ title, content: wrapper });
}
