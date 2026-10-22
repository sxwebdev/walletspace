import * as React from "react"
import { TriangleAlertIcon } from "lucide-react"
import { Navigate } from "react-router"

import { FormError } from "@/components/common/form-error"
import { ScreenHeader } from "@/components/layout/screen-header"
import { Screen } from "@/components/layout/widget"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { Textarea } from "@/components/ui/textarea"
import { toast } from "@/components/ui/toast"
import { useSendConfirmation } from "@/features/security/send-confirmation"
import { useSession } from "@/features/session/session"
import { GroupedAddress } from "@/features/wallet/send/grouped-address"
import { retryIsSafe } from "@/features/wallet/send/send"
import { inFlight } from "@/features/wallet/send/tx-tracker"
import { useAccountRoute } from "@/features/wallet/use-account-route"
import { useWallet } from "@/features/wallet/wallet-data"
import { tronApi } from "@/lib/api/chain"
import { errorMessage } from "@/lib/api/client"
import type { TronDeployBody, TronDeployEstimate, TronDeployResult } from "@/lib/api/types"

export function DeployScreen() {
  const { account, network, loading } = useAccountRoute()
  const { space } = useSession()
  const { refreshBalances } = useWallet()
  const confirm = useSendConfirmation()
  const [form, setForm] = React.useState({
    name: "",
    bytecode: "",
    abi: "",
    constructor_params: "",
    fee_limit: "100",
    consume_user_resource_percent: "100",
    origin_energy_limit: "10000000",
  })
  const [estimate, setEstimate] = React.useState<{ body: string; value: TronDeployEstimate } | null>(null)
  const [busy, setBusy] = React.useState(false)
  const [error, setError] = React.useState<string | null>(null)
  const [result, setResult] = React.useState<TronDeployResult | null>(null)
  const key = React.useRef<string | null>(null)

  if (loading) return null
  if (!account || !network || network.family !== "tron") return <Navigate to="/" replace />
  const back = `/wallet/${account.id}/${network.id}`

  const body = (): TronDeployBody => ({
    name: form.name,
    abi: form.abi,
    bytecode: form.bytecode.trim(),
    constructor_params: form.constructor_params,
    fee_limit: form.fee_limit.trim(),
    consume_user_resource_percent: Number(form.consume_user_resource_percent),
    origin_energy_limit: Number(form.origin_energy_limit),
  })

  // Any edit throws away the estimate and the key: the deployment signed is
  // the one that was estimated.
  const set = (field: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
    setForm((f) => ({ ...f, [field]: e.target.value }))
    setEstimate(null)
    key.current = null
  }

  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    if (busy) return
    const request = body()
    const serialized = JSON.stringify(request)
    setBusy(true)
    setError(null)
    try {
      if (!estimate || estimate.body !== serialized) {
        const value = await tronApi.deployEstimate(space!.id, network.id, account.id, request)
        setEstimate({ body: serialized, value })
        return
      }
      key.current ??= crypto.randomUUID()
      const k = key.current
      const response = await confirm(() => tronApi.deploy(space!.id, network.id, account.id, request, k))
      refreshBalances({ accountIds: [account.id], networkIds: [network.id] })
      if (inFlight(response.status)) {
        toast.add({
          type: "error",
          title: "The deployment status is unknown",
          description: "Check the transaction in the explorer before deploying again.",
        })
      } else if (response.failure) {
        toast.add({ type: "error", title: `Deploy failed: ${response.failure}` })
      } else {
        toast.add({ type: "success", title: "Contract deployed" })
      }
      setResult(response)
    } catch (cause) {
      if (retryIsSafe(cause)) key.current = null
      const message = errorMessage(cause)
      setError(message)
      toast.add({ type: "error", title: "Not deployed", description: message })
    } finally {
      setBusy(false)
    }
  }

  if (result) {
    return (
      <Screen header={<ScreenHeader title="Deployment" back={back} />}>
        <div className="flex flex-col gap-4 pt-2 text-sm">
          <div className="flex flex-col divide-y rounded-2xl border px-4">
            <div className="py-3">
              <div className="text-xs text-muted-foreground">Transaction</div>
              <GroupedAddress value={result.tx_id} />
            </div>
            {result.address && (
              <div className="py-3">
                <div className="text-xs text-muted-foreground">Contract address</div>
                <GroupedAddress value={result.address} />
              </div>
            )}
            <div className="py-3">
              <div className="text-xs text-muted-foreground">Result</div>
              {result.failure
                ? `Failed on chain: ${result.failure}`
                : result.confirmed
                  ? `Confirmed · energy ${result.energy_used ?? "—"} · fee ${result.fee ?? "—"} TRX`
                  : "Not confirmed yet — check the explorer"}
            </div>
          </div>
          <Button onClick={() => setResult(null)} variant="secondary">
            Deploy another
          </Button>
        </div>
      </Screen>
    )
  }

  return (
    <Screen
      header={<ScreenHeader title="Deploy contract" back={back} />}
      footer={
        <Button type="submit" form="deploy" size="lg" className="w-full" disabled={busy || !form.bytecode.trim()}>
          {busy && <Spinner data-icon="inline-start" />}
          {estimate ? "Sign and deploy" : "Estimate the deployment"}
        </Button>
      }
    >
      <form id="deploy" onSubmit={submit} autoComplete="off" className="pt-2">
        <FieldGroup>
          <Alert>
            <TriangleAlertIcon />
            <AlertDescription>A deployment spends energy even when it fails. Check the estimate first.</AlertDescription>
          </Alert>
          <Field>
            <FieldLabel htmlFor="d-name">Contract name</FieldLabel>
            <Input id="d-name" value={form.name} onChange={set("name")} />
          </Field>
          <Field>
            <FieldLabel htmlFor="d-bytecode">Bytecode (hex)</FieldLabel>
            <Textarea id="d-bytecode" rows={3} spellCheck={false} className="font-mono text-xs" value={form.bytecode} onChange={set("bytecode")} required />
          </Field>
          <Field>
            <FieldLabel htmlFor="d-abi">ABI JSON</FieldLabel>
            <Textarea id="d-abi" rows={3} spellCheck={false} className="font-mono text-xs" value={form.abi} onChange={set("abi")} />
          </Field>
          <Field>
            <FieldLabel htmlFor="d-params">Constructor params</FieldLabel>
            <Input id="d-params" spellCheck={false} className="font-mono text-xs" placeholder='[{"uint256":"1000"}]' value={form.constructor_params} onChange={set("constructor_params")} />
          </Field>
          <div className="grid grid-cols-3 gap-3">
            <Field>
              <FieldLabel htmlFor="d-fee">Fee limit, TRX</FieldLabel>
              <Input id="d-fee" inputMode="decimal" value={form.fee_limit} onChange={set("fee_limit")} required />
            </Field>
            <Field>
              <FieldLabel htmlFor="d-pct">User resource, %</FieldLabel>
              <Input id="d-pct" type="number" min={0} max={100} value={form.consume_user_resource_percent} onChange={set("consume_user_resource_percent")} />
            </Field>
            <Field>
              <FieldLabel htmlFor="d-energy">Origin energy</FieldLabel>
              <Input id="d-energy" type="number" min={0} value={form.origin_energy_limit} onChange={set("origin_energy_limit")} />
            </Field>
          </div>
          {estimate && (
            <div className="grid grid-cols-3 gap-2 rounded-2xl bg-muted/40 p-3 text-sm">
              <div>
                <div className="text-xs text-muted-foreground">Energy</div>
                {estimate.value.energy}
              </div>
              <div>
                <div className="text-xs text-muted-foreground">Fee</div>
                {estimate.value.fee} TRX
              </div>
              <div>
                <div className="text-xs text-muted-foreground">Min fee limit</div>
                {estimate.value.min_fee_limit} TRX
              </div>
            </div>
          )}
          <FormError message={error} />
        </FieldGroup>
      </form>
    </Screen>
  )
}
