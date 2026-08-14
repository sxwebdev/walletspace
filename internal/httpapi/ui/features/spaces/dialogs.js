import {
  changeSpacePassword,
  confirmSend,
  createSpace,
  downloadBackup,
  renameSpace,
  revealMnemonic,
  unlockSpace,
} from "../../services/spaces.js";
import { escapeHTML, modal, secretBlock, setBusy, toast } from "../../components/ui.js";

// The backend's word for "ask the person at the keyboard and come back". Matched
// on the code rather than the message, which is prose and will be reworded.
const SEND_CONFIRMATION_REQUIRED = "send_confirmation_required";

// withSendConfirmation runs a signing request, and if the wallet asks for the
// password first, gets it and runs the identical request again.
//
// The retry is deliberately the same call with the same idempotency key: the
// confirmation authorises the transfer that was already described, it does not
// start a different one. Anything else — a fresh key, a re-priced fee — would
// mean the user confirmed one transfer and signed another.
export async function withSendConfirmation(spaceID, attempt) {
  try {
    return await attempt();
  } catch (cause) {
    if (cause?.payload?.code !== SEND_CONFIRMATION_REQUIRED) throw cause;
    await askToConfirmSending(spaceID);
    return attempt();
  }
}

// What the caller is told when the dialog goes away without an accepted
// password. It is a promise nothing was signed, so it must never be given for
// a check that did in fact go through.
const NOT_CONFIRMED = "The transfer was not confirmed, so nothing was signed.";

// askToConfirmSending resolves once the password has been accepted, and
// rejects if the dialog is dismissed — which has to leave nothing signed.
//
// The check is an Argon2id derivation, so it runs long enough to be dismissed
// while it is still in flight, and that is the one moment when this answer and
// the state on the server can disagree. Dismissing cancels the request, and
// what the dismissal then reports is decided by what the request did rather
// than by a flag set after it — a check that had already come back opened the
// spending window, so the caller hears that it did, instead of a promise that
// nothing was confirmed while the window quietly stands open for five minutes.
function askToConfirmSending(spaceID) {
  return new Promise((resolve, reject) => {
    const controller = new AbortController();
    let inFlight;
    modal({
      title: "Confirm with your space password",
      subtitle: "Unlocking this space opened it. Spending from it is a separate answer.",
      // Stacked over the transfer it authorises. The summary of what is about
      // to be signed is what the password is being given for, so it stays on
      // screen, and the dialog underneath stays attached to report its own
      // outcome afterwards.
      stack: true,
      content: `<form class="form-stack" data-form>
        <div class="notice">Nothing has been signed yet. The transfer goes ahead exactly as it was shown, once the password checks out.</div>
        <label class="field"><span>Space password</span><input name="password" type="password" required autocomplete="current-password"></label>
        <div class="error-text" data-error></div>
        <button class="button primary" type="submit">Confirm and send</button>
      </form>`,
      onMount(element, close) {
        const form = element.querySelector("[data-form]");
        form.addEventListener("submit", async (event) => {
          event.preventDefault();
          setBusy(form, true, "Checking…");
          try {
            // Kept where a dismissal can find it: it is what the answer to
            // this dialog is decided on once the check is on its way.
            inFlight = confirmSend(spaceID, new FormData(form).get("password"), controller.signal);
            const grant = await inFlight;
            form.reset();
            close();
            resolve(grant);
          } catch (cause) {
            form.querySelector("[data-error]").textContent = cause.message;
          } finally {
            setBusy(form, false);
          }
        });
      },
      onClose() {
        // The window must not outlive the question. Aborting is what keeps a
        // check that is still travelling from being applied to a dialog the
        // user has already taken back.
        controller.abort();
        if (!inFlight) {
          reject(new Error(NOT_CONFIRMED));
          return;
        }
        inFlight.then(resolve, () => reject(new Error(NOT_CONFIRMED)));
      },
    });
  });
}

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
      <label class="field"><span>Password</span><input type="password" name="password" required minlength="12"></label>
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

// The recovery phrase is every account in the space at once, so an unlocked
// tab is not enough on its own — the password is asked for again here.
export function showMnemonic(spaceID) {
  modal({
    title: "Reveal the recovery phrase",
    subtitle: "The phrase is full control over every wallet in this space.",
    content: `<form class="form-stack" data-form>
      <div class="notice danger">Anyone who reads this phrase can spend everything in the space. Do not photograph it and do not paste it into a chat.</div>
      <label class="field"><span>Space password</span><input name="password" type="password" required autocomplete="current-password"></label>
      <div class="error-text" data-error></div>
      <button class="button danger" type="submit">Reveal the phrase</button>
    </form>`,
    onMount(element) {
      const form = element.querySelector("[data-form]");
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        setBusy(form, true, "Checking…");
        try {
          const phrase = await revealMnemonic(spaceID, new FormData(form).get("password"));
          form.replaceWith(secretBlock(phrase, "Recovery phrase"));
        } catch (cause) {
          form.querySelector("[data-error]").textContent = cause.message;
        } finally {
          setBusy(form, false);
          if (form.isConnected) form.reset();
        }
      });
    },
  });
}

export function showChangePassword(spaceID) {
  modal({
    title: "Change the space password",
    subtitle: "The addresses and the recovery phrase stay the same.",
    content: `<form class="form-stack" data-form autocomplete="off">
      <label class="field"><span>Current password</span><input type="password" name="current" required></label>
      <label class="field"><span>New password</span><input type="password" name="next" minlength="12" required></label>
      <label class="field"><span>Repeat the new password</span><input type="password" name="confirmation" minlength="12" required></label>
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

// The backup is the whole vault in a file, which is worth more to whoever ends
// up with it than any single exported key: it can be attacked offline, for as
// long as they like. So it asks for the password, exactly as the phrase does.
export function backupSpace(space) {
  modal({
    title: "Download an encrypted backup",
    subtitle: "The file holds the vault as it is stored — encrypted with this password.",
    content: `<form class="form-stack" data-form>
      <div class="notice">Keep the file where the password is not written down beside it. Anyone who has both has the space.</div>
      <label class="field"><span>Space password</span><input name="password" type="password" required autocomplete="current-password"></label>
      <div class="error-text" data-error></div>
      <button class="button primary" type="submit">Download the backup</button>
    </form>`,
    onMount(element, close) {
      const form = element.querySelector("[data-form]");
      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        setBusy(form, true, "Checking…");
        try {
          await downloadBackup(space.id, space.name, new FormData(form).get("password"));
          form.reset();
          close();
          toast("Encrypted backup downloaded");
        } catch (cause) {
          form.querySelector("[data-error]").textContent = cause.message;
        } finally {
          setBusy(form, false);
        }
      });
    },
  });
}

function showSecret(title, value) {
  modal({ title, content: secretBlock(value, title) });
}
