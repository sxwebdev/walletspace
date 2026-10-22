import * as React from "react"
import { useQuery, useQueryClient } from "@tanstack/react-query"

import { networksApi, settingsApi } from "@/lib/api/chain"
import { APIError } from "@/lib/api/client"
import { spacesApi } from "@/lib/api/spaces"
import type { Network, Settings, Space } from "@/lib/api/types"
import { sessionGet, sessionSet } from "@/lib/storage"

const SPACE_KEY = "walletspace:space"

export const queryKeys = {
  spaces: ["spaces"] as const,
  networks: ["networks"] as const,
  settings: ["settings"] as const,
  overrides: ["settings", "networks"] as const,
  doctor: ["doctor"] as const,
  accounts: (spaceId: string) => ["accounts", spaceId] as const,
  assets: (networkId: string) => ["assets", networkId] as const,
  prices: (ids: string[]) => ["prices", ...ids] as const,
  resources: (spaceId: string, networkId: string, accountId: string) =>
    ["tron-resources", spaceId, networkId, accountId] as const,
  emptyTokenAccounts: (spaceId: string, networkId: string, accountId: string) =>
    ["empty-token-accounts", spaceId, networkId, accountId] as const,
}

type SessionState = {
  spaces: Space[]
  networks: Network[]
  enabledNetworks: Network[]
  networkById: Map<string, Network>
  settings: Settings | undefined
  space: Space | undefined
  selectSpace: (spaceId: string) => void
  // Called by anything that learns the space was locked behind the UI's back
  // (a 423, the auto-lock): the lock screen comes back at once.
  markLocked: () => void
  refetchSpaces: () => Promise<unknown>
}

const SessionContext = React.createContext<SessionState | null>(null)

export function useSession() {
  const context = React.useContext(SessionContext)
  if (!context) throw new Error("useSession must be used within SessionProvider")
  return context
}

export function isLockedError(error: unknown) {
  return error instanceof APIError && error.status === 423
}

export function useSessionQueries() {
  // Auto-lock happens on the server and says nothing; polling the space list
  // is how the widget notices and goes back to the lock screen.
  const spaces = useQuery({
    queryKey: queryKeys.spaces,
    queryFn: spacesApi.list,
    refetchInterval: 20_000,
    refetchOnWindowFocus: true,
  })
  const networks = useQuery({
    queryKey: queryKeys.networks,
    queryFn: networksApi.list,
    staleTime: 60_000,
  })
  const settings = useQuery({
    queryKey: queryKeys.settings,
    queryFn: settingsApi.get,
  })
  return { spaces, networks, settings }
}

export function SessionProvider({
  spaces,
  networks,
  settings,
  children,
}: {
  spaces: Space[]
  networks: Network[]
  settings: Settings | undefined
  children: React.ReactNode
}) {
  const queryClient = useQueryClient()
  const [selected, setSelected] = React.useState(() => sessionGet(SPACE_KEY))

  const space =
    spaces.find((item) => item.id === selected) ??
    spaces.find((item) => item.id === settings?.ui.last_space_id) ??
    spaces[0]

  const selectSpace = React.useCallback((spaceId: string) => {
    sessionSet(SPACE_KEY, spaceId)
    setSelected(spaceId)
  }, [])

  const markLocked = React.useCallback(() => {
    if (!space) return
    queryClient.setQueryData<{ spaces: Space[] }>(queryKeys.spaces, (data) =>
      data
        ? {
            spaces: data.spaces.map((item) =>
              item.id === space.id ? { ...item, locked: true } : item
            ),
          }
        : data
    )
  }, [queryClient, space])

  // Every query or mutation that hits a locked vault reports it here.
  React.useEffect(() => {
    const onError = (error: unknown) => {
      if (isLockedError(error)) markLocked()
    }
    const unsubscribeQueries = queryClient.getQueryCache().subscribe((event) => {
      if (event.type === "updated" && event.action.type === "error") {
        onError(event.action.error)
      }
    })
    const unsubscribeMutations = queryClient
      .getMutationCache()
      .subscribe((event) => {
        if (event.type === "updated" && event.action.type === "error") {
          onError(event.action.error)
        }
      })
    return () => {
      unsubscribeQueries()
      unsubscribeMutations()
    }
  }, [queryClient, markLocked])

  const value = React.useMemo<SessionState>(() => {
    const enabledNetworks = networks.filter((item) => item.enabled)
    return {
      spaces,
      networks,
      enabledNetworks,
      networkById: new Map(networks.map((item) => [item.id, item])),
      settings,
      space,
      selectSpace,
      markLocked,
      refetchSpaces: () =>
        queryClient.invalidateQueries({ queryKey: queryKeys.spaces }),
    }
  }, [spaces, networks, settings, space, selectSpace, markLocked, queryClient])

  return (
    <SessionContext.Provider value={value}>{children}</SessionContext.Provider>
  )
}

export function setSpaceLocked(
  queryClient: ReturnType<typeof useQueryClient>,
  spaceId: string,
  locked: boolean
) {
  queryClient.setQueryData<{ spaces: Space[] }>(queryKeys.spaces, (data) =>
    data
      ? {
          spaces: data.spaces.map((item) =>
            item.id === spaceId ? { ...item, locked } : item
          ),
        }
      : data
  )
}
