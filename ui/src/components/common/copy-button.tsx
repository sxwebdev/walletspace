import * as React from "react"
import { AnimatePresence, motion } from "motion/react"
import { CheckIcon, CopyIcon } from "lucide-react"

import { Button } from "@/components/ui/button"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"

export async function copyText(value: string) {
  await navigator.clipboard.writeText(value)
}

export function CopyButton({
  value,
  label = "Copy",
  size = "icon-sm",
  variant = "ghost",
  clearAfterMs,
}: {
  value: string
  label?: string
  size?: "icon-xs" | "icon-sm" | "icon"
  variant?: "ghost" | "outline" | "secondary"
  // Secrets are wiped from the clipboard after a while, if it still holds them.
  clearAfterMs?: number
}) {
  const [copied, setCopied] = React.useState(false)

  React.useEffect(() => {
    if (!copied) return
    const id = window.setTimeout(() => setCopied(false), 1500)
    return () => window.clearTimeout(id)
  }, [copied])

  const onCopy = async () => {
    try {
      await copyText(value)
      setCopied(true)
      if (clearAfterMs) {
        window.setTimeout(async () => {
          try {
            if ((await navigator.clipboard.readText()) === value) {
              await navigator.clipboard.writeText("")
            }
          } catch {
            // Reading the clipboard needs a permission; without it leave it.
          }
        }, clearAfterMs)
      }
    } catch {
      setCopied(false)
    }
  }

  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <Button
            type="button"
            variant={variant}
            size={size}
            onClick={onCopy}
            aria-label={label}
          />
        }
      >
        <AnimatePresence mode="wait" initial={false}>
          <motion.span
            key={copied ? "done" : "copy"}
            initial={{ scale: 0.6, opacity: 0 }}
            animate={{ scale: 1, opacity: 1 }}
            exit={{ scale: 0.6, opacity: 0 }}
            transition={{ duration: 0.12 }}
            className="inline-flex"
          >
            {copied ? <CheckIcon /> : <CopyIcon />}
          </motion.span>
        </AnimatePresence>
      </TooltipTrigger>
      <TooltipContent>{copied ? "Copied" : label}</TooltipContent>
    </Tooltip>
  )
}
