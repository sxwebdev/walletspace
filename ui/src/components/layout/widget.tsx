import * as React from "react"
import { motion } from "motion/react"
import { cn } from "cn"

import { easeOut } from "@/components/common/motion"

// The whole wallet lives in one centered card, the way a browser-extension
// wallet does. Wide screens (settings, node health) widen the card in place
// rather than switching to a page layout.
export function Widget({
  wide = false,
  children,
}: {
  wide?: boolean
  children: React.ReactNode
}) {
  return (
    <div className="flex min-h-svh items-center justify-center bg-muted/40 p-0 sm:p-6">
      <motion.div
        layout
        transition={{ layout: { duration: 0.35, ease: easeOut } }}
        className={cn(
          "relative flex h-svh w-full flex-col overflow-hidden bg-background sm:h-[min(780px,calc(100svh-3rem))] sm:rounded-3xl sm:border sm:shadow-2xl sm:shadow-black/5",
          wide ? "sm:max-w-3xl" : "sm:max-w-[440px]"
        )}
      >
        {children}
      </motion.div>
    </div>
  )
}

// One screen inside the widget: a header row, then a body that scrolls on its
// own so the header and any footer action stay put.
export function Screen({
  header,
  footer,
  children,
  className,
}: {
  header?: React.ReactNode
  footer?: React.ReactNode
  children: React.ReactNode
  className?: string
}) {
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      {header}
      <div
        className={cn(
          "min-h-0 flex-1 overflow-y-auto overscroll-contain px-5 pb-5",
          className
        )}
      >
        {children}
      </div>
      {footer && <div className="border-t px-5 py-4">{footer}</div>}
    </div>
  )
}
