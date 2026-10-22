import { cn } from "cn"

import { Avatar, AvatarFallback } from "@/components/ui/avatar"

// No remote logos: the CSP serves images from the binary only, and fetching a
// token's icon would tell a third party which assets this wallet holds.
export function TokenAvatar({
  symbol,
  className,
}: {
  symbol: string
  className?: string
}) {
  const letters = symbol.replace(/[^A-Za-z0-9]/g, "").slice(0, 3) || "?"
  return (
    <Avatar className={cn("size-8", className)}>
      <AvatarFallback className="text-[10px] font-semibold tracking-tight">
        {letters.toUpperCase()}
      </AvatarFallback>
    </Avatar>
  )
}
