import * as React from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import {
  ChevronRightIcon,
  DownloadIcon,
  FileKeyIcon,
  KeyRoundIcon,
  LockIcon,
  PencilIcon,
  ShieldPlusIcon,
  TriangleAlertIcon,
} from "lucide-react"

import { FormError } from "@/components/common/form-error"
import { PasswordInput } from "@/components/common/password-input"
import { SecretReveal } from "@/components/common/secret-reveal"
import { ScreenHeader } from "@/components/layout/screen-header"
import { Screen } from "@/components/layout/widget"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Item, ItemActions, ItemContent, ItemDescription, ItemGroup, ItemMedia, ItemTitle } from "@/components/ui/item"
import { Spinner } from "@/components/ui/spinner"
import { Switch } from "@/components/ui/switch"
import { Textarea } from "@/components/ui/textarea"
import { toast } from "@/components/ui/toast"
import { queryKeys, setSpaceLocked, useSession } from "@/features/session/session"
import { errorMessage } from "@/lib/api/client"
import { spacesApi } from "@/lib/api/spaces"
import type { Space } from "@/lib/api/types"
import { formatDateTime } from "@/lib/format"

type Action = "rename" | "password" | "phrase" | "add-phrase" | "backup" | null

export function SpaceSettings() {
  const { space, settings } = useSession()
  const queryClient = useQueryClient()
  const [action, setAction] = React.useState<Action>(null)
  const lock = useMutation({
    mutationFn: () => spacesApi.lock(space!.id),
    onSuccess: () => setSpaceLocked(queryClient, space!.id, true),
  })
  if (!space) return null
  const close = () => setAction(null)

  const items = [
    { key: "rename" as const, icon: PencilIcon, title: "Rename", description: space.name },
    { key: "password" as const, icon: KeyRoundIcon, title: "Change password", description: "Addresses and the phrase stay the same" },
    space.has_phrase
      ? { key: "phrase" as const, icon: FileKeyIcon, title: "Show recovery phrase", description: "Requires the password" }
      : {
          key: "add-phrase" as const,
          icon: ShieldPlusIcon,
          title: "Create a recovery phrase",
          description: "This space has none: it cannot be restored or synced",
        },
    { key: "backup" as const, icon: DownloadIcon, title: "Download encrypted backup", description: "The vault file, as stored" },
  ]

  return (
    <Screen header={<ScreenHeader title="Space & security" back="/" />}>
      <div className="flex flex-col gap-6 pt-2">
        <div className="flex flex-col items-center gap-1 text-center">
          <span className="flex size-14 items-center justify-center rounded-2xl bg-muted text-xl font-semibold uppercase">
            {space.name.slice(0, 1)}
          </span>
          <span className="pt-2 text-lg font-semibold">{space.name}</span>
          <span className="text-xs text-muted-foreground">
            {space.account_count} wallets · created {formatDateTime(space.created_at)}
          </span>
        </div>
        <ItemGroup className="gap-1">
          {items.map((item) => (
            <Item key={item.key} variant="outline" render={<button type="button" onClick={() => setAction(item.key)} />} className="w-full text-left">
              <ItemMedia variant="icon">
                <item.icon />
              </ItemMedia>
              <ItemContent>
                <ItemTitle>{item.title}</ItemTitle>
                <ItemDescription>{item.description}</ItemDescription>
              </ItemContent>
              <ItemActions>
                <ChevronRightIcon className="size-4 text-muted-foreground" />
              </ItemActions>
            </Item>
          ))}
        </ItemGroup>
        <p className="text-center text-xs text-muted-foreground">
          Auto-lock after {settings?.security.auto_lock ?? "—"} of inactivity.
        </p>
        <Button variant="secondary" onClick={() => lock.mutate()} disabled={lock.isPending}>
          <LockIcon data-icon="inline-start" />
          Lock now
        </Button>
      </div>
      <ActionDialog open={action === "rename"} onClose={close}>
        <RenameBody space={space} onClose={close} />
      </ActionDialog>
      <ActionDialog open={action === "password"} onClose={close}>
        <PasswordBody space={space} onClose={close} />
      </ActionDialog>
      <ActionDialog open={action === "phrase"} onClose={close} wide>
        <PhraseBody space={space} />
      </ActionDialog>
      <ActionDialog open={action === "add-phrase"} onClose={close} wide>
        <AddPhraseBody space={space} />
      </ActionDialog>
      <ActionDialog open={action === "backup"} onClose={close}>
        <BackupBody space={space} onClose={close} />
      </ActionDialog>
    </Screen>
  )
}

type BodyProps = { space: Space; onClose: () => void }

// The form lives in a body that exists only while its dialog is open, so a
// reopened dialog starts empty and a typed password does not survive it.
function ActionDialog({
  open,
  onClose,
  wide,
  children,
}: {
  open: boolean
  onClose: () => void
  wide?: boolean
  children: React.ReactNode
}) {
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className={wide ? "sm:max-w-md" : "sm:max-w-sm"}>{open && children}</DialogContent>
    </Dialog>
  )
}

function RenameBody({ space, onClose }: BodyProps) {
  const queryClient = useQueryClient()
  const [name, setName] = React.useState(space.name)
  const rename = useMutation({
    mutationFn: () => spacesApi.rename(space.id, name.trim()),
    onSuccess: (updated) => {
      queryClient.setQueryData<{ spaces: Space[] }>(queryKeys.spaces, (data) =>
        data ? { spaces: data.spaces.map((s) => (s.id === updated.id ? updated : s)) } : data
      )
      onClose()
    },
  })
  return (
    <>
        <form className="flex flex-col gap-5" onSubmit={(e) => (e.preventDefault(), rename.mutate())}>
          <DialogHeader>
            <DialogTitle>Rename space</DialogTitle>
          </DialogHeader>
          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="rename-space">Name</FieldLabel>
              <Input id="rename-space" value={name} onChange={(e) => setName(e.target.value)} required autoFocus maxLength={80} />
            </Field>
            <FormError message={rename.isError ? errorMessage(rename.error) : null} />
          </FieldGroup>
          <DialogFooter>
            <Button type="submit" disabled={!name.trim() || rename.isPending}>
              {rename.isPending && <Spinner data-icon="inline-start" />}
              Save
            </Button>
          </DialogFooter>
        </form>
    </>
  )
}

function PasswordBody({ space, onClose }: BodyProps) {
  const [current, setCurrent] = React.useState("")
  const [next, setNext] = React.useState("")
  const [repeat, setRepeat] = React.useState("")
  const [mismatch, setMismatch] = React.useState(false)
  const change = useMutation({
    mutationFn: () => spacesApi.changePassword(space.id, current, next),
    onSuccess: () => {
      toast.add({ type: "success", title: "Password changed" })
      onClose()
    },
  })
  return (
    <>
        <form
          className="flex flex-col gap-5"
          autoComplete="off"
          onSubmit={(e) => {
            e.preventDefault()
            setMismatch(next !== repeat)
            if (next === repeat) change.mutate()
          }}
        >
          <DialogHeader>
            <DialogTitle>Change password</DialogTitle>
            <DialogDescription>The vault is re-encrypted; addresses and the phrase stay the same.</DialogDescription>
          </DialogHeader>
          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="pw-current">Current password</FieldLabel>
              <PasswordInput id="pw-current" value={current} onChange={(e) => setCurrent(e.target.value)} required autoFocus />
            </Field>
            <Field>
              <FieldLabel htmlFor="pw-next">New password</FieldLabel>
              <PasswordInput id="pw-next" autoComplete="new-password" minLength={12} value={next} onChange={(e) => setNext(e.target.value)} required />
            </Field>
            <Field data-invalid={mismatch || undefined}>
              <FieldLabel htmlFor="pw-repeat">Repeat the new password</FieldLabel>
              <PasswordInput id="pw-repeat" autoComplete="new-password" minLength={12} value={repeat} onChange={(e) => setRepeat(e.target.value)} required aria-invalid={mismatch || undefined} />
            </Field>
            <FormError message={mismatch ? "The new passwords do not match." : change.isError ? errorMessage(change.error) : null} />
          </FieldGroup>
          <DialogFooter>
            <Button type="submit" disabled={change.isPending}>
              {change.isPending && <Spinner data-icon="inline-start" />}
              {change.isPending ? "Re-encrypting…" : "Change password"}
            </Button>
          </DialogFooter>
        </form>
    </>
  )
}

function PhraseBody({ space }: Pick<BodyProps, "space">) {
  const [password, setPassword] = React.useState("")
  const reveal = useMutation({
    mutationFn: () => spacesApi.revealMnemonic(space.id, password),
    onSettled: () => setPassword(""),
  })
  return (
    <>
        <DialogHeader>
          <DialogTitle>Recovery phrase</DialogTitle>
          <DialogDescription>The phrase is full control over every derived wallet in this space.</DialogDescription>
        </DialogHeader>
        {reveal.data ? (
          <SecretReveal value={reveal.data.mnemonic} label="Recovery phrase" words />
        ) : (
          <form className="flex flex-col gap-5" autoComplete="off" onSubmit={(e) => (e.preventDefault(), reveal.mutate())}>
            <Alert variant="destructive">
              <TriangleAlertIcon />
              <AlertDescription>Anyone who reads it can spend everything here. Do not photograph it or paste it into a chat.</AlertDescription>
            </Alert>
            <FieldGroup>
              <Field>
                <FieldLabel htmlFor="phrase-password">Space password</FieldLabel>
                <PasswordInput id="phrase-password" value={password} onChange={(e) => setPassword(e.target.value)} required autoFocus />
              </Field>
              <FormError message={reveal.isError ? errorMessage(reveal.error) : null} />
            </FieldGroup>
            <DialogFooter>
              <Button type="submit" variant="destructive" disabled={!password || reveal.isPending}>
                {reveal.isPending && <Spinner data-icon="inline-start" />}
                Reveal the phrase
              </Button>
            </DialogFooter>
          </form>
        )}
    </>
  )
}

// A space created without a phrase gets one. Nothing in it derives from the
// phrase, but the phrase is what restores the space — imported keys included —
// on another computer.
function AddPhraseBody({ space }: Pick<BodyProps, "space">) {
  const queryClient = useQueryClient()
  const [password, setPassword] = React.useState("")
  // A space copied to several computers before it had a phrase must get the
  // same phrase on all of them, or each copy syncs on its own.
  const [fromOther, setFromOther] = React.useState(false)
  const [existing, setExisting] = React.useState("")
  const add = useMutation({
    mutationFn: () => spacesApi.addRecoveryPhrase(space.id, password, fromOther ? existing.trim() : ""),
    onSettled: () => {
      setPassword("")
      setExisting("")
    },
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: queryKeys.spaces }),
  })
  return (
    <>
      <DialogHeader>
        <DialogTitle>Create a recovery phrase</DialogTitle>
        <DialogDescription>
          This space was made without a phrase, so its keys live only on this computer. With a phrase, the space comes
          back whole from three things together: the phrase, its BIP39 passphrase if it has one, and a current copy of
          the space in the sync folder. The phrase alone does not bring back imported keys.
        </DialogDescription>
      </DialogHeader>
      {add.data ? (
        <div className="flex flex-col gap-4">
          <Alert>
            <TriangleAlertIcon />
            <AlertDescription>
              Write these words down now and keep them offline. Anyone who reads them controls this space.
            </AlertDescription>
          </Alert>
          <SecretReveal value={add.data.mnemonic} label="Recovery phrase" words />
        </div>
      ) : (
        <form className="flex flex-col gap-5" autoComplete="off" onSubmit={(e) => (e.preventDefault(), add.mutate())}>
          <FieldGroup>
            <Field orientation="horizontal">
              <Switch id="add-phrase-other" checked={fromOther} onCheckedChange={setFromOther} />
              <FieldLabel htmlFor="add-phrase-other" className="font-normal">
                This space already got a phrase on another computer
              </FieldLabel>
            </Field>
            {fromOther ? (
              <Field>
                <FieldLabel htmlFor="add-phrase-existing">That phrase</FieldLabel>
                <Textarea
                  id="add-phrase-existing"
                  rows={3}
                  spellCheck={false}
                  autoCapitalize="off"
                  className="font-mono"
                  value={existing}
                  onChange={(e) => setExisting(e.target.value)}
                  required
                />
                <FieldDescription>
                  The copies of this space then sync with each other. A new phrase here would make this copy sync on
                  its own.
                </FieldDescription>
              </Field>
            ) : (
              <FieldDescription>
                If this space is on other computers too, create the phrase on one of them and enter it on the others.
              </FieldDescription>
            )}
            <Field>
              <FieldLabel htmlFor="add-phrase-password">Space password</FieldLabel>
              <PasswordInput
                id="add-phrase-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
                autoFocus
              />
            </Field>
            <FormError message={add.isError ? errorMessage(add.error) : null} />
          </FieldGroup>
          <DialogFooter>
            <Button type="submit" disabled={!password || (fromOther && !existing.trim()) || add.isPending}>
              {add.isPending ? <Spinner data-icon="inline-start" /> : <ShieldPlusIcon data-icon="inline-start" />}
              {fromOther ? "Use this phrase" : "Create the phrase"}
            </Button>
          </DialogFooter>
        </form>
      )}
    </>
  )
}

function BackupBody({ space, onClose }: BodyProps) {
  const [password, setPassword] = React.useState("")
  const backup = useMutation({
    mutationFn: () => spacesApi.backup(space.id, password),
    onSuccess: (blob) => {
      const url = URL.createObjectURL(blob)
      const link = document.createElement("a")
      link.href = url
      link.download = `${space.name || "walletspace"}-backup.json`
      link.click()
      setTimeout(() => URL.revokeObjectURL(url), 0)
      setPassword("")
      toast.add({ type: "success", title: "Encrypted backup downloaded" })
      onClose()
    },
  })
  return (
    <>
        <form className="flex flex-col gap-5" autoComplete="off" onSubmit={(e) => (e.preventDefault(), backup.mutate())}>
          <DialogHeader>
            <DialogTitle>Encrypted backup</DialogTitle>
            <DialogDescription>
              The file holds the vault encrypted with this password. Do not keep the password next to it.
            </DialogDescription>
          </DialogHeader>
          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="backup-password">Space password</FieldLabel>
              <PasswordInput id="backup-password" value={password} onChange={(e) => setPassword(e.target.value)} required autoFocus />
            </Field>
            <FormError message={backup.isError ? errorMessage(backup.error) : null} />
          </FieldGroup>
          <DialogFooter>
            <Button type="submit" disabled={!password || backup.isPending}>
              {backup.isPending ? <Spinner data-icon="inline-start" /> : <DownloadIcon data-icon="inline-start" />}
              Download
            </Button>
          </DialogFooter>
        </form>
    </>
  )
}
