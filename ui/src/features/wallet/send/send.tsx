import * as React from "react"
import { AnimatePresence, motion } from "motion/react"
import { ArrowDownIcon, PencilIcon, TriangleAlertIcon } from "lucide-react"
import { Navigate, useNavigate, useSearchParams } from "react-router"

import { FormError } from "@/components/common/form-error"
import { easeOut } from "@/components/common/motion"
import { TokenAvatar } from "@/components/common/token-avatar"
import { ScreenHeader } from "@/components/layout/screen-header"
import { Screen } from "@/components/layout/widget"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
  InputGroupTextarea,
} from "@/components/ui/input-group"
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Spinner } from "@/components/ui/spinner"
import { toast } from "@/components/ui/toast"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { needsConfirmation, useSendConfirmation } from "@/features/security/send-confirmation"
import { useSession } from "@/features/session/session"
import { GroupedAddress } from "@/features/wallet/send/grouped-address"
import { RefuelPanel } from "@/features/wallet/send/refuel"
import { DiscardAttemptDialog } from "@/features/wallet/send/discard-dialog"
import {
  classifyRefusal,
  staysUnsure,
  UNSURE_MESSAGE,
  UNSURE_TITLE,
  unsureMessage,
} from "@/features/wallet/send/refusal"
import { inFlight, useTxTracker } from "@/features/wallet/send/tx-tracker"
import {
  clearUnsure,
  loadUnsure,
  saveUnsure,
  saveUnsureOwned,
  unsureStoreKey,
} from "@/features/wallet/send/unsure-store"
import { TxResult } from "@/features/wallet/send/tx-result"
import { useAccountRoute } from "@/features/wallet/use-account-route"
import { accountAddress, accountNetworks, accountTitle, balanceKey, useWallet } from "@/features/wallet/wallet-data"
import { transfersApi, type FeeApproval } from "@/lib/api/chain"
import { APIError, errorMessage } from "@/lib/api/client"
import type {
  Account,
  Asset,
  Network,
  NetworkFamily,
  Operation,
  TransferBody,
  TransferEstimate,
} from "@/lib/api/types"
import { isDecimalInput, isPositive, subtractDecimal, toNumber } from "@/lib/decimal"
import { addressPlaceholder, isSolanaAddress, sameAddress } from "@/lib/family"
import { formatAmount, formatUSD } from "@/lib/format"

type Step = "form" | "review" | "result"
type Approved = { body: TransferBody; estimate: TransferEstimate }
// What is kept of an unsure attempt across a remount.
type StoredAttempt = {
  key: string
  approved: Approved
  action: "wrap" | "unwrap"
  fromMax: boolean
  // A request under the key may have passed the password gate.
  reserved: boolean
}

// The server refuses to reuse a key only with this wording; every other
// failure keeps the key, so a retry cannot sign a second transaction.
export function retryIsSafe(error: unknown) {
  return error instanceof APIError && error.message.includes("new idempotency key")
}

// The fee the estimate showed, echoed back exactly: the server signs what was
// approved and refuses a send whose approval does not match its family.
function approvalOf(family: NetworkFamily, fee: TransferEstimate): FeeApproval {
  switch (family) {
    case "solana":
      return {
        fee_model: fee.fee_model,
        fee: fee.fee,
        compute_unit_limit: fee.compute_unit_limit,
        compute_unit_price_micro_lamports: fee.compute_unit_price_micro_lamports,
        creates_token_account: fee.creates_token_account,
        token_account_rent: fee.token_account_rent,
      }
    case "evm":
    case "tron":
      return {
        fee_model: fee.fee_model,
        gas_limit: fee.gas_limit,
        max_fee_per_gas: fee.max_fee_per_gas,
        max_priority_fee_per_gas: fee.max_priority_fee_per_gas,
      }
  }
}

export function SendScreen({ mode }: { mode: "send" | "wrap" }) {
  const { account, network, address, loading } = useAccountRoute()
  if (loading) return null
  if (!account || !network || !network.enabled) return <Navigate to="/" replace />
  if (mode === "wrap" && !network.wrapped_native) return <Navigate to={`/wallet/${account.id}/${network.id}`} replace />
  return <SendFlow key={`${account.id}:${network.id}:${mode}`} mode={mode} account={account} network={network} address={address} />
}

function SendFlow({
  mode,
  account,
  network,
  address,
}: {
  mode: "send" | "wrap"
  account: Account
  network: Network
  address: string
}) {
  const conversion = mode === "wrap"
  const { space } = useSession()
  const spaceId = space!.id
  const { assets, balances, prices, refreshBalances, accounts } = useWallet()
  const confirm = useSendConfirmation()
  const navigate = useNavigate()
  const [params] = useSearchParams()

  const networkAssets = React.useMemo(() => assets.get(network.id) ?? [], [assets, network.id])
  const native = networkAssets.find((a) => a.kind === "native")
  const wrapped = networkAssets.find(
    (a) => a.contract && network.wrapped_native && a.contract.toLowerCase() === network.wrapped_native.contract.toLowerCase()
  )

  // An attempt that got no clear answer before this screen was last left
  // (an auto-lock, a reload) comes back as it was: its review, its key.
  const storeKey = unsureStoreKey(mode, spaceId, account.id, network.id)
  const [restored] = React.useState(() => {
    const stored = loadUnsure<StoredAttempt>(storeKey)
    // Nothing got past the password gate under that key (the prompt was cut
    // short, say by the auto-lock): nothing was reserved, so there is nothing
    // to come back to.
    if (stored && stored.reserved === false) {
      clearUnsure(storeKey, stored.key)
      return null
    }
    return stored
  })
  const [action, setAction] = React.useState<"wrap" | "unwrap">(restored?.action ?? "wrap")
  const [assetId, setAssetId] = React.useState(() => {
    if (restored) return restored.approved.body.asset_id
    const requested = params.get("asset")
    return networkAssets.some((a) => a.id === requested) ? requested! : (native?.id ?? networkAssets[0]?.id ?? "")
  })
  const [to, setTo] = React.useState(restored?.approved.body.to ?? (conversion ? address : ""))
  const [amount, setAmount] = React.useState(restored?.approved.body.amount ?? "")
  const [step, setStep] = React.useState<Step>(restored ? "review" : "form")
  const [approved, setApproved] = React.useState<Approved | null>(restored?.approved ?? null)
  const [needsRefuel, setNeedsRefuel] = React.useState<Approved | null>(null)
  const [error, setError] = React.useState<string | null>(restored ? UNSURE_MESSAGE : null)
  // The last signature got no clear answer: it may still go through, so only
  // the same key and request may follow. Only a success, the server's word
  // that this key never went out (new_key), or an explicit discard ends it.
  const [unsure, setUnsure] = React.useState(restored !== null)
  const [discarding, setDiscarding] = React.useState(false)
  // Shown on a review that replaced one the network moved past.
  const [notice, setNotice] = React.useState<string | null>(null)
  const [busy, setBusy] = React.useState<"estimate" | "max" | "sign" | null>(null)
  // The approval the sign button was armed for; a new approval disarms it.
  const [armedFor, setArmedFor] = React.useState<Approved | null>(null)
  const armed = armedFor !== null && armedFor === approved
  // What went out, with the request that was signed. It is kept apart from
  // the approval, so the result is shown even if anything else was reset
  // meanwhile; a transfer that went out must never look like it did not.
  const [sent, setSent] = React.useState<{ operation: Operation; body: TransferBody } | null>(null)
  const generation = React.useRef(0)
  // Whether the review on screen came from MAX: a fresh estimate after a fee
  // change asks for the maximum again rather than the old, now too large,
  // amount.
  const fromMax = React.useRef(restored?.fromMax ?? false)
  const idempotencyKey = React.useRef<string | null>(restored?.key ?? null)

  const effectiveAssetId = conversion ? (action === "wrap" ? native?.id : wrapped?.id) ?? "" : assetId
  const asset = networkAssets.find((a) => a.id === effectiveAssetId)
  const balance = balances.get(balanceKey(network.id, account.id, effectiveAssetId))
  const quote = network.testnet ? undefined : prices.get(effectiveAssetId)
  const amountUsd = quote && amount ? toNumber(amount) * toNumber(quote.current_usd) : null

  // Anything the user changes invalidates the estimate, the approval and the
  // idempotency key: what is signed must be what was reviewed.
  const invalidate = () => {
    generation.current += 1
    setApproved(null)
    setNeedsRefuel(null)
    idempotencyKey.current = null
    setError(null)
    setNotice(null)
    // An estimate still out belongs to the old generation and will not clear
    // its own spinner; a signature in flight finishes and clears it itself.
    setBusy((current) => (current === "sign" ? current : null))
  }

  const body = (overrideAmount?: string): TransferBody => ({
    ...(conversion ? { action } : {}),
    account_id: account.id,
    asset_id: effectiveAssetId,
    to: conversion ? address : to.trim(),
    amount: overrideAmount ?? amount.trim(),
  })

  // Reports whether a review is now on screen.
  const present = (request: TransferBody, estimate: TransferEstimate) => {
    if (network.testnet && isPositive(estimate.fee_shortfall)) {
      setError(
        `Not enough ${network.native.symbol} for ${network.family === "solana" ? "the network fee" : "gas"}. Get testnet funds from the network's faucet.`
      )
      return false
    }
    if (!conversion && network.family === "evm" && isPositive(estimate.fee_shortfall)) {
      setNeedsRefuel({ body: request, estimate })
      setStep("review")
      return true
    }
    setApproved({ body: request, estimate })
    setStep("review")
    return true
  }

  const estimate = async (max: boolean) => {
    const current = ++generation.current
    setError(null)
    setNotice(null)
    if (!conversion && !to.trim()) {
      setError(max ? "Enter the recipient first — the fee depends on it." : "Enter the recipient.")
      return
    }
    if (!conversion && network.family === "solana" && !isSolanaAddress(to.trim())) {
      setError("Not a Solana address: it is 32 bytes in base58.")
      return
    }
    if (!max && !isPositive(amount)) {
      setError("Enter an amount.")
      return
    }
    if (!max && balance?.amount !== undefined && toNumber(amount) > toNumber(balance.amount)) {
      setError(`Not enough ${asset?.symbol ?? "funds"}: the balance is ${formatAmount(balance.amount)}.`)
      return
    }
    setBusy(max ? "max" : "estimate")
    try {
      const request = body(max ? "max" : undefined)
      const result = await transfersApi.estimate(spaceId, network.id, request)
      if (current !== generation.current) return
      fromMax.current = max
      if (max) setAmount(result.amount)
      present({ ...request, amount: result.amount || request.amount }, result)
    } catch (cause) {
      if (current === generation.current) setError(errorMessage(cause))
    } finally {
      if (current === generation.current) setBusy(null)
    }
  }

  // After a 409 the same transfer is estimated again and its new review shown;
  // nothing is sent until the user signs that one. The approval is cleared
  // first and the answer is dropped if anything changed meanwhile, so a late
  // answer cannot arm the button with a stale approval.
  const reestimate = async (previous: TransferBody) => {
    const current = ++generation.current
    setApproved(null)
    setNeedsRefuel(null)
    idempotencyKey.current = null
    setError(null)
    setNotice(null)
    setBusy("estimate")
    try {
      const request = fromMax.current ? { ...previous, amount: "max" } : previous
      const result = await transfersApi.estimate(spaceId, network.id, request)
      if (current !== generation.current) return
      if (fromMax.current) setAmount(result.amount)
      if (present({ ...request, amount: result.amount || request.amount }, result)) {
        setNotice("The network fee changed; review the new fee and sign again.")
      }
    } catch (cause) {
      if (current === generation.current) setError(errorMessage(cause))
    } finally {
      if (current === generation.current) setBusy(null)
    }
  }

  // The unsure attempt outlives a remount. It is written ahead in sign() and
  // removed only where an answer ends it (success, a proof that nothing was
  // reserved, a discard) — never by a re-render.
  React.useEffect(() => {
    if (unsure && approved && idempotencyKey.current) {
      saveUnsure(storeKey, {
        key: idempotencyKey.current,
        approved,
        action,
        fromMax: fromMax.current,
        reserved: true,
      } satisfies StoredAttempt)
    }
  }, [unsure, approved, action, storeKey])

  // The sign button wakes up 600 ms after the review appears, so a held Enter
  // or a double click on "Review" cannot sign.
  React.useEffect(() => {
    if (step !== "review" || !approved) return
    const id = window.setTimeout(() => setArmedFor(approved), 600)
    return () => window.clearTimeout(id)
  }, [step, approved])

  const sign = async () => {
    if (!approved || !armed || busy) return
    setBusy("sign")
    setError(null)
    idempotencyKey.current ??= crypto.randomUUID()
    const key = idempotencyKey.current
    const { estimate: fee } = approved
    const started = generation.current
    let changed = false
    // Written ahead: if the screen goes away while the request is out (a
    // reload, the auto-lock, navigation), the attempt comes back as unsure
    // with this key instead of a fresh form that would sign again. reserved
    // turns true as each request leaves, and back to false when the password
    // gate refuses it — unless an earlier attempt may already hold the key.
    const wasReserved = unsure
    let reserved = wasReserved
    const record = () =>
      saveUnsureOwned(storeKey, {
        key,
        approved,
        action,
        fromMax: fromMax.current,
        reserved,
      } satisfies StoredAttempt)
    saveUnsure(storeKey, { key, approved, action, fromMax: fromMax.current, reserved } satisfies StoredAttempt)
    const dispatch = async () => {
      reserved = true
      record()
      try {
        return await transfersApi.send(spaceId, network.id, { ...approved.body, ...approvalOf(network.family, fee) }, key)
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
      setSent({ operation: result, body: approved.body })
      setUnsure(false)
      clearUnsure(storeKey, key)
      setStep("result")
    } catch (cause) {
      const refusal = classifyRefusal(cause)
      const stillUnsure = staysUnsure(unsure, refusal)
      setUnsure(stillUnsure)
      // Proven that nothing was reserved under the key: the saved attempt goes.
      if (!stillUnsure) clearUnsure(storeKey, key)
      // The fee or the chain moved: a fresh estimate follows.
      changed = !stillUnsure && refusal === "changed"
      // Proven not signed: the same approval may go under a new key. A
      // cancelled password prompt never used its key and keeps it.
      if (!stillUnsure && (refusal === "new_key" || refusal === "refused")) idempotencyKey.current = null
      if (stillUnsure) {
        const message = unsureMessage(refusal, cause)
        setError(message)
        toast.add({ type: "error", title: UNSURE_TITLE, description: message })
      } else if (!changed) {
        const message = errorMessage(cause)
        setError(message)
        toast.add({ type: "error", title: "Not sent", description: message })
      }
    } finally {
      setBusy(null)
    }
    // Not if the user went back to the form while the send was out.
    if (changed && started === generation.current) await reestimate(approved.body)
  }

  const txId = sent?.operation.tx_hash
  const recipientTo = sent?.body.to ?? approved?.body.to
  const recipientAccount = accounts.find(
    (a) =>
      a.id !== account.id &&
      accountNetworks(a).includes(network.id) &&
      recipientTo !== undefined &&
      sameAddress(network.family, accountAddress(a, network), recipientTo)
  )
  const tracked = useTxTracker(spaceId, network.id, sent ? txId : undefined, (status) => {
    toast.add(
      status === "confirmed"
        ? { type: "success", title: "Transaction confirmed" }
        : { type: "error", title: "The transaction failed" }
    )
    refreshBalances({
      accountIds: [account.id, ...(recipientAccount ? [recipientAccount.id] : [])],
      networkIds: [network.id],
    })
  })

  // Back to the form from the review, but not while a signature is out: the
  // answer to it decides what the screen shows next.
  const leaveReview = () => {
    if (busy === "sign" || unsure) return
    invalidate()
    setStep("form")
  }

  const title = conversion ? (action === "wrap" ? "Wrap" : "Unwrap") : "Send"
  const symbol = asset?.symbol ?? ""
  const back = `/wallet/${account.id}/${network.id}`

  if (sent) {
    return (
      <Screen header={<div className="h-14 sm:hidden" />}>
        <TxResult
          network={network}
          txId={txId}
          status={inFlight(sent.operation.status) ? "unknown" : tracked}
          summary={`${formatAmount(sent.body.amount)} ${symbol} · ${network.name}`}
          onDone={() => navigate(back, { replace: true })}
        />
      </Screen>
    )
  }

  const reviewing = step === "review"
  const target = approved ?? needsRefuel

  return (
    <Screen
      header={
        <ScreenHeader
          title={reviewing ? "Review" : title}
          back={reviewing ? leaveReview : back}
        />
      }
      footer={
        reviewing ? (
          approved ? (
            <div className="flex flex-col gap-2">
              <Button size="lg" className="w-full" onClick={sign} disabled={!armed || busy === "sign"}>
                {busy === "sign" && <Spinner data-icon="inline-start" />}
                {busy === "sign"
                  ? "Signing…"
                  : unsure
                    ? "Try again"
                    : conversion
                      ? `Sign and ${action}`
                      : "Sign and send"}
              </Button>
              {unsure && (
                <Button variant="ghost" className="w-full" disabled={busy !== null} onClick={() => setDiscarding(true)}>
                  Discard and start over
                </Button>
              )}
            </div>
          ) : busy === "estimate" ? (
            <Button size="lg" className="w-full" disabled>
              <Spinner data-icon="inline-start" />
              Calculating the new fee…
            </Button>
          ) : null
        ) : (
          <Button type="submit" form="send-form" size="lg" className="w-full" disabled={busy !== null || !asset}>
            {busy === "estimate" && <Spinner data-icon="inline-start" />}
            {busy === "estimate" ? "Calculating the fee…" : "Review"}
          </Button>
        )
      }
    >
      <AnimatePresence mode="wait" initial={false}>
        {!reviewing ? (
          <motion.form
            key="form"
            id="send-form"
            initial={{ opacity: 0, x: -16 }}
            animate={{ opacity: 1, x: 0 }}
            exit={{ opacity: 0, x: -16 }}
            transition={{ duration: 0.2, ease: easeOut }}
            autoComplete="off"
            className="pt-2"
            onSubmit={(e) => {
              e.preventDefault()
              void estimate(false)
            }}
          >
            <FieldGroup>
              {network.testnet && (
                <Alert>
                  <TriangleAlertIcon />
                  <AlertDescription>This is a testnet. Its tokens have no mainnet value.</AlertDescription>
                </Alert>
              )}
              {conversion && native && wrapped ? (
                <Field>
                  <FieldLabel>Direction</FieldLabel>
                  <ToggleGroup
                    value={[action]}
                    onValueChange={(v) => {
                      if (!v[0]) return
                      setAction(v[0] as "wrap" | "unwrap")
                      setAmount("")
                      invalidate()
                    }}
                    variant="outline"
                    spacing={0}
                    className="w-full"
                  >
                    <ToggleGroupItem value="wrap" className="flex-1">
                      {native.symbol} → {wrapped.symbol}
                    </ToggleGroupItem>
                    <ToggleGroupItem value="unwrap" className="flex-1">
                      {wrapped.symbol} → {native.symbol}
                    </ToggleGroupItem>
                  </ToggleGroup>
                  <FieldDescription>
                    1:1 on {network.name}. Funds stay in this wallet; keep {native.symbol} for gas.
                  </FieldDescription>
                </Field>
              ) : (
                <Field>
                  <FieldLabel>Asset</FieldLabel>
                  <AssetSelect
                    assets={networkAssets}
                    value={assetId}
                    onChange={(id) => {
                      setAssetId(id)
                      invalidate()
                    }}
                    balanceOf={(id) => balances.get(balanceKey(network.id, account.id, id))?.amount}
                  />
                </Field>
              )}
              {!conversion && (
                <Field>
                  <FieldLabel htmlFor="send-to">To</FieldLabel>
                  <InputGroup>
                    <InputGroupTextarea
                      id="send-to"
                      rows={1}
                      spellCheck={false}
                      autoCapitalize="off"
                      placeholder={addressPlaceholder[network.family]}
                      className="min-h-0 font-mono text-sm"
                      value={to}
                      onChange={(e) => {
                        setTo(e.target.value.replace(/\s/g, ""))
                        invalidate()
                      }}
                      onKeyDown={(e) => {
                        if (e.key === "Enter") {
                          e.preventDefault()
                          e.currentTarget.form?.requestSubmit()
                        }
                      }}
                    />
                  </InputGroup>
                </Field>
              )}
              <Field>
                <div className="flex items-baseline justify-between">
                  <FieldLabel htmlFor="send-amount">Amount</FieldLabel>
                  <span className="text-xs text-muted-foreground tabular">
                    Balance: {balance?.amount !== undefined ? formatAmount(balance.amount) : "…"} {symbol}
                  </span>
                </div>
                <InputGroup className="h-12">
                  <InputGroupInput
                    id="send-amount"
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
                    <span className="text-sm text-muted-foreground">{symbol}</span>
                    <InputGroupButton
                      variant="secondary"
                      size="xs"
                      onClick={() => void estimate(true)}
                      disabled={busy !== null}
                    >
                      {busy === "max" ? <Spinner /> : "MAX"}
                    </InputGroupButton>
                  </InputGroupAddon>
                </InputGroup>
                {amountUsd !== null && <FieldDescription className="tabular">≈ {formatUSD(amountUsd)}</FieldDescription>}
              </Field>
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
            {target && asset && (
              <Review
                account={account}
                network={network}
                address={address}
                asset={asset}
                approved={target}
                conversion={conversion ? { from: action === "wrap" ? native! : wrapped!, to: action === "wrap" ? wrapped! : native! } : null}
                usd={quote ? toNumber(target.body.amount) * toNumber(quote.current_usd) : null}
                onEdit={leaveReview}
                editDisabled={busy === "sign" || unsure}
              />
            )}
            {needsRefuel && !approved && (
              <RefuelPanel
                account={account}
                network={network}
                body={needsRefuel.body}
                estimate={needsRefuel.estimate}
                onReady={(estimate) => {
                  setApproved({ body: needsRefuel.body, estimate })
                  setNeedsRefuel(null)
                }}
              />
            )}
            {notice && (
              <Alert>
                <TriangleAlertIcon />
                <AlertDescription>{notice}</AlertDescription>
              </Alert>
            )}
            <FormError message={error} />
          </motion.div>
        )}
      </AnimatePresence>
      <DiscardAttemptDialog
        open={discarding}
        onOpenChange={setDiscarding}
        onDiscard={() => {
          if (idempotencyKey.current) clearUnsure(storeKey, idempotencyKey.current)
          setUnsure(false)
          invalidate()
          setStep("form")
        }}
      />
    </Screen>
  )
}

function AssetSelect({
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
  // Tokens without a balance sink to the bottom; native stays first.
  const sorted = [...assets].sort((a, b) => {
    if (a.kind === "native") return -1
    if (b.kind === "native") return 1
    return Number(isPositive(balanceOf(b.id))) - Number(isPositive(balanceOf(a.id)))
  })
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
          {sorted.map((asset) => (
            <SelectItem key={asset.id} value={asset.id}>
              <span className="flex w-full items-center gap-2">
                <TokenAvatar symbol={asset.symbol} className="size-6" />
                <span className="font-medium">{asset.symbol}</span>
                <span className="flex-1 truncate text-muted-foreground">{asset.name}</span>
                <span className="text-xs text-muted-foreground tabular">{formatAmount(balanceOf(asset.id))}</span>
              </span>
            </SelectItem>
          ))}
        </SelectGroup>
      </SelectContent>
    </Select>
  )
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1 py-2.5">
      <span className="text-xs text-muted-foreground">{label}</span>
      <div className="text-sm">{children}</div>
    </div>
  )
}

function Review({
  account,
  network,
  address,
  asset,
  approved,
  conversion,
  usd,
  onEdit,
  editDisabled,
}: {
  account: Account
  network: Network
  address: string
  asset: Asset
  approved: Approved
  conversion: { from: Asset; to: Asset } | null
  usd: number | null
  onEdit: () => void
  editDisabled: boolean
}) {
  const { body, estimate } = approved
  return (
    <>
      <div className="flex flex-col items-center gap-2 pt-2 pb-4 text-center">
        <TokenAvatar symbol={asset.symbol} className="size-12" />
        <div className="text-3xl font-semibold tracking-tight tabular">
          {formatAmount(body.amount, 8)} <span className="text-muted-foreground">{asset.symbol}</span>
        </div>
        {usd !== null && <div className="text-sm text-muted-foreground tabular">≈ {formatUSD(usd)}</div>}
        {conversion && (
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            {conversion.from.symbol} <ArrowDownIcon className="size-3.5 -rotate-90" /> {conversion.to.symbol} · 1:1
          </div>
        )}
      </div>
      <div className="flex flex-col divide-y rounded-2xl border px-4">
        <Row label="Network">
          {network.name} · chain {network.chain_id}
        </Row>
        <Row label={`From · ${accountTitle(account)}`}>
          <GroupedAddress value={address} />
        </Row>
        {!conversion && (
          <Row label="To">
            <GroupedAddress value={body.to} />
          </Row>
        )}
        {(asset.contract || conversion) && (
          <Row label={conversion ? "Wrapper contract" : asset.kind === "spl" ? "Token mint" : "Token contract"}>
            <GroupedAddress value={conversion ? network.wrapped_native!.contract : asset.contract!} />
          </Row>
        )}
        {network.family === "solana" ? (
          <SolanaFeeRows network={network} estimate={estimate} />
        ) : (
          <Row label="Network fee (max)">
            <span className="tabular">
              {formatAmount(estimate.fee, 8)} {network.native.symbol}
            </span>
            {estimate.gas_limit !== undefined && (
              <span className="block text-xs text-muted-foreground tabular">
                Gas limit {estimate.gas_limit.toLocaleString()} · {estimate.fee_model}
              </span>
            )}
            {isPositive(estimate.additional_fee) && (
              <span className="block text-xs text-muted-foreground tabular">
                Includes an L1 fee of about {formatAmount(estimate.additional_fee, 8)} {network.native.symbol}
              </span>
            )}
          </Row>
        )}
      </div>
      {estimate.warning && (
        <Alert>
          <TriangleAlertIcon />
          <AlertDescription>{estimate.warning}</AlertDescription>
        </Alert>
      )}
      <Button
        variant="ghost"
        size="sm"
        className="self-center text-muted-foreground"
        onClick={onEdit}
        disabled={editDisabled}
      >
        <PencilIcon data-icon="inline-start" />
        Edit
      </Button>
    </>
  )
}

// A Solana fee is exact: the compute-unit limit and price are signed as
// approved. The estimate's fee is the total, so a new recipient token account's
// rent is split out of it here for display only.
function SolanaFeeRows({ network, estimate }: { network: Network; estimate: TransferEstimate }) {
  const { decimals, symbol } = network.native
  const rent = estimate.creates_token_account ? (estimate.token_account_rent ?? "0") : "0"
  const opens = estimate.creates_token_account && isPositive(rent)
  return (
    <>
      <Row label="Network fee">
        <span className="tabular">
          {formatAmount(opens ? subtractDecimal(estimate.fee, rent, decimals) : estimate.fee, decimals)} {symbol}
        </span>
        {estimate.compute_unit_limit !== undefined && (
          <span className="block text-xs text-muted-foreground tabular">
            {estimate.compute_unit_limit.toLocaleString()} compute units · priority{" "}
            {(estimate.compute_unit_price_micro_lamports ?? 0).toLocaleString()} micro-lamports per unit
          </span>
        )}
      </Row>
      {opens && (
        <>
          <Row label="Recipient token account">
            <span className="tabular">
              {formatAmount(rent, decimals)} {symbol}
            </span>
            <span className="block text-xs text-muted-foreground">
              One-time rent to open the recipient&apos;s account for this token; paid by you.
            </span>
          </Row>
          <Row label="Total fee">
            <span className="tabular">
              {formatAmount(estimate.fee, decimals)} {symbol}
            </span>
          </Row>
        </>
      )}
    </>
  )
}
