import { createSpace } from "../../api/spaces.js";
import { navigate } from "../../router.js";
import { update } from "../../state/store.js";
import { modal, setBusy, toast } from "../../components/ui.js";

export function renderOnboarding(root, signal) {
  root.innerHTML = `
    <main class="onboarding">
      <section class="onboarding-copy">
        <span class="brand-mark" aria-hidden="true">W</span>
        <p class="eyebrow" style="margin-top:24px">Your keys. Your spaces.</p>
        <h1><span class="gradient-text">One walletspace.</span><br>All of your networks.</h1>
        <p>Create an encrypted collection of wallets. Start from a new recovery phrase, or restore your own.</p>
        <a class="button" href="/settings" data-settings>Set up RPC before starting</a>
      </section>
      <section class="onboarding-card">
        <p class="eyebrow">First run</p>
        <h2>Create a Secure Space</h2>
        <form class="form-stack" data-form autocomplete="off">
          <label class="field">
            <span>Space name <small class="muted">(optional)</small></span>
            <input name="name" value="default" maxlength="80">
          </label>
          <label class="field">
            <span>Your own mnemonic <small class="muted">(optional)</small></span>
            <textarea name="mnemonic" placeholder="Paste a BIP39 phrase to restore from" autocomplete="off" spellcheck="false"></textarea>
            <small class="hint">Leaving this empty means: generate a new 24-word phrase.</small>
          </label>
          <details>
            <summary>Advanced: BIP39 passphrase</summary>
            <label class="field" style="margin-top:12px">
              <span>BIP39 passphrase</span>
              <input type="password" name="bip39_passphrase" autocomplete="new-password">
              <small class="hint">This is part of the derivation and changes the addresses. It is not the space password.</small>
            </label>
          </details>
          <label class="field">
            <span>Space password</span>
            <input type="password" name="password" required minlength="8" autocomplete="new-password">
          </label>
          <label class="field">
            <span>Repeat the password</span>
            <input type="password" name="confirmation" required minlength="8" autocomplete="new-password">
          </label>
          <div class="error-text" data-error role="alert"></div>
          <button class="button primary" type="submit">Create a Secure Space</button>
        </form>
      </section>
    </main>`;

  root.querySelector("[data-settings]").addEventListener("click", (event) => {
    event.preventDefault();
    navigate("/settings");
  });
  const form = root.querySelector("[data-form]");
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const values = new FormData(form);
    const password = values.get("password");
    const confirmation = values.get("confirmation");
    const error = form.querySelector("[data-error]");
    error.textContent = "";
    if (password !== confirmation) {
      error.textContent = "The passwords do not match.";
      return;
    }
    setBusy(form, true, "Encrypting the vault…");
    try {
      const result = await createSpace({
        name: values.get("name"),
        mnemonic: values.get("mnemonic"),
        bip39_passphrase: values.get("bip39_passphrase"),
        password,
        first: true,
      }, signal);
      update({
        currentSpaceID: result.space.id,
      });
      form.reset();
      if (result.mnemonic_generated) {
        showRecovery(result.mnemonic, () => navigate("/", { replace: true }));
      } else {
        toast("Space restored");
        await navigate("/", { replace: true });
      }
    } catch (cause) {
      error.textContent = cause.message;
    } finally {
      setBusy(form, false);
    }
  });
}

function showRecovery(mnemonic, done) {
  modal({
    title: "Save the recovery phrase",
    subtitle: "It is shown now, and stays available after an unlock.",
    wide: true,
    content: `
      <div class="form-stack">
        <div class="notice danger">Anyone holding this phrase and the BIP39 passphrase controls the derived accounts. Do not send it to a cloud service or a messenger.</div>
        <div class="secret" data-secret></div>
        <button class="button secondary" type="button" data-copy>Copy</button>
        <label><input type="checkbox" data-confirm> I have stored the phrase somewhere safe</label>
        <button class="button primary" type="button" data-continue disabled>Go to Walletspace</button>
      </div>`,
    onMount(element, close) {
      element.querySelector("[data-secret]").textContent = mnemonic;
      element.querySelector("[data-copy]").addEventListener("click", async () => {
        await navigator.clipboard.writeText(mnemonic);
        toast("Recovery phrase copied");
      });
      const checkbox = element.querySelector("[data-confirm]");
      const button = element.querySelector("[data-continue]");
      checkbox.addEventListener("change", () => {
        button.disabled = !checkbox.checked;
      });
      button.addEventListener("click", () => {
        element.querySelector("[data-secret]").textContent = "";
        mnemonic = "";
        close();
        done();
      });
    },
  });
}
