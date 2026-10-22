import * as React from "react"
import { ArrowDownUpIcon, RefreshCwIcon, TriangleAlertIcon } from "lucide-react"
import { Navigate, useNavigate } from "react-router"

import { FormError } from "@/components/common/form-error"
import { TokenAvatar } from "@/components/common/token-avatar"
import { ScreenHeader } from "@/components/layout/screen-header"
import { Screen } from "@/components/layout/widget"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
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
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Spinner } from "@/components/ui/spinner"
import { toast } from "@/components/ui/toast"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import {
  needsConfirmation,
  useSendConfirmation,
} from "@/features/security/send-confirmation"
import { useSession } from "@/features/session/session"
import { DiscardAttemptDialog } from "@/features/wallet/send/discard-dialog"
import {
  classifyRefusal,
  NOT_CONFIRMED_TRANSACTION,
  staysUnsure,
  UNSURE_MESSAGE,
  UNSURE_TITLE,
  unsureMessage,
} from "@/features/wallet/send/refusal"
import { TxResult } from "@/features/wallet/send/tx-result"
import { inFlight, useTxTracker } from "@/features/wallet/send/tx-tracker"
import {
  clearUnsure,
  loadUnsure,
  saveUnsure,
  saveUnsureOwned,
  unsureStoreKey,
} from "@/features/wallet/send/unsure-store"
import { useAccountRoute } from "@/features/wallet/use-account-route"
import { balanceKey, useWallet } from "@/features/wallet/wallet-data"
import { solanaApi } from "@/lib/api/chain"
import { errorMessage } from "@/lib/api/client"
import type {
  Account,
  Asset,
  Network,
  SolanaOperationResult,
  SolanaSwapBody,
  SolanaSwapQuote,
  SolanaSwapQuoteBody,
} from "@/lib/api/types"
import {
  isDecimalInput,
  isPositive,
  subtractDecimal,
  toNumber,
} from "@/lib/decimal"
import { formatAmount, shortAddress } from "@/lib/format"

// A quote older than this is fetched again: the minimum on screen has to
// come from a price that still holds.
const QUOTE_TTL_MS = 30_000
const SLIPPAGES = [10, 50, 100] // basis points
// The server refuses a minimum more than 5% below what the pools pay at
// signing time; 3% leaves room for the price to move in the user's favour.
const MAX_SLIPPAGE_BPS = 300
// Selling all SOL would leave nothing for the fee and for the token accounts
// the swap may open; MAX keeps this much back. The quote states the exact
// fee, and the server refuses a swap the balance cannot pay for.
const SOL_RESERVE = "0.01"
const WRAPPED_SOL = "So11111111111111111111111111111111111111112"

type Quoted = {
  request: SolanaSwapQuoteBody
  quote: SolanaSwapQuote
  at: number
}

// An unsure swap kept across a remount, like a send's (see unsure-store).
type StoredSwap = {
  key: string
  request: SolanaSwapQuoteBody
  quote: SolanaSwapQuote
  reserved: boolean
}

const sentence = (text: string) => (/[.!?]$/.test(text) ? text : `${text}.`)

// Raydium's pool programs by the names it gives them.
const POOL_KINDS: Record<string, string> = {
  "amm-v4": "Raydium AMM v4",
  cpmm: "Raydium CPMM",
  clmm: "Raydium CLMM",
}
const poolKind = (kind: string) => POOL_KINDS[kind] ?? kind

// The approval, echoed back exactly as quoted.
function swapBody(quoted: Quoted): SolanaSwapBody {
  const { quote } = quoted
  return {
    ...quoted.request,
    min_out: quote.min_out,
    route: quote.route,
    compute_unit_limit: quote.compute_unit_limit,
    compute_unit_price_micro_lamports:
      quote.compute_unit_price_micro_lamports ?? 0,
    token_accounts_created: quote.token_accounts_created,
    ...(quote.token_account_rent
      ? { token_account_rent: quote.token_account_rent }
      : {}),
    fee: quote.fee,
  }
}

export function SolanaSwapScreen() {
  const { account, network, loading } = useAccountRoute()
  if (loading) return null
  if (
    !account ||
    !network ||
    !network.enabled ||
    !network.capabilities.solana_swap
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
  const networkAssets = React.useMemo(
    () => assets.get(network.id) ?? [],
    [assets, network.id]
  )
  const native = networkAssets.find((a) => a.kind === "native")
  const symbolOf = (mint: string | undefined) => {
    if (!mint) return ""
    if (mint === WRAPPED_SOL) return native?.symbol ?? "SOL"
    // Mints are base58: compared and shown exactly as given.
    return (
      networkAssets.find((a) => a.contract === mint)?.symbol ??
      shortAddress(mint, 4, 4)
    )
  }

  // An unsure swap from before a remount comes back as it was: its review,
  // its key. One that never got past the password gate reserved nothing.
  const storeKey = unsureStoreKey("swap", spaceId, account.id, network.id)
  const [restored] = React.useState(() => {
    const stored = loadUnsure<StoredSwap>(storeKey)
    if (stored && stored.reserved === false) {
      clearUnsure(storeKey, stored.key)
      return null
    }
    return stored
  })

  const [inId, setInId] = React.useState(
    () => restored?.request.in_asset_id ?? native?.id ?? ""
  )
  const [outId, setOutId] = React.useState(
    () =>
      restored?.request.out_asset_id ??
      networkAssets.find((a) => a.kind === "spl" && a.symbol === "USDC")?.id ??
      networkAssets.find((a) => a.id !== native?.id)?.id ??
      ""
  )
  const [amount, setAmount] = React.useState(restored?.request.amount ?? "")
  // A preset is always chosen; a custom value, while valid, overrides it, and
  // clearing it goes back to the preset still highlighted.
  const restoredBps = restored?.request.slippage_bps
  const [preset, setPreset] = React.useState(
    restoredBps !== undefined && SLIPPAGES.includes(restoredBps)
      ? restoredBps
      : 50
  )
  const [custom, setCustom] = React.useState(
    restoredBps !== undefined && !SLIPPAGES.includes(restoredBps)
      ? String(restoredBps / 100)
      : ""
  )
  const customBps = Math.round(toNumber(custom) * 100)
  const customInvalid =
    custom !== "" && (customBps < 1 || customBps > MAX_SLIPPAGE_BPS)
  const slippage = custom !== "" ? customBps : preset
  const [quoted, setQuoted] = React.useState<Quoted | null>(
    restored
      ? { request: restored.request, quote: restored.quote, at: 0 }
      : null
  )
  const [step, setStep] = React.useState<"form" | "review" | "result">(
    restored ? "review" : "form"
  )
  const [busy, setBusy] = React.useState<"quote" | "sign" | null>(null)
  const [error, setError] = React.useState<string | null>(
    restored ? UNSURE_MESSAGE : null
  )
  const [notice, setNotice] = React.useState<string | null>(null)
  // The last signature got no clear answer; see send.tsx for the rule.
  const [unsure, setUnsure] = React.useState(restored !== null)
  const [discarding, setDiscarding] = React.useState(false)
  const [sent, setSent] = React.useState<{
    result: SolanaOperationResult
    quoted: Quoted
  } | null>(null)
  const [armedFor, setArmedFor] = React.useState<Quoted | null>(null)
  const armed = armedFor !== null && armedFor === quoted
  const [now, setNow] = React.useState(0)
  // Automatic re-quoting when the countdown runs out. It stops after a quote
  // fails or a swap is refused: the reason stays on screen and the next quote
  // is the user's to ask for.
  const [auto, setAuto] = React.useState(true)
  const generation = React.useRef(0)
  const idempotencyKey = React.useRef<string | null>(restored?.key ?? null)

  const assetIn = networkAssets.find((a) => a.id === inId)
  const assetOut = networkAssets.find((a) => a.id === outId)
  const balanceOf = (id: string) =>
    balances.get(balanceKey(network.id, account.id, id))?.amount
  const balanceIn = balanceOf(inId)

  // A quote with no valid age: Sign is replaced by "Get a new quote".
  const expire = () =>
    setQuoted((current) => (current ? { ...current, at: 0 } : current))

  // Anything the user changes throws the quote away, with its key.
  const invalidate = () => {
    generation.current += 1
    setQuoted(null)
    idempotencyKey.current = null
    setError(null)
    setNotice(null)
    setBusy((current) => (current === "sign" ? current : null))
  }

  const request = (): SolanaSwapQuoteBody => ({
    in_asset_id: inId,
    out_asset_id: outId,
    amount: amount.trim(),
    slippage_bps: slippage,
  })

  // Prices a request and shows it for review; a new quote is a new approval
  // and gets a new key when it is signed.
  // reason is a refusal that led here; it stays on screen with the outcome.
  const fetchQuote = async (
    body: SolanaSwapQuoteBody,
    note?: string,
    reason?: string
  ) => {
    const current = ++generation.current
    setBusy("quote")
    setError(reason ?? null)
    setNotice(null)
    try {
      const quote = await solanaApi.swapQuote(
        spaceId,
        network.id,
        account.id,
        body
      )
      if (current !== generation.current) return
      idempotencyKey.current = null
      const at = Date.now()
      setQuoted({ request: body, quote, at })
      setNow(at)
      setAuto(true)
      setStep("review")
      setError(null)
      if (note)
        setNotice(
          reason ? `${note} The swap was refused: ${sentence(reason)}` : note
        )
    } catch (cause) {
      if (current === generation.current) {
        setError(
          reason
            ? `${sentence(reason)} A new quote could not be fetched: ${errorMessage(cause)}`
            : errorMessage(cause)
        )
        // A quote that could not be refreshed is not signed: it is expired
        // until the user asks for a new one.
        setAuto(false)
        expire()
      }
    } finally {
      if (current === generation.current) setBusy(null)
    }
  }

  const getQuote = () => {
    if (!assetIn || !assetOut) {
      setError("Choose what to sell and what to buy.")
      return
    }
    if (inId === outId) {
      setError("Choose two different assets.")
      return
    }
    if (!isPositive(amount)) {
      setError("Enter an amount.")
      return
    }
    if (customInvalid) return
    void fetchQuote(request())
  }

  // The swap button wakes up 600 ms after a quote appears.
  React.useEffect(() => {
    if (step !== "review" || !quoted) return
    const id = window.setTimeout(() => setArmedFor(quoted), 600)
    return () => window.clearTimeout(id)
  }, [step, quoted])

  // The unsure swap outlives a remount. It is written ahead in sign() and
  // removed only where an answer ends it.
  React.useEffect(() => {
    if (unsure && quoted && idempotencyKey.current) {
      saveUnsure(storeKey, {
        key: idempotencyKey.current,
        request: quoted.request,
        quote: quoted.quote,
        reserved: true,
      } satisfies StoredSwap)
    }
  }, [unsure, quoted, storeKey])

  // The countdown, and a fresh quote when it runs out — never while a
  // signature is out or unanswered: that approval must stay as it was. A
  // quote with no age (restored, or after a refusal) counts as expired.
  const expired =
    quoted !== null && (quoted.at === 0 || now - quoted.at >= QUOTE_TTL_MS)
  // The clock stops while a signature is out — the quote cannot run out
  // under it — and for good once the swap went out.
  React.useEffect(() => {
    if (step !== "review" || !quoted || unsure || quoted.at === 0) return
    if (busy === "sign" || sent) return
    const id = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(id)
  }, [step, quoted, unsure, busy, sent])
  React.useEffect(() => {
    if (!expired || !auto || busy !== null || unsure || sent) return
    if (!quoted || quoted.at === 0) return
    const request = quoted.request
    const id = window.setTimeout(() => void fetchQuote(request), 0)
    return () => window.clearTimeout(id)
    // fetchQuote is recreated each render; the expiry is what triggers this,
    // once: a failure turns auto off.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [expired, auto, busy, unsure, sent])

  const sign = async () => {
    if (!quoted || !armed || busy) return
    // An expired quote is never signed; an unsure retry keeps its quote,
    // whatever its age: it has to be the same request.
    if (
      !unsure &&
      (quoted.at === 0 || Date.now() - quoted.at >= QUOTE_TTL_MS)
    ) {
      expire()
      return
    }
    setBusy("sign")
    setError(null)
    idempotencyKey.current ??= crypto.randomUUID()
    const key = idempotencyKey.current
    const approved = quoted
    const body = swapBody(approved)
    const started = generation.current
    let changed = false
    let reason: string | undefined
    // Written ahead, as for a send: leaving mid-request brings the swap
    // back unsure with this key. reserved turns true as each request leaves
    // and back to false when the password gate refuses it.
    const wasReserved = unsure
    let reserved = wasReserved
    const record = () =>
      saveUnsureOwned(storeKey, {
        key,
        request: approved.request,
        quote: approved.quote,
        reserved,
      } satisfies StoredSwap)
    saveUnsure(storeKey, {
      key,
      request: approved.request,
      quote: approved.quote,
      reserved,
    } satisfies StoredSwap)
    const dispatch = async () => {
      reserved = true
      record()
      try {
        return await solanaApi.swap(spaceId, network.id, account.id, body, key)
      } catch (cause) {
        if (needsConfirmation(cause) && !wasReserved) {
          reserved = false
          record()
        }
        throw cause
      }
    }
    try {
      const result = await confirm(dispatch)
      // Done: no more quotes, no timers.
      setAuto(false)
      setSent({ result, quoted: approved })
      setUnsure(false)
      clearUnsure(storeKey, key)
      setStep("result")
      refreshBalances({ accountIds: [account.id], networkIds: [network.id] })
    } catch (cause) {
      const refusal = classifyRefusal(cause)
      const stillUnsure = staysUnsure(unsure, refusal)
      setUnsure(stillUnsure)
      if (!stillUnsure) clearUnsure(storeKey, key)
      // The price or the chain moved: a fresh quote follows. The old one
      // and its key go first, so nothing signs them while it is out.
      changed = !stillUnsure && refusal === "changed"
      if (!stillUnsure && (refusal === "new_key" || refusal === "refused"))
        idempotencyKey.current = null
      if (!stillUnsure) {
        // After a refusal nothing re-quotes by itself: the reason stays on
        // screen. A quote that was unsure, or refused for its price, is
        // expired: only a fresh one may be signed.
        setAuto(false)
        if (unsure || changed) expire()
        if (changed) idempotencyKey.current = null
      }
      if (stillUnsure) {
        const message = unsureMessage(refusal, cause)
        setError(message)
        toast.add({ type: "error", title: UNSURE_TITLE, description: message })
      } else if (changed) {
        reason = errorMessage(cause)
        setError(reason)
        toast.add({
          type: "error",
          title: "Not swapped — the price or fee changed",
          description: reason,
        })
      } else {
        const message =
          refusal === "not_signed"
            ? NOT_CONFIRMED_TRANSACTION
            : errorMessage(cause)
        setError(message)
        toast.add({ type: "error", title: "Not swapped", description: message })
      }
    } finally {
      setBusy(null)
    }
    if (changed && started === generation.current)
      await fetchQuote(
        approved.request,
        "The price or the fee changed; review the new quote and swap again.",
        reason
      )
  }

  const leaveReview = () => {
    if (busy === "sign" || unsure) return
    invalidate()
    setStep("form")
  }

  const tracked = useTxTracker(
    spaceId,
    network.id,
    sent && !inFlight(sent.result.status) ? sent.result.tx_id : undefined,
    () =>
      refreshBalances({ accountIds: [account.id], networkIds: [network.id] })
  )

  if (sent) {
    const sentIn = networkAssets.find(
      (a) => a.id === sent.quoted.request.in_asset_id
    )
    const sentOut = networkAssets.find(
      (a) => a.id === sent.quoted.request.out_asset_id
    )
    return (
      <Screen header={<div className="h-14 sm:hidden" />}>
        <TxResult
          network={network}
          txId={sent.result.tx_id}
          status={inFlight(sent.result.status) ? "unknown" : tracked}
          summary={`${formatAmount(sent.quoted.request.amount)} ${sentIn?.symbol ?? ""} → at least ${formatAmount(sent.quoted.quote.min_out)} ${sentOut?.symbol ?? ""}`}
          onDone={() => navigate(back, { replace: true })}
        />
      </Screen>
    )
  }

  const discardDialog = (
    <DiscardAttemptDialog
      open={discarding}
      onOpenChange={setDiscarding}
      onDiscard={() => {
        if (idempotencyKey.current)
          clearUnsure(storeKey, idempotencyKey.current)
        setUnsure(false)
        invalidate()
        setStep("form")
      }}
    />
  )

  if (step === "review" && quoted) {
    const quote = quoted.quote
    const quotedIn = networkAssets.find(
      (a) => a.id === quoted.request.in_asset_id
    )
    const quotedOut = networkAssets.find(
      (a) => a.id === quoted.request.out_asset_id
    )
    const impact =
      quote.price_impact_pct !== undefined
        ? toNumber(quote.price_impact_pct)
        : null
    const left =
      quoted.at > 0
        ? Math.max(0, Math.ceil((QUOTE_TTL_MS - (now - quoted.at)) / 1000))
        : null
    const rent = quote.token_account_rent ?? "0"
    return (
      <Screen
        header={<ScreenHeader title="Review" back={leaveReview} />}
        footer={
          <div className="flex flex-col gap-2">
            {busy === "sign" ? (
              <Button size="lg" className="w-full" disabled>
                <Spinner data-icon="inline-start" />
                Signing…
              </Button>
            ) : expired && !unsure ? (
              <Button
                size="lg"
                className="w-full"
                onClick={() => void fetchQuote(quoted.request)}
                disabled={busy !== null}
              >
                {busy === "quote" && <Spinner data-icon="inline-start" />}
                {busy === "quote" ? "Getting a new quote…" : "Get a new quote"}
              </Button>
            ) : (
              <Button
                size="lg"
                className="w-full"
                onClick={() => void sign()}
                disabled={!armed || busy !== null}
              >
                {busy !== null && <Spinner data-icon="inline-start" />}
                {busy === "quote"
                  ? "Refreshing the quote…"
                  : unsure
                    ? "Try again"
                    : "Sign and swap"}
              </Button>
            )}
            {unsure && (
              <Button
                variant="ghost"
                className="w-full"
                disabled={busy !== null}
                onClick={() => setDiscarding(true)}
              >
                Discard and start over
              </Button>
            )}
          </div>
        }
      >
        <div className="flex flex-col gap-4 pt-2">
          <div className="flex flex-col divide-y rounded-2xl border px-4">
            <Row label="You pay">
              <span className="flex items-center gap-2">
                <TokenAvatar
                  symbol={quotedIn?.symbol ?? "?"}
                  className="size-6"
                />
                <span className="text-base font-medium tabular">
                  {formatAmount(quote.amount_in, 9)} {quotedIn?.symbol}
                </span>
              </span>
            </Row>
            <Row label="You receive at least">
              <span className="flex items-center gap-2">
                <TokenAvatar
                  symbol={quotedOut?.symbol ?? "?"}
                  className="size-6"
                />
                <span className="text-base font-medium tabular">
                  {formatAmount(quote.min_out, 9)} {quotedOut?.symbol}
                </span>
              </span>
              <span className="block text-xs text-muted-foreground tabular">
                Expected {formatAmount(quote.amount_out, 9)} {quotedOut?.symbol}{" "}
                · slippage {quote.slippage_bps / 100}%
              </span>
            </Row>
            <Row label="Route">
              {quote.route.hops.map((hop) => poolKind(hop.kind)).join(" → ")}
              {quote.route.hops.length > 1 && (
                <span className="block text-xs text-muted-foreground">
                  Via{" "}
                  {symbolOf(
                    quote.intermediate_mint ?? quote.route.hops[0].output_mint
                  )}
                </span>
              )}
            </Row>
            {impact !== null && (
              <Row label="Price impact">
                <span
                  className={
                    impact > 5
                      ? "font-medium text-destructive tabular"
                      : "tabular"
                  }
                >
                  {impact.toFixed(2)}%
                </span>
              </Row>
            )}
            <Row label="Network fee">
              <span className="tabular">
                {formatAmount(
                  isPositive(rent)
                    ? subtractDecimal(quote.fee, rent, 9)
                    : quote.fee,
                  9
                )}{" "}
                SOL
              </span>
              <span className="block text-xs text-muted-foreground tabular">
                {quote.compute_unit_limit.toLocaleString()} compute units ·
                priority{" "}
                {(
                  quote.compute_unit_price_micro_lamports ?? 0
                ).toLocaleString()}{" "}
                micro-lamports per unit
              </span>
            </Row>
            {quote.token_accounts_created > 0 && (
              <Row label="Token accounts opened">
                {quote.token_accounts_created}
                {isPositive(rent) && (
                  <span className="block text-xs text-muted-foreground tabular">
                    One-time rent {formatAmount(rent, 9)} SOL, paid by you.
                  </span>
                )}
              </Row>
            )}
            {quote.intermediate_leftover &&
              isPositive(quote.intermediate_leftover) && (
                <Row label="Left in the wallet">
                  <span className="tabular">
                    {formatAmount(quote.intermediate_leftover, 9)}{" "}
                    {symbolOf(quote.intermediate_mint)}
                  </span>
                  <span className="block text-xs text-muted-foreground">
                    What the first hop pays beyond its guaranteed part stays
                    with you.
                  </span>
                </Row>
              )}
            {quote.unwraps_wrapped_sol &&
              isPositive(quote.unwraps_wrapped_sol) && (
                <Row label="Wrapped SOL">
                  <span className="text-xs text-muted-foreground">
                    The {formatAmount(quote.unwraps_wrapped_sol, 9)} wrapped SOL
                    you hold comes back as SOL.
                  </span>
                </Row>
              )}
          </div>
          {impact !== null && impact > 1 && (
            <Alert variant={impact > 5 ? "destructive" : "default"}>
              <TriangleAlertIcon />
              {impact > 5 && <AlertTitle>High price impact</AlertTitle>}
              <AlertDescription>
                This swap moves the pool price by {impact.toFixed(2)}%. You get
                noticeably less than the market rate.
              </AlertDescription>
            </Alert>
          )}
          <div className="flex items-center justify-between text-xs text-muted-foreground">
            <span>
              {quote.source === "raydium_api"
                ? "Routed by Raydium"
                : "Found on chain in Raydium pools"}
              {quote.api_route_rejected && (
                <span className="block">
                  Raydium's own route was not used: {quote.api_route_rejected}
                </span>
              )}
            </span>
            {!unsure && (
              <Button
                variant="ghost"
                size="xs"
                disabled={busy !== null}
                onClick={() => void fetchQuote(quoted.request)}
              >
                <RefreshCwIcon data-icon="inline-start" />
                {left !== null ? `Refresh · ${left}s` : "Refresh"}
              </Button>
            )}
          </div>
          {notice && (
            <Alert>
              <TriangleAlertIcon />
              <AlertDescription>{notice}</AlertDescription>
            </Alert>
          )}
          <Button
            variant="ghost"
            size="sm"
            className="self-center text-muted-foreground"
            disabled={busy !== null || unsure}
            onClick={leaveReview}
          >
            Edit
          </Button>
          <FormError message={error} />
        </div>
        {discardDialog}
      </Screen>
    )
  }

  const solIn = assetIn?.kind === "native"
  return (
    <Screen
      header={<ScreenHeader title="Swap" back={back} />}
      footer={
        <Button
          type="submit"
          form="solana-swap-form"
          size="lg"
          className="w-full"
          disabled={busy !== null || customInvalid}
        >
          {busy === "quote" && <Spinner data-icon="inline-start" />}
          {busy === "quote" ? "Finding a route…" : "Get a quote"}
        </Button>
      }
    >
      <form
        id="solana-swap-form"
        autoComplete="off"
        className="pt-2"
        onSubmit={(e) => {
          e.preventDefault()
          getQuote()
        }}
      >
        <FieldGroup>
          <Field>
            <div className="flex items-baseline justify-between">
              <FieldLabel htmlFor="swap-amount">You sell</FieldLabel>
              <span className="text-xs text-muted-foreground tabular">
                Balance:{" "}
                {balanceIn !== undefined ? formatAmount(balanceIn) : "…"}{" "}
                {assetIn?.symbol}
              </span>
            </div>
            <AssetPicker
              assets={networkAssets}
              value={inId}
              balanceOf={balanceOf}
              onChange={(id) => {
                setInId(id)
                setAmount("")
                invalidate()
              }}
            />
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
                    setAmount(
                      solIn
                        ? subtractDecimal(
                            balance,
                            SOL_RESERVE,
                            assetIn?.decimals ?? 9
                          )
                        : balance
                    )
                    invalidate()
                  }}
                >
                  MAX
                </InputGroupButton>
              </InputGroupAddon>
            </InputGroup>
            {solIn && (
              <FieldDescription>
                MAX keeps {SOL_RESERVE} SOL back for the network fee and any
                token account the swap opens.
              </FieldDescription>
            )}
          </Field>
          <div className="flex justify-center">
            <Button
              type="button"
              variant="outline"
              size="icon-sm"
              aria-label="Swap direction"
              onClick={() => {
                setInId(outId)
                setOutId(inId)
                setAmount("")
                invalidate()
              }}
            >
              <ArrowDownUpIcon />
            </Button>
          </div>
          <Field>
            <FieldLabel>You buy</FieldLabel>
            <AssetPicker
              assets={networkAssets}
              value={outId}
              balanceOf={balanceOf}
              onChange={(id) => {
                setOutId(id)
                invalidate()
              }}
            />
          </Field>
          <Field>
            <FieldLabel>Slippage tolerance</FieldLabel>
            <div className="flex items-center gap-2">
              <ToggleGroup
                value={custom ? [] : [String(preset)]}
                onValueChange={(v) => {
                  if (!v[0]) return
                  setCustom("")
                  setPreset(Number(v[0]))
                  invalidate()
                }}
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
              <InputGroup className="w-28">
                <InputGroupInput
                  inputMode="decimal"
                  placeholder="Custom"
                  aria-label="Custom slippage in percent"
                  className="tabular"
                  value={custom}
                  onChange={(e) => {
                    const value = e.target.value.replace(",", ".")
                    if (value !== "" && !isDecimalInput(value)) return
                    setCustom(value)
                    invalidate()
                  }}
                />
                <InputGroupAddon align="inline-end">%</InputGroupAddon>
              </InputGroup>
            </div>
            {customInvalid ? (
              <FieldDescription className="text-destructive">
                Between 0.01% and {MAX_SLIPPAGE_BPS / 100}%. Clear it to use the{" "}
                {preset / 100}% preset.
              </FieldDescription>
            ) : (
              <FieldDescription>
                The swap fails rather than pay out less than this below the
                quote.
              </FieldDescription>
            )}
          </Field>
          <FieldDescription>
            Routes come from Raydium (powered by Raydium); a pool found on chain
            is used when its own route cannot be.
          </FieldDescription>
          <FormError message={error} />
        </FieldGroup>
      </form>
    </Screen>
  )
}

function AssetPicker({
  assets,
  value,
  onChange,
  balanceOf,
}: {
  assets: Asset[]
  value: string
  onChange: (id: string) => void
  balanceOf: (id: string) => string | undefined
}) {
  const byId = new Map(assets.map((a) => [a.id, a]))
  return (
    <Select value={value} onValueChange={(v) => onChange(String(v))}>
      <SelectTrigger className="h-12 w-full">
        <SelectValue>
          {(id: string) => {
            const asset = byId.get(id)
            if (!asset) return "Choose an asset"
            return (
              <span className="flex items-center gap-2">
                <TokenAvatar symbol={asset.symbol} className="size-7" />
                <span className="font-medium">{asset.symbol}</span>
                <span className="text-muted-foreground">{asset.name}</span>
              </span>
            )
          }}
        </SelectValue>
      </SelectTrigger>
      <SelectContent className="max-h-80">
        <SelectGroup>
          {assets.map((asset) => (
            <SelectItem key={asset.id} value={asset.id}>
              <span className="flex w-full items-center gap-2">
                <TokenAvatar symbol={asset.symbol} className="size-6" />
                <span className="font-medium">{asset.symbol}</span>
                <span className="flex-1 truncate text-muted-foreground">
                  {asset.name}
                </span>
                <span className="text-xs text-muted-foreground tabular">
                  {formatAmount(balanceOf(asset.id))}
                </span>
              </span>
            </SelectItem>
          ))}
        </SelectGroup>
      </SelectContent>
    </Select>
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
