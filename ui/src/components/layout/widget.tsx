import * as React from "react"
import { motion } from "motion/react"
import { cn } from "cn"

import { easeOut } from "@/components/common/motion"

// The whole wallet lives in one centered column, the way a browser-extension
// wallet does, with no card around it: page and widget share a background so
// a large screen shows a calm column rather than a small box in empty space.
// `top` is the app bar: it spans the window and stays put while the screens
// in the column change. Wide screens (settings) widen the column in place.
export function Widget({
  wide = false,
  top,
  children,
}: {
  wide?: boolean
  top?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <div className="flex h-svh flex-col bg-background">
      {top}
      <div className="flex min-h-0 flex-1 justify-center">
        <motion.div
          layout
          transition={{ layout: { duration: 0.35, ease: easeOut } }}
          className={cn(
            "relative flex h-full w-full flex-col overflow-hidden sm:pb-6",
            wide ? "sm:max-w-3xl" : "sm:max-w-md"
          )}
        >
          {children}
        </motion.div>
      </div>
    </div>
  )
}

// The app bar's row: left and right groups at the window's edges.
export function AppBar({ left, right }: { left?: React.ReactNode; right?: React.ReactNode }) {
  return (
    <div className="flex h-14 shrink-0 items-center justify-between gap-2 px-3 sm:h-16 sm:px-4">
      <div className="flex min-w-0 items-center">{left}</div>
      <div className="flex items-center gap-0.5">{right}</div>
    </div>
  )
}

// One screen inside the widget.
//
// On a phone: the header on top, a body that scrolls on its own, the footer
// action pinned to the bottom. On a wide window the header, the body and the
// footer form one block centered in the window, so nothing floats apart from
// the content it belongs to. my-auto rather than justify-center: a block
// taller than the window still scrolls from its top.
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
    <div className="flex min-h-0 flex-1 flex-col sm:overflow-y-auto sm:overscroll-contain">
      <div className="flex min-h-0 flex-1 flex-col sm:my-auto sm:flex-none">
        {header}
        <div
          className={cn(
            "min-h-0 flex-1 overflow-y-auto overscroll-contain px-5 pb-5 sm:flex-none sm:overflow-visible",
            className
          )}
        >
          {children}
          {footer && <div className="hidden pt-6 sm:block">{footer}</div>}
        </div>
      </div>
      {footer && <div className="border-t px-5 py-4 sm:hidden">{footer}</div>}
    </div>
  )
}
