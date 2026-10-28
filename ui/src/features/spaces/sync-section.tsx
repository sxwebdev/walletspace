import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { CloudIcon, RefreshCwIcon, TriangleAlertIcon } from "lucide-react"

import { FormError } from "@/components/common/form-error"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Field,
  FieldContent,
  FieldDescription,
  FieldGroup,
  FieldLabel,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { Switch } from "@/components/ui/switch"
import { queryKeys, useSession } from "@/features/session/session"
import { SectionHeader } from "@/features/settings/settings"
import {
  settingsErrorMessage,
  useAdoptRevision,
  useSettingsMutation,
} from "@/features/settings/use-settings-save"
import { settingsApi } from "@/lib/api/chain"
import { errorMessage } from "@/lib/api/client"
import { syncApi } from "@/lib/api/spaces"
import type { Settings, Space, SyncStatus } from "@/lib/api/types"
import { formatDateTime } from "@/lib/format"

const stateLabel: Record<SyncStatus["state"], string> = {
  off: "Off",
  no_phrase: "No recovery phrase",
  locked: "Syncs when unlocked",
  ok: "Synced",
  error: "Failed",
}

// mergeFirstText says why this computer's file in the folder is not
// replaced yet, and what brings the space's changes there.
function mergeFirstText(status: SyncStatus): string {
  const held = "This space's changes are not sent until then."
  if (status.merge_first_reason === "newer_version") {
    return `This computer's file in the sync folder was written by a newer version of Walletspace, which this one cannot read. Update Walletspace on this computer to merge it. ${held}`
  }
  if (status.needs_unlock) {
    return "This computer's file in the sync folder holds changes this space may not have yet — for example after the data folder was restored from a backup. Unlock this space to merge them; its own changes are sent after that."
  }
  if (status.merge_first_reason === "merge_failed") {
    return `This computer's file in the sync folder could not be merged${status.error ? `: ${status.error}` : ""}. ${held}`
  }
  return `This computer's file in the sync folder is not downloaded yet. It is merged once it is. ${held}`
}

// deliveryLine says how far this computer's latest change to a space got.
function deliveryLine(
  status: SyncStatus
): { text: string; attention: boolean } | null {
  switch (status.delivery) {
    case "upload_unknown":
      return {
        text:
          "Latest change written to the sync folder" +
          (status.last_write ? ` (${formatDateTime(status.last_write)})` : "") +
          ". Whether it has reached the cloud cannot be checked from here: leave this computer on and online until the folder has synced.",
        attention: false,
      }
    case "unsent":
      if (status.merge_first) {
        return { text: mergeFirstText(status), attention: true }
      }
      if (status.needs_unlock) {
        return {
          text: "Changes made on this computer are not in the sync folder, and are not prepared to be sent. Unlock this space to send them.",
          attention: true,
        }
      }
      if (status.send_error) {
        return {
          text: `Changes made on this computer are not in the sync folder yet: ${status.send_error}. They stay queued and are sent when the folder answers, even with the space locked.`,
          attention: true,
        }
      }
      return {
        text: "Changes made on this computer are waiting to be written to the sync folder.",
        attention: true,
      }
    default:
      return null
  }
}

function SpaceSyncRow({
  space,
  status: last,
}: {
  space: Space
  status: SyncStatus
}) {
  // The server keeps a space's last pass after it locks; nothing merges until
  // it is unlocked again, so it is not shown as synced.
  const status: SyncStatus =
    space.locked && last.state !== "off" && last.state !== "no_phrase"
      ? { ...last, state: "locked" }
      : last
  const queryClient = useQueryClient()
  const now = useMutation({
    mutationFn: () => syncApi.now(space.id),
    onSettled: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.sync })
      void queryClient.invalidateQueries({
        queryKey: queryKeys.accounts(space.id),
      })
      void queryClient.invalidateQueries({ queryKey: queryKeys.spaces })
    },
  })
  const warnings: string[] = []
  if (status.state === "no_phrase") {
    warnings.push(
      "This space was made without a recovery phrase. Create one in the space's settings to sync it."
    )
  }
  if (status.error) warnings.push(status.error)
  if (status.dropped?.length) {
    warnings.push(
      `${status.dropped.length} wallet(s) from another computer did not match this space's keys and were not kept.`
    )
  }
  if (status.key_conflicts?.length) {
    warnings.push(
      `${status.key_conflicts.length} imported key reference(s) hold different keys on different computers. Every key is kept, and each wallet signs with the key of its own address.`
    )
  }
  if (status.duplicates?.length) {
    warnings.push(
      `${status.duplicates.length} wallet(s) appear twice — two computers created them before syncing. Delete the extra entry.`
    )
  }
  if (status.future_records) {
    warnings.push(
      "Another computer's clock is far ahead of this one. Fix the clock: times decide which change wins."
    )
  }
  if (status.missing > 0) {
    const why =
      status.missing_reason === "requests_failed"
        ? "Walletspace asked for them, and the requests keep failing."
        : "Walletspace cannot ask this folder's provider to download them."
    warnings.push(
      `${status.missing} file(s) from other computers are not downloaded. ${why} Keep the sync folder downloaded in your cloud app (in Finder: right-click the folder, Keep Downloaded). Until then this space may be missing what those computers changed.`
    )
  }
  const delivery = deliveryLine(status)
  return (
    <div className="flex flex-col gap-2 rounded-2xl border p-3">
      <div className="flex items-center gap-2">
        <span className="min-w-0 flex-1 truncate text-sm font-medium">
          {space.name}
        </span>
        <Badge variant={status.state === "error" ? "destructive" : "outline"}>
          {stateLabel[status.state] ?? status.state}
        </Badge>
        {!space.locked && status.state !== "no_phrase" && (
          <Button
            type="button"
            size="icon"
            variant="ghost"
            aria-label={`Sync ${space.name} now`}
            disabled={now.isPending}
            onClick={() => now.mutate()}
          >
            {now.isPending ? <Spinner /> : <RefreshCwIcon />}
          </Button>
        )}
      </div>
      {status.state === "ok" && (
        <p className="text-xs text-muted-foreground">
          {status.devices} computer(s) in the folder
          {status.last_sync && ` · checked ${formatDateTime(status.last_sync)}`}
          {status.last_write &&
            ` · this computer wrote ${formatDateTime(status.last_write)}`}
          {status.pending - status.missing > 0 &&
            ` · ${status.pending - status.missing} file(s) still downloading — not everything from the other computers is here yet`}
          {status.unreadable > 0 &&
            ` · ${status.unreadable} file(s) did not open`}
        </p>
      )}
      {delivery && (
        <p
          className={
            delivery.attention
              ? "text-xs text-amber-600 dark:text-amber-500"
              : "text-xs text-muted-foreground"
          }
        >
          {delivery.text}
        </p>
      )}
      {warnings.map((warning) => (
        <p key={warning} className="text-xs text-destructive">
          {warning}
        </p>
      ))}
      {now.isError && <FormError message={errorMessage(now.error)} />}
    </div>
  )
}

export function SyncBlock({ settings }: { settings: Settings }) {
  const { spaces } = useSession()
  const adopt = useAdoptRevision()
  const overview = useQuery({
    queryKey: queryKeys.sync,
    queryFn: syncApi.overview,
    refetchInterval: 15_000,
  })
  const [enabled, setEnabled] = React.useState(settings.sync.enabled)
  // null until the person edits the field: until then it shows the saved
  // folder, or the iCloud Drive suggestion when none is saved.
  const [folder, setFolder] = React.useState<string | null>(null)
  const suggested = overview.data?.suggested_folder ?? ""
  const shownFolder = folder ?? (settings.sync.folder || suggested)
  const queryClient = useQueryClient()
  const save = useSettingsMutation(
    () =>
      settingsApi.patchSync(settings.revision, {
        enabled,
        // Turning sync off keeps the saved folder: an edited one is checked
        // even when off, and must not stand in the way of turning it off.
        folder: enabled ? shownFolder.trim() : settings.sync.folder,
      }),
    (saved) => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.sync })
      // Adopting the new revision starts this form over (see TransferSection).
      adopt.settings(saved)
    }
  )

  return (
    <div className="flex flex-col">
      <SectionHeader
        title="Sync between computers"
        description="Keep every space current on the computers that hold its recovery phrase."
      />
      <Alert className="mb-5">
        <CloudIcon />
        <AlertTitle>What goes into the folder</AlertTitle>
        <AlertDescription>
          Each space writes one file there, encrypted with a key that only its
          recovery phrase produces: wallets, labels, networks and imported
          private keys. The phrase and the password never leave this computer.
          On a new computer, a space comes back whole only from all three: its
          phrase, its passphrase if it has one, and a current copy of its file
          in this folder. The phrase alone brings back the wallets made from it,
          not imported keys — keep the phrase on paper and this folder in sync.
          Turn sync on with the same folder on every computer. A space takes in
          other computers' changes while it is unlocked; its own changes are
          sent to the folder even after it locks.
        </AlertDescription>
      </Alert>
      <form
        className="flex flex-col gap-5"
        onSubmit={(e) => (e.preventDefault(), save.mutate())}
      >
        <FieldGroup>
          <Field orientation="horizontal">
            <FieldContent>
              <FieldLabel htmlFor="sync-enabled">Sync this computer</FieldLabel>
            </FieldContent>
            <Switch
              id="sync-enabled"
              checked={enabled}
              onCheckedChange={setEnabled}
            />
          </Field>
          <Field>
            <FieldLabel htmlFor="sync-folder">Folder</FieldLabel>
            <Input
              id="sync-folder"
              value={shownFolder}
              onChange={(e) => setFolder(e.target.value)}
              placeholder="/Users/you/Library/Mobile Documents/com~apple~CloudDocs/Walletspace"
              className="font-mono"
            />
            <FieldDescription>
              {suggested
                ? "A folder in iCloud Drive is filled in. Dropbox or a Syncthing folder works the same way."
                : "A folder that iCloud Drive, Dropbox or Syncthing copies between your computers."}{" "}
              It must not be inside the wallet's data folder.
            </FieldDescription>
            <FieldDescription>
              A disk or virtual-machine clone of this computer keeps its sync
              identity. Before using a clone, quit Walletspace on it, delete
              device.json in its data folder, start it again and unlock its
              spaces.
            </FieldDescription>
          </Field>
          <FormError
            message={save.isError ? settingsErrorMessage(save.error) : null}
          />
        </FieldGroup>
        <Button
          type="submit"
          className="self-start"
          disabled={save.isPending || (enabled && !shownFolder.trim())}
        >
          {save.isPending && <Spinner data-icon="inline-start" />}
          Save
        </Button>
      </form>
      {overview.data?.enabled && (
        <div className="flex flex-col gap-2 pt-6">
          {overview.data.error && (
            <Alert variant="destructive">
              <TriangleAlertIcon />
              <AlertDescription>{overview.data.error}</AlertDescription>
            </Alert>
          )}
          {overview.data.unidentified > 0 && (
            <Alert>
              <CloudIcon />
              <AlertDescription>
                {overview.data.unidentified} space(s) on this computer have not
                synced here yet. Unlock them before restoring anything: until
                then the folder cannot tell whether a space in it is one of
                them.
              </AlertDescription>
            </Alert>
          )}
          {overview.data.unknown > 0 && (
            <Alert>
              <CloudIcon />
              <AlertDescription>
                {overview.data.unknown} space(s) in the folder are not on this
                computer. Restore each with its recovery phrase, or import a
                backup file.
              </AlertDescription>
            </Alert>
          )}
          {spaces.map((space) => (
            <SpaceSyncRow
              key={space.id}
              space={space}
              status={
                overview.data.spaces[space.id] ?? {
                  state: "locked",
                  devices: 0,
                  pending: 0,
                  missing: 0,
                  unreadable: 0,
                  local_revision: 0,
                  outbox_revision: 0,
                  written_revision: 0,
                }
              }
            />
          ))}
        </div>
      )}
    </div>
  )
}
