// sessionStorage is per tab and dies with it, which is the lifetime every key
// here wants. It can throw (private window, blocked site data); nothing that
// lives here is essential, so a failure just means it is not remembered.
export function sessionGet(key: string): string | null {
  try {
    return sessionStorage.getItem(key)
  } catch {
    return null
  }
}

export function sessionSet(key: string, value: string | null) {
  try {
    if (value === null) sessionStorage.removeItem(key)
    else sessionStorage.setItem(key, value)
  } catch {
    // Not remembered.
  }
}
