export class APIError extends Error {
  readonly status: number
  readonly payload: unknown

  constructor(message: string, status: number, payload: unknown) {
    super(message)
    this.name = "APIError"
    this.status = status
    this.payload = payload
  }
}

const TOKEN_HEADER = "X-Walletspace-Token"
const TOKEN_KEY = "walletspace:token"

export const NO_TOKEN_MESSAGE =
  "This tab has no capability token. Re-open Walletspace using the link it printed on start."

// What the guard answers a request without a usable token, word for word. It
// is the only 401 that means the tab lost its token: a wrong space password
// comes back with the same status and the wallet's own explanation.
const GUARD_REFUSAL = "missing or invalid capability token"

// The process hands the token over in the URL fragment, which the browser
// never sends to a server. It is moved out of the address bar at once, so it
// does not linger in screenshots, and kept in sessionStorage because client
// routing drops the fragment and a reload would otherwise lock the tab out.
function readToken(): string {
  const fragment = new URLSearchParams(location.hash.slice(1))
  const handedOver = fragment.get("token")
  if (!handedOver) {
    try {
      return sessionStorage.getItem(TOKEN_KEY) ?? ""
    } catch {
      return ""
    }
  }
  try {
    sessionStorage.setItem(TOKEN_KEY, handedOver)
  } catch {
    // The token still works for this page load.
  }
  fragment.delete("token")
  const rest = fragment.toString()
  history.replaceState(
    null,
    "",
    location.pathname + location.search + (rest ? `#${rest}` : "")
  )
  return handedOver
}

const token = readToken()

export const hasToken = token !== ""

type RequestOptions = {
  method?: string
  body?: unknown
  // Sent as it is instead of as JSON: an uploaded file that is JSON already.
  rawBody?: Blob
  headers?: HeadersInit
  signal?: AbortSignal
}

function readBody(response: Response): Promise<unknown> {
  const type = response.headers.get("content-type") ?? ""
  if (type.includes("json")) return response.json().catch(() => ({}))
  return response.text().catch(() => "")
}

async function failure(response: Response): Promise<APIError> {
  const payload = await readBody(response)
  const reported =
    typeof payload === "string"
      ? payload.trim()
      : (payload as { error?: string } | null)?.error
  if (response.status === 401 && (!reported || reported === GUARD_REFUSAL)) {
    return new APIError(NO_TOKEN_MESSAGE, 401, payload)
  }
  return new APIError(
    reported || `HTTP ${response.status}`,
    response.status,
    payload
  )
}

async function send(path: string, options: RequestOptions = {}) {
  const headers = new Headers(options.headers)
  if (
    options.body !== undefined ||
    options.rawBody !== undefined ||
    (options.method && options.method !== "GET")
  ) {
    // The guard requires JSON on every non-GET, bodyless ones included.
    headers.set("Content-Type", "application/json")
  }
  headers.set(TOKEN_HEADER, token)
  const response = await fetch(path, {
    method: options.method ?? "GET",
    headers,
    signal: options.signal,
    body:
      options.rawBody ??
      (options.body === undefined ? undefined : JSON.stringify(options.body)),
  })
  if (!response.ok) throw await failure(response)
  return response
}

export async function request<T>(
  path: string,
  options: RequestOptions = {}
): Promise<T> {
  const response = await send(path, options)
  return (await readBody(response)) as T
}

export async function requestWithResponse<T>(
  path: string,
  options: RequestOptions = {}
): Promise<{ data: T; response: Response }> {
  const response = await send(path, options)
  return { data: (await readBody(response)) as T, response }
}

export async function download(path: string, options: RequestOptions = {}) {
  const response = await send(path, options)
  return response.blob()
}

export async function streamNDJSON<T>(
  path: string,
  { signal, onValue }: { signal?: AbortSignal; onValue: (value: T) => void }
) {
  const response = await send(path, {
    signal,
    headers: { Accept: "application/x-ndjson" },
  })
  if (!response.body) {
    throw new APIError("The stream returned no body", response.status, null)
  }
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ""
  for (;;) {
    const { value, done } = await reader.read()
    buffer += decoder.decode(value ?? new Uint8Array(), { stream: !done })
    const lines = buffer.split("\n")
    buffer = lines.pop() ?? ""
    for (const line of lines) {
      if (line.trim()) onValue(JSON.parse(line) as T)
    }
    if (done) break
  }
  if (buffer.trim()) onValue(JSON.parse(buffer) as T)
}

export function revisionHeaders(revision?: string): HeadersInit {
  return revision ? { "If-Match": `"${revision}"` } : {}
}

export function errorMessage(error: unknown): string {
  if (error instanceof Error) return error.message
  return String(error)
}
