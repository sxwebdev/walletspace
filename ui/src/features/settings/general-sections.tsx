import * as React from "react"

import { FormError } from "@/components/common/form-error"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldContent, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Spinner } from "@/components/ui/spinner"
import { Switch } from "@/components/ui/switch"
import { useSession } from "@/features/session/session"
import { SectionHeader } from "@/features/settings/settings"
import { settingsErrorMessage, useAdoptRevision, useSettingsMutation } from "@/features/settings/use-settings-save"
import { settingsApi } from "@/lib/api/chain"
import type { Settings } from "@/lib/api/types"

function SaveButton({ pending, children }: { pending: boolean; children: React.ReactNode }) {
  return (
    <Button type="submit" disabled={pending} className="self-start">
      {pending && <Spinner data-icon="inline-start" />}
      {children}
    </Button>
  )
}

export function GeneralSection({ settings }: { settings: Settings }) {
  const { spaces } = useSession()
  const adopt = useAdoptRevision()
  const [addr, setAddr] = React.useState(settings.server.addr)
  const [openBrowser, setOpenBrowser] = React.useState(settings.server.open_browser)
  // config.yaml can name a space that was since removed; the wallet already
  // ignores it, so the form starts from "Not set" and saving clears it.
  const savedSpace = settings.ui.last_space_id
  const savedSpaceMissing = savedSpace !== "" && !spaces.some((s) => s.id === savedSpace)
  const [lastSpace, setLastSpace] = React.useState(savedSpaceMissing ? "" : savedSpace)
  const save = useSettingsMutation(
    () =>
      settingsApi.patchGeneral(settings.revision, {
        server: { addr: addr.trim(), open_browser: openBrowser },
        ui: { last_space_id: lastSpace },
      }),
    adopt.settings
  )
  const restart = settings.restart_required.includes("server.addr")
  const spaceName = new Map(spaces.map((s) => [s.id, s.name]))

  return (
    <form className="flex flex-col gap-6" onSubmit={(e) => (e.preventDefault(), save.mutate())}>
      <SectionHeader title="General" description="Where the wallet listens and how it starts." />
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor="addr">Listen address</FieldLabel>
          <Input id="addr" value={addr} onChange={(e) => setAddr(e.target.value)} required className="font-mono" />
          <FieldDescription>
            Loopback only, e.g. 127.0.0.1:0 for a random port.
            {restart && " Saved now; the listener keeps its address until Walletspace restarts."}
          </FieldDescription>
        </Field>
        <Field orientation="horizontal">
          <FieldContent>
            <FieldLabel htmlFor="open-browser">Open a browser on start</FieldLabel>
          </FieldContent>
          <Switch id="open-browser" checked={openBrowser} onCheckedChange={setOpenBrowser} />
        </Field>
        <Field>
          <FieldLabel>Default space</FieldLabel>
          <Select value={lastSpace} onValueChange={(v) => setLastSpace(String(v ?? ""))}>
            <SelectTrigger className="w-full">
              <SelectValue>{(v: string) => (v ? (spaceName.get(v) ?? "Not set") : "Not set")}</SelectValue>
            </SelectTrigger>
            <SelectContent>
              <SelectGroup>
                <SelectItem value="">Not set</SelectItem>
                {spaces.map((s) => (
                  <SelectItem key={s.id} value={s.id}>
                    {s.name}
                  </SelectItem>
                ))}
              </SelectGroup>
            </SelectContent>
          </Select>
          {savedSpaceMissing && lastSpace === "" && (
            <FieldDescription>
              The saved default space no longer exists. Pick one, or save to clear it.
            </FieldDescription>
          )}
        </Field>
        <FormError message={save.isError ? settingsErrorMessage(save.error) : null} />
      </FieldGroup>
      <SaveButton pending={save.isPending}>Save</SaveButton>
    </form>
  )
}

export function SecuritySection({ settings }: { settings: Settings }) {
  const adopt = useAdoptRevision()
  const [autoLock, setAutoLock] = React.useState(settings.security.auto_lock)
  const [grant, setGrant] = React.useState(settings.security.send_grant_ttl)
  const readOnlyConfirm = settings.restart_required.includes("security.confirm_sends")
  const [confirmSends, setConfirmSends] = React.useState(settings.security.confirm_sends)
  const save = useSettingsMutation(
    () =>
      settingsApi.patchSecurity(settings.revision, {
        auto_lock: autoLock.trim(),
        // The server refuses any change to confirm_sends; the stored value is echoed.
        confirm_sends: readOnlyConfirm ? settings.security.confirm_sends : confirmSends,
        send_grant_ttl: grant.trim(),
      }),
    adopt.settings
  )
  return (
    <form className="flex flex-col gap-6" onSubmit={(e) => (e.preventDefault(), save.mutate())}>
      <SectionHeader title="Security" description="Auto-lock is counted separately for every space." />
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor="auto-lock">Auto-lock after</FieldLabel>
          <Input id="auto-lock" value={autoLock} onChange={(e) => setAutoLock(e.target.value)} placeholder="15m" required />
          <FieldDescription>
            Go duration between 1m and 24h (5m, 15m, 2h). It cannot be turned off: it takes the decrypted seed out of memory.
          </FieldDescription>
        </Field>
        <Field orientation="horizontal">
          <FieldContent>
            <FieldLabel htmlFor="confirm-sends" className="flex items-center gap-2">
              Ask for the password before sending
              {readOnlyConfirm && <Badge variant="outline">config.yaml</Badge>}
            </FieldLabel>
            <FieldDescription>
              {readOnlyConfirm
                ? "An unlocked space is reachable by anything on this machine that can talk to the wallet. The API refuses to change this; set security.confirm_sends in config.yaml and restart."
                : "Turning it off leaves the review screen as the only barrier before a transfer."}
            </FieldDescription>
          </FieldContent>
          <Switch
            id="confirm-sends"
            checked={readOnlyConfirm ? settings.security.confirm_sends : confirmSends}
            onCheckedChange={setConfirmSends}
            disabled={readOnlyConfirm}
          />
        </Field>
        <Field>
          <FieldLabel htmlFor="grant">One confirmation lasts</FieldLabel>
          <Input id="grant" value={grant} onChange={(e) => setGrant(e.target.value)} placeholder="5m" required />
          <FieldDescription>Between 1m and 1h. Locking the space ends it early.</FieldDescription>
        </Field>
        <FormError message={save.isError ? settingsErrorMessage(save.error) : null} />
      </FieldGroup>
      <SaveButton pending={save.isPending}>Save</SaveButton>
    </form>
  )
}

export function DiscoverySection({ settings }: { settings: Settings }) {
  const adopt = useAdoptRevision()
  const current = settings.node_discovery
  const [enabled, setEnabled] = React.useState(current.enabled)
  const [url, setUrl] = React.useState(current.url)
  const [refresh, setRefresh] = React.useState(current.refresh_interval)
  const [timeout, setTimeoutValue] = React.useState(current.request_timeout)
  const [insecure, setInsecure] = React.useState(current.allow_insecure_rpc)
  const save = useSettingsMutation(
    () =>
      settingsApi.patchDiscovery(settings.revision, {
        enabled,
        url: url.trim(),
        refresh_interval: refresh.trim(),
        request_timeout: timeout.trim(),
        allow_insecure_rpc: insecure,
      }),
    adopt.settings
  )
  return (
    <form className="flex flex-col gap-6" onSubmit={(e) => (e.preventDefault(), save.mutate())}>
      <SectionHeader
        title="Node discovery"
        description="Discovery suggests RPC candidates; chain id and address safety are verified locally."
      />
      <FieldGroup>
        <Field orientation="horizontal">
          <FieldContent>
            <FieldLabel htmlFor="discovery-enabled">Use node discovery</FieldLabel>
          </FieldContent>
          <Switch id="discovery-enabled" checked={enabled} onCheckedChange={setEnabled} />
        </Field>
        <Field>
          <FieldLabel htmlFor="discovery-url">Service URL</FieldLabel>
          <Input
            id="discovery-url"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="https://discovery.example"
            required={enabled}
            className="font-mono"
          />
        </Field>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field>
            <FieldLabel htmlFor="discovery-refresh">Refresh interval</FieldLabel>
            <Input id="discovery-refresh" value={refresh} onChange={(e) => setRefresh(e.target.value)} required />
          </Field>
          <Field>
            <FieldLabel htmlFor="discovery-timeout">Request timeout</FieldLabel>
            <Input id="discovery-timeout" value={timeout} onChange={(e) => setTimeoutValue(e.target.value)} required />
          </Field>
        </div>
        <Field orientation="horizontal">
          <FieldContent>
            <FieldLabel htmlFor="insecure" className="flex items-center gap-2">
              Allow HTTP and private RPC <Badge variant="destructive">not recommended</Badge>
            </FieldLabel>
          </FieldContent>
          <Switch id="insecure" checked={insecure} onCheckedChange={setInsecure} />
        </Field>
        <FormError message={save.isError ? settingsErrorMessage(save.error) : null} />
      </FieldGroup>
      <SaveButton pending={save.isPending}>Save</SaveButton>
    </form>
  )
}
