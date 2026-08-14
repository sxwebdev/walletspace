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

// What the guard answers a request that arrived without a usable token, word
// for word. It is the only 401 that means the tab has lost its token: a
// password the user got wrong, or did not give at all, comes back with the same
// status and the wallet's own description of what went wrong.
const GUARD_REFUSAL = "missing or invalid capability token";

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

// Every call the page makes carries the token, so building the headers is one
// function rather than a line each caller has to remember. The backup download
// is what happens when it is not: it went out on a bare fetch, without the
// header, and the guard turned the wallet's own request away.
function authorized(options = {}) {
  const headers = new Headers(options.headers || {});
  if (options.body !== undefined) {
    headers.set("Content-Type", "application/json");
  }
  headers.set(TOKEN_HEADER, token);
  return headers;
}

// The server labels what it sends, so the body is read the way it was labelled.
// Asking for JSON regardless — which the download and the stream used to do —
// throws away the only description a plain-text failure carries.
function readBody(response) {
  const type = response.headers.get("content-type") || "";
  if (type.includes("json")) return response.json().catch(() => ({}));
  return response.text().catch(() => "");
}

// One place where a refused response becomes an APIError, because there were
// three of them and they disagreed: about how to read a body that is not JSON,
// and about what a 401 means.
async function failure(response) {
  const payload = await readBody(response);
  const reported = typeof payload === "string" ? payload.trim() : payload?.error;
  // The guard's own refusal is the only one the relaunch advice fits, and it is
  // the only 401 that says nothing a user can act on. Everything else the
  // wallet answers 401 with — a wrong space password, a step-up nobody
  // attempted — is a sentence written for the person reading it, and telling
  // them their tab lost its token instead sends them to reopen a wallet that
  // was never the problem.
  if (response.status === 401 && (!reported || reported === GUARD_REFUSAL)) {
    return new APIError(NO_TOKEN, 401, payload);
  }
  return new APIError(reported || `HTTP ${response.status}`, response.status, payload);
}

// Every call leaves through here: the token on it, the caller's own headers
// and method kept, an abort signal honoured, a body serialised only when there
// is one, and a refusal raised in one place. What the three callers below do
// with a response that is fine is the only thing they are free to differ on —
// the download was allowed to differ on the rest, and it drifted.
async function send(path, options) {
  const response = await fetch(path, {
    ...options,
    headers: authorized(options),
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
  });
  if (!response.ok) throw await failure(response);
  return response;
}

export async function request(path, options = {}) {
  const response = await send(path, options);
  return { data: await readBody(response), response };
}

// download is request for a response that is a file: the body reaches the
// caller as a Blob rather than parsed.
export async function download(path, options = {}) {
  const response = await send(path, options);
  return response.blob();
}

export async function streamNDJSON(path, { signal, onValue }) {
  const response = await send(path, { signal, headers: { Accept: "application/x-ndjson" } });
  if (!response.body) {
    throw new APIError("The stream returned no body", response.status, null);
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
