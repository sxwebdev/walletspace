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
import { useSendConfirmation } from "@/features/security/send-confirmation"
import { useSession } from "@/features/session/session"
import { GroupedAddress } from "@/features/wallet/send/grouped-address"
import { RefuelPanel } from "@/features/wallet/send/refuel"
import { inFlight, useTxTracker } from "@/features/wallet/send/tx-tracker"
import { TxResult } from "@/features/wallet/send/tx-result"
import { useAccountRoute } from "@/features/wallet/use-account-route"
import { accountAddress, accountNetworks, accountTitle, balanceKey, useWallet } from "@/features/wallet/wallet-data"
import { transfersApi } from "@/lib/api/chain"
import { APIError, errorMessage } from "@/lib/api/client"
import type { Account, Asset, Network, Operation, TransferBody, TransferEstimate } from "@/lib/api/types"
import { isDecimalInput, isPositive, toNumber } from "@/lib/decimal"
import { formatAmount, formatUSD } from "@/lib/format"

type Step = "form" | "review" | "result"
type Approved = { body: TransferBody; estimate: TransferEstimate }

// The server refuses to reuse a key only with this wording; every other
// failure keeps the key, so a retry cannot sign a second transaction.
export function retryIsSafe(error: unknown) {
  return error instanceof APIError && error.message.includes("new idempotency key")
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

  const [action, setAction] = React.useState<"wrap" | "unwrap">("wrap")
  const [assetId, setAssetId] = React.useState(() => {
    const requested = params.get("asset")
    return networkAssets.some((a) => a.id === requested) ? requested! : (native?.id ?? networkAssets[0]?.id ?? "")
  })
  const [to, setTo] = React.useState(conversion ? address : "")
  const [amount, setAmount] = React.useState("")
  const [step, setStep] = React.useState<Step>("form")
  const [approved, setApproved] = React.useState<Approved | null>(null)
  const [needsRefuel, setNeedsRefuel] = React.useState<Approved | null>(null)
  const [error, setError] = React.useState<string | null>(null)
  const [busy, setBusy] = React.useState<"estimate" | "max" | "sign" | null>(null)
  // The approval the sign button was armed for; a new approval disarms it.
  const [armedFor, setArmedFor] = React.useState<Approved | null>(null)
  const armed = armedFor !== null && armedFor === approved
  const [operation, setOperation] = React.useState<Operation | null>(null)
  const generation = React.useRef(0)
  const idempotencyKey = React.useRef<string | null>(null)

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
  }

  const body = (overrideAmount?: string): TransferBody => ({
    ...(conversion ? { action } : {}),
    account_id: account.id,
    asset_id: effectiveAssetId,
    to: conversion ? address : to.trim(),
    amount: overrideAmount ?? amount.trim(),
  })

  const present = (request: TransferBody, estimate: TransferEstimate) => {
    if (network.testnet && isPositive(estimate.fee_shortfall)) {
      setError(`Not enough ${network.native.symbol} for gas. Get testnet funds from the network's faucet.`)
      return
    }
    if (!conversion && network.family === "evm" && isPositive(estimate.fee_shortfall)) {
      setNeedsRefuel({ body: request, estimate })
      setStep("review")
      return
    }
    setApproved({ body: request, estimate })
    setStep("review")
  }

  const estimate = async (max: boolean) => {
    const current = ++generation.current
    setError(null)
    if (!conversion && !to.trim()) {
      setError(max ? "Enter the recipient first — the fee depends on it." : "Enter the recipient.")
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
      if (max) setAmount(result.amount)
      present({ ...request, amount: result.amount || request.amount }, result)
    } catch (cause) {
      if (current === generation.current) setError(errorMessage(cause))
    } finally {
      if (current === generation.current) setBusy(null)
    }
  }

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
    try {
      const result = await confirm(() =>
        transfersApi.send(
          spaceId,
          network.id,
          {
            ...approved.body,
            fee_model: fee.fee_model,
            gas_limit: fee.gas_limit,
            max_fee_per_gas: fee.max_fee_per_gas,
            max_priority_fee_per_gas: fee.max_priority_fee_per_gas,
          },
          key
        )
      )
      setOperation(result)
      setStep("result")
    } catch (cause) {
      if (retryIsSafe(cause)) idempotencyKey.current = null
      const message = errorMessage(cause)
      setError(message)
      toast.add({ type: "error", title: "Not sent", description: message })
      if (cause instanceof APIError && cause.status === 409) {
        // The fee moved or the key is taken: back to a fresh estimate.
        setApproved(null)
        idempotencyKey.current = null
      }
    } finally {
      setBusy(null)
    }
  }

  const txId = operation?.tx_hash
  const recipientAccount = accounts.find(
    (a) =>
      a.id !== account.id &&
      accountNetworks(a).includes(network.id) &&
      accountAddress(a, network).toLowerCase() === approved?.body.to.toLowerCase()
  )
  const tracked = useTxTracker(spaceId, network.id, step === "result" ? txId : undefined, (status) => {
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

  const title = conversion ? (action === "wrap" ? "Wrap" : "Unwrap") : "Send"
  const symbol = asset?.symbol ?? ""
  const back = `/wallet/${account.id}/${network.id}`

  if (step === "result" && operation && approved) {
    return (
      <Screen header={<div className="h-14 sm:hidden" />}>
        <TxResult
          network={network}
          txId={txId}
          status={inFlight(operation.status) ? "unknown" : tracked}
          summary={`${formatAmount(approved.body.amount)} ${symbol} · ${network.name}`}
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
          back={reviewing ? () => { invalidate(); setStep("form") } : back}
        />
      }
      footer={
        reviewing ? (
          approved ? (
            <Button size="lg" className="w-full" onClick={sign} disabled={!armed || busy === "sign"}>
              {busy === "sign" && <Spinner data-icon="inline-start" />}
              {busy === "sign" ? "Signing…" : conversion ? `Sign and ${action}` : "Sign and send"}
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
                      placeholder={network.family === "tron" ? "T…" : "0x…"}
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
                onEdit={() => {
                  invalidate()
                  setStep("form")
                }}
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
            <FormError message={error} />
          </motion.div>
        )}
      </AnimatePresence>
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
}: {
  account: Account
  network: Network
  address: string
  asset: Asset
  approved: Approved
  conversion: { from: Asset; to: Asset } | null
  usd: number | null
  onEdit: () => void
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
          <Row label={conversion ? "Wrapper contract" : "Token contract"}>
            <GroupedAddress value={conversion ? network.wrapped_native!.contract : asset.contract!} />
          </Row>
        )}
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
      </div>
      {estimate.warning && (
        <Alert>
          <TriangleAlertIcon />
          <AlertDescription>{estimate.warning}</AlertDescription>
        </Alert>
      )}
      <Button variant="ghost" size="sm" className="self-center text-muted-foreground" onClick={onEdit}>
        <PencilIcon data-icon="inline-start" />
        Edit
      </Button>
    </>
  )
}
