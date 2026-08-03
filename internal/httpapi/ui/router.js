import { closeModal } from "./components/ui.js";

let cleanup = () => {};
let generation = 0;

export async function navigate(path, { replace = false } = {}) {
  if (location.pathname !== path) {
    history[replace ? "replaceState" : "pushState"]({}, "", path);
  }
  await renderRoute();
}

export async function renderRoute() {
  const currentGeneration = ++generation;
  closeModal();
  cleanup();
  cleanup = () => {};
  const controller = new AbortController();
  const route = location.pathname === "/settings" ? "settings" : "dashboard";
  const module =
    route === "settings" ? await import("./views/settings.js") : await import("./views/dashboard.js");
  if (currentGeneration !== generation) {
    controller.abort();
    return;
  }
  const routeCleanup = await module.render(document.querySelector("#app"), controller.signal);
  if (currentGeneration !== generation) {
    controller.abort();
    routeCleanup?.();
    return;
  }
  cleanup = () => {
    controller.abort();
    routeCleanup?.();
  };
}

window.addEventListener("popstate", renderRoute);
