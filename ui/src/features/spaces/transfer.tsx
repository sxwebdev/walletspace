import * as React from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { DownloadIcon, FileUpIcon, ShieldAlertIcon } from "lucide-react"
import { useNavigate } from "react-router"

import { FormError } from "@/components/common/form-error"
import { PasswordInput } from "@/components/common/password-input"
import { ScreenHeader } from "@/components/layout/screen-header"
import { Screen } from "@/components/layout/widget"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Field,
  FieldContent,
  FieldDescription,
  FieldGroup,
  FieldLabel,
} from "@/components/ui/field"
import { Spinner } from "@/components/ui/spinner"
import { Switch } from "@/components/ui/switch"
import { toast } from "@/components/ui/toast"
import { queryKeys, useSession } from "@/features/session/session"
import { SectionHeader } from "@/features/settings/settings"
import { SyncBlock } from "@/features/spaces/sync-section"
import { errorMessage } from "@/lib/api/client"
import { spacesApi } from "@/lib/api/spaces"
import type { BundleImport, Settings, Space } from "@/lib/api/types"

function saveFile(blob: Blob, name: string) {
  const url = URL.createObjectURL(blob)
  const link = document.createElement("a")
  link.href = url
  link.download = name
  link.click()
  setTimeout(() => URL.revokeObjectURL(url), 0)
}

const skipReason: Record<BundleImport["skipped"][number]["reason"], string> = {
  exists: "already on this computer",
  quota: "the space limit is reached",
}

// Reads a backup file and adds its spaces, locked. Used on its own screen
// before any space exists and in Settings afterwards.
export function ImportFromFile({
  onImported,
}: {
  onImported?: (result: BundleImport) => void
}) {
  const queryClient = useQueryClient()
  const input = React.useRef<HTMLInputElement>(null)
  const [file, setFile] = React.useState<File | null>(null)
  const [result, setResult] = React.useState<BundleImport | null>(null)
  const importing = useMutation({
    mutationFn: (selected: File) => spacesApi.importBundle(selected),
    onSuccess: (imported) => {
      setResult(imported)
      setFile(null)
      if (input.current) input.current.value = ""
      void queryClient.invalidateQueries({ queryKey: queryKeys.spaces })
      if (imported.imported.length > 0) {
        toast.add({
          type: "success",
          title: `${imported.imported.length} space${imported.imported.length === 1 ? "" : "s"} imported`,
        })
      }
      onImported?.(imported)
    },
  })

  return (
    <form
      className="flex flex-col gap-5"
      onSubmit={(e) => {
        e.preventDefault()
        if (file) importing.mutate(file)
      }}
    >
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor="import-file">Backup file</FieldLabel>
          <input
            ref={input}
            id="import-file"
            type="file"
            accept=".wsbackup,.json,application/json"
            className="text-sm file:mr-3 file:rounded-xl file:border-0 file:bg-muted file:px-3 file:py-2 file:text-sm file:font-medium"
            onChange={(e) => {
              setResult(null)
              setFile(e.target.files?.[0] ?? null)
            }}
          />
          <FieldDescription>
            A .wsbackup file exported from another computer, or one space's
            encrypted backup. Spaces arrive locked and open with the passwords
            they had.
          </FieldDescription>
        </Field>
        <FormError
          message={importing.isError ? errorMessage(importing.error) : null}
        />
      </FieldGroup>
      {result && (
        <Alert>
          <FileUpIcon />
          <AlertTitle>
            {result.imported.length === 0
              ? "Nothing new in this file"
              : `Imported: ${result.imported.map((s) => s.name).join(", ")}`}
          </AlertTitle>
          <AlertDescription>
            {result.skipped.length > 0 && (
              <span className="block">
                Skipped:{" "}
                {result.skipped
                  .map((s) => `${s.name} (${skipReason[s.reason] ?? s.reason})`)
                  .join(", ")}
                .
              </span>
            )}
            {result.skipped.length > 0
              ? "Keep the backup file until you have checked the skipped spaces: a space already here may be older than the one in the file."
              : "Delete the backup file now, here and on the computer it came from."}
          </AlertDescription>
        </Alert>
      )}
      <Button
        type="submit"
        className="self-start"
        disabled={!file || importing.isPending}
      >
        {importing.isPending ? (
          <Spinner data-icon="inline-start" />
        ) : (
          <FileUpIcon data-icon="inline-start" />
        )}
        Import
      </Button>
    </form>
  )
}

// The screen reached from the welcome page and the new-space choice.
export function ImportScreen({ first }: { first: boolean }) {
  const navigate = useNavigate()
  const { selectSpace } = useSession()
  return (
    <Screen
      header={
        <ScreenHeader
          title="Import from a file"
          back={first ? "/" : "/spaces/new"}
        />
      }
    >
      <div className="pt-2">
        <ImportFromFile
          onImported={(result) => {
            const opened = result.imported[0]
            // Skipped spaces stay on screen: the person has to see them
            // before deciding what to do with the file.
            if (!opened || result.skipped.length > 0) return
            selectSpace(opened.id)
            navigate("/", { replace: true })
          }}
        />
      </div>
    </Screen>
  )
}

function today() {
  return new Date().toISOString().slice(0, 10)
}

function ExportSpaces({ spaces }: { spaces: Space[] }) {
  const [selected, setSelected] = React.useState<Set<string>>(
    () => new Set(spaces.map((s) => s.id))
  )
  const [shared, setShared] = React.useState(true)
  const [password, setPassword] = React.useState("")
  const [passwords, setPasswords] = React.useState<Record<string, string>>({})
  const chosen = spaces.filter((s) => selected.has(s.id))
  const ready =
    chosen.length > 0 &&
    (shared
      ? password !== ""
      : chosen.every((s) => (passwords[s.id] ?? "") !== ""))

  const exporting = useMutation({
    mutationFn: () =>
      spacesApi.exportBundle(
        chosen.map((s) => ({
          id: s.id,
          password: shared ? password : (passwords[s.id] ?? ""),
        }))
      ),
    // Passwords go whatever the outcome, as the other password dialogs do:
    // a failed export must not leave them in the form to be revealed.
    onSettled: () => {
      setPassword("")
      setPasswords({})
    },
    onSuccess: (blob) => {
      saveFile(blob, `walletspace-${today()}.wsbackup`)
      toast.add({ type: "success", title: "Backup file downloaded" })
    },
  })

  const toggle = (id: string, on: boolean) =>
    setSelected((current) => {
      const next = new Set(current)
      if (on) next.add(id)
      else next.delete(id)
      return next
    })

  return (
    <form
      className="flex flex-col gap-5"
      autoComplete="off"
      onSubmit={(e) => {
        e.preventDefault()
        if (ready) exporting.mutate()
      }}
    >
      <FieldGroup>
        <Field>
          <FieldLabel>Spaces</FieldLabel>
          <div className="flex flex-col gap-2">
            {spaces.map((s) => (
              <Field key={s.id} orientation="horizontal">
                <Checkbox
                  id={`export-${s.id}`}
                  checked={selected.has(s.id)}
                  onCheckedChange={(v) => toggle(s.id, Boolean(v))}
                />
                <FieldLabel htmlFor={`export-${s.id}`} className="font-normal">
                  {s.name}
                  <span className="text-muted-foreground">
                    {" "}
                    · {s.account_count} wallets
                  </span>
                </FieldLabel>
              </Field>
            ))}
          </div>
        </Field>
        <Field orientation="horizontal">
          <FieldContent>
            <FieldLabel htmlFor="export-shared">
              The same password for every space
            </FieldLabel>
          </FieldContent>
          <Switch
            id="export-shared"
            checked={shared}
            onCheckedChange={(on) => {
              setShared(on)
              exporting.reset()
            }}
          />
        </Field>
        {shared ? (
          <Field>
            <FieldLabel htmlFor="export-password">Space password</FieldLabel>
            <PasswordInput
              id="export-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </Field>
        ) : (
          chosen.map((s) => (
            <Field key={s.id}>
              <FieldLabel htmlFor={`export-password-${s.id}`}>
                Password of {s.name}
              </FieldLabel>
              <PasswordInput
                id={`export-password-${s.id}`}
                value={passwords[s.id] ?? ""}
                onChange={(e) =>
                  setPasswords((current) => ({
                    ...current,
                    [s.id]: e.target.value,
                  }))
                }
              />
            </Field>
          ))
        )}
        <FormError
          message={exporting.isError ? errorMessage(exporting.error) : null}
        />
      </FieldGroup>
      <Button
        type="submit"
        className="self-start"
        disabled={!ready || exporting.isPending}
      >
        {exporting.isPending ? (
          <Spinner data-icon="inline-start" />
        ) : (
          <DownloadIcon data-icon="inline-start" />
        )}
        Download backup file
      </Button>
    </form>
  )
}

export function TransferSection({ settings }: { settings: Settings }) {
  const { spaces } = useSession()
  return (
    <div className="flex flex-col gap-10">
      {/* A new revision — saved here, in another section or in another tab —
          starts the form over from what is saved, so a save never sends old
          values under a new revision. */}
      <SyncBlock key={settings.revision} settings={settings} />
      <div className="flex flex-col">
        <SectionHeader
          title="Move spaces to another computer"
          description="One file with the selected spaces. Each vault in it stays encrypted with its own password."
        />
        <Alert className="mb-5">
          <ShieldAlertIcon />
          <AlertTitle>Carry it directly</AlertTitle>
          <AlertDescription>
            The file is as sensitive as the wallet's data folder. Move it by
            AirDrop, a USB stick or scp — not through a cloud drive or a
            messenger — and delete it on both computers after importing.
          </AlertDescription>
        </Alert>
        {spaces.length > 0 ? (
          <ExportSpaces spaces={spaces} />
        ) : (
          <p className="text-sm text-muted-foreground">
            There are no spaces to export yet.
          </p>
        )}
      </div>
      <div className="flex flex-col">
        <SectionHeader
          title="Import from a file"
          description="Add the spaces from a backup file to this computer."
        />
        <ImportFromFile />
      </div>
    </div>
  )
}
