import { cn } from "cn"

import { CopyButton } from "@/components/common/copy-button"
import { shortAddress } from "@/lib/format"

export function Address({
  value,
  full = false,
  copy = true,
  className,
}: {
  value: string
  full?: boolean
  copy?: boolean
  className?: string
}) {
  return (
    <span className={cn("inline-flex min-w-0 items-center gap-1", className)}>
      <span
        className={cn(
          "font-mono text-xs text-muted-foreground",
          full ? "break-all" : "truncate"
        )}
        title={value}
      >
        {full ? value : shortAddress(value)}
      </span>
      {copy && <CopyButton value={value} label="Copy address" size="icon-xs" />}
    </span>
  )
}
