import * as React from "react"
import { useQuery } from "@tanstack/react-query"
import { BatteryChargingIcon, GaugeIcon, HourglassIcon } from "lucide-react"
import { Navigate, useNavigate } from "react-router"

import { FormError } from "@/components/common/form-error"
import { ScreenHeader } from "@/components/layout/screen-header"
import { Screen } from "@/components/layout/widget"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Progress } from "@/components/ui/progress"
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { toast } from "@/components/ui/toast"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { useSendConfirmation } from "@/features/security/send-confirmation"
import { queryKeys, useSession } from "@/features/session/session"
import { retryIsSafe } from "@/features/wallet/send/send"
import { inFlight, useTxTracker } from "@/features/wallet/send/tx-tracker"
import { TxResult } from "@/features/wallet/send/tx-result"
import { useAccountRoute } from "@/features/wallet/use-account-route"
import { useWallet } from "@/features/wallet/wallet-data"
import { tronApi, type TronAction, type TronActionBody } from "@/lib/api/chain"
import { errorMessage } from "@/lib/api/client"
import type { TronOperationResult, TronResource } from "@/lib/api/types"
import { isDecimalInput, toNumber } from "@/lib/decimal"
import { formatAmount, formatDateTime, shortAddress } from "@/lib/format"

const actions: { value: TronAction; label: string }[] = [
  { value: "stake", label: "Stake TRX" },
  { value: "unstake", label: "Unstake TRX" },
  { value: "delegate", label: "Delegate resource" },
  { value: "reclaim", label: "Reclaim resource" },
  { value: "withdraw", label: "Withdraw matured unstakes" },
  { value: "cancel-unstakes", label: "Cancel all unstakes" },
]

export function TronResourcesScreen() {
  const { account, network, loading } = useAccountRoute()
  const { space } = useSession()
  const { refreshBalances } = useWallet()
  const confirm = useSendConfirmation()
  const navigate = useNavigate()
  const spaceId = space!.id
  const [action, setAction] = React.useState<TronAction>("stake")
  const [resource, setResource] = React.useState<TronResource>("energy")
  const [amount, setAmount] = React.useState("")
  const [to, setTo] = React.useState("")
  const [all, setAll] = React.useState(false)
  const [busy, setBusy] = React.useState(false)
  const [error, setError] = React.useState<string | null>(null)
  const [result, setResult] = React.useState<TronOperationResult | null>(null)
  const last = React.useRef<{ fingerprint: string; key: string } | null>(null)

  const resources = useQuery({
    queryKey: queryKeys.resources(spaceId, network?.id ?? "", account?.id ?? ""),
    queryFn: () => tronApi.resources(spaceId, network!.id, account!.id),
    enabled: Boolean(account && network?.family === "tron"),
  })
  const tracked = useTxTracker(spaceId, network?.id ?? "", result?.tx_id, () => {
    refreshBalances({ accountIds: [account!.id], networkIds: [network!.id] })
    void resources.refetch()
  })

  if (loading) return null
  if (!account || !network || network.family !== "tron") return <Navigate to="/" replace />
  const back = `/wallet/${account.id}/${network.id}`

  if (result) {
    return (
      <Screen header={<div className="h-14 sm:hidden" />}>
        <TxResult
          network={network}
          txId={result.tx_id}
          status={inFlight(result.status) ? "unknown" : tracked === "timeout" ? "timeout" : tracked}
          summary={actions.find((a) => a.value === action)?.label ?? "Tron transaction"}
          onDone={() => {
            setResult(null)
            void resources.refetch()
          }}
        />
      </Screen>
    )
  }

  const needsResource = action !== "withdraw" && action !== "cancel-unstakes"
  const needsTo = action === "delegate" || action === "reclaim"
  const needsAmount = needsResource && !(action === "reclaim" && all)
  const unitsLabel = action === "stake" || action === "unstake" ? "TRX" : resource === "energy" ? "energy" : "bandwidth"

  const operationBody = (): TronActionBody => {
    if (!needsResource) return {}
    if (action === "stake" || action === "unstake") return { resource, amount: amount.trim() }
    if (action === "delegate") return { resource, amount: amount.trim(), to: to.trim() }
    return { resource, amount: all ? "" : amount.trim(), to: to.trim(), all }
  }

  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    if (busy) return
    const body = operationBody()
    // Resubmitting the same values reuses the key, so a retry after a lost
    // answer cannot sign twice.
    const fingerprint = JSON.stringify({ action, body })
    if (last.current?.fingerprint !== fingerprint) last.current = { fingerprint, key: crypto.randomUUID() }
    const key = last.current.key
    setBusy(true)
    setError(null)
    try {
      const response = await confirm(() => tronApi.operate(spaceId, network.id, account.id, action, body, key))
      last.current = null
      setResult(response)
    } catch (cause) {
      if (retryIsSafe(cause)) last.current = null
      const message = errorMessage(cause)
      setError(message)
      toast.add({ type: "error", title: "Not submitted", description: message })
    } finally {
      setBusy(false)
    }
  }

  const data = resources.data
  return (
    <Screen
      header={<ScreenHeader title="Resources & staking" back={back} />}
      footer={
        <Button type="submit" form="tron-op" size="lg" className="w-full" disabled={busy}>
          {busy && <Spinner data-icon="inline-start" />}
          {busy ? "Signing…" : "Sign and submit"}
        </Button>
      }
    >
      <div className="flex flex-col gap-6 pt-2">
        {resources.isPending ? (
          <Skeleton className="h-36 w-full rounded-2xl" />
        ) : resources.isError ? (
          <FormError message={errorMessage(resources.error)} />
        ) : data ? (
          <div className="flex flex-col gap-3">
            <Meter icon={BatteryChargingIcon} label="Energy" available={data.energy.available} total={data.energy.total} />
            <Meter icon={GaugeIcon} label="Bandwidth" available={data.bandwidth.available} total={data.bandwidth.total} />
            <div className="grid grid-cols-2 gap-2 text-sm">
              <Stat label="Staked for energy" value={`${formatAmount(data.staked_energy)} TRX`} />
              <Stat label="Staked for bandwidth" value={`${formatAmount(data.staked_bandwidth)} TRX`} />
              <Stat label="Unstaking" value={`${formatAmount(data.unstaking)} TRX`} />
              <Stat label="Withdrawable now" value={`${formatAmount(data.withdrawable_now)} TRX`} />
            </div>
            {(data.pending?.length ?? 0) > 0 && (
              <div className="flex flex-col gap-1 rounded-2xl border p-3 text-xs">
                <span className="flex items-center gap-1.5 font-medium">
                  <HourglassIcon className="size-3.5" /> Pending unstakes
                </span>
                {data.pending!.map((p, i) => (
                  <span key={i} className="text-muted-foreground">
                    {formatAmount(p.Amount)} TRX {p.Resource} · available {formatDateTime(p.ExpireAt)}
                  </span>
                ))}
              </div>
            )}
            {(data.delegations?.length ?? 0) > 0 && (
              <div className="flex flex-col gap-1 rounded-2xl border p-3 text-xs">
                <span className="font-medium">Delegations</span>
                {data.delegations!.map((d, i) => (
                  <span key={i} className="text-muted-foreground">
                    {formatAmount(d.Amount)} TRX {d.Resource} → {shortAddress(d.To)}
                  </span>
                ))}
              </div>
            )}
          </div>
        ) : null}

        <form id="tron-op" onSubmit={submit} autoComplete="off">
          <FieldGroup>
            <Field>
              <FieldLabel>Action</FieldLabel>
              <Select
                value={action}
                onValueChange={(v) => {
                  setAction(v as TronAction)
                  setError(null)
                }}
              >
                <SelectTrigger className="w-full">
                  <SelectValue>{(v: string) => actions.find((a) => a.value === v)?.label}</SelectValue>
                </SelectTrigger>
                <SelectContent>
                  <SelectGroup>
                    {actions.map((a) => (
                      <SelectItem key={a.value} value={a.value}>
                        {a.label}
                      </SelectItem>
                    ))}
                  </SelectGroup>
                </SelectContent>
              </Select>
            </Field>
            {needsResource && (
              <Field>
                <FieldLabel>Resource</FieldLabel>
                <ToggleGroup
                  value={[resource]}
                  onValueChange={(v) => v[0] && setResource(v[0] as TronResource)}
                  variant="outline"
                  spacing={0}
                  className="w-full"
                >
                  <ToggleGroupItem value="energy" className="flex-1">
                    Energy
                  </ToggleGroupItem>
                  <ToggleGroupItem value="bandwidth" className="flex-1">
                    Bandwidth
                  </ToggleGroupItem>
                </ToggleGroup>
              </Field>
            )}
            {needsTo && (
              <Field>
                <FieldLabel htmlFor="tron-to">Receiver</FieldLabel>
                <Input
                  id="tron-to"
                  spellCheck={false}
                  className="font-mono text-sm"
                  placeholder="T…"
                  value={to}
                  onChange={(e) => setTo(e.target.value.trim())}
                  required
                />
              </Field>
            )}
            {action === "reclaim" && (
              <Field orientation="horizontal">
                <Checkbox id="reclaim-all" checked={all} onCheckedChange={(v) => setAll(Boolean(v))} />
                <FieldLabel htmlFor="reclaim-all" className="font-normal">
                  Reclaim everything delegated to this receiver
                </FieldLabel>
              </Field>
            )}
            {needsAmount && (
              <Field>
                <FieldLabel htmlFor="tron-amount">Amount, {unitsLabel}</FieldLabel>
                <Input
                  id="tron-amount"
                  inputMode="decimal"
                  placeholder="0"
                  value={amount}
                  onChange={(e) => {
                    const v = e.target.value.replace(",", ".")
                    if (v === "" || isDecimalInput(v)) setAmount(v)
                  }}
                  required
                />
                {action === "delegate" && data && (
                  <FieldDescription>
                    Up to {formatAmount(resource === "energy" ? data.can_delegate_energy : data.can_delegate_bandwidth)} TRX
                    worth can be delegated.
                  </FieldDescription>
                )}
              </Field>
            )}
            <FormError message={error} />
          </FieldGroup>
        </form>
        <Button variant="ghost" size="sm" className="self-center text-muted-foreground" onClick={() => navigate(back)}>
          Back to the wallet
        </Button>
      </div>
    </Screen>
  )
}

function Meter({
  icon: Icon,
  label,
  available,
  total,
}: {
  icon: typeof GaugeIcon
  label: string
  available: string
  total: string
}) {
  const t = toNumber(total)
  const value = t > 0 ? (toNumber(available) / t) * 100 : 0
  return (
    <div className="flex flex-col gap-2 rounded-2xl border p-3">
      <div className="flex items-center justify-between text-sm">
        <span className="flex items-center gap-2 font-medium">
          <Icon className="size-4" /> {label}
        </span>
        <span className="text-muted-foreground tabular">
          {formatAmount(available, 0)} / {formatAmount(total, 0)}
        </span>
      </div>
      <Progress value={value} className="**:data-[slot=progress-track]:h-1.5" />
    </div>
  )
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex flex-col gap-0.5 rounded-2xl bg-muted/40 p-3">
      <span className="text-xs text-muted-foreground">{label}</span>
      <span className="font-medium tabular">{value}</span>
    </div>
  )
}
