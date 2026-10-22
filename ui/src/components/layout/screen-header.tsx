import * as React from "react"
import { ChevronLeftIcon } from "lucide-react"
import { useNavigate } from "react-router"

import { Button } from "@/components/ui/button"

// A sub-screen's title bar: back on the left, the title centered, an optional
// action on the right — the layout every wallet popup uses.
export function ScreenHeader({
  title,
  back,
  action,
}: {
  title: React.ReactNode
  back?: string | (() => void)
  action?: React.ReactNode
}) {
  const navigate = useNavigate()
  const onBack = () => {
    if (typeof back === "function") back()
    else if (typeof back === "string") navigate(back)
    else navigate(-1)
  }
  return (
    <div className="grid h-14 shrink-0 grid-cols-[2.5rem_1fr_2.5rem] items-center px-3">
      <Button variant="ghost" size="icon-sm" onClick={onBack} aria-label="Back">
        <ChevronLeftIcon />
      </Button>
      <div className="truncate text-center text-sm font-medium">{title}</div>
      <div className="flex justify-end">{action}</div>
    </div>
  )
}
