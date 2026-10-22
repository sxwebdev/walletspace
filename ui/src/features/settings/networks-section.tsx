import * as React from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"
import { AnimatePresence, motion } from "motion/react"
import { CheckCircle2Icon, ChevronDownIcon } from "lucide-react"

import { FormError } from "@/components/common/form-error"
import { PasswordInput } from "@/components/common/password-input"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Field, FieldContent, FieldDescription, FieldGroup, FieldLabel, FieldSet, FieldLegend } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { Switch } from "@/components/ui/switch"
import { Textarea } from "@/components/ui/textarea"
import { toast } from "@/components/ui/toast"
import { queryKeys, useSession } from "@/features/session/session"
import { SectionHeader } from "@/features/settings/settings"
import { settingsErrorMessage, useAdoptRevision } from "@/features/settings/use-settings-save"
import { settingsApi, type EndpointInput } from "@/lib/api/chain"
import type { Network, NetworkOverride } from "@/lib/api/types"

export function NetworksSection() {
  const { networks } = useSession()
  const overrides = useQuery({ queryKey: queryKeys.overrides, queryFn: settingsApi.networks })
  return (
    <div className="flex flex-col">
      <SectionHeader
        title="Networks & RPC"
        description="One HTTPS RPC URL per line. With no override, discovery and the official fallbacks are used."
      />
      {overrides.isPending ? (
        <Skeleton className="h-40 w-full" />
      ) : (
        <div className="flex flex-col divide-y rounded-2xl border">
          {networks.map((network) => (
            <NetworkRow key={network.id} network={network} override={overrides.data?.networks[network.id]} />
          ))}
        </div>
      )}
    </div>
  )
}

const parseUrls = (text: string) => text.split(/\s+/).map((s) => s.trim()).filter(Boolean)

function NetworkRow({ network, override }: { network: Network; override?: NetworkOverride }) {
  const queryClient = useQueryClient()
  const { settings } = useSession()
  const adopt = useAdoptRevision()
  const [open, setOpen] = React.useState(false)
  const initialEnabled = override?.enabled ?? network.enabled
  const [enabled, setEnabled] = React.useState(initialEnabled)
  const [urls, setUrls] = React.useState((override?.endpoints ?? []).map((e) => e.url).join("\n"))
  const [discovery, setDiscovery] = React.useState(
    override?.discovery_enabled === undefined ? "" : String(override.discovery_enabled)
  )
  const [explorer, setExplorer] = React.useState(override?.explorer ?? { address: "", tx: "", block: "" })
  const [headers, setHeaders] = React.useState<Record<string, { name: string; value: string; clear: boolean }>>({})
  const [error, setError] = React.useState<string | null>(null)
  const [busy, setBusy] = React.useState<"save" | "reset" | "test" | null>(null)
  const [tested, setTested] = React.useState(false)

  const list = parseUrls(urls)
  const stored = new Map((override?.endpoints ?? []).map((e) => [e.url, e]))

  // A secret is keyed by its URL, never by its line, so moving lines around
  // cannot attach a credential to the wrong endpoint.
  const endpoints = (): EndpointInput[] =>
    list.map((url) => {
      const h = headers[url]
      if (h?.clear) return { url, headers: {} }
      if (h?.name.trim() && h.value) return { url, headers: { [h.name.trim()]: h.value } }
      return { url }
    })

  const refreshNetworks = () => {
    void queryClient.invalidateQueries({ queryKey: queryKeys.networks })
    void queryClient.invalidateQueries({ queryKey: queryKeys.doctor })
  }

  const run = async (kind: "save" | "reset" | "test") => {
    setError(null)
    setTested(false)
    setBusy(kind)
    const revision = settings?.revision ?? ""
    try {
      if (kind === "test") {
        await settingsApi.testRPC(network.id, endpoints())
        setTested(true)
      } else if (kind === "save") {
        const anyExplorer = explorer.address || explorer.tx || explorer.block
        const result = await settingsApi.putNetwork(revision, network.id, {
          enabled,
          endpoints: endpoints(),
          discovery_enabled: discovery === "" ? null : discovery === "true",
          explorer: anyExplorer ? explorer : null,
        })
        adopt.overrides(result)
        setHeaders({})
        refreshNetworks()
        toast.add({ type: "success", title: `${network.name} saved` })
      } else {
        const result = await settingsApi.resetNetwork(revision, network.id)
        adopt.overrides(result)
        setUrls("")
        setHeaders({})
        setDiscovery("")
        setExplorer({ address: "", tx: "", block: "" })
        setEnabled(network.enabled)
        refreshNetworks()
        toast.add({ type: "success", title: `${network.name} reset to defaults` })
      }
    } catch (cause) {
      setError(settingsErrorMessage(cause))
    } finally {
      setBusy(null)
    }
  }

  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <div className="flex items-center gap-3 px-4 py-3">
        <CollapsibleTrigger className="flex min-w-0 flex-1 items-center gap-2 text-left">
          <motion.span animate={{ rotate: open ? 0 : -90 }} transition={{ duration: 0.2 }}>
            <ChevronDownIcon className="size-4 text-muted-foreground" />
          </motion.span>
          <span className="flex min-w-0 flex-col">
            <span className="flex items-center gap-2 truncate text-sm font-medium">
              {network.name}
              {network.testnet && <Badge variant="outline">testnet</Badge>}
              {override && <Badge variant="secondary">custom</Badge>}
            </span>
            <span className="text-xs text-muted-foreground">
              {network.family.toUpperCase()} · chain {network.chain_id}
            </span>
          </span>
        </CollapsibleTrigger>
        <Switch
          checked={enabled}
          onCheckedChange={(value) => {
            setEnabled(value)
            setOpen(true)
          }}
          aria-label={`Enable ${network.name}`}
        />
      </div>
      <CollapsibleContent>
        <form
          className="flex flex-col gap-5 px-4 pb-4"
          autoComplete="off"
          onSubmit={(e) => {
            e.preventDefault()
            void run("save")
          }}
        >
          <FieldGroup>
            <Field>
              <FieldLabel htmlFor={`rpc-${network.id}`}>RPC URLs</FieldLabel>
              <Textarea
                id={`rpc-${network.id}`}
                rows={2}
                spellCheck={false}
                className="font-mono text-xs"
                placeholder={network.rpc_fallbacks[0] ?? "https://"}
                value={urls}
                onChange={(e) => {
                  setUrls(e.target.value)
                  setTested(false)
                }}
              />
              <FieldDescription>${"{ENV}"} references are expanded on use and shown back as written.</FieldDescription>
            </Field>

            {list.length > 0 && (
              <FieldSet>
                <FieldLegend variant="label">Provider credentials</FieldLegend>
                {list.map((url) => {
                  const h = headers[url] ?? { name: "", value: "", clear: false }
                  const has = stored.get(url)?.has_headers
                  const update = (patch: Partial<typeof h>) => setHeaders((all) => ({ ...all, [url]: { ...h, ...patch } }))
                  return (
                    <div key={url} className="flex flex-col gap-2 rounded-xl bg-muted/40 p-3">
                      <span className="truncate font-mono text-xs text-muted-foreground">{url}</span>
                      <div className="grid gap-2 sm:grid-cols-[10rem_1fr]">
                        <Input
                          placeholder="Authorization"
                          value={h.name}
                          onChange={(e) => update({ name: e.target.value })}
                          aria-label="Header name"
                          disabled={h.clear}
                        />
                        <PasswordInput
                          autoComplete="new-password"
                          placeholder={has ? "Stored — leave empty to keep" : "Optional secret"}
                          value={h.value}
                          onChange={(e) => update({ value: e.target.value })}
                          aria-label="Header value"
                          disabled={h.clear}
                        />
                      </div>
                      {has && (
                        <Field orientation="horizontal">
                          <Checkbox
                            id={`clear-${network.id}-${url}`}
                            checked={h.clear}
                            onCheckedChange={(v) => update({ clear: Boolean(v) })}
                          />
                          <FieldLabel htmlFor={`clear-${network.id}-${url}`} className="font-normal">
                            Delete the stored credential
                          </FieldLabel>
                        </Field>
                      )}
                    </div>
                  )
                })}
              </FieldSet>
            )}

            <Collapsible>
              <CollapsibleTrigger render={<Button type="button" variant="ghost" size="sm" className="-ml-2 self-start text-muted-foreground" />}>
                Advanced
                <ChevronDownIcon data-icon="inline-end" />
              </CollapsibleTrigger>
              <CollapsibleContent>
                <FieldGroup className="pt-3">
                  <Field orientation="horizontal">
                    <FieldContent>
                      <FieldLabel>Node discovery</FieldLabel>
                    </FieldContent>
                    <Select value={discovery} onValueChange={(v) => setDiscovery(String(v ?? ""))}>
                      <SelectTrigger className="w-44">
                        <SelectValue>
                          {(v: string) => (v === "" ? "Global policy" : v === "true" ? "Enabled" : "Disabled")}
                        </SelectValue>
                      </SelectTrigger>
                      <SelectContent>
                        <SelectGroup>
                          <SelectItem value="">Global policy</SelectItem>
                          <SelectItem value="true">Enabled</SelectItem>
                          <SelectItem value="false">Disabled</SelectItem>
                        </SelectGroup>
                      </SelectContent>
                    </Select>
                  </Field>
                  {(["address", "tx", "block"] as const).map((key) => (
                    <Field key={key}>
                      <FieldLabel htmlFor={`explorer-${network.id}-${key}`}>Explorer {key} template</FieldLabel>
                      <Input
                        id={`explorer-${network.id}-${key}`}
                        className="font-mono text-xs"
                        placeholder={network.explorer[key]}
                        value={explorer[key]}
                        onChange={(e) => setExplorer((x) => ({ ...x, [key]: e.target.value }))}
                      />
                    </Field>
                  ))}
                </FieldGroup>
              </CollapsibleContent>
            </Collapsible>
            <FormError message={error} />
          </FieldGroup>
          <div className="flex flex-wrap items-center gap-2">
            <Button type="submit" disabled={busy !== null}>
              {busy === "save" && <Spinner data-icon="inline-start" />}
              Apply
            </Button>
            {list.length > 0 && (
              <Button type="button" variant="secondary" disabled={busy !== null} onClick={() => void run("test")}>
                {busy === "test" && <Spinner data-icon="inline-start" />}
                Test RPC
              </Button>
            )}
            {override && (
              <Button type="button" variant="ghost" disabled={busy !== null} onClick={() => void run("reset")}>
                {busy === "reset" && <Spinner data-icon="inline-start" />}
                Reset
              </Button>
            )}
            <AnimatePresence>
              {tested && (
                <motion.span
                  initial={{ opacity: 0, scale: 0.9 }}
                  animate={{ opacity: 1, scale: 1 }}
                  exit={{ opacity: 0 }}
                  className="flex items-center gap-1 text-sm text-success"
                >
                  <CheckCircle2Icon className="size-4" /> Chain verified
                </motion.span>
              )}
            </AnimatePresence>
          </div>
        </form>
      </CollapsibleContent>
    </Collapsible>
  )
}
