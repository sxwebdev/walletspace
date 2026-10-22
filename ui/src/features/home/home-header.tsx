import { useMutation, useQueryClient } from "@tanstack/react-query"
import {
  EllipsisVerticalIcon,
  KeyRoundIcon,
  LockIcon,
  MonitorIcon,
  MoonIcon,
  ServerIcon,
  SettingsIcon,
  SunIcon,
} from "lucide-react"
import { useNavigate } from "react-router"

import { AppBar } from "@/components/layout/widget"
import { useTheme } from "@/components/theme-provider"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuSub,
  DropdownMenuSubContent,
  DropdownMenuSubTrigger,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { toast } from "@/components/ui/toast"
import { DoctorButton } from "@/features/home/doctor-status"
import { setSpaceLocked, useSession } from "@/features/session/session"
import { SpaceSwitcher } from "@/features/spaces/space-switcher"
import { errorMessage } from "@/lib/api/client"
import { spacesApi } from "@/lib/api/spaces"

// The bar over every unlocked screen: which space, node health, lock, menu.
export function AppHeader() {
  const { space } = useSession()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { theme, setTheme } = useTheme()

  const lock = useMutation({
    mutationFn: () => spacesApi.lock(space!.id),
    onSuccess: () => setSpaceLocked(queryClient, space!.id, true),
    onError: (error) =>
      toast.add({
        type: "error",
        title: "Could not lock",
        description: errorMessage(error),
      }),
  })

  return (
    <AppBar
      left={<SpaceSwitcher />}
      right={
        <>
          <DoctorButton />
          <Tooltip>
            <TooltipTrigger
              render={
                <Button
                  variant="ghost"
                  size="icon-sm"
                  onClick={() => lock.mutate()}
                  disabled={lock.isPending}
                  aria-label="Lock"
                />
              }
            >
              <LockIcon />
            </TooltipTrigger>
            <TooltipContent>Lock</TooltipContent>
          </Tooltip>
          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button variant="ghost" size="icon-sm" aria-label="Menu" />
              }
            >
              <EllipsisVerticalIcon />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-52">
              <DropdownMenuGroup>
                <DropdownMenuItem onClick={() => navigate("/space")}>
                  <KeyRoundIcon />
                  Space &amp; security
                </DropdownMenuItem>
                <DropdownMenuItem onClick={() => navigate("/nodes")}>
                  <ServerIcon />
                  Node health
                </DropdownMenuItem>
                <DropdownMenuItem onClick={() => navigate("/settings")}>
                  <SettingsIcon />
                  Settings
                </DropdownMenuItem>
              </DropdownMenuGroup>
              <DropdownMenuSeparator />
              <DropdownMenuSub>
                <DropdownMenuSubTrigger>
                  {theme === "dark" ? (
                    <MoonIcon />
                  ) : theme === "light" ? (
                    <SunIcon />
                  ) : (
                    <MonitorIcon />
                  )}
                  Theme
                </DropdownMenuSubTrigger>
                <DropdownMenuSubContent>
                  <DropdownMenuRadioGroup
                    value={theme}
                    onValueChange={(value) => setTheme(value as typeof theme)}
                  >
                    <DropdownMenuRadioItem value="light">
                      Light
                    </DropdownMenuRadioItem>
                    <DropdownMenuRadioItem value="dark">
                      Dark
                    </DropdownMenuRadioItem>
                    <DropdownMenuRadioItem value="system">
                      System
                    </DropdownMenuRadioItem>
                  </DropdownMenuRadioGroup>
                </DropdownMenuSubContent>
              </DropdownMenuSub>
            </DropdownMenuContent>
          </DropdownMenu>
        </>
      }
    />
  )
}
