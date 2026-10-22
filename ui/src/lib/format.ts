const usd = new Intl.NumberFormat("en-US", {
  style: "currency",
  currency: "USD",
  maximumFractionDigits: 2,
})

const usdSmall = new Intl.NumberFormat("en-US", {
  style: "currency",
  currency: "USD",
  maximumSignificantDigits: 3,
})

export function formatUSD(value: string | number | null | undefined) {
  if (value === null || value === undefined || value === "") return "—"
  const n = typeof value === "number" ? value : Number(value)
  if (!Number.isFinite(n)) return "—"
  if (n !== 0 && Math.abs(n) < 0.01) return usdSmall.format(n)
  return usd.format(n)
}

export function formatPercent(value: number | null | undefined) {
  if (value === null || value === undefined || !Number.isFinite(value))
    return "—"
  const sign = value > 0 ? "+" : ""
  return `${sign}${value.toFixed(2)}%`
}

// Amounts arrive as exact decimal strings; they are grouped and trimmed for
// display only, never parsed into floats that would round a balance.
export function formatAmount(value: string | null | undefined, maxFraction = 6) {
  if (value === null || value === undefined || value === "") return "—"
  const negative = value.startsWith("-")
  const raw = negative ? value.slice(1) : value
  const [whole, fraction = ""] = raw.split(".")
  const grouped = whole.replace(/\B(?=(\d{3})+(?!\d))/g, ",")
  let trimmed = fraction.slice(0, maxFraction).replace(/0+$/, "")
  if (!trimmed && /[1-9]/.test(fraction) && whole === "0") {
    // A dust balance must not read as zero.
    trimmed = fraction.replace(/0+$/, "").slice(0, fraction.search(/[1-9]/) + 2)
  }
  return `${negative ? "-" : ""}${grouped}${trimmed ? `.${trimmed}` : ""}`
}

export function isZeroAmount(value: string | null | undefined) {
  return !value || !/[1-9]/.test(value)
}

export function shortAddress(address: string, head = 6, tail = 4) {
  if (address.length <= head + tail + 1) return address
  return `${address.slice(0, head)}…${address.slice(-tail)}`
}

const relative = new Intl.RelativeTimeFormat("en", { numeric: "auto" })

export function formatRelative(date: string | number | Date | null | undefined) {
  if (!date) return "—"
  const t = new Date(date).getTime()
  if (!Number.isFinite(t)) return "—"
  const seconds = Math.round((t - Date.now()) / 1000)
  const abs = Math.abs(seconds)
  if (abs < 60) return relative.format(seconds, "second")
  if (abs < 3600) return relative.format(Math.round(seconds / 60), "minute")
  if (abs < 86400) return relative.format(Math.round(seconds / 3600), "hour")
  return relative.format(Math.round(seconds / 86400), "day")
}

export function formatDateTime(date: string | number | Date | null | undefined) {
  if (!date) return "—"
  const d = new Date(date)
  if (!Number.isFinite(d.getTime())) return "—"
  return d.toLocaleString()
}
