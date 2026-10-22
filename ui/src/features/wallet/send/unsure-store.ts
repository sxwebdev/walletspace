import { sessionGet, sessionSet } from "@/lib/storage"

// Attempts that got no clear answer, kept for the life of the tab: a remount
// the user did not choose — the auto-lock, a reload — must bring one back with
// its idempotency key rather than leave the next attempt to a new key. What
// is kept is the request as it was signed: account and asset ids, addresses,
// amounts, the approved fee and the key. No key material.
//
// reserved says whether a request under the key may have passed the
// password gate. The server checks the send confirmation before it records
// the key, so an attempt that never got past the gate reserved nothing and is
// dropped on restore.
export function unsureStoreKey(
  kind: "send" | "wrap" | "close" | "swap",
  spaceId: string,
  accountId: string,
  networkId: string
) {
  return `walletspace:unsure:${kind}:${spaceId}:${accountId}:${networkId}`
}

export function loadUnsure<T>(key: string): T | null {
  const raw = sessionGet(key)
  if (!raw) return null
  try {
    return JSON.parse(raw) as T
  } catch {
    return null
  }
}

export function saveUnsure(key: string, value: unknown) {
  sessionSet(key, JSON.stringify(value))
}

type Keyed = { key?: string; batch?: { key: string } }

// Compare-and-delete: removes only what belongs to idempotencyKey. Another
// screen may have written its own attempt to the same slot since, and a late
// answer here must not erase it. A slot holding several batches loses just
// the one.
export function clearUnsure(key: string, idempotencyKey: string) {
  const stored = loadUnsure<Keyed | Keyed[]>(key)
  if (!stored) return
  if (Array.isArray(stored)) {
    const rest = stored.filter((item) => item.batch?.key !== idempotencyKey)
    if (rest.length === stored.length) return
    sessionSet(key, rest.length > 0 ? JSON.stringify(rest) : null)
    return
  }
  if (stored.key === idempotencyKey) sessionSet(key, null)
}

// Writes a single attempt only into an empty slot or over its own earlier
// record: a late write from a screen that was left must not replace the
// attempt another screen wrote since.
export function saveUnsureOwned<T extends { key: string }>(
  key: string,
  value: T
) {
  const stored = loadUnsure<Keyed>(key)
  if (stored && !Array.isArray(stored) && stored.key !== value.key) return
  saveUnsure(key, value)
}

// Adds or replaces one batch in a slot of several, leaving the others.
export function upsertUnsureBatch(
  key: string,
  entry: { batch?: { key: string } }
) {
  const stored = loadUnsure<Keyed[]>(key)
  const list = Array.isArray(stored) ? stored : []
  const rest = list.filter((item) => item.batch?.key !== entry.batch?.key)
  saveUnsure(key, [entry, ...rest])
}
