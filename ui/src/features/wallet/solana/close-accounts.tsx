import * as React from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { motion } from "motion/react"
import {
  ExternalLinkIcon,
  PencilIcon,
  RotateCwIcon,
  TriangleAlertIcon,
  WalletCardsIcon,
} from "lucide-react"
import { Navigate, useNavigate } from "react-router"

import { CopyButton } from "@/components/common/copy-button"
import { FormError } from "@/components/common/form-error"
import { listItem } from "@/components/common/motion"
import { ScreenHeader } from "@/components/layout/screen-header"
import { Screen } from "@/components/layout/widget"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty"
import { Progress } from "@/components/ui/progress"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { toast } from "@/components/ui/toast"
import {
  needsConfirmation,
  useSendConfirmation,
} from "@/features/security/send-confirmation"
import { queryKeys, useSession } from "@/features/session/session"
import { DiscardAttemptDialog } from "@/features/wallet/send/discard-dialog"
import {
  classifyRefusal,
  NOT_CONFIRMED_TRANSACTION,
  type Refusal,
  staysUnsure,
  unsureMessage,
  UNSURE_MESSAGE,
  UNSURE_TITLE,
} from "@/features/wallet/send/refusal"
import {
  clearUnsure,
  loadUnsure,
  saveUnsure,
  unsureStoreKey,
  upsertUnsureBatch,
} from "@/features/wallet/send/unsure-store"
import {
  inFlight,
  type TrackedStatus,
  useTxTracker,
} from "@/features/wallet/send/tx-tracker"
import { useAccountRoute } from "@/features/wallet/use-account-route"
import { useWallet } from "@/features/wallet/wallet-data"
import { explorerUrl } from "@/features/wallet/wallet-screen"
import { solanaApi } from "@/lib/api/chain"
import { errorMessage } from "@/lib/api/client"
import type {
  Account,
  CloseAccountsBody,
  CloseAccountsEstimate,
  EmptyTokenAccount,
  Network,
  SolanaOperationResult,
} from "@/lib/api/types"
import { subtractDecimal, sumDecimals } from "@/lib/decimal"
import { formatAmount, shortAddress } from "@/lib/format"

// One close transaction: up to max_per_transaction accounts, its approved
// fee, and its own idempotency key.
type Batch = {
  accounts: string[]
  estimate: CloseAccountsEstimate
  key: string
}

type Closed = Batch & { result: SolanaOperationResult }

// Accounts not closed yet, one entry per transaction. This queue is the one
// place pending batches live: a priced entry carries its batch, key included.
// unsure marks a batch whose request got no clear answer. It may still go
// through, so it keeps its batch and key — trying again gets the server's
// answer for it — and is never priced anew unless the user discards it.
// reserved, kept with a stored entry, says whether a request under its key
// may have passed the password gate (see unsure-store).
type Pending = {
  accounts: string[]
  batch?: Batch
  unsure?: boolean
  reserved?: boolean
}

// The entries to keep across a remount: every unsure batch, and the one whose
// request is out right now, which is unsure until its answer arrives.
function keptOf(items: Pending[], flying: Pending | null): Pending[] {
  const kept = items
    .filter((item) => item.unsure && item.batch)
    .map((item) => ({ ...item, reserved: item.reserved ?? true }))
  if (flying) {
    const rest = kept.filter((item) => item.batch!.key !== flying.batch!.key)
    return [{ ...flying, unsure: true }, ...rest]
  }
  return kept
}

const FEE_CHANGED_NOTE =
  "The network fee changed; review the new fee and confirm again."

// The fee the estimate showed, echoed back exactly: the server signs what was
// approved.
function approvalOf(
  estimate: CloseAccountsEstimate
): Omit<CloseAccountsBody, "accounts"> {
  return {
    fee_model: estimate.fee_model,
    fee: estimate.fee,
    compute_unit_limit: estimate.compute_unit_limit,
    compute_unit_price_micro_lamports:
      estimate.compute_unit_price_micro_lamports,
  }
}

function chunk<T>(items: T[], size: number) {
  const out: T[][] = []
  for (let i = 0; i < items.length; i += size)
    out.push(items.slice(i, i + size))
  return out
}

export function CloseAccountsScreen() {
  const { account, network, loading } = useAccountRoute()
  if (loading) return null
  if (
    !account ||
    !network ||
    !network.enabled ||
    !network.capabilities.solana_close_accounts
  ) {
    return <Navigate to="/" replace />
  }
  return (
    <CloseAccountsFlow
      key={`${account.id}:${network.id}`}
      account={account}
      network={network}
    />
  )
}

function CloseAccountsFlow({
  account,
  network,
}: {
  account: Account
  network: Network
}) {
  const { space } = useSession()
  const spaceId = space!.id
  const { refreshBalances } = useWallet()
  const queryClient = useQueryClient()
  const confirm = useSendConfirmation()
  const navigate = useNavigate()
  const back = `/wallet/${account.id}/${network.id}`
  const screenPath = `${back}/close-accounts`
  const { decimals, symbol } = network.native

  const listing = useQuery({
    queryKey: queryKeys.emptyTokenAccounts(spaceId, network.id, account.id),
    queryFn: () =>
      solanaApi.emptyTokenAccounts(spaceId, network.id, account.id),
  })
  const listed = listing.data?.accounts ?? []
  const perTransaction = listing.data?.max_per_transaction ?? 20

  // Everything listed starts selected; the user takes accounts out.
  const [excluded, setExcluded] = React.useState(() => new Set<string>())
  const selected = listed.filter((a) => !excluded.has(a.address))
  // Unsure batches left behind by a remount the user did not choose (the
  // auto-lock, a reload) come back with their keys, ready to try again.
  const storeKey = unsureStoreKey("close", spaceId, account.id, network.id)
  const [restored] = React.useState(() => {
    const stored = loadUnsure<Pending[]>(storeKey) ?? []
    // A batch that never got past the password gate (the prompt was cut
    // short, say by the auto-lock) reserved nothing: it is dropped.
    for (const item of stored)
      if (item.reserved === false && item.batch)
        clearUnsure(storeKey, item.batch.key)
    const kept = stored.filter((item) => item.reserved !== false && item.batch)
    return kept.length > 0 ? kept : null
  })
  const [step, setStep] = React.useState<"select" | "review" | "result">(
    restored?.length ? "review" : "select"
  )
  // What went out is kept apart and never dropped: whatever happens next, the
  // screen can still show and track it. Everything else is in queue.
  const [queue, setQueue] = React.useState<Pending[]>(restored ?? [])
  const [closed, setClosed] = React.useState<Closed[]>([])
  const [busy, setBusy] = React.useState<"estimate" | "close" | null>(null)
  const [progress, setProgress] = React.useState({ index: 0, total: 0 })
  const [error, setError] = React.useState<string | null>(
    restored?.length ? UNSURE_MESSAGE : null
  )
  const [notice, setNotice] = React.useState<string | null>(null)
  const [discarding, setDiscarding] = React.useState(false)
  const [armedFor, setArmedFor] = React.useState<Pending[] | null>(null)
  const armed = armedFor !== null && armedFor === queue
  const priced = queue.length > 0 && queue.every((item) => item.batch)
  const unsure = queue.some((item) => item.unsure)
  const generation = React.useRef(0)

  // Changing the selection or going back drops an estimate still out: what
  // is signed must be what was reviewed. The queue itself is left alone.
  const invalidate = () => {
    generation.current += 1
    setError(null)
    setNotice(null)
    // An estimate still out belongs to the old generation and will not clear
    // its own spinner; a run in flight finishes and clears it itself.
    setBusy((current) => (current === "close" ? current : null))
  }

  // Back from the review: to the result when something already went out, so
  // those transactions stay on screen. Not while a request is out or has no
  // clear answer; only an explicit discard lets go of that.
  const leaveReview = () => {
    if (busy === "close" || unsure) return
    invalidate()
    setStep(closed.length > 0 ? "result" : "select")
  }

  // Prices the entries and shows them for review. An unsure entry keeps its
  // batch, key and approval: it is sent again exactly as before.
  const price = async (items: Pending[], note: string | null = null) => {
    const current = ++generation.current
    setError(null)
    setNotice(null)
    setBusy("estimate")
    try {
      const out: Pending[] = []
      for (const item of items) {
        if (item.unsure && item.batch) {
          out.push(item)
          continue
        }
        const estimate = await solanaApi.closeEstimate(
          spaceId,
          network.id,
          account.id,
          { accounts: item.accounts }
        )
        out.push({
          accounts: item.accounts,
          batch: {
            accounts: item.accounts,
            estimate,
            key: crypto.randomUUID(),
          },
        })
      }
      if (current !== generation.current) return
      setQueue(out)
      setStep("review")
      if (note) setNotice(note)
    } catch (cause) {
      if (current === generation.current) setError(errorMessage(cause))
    } finally {
      if (current === generation.current) setBusy(null)
    }
  }

  const start = () => {
    const items = chunk(
      selected.map((a) => a.address),
      perTransaction
    ).map((accounts): Pending => ({ accounts }))
    setQueue(items)
    void price(items)
  }

  // The only way to let go of an unsure batch: its accounts go back to being
  // priced anew, under a new key.
  const discard = () => {
    for (const item of queue)
      if (item.unsure && item.batch) clearUnsure(storeKey, item.batch.key)
    invalidate()
    if (closed.length > 0) {
      setQueue((items) =>
        items.map((item) => (item.unsure ? { accounts: item.accounts } : item))
      )
      setStep("result")
    } else {
      setQueue([])
      setStep("select")
    }
  }

  // Unsure batches outlive a remount. The batch whose request is out is kept
  // too (written ahead in run()), so leaving mid-request cannot lose its key.
  // Entries are removed only where an answer ends them (in run(), and by a
  // discard), one key at a time — never by a re-render, which could erase
  // what another screen wrote since.
  const flying = React.useRef<Pending | null>(null)
  React.useEffect(() => {
    const kept = keptOf(queue, flying.current)
    if (kept.length > 0) saveUnsure(storeKey, kept)
  }, [queue, storeKey])

  // A screen that was left never starts another batch; one already out just
  // finishes.
  const alive = React.useRef(true)
  React.useEffect(() => {
    alive.current = true
    return () => {
      alive.current = false
    }
  }, [])

  // The confirm button wakes up 600 ms after the review appears, so a held
  // Enter or a double click on "Review" cannot sign.
  React.useEffect(() => {
    if (step !== "review" || !priced) return
    const id = window.setTimeout(() => setArmedFor(queue), 600)
    return () => window.clearTimeout(id)
  }, [step, priced, queue])

  const refresh = () => {
    refreshBalances({ accountIds: [account.id], networkIds: [network.id] })
    void queryClient.invalidateQueries({
      queryKey: queryKeys.emptyTokenAccounts(spaceId, network.id, account.id),
    })
  }

  // Sends the batches one after another and stops at the first that does not
  // go through: a refusal, a fee change, or an answer that was lost.
  const run = async () => {
    if (!priced || !armed || busy) return
    const started = generation.current
    setBusy("close")
    setError(null)
    setNotice(null)
    const total = queue.length
    let rest = queue
    let sent = 0
    let refusal: Refusal | null = null
    let stillUnsure = false
    while (rest.length > 0) {
      // Leaving the screen's flow, or the screen, stops the run before the
      // next batch. The path is checked as well as the mount: a screen that
      // was left stays mounted while its exit animation runs.
      if (
        generation.current !== started ||
        !alive.current ||
        window.location.pathname !== screenPath
      )
        break
      const item = rest[0]
      const batch = item.batch!
      setProgress({ index: sent + 1, total })
      // Written ahead: if the screen goes away while this request is out (a
      // reload, the auto-lock), the batch comes back unsure with its key.
      // reserved turns true as each request leaves, and back to false when
      // the password gate refuses it — unless an earlier attempt may already
      // hold the key.
      const wasReserved = Boolean(item.unsure)
      const record = (reserved: boolean) => {
        flying.current = { ...item, unsure: true, reserved }
        upsertUnsureBatch(storeKey, flying.current)
      }
      record(wasReserved)
      const dispatch = async () => {
        record(true)
        try {
          return await solanaApi.close(
            spaceId,
            network.id,
            account.id,
            { accounts: batch.accounts, ...approvalOf(batch.estimate) },
            batch.key
          )
        } catch (cause) {
          if (needsConfirmation(cause) && !wasReserved) record(false)
          throw cause
        }
      }
      try {
        const result = await confirm(dispatch)
        flying.current = null
        clearUnsure(storeKey, batch.key)
        sent += 1
        rest = rest.slice(1)
        setClosed((list) => [...list, { ...batch, result }])
        setQueue(rest)
        if (inFlight(result.status)) break
      } catch (cause) {
        flying.current = null
        refusal = classifyRefusal(cause)
        stillUnsure = staysUnsure(Boolean(item.unsure), refusal)
        let next: Pending
        switch (stillUnsure ? "unsure" : refusal) {
          case "unsure":
            // No clear answer: the batch, key included, stays as it was.
            next = { ...item, unsure: true }
            break
          case "changed":
            // Proven not broadcast; the new numbers need a new estimate.
            next = { accounts: item.accounts }
            break
          case "new_key":
          case "refused":
            // Nothing was signed: the same approval may go under a new key.
            next = {
              accounts: item.accounts,
              batch: { ...batch, key: crypto.randomUUID() },
            }
            break
          case "not_signed":
            // The password prompt was cancelled; the key was never used.
            next = { ...item, unsure: false }
            break
        }
        rest = [next, ...rest.slice(1)]
        setQueue(rest)
        // An answer that ends the attempt removes it here as well: the
        // screen may be gone, and with it the effect that would.
        if (!stillUnsure) clearUnsure(storeKey, batch.key)
        if (stillUnsure) {
          const message = unsureMessage(refusal, cause)
          setError(message)
          toast.add({
            type: "error",
            title: UNSURE_TITLE,
            description: message,
          })
        } else if (refusal !== "changed") {
          const message =
            refusal === "not_signed"
              ? NOT_CONFIRMED_TRANSACTION
              : errorMessage(cause)
          setError(message)
          toast.add({
            type: "error",
            title: "Not closed",
            description: message,
          })
        }
        break
      }
    }
    setBusy(null)
    if (sent > 0) refresh()
    const wentOut = closed.length + sent > 0
    if (refusal === "changed" && !stillUnsure && !wentOut) {
      if (started === generation.current) await price(rest, FEE_CHANGED_NOTE)
      return
    }
    if (wentOut) {
      // The result lists what went out; what is left — an unsure batch with
      // its key — is offered from there in addition to it.
      if (refusal === "changed" && !stillUnsure)
        setNotice(
          "The network fee changed for the remaining accounts; continue to review the new fee."
        )
      setStep("result")
    }
    // Nothing went out: the review stays, with the error.
  }

  const discardDialog = (
    <DiscardAttemptDialog
      open={discarding}
      onOpenChange={setDiscarding}
      onDiscard={discard}
    />
  )

  if (step === "result") {
    const left = queue.reduce((n, item) => n + item.accounts.length, 0)
    const unanswered = queue.find((item) => item.unsure)
    return (
      <Screen
        header={<ScreenHeader title="Close token accounts" back={back} />}
        footer={
          <Button
            size="lg"
            className="w-full"
            onClick={() => navigate(back, { replace: true })}
          >
            Done
          </Button>
        }
      >
        <div className="flex flex-col gap-4 pt-2">
          <ul className="flex flex-col divide-y rounded-2xl border">
            {closed.map((batch) => (
              <ClosedRow
                key={batch.key}
                batch={batch}
                network={network}
                spaceId={spaceId}
                onFinal={refresh}
              />
            ))}
          </ul>
          {notice && (
            <Alert>
              <TriangleAlertIcon />
              <AlertDescription>{notice}</AlertDescription>
            </Alert>
          )}
          {left > 0 && (
            <div className="flex flex-col items-start gap-2">
              <p className="text-sm text-muted-foreground">
                {unanswered
                  ? `The transaction for ${unanswered.accounts.length} account${unanswered.accounts.length > 1 ? "s" : ""} got no clear answer and may still be processing. Continuing sends that same request first, so they cannot be closed twice.`
                  : `${left} account${left > 1 ? "s were" : " was"} not closed.`}
              </p>
              <div className="flex flex-wrap gap-2">
                <Button
                  variant="outline"
                  disabled={busy !== null}
                  onClick={() => void price(queue)}
                >
                  {busy !== null && <Spinner data-icon="inline-start" />}
                  Continue with the remaining {left} account
                  {left > 1 ? "s" : ""}
                </Button>
                {unanswered && (
                  <Button
                    variant="ghost"
                    disabled={busy !== null}
                    onClick={() => setDiscarding(true)}
                  >
                    Discard and start over
                  </Button>
                )}
              </div>
            </div>
          )}
          <FormError message={error} />
        </div>
        {discardDialog}
      </Screen>
    )
  }

  if (step === "review") {
    const batches = queue.flatMap((item) => (item.batch ? [item.batch] : []))
    const count = batches.reduce((n, b) => n + b.accounts.length, 0)
    const returned = sumDecimals(
      batches.map((b) => b.estimate.returned),
      decimals
    )
    const fee = sumDecimals(
      batches.map((b) => b.estimate.fee),
      decimals
    )
    return (
      <Screen
        header={<ScreenHeader title="Review" back={leaveReview} />}
        footer={
          busy === "close" ? (
            <Button size="lg" className="w-full" disabled>
              <Spinner data-icon="inline-start" />
              {progress.total > 1
                ? `Closing ${progress.index} of ${progress.total}…`
                : "Signing…"}
            </Button>
          ) : priced ? (
            <div className="flex flex-col gap-2">
              <Button
                size="lg"
                className="w-full"
                onClick={run}
                disabled={!armed}
              >
                {queue[0]?.unsure ? "Try again" : "Confirm and close"}
              </Button>
              {unsure && (
                <Button
                  variant="ghost"
                  className="w-full"
                  onClick={() => setDiscarding(true)}
                >
                  Discard and start over
                </Button>
              )}
            </div>
          ) : busy === "estimate" ? (
            <Button size="lg" className="w-full" disabled>
              <Spinner data-icon="inline-start" />
              Calculating the fee…
            </Button>
          ) : null
        }
      >
        <div className="flex flex-col gap-4 pt-2">
          {closed.length > 0 && (
            <div className="flex flex-col gap-2">
              <span className="text-xs text-muted-foreground">
                Already sent; below are the remaining accounts
              </span>
              <ul className="flex flex-col divide-y rounded-2xl border">
                {closed.map((batch) => (
                  <ClosedRow
                    key={batch.key}
                    batch={batch}
                    network={network}
                    spaceId={spaceId}
                    onFinal={refresh}
                  />
                ))}
              </ul>
            </div>
          )}
          {priced && (
            <>
              <div className="flex flex-col items-center gap-1 pt-2 pb-4 text-center">
                <span className="text-sm text-muted-foreground">
                  Returned to you
                </span>
                <div className="text-3xl font-semibold tracking-tight tabular">
                  {formatAmount(
                    subtractDecimal(returned, fee, decimals),
                    decimals
                  )}{" "}
                  <span className="text-muted-foreground">{symbol}</span>
                </div>
                <span className="text-sm text-muted-foreground">
                  after the network fee
                </span>
              </div>
              <div className="flex flex-col divide-y rounded-2xl border px-4">
                <Row label="Network">{network.name}</Row>
                <Row label="Token accounts to close">
                  {count}
                  {batches.length > 1 && (
                    <span className="block text-xs text-muted-foreground">
                      In {batches.length} transactions of up to {perTransaction}{" "}
                      accounts, sent one after another.
                    </span>
                  )}
                </Row>
                <Row label="Rent returned">
                  <span className="tabular">
                    {formatAmount(returned, decimals)} {symbol}
                  </span>
                </Row>
                <Row label="Network fee">
                  <span className="tabular">
                    {formatAmount(fee, decimals)} {symbol}
                  </span>
                </Row>
              </div>
              {unsure && (
                <p className="text-xs text-muted-foreground">
                  Includes a transaction that got no clear answer; it is sent
                  again unchanged, so it cannot go out twice.
                </p>
              )}
              {busy === "close" && progress.total > 1 && (
                <Progress
                  value={((progress.index - 1) / progress.total) * 100}
                  className="**:data-[slot=progress-track]:h-1"
                />
              )}
              <Button
                variant="ghost"
                size="sm"
                className="self-center text-muted-foreground"
                disabled={busy !== null || unsure}
                onClick={leaveReview}
              >
                <PencilIcon data-icon="inline-start" />
                {closed.length > 0 ? "Back to the sent transactions" : "Edit"}
              </Button>
            </>
          )}
          {notice && (
            <Alert>
              <TriangleAlertIcon />
              <AlertDescription>{notice}</AlertDescription>
            </Alert>
          )}
          <FormError message={error} />
        </div>
        {discardDialog}
      </Screen>
    )
  }

  const reclaim = sumDecimals(
    selected.map((a) => a.rent),
    decimals
  )
  const allSelected = selected.length === listed.length
  return (
    <Screen
      header={<ScreenHeader title="Close token accounts" back={back} />}
      footer={
        listed.length > 0 ? (
          <Button
            size="lg"
            className="w-full"
            disabled={selected.length === 0 || busy !== null}
            onClick={start}
          >
            {busy === "estimate" && <Spinner data-icon="inline-start" />}
            {busy === "estimate" ? "Calculating the fee…" : "Review"}
          </Button>
        ) : null
      }
    >
      <div className="flex flex-col gap-4 pt-2">
        <p className="text-sm text-muted-foreground">
          Closes empty token accounts and returns their rent to you. Tokens are
          not affected; frozen accounts are left alone.
        </p>
        {listing.isPending ? (
          <Skeleton className="h-36 w-full rounded-2xl" />
        ) : listing.isError ? (
          <div className="flex flex-col items-start gap-2">
            <FormError message={errorMessage(listing.error)} />
            <Button
              variant="outline"
              size="sm"
              onClick={() => void listing.refetch()}
            >
              <RotateCwIcon data-icon="inline-start" />
              Try again
            </Button>
          </div>
        ) : listed.length === 0 ? (
          <Empty className="py-10">
            <EmptyHeader>
              <EmptyMedia variant="icon">
                <WalletCardsIcon />
              </EmptyMedia>
              <EmptyTitle>No empty token accounts</EmptyTitle>
              <EmptyDescription>
                There is no rent to reclaim for this wallet on {network.name}.
              </EmptyDescription>
            </EmptyHeader>
            <EmptyContent>
              <Button variant="outline" onClick={() => void listing.refetch()}>
                <RotateCwIcon data-icon="inline-start" />
                Check again
              </Button>
            </EmptyContent>
          </Empty>
        ) : (
          <>
            <div className="flex items-center justify-between">
              <span className="text-sm text-muted-foreground tabular">
                {selected.length} of {listed.length} selected ·{" "}
                {formatAmount(reclaim, decimals)} {symbol}
              </span>
              <Button
                variant="ghost"
                size="sm"
                disabled={busy !== null}
                onClick={() => {
                  invalidate()
                  setExcluded(
                    allSelected
                      ? new Set(listed.map((a) => a.address))
                      : new Set()
                  )
                }}
              >
                {allSelected ? "Select none" : "Select all"}
              </Button>
            </div>
            <ul className="flex flex-col divide-y rounded-2xl border">
              {listed.map((item, i) => (
                <motion.li
                  key={item.address}
                  variants={listItem}
                  initial="hidden"
                  animate="show"
                  custom={i}
                >
                  <AccountRow
                    item={item}
                    symbol={symbol}
                    decimals={decimals}
                    checked={!excluded.has(item.address)}
                    disabled={busy !== null}
                    onCheckedChange={(checked) => {
                      invalidate()
                      setExcluded((current) => {
                        const next = new Set(current)
                        if (checked) next.delete(item.address)
                        else next.add(item.address)
                        return next
                      })
                    }}
                  />
                </motion.li>
              ))}
            </ul>
            {selected.length > perTransaction && (
              <p className="text-xs text-muted-foreground">
                Closed in {Math.ceil(selected.length / perTransaction)}{" "}
                transactions of up to {perTransaction} accounts each.
              </p>
            )}
          </>
        )}
        <FormError message={error} />
      </div>
    </Screen>
  )
}

function AccountRow({
  item,
  symbol,
  decimals,
  checked,
  disabled,
  onCheckedChange,
}: {
  item: EmptyTokenAccount
  symbol: string
  decimals: number
  checked: boolean
  disabled: boolean
  onCheckedChange: (checked: boolean) => void
}) {
  const id = `close-${item.address}`
  return (
    <label
      htmlFor={id}
      className="flex cursor-pointer items-center gap-3 px-3 py-2.5"
    >
      <Checkbox
        id={id}
        checked={checked}
        disabled={disabled}
        onCheckedChange={(value) => onCheckedChange(Boolean(value))}
      />
      <span className="flex min-w-0 flex-1 flex-col">
        <span className="truncate text-sm font-medium">
          {item.symbol ?? "Unknown token"}
        </span>
        {/* Mints are base58: shown exactly as the chain has them. */}
        <span
          className="truncate font-mono text-xs text-muted-foreground"
          title={item.mint}
        >
          {shortAddress(item.mint, 6, 6)}
        </span>
      </span>
      <span className="text-sm tabular">
        {formatAmount(item.rent, decimals)} {symbol}
      </span>
    </label>
  )
}

const statusLabel: Record<TrackedStatus, string> = {
  pending: "Pending",
  confirmed: "Confirmed",
  failed: "Failed",
  timeout: "Still pending",
  unknown: "Status unknown",
}

function ClosedRow({
  batch,
  network,
  spaceId,
  onFinal,
}: {
  batch: Closed
  network: Network
  spaceId: string
  onFinal: () => void
}) {
  const { result } = batch
  const unknown = inFlight(result.status)
  const tracked = useTxTracker(
    spaceId,
    network.id,
    unknown ? undefined : result.tx_id,
    onFinal
  )
  const status: TrackedStatus = unknown ? "unknown" : tracked
  const explorer = result.tx_id
    ? explorerUrl(network.explorer.tx, "tx", result.tx_id)
    : null
  return (
    <li className="flex flex-col gap-2 px-3 py-2.5">
      <div className="flex items-center justify-between gap-2">
        <span className="text-sm font-medium">
          {batch.accounts.length} account
          {batch.accounts.length > 1 ? "s" : ""} ·{" "}
          {formatAmount(batch.estimate.returned, network.native.decimals)}{" "}
          {network.native.symbol}
        </span>
        <Badge
          variant={
            status === "failed" || status === "unknown"
              ? "destructive"
              : status === "confirmed"
                ? "secondary"
                : "outline"
          }
        >
          {status === "pending" && <Spinner data-icon="inline-start" />}
          {statusLabel[status]}
        </Badge>
      </div>
      {result.tx_id && (
        <div className="flex items-center gap-1">
          <span className="font-mono text-xs text-muted-foreground">
            {shortAddress(result.tx_id, 10, 8)}
          </span>
          <CopyButton
            value={result.tx_id}
            label="Copy transaction id"
            size="icon-xs"
          />
          {explorer && (
            <Button
              variant="ghost"
              size="icon-xs"
              render={
                <a href={explorer} target="_blank" rel="noreferrer noopener" />
              }
              aria-label="Open in explorer"
            >
              <ExternalLinkIcon />
            </Button>
          )}
        </div>
      )}
      {unknown && (
        <p className="text-xs text-destructive">
          It may already be on chain. Check the explorer before closing these
          accounts again.
        </p>
      )}
    </li>
  )
}

function Row({
  label,
  children,
}: {
  label: string
  children: React.ReactNode
}) {
  return (
    <div className="flex flex-col gap-1 py-2.5">
      <span className="text-xs text-muted-foreground">{label}</span>
      <div className="text-sm">{children}</div>
    </div>
  )
}
