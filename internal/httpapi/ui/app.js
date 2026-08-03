import { renderRoute } from "./router.js";

window.addEventListener("unhandledrejection", (event) => {
  if (event.reason?.name !== "AbortError") console.error(event.reason);
});

renderRoute();
