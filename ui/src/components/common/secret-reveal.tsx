import * as React from "react"
import { EyeOffIcon } from "lucide-react"

import { CopyButton } from "@/components/common/copy-button"
import { Progress } from "@/components/ui/progress"

const VISIBLE_MS = 90_000
const CLIPBOARD_MS = 45_000

async function clearClipboardIf(value: string) {
  try {
    if ((await navigator.clipboard.readText()) === value) {
      await navigator.clipboard.writeText("")
    }
  } catch {
    // Without clipboard-read permission the clipboard is left alone rather
    // than clobbering whatever the user copied since.
  }
}

// A revealed mnemonic or private key. It hides itself after 90 seconds and on
// unmount (dialog closed, screen left), and wipes the clipboard if it still
// holds the secret.
export function SecretReveal({
  value,
  label,
  words = false,
}: {
  value: string
  label: string
  words?: boolean
}) {
  const [hidden, setHidden] = React.useState(false)
  const [left, setLeft] = React.useState(VISIBLE_MS)
  const copied = React.useRef(false)

  React.useEffect(() => {
    const started = Date.now()
    const id = window.setInterval(() => {
      const remaining = VISIBLE_MS - (Date.now() - started)
      setLeft(Math.max(0, remaining))
      if (remaining <= 0) {
        setHidden(true)
        window.clearInterval(id)
      }
    }, 250)
    return () => window.clearInterval(id)
  }, [])

  React.useEffect(() => {
    if (hidden && copied.current) void clearClipboardIf(value)
  }, [hidden, value])

  React.useEffect(
    () => () => {
      if (copied.current) void clearClipboardIf(value)
    },
    [value]
  )

  if (hidden) {
    return (
      <div className="flex flex-col items-center gap-2 rounded-2xl border border-dashed p-6 text-center text-sm text-muted-foreground">
        <EyeOffIcon />
        {label} was hidden. Reveal it again if you still need it.
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-3">
      <div className="relative rounded-2xl border bg-muted/40 p-4">
        {words ? (
          <ol className="grid grid-cols-3 gap-x-3 gap-y-2 font-mono text-sm">
            {value.split(/\s+/).map((word, i) => (
              <li key={i} className="flex gap-1.5">
                <span className="w-5 text-right text-muted-foreground tabular">
                  {i + 1}
                </span>
                <span>{word}</span>
              </li>
            ))}
          </ol>
        ) : (
          <p className="font-mono text-sm break-all">{value}</p>
        )}
        <div
          className="absolute top-2 right-2"
          onClickCapture={() => {
            copied.current = true
            window.setTimeout(() => void clearClipboardIf(value), CLIPBOARD_MS)
          }}
        >
          <CopyButton value={value} label={`Copy ${label.toLowerCase()}`} />
        </div>
      </div>
      <div className="flex items-center gap-3 text-xs text-muted-foreground">
        <Progress value={(left / VISIBLE_MS) * 100} className="flex-1 **:data-[slot=progress-track]:h-1" />
        <span className="tabular">Hides in {Math.ceil(left / 1000)}s</span>
      </div>
    </div>
  )
}
