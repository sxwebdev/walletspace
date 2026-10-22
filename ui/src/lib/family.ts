import type { KeyCurve, NetworkFamily } from "@/lib/api/types"

// Everything here that differs per family is a Record over NetworkFamily, so
// a new family does not compile until each of them says what it does there.

export const familyName = {
  evm: "EVM",
  tron: "Tron",
  solana: "Solana",
} satisfies Record<NetworkFamily, string>

// The token standard a family's networks carry besides the native coin.
export const tokenStandard = {
  evm: "ERC-20",
  tron: "TRC-20",
  solana: "SPL",
} satisfies Record<NetworkFamily, string>

export const addressPlaceholder = {
  evm: "0x…",
  tron: "T…",
  solana: "Base58 address",
} satisfies Record<NetworkFamily, string>

// The curve the server expects an imported key of the family to be on; it
// refuses a key whose stated curve disagrees with the network.
export const keyCurve = {
  evm: "secp256k1",
  tron: "secp256k1",
  solana: "ed25519",
} satisfies Record<NetworkFamily, KeyCurve>

// Only EVM hex addresses are case-insensitive. Tron and Solana are base58,
// where case is part of the value: lowercasing one names a different key.
export function sameAddress(family: NetworkFamily, a: string, b: string) {
  if (family === "evm") return a.toLowerCase() === b.toLowerCase()
  return a === b
}

const BASE58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
const BASE58_INDEX = new Map([...BASE58].map((char, i) => [char, i]))

// decodeBase58 returns the bytes a Bitcoin-alphabet base58 string encodes, or
// null when it is empty or has a character outside the alphabet (0, O, I, l,
// anything not alphanumeric). Each leading "1" is a zero byte.
export function decodeBase58(value: string): Uint8Array | null {
  if (!value) return null
  // Little-endian base-256 digits of the number read so far.
  const digits: number[] = []
  for (const char of value) {
    let carry = BASE58_INDEX.get(char)
    if (carry === undefined) return null
    for (let i = 0; i < digits.length; i++) {
      carry += digits[i] * 58
      digits[i] = carry & 0xff
      carry >>= 8
    }
    while (carry > 0) {
      digits.push(carry & 0xff)
      carry >>= 8
    }
  }
  let zeros = 0
  while (value[zeros] === "1") zeros++
  const out = new Uint8Array(zeros + digits.length)
  digits.forEach((digit, i) => (out[out.length - 1 - i] = digit))
  return out
}

// A Solana address or mint is a 32-byte ed25519 public key in base58: 32 to
// 44 characters. The value is checked exactly as given, never case-folded.
export function isSolanaAddress(value: string) {
  if (value.length < 32 || value.length > 44) return false
  return decodeBase58(value)?.length === 32
}
