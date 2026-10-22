import * as React from "react"
import { ShieldCheckIcon } from "lucide-react"

import { FormError } from "@/components/common/form-error"
import { PasswordInput } from "@/components/common/password-input"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Spinner } from "@/components/ui/spinner"
import { useSession } from "@/features/session/session"
import { APIError, errorMessage } from "@/lib/api/client"
import { spacesApi } from "@/lib/api/spaces"

const NOT_CONFIRMED = "The transfer was not confirmed, so nothing was signed."

type Pending = { resolve: () => void; reject: (error: Error) => void }

type Confirm = <T>(run: () => Promise<T>) => Promise<T>

const ConfirmContext = React.createContext<Confirm | null>(null)

export function useSendConfirmation() {
  const context = React.useContext(ConfirmContext)
  if (!context) throw new Error("useSendConfirmation needs SendConfirmationProvider")
  return context
}

function needsConfirmation(error: unknown) {
  return (
    error instanceof APIError &&
    (error.payload as { code?: string } | null)?.code === "send_confirmation_required"
  )
}

// Unlocking a space is not authority to spend from it: with confirm_sends on,
// the server refuses a spend until the password is given again. This runs the
// request, asks for the password when refused, and re-runs the identical
// request (same body, same Idempotency-Key) exactly once.
export function SendConfirmationProvider({ children }: { children: React.ReactNode }) {
  const { space } = useSession()
  const [pending, setPending] = React.useState<Pending | null>(null)
  const [password, setPassword] = React.useState("")
  const [error, setError] = React.useState<string | null>(null)
  const [busy, setBusy] = React.useState(false)
  const inflight = React.useRef<{ controller: AbortController; done: Promise<boolean> } | null>(null)

  const ask = React.useCallback(
    () =>
      new Promise<void>((resolve, reject) => {
        setPassword("")
        setError(null)
        setPending({ resolve, reject })
      }),
    []
  )

  const confirm = React.useCallback<Confirm>(
    async (run) => {
      try {
        return await run()
      } catch (cause) {
        if (!needsConfirmation(cause)) throw cause
      }
      await ask()
      return run()
    },
    [ask]
  )

  const close = async () => {
    const current = pending
    setPending(null)
    if (!current) return
    const attempt = inflight.current
    if (!attempt) {
      current.reject(new Error(NOT_CONFIRMED))
      return
    }
    // The password may already have opened the window; only claim that
    // nothing was signed when it did not.
    attempt.controller.abort()
    if (await attempt.done) current.resolve()
    else current.reject(new Error(NOT_CONFIRMED))
  }

  const submit = async (event: React.FormEvent) => {
    event.preventDefault()
    if (!space || !pending || busy) return
    setBusy(true)
    setError(null)
    const controller = new AbortController()
    const done = spacesApi
      .confirmSend(space.id, password, controller.signal)
      .then(() => true)
      .catch((cause) => {
        if (!controller.signal.aborted) setError(errorMessage(cause))
        return false
      })
    inflight.current = { controller, done }
    const ok = await done
    inflight.current = null
    setBusy(false)
    setPassword("")
    if (ok) {
      const current = pending
      setPending(null)
      current.resolve()
    }
  }

  return (
    <ConfirmContext.Provider value={confirm}>
      {children}
      <Dialog open={pending !== null} onOpenChange={(open) => !open && void close()}>
        <DialogContent className="sm:max-w-sm">
          <form onSubmit={submit} className="flex flex-col gap-5" autoComplete="off">
            <DialogHeader>
              <div className="mb-2 flex size-10 items-center justify-center rounded-full bg-muted">
                <ShieldCheckIcon className="size-5" />
              </div>
              <DialogTitle>Confirm with your password</DialogTitle>
              <DialogDescription>
                Nothing has been signed yet. The transfer goes ahead exactly as
                shown once the password checks out.
              </DialogDescription>
            </DialogHeader>
            <FieldGroup>
              <Field>
                <FieldLabel htmlFor="confirm-password">Space password</FieldLabel>
                <PasswordInput
                  id="confirm-password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  autoFocus
                  required
                />
              </Field>
              <FormError message={error} />
            </FieldGroup>
            <DialogFooter>
              <Button type="submit" disabled={busy || !password} className="w-full">
                {busy && <Spinner data-icon="inline-start" />}
                Confirm and send
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </ConfirmContext.Provider>
  )
}
