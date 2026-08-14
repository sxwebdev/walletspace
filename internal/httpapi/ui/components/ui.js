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

// The dialogs on screen are a stack rather than a slideshow.
//
// Some questions are asked on top of another one: the spending confirmation is
// answered against the transfer summary it belongs to, so taking that summary
// off the screen to ask for the password would leave the user authorising
// numbers they can no longer read. It also detached the dialog underneath,
// which goes on writing into the error box of a node that is no longer in the
// document — a refused transfer that reported nothing anywhere.
const openModals = [];

// One key listener for the whole stack. One per dialog meant a single Escape
// reached all of them and closed the lot.
function onModalKey(event) {
  if (event.key === "Escape") closeModal();
}

// Two dialogs on screen cannot share a heading id, or aria-labelledby resolves
// both of them to whichever one came first in the document.
let modalSequence = 0;

// closeModal closes the dialog on top — the one the user is being asked about.
export function closeModal() {
  openModals.at(-1)?.close();
}

// closeModals empties the stack. A dialog left behind by a route change would
// go on asking about a screen that is no longer there.
export function closeModals() {
  while (openModals.length) closeModal();
}

export function modal({
  title, subtitle = "", content, wide = false, stack = false, onMount, onClose,
}) {
  // A dialog that does not ask to be stacked replaces what is on screen, which
  // is what every caller except the spending confirmation means by opening one.
  if (!stack) closeModals();
  const root = document.querySelector("#modal-root");
  const covered = openModals.at(-1);
  const restoreFocus = document.activeElement;
  const titleID = `modal-title-${++modalSequence}`;
  const backdrop = document.createElement("div");
  backdrop.className = "modal-backdrop";
  backdrop.innerHTML = `
    <section class="modal ${wide ? "wide" : ""}" role="dialog" aria-modal="true" aria-labelledby="${titleID}">
      <header class="modal-header">
        <div>
          <h2 id="${titleID}">${escapeHTML(title)}</h2>
          ${subtitle ? `<p class="muted">${escapeHTML(subtitle)}</p>` : ""}
        </div>
        <button class="button icon" type="button" data-close aria-label="Close">×</button>
      </header>
      <div data-content></div>
    </section>`;
  const close = () => {
    const index = openModals.findIndex((item) => item.close === close);
    // Already closed. A caller holding on to close() must not fire onClose a
    // second time, and closeModals() would never finish if the entry stayed.
    if (index === -1) return;
    openModals.splice(index, 1);
    // Before it leaves the document, so that anything holding a secret can take
    // it back. Dispatched at each block rather than at the dialog, because an
    // event at the dialog travels upwards, away from them.
    for (const block of backdrop.querySelectorAll("[data-secret-block]")) {
      block.dispatchEvent(new CustomEvent("secret-dismissed"));
    }
    backdrop.remove();
    if (!openModals.length) document.removeEventListener("keydown", onModalKey);
    // Only the dialog on top holds the focus and blocks the input of the one
    // below it, so closing one from underneath leaves both where they are.
    const uncovered = index === openModals.length ? openModals.at(-1) : undefined;
    if (uncovered) {
      uncovered.backdrop.removeAttribute("inert");
      focusFirstControl(uncovered.backdrop);
    } else if (!openModals.length && restoreFocus?.isConnected) {
      restoreFocus.focus();
    }
    // Fires however the dialog went away — the button, Escape, a click on the
    // backdrop, or another dialog taking its place. A caller waiting on an
    // answer needs to hear about the ways of not giving one too.
    onClose?.();
  };
  const entry = { backdrop, close };
  backdrop.querySelector("[data-content]").append(
    typeof content === "string" ? document.createRange().createContextualFragment(content) : content,
  );
  backdrop.querySelector("[data-close]").addEventListener("click", close);
  backdrop.addEventListener("click", (event) => {
    // Backdrops are siblings, so this only ever fires for the one that was
    // clicked; a stacked dialog cannot dismiss the dialog underneath it.
    if (event.target === backdrop) close();
  });
  if (!openModals.length) document.addEventListener("keydown", onModalKey);
  // The dialog underneath stays on screen to be read and stops taking input:
  // it is not the question being asked, and tabbing into the form behind the
  // one that is would answer the wrong dialog.
  covered?.backdrop.setAttribute("inert", "");
  openModals.push(entry);
  root.append(backdrop);
  queueMicrotask(() => {
    if (!backdrop.isConnected) return;
    // Anything opened above this one in the meantime keeps the focus. onMount
    // still runs: the dialog is on screen, and its handlers have to be bound
    // before it is uncovered again.
    if (openModals.at(-1) === entry) focusFirstControl(backdrop);
    onMount?.(backdrop, close);
  });
  return { element: backdrop, close };
}

function focusFirstControl(backdrop) {
  backdrop.querySelector("input, textarea, select, button")?.focus();
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

  // Closing the dialog must not leave the timers, or the secret, alive. The
  // attribute is how modal() finds this block on the way out: an event
  // dispatched at the dialog would travel up and away from the wrapper, never
  // reaching the listener below. It went unnoticed because nothing dispatched
  // the event at all, so a phrase read and dismissed stayed on its detached
  // node and in the clipboard until the timers happened to fire.
  wrapper.dataset.secretBlock = "";
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
