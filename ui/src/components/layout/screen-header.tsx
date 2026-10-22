import * as React from "react"
import { ChevronLeftIcon } from "lucide-react"
import { useNavigate } from "react-router"

import { HeaderBar } from "@/components/layout/header-bar"
import { Button } from "@/components/ui/button"

// A sub-screen's header: back, title, an optional action. back={false} is for
// screens that must not be left sideways, such as the recovery phrase that has
// to be acknowledged.
export function ScreenHeader({
  title,
  back,
  action,
}: {
  title: React.ReactNode
  back?: string | (() => void) | false
  action?: React.ReactNode
}) {
  const navigate = useNavigate()
  const onBack = () => {
    if (typeof back === "function") back()
    else if (typeof back === "string") navigate(back)
    else navigate(-1)
  }
  return (
    <HeaderBar
      left={
        back === false ? null : (
          <Button
            variant="ghost"
            size="icon-sm"
            onClick={onBack}
            aria-label="Back"
          >
            <ChevronLeftIcon />
          </Button>
        )
      }
      center={title}
      right={action}
    />
  )
}
