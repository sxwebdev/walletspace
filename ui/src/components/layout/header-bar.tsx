import * as React from "react"

// A screen's header row: left (back, space switcher), a centered title, right
// (actions). It is the first row of the screen's block, so on a wide window it
// stays attached to the content it controls instead of drifting to the
// window's edges.
export function HeaderBar({
  left,
  center,
  right,
}: {
  left?: React.ReactNode
  center?: React.ReactNode
  right?: React.ReactNode
}) {
  return (
    <div className="grid h-14 shrink-0 grid-cols-[1fr_auto_1fr] items-center gap-2 px-3">
      <div className="flex min-w-0 items-center justify-self-start">{left}</div>
      <div className="truncate text-center text-sm font-medium">{center}</div>
      <div className="flex items-center gap-0.5 justify-self-end">{right}</div>
    </div>
  )
}
