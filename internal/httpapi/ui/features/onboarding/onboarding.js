import { createSpace } from "../../api/spaces.js";
import { navigate } from "../../router.js";
import { modal, setBusy, toast } from "../../components/ui.js";

export function renderOnboarding(root, signal) {
  root.innerHTML = `
    <main class="onboarding">
      <section class="onboarding-copy">
        <span class="brand-mark" aria-hidden="true">W</span>
        <p class="eyebrow" style="margin-top:24px">Your keys. Your spaces.</p>
        <h1><span class="gradient-text">Один walletspace.</span><br>Все ваши сети.</h1>
        <p>Создайте зашифрованную коллекцию кошельков. Можно начать с новой recovery phrase или восстановить свою.</p>
        <a class="button" href="/settings" data-settings>Настроить RPC до старта</a>
      </section>
      <section class="onboarding-card">
        <p class="eyebrow">Первый запуск</p>
        <h2>Создать space</h2>
        <p class="muted">Название и мнемоника необязательны. Если оставить их пустыми, будет создан <strong>default</strong> с новой фразой.</p>
        <form class="form-stack" data-form autocomplete="off">
          <label class="field">
            <span>Название space</span>
            <input name="name" placeholder="default" maxlength="80">
          </label>
          <label class="field">
            <span>Своя мнемоника <small class="muted">(необязательно)</small></span>
            <textarea name="mnemonic" placeholder="Вставьте BIP39-фразу для восстановления" autocomplete="off" spellcheck="false"></textarea>
            <small class="hint">Пустое поле означает: сгенерировать новую 24-word phrase.</small>
          </label>
          <details>
            <summary>Advanced: BIP39-пасфраза</summary>
            <label class="field" style="margin-top:12px">
              <span>BIP39-пасфраза</span>
              <input type="password" name="bip39_passphrase" autocomplete="new-password">
              <small class="hint">Это часть derivation и она меняет адреса. Это не пароль space.</small>
            </label>
          </details>
          <label class="field">
            <span>Пароль space</span>
            <input type="password" name="password" required minlength="8" autocomplete="new-password">
          </label>
          <label class="field">
            <span>Повторите пароль</span>
            <input type="password" name="confirmation" required minlength="8" autocomplete="new-password">
          </label>
          <div class="error-text" data-error role="alert"></div>
          <button class="button primary" type="submit">Создать space</button>
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
      error.textContent = "Пароли не совпадают.";
      return;
    }
    setBusy(form, true, "Шифруем vault…");
    try {
      const result = await createSpace({
        name: values.get("name"),
        mnemonic: values.get("mnemonic"),
        bip39_passphrase: values.get("bip39_passphrase"),
        password,
        first: true,
      }, signal);
      form.reset();
      if (result.mnemonic_generated) {
        showRecovery(result.mnemonic, () => navigate("/", { replace: true }));
      } else {
        toast("Space восстановлен");
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
    title: "Сохраните recovery phrase",
    subtitle: "Она показывается сейчас, но останется доступна после unlock.",
    wide: true,
    content: `
      <div class="form-stack">
        <div class="notice danger">Любой, у кого есть эта фраза и BIP39-пасфраза, контролирует derived-аккаунты. Не отправляйте её в облако или мессенджер.</div>
        <div class="secret" data-secret></div>
        <button class="button secondary" type="button" data-copy>Копировать</button>
        <label><input type="checkbox" data-confirm> Я сохранил фразу в безопасном месте</label>
        <button class="button primary" type="button" data-continue disabled>Перейти в Walletspace</button>
      </div>`,
    onMount(element, close) {
      element.querySelector("[data-secret]").textContent = mnemonic;
      element.querySelector("[data-copy]").addEventListener("click", async () => {
        await navigator.clipboard.writeText(mnemonic);
        toast("Recovery phrase скопирована");
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
