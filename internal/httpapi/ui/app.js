// Imported first and for its side effect: the API client captures the
// capability token out of the URL fragment as it loads, and the router
// navigates with pushState, which would drop the fragment before then.
import "./services/client.js";
import { renderRoute } from "./router.js";

window.addEventListener("unhandledrejection", (event) => {
  if (event.reason?.name !== "AbortError") console.error(event.reason);
});

renderRoute();
