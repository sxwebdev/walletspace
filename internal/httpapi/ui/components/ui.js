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
        <button class="button icon" type="button" data-close aria-label="Закрыть">×</button>
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

export function setBusy(form, busy, label = "Сохранение…") {
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

export const shortAddress = (value = "") =>
  value.length > 18 ? `${value.slice(0, 9)}…${value.slice(-7)}` : value;
