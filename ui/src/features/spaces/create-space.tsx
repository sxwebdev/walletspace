import * as React from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { AnimatePresence, motion } from "motion/react"
import { ChevronDownIcon, FileUpIcon, KeyRoundIcon, ShieldAlertIcon, SparklesIcon } from "lucide-react"
import { useNavigate, useSearchParams } from "react-router"

import { CopyButton } from "@/components/common/copy-button"
import { FormError } from "@/components/common/form-error"
import { Logo } from "@/components/common/logo"
import { easeOut, FadeIn } from "@/components/common/motion"
import { PasswordInput } from "@/components/common/password-input"
import { ScreenHeader } from "@/components/layout/screen-header"
import { Screen } from "@/components/layout/widget"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import {
  Field,
  FieldDescription,
  FieldGroup,
  FieldLabel,
} from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Item, ItemContent, ItemDescription, ItemMedia, ItemTitle } from "@/components/ui/item"
import { Spinner } from "@/components/ui/spinner"
import { Textarea } from "@/components/ui/textarea"
import { toast } from "@/components/ui/toast"
import { queryKeys } from "@/features/session/session"
import { errorMessage } from "@/lib/api/client"
import { firstSyncIncomplete, firstSyncIncompleteMessage, spacesApi } from "@/lib/api/spaces"
import type { Space } from "@/lib/api/types"
import { sessionSet } from "@/lib/storage"

const MIN_PASSWORD = 12

// The first screen of a wallet with no spaces.
export function Welcome() {
  const navigate = useNavigate()
  return (
    <div className="flex flex-1 flex-col items-center justify-between px-8 py-12">
      <div />
      <FadeIn className="flex flex-col items-center gap-5 text-center">
        <Logo className="size-14 rounded-3xl" />
        <div className="flex flex-col gap-2">
          <h1 className="text-2xl font-semibold tracking-tight">Walletspace</h1>
          <p className="text-sm text-balance text-muted-foreground">
            One encrypted space for your wallets on Tron, Solana and every EVM network.
            Keys never leave this machine.
          </p>
        </div>
      </FadeIn>
      <FadeIn delay={0.1} className="flex w-full flex-col gap-2">
        <Button size="lg" onClick={() => navigate("/create?mode=new")}>
          <SparklesIcon data-icon="inline-start" />
          Create a new space
        </Button>
        <Button size="lg" variant="secondary" onClick={() => navigate("/create?mode=restore")}>
          <KeyRoundIcon data-icon="inline-start" />I have a recovery phrase
        </Button>
        <Button size="lg" variant="ghost" onClick={() => navigate("/import")}>
          <FileUpIcon data-icon="inline-start" />
          Import from a backup file
        </Button>
        <Button variant="link" className="text-muted-foreground" onClick={() => navigate("/settings")}>
          Set up RPC nodes first
        </Button>
      </FadeIn>
    </div>
  )
}

// Choosing how an additional space starts.
export function NewSpaceChoice() {
  const navigate = useNavigate()
  const options = [
    {
      to: "/create?mode=new",
      mode: "new",
      icon: SparklesIcon,
      title: "New recovery phrase",
      description: "Generate a fresh 24-word phrase for this space.",
    },
    {
      to: "/create?mode=restore",
      mode: "restore",
      icon: KeyRoundIcon,
      title: "Restore from a phrase",
      description: "Bring an existing BIP39 recovery phrase.",
    },
    {
      to: "/import",
      mode: "file",
      icon: FileUpIcon,
      title: "From a backup file",
      description: "Spaces exported on another computer.",
    },
  ]
  return (
    <Screen header={<ScreenHeader title="New space" back="/" />}>
      <div className="flex flex-col gap-2 pt-2">
        {options.map((option, i) => (
          <FadeIn key={option.mode} delay={i * 0.05}>
            <Item
              variant="outline"
              render={<button type="button" onClick={() => navigate(option.to)} />}
              className="w-full text-left"
            >
              <ItemMedia variant="icon">
                <option.icon />
              </ItemMedia>
              <ItemContent>
                <ItemTitle>{option.title}</ItemTitle>
                <ItemDescription>{option.description}</ItemDescription>
              </ItemContent>
            </Item>
          </FadeIn>
        ))}
      </div>
    </Screen>
  )
}

type Mode = "new" | "restore"

export function CreateSpace({ first }: { first: boolean }) {
  const [params] = useSearchParams()
  const mode = (["new", "restore"].includes(params.get("mode") ?? "")
    ? params.get("mode")
    : "new") as Mode
  const navigate = useNavigate()
  const queryClient = useQueryClient()

  const [name, setName] = React.useState("")
  const [mnemonic, setMnemonic] = React.useState("")
  const [passphrase, setPassphrase] = React.useState("")
  const [password, setPassword] = React.useState("")
  const [confirmation, setConfirmation] = React.useState("")
  const [localError, setLocalError] = React.useState<string | null>(null)
  const [phrase, setPhrase] = React.useState<{ value: string; space: Space } | null>(null)

  const finish = (space: Space) => {
    sessionSet("walletspace:space", space.id)
    queryClient.setQueryData<{ spaces: Space[] }>(queryKeys.spaces, (data) => ({
      spaces: [...(data?.spaces ?? []).filter((s) => s.id !== space.id), space],
    }))
    void queryClient.invalidateQueries({ queryKey: queryKeys.spaces })
    navigate("/", { replace: true })
  }

  const create = useMutation({
    mutationFn: () =>
      spacesApi.create({
        name: name.trim(),
        password,
        mnemonic: mode === "restore" ? mnemonic.trim() : undefined,
        bip39_passphrase: passphrase || undefined,
        first: first || undefined,
      }),
    onSuccess: (result) => {
      setPassword("")
      setConfirmation("")
      setMnemonic("")
      setPassphrase("")
      if (result.mnemonic_generated && result.mnemonic) {
        setPhrase({ value: result.mnemonic, space: result.space })
      } else {
        if (firstSyncIncomplete(result.sync)) {
          // A restore still waiting for files is not shown as complete.
          toast.add({ type: "warning", ...firstSyncIncompleteMessage })
        } else {
          toast.add({ type: "success", title: mode === "restore" ? "Space restored" : "Space created" })
        }
        finish(result.space)
      }
    },
  })

  const submit = (event: React.FormEvent) => {
    event.preventDefault()
    setLocalError(null)
    if (password.length < MIN_PASSWORD) {
      setLocalError(`Use at least ${MIN_PASSWORD} characters.`)
      return
    }
    if (password !== confirmation) {
      setLocalError("The passwords do not match.")
      return
    }
    create.mutate()
  }

  if (phrase) {
    return <SavePhrase phrase={phrase.value} onDone={() => finish(phrase.space)} />
  }

  const title = mode === "restore" ? "Restore a space" : "Create a space"

  return (
    <Screen
      header={<ScreenHeader title={title} back={first ? "/" : "/spaces/new"} />}
      footer={
        <Button type="submit" form="create-space" size="lg" className="w-full" disabled={create.isPending}>
          {create.isPending && <Spinner data-icon="inline-start" />}
          {create.isPending ? "Encrypting the vault…" : mode === "restore" ? "Restore" : "Create"}
        </Button>
      }
    >
      <form id="create-space" onSubmit={submit} autoComplete="off" className="pt-2">
        <FieldGroup>
          <Field>
            <FieldLabel htmlFor="space-name">Name</FieldLabel>
            <Input
              id="space-name"
              placeholder="default"
              maxLength={80}
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </Field>
          {mode === "restore" && (
            <Field>
              <FieldLabel htmlFor="space-mnemonic">Recovery phrase</FieldLabel>
              <Textarea
                id="space-mnemonic"
                rows={4}
                required
                spellCheck={false}
                autoCapitalize="off"
                className="font-mono"
                placeholder="word1 word2 word3 …"
                value={mnemonic}
                onChange={(e) => setMnemonic(e.target.value)}
              />
              <FieldDescription>12 or 24 words, separated by spaces.</FieldDescription>
            </Field>
          )}
          <Field>
            <FieldLabel htmlFor="space-password">Password</FieldLabel>
            <PasswordInput
              id="space-password"
              autoComplete="new-password"
              required
              minLength={MIN_PASSWORD}
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
            <PasswordMeter value={password} />
          </Field>
          <Field>
            <FieldLabel htmlFor="space-password-2">Repeat the password</FieldLabel>
            <PasswordInput
              id="space-password-2"
              autoComplete="new-password"
              required
              value={confirmation}
              onChange={(e) => setConfirmation(e.target.value)}
            />
          </Field>
          <Collapsible>
            <CollapsibleTrigger
              render={
                <Button type="button" variant="ghost" size="sm" className="-ml-2 text-muted-foreground" />
              }
            >
              Advanced: BIP39 passphrase
              <ChevronDownIcon data-icon="inline-end" />
            </CollapsibleTrigger>
            <CollapsibleContent>
              <Field className="pt-3">
                <FieldLabel htmlFor="space-passphrase">BIP39 passphrase</FieldLabel>
                <PasswordInput
                  id="space-passphrase"
                  autoComplete="new-password"
                  value={passphrase}
                  onChange={(e) => setPassphrase(e.target.value)}
                />
                <FieldDescription>
                  Part of the derivation: it changes every address. It is not the space password.
                </FieldDescription>
              </Field>
            </CollapsibleContent>
          </Collapsible>
          <FormError message={localError ?? (create.isError ? errorMessage(create.error) : null)} />
        </FieldGroup>
      </form>
    </Screen>
  )
}

function PasswordMeter({ value }: { value: string }) {
  const score =
    value.length === 0
      ? 0
      : Math.min(
          4,
          (value.length >= MIN_PASSWORD ? 1 : 0) +
            (value.length >= 16 ? 1 : 0) +
            (/[A-Z]/.test(value) && /[a-z]/.test(value) ? 1 : 0) +
            (/\d/.test(value) || /[^A-Za-z0-9]/.test(value) ? 1 : 0)
        )
  const label = ["", "Weak", "Fair", "Good", "Strong"][score]
  return (
    <div className="flex items-center gap-2" aria-live="polite">
      <div className="flex flex-1 gap-1">
        {[1, 2, 3, 4].map((step) => (
          <motion.span
            key={step}
            className="h-1 flex-1 rounded-full bg-muted"
            animate={{ opacity: 1 }}
          >
            <motion.span
              className="block h-full rounded-full bg-primary"
              initial={false}
              animate={{ width: score >= step ? "100%" : "0%" }}
              transition={{ duration: 0.3, ease: easeOut }}
            />
          </motion.span>
        ))}
      </div>
      <span className="w-12 text-right text-xs text-muted-foreground">
        {value.length > 0 && value.length < MIN_PASSWORD ? `${value.length}/${MIN_PASSWORD}` : label}
      </span>
    </div>
  )
}

function SavePhrase({ phrase, onDone }: { phrase: string; onDone: () => void }) {
  const [saved, setSaved] = React.useState(false)
  const [revealed, setRevealed] = React.useState(false)
  const words = phrase.split(/\s+/)
  return (
    <Screen
      header={<ScreenHeader title="Recovery phrase" back={false} />}
      footer={
        <div className="flex flex-col gap-4">
          <Field orientation="horizontal">
            <Checkbox id="phrase-saved" checked={saved} onCheckedChange={(v) => setSaved(Boolean(v))} />
            <FieldLabel htmlFor="phrase-saved" className="font-normal">
              I have stored the phrase somewhere safe
            </FieldLabel>
          </Field>
          <Button size="lg" disabled={!saved} onClick={onDone}>
            Continue
          </Button>
        </div>
      }
    >
      <div className="flex flex-col gap-4 pt-2">
        <Alert>
          <ShieldAlertIcon />
          <AlertTitle>Write these words down</AlertTitle>
          <AlertDescription>
            Anyone with this phrase controls every derived wallet. Do not paste it into a chat or a cloud note.
          </AlertDescription>
        </Alert>
        <div className="relative">
          <ol
            className="grid grid-cols-3 gap-2 rounded-2xl border bg-muted/30 p-3 font-mono text-sm data-[blur=true]:blur-sm data-[blur=true]:select-none"
            data-blur={!revealed}
          >
            {words.map((word, i) => (
              <motion.li
                key={i}
                initial={{ opacity: 0, y: 4 }}
                animate={{ opacity: 1, y: 0 }}
                transition={{ delay: i * 0.015 }}
                className="flex items-baseline gap-1.5 rounded-lg bg-background px-2 py-1.5"
              >
                <span className="w-4 text-right text-[10px] text-muted-foreground tabular">{i + 1}</span>
                {word}
              </motion.li>
            ))}
          </ol>
          <AnimatePresence>
            {!revealed && (
              <motion.div
                exit={{ opacity: 0 }}
                className="absolute inset-0 flex items-center justify-center"
              >
                <Button variant="secondary" onClick={() => setRevealed(true)}>
                  Reveal the phrase
                </Button>
              </motion.div>
            )}
          </AnimatePresence>
        </div>
        {revealed && (
          <div className="flex justify-center">
            <CopyButton value={phrase} label="Copy phrase" variant="outline" size="icon" clearAfterMs={45_000} />
          </div>
        )}
      </div>
    </Screen>
  )
}
