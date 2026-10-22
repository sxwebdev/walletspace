import * as React from "react"

export type Theme = "dark" | "light" | "system"
type ResolvedTheme = "dark" | "light"

type ThemeProviderState = {
  theme: Theme
  resolvedTheme: ResolvedTheme
  setTheme: (theme: Theme) => void
}

const COLOR_SCHEME_QUERY = "(prefers-color-scheme: dark)"
const STORAGE_KEY = "walletspace:theme"

const ThemeProviderContext = React.createContext<ThemeProviderState | null>(
  null
)

function isTheme(value: string | null): value is Theme {
  return value === "dark" || value === "light" || value === "system"
}

function systemTheme(): ResolvedTheme {
  return window.matchMedia(COLOR_SCHEME_QUERY).matches ? "dark" : "light"
}

// Storage can throw in a private window or with site data blocked; the theme
// is a convenience and must never break the wallet.
function readStored(): Theme {
  try {
    const stored = localStorage.getItem(STORAGE_KEY)
    return isTheme(stored) ? stored : "system"
  } catch {
    return "system"
  }
}

function writeStored(theme: Theme) {
  try {
    localStorage.setItem(STORAGE_KEY, theme)
  } catch {
    // Not persisted; the choice still applies to this tab.
  }
}

// The CSP forbids inline <style>, so transitions are paused with a class that
// index.css defines rather than an injected stylesheet.
function applyTheme(resolved: ResolvedTheme) {
  const root = document.documentElement
  root.classList.add("theme-switching")
  root.classList.remove("light", "dark")
  root.classList.add(resolved)
  root.style.colorScheme = resolved
  // Reading a computed style flushes the class change before transitions resume.
  void window.getComputedStyle(root).opacity
  requestAnimationFrame(() =>
    requestAnimationFrame(() => root.classList.remove("theme-switching"))
  )
}

export function ThemeProvider({ children }: { children: React.ReactNode }) {
  const [theme, setThemeState] = React.useState<Theme>(readStored)
  const [system, setSystem] = React.useState<ResolvedTheme>(systemTheme)
  const resolvedTheme = theme === "system" ? system : theme

  React.useEffect(() => {
    const query = window.matchMedia(COLOR_SCHEME_QUERY)
    const onChange = () => setSystem(systemTheme())
    query.addEventListener("change", onChange)
    return () => query.removeEventListener("change", onChange)
  }, [])

  React.useEffect(() => applyTheme(resolvedTheme), [resolvedTheme])

  const setTheme = React.useCallback((next: Theme) => {
    writeStored(next)
    setThemeState(next)
  }, [])

  const value = React.useMemo(
    () => ({ theme, resolvedTheme, setTheme }),
    [theme, resolvedTheme, setTheme]
  )

  return (
    <ThemeProviderContext.Provider value={value}>
      {children}
    </ThemeProviderContext.Provider>
  )
}

export function useTheme() {
  const context = React.useContext(ThemeProviderContext)
  if (!context) throw new Error("useTheme must be used within ThemeProvider")
  return context
}
