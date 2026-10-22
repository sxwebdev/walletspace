import * as React from "react"
import { FuelIcon } from "lucide-react"

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Field, FieldLabel } from "@/components/ui/field"
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Spinner } from "@/components/ui/spinner"
import { useSendConfirmation } from "@/features/security/send-confirmation"
import { useSession } from "@/features/session/session"
import { GroupedAddress } from "@/features/wallet/send/grouped-address"
import { accountNetworks, accountTitle, balanceKey, useWallet } from "@/features/wallet/wallet-data"
import { transfersApi } from "@/lib/api/chain"
import { APIError, errorMessage } from "@/lib/api/client"
import type { Account, Network, RefuelPlan, TransferBody, TransferEstimate } from "@/lib/api/types"
import { fromBaseUnits, isPositive } from "@/lib/decimal"
import { formatAmount } from "@/lib/format"
import { sessionGet, sessionSet } from "@/lib/storage"

const usd4 = new Intl.NumberFormat("en-US", { style: "currency", currency: "USD", maximumFractionDigits: 4 })
const POLL_MS = 5000
const POLL_LIMIT_MS = 10 * 60_000

// Not enough native coin for an ERC-20 transfer: fund the gas from another
// wallet in the space (same network or cross-chain), wait for confirmed
// delivery, then hand a fresh estimate back to the send flow. The plan id is
// stored before paying and is never replaced by a new quote while it is
// pending, so a reload or a retry cannot pay twice.
export function RefuelPanel({
  account,
  network,
  body,
  estimate,
  onReady,
}: {
  account: Account
  network: Network
  body: TransferBody
  estimate: TransferEstimate
  onReady: (estimate: TransferEstimate) => void
}) {
  const { space, enabledNetworks, networkById } = useSession()
  const spaceId = space!.id
  const { accounts, balances } = useWallet()
  const confirm = useSendConfirmation()
  const storeKey = `walletspace:refuel:${spaceId}:${network.id}:${account.id}`

  const [pendingId, setPendingIdState] = React.useState(() => sessionGet(storeKey))
  const setPendingId = (id: string | null) => {
    sessionSet(storeKey, id)
    setPendingIdState(id)
  }
  const [source, setSource] = React.useState("")
  const [plans, setPlans] = React.useState<RefuelPlan[]>([])
  const [selected, setSelected] = React.useState<RefuelPlan | null>(null)
  const [status, setStatus] = React.useState<string>("")
  const [phase, setPhase] = React.useState<"idle" | "quoting" | "funding" | "waiting">("idle")
  const [armedFor, setArmedFor] = React.useState<RefuelPlan | null>(null)
  const armed = armedFor !== null && armedFor === selected
  const alive = React.useRef(true)
  const generation = React.useRef(0)
  React.useEffect(() => () => void (alive.current = false), [])

  const sources = React.useMemo(
    () =>
      accounts.flatMap((a) =>
        enabledNetworks
          .filter(
            (n) =>
              n.family === "evm" &&
              !n.testnet &&
              accountNetworks(a).includes(n.id) &&
              !(a.id === account.id && n.id === network.id)
          )
          .map((n) => ({ account: a, network: n, balance: balances.get(balanceKey(n.id, a.id, `${n.id}:native`)) }))
          .filter((s) => !s.balance?.error && isPositive(s.balance?.amount))
      ),
    [accounts, enabledNetworks, balances, account.id, network.id]
  )

  const finishWithFreshEstimate = async () => {
    setStatus("Gas received. Rechecking the transfer fee…")
    const fresh = await transfersApi.estimate(spaceId, network.id, body)
    if (!alive.current) return
    if (isPositive(fresh.fee_shortfall)) {
      setStatus("Gas arrived, but the fee increased. Calculate again before topping up further.")
      setPhase("idle")
      return
    }
    onReady(fresh)
  }

  const monitor = async (planId: string) => {
    setPhase("waiting")
    setStatus("Waiting for the gas top-up. This can take a few minutes; do not pay again.")
    const deadline = Date.now() + POLL_LIMIT_MS
    while (alive.current && Date.now() < deadline) {
      try {
        const state = await transfersApi.refuelStatus(spaceId, network.id, planId)
        if (!alive.current) return
        setSelected(state.plan)
        if (state.status === "delivered") {
          setPendingId(null)
          await finishWithFreshEstimate()
          return
        }
        if (state.status === "failed" || state.status === "cancelled") {
          setPendingId(null)
          setPhase("idle")
          setStatus(
            state.status === "failed"
              ? "The source transaction failed. Calculate a new quote to retry."
              : "The provider cancelled this top-up. Check its refund before paying again."
          )
          return
        }
        if (state.status === "not_started") {
          setPhase("idle")
          setStatus("This top-up was not submitted. Resume the same quote to send it.")
          return
        }
        setStatus("Top-up in progress. Waiting for confirmed delivery; nothing will be paid twice.")
      } catch (cause) {
        if (!alive.current) return
        setStatus(`${errorMessage(cause)}. The top-up id is saved; checking again without resending.`)
      }
      await new Promise((r) => setTimeout(r, POLL_MS))
    }
    if (alive.current) {
      setPhase("idle")
      setStatus("Still waiting. Come back to this transfer later to check the saved top-up.")
    }
  }

  const calculate = async (sourceKey: string) => {
    const current = ++generation.current
    setPlans([])
    setSelected(null)
    setPhase("quoting")
    setStatus("Checking balances, source fees and top-up quotes…")
    const [accountId, networkId] = sourceKey ? sourceKey.split("|") : []
    try {
      const result = await transfersApi.refuelQuotes(
        spaceId,
        network.id,
        { account_id: body.account_id, asset_id: body.asset_id, to: body.to, amount: body.amount },
        sourceKey ? { accountId, networkId } : undefined
      )
      if (current !== generation.current || !alive.current) return
      setPhase("idle")
      if (!isPositive(result.estimate.fee_shortfall)) {
        onReady(result.estimate)
        return
      }
      if (result.quotes.length === 0) {
        const notes = result.unavailable?.join("; ")
        setStatus(notes ? `No affordable route. ${notes}` : "No affordable route. Connect another funded EVM wallet to this space.")
        return
      }
      setPlans(result.quotes)
      setSelected(result.quotes[0])
      setStatus(
        (result.sources_total ?? 0) > (result.sources_checked ?? 0)
          ? `Compared ${result.sources_checked} sources. Pick a wallet to check another.`
          : "Review the route and the total cost before signing."
      )
    } catch (cause) {
      if (current !== generation.current || !alive.current) return
      setPhase("idle")
      const pending = cause instanceof APIError ? (cause.payload as { pending_plan_id?: string })?.pending_plan_id : undefined
      if (pending) {
        setPendingId(pending)
        void resume(pending)
        return
      }
      setStatus(errorMessage(cause))
    }
  }

  const resume = async (planId: string) => {
    setStatus("Loading the saved top-up…")
    try {
      const state = await transfersApi.refuelStatus(spaceId, network.id, planId)
      if (!alive.current) return
      setSelected(state.plan)
      setPlans([state.plan])
      if (state.status === "not_started") {
        setStatus("This top-up was saved but not sent. Resume it to pay the same quote.")
      } else {
        void monitor(planId)
      }
    } catch (cause) {
      if (alive.current) setStatus(errorMessage(cause))
    }
  }

  // Starts the first quote (or the saved top-up) once per mounted panel.
  React.useEffect(() => {
    queueMicrotask(() => {
      if (pendingId) void resume(pendingId)
      else void calculate("")
    })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  React.useEffect(() => {
    if (!selected || phase !== "idle") return
    const id = window.setTimeout(() => setArmedFor(selected), 600)
    return () => window.clearTimeout(id)
  }, [selected, phase])

  const submit = async () => {
    if (!selected || phase !== "idle") return
    if (Date.parse(selected.quote.expires_at) <= Date.now() && selected.id !== pendingId) {
      setStatus("The quote expired. Calculate new options.")
      setArmedFor(null)
      return
    }
    setPhase("funding")
    setPendingId(selected.id)
    setStatus("Confirm the top-up. The token transfer is reviewed again after delivery.")
    try {
      await confirm(() => transfersApi.refuelSend(spaceId, network.id, selected.id))
      if (alive.current) void monitor(selected.id)
    } catch (cause) {
      if (!alive.current) return
      setPhase("idle")
      setStatus(errorMessage(cause))
    }
  }

  const sourceNetwork = selected ? networkById.get(selected.source_network_id) : undefined
  const resuming = Boolean(pendingId && selected?.id === pendingId)

  return (
    <div className="flex flex-col gap-4">
      <Alert>
        <FuelIcon />
        <AlertTitle>Not enough {network.native.symbol} for gas</AlertTitle>
        <AlertDescription>
          Missing {formatAmount(estimate.fee_shortfall, 8)} {network.native.symbol} (balance{" "}
          {formatAmount(estimate.native_balance, 8)}, fee budget {formatAmount(estimate.fee, 8)}). Top it up from another
          wallet, then send.
        </AlertDescription>
      </Alert>

      {!resuming && phase !== "waiting" && (
        <Field>
          <FieldLabel>Pay gas from</FieldLabel>
          <Select
            value={source}
            onValueChange={(v) => {
              setSource(String(v))
              void calculate(String(v))
            }}
          >
            <SelectTrigger className="w-full" disabled={phase !== "idle"}>
              <SelectValue>
                {(v: string) => {
                  if (!v) return "Compare all funded wallets"
                  const s = sources.find((x) => `${x.account.id}|${x.network.id}` === v)
                  return s ? `${accountTitle(s.account)} · ${s.network.name}` : v
                }}
              </SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                <SelectItem value="">Compare all funded wallets</SelectItem>
                {sources.map((s) => (
                  <SelectItem key={`${s.account.id}|${s.network.id}`} value={`${s.account.id}|${s.network.id}`}>
                    {accountTitle(s.account)} · {s.network.name} · {formatAmount(s.balance?.amount)} {s.network.native.symbol}
                  </SelectItem>
                ))}
              </SelectGroup>
            </SelectContent>
          </Select>
        </Field>
      )}

      {plans.length > 1 && !resuming && (
        <div className="flex flex-col gap-1.5">
          {plans.map((plan) => {
            const n = networkById.get(plan.source_network_id)
            const active = selected?.id === plan.id
            return (
              <button
                key={plan.id}
                type="button"
                onClick={() => setSelected(plan)}
                data-active={active}
                className="flex items-center justify-between rounded-2xl border px-3 py-2.5 text-left text-sm transition-colors data-[active=true]:border-primary data-[active=true]:bg-muted/50"
              >
                <span className="truncate">
                  {plan.source_label || plan.source_account_id} · {n?.name ?? plan.source_network_id}
                </span>
                <span className="text-muted-foreground tabular">
                  {plan.total_usd ? usd4.format(Number(plan.total_usd)) : `${formatAmount(plan.total_source, 6)} ${n?.native.symbol ?? ""}`}
                </span>
              </button>
            )
          })}
        </div>
      )}

      {selected && (
        <div className="flex flex-col gap-1.5 rounded-2xl border p-4 text-sm">
          <div className="font-medium">
            {selected.source_label || selected.source_account_id} · {sourceNetwork?.name ?? selected.source_network_id}
          </div>
          <div className="text-muted-foreground">
            Pay {fromBaseUnits(selected.quote.deposit, sourceNetwork?.native.decimals ?? 18)} {sourceNetwork?.native.symbol} + up
            to {formatAmount(selected.source_estimate.fee, 8)} {sourceNetwork?.native.symbol} source gas.
          </div>
          <div>
            Total up to {formatAmount(selected.total_source, 8)} {sourceNetwork?.native.symbol}
            {selected.total_usd ? ` · ≈ ${usd4.format(Number(selected.total_usd))}` : " · USD unavailable"}
          </div>
          <div>
            Receive {formatAmount(selected.expected, 8)} {network.native.symbol}; about{" "}
            {formatAmount(selected.remainder, 8)} {network.native.symbol} left after the transfer.
          </div>
          {selected.quote.minimum_limited && (
            <div className="text-muted-foreground">The provider minimum is larger than the gas this transfer needs.</div>
          )}
          <div className="text-xs text-muted-foreground">
            {selected.quote.provider} · quote expires {new Date(selected.quote.expires_at).toLocaleTimeString()}
          </div>
          <Collapsible>
            <CollapsibleTrigger render={<Button variant="link" size="xs" className="h-auto px-0" />}>
              Check the addresses
            </CollapsibleTrigger>
            <CollapsibleContent className="flex flex-col gap-2 pt-2 text-xs">
              <div>
                <div className="text-muted-foreground">Source</div>
                <GroupedAddress value={selected.source_address} />
              </div>
              <div>
                <div className="text-muted-foreground">Gas recipient</div>
                <GroupedAddress value={selected.recipient} />
              </div>
              <div>
                <div className="text-muted-foreground">Token recipient</div>
                <GroupedAddress value={selected.target.to} />
              </div>
            </CollapsibleContent>
          </Collapsible>
        </div>
      )}

      {status && (
        <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
          {(phase === "quoting" || phase === "waiting" || phase === "funding") && <Spinner />}
          {status}
        </p>
      )}

      {selected && phase !== "waiting" && (
        <Button size="lg" onClick={submit} disabled={!armed || phase !== "idle"}>
          {phase === "funding" && <Spinner data-icon="inline-start" />}
          {resuming ? "Resume this top-up" : "Top up gas, then continue"}
        </Button>
      )}
      {!selected && phase === "idle" && !pendingId && (
        <Button variant="secondary" onClick={() => void calculate(source)}>
          Calculate again
        </Button>
      )}
    </div>
  )
}
