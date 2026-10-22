import { CheckIcon, ChevronsUpDownIcon, LockIcon, PlusIcon } from "lucide-react"
import { useNavigate } from "react-router"

import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { useSession } from "@/features/session/session"

function Initial({ name }: { name: string }) {
  return (
    <span className="flex size-6 shrink-0 items-center justify-center rounded-lg bg-muted text-[11px] font-semibold uppercase">
      {name.slice(0, 1) || "?"}
    </span>
  )
}

export function SpaceSwitcher({ align = "start" }: { align?: "start" | "center" }) {
  const { spaces, space, selectSpace } = useSession()
  const navigate = useNavigate()
  if (!space) return null

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={<Button variant="ghost" className="max-w-56 gap-2 px-2" />}
      >
        <Initial name={space.name} />
        <span className="truncate font-medium">{space.name}</span>
        <ChevronsUpDownIcon className="text-muted-foreground" data-icon="inline-end" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align={align} className="w-64">
        <DropdownMenuGroup>
          <DropdownMenuLabel>Spaces</DropdownMenuLabel>
          {spaces.map((item) => (
            <DropdownMenuItem key={item.id} onClick={() => selectSpace(item.id)}>
              <Initial name={item.name} />
              <span className="flex-1 truncate">{item.name}</span>
              {item.locked && <LockIcon className="text-muted-foreground" />}
              {item.id === space.id && <CheckIcon />}
            </DropdownMenuItem>
          ))}
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        <DropdownMenuGroup>
          <DropdownMenuItem onClick={() => navigate("/spaces/new")}>
            <PlusIcon />
            New space
          </DropdownMenuItem>
        </DropdownMenuGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
