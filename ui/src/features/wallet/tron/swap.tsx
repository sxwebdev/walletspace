import * as React from "react"
import { AnimatePresence, motion } from "motion/react"
import {
  ArrowDownIcon,
  CheckIcon,
  CircleIcon,
  ExternalLinkIcon,
  RefreshCwIcon,
  TriangleAlertIcon,
  XIcon,
} from "lucide-react"
import { Navigate, useNavigate } from "react-router"

import { FormError } from "@/components/common/form-error"
import { easeOut } from "@/components/common/motion"
import { TokenAvatar } from "@/components/common/token-avatar"
import { ScreenHeader } from "@/components/layout/screen-header"
import { Screen } from "@/components/layout/widget"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Field,
  FieldDescription,
  FieldGroup,
  FieldLabel,
} from "@/components/ui/field"
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group"
import { Spinner } from "@/components/ui/spinner"
import { toast } from "@/components/ui/toast"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import {
  useSendConfirmation,
  wasNotConfirmed,
} from "@/features/security/send-confirmation"
import { useSession } from "@/features/session/session"
import { inFlight } from "@/features/wallet/send/tx-tracker"
import { useAccountRoute } from "@/features/wallet/use-account-route"
import { balanceKey, useWallet } from "@/features/wallet/wallet-data"
import { explorerUrl } from "@/features/wallet/wallet-screen"
import { transfersApi, tronApi } from "@/lib/api/chain"
import { APIError, errorMessage } from "@/lib/api/client"
import type {
  Account,
  Asset,
  Network,
  SwapAsset,
  SwapBody,
  SwapQuote,
  SwapQuotes,
  SwapRoute,
  SwapStepKind,
} from "@/lib/api/types"
import {
  fromBaseUnits,
  isDecimalInput,
  isPositive,
  toNumber,
} from "@/lib/decimal"
import { formatAmount } from "@/lib/format"
import { cn } from "@/lib/utils"

// A quote older than this is fetched again before anything is signed, so the
// minimum on screen is computed from a price that still holds.
const QUOTE_TTL_MS = 30_000
// Selling all TRX would leave nothing to pay the swap's energy with.
const TRX_RESERVE = 50
const SLIPPAGES = [10, 50, 100] // basis points

// Quote times are read only from event handlers, never during render.
const quoteTime = () => Date.now()
const quoteIsStale = (at: number) => quoteTime() - at > QUOTE_TTL_MS

type Stage = "form" | "review" | "run"
type StepStatus =
  | "waiting"
  | "signing"
  | "pending"
  | "confirmed"
  | "failed"
  | "unknown"
  | "skipped"
type StepState = { kind: SwapStepKind; status: StepStatus; txId?: string }
type Quoted = {
  body: { in: SwapAsset; amount: string; unlimited: boolean }
  value: SwapQuotes
  at: number
}

export function SwapScreen() {
  const { account, network, loading } = useAccountRoute()
  if (loading) return null
  if (
    !account ||
    !network ||
    !network.enabled ||
    !network.capabilities.tron_swap
  ) {
    return <Navigate to="/" replace />
  }
  return (
    <SwapFlow
      key={`${account.id}:${network.id}`}
      account={account}
      network={network}
    />
  )
}

export function routeLabel(route: SwapRoute) {
  return `SunSwap ${route.protocol.toUpperCase()} · ${feePercent(route.fee)}`
}

function feePercent(fee: number) {
  return `${Number((fee / 10_000).toFixed(4))}%`
}

const stepLabels: Record<SwapStepKind, string> = {
  approve: "Approve USDT",
  permit2: "Grant through Permit2",
  swap: "Swap",
}

// Whole units of an exact decimal string, without a float in between.
function toUnits(value: string, decimals: number): bigint {
  const [whole, fraction = ""] = value.split(".")
  return BigInt(
    (whole || "0") + fraction.padEnd(decimals, "0").slice(0, decimals)
  )
}

export function minimumOut(
  amountOut: string,
  decimals: number,
  slippageBps: number
) {
  const units =
    (toUnits(amountOut, decimals) * BigInt(10_000 - slippageBps)) / 10_000n
  return fromBaseUnits(units.toString(), decimals)
}

function sameRoute(a: SwapRoute, b: SwapRoute) {
  return (
    a.protocol === b.protocol &&
    a.fee === b.fee &&
    (a.tick_spacing ?? 0) === (b.tick_spacing ?? 0)
  )
}

async function waitForTransaction(
  spaceId: string,
  networkId: string,
  txId: string,
  signal: AbortSignal
) {
  for (let attempt = 0; attempt < 60; attempt++) {
    await new Promise((resolve) => setTimeout(resolve, 3000))
    if (signal.aborted) return "unknown" as const
    try {
      const tx = await transfersApi.transaction(spaceId, networkId, txId)
      if (tx.status !== "pending") return tx.status
    } catch {
      // A failed poll is not a failed transaction; keep asking.
    }
  }
  return "unknown" as const
}

function SwapFlow({
  account,
  network,
}: {
  account: Account
  network: Network
}) {
  const { space } = useSession()
  const spaceId = space!.id
  const { assets, balances, refreshBalances } = useWallet()
  const confirm = useSendConfirmation()
  const navigate = useNavigate()
  const back = `/wallet/${account.id}/${network.id}`

  const networkAssets = assets.get(network.id) ?? []
  const trx = networkAssets.find((a) => a.kind === "native")
  const usdt = networkAssets.find(
    (a) => a.kind === "trc20" && a.symbol === "USDT"
  )

  const [sell, setSell] = React.useState<SwapAsset>("trx")
  const [amount, setAmount] = React.useState("")
  const [unlimited, setUnlimited] = React.useState(false)
  const [slippage, setSlippage] = React.useState(50)
  const [stage, setStage] = React.useState<Stage>("form")
  const [quoted, setQuoted] = React.useState<Quoted | null>(null)
  const [route, setRoute] = React.useState<SwapRoute | null>(null)
  const [busy, setBusy] = React.useState<"quote" | "run" | null>(null)
  const [error, setError] = React.useState<string | null>(null)
  const [steps, setSteps] = React.useState<StepState[]>([])
  const [finished, setFinished] = React.useState(false)
  // A step whose transaction may or may not be on chain. Offering a new quote
  // then would invite signing the same swap twice.
  const [stuck, setStuck] = React.useState(false)
  const [armedFor, setArmedFor] = React.useState<Quoted | null>(null)
  // One idempotency key per step of the plan being run. A retry of a step
  // reuses its key, so a lost answer can never sign that step twice.
  const keys = React.useRef(new Map<SwapStepKind, string>())
  const abort = React.useRef<AbortController | null>(null)
  React.useEffect(() => () => abort.current?.abort(), [])

  const assetIn = sell === "trx" ? trx : usdt
  const assetOut = sell === "trx" ? usdt : trx
  const balanceOf = (asset?: Asset) =>
    asset
      ? balances.get(balanceKey(network.id, account.id, asset.id))?.amount
      : undefined
  const balanceIn = balanceOf(assetIn)
  const quotes = quoted?.value.quotes ?? []
  const selected =
    quotes.find((q) => route && sameRoute(q.route, route)) ?? quotes[0]
  const minOut =
    selected && assetOut
      ? minimumOut(selected.amount_out, assetOut.decimals, slippage)
      : ""
  const armed = armedFor !== null && armedFor === quoted

  // Anything the user changes outside the review throws the quote away.
  const invalidate = () => {
    setQuoted(null)
    setRoute(null)
    setError(null)
    keys.current.clear()
  }

  const fetchQuotes = async (keepRoute: boolean) => {
    if (!isPositive(amount)) {
      setError("Enter an amount.")
      return
    }
    setBusy("quote")
    setError(null)
    try {
      const body = { in: sell, amount: amount.trim(), unlimited }
      const value = await tronApi.swapQuote(
        spaceId,
        network.id,
        account.id,
        body
      )
      if (!value.quotes?.length) {
        setError(
          value.unavailable?.length
            ? `No route can take this swap: ${value.unavailable.join("; ")}`
            : "No route can take this swap."
        )
        return
      }
      const next = { body, value, at: quoteTime() }
      setQuoted(next)
      if (
        !keepRoute ||
        !route ||
        !value.quotes.some((q) => sameRoute(q.route, route))
      ) {
        setRoute(value.quotes[0].route)
      }
      setStage("review")
    } catch (cause) {
      setError(errorMessage(cause))
    } finally {
      setBusy(null)
    }
  }

  // The swap button wakes up 600 ms after a quote appears, so a double click
  // on "Get quotes" cannot sign.
  React.useEffect(() => {
    if (stage !== "review" || !quoted) return
    const id = window.setTimeout(() => setArmedFor(quoted), 600)
    return () => window.clearTimeout(id)
  }, [stage, quoted])

  const setStep = (index: number, patch: Partial<StepState>) =>
    setSteps((current) =>
      current.map((step, i) => (i === index ? { ...step, ...patch } : step))
    )

  const run = async () => {
    if (!quoted || !selected || !assetOut || !armed || busy) return
    if (quoteIsStale(quoted.at)) {
      // Prices move; refresh and let the user look again before signing.
      await fetchQuotes(true)
      toast.add({
        type: "info",
        title: "The quote was refreshed",
        description: "Check the new amounts and swap again.",
      })
      return
    }
    const plan = selected
    const body = (step: SwapStepKind): SwapBody => ({
      in: quoted.body.in,
      amount: quoted.body.amount,
      min_out: minOut,
      route: plan.route,
      step,
      unlimited: quoted.body.unlimited,
    })
    setBusy("run")
    setError(null)
    setStage("run")
    setFinished(false)
    setStuck(false)
    const initial = plan.steps.map((s) => ({
      kind: s.kind,
      status: "waiting" as StepStatus,
    }))
    setSteps(initial)
    abort.current = new AbortController()
    const signal = abort.current.signal

    try {
      for (let i = 0; i < initial.length; i++) {
        const kind = initial[i].kind
        setStep(i, { status: "signing" })
        let key = keys.current.get(kind)
        if (!key) {
          key = crypto.randomUUID()
          keys.current.set(kind, key)
        }
        const k = key
        let result
        try {
          result = await confirm(() =>
            tronApi.swap(spaceId, network.id, account.id, body(kind), k)
          )
        } catch (cause) {
          // The server labels a failure it recorded as never broadcast; only
          // then is the key spent and a new request safe. Anything else — a
          // key already in use, an answer that never came — may hide a
          // transaction on chain, so the run stops where it is.
          const notBroadcast =
            cause instanceof APIError &&
            (cause.payload as { code?: string } | null)?.code ===
              "not_broadcast"
          if (notBroadcast || wasNotConfirmed(cause)) {
            keys.current.delete(kind)
          } else {
            setStuck(true)
          }
          // An approval that is already on chain needs no transaction.
          if (
            notBroadcast &&
            kind !== "swap" &&
            (cause as APIError).message.includes("already in place")
          ) {
            setStep(i, { status: "skipped" })
            continue
          }
          setStep(i, { status: notBroadcast ? "failed" : "unknown" })
          throw cause
        }
        if (inFlight(result.status)) {
          setStep(i, { status: "unknown", txId: result.tx_id })
          setStuck(true)
          setError(
            "The node did not confirm the broadcast. Check the transaction in the explorer before trying again."
          )
          return
        }
        setStep(i, { status: "pending", txId: result.tx_id })
        const outcome = await waitForTransaction(
          spaceId,
          network.id,
          result.tx_id,
          signal
        )
        setStep(i, { status: outcome })
        // A reverted transaction is final: trying again is a new decision.
        if (outcome === "failed") keys.current.delete(kind)
        if (outcome === "unknown") setStuck(true)
        if (outcome !== "confirmed") {
          setError(
            outcome === "failed"
              ? `${stepLabels[kind]} was included but reverted. Its fee is spent; get a new quote to try again.`
              : `${stepLabels[kind]} is not confirmed yet. Check it in the explorer before swapping again.`
          )
          return
        }
        // A confirmed step is done; its key must not sign anything else.
        keys.current.delete(kind)
      }
      setFinished(true)
      toast.add({
        type: "success",
        title: `Swapped ${formatAmount(quoted.body.amount)} ${assetIn?.symbol}`,
      })
    } catch (cause) {
      setError(errorMessage(cause))
    } finally {
      setBusy(null)
      refreshBalances({ accountIds: [account.id], networkIds: [network.id] })
    }
  }

  if (!trx || !usdt) {
    return (
      <Screen header={<ScreenHeader title="Swap" back={back} />}>
        <p className="pt-4 text-sm text-muted-foreground">
          USDT is not configured for {network.name}.
        </p>
      </Screen>
    )
  }

  if (stage === "run" && quoted && selected) {
    return (
      <Screen
        header={<ScreenHeader title="Swap" back={busy ? undefined : back} />}
        footer={
          busy ? null : finished || stuck ? (
            <Button
              size="lg"
              className="w-full"
              onClick={() => navigate(back, { replace: true })}
            >
              {finished ? "Done" : "Back to the wallet"}
            </Button>
          ) : (
            <Button
              size="lg"
              variant="secondary"
              className="w-full"
              onClick={() => void fetchQuotes(true)}
            >
              Get a new quote
            </Button>
          )
        }
      >
        <div className="flex flex-col gap-4 pt-2">
          <SwapSummary
            amountIn={quoted.body.amount}
            assetIn={assetIn!}
            amountOut={selected.amount_out}
            assetOut={assetOut!}
            route={selected.route}
          />
          <ol className="flex flex-col divide-y rounded-2xl border px-4">
            {steps.map((step) => (
              <li
                key={step.kind}
                className="flex items-center gap-3 py-3 text-sm"
              >
                <StepIcon status={step.status} />
                <span className="flex-1">{stepLabels[step.kind]}</span>
                <StepTx network={network} step={step} />
              </li>
            ))}
          </ol>
          {stuck && (
            <Alert variant="destructive">
              <TriangleAlertIcon />
              <AlertDescription>
                It is not known whether the last step reached the network. Check
                the wallet's transactions in the explorer before swapping again
                — signing it a second time could swap twice.
              </AlertDescription>
            </Alert>
          )}
          {finished && (
            <Alert>
              <CheckIcon />
              <AlertDescription>
                The swap is on chain. At least {formatAmount(minOut)}{" "}
                {assetOut!.symbol} was guaranteed; the balance shows what
                arrived.
              </AlertDescription>
            </Alert>
          )}
          <FormError message={error} />
        </div>
      </Screen>
    )
  }

  const reviewing = stage === "review" && quoted !== null
  // The server prices the shortfall for the best route; the user may pick
  // another, so it is worked out again here for the one on screen. Only a
  // warning rides on it, so floats are fine.
  const trxBalance = balanceOf(trx)
  const shortfall =
    selected && quoted && trxBalance !== undefined
      ? Math.max(
          0,
          toNumber(selected.fee) +
            (quoted.body.in === "trx" ? toNumber(quoted.body.amount) : 0) -
            toNumber(trxBalance)
        ).toFixed(6)
      : quoted?.value.shortfall
  // Quotes are shown whatever the balance, so a price can be checked before
  // funding the wallet; signing is not.
  const insufficient =
    quoted !== null &&
    balanceIn !== undefined &&
    toNumber(quoted.body.amount) > toNumber(balanceIn)

  return (
    <Screen
      header={
        <ScreenHeader
          title={reviewing ? "Choose a route" : "Swap"}
          back={reviewing ? () => setStage("form") : back}
        />
      }
      footer={
        reviewing ? (
          <Button
            size="lg"
            className="w-full"
            onClick={() => void run()}
            disabled={!armed || busy !== null || !selected || insufficient}
          >
            {busy !== null && <Spinner data-icon="inline-start" />}
            {selected && selected.steps.length > 1
              ? `Sign ${selected.steps.length} transactions`
              : "Sign and swap"}
          </Button>
        ) : (
          <Button
            type="submit"
            form="swap-form"
            size="lg"
            className="w-full"
            disabled={busy !== null}
          >
            {busy === "quote" && <Spinner data-icon="inline-start" />}
            {busy === "quote" ? "Comparing routes…" : "Get quotes"}
          </Button>
        )
      }
    >
      <AnimatePresence mode="wait" initial={false}>
        {!reviewing ? (
          <motion.form
            key="form"
            id="swap-form"
            initial={{ opacity: 0, x: -16 }}
            animate={{ opacity: 1, x: 0 }}
            exit={{ opacity: 0, x: -16 }}
            transition={{ duration: 0.2, ease: easeOut }}
            autoComplete="off"
            className="pt-2"
            onSubmit={(e) => {
              e.preventDefault()
              void fetchQuotes(false)
            }}
          >
            <FieldGroup>
              {network.testnet && (
                <Alert>
                  <TriangleAlertIcon />
                  <AlertDescription>
                    This is a testnet. Its tokens have no mainnet value.
                  </AlertDescription>
                </Alert>
              )}
              <Field>
                <FieldLabel>Direction</FieldLabel>
                <ToggleGroup
                  value={[sell]}
                  onValueChange={(v) => {
                    if (!v[0]) return
                    setSell(v[0] as SwapAsset)
                    setAmount("")
                    invalidate()
                  }}
                  variant="outline"
                  spacing={0}
                  className="w-full"
                >
                  <ToggleGroupItem value="trx" className="flex-1">
                    TRX → USDT
                  </ToggleGroupItem>
                  <ToggleGroupItem value="usdt" className="flex-1">
                    USDT → TRX
                  </ToggleGroupItem>
                </ToggleGroup>
                <FieldDescription>
                  SunSwap V2, V3 and V4 are compared; the route that leaves you
                  the most after network fees is picked first.
                </FieldDescription>
              </Field>
              <Field>
                <div className="flex items-baseline justify-between">
                  <FieldLabel htmlFor="swap-amount">You sell</FieldLabel>
                  <span className="text-xs text-muted-foreground tabular">
                    Balance:{" "}
                    {balanceIn !== undefined ? formatAmount(balanceIn) : "…"}{" "}
                    {assetIn?.symbol}
                  </span>
                </div>
                <InputGroup className="h-12">
                  <InputGroupInput
                    id="swap-amount"
                    inputMode="decimal"
                    placeholder="0.0"
                    className="text-lg font-medium tabular"
                    value={amount}
                    onChange={(e) => {
                      const value = e.target.value.replace(",", ".")
                      if (value === "" || isDecimalInput(value)) {
                        setAmount(value)
                        invalidate()
                      }
                    }}
                  />
                  <InputGroupAddon align="inline-end">
                    <span className="text-sm text-muted-foreground">
                      {assetIn?.symbol}
                    </span>
                    <InputGroupButton
                      variant="secondary"
                      size="xs"
                      disabled={!isPositive(balanceIn)}
                      onClick={() => {
                        const balance = balanceIn ?? "0"
                        // Only the computed TRX figure has padding to trim;
                        // the USDT balance is exact and may be a whole number.
                        const max =
                          sell === "trx"
                            ? Math.max(0, toNumber(balance) - TRX_RESERVE)
                                .toFixed(6)
                                .replace(/\.?0+$/, "")
                            : balance
                        setAmount(max || "0")
                        invalidate()
                      }}
                    >
                      MAX
                    </InputGroupButton>
                  </InputGroupAddon>
                </InputGroup>
                {sell === "trx" && (
                  <FieldDescription>
                    MAX keeps {TRX_RESERVE} TRX back for network fees.
                  </FieldDescription>
                )}
              </Field>
              {sell === "usdt" && (
                <Field orientation="horizontal">
                  <Checkbox
                    id="swap-unlimited"
                    checked={unlimited}
                    onCheckedChange={(v) => {
                      setUnlimited(Boolean(v))
                      invalidate()
                    }}
                  />
                  <div className="flex flex-col gap-0.5">
                    <FieldLabel
                      htmlFor="swap-unlimited"
                      className="font-normal"
                    >
                      Approve USDT without a limit
                    </FieldLabel>
                    <FieldDescription>
                      Saves an approval on later swaps, but the router can then
                      move any amount of your USDT. Off by default: each swap
                      approves exactly what it sells.
                    </FieldDescription>
                  </div>
                </Field>
              )}
              <FormError message={error} />
            </FieldGroup>
          </motion.form>
        ) : (
          <motion.div
            key="review"
            initial={{ opacity: 0, x: 16 }}
            animate={{ opacity: 1, x: 0 }}
            exit={{ opacity: 0, x: 16 }}
            transition={{ duration: 0.2, ease: easeOut }}
            className="flex flex-col gap-4 pt-2"
          >
            {selected && (
              <SwapSummary
                amountIn={quoted!.body.amount}
                assetIn={assetIn!}
                amountOut={selected.amount_out}
                assetOut={assetOut!}
                route={selected.route}
              />
            )}
            <div className="flex items-center justify-between">
              <span className="text-xs text-muted-foreground">
                Ranked by what you keep after network fees
              </span>
              <Button
                variant="ghost"
                size="xs"
                onClick={() => void fetchQuotes(true)}
                disabled={busy !== null}
              >
                <RefreshCwIcon data-icon="inline-start" />
                Refresh
              </Button>
            </div>
            <div
              className="flex flex-col gap-2"
              role="radiogroup"
              aria-label="Route"
            >
              {quotes.map((quote, i) => (
                <RouteCard
                  key={`${quote.route.protocol}:${quote.route.fee}:${quote.route.tick_spacing ?? 0}`}
                  quote={quote}
                  best={i === 0}
                  selected={selected === quote}
                  symbol={assetOut!.symbol}
                  onSelect={() => setRoute(quote.route)}
                />
              ))}
            </div>
            {selected && (
              <div className="flex flex-col divide-y rounded-2xl border px-4 text-sm">
                <Detail label="Slippage tolerance">
                  <ToggleGroup
                    value={[String(slippage)]}
                    onValueChange={(v) => v[0] && setSlippage(Number(v[0]))}
                    variant="outline"
                    size="sm"
                    spacing={0}
                  >
                    {SLIPPAGES.map((bps) => (
                      <ToggleGroupItem key={bps} value={String(bps)}>
                        {bps / 100}%
                      </ToggleGroupItem>
                    ))}
                  </ToggleGroup>
                </Detail>
                <Detail label="Minimum received">
                  <span className="tabular">
                    {formatAmount(minOut)} {assetOut!.symbol}
                  </span>
                </Detail>
                <Detail label="Network fees">
                  <span className="tabular">
                    ≈ {formatAmount(selected.fee, 4)} TRX
                  </span>
                </Detail>
                <div className="flex flex-col gap-1.5 py-2.5">
                  {selected.steps.map((step) => (
                    <div
                      key={step.kind}
                      className="flex items-center justify-between text-xs text-muted-foreground"
                    >
                      <span>{stepLabels[step.kind]}</span>
                      <span className="tabular">
                        {step.measured ? "" : "≈ "}
                        {step.energy.toLocaleString()} energy ·{" "}
                        {formatAmount(step.fee, 4)} TRX
                      </span>
                    </div>
                  ))}
                </div>
              </div>
            )}
            {selected && selected.steps.length > 1 && (
              <Alert>
                <TriangleAlertIcon />
                <AlertDescription>
                  {selected.steps.length} transactions are signed one after
                  another, each once the one before it is confirmed.{" "}
                  {quoted!.body.unlimited
                    ? "The approval has no limit."
                    : "The approval covers exactly this swap."}
                </AlertDescription>
              </Alert>
            )}
            {insufficient && (
              <Alert variant="destructive">
                <TriangleAlertIcon />
                <AlertDescription>
                  Not enough {assetIn!.symbol}: the balance is{" "}
                  {formatAmount(balanceIn)}.
                </AlertDescription>
              </Alert>
            )}
            {!insufficient && isPositive(shortfall) && (
              <Alert variant="destructive">
                <TriangleAlertIcon />
                <AlertDescription>
                  About {formatAmount(shortfall, 4)} TRX more is needed to pay
                  for the
                  {sell === "trx"
                    ? " swap and its network fees"
                    : " network fees"}
                  . Stake energy or top up TRX first.
                </AlertDescription>
              </Alert>
            )}
            {quoted?.value.unavailable?.length ? (
              <p className="text-xs text-muted-foreground">
                Not quoted: {quoted.value.unavailable.join("; ")}
              </p>
            ) : null}
            <FormError message={error} />
          </motion.div>
        )}
      </AnimatePresence>
    </Screen>
  )
}

function SwapSummary({
  amountIn,
  assetIn,
  amountOut,
  assetOut,
  route,
}: {
  amountIn: string
  assetIn: Asset
  amountOut: string
  assetOut: Asset
  route: SwapRoute
}) {
  return (
    <div className="flex flex-col items-center gap-1 pb-2 text-center">
      <div className="flex items-center gap-2 text-lg font-medium tabular">
        <TokenAvatar symbol={assetIn.symbol} className="size-6" />
        {formatAmount(amountIn)} {assetIn.symbol}
      </div>
      <ArrowDownIcon className="size-4 text-muted-foreground" />
      <div className="flex items-center gap-2 text-2xl font-semibold tracking-tight tabular">
        <TokenAvatar symbol={assetOut.symbol} className="size-7" />≈{" "}
        {formatAmount(amountOut)}{" "}
        <span className="text-muted-foreground">{assetOut.symbol}</span>
      </div>
      <div className="text-xs text-muted-foreground">{routeLabel(route)}</div>
    </div>
  )
}

function RouteCard({
  quote,
  best,
  selected,
  symbol,
  onSelect,
}: {
  quote: SwapQuote
  best: boolean
  selected: boolean
  symbol: string
  onSelect: () => void
}) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      onClick={onSelect}
      className={cn(
        "flex w-full items-center gap-3 rounded-2xl border px-4 py-3 text-left transition-colors",
        selected ? "border-primary bg-primary/5" : "hover:bg-muted/60"
      )}
    >
      <span className="flex min-w-0 flex-1 flex-col gap-0.5">
        <span className="flex items-center gap-2 text-sm font-medium">
          {routeLabel(quote.route)}
          {best && <Badge variant="secondary">Best</Badge>}
        </span>
        <span className="text-xs text-muted-foreground tabular">
          {quote.steps.length === 1
            ? "1 transaction"
            : `${quote.steps.length} transactions`}{" "}
          · fees ≈ {formatAmount(quote.fee, 2)} TRX
        </span>
      </span>
      <span className="text-right text-sm font-medium tabular">
        {formatAmount(quote.amount_out)} {symbol}
      </span>
    </button>
  )
}

function Detail({
  label,
  children,
}: {
  label: string
  children: React.ReactNode
}) {
  return (
    <div className="flex items-center justify-between gap-3 py-2.5">
      <span className="text-xs text-muted-foreground">{label}</span>
      {children}
    </div>
  )
}

function StepIcon({ status }: { status: StepStatus }) {
  switch (status) {
    case "signing":
    case "pending":
      return <Spinner className="size-4" />
    case "confirmed":
    case "skipped":
      return (
        <CheckIcon className="size-4 text-emerald-600 dark:text-emerald-400" />
      )
    case "failed":
      return <XIcon className="size-4 text-destructive" />
    case "unknown":
      return <TriangleAlertIcon className="size-4 text-amber-600" />
    default:
      return <CircleIcon className="size-4 text-muted-foreground" />
  }
}

function StepTx({ network, step }: { network: Network; step: StepState }) {
  if (step.status === "skipped")
    return (
      <span className="text-xs text-muted-foreground">already in place</span>
    )
  if (!step.txId) return null
  const url = explorerUrl(network.explorer.tx, "tx", step.txId)
  if (!url) return null
  return (
    <a
      href={url}
      target="_blank"
      rel="noreferrer noopener"
      className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
    >
      {step.txId.slice(0, 8)}…
      <ExternalLinkIcon className="size-3" />
    </a>
  )
}
