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

export function isDecimalInput(value: string) {
  return /^\d*\.?\d*$/.test(value) && value !== "."
}
