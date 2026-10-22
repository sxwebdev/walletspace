import * as React from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { motion, useAnimationControls } from "motion/react"
import { LockKeyholeIcon, SettingsIcon } from "lucide-react"
import { useNavigate } from "react-router"

import { FormError } from "@/components/common/form-error"
import { easeOut } from "@/components/common/motion"
import { PasswordInput } from "@/components/common/password-input"
import { Button } from "@/components/ui/button"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Spinner } from "@/components/ui/spinner"
import { setSpaceLocked, useSession } from "@/features/session/session"
import { SpaceSwitcher } from "@/features/spaces/space-switcher"
import { errorMessage } from "@/lib/api/client"
import { spacesApi } from "@/lib/api/spaces"

// The first thing a locked space shows. Nothing of the space — wallets,
// balances, addresses — is rendered until the password checks out. The shell
// keys it by space, so switching spaces starts from an empty form.
export function LockScreen() {
  const { space } = useSession()
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const shake = useAnimationControls()
  const [password, setPassword] = React.useState("")
  const input = React.useRef<HTMLInputElement>(null)

  const unlock = useMutation({
    mutationFn: () => spacesApi.unlock(space!.id, password),
    onSuccess: () => {
      setPassword("")
      setSpaceLocked(queryClient, space!.id, false)
    },
    onError: () => {
      void shake.start({ x: [0, -10, 10, -6, 6, 0], transition: { duration: 0.4 } })
      input.current?.select()
    },
  })

  if (!space) return null

  return (
    <div className="flex flex-1 flex-col">
      <div className="flex h-14 items-center justify-end px-3">
        <Button
          variant="ghost"
          size="icon-sm"
          onClick={() => navigate("/settings")}
          aria-label="Settings"
        >
          <SettingsIcon />
        </Button>
      </div>
      <div className="flex flex-1 flex-col items-center justify-center gap-8 px-8 pb-16">
        <motion.div
          initial={{ scale: 0.8, opacity: 0 }}
          animate={{ scale: 1, opacity: 1 }}
          transition={{ duration: 0.5, ease: easeOut }}
          className="relative flex size-20 items-center justify-center"
        >
          <motion.span
            className="absolute inset-0 rounded-full bg-muted"
            animate={{ scale: [1, 1.08, 1] }}
            transition={{ duration: 3, repeat: Infinity, ease: "easeInOut" }}
          />
          <LockKeyholeIcon className="relative size-8" />
        </motion.div>
        <div className="flex flex-col items-center gap-1 text-center">
          <h1 className="text-xl font-semibold tracking-tight">Welcome back</h1>
          <p className="text-sm text-muted-foreground">
            Enter the password to unlock this space
          </p>
        </div>
        <motion.form
          animate={shake}
          className="flex w-full flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            if (password && !unlock.isPending) unlock.mutate()
          }}
        >
          <div className="flex justify-center">
            <SpaceSwitcher align="center" />
          </div>
          <FieldGroup>
            <Field data-invalid={unlock.isError || undefined}>
              <FieldLabel htmlFor="unlock-password" className="sr-only">
                Password
              </FieldLabel>
              <PasswordInput
                ref={input}
                id="unlock-password"
                placeholder="Password"
                value={password}
                onChange={(event) => setPassword(event.target.value)}
                aria-invalid={unlock.isError || undefined}
                autoFocus
              />
            </Field>
            <FormError message={unlock.isError ? errorMessage(unlock.error) : null} />
          </FieldGroup>
          <Button type="submit" size="lg" disabled={!password || unlock.isPending}>
            {unlock.isPending && <Spinner data-icon="inline-start" />}
            {unlock.isPending ? "Unlocking…" : "Unlock"}
          </Button>
        </motion.form>
      </div>
    </div>
  )
}
