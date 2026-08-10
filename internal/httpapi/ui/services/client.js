export class APIError extends Error {
  constructor(message, status, payload) {
    super(message);
    this.name = "APIError";
    this.status = status;
    this.payload = payload;
  }
}

const TOKEN_HEADER = "X-Walletspace-Token";
const TOKEN_KEY = "walletspace:token";
const NO_TOKEN =
  "This tab has no capability token. Re-open Walletspace using the link it printed on start.";

// The process hands the token over in the URL fragment, which the browser keeps
// to itself: it is never sent to a server, never lands in a Referer header and
// never reaches an access log the way a query parameter would.
//
// It is moved out of the address bar immediately so it does not linger in
// screenshots or over a shoulder, and mirrored into sessionStorage because the
// router navigates with pushState — which drops the fragment — and a reload
// would otherwise lock the tab out of its own API. sessionStorage is per-tab
// and dies with it; against same-origin script injection it is no weaker than a
// variable, since injected script can simply call the API itself.
function readToken() {
  const fragment = new URLSearchParams(location.hash.slice(1));
  const handedOver = fragment.get("token");
  if (!handedOver) return sessionStorage.getItem(TOKEN_KEY) || "";

  sessionStorage.setItem(TOKEN_KEY, handedOver);
  fragment.delete("token");
  const rest = fragment.toString();
  history.replaceState(null, "", location.pathname + location.search + (rest ? `#${rest}` : ""));
  return handedOver;
}

const token = readToken();

export async function request(path, options = {}) {
  const headers = new Headers(options.headers || {});
  if (options.body !== undefined) {
    headers.set("Content-Type", "application/json");
  }
  headers.set(TOKEN_HEADER, token);
  const response = await fetch(path, {
    ...options,
    headers,
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
  });
  const type = response.headers.get("content-type") || "";
  const payload = type.includes("json") ? await response.json().catch(() => ({})) : await response.text();
  if (!response.ok) {
    if (response.status === 401) {
      throw new APIError(NO_TOKEN, 401, payload);
    }
    throw new APIError(payload?.error || payload || `HTTP ${response.status}`, response.status, payload);
  }
  return { data: payload, response };
}

export async function streamNDJSON(path, { signal, onValue }) {
  const response = await fetch(path, {
    signal,
    headers: { Accept: "application/x-ndjson", [TOKEN_HEADER]: token },
  });
  if (!response.ok || !response.body) {
    const payload = await response.json().catch(() => ({}));
    if (response.status === 401) {
      throw new APIError(NO_TOKEN, 401, payload);
    }
    throw new APIError(payload.error || `HTTP ${response.status}`, response.status, payload);
  }
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  while (true) {
    const { value, done } = await reader.read();
    buffer += decoder.decode(value || new Uint8Array(), { stream: !done });
    const lines = buffer.split("\n");
    buffer = lines.pop() || "";
    for (const line of lines) {
      if (line.trim()) onValue(JSON.parse(line));
    }
    if (done) break;
  }
  if (buffer.trim()) onValue(JSON.parse(buffer));
}

export function revisionHeaders(revision) {
  return revision ? { "If-Match": `"${revision}"` } : {};
}
