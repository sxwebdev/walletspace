import * as React from "react"

import { transfersApi } from "@/lib/api/chain"
import type { OperationStatus } from "@/lib/api/types"

export type TrackedStatus = "pending" | "confirmed" | "failed" | "unknown" | "timeout"

export const inFlight = (status: OperationStatus | undefined) =>
  status === "broadcast_unknown" || status === "broadcasting"

// Polls a transaction every 3 s, at most 60 times (about three minutes).
// Both families report "failed" for a transaction that was mined and reverted.
export function useTxTracker(
  spaceId: string,
  networkId: string,
  txId: string | undefined,
  onFinal?: (status: "confirmed" | "failed") => void
) {
  // Keyed by transaction, so a new one starts as pending without a reset.
  const [state, setState] = React.useState<{ txId?: string; status: TrackedStatus }>({ status: "pending" })
  const status = state.txId === txId ? state.status : "pending"
  const setStatus = React.useCallback((next: TrackedStatus) => setState({ txId, status: next }), [txId])
  const finish = React.useEffectEvent((next: "confirmed" | "failed") => onFinal?.(next))

  React.useEffect(() => {
    if (!txId) return
    let cancelled = false
    let timer: number
    let attempts = 0
    const tick = async () => {
      attempts += 1
      try {
        const tx = await transfersApi.transaction(spaceId, networkId, txId)
        if (cancelled) return
        if (tx.status !== "pending") {
          setStatus(tx.status)
          finish(tx.status)
          return
        }
      } catch {
        // A failed poll is not a failed transaction; keep asking.
      }
      if (cancelled) return
      if (attempts >= 60) {
        setStatus("timeout")
        return
      }
      timer = window.setTimeout(tick, 3000)
    }
    timer = window.setTimeout(tick, 3000)
    return () => {
      cancelled = true
      window.clearTimeout(timer)
    }
  }, [spaceId, networkId, txId, setStatus])

  return status
}
