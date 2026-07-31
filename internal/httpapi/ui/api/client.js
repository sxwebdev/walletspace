export class APIError extends Error {
  constructor(message, status, payload) {
    super(message);
    this.name = "APIError";
    this.status = status;
    this.payload = payload;
  }
}

export async function request(path, options = {}) {
  const headers = new Headers(options.headers || {});
  if (options.body !== undefined) {
    headers.set("Content-Type", "application/json");
  }
  const response = await fetch(path, {
    ...options,
    headers,
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
  });
  const type = response.headers.get("content-type") || "";
  const payload = type.includes("json") ? await response.json().catch(() => ({})) : await response.text();
  if (!response.ok) {
    throw new APIError(payload?.error || payload || `HTTP ${response.status}`, response.status, payload);
  }
  return { data: payload, response };
}

export async function streamNDJSON(path, { signal, onValue }) {
  const response = await fetch(path, { signal, headers: { Accept: "application/x-ndjson" } });
  if (!response.ok || !response.body) {
    const payload = await response.json().catch(() => ({}));
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
