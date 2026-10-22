// Balances and fees are exact decimal strings. Everything that decides what to
// sign keeps them as strings; numbers are only used for USD estimates and for
// "is this more than zero", where float precision does not matter.
export function toNumber(value: string | null | undefined): number {
  if (!value) return 0
  const n = Number(value)
  return Number.isFinite(n) ? n : 0
}

export function isPositive(value: string | null | undefined) {
  return toNumber(value) > 0
}

// An integer amount of base units (wei, sun) as a whole-unit decimal string.
export function fromBaseUnits(raw: string, decimals: number): string {
  const digits = raw.replace(/^0+/, "") || "0"
  if (decimals === 0) return digits
  const padded = digits.padStart(decimals + 1, "0")
  const whole = padded.slice(0, -decimals)
  const fraction = padded.slice(-decimals).replace(/0+$/, "")
  return fraction ? `${whole}.${fraction}` : whole
}

function decimalUnits(value: string, decimals: number) {
  const [whole, fraction = ""] = value.split(".")
  return BigInt(
    (whole || "0") + fraction.padEnd(decimals, "0").slice(0, decimals)
  )
}

// a − b for exact decimal strings with at most `decimals` fraction digits,
// never below zero. For splitting a shown total into its parts, not for
// anything that is signed.
export function subtractDecimal(
  a: string,
  b: string,
  decimals: number
): string {
  const diff = decimalUnits(a, decimals) - decimalUnits(b, decimals)
  return fromBaseUnits((diff > 0n ? diff : 0n).toString(), decimals)
}

// The exact sum of decimal strings, for display.
export function sumDecimals(values: string[], decimals: number): string {
  const total = values.reduce((sum, v) => sum + decimalUnits(v, decimals), 0n)
  return fromBaseUnits(total.toString(), decimals)
}

export function isDecimalInput(value: string) {
  return /^\d*\.?\d*$/.test(value) && value !== "."
}
