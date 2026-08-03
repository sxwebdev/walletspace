export function escapeHTML(value = "") {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");
}

export function toast(message, type = "") {
  const root = document.querySelector("#toast-root");
  const item = document.createElement("div");
  item.className = `toast ${type}`;
  item.textContent = message;
  root.append(item);
  setTimeout(() => item.remove(), 4200);
}

let activeModalClose;

export function closeModal() {
  activeModalClose?.();
}

export function modal({ title, subtitle = "", content, wide = false, onMount }) {
  closeModal();
  const root = document.querySelector("#modal-root");
  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <section class="modal ${wide ? "wide" : ""}" role="dialog" aria-modal="true" aria-labelledby="modal-title">
      <header class="modal-header">
        <div>
          <h2 id="modal-title">${escapeHTML(title)}</h2>
          ${subtitle ? `<p class="muted">${escapeHTML(subtitle)}</p>` : ""}
        </div>
        <button class="button icon" type="button" data-close aria-label="Close">×</button>
      </header>
      <div data-content></div>
    </section>`;
  const close = () => {
    document.removeEventListener("keydown", onKey);
    backdrop.remove();
    if (activeModalClose === close) activeModalClose = undefined;
  };
  activeModalClose = close;
  const onKey = (event) => {
    if (event.key === "Escape") close();
  };
  backdrop.querySelector("[data-content]").append(
    typeof content === "string" ? document.createRange().createContextualFragment(content) : content,
  );
  backdrop.querySelector("[data-close]").addEventListener("click", close);
  backdrop.addEventListener("click", (event) => {
    if (event.target === backdrop) close();
  });
  document.addEventListener("keydown", onKey);
  root.replaceChildren(backdrop);
  queueMicrotask(() => {
    if (!backdrop.isConnected || activeModalClose !== close) return;
    backdrop.querySelector("input, textarea, select, button")?.focus();
    onMount?.(backdrop, close);
  });
  return { element: backdrop, close };
}

export function setBusy(form, busy, label = "Saving…") {
  const button = form.querySelector('[type="submit"]');
  if (!button) return;
  if (busy) {
    button.dataset.label = button.textContent;
    button.textContent = label;
    button.disabled = true;
  } else {
    button.textContent = button.dataset.label || button.textContent;
    button.disabled = false;
  }
}

// How long a revealed seed or private key stays on screen, and how long a copy
// of it is left in the system clipboard.
const SECRET_VISIBLE_MS = 90_000;
const CLIPBOARD_CLEAR_MS = 45_000;

// secretBlock renders a revealed secret and then takes it back.
//
// A recovery phrase left on screen and a copy of it left in the clipboard both
// outlive the moment the user needed them: clipboard managers persist and index
// their history to disk, and the vault's own auto-lock does nothing about a
// secret that has already been rendered. So the display expires on its own and
// the clipboard is cleared behind it.
export function secretBlock(value, label = "Secret") {
  const wrapper = document.createElement("div");
  wrapper.className = "form-stack";
  wrapper.innerHTML =
    `<div class="secret" data-secret></div>` +
    `<button class="button secondary" type="button" data-copy-secret>Copy</button>` +
    `<p class="muted" data-secret-note></p>`;

  const field = wrapper.querySelector("[data-secret]");
  const note = wrapper.querySelector("[data-secret-note]");
  field.textContent = value;
  note.textContent = `${label} hides itself in ${Math.round(SECRET_VISIBLE_MS / 1000)} seconds.`;

  let clipboardTimer;
  let copied = false;
  // Only clear what we put there: if the user copied something else since,
  // overwriting it would be destructive.
  const clearClipboard = async () => {
    clearTimeout(clipboardTimer);
    if (!copied) return;
    copied = false;
    const current = await navigator.clipboard.readText().catch(() => null);
    if (current === value) await navigator.clipboard.writeText("").catch(() => {});
  };
  const wipe = () => {
    field.textContent = "";
    wrapper.querySelector("[data-copy-secret]")?.remove();
    note.textContent = `${label} was hidden. Open this dialog again if you still need it.`;
    // Cleared now rather than left to its timer. Cancelling the pending clear
    // here — which is what this used to do — meant closing the dialog left the
    // phrase in the clipboard, and in the clipboard manager's history, for good.
    void clearClipboard();
  };
  const hideTimer = setTimeout(wipe, SECRET_VISIBLE_MS);

  wrapper.querySelector("[data-copy-secret]").addEventListener("click", async () => {
    await navigator.clipboard.writeText(value);
    copied = true;
    toast(`${label} copied — the clipboard is cleared in ${Math.round(CLIPBOARD_CLEAR_MS / 1000)}s`);
    clearTimeout(clipboardTimer);
    clipboardTimer = setTimeout(clearClipboard, CLIPBOARD_CLEAR_MS);
  });

  // Closing the dialog must not leave the timers, or the secret, alive.
  wrapper.addEventListener("secret-dismissed", () => {
    clearTimeout(hideTimer);
    wipe();
  });

  return wrapper;
}

export const shortAddress = (value = "") =>
  value.length > 18 ? `${value.slice(0, 9)}…${value.slice(-7)}` : value;

// Every character, grouped in fours so they can be compared by eye.
//
// shortAddress is fine for a list, but not on the screen that precedes a
// signature: it keeps 9 characters and the last 7, and address-poisoning works
// by matching exactly those. The middle is the part that differs, so it is the
// part that has to be visible. The chunks carry no whitespace between them, so
// selecting the address still copies it unbroken.
export function addressGroups(value = "") {
  const text = String(value);
  const chunks = text.match(/.{1,4}/g) || [];
  return chunks.map((chunk) => `<span class="addr-chunk">${escapeHTML(chunk)}</span>`).join("");
}
