import { wasNotConfirmed } from "@/features/security/send-confirmation"
import { APIError, errorMessage } from "@/lib/api/client"

// What a refused spend (a transfer, closing token accounts) means for the
// next attempt. The server dedupes by idempotency key alone, so the one
// mistake that costs money is a new key for a request that may still go
// through.
//
//   changed     the fee or the chain moved since the estimate (the server's
//               ErrFeeChanged / ErrStateChanged). Nothing was signed: price
//               it again; a new key goes with the new numbers.
//   new_key     the operation was recorded as never broadcast (code
//               "not_broadcast", or the replay refusal that asks for a new
//               key). The same request may go out again under a new key.
//   not_signed  the password prompt was cancelled: the request never reached
//               the signer and its key was never used.
//   refused     the server answered with a definite refusal (a 4xx with its
//               reason): nothing was signed, the reason is shown as it is.
//   unsure      no clear answer: the request is still in progress, the answer
//               was lost, or the server failed in a way that does not say what
//               became of it. Only the same key may follow — it gets the
//               original result — until the user explicitly discards it.
export type Refusal =
  "changed" | "new_key" | "not_signed" | "refused" | "unsure"

// The texts of chain.ErrFeeChanged and chain.ErrStateChanged, which the
// server wraps verbatim; it attaches no code to them.
const CHANGED = [
  "fee estimate changed since it was approved",
  "chain state changed since it was read",
]
const NEW_KEY = "new idempotency key"

export function classifyRefusal(error: unknown): Refusal {
  if (wasNotConfirmed(error)) return "not_signed"
  if (!(error instanceof APIError)) return "unsure"
  const code = (error.payload as { code?: string } | null)?.code
  if (error.status === 409 && CHANGED.some((t) => error.message.includes(t)))
    return "changed"
  if (code === "not_broadcast" || error.message.includes(NEW_KEY))
    return "new_key"
  // A 409 otherwise is a key still in progress or already used for another
  // request; 408 and 499 are requests that ended without an answer.
  if (
    error.status >= 400 &&
    error.status < 500 &&
    ![408, 409, 499].includes(error.status)
  )
    return "refused"
  return "unsure"
}

export const UNSURE_TITLE = "No clear answer"
export const UNSURE_MESSAGE =
  "No clear answer — the request may still be processing. Try again sends the same request, so it cannot go out twice."
// A retry's own refusal, said together with what still holds for the attempt
// before it.
export function stillUnsureMessage(message: string) {
  const sentence = /[.!?]$/.test(message) ? message : `${message}.`
  return `${sentence} The earlier attempt still has no clear answer; Try again sends the same request.`
}

export const DISCARD_WARNING =
  "A previous attempt may still go through. Starting over can send a second transaction."

// Whether an attempt stays unsure after this answer. A first attempt becomes
// unsure only without a clear answer. A retry of an unsure one can be refused
// by checks that run before the server looks the key up — the password, the
// network, the space — and those say nothing about the first attempt. Only
// two answers speak for the key itself: new_key (it was recorded as never
// broadcast) and changed (the fee or state check runs only once the key was
// accepted as new, so the first attempt never registered it).
export function staysUnsure(wasUnsure: boolean, refusal: Refusal) {
  if (!wasUnsure) return refusal === "unsure"
  return refusal !== "new_key" && refusal !== "changed"
}

// The message for an attempt that is still unsure after this answer.
export function unsureMessage(refusal: Refusal, cause: unknown) {
  if (refusal === "unsure") return UNSURE_MESSAGE
  if (refusal === "not_signed")
    return "This retry was not confirmed. The earlier attempt still has no clear answer; Try again sends the same request."
  return stillUnsureMessage(errorMessage(cause))
}

// The cancelled password prompt in neutral words, for screens that do not
// send a transfer.
export const NOT_CONFIRMED_TRANSACTION =
  "The transaction was not confirmed, so nothing was signed."
