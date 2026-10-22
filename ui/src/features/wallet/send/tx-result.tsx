import { AnimatePresence, motion } from "motion/react"
import { CheckIcon, ClockIcon, ExternalLinkIcon, TriangleAlertIcon, XIcon } from "lucide-react"

import { CopyButton } from "@/components/common/copy-button"
import { easeOut } from "@/components/common/motion"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Spinner } from "@/components/ui/spinner"
import { explorerUrl } from "@/features/wallet/wallet-screen"
import type { TrackedStatus } from "@/features/wallet/send/tx-tracker"
import type { Network } from "@/lib/api/types"
import { shortAddress } from "@/lib/format"

const copy: Record<TrackedStatus, { title: string; description: string }> = {
  pending: { title: "Transaction sent", description: "Waiting for the network to include it…" },
  confirmed: { title: "Confirmed", description: "The transaction is on chain." },
  failed: { title: "Transaction failed", description: "The network included it, but it reverted." },
  timeout: {
    title: "Still pending",
    description: "No confirmation yet. Check the explorer before trying again.",
  },
  unknown: {
    title: "Status unknown",
    description: "The node did not confirm the broadcast.",
  },
}

export function TxResult({
  network,
  txId,
  status,
  summary,
  onDone,
}: {
  network: Network
  txId?: string
  status: TrackedStatus
  summary: string
  onDone: () => void
}) {
  const explorer = txId ? explorerUrl(network.explorer.tx, "tx", txId) : null
  // A Solana transaction does not revert; it fails, or its blockhash expires
  // before any block includes it.
  const text =
    status === "failed" && network.family === "solana"
      ? { ...copy.failed, description: "It failed on chain, or expired before the network included it." }
      : copy[status]
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-6 py-8 text-center">
      <StatusIcon status={status} />
      <div className="flex flex-col gap-1">
        <AnimatePresence mode="wait">
          <motion.h2
            key={status}
            initial={{ opacity: 0, y: 4 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: -4 }}
            className="text-xl font-semibold tracking-tight"
          >
            {text.title}
          </motion.h2>
        </AnimatePresence>
        <p className="text-sm text-muted-foreground">{summary}</p>
        <p className="text-sm text-muted-foreground">{text.description}</p>
      </div>
      {status === "unknown" && (
        <Alert variant="destructive" className="text-left">
          <TriangleAlertIcon />
          <AlertTitle>Do not send it again yet</AlertTitle>
          <AlertDescription>
            It may already be on chain. Sending a second one could move the funds twice.
          </AlertDescription>
        </Alert>
      )}
      {txId && (
        <div className="flex items-center gap-1 rounded-full bg-muted/60 py-1 pr-1 pl-3">
          <span className="font-mono text-xs">{shortAddress(txId, 10, 8)}</span>
          <CopyButton value={txId} label="Copy transaction id" size="icon-xs" />
          {explorer && (
            <Button
              variant="ghost"
              size="icon-xs"
              render={<a href={explorer} target="_blank" rel="noreferrer noopener" />}
              aria-label="Open in explorer"
            >
              <ExternalLinkIcon />
            </Button>
          )}
        </div>
      )}
      <Button size="lg" className="w-full" variant={status === "pending" ? "secondary" : "default"} onClick={onDone}>
        Done
      </Button>
    </div>
  )
}

function StatusIcon({ status }: { status: TrackedStatus }) {
  const tone =
    status === "confirmed"
      ? "bg-success/15 text-success"
      : status === "failed" || status === "unknown"
        ? "bg-destructive/15 text-destructive"
        : "bg-muted text-foreground"
  return (
    <motion.div
      layout
      className={`relative flex size-20 items-center justify-center rounded-full transition-colors duration-500 ${tone}`}
      initial={{ scale: 0.6, opacity: 0 }}
      animate={{ scale: 1, opacity: 1 }}
      transition={{ duration: 0.45, ease: easeOut }}
    >
      {status === "pending" && (
        <motion.span
          className="absolute inset-0 rounded-full border-2 border-foreground/10 border-t-foreground/60"
          animate={{ rotate: 360 }}
          transition={{ duration: 1.1, repeat: Infinity, ease: "linear" }}
        />
      )}
      <AnimatePresence mode="wait">
        <motion.span
          key={status}
          initial={{ scale: 0.4, opacity: 0 }}
          animate={{ scale: 1, opacity: 1 }}
          exit={{ scale: 0.4, opacity: 0 }}
          transition={{ type: "spring", stiffness: 400, damping: 20 }}
        >
          {status === "confirmed" ? (
            <CheckIcon className="size-9" strokeWidth={2.5} />
          ) : status === "failed" ? (
            <XIcon className="size-9" strokeWidth={2.5} />
          ) : status === "unknown" ? (
            <TriangleAlertIcon className="size-8" />
          ) : status === "timeout" ? (
            <ClockIcon className="size-8" />
          ) : (
            <Spinner className="size-6 opacity-0" />
          )}
        </motion.span>
      </AnimatePresence>
    </motion.div>
  )
}
