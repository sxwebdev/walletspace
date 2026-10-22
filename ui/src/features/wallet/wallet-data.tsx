import * as React from "react"
import { useQueries, useQuery, useQueryClient } from "@tanstack/react-query"

import { queryKeys, useSession } from "@/features/session/session"
import { networksApi, streamBalances } from "@/lib/api/chain"
import { errorMessage } from "@/lib/api/client"
import { accountsApi } from "@/lib/api/spaces"
import type { Account, Asset, Balance, Network, PriceQuote } from "@/lib/api/types"
import { isPositive, toNumber } from "@/lib/decimal"
import { toast } from "@/components/ui/toast"

export type Holding = {
  account: Account
  network: Network
  asset: Asset
  balance: Balance
  usd: number | null
  previousUsd: number | null
}

export type Portfolio = {
  // Mainnet value in USD; testnets never count.
  totalUsd: number
  previousUsd: number | null
  change24h: number | null
  unpriced: number
  failedNetworks: number
  hasMainnet: boolean
}

type WalletData = {
  accounts: Account[]
  accountsLoading: boolean
  assets: Map<string, Asset[]>
  assetById: Map<string, Asset>
  balances: Map<string, Balance>
  balancesLoading: boolean
  prices: Map<string, PriceQuote>
  pricesLoading: boolean
  pricesStale: boolean
  holdings: Holding[]
  portfolio: Portfolio
  refreshBalances: (options?: { accountIds?: string[]; networkIds?: string[] }) => void
  upsertAccount: (account: Account) => void
}

const WalletContext = React.createContext<WalletData | null>(null)

export function useWallet() {
  const context = React.useContext(WalletContext)
  if (!context) throw new Error("useWallet must be used within WalletProvider")
  return context
}

export const balanceKey = (networkId: string, accountId: string, assetId: string) =>
  `${networkId}:${accountId}:${assetId}`

export function accountNetworks(account: Account) {
  return account.network_ids ?? []
}

export function accountAddress(account: Account, network: Network) {
  return account.addresses[network.family] ?? ""
}

export function accountTitle(account: Account) {
  if (account.label) return account.label
  if (account.kind === "imported") return "Imported wallet"
  return `Account ${account.index ?? ""}`.trim()
}

export function WalletProvider({ children }: { children: React.ReactNode }) {
  const { space, enabledNetworks, networkById } = useSession()
  const spaceId = space!.id
  const queryClient = useQueryClient()

  const accountsQuery = useQuery({
    queryKey: queryKeys.accounts(spaceId),
    queryFn: () => accountsApi.list(spaceId),
  })
  const accounts = React.useMemo(
    () => accountsQuery.data?.accounts ?? [],
    [accountsQuery.data]
  )

  // combine is memoized by TanStack Query, so the map only changes when one
  // of the asset lists does.
  const { assets, assetsReady } = useQueries({
    queries: enabledNetworks.map((network) => ({
      queryKey: queryKeys.assets(network.id),
      queryFn: () => networksApi.assets(network.id),
      staleTime: 5 * 60_000,
    })),
    combine: (results) => {
      const map = new Map<string, Asset[]>()
      enabledNetworks.forEach((network, i) => map.set(network.id, results[i]?.data?.assets ?? []))
      return { assets: map, assetsReady: results.every((query) => !query.isPending) }
    },
  })
  const assetById = React.useMemo(() => {
    const map = new Map<string, Asset>()
    for (const list of assets.values()) for (const asset of list) map.set(asset.id, asset)
    return map
  }, [assets])

  // Balances arrive one NDJSON line at a time; they are buffered and applied
  // once per frame so a large portfolio does not re-render per line.
  const [balances, setBalances] = React.useState(() => new Map<string, Balance>())
  const [pending, setPending] = React.useState(0)
  const [failedNetworks, setFailedNetworks] = React.useState(0)
  const [generation, setGeneration] = React.useState(0)
  const buffer = React.useRef(new Map<string, Balance>())
  const frame = React.useRef<number | null>(null)
  const mainStream = React.useRef<AbortController | null>(null)
  const side = React.useRef(new Set<AbortController>())

  const flush = React.useCallback(() => {
    frame.current = null
    if (buffer.current.size === 0) return
    const batch = buffer.current
    buffer.current = new Map()
    setBalances((current) => {
      const next = new Map(current)
      for (const [key, value] of batch) next.set(key, value)
      return next
    })
  }, [])

  const push = React.useCallback(
    (networkId: string, balance: Balance) => {
      buffer.current.set(balanceKey(networkId, balance.account_id, balance.asset_id), balance)
      if (frame.current === null) frame.current = requestAnimationFrame(flush)
    },
    [flush]
  )

  const run = React.useCallback(
    (options: { refresh?: boolean; accountIds?: string[]; networkIds?: string[]; main: boolean }) => {
      const controller = new AbortController()
      if (options.main) {
        mainStream.current?.abort()
        for (const c of side.current) c.abort()
        side.current.clear()
        mainStream.current = controller
        setFailedNetworks(0)
      } else {
        side.current.add(controller)
      }
      const targets = enabledNetworks.filter(
        (network) =>
          (!options.networkIds || options.networkIds.includes(network.id)) &&
          accounts.some(
            (account) =>
              accountNetworks(account).includes(network.id) &&
              (!options.accountIds || options.accountIds.includes(account.id))
          )
      )
      if (targets.length === 0) return
      setPending((n) => n + 1)
      void Promise.allSettled(
        targets.map((network) =>
          streamBalances(spaceId, network.id, {
            refresh: options.refresh,
            accountIds: options.accountIds,
            signal: controller.signal,
            onValue: (balance) => push(network.id, balance),
          })
        )
      ).then((results) => {
        side.current.delete(controller)
        setPending((n) => n - 1)
        if (controller.signal.aborted) return
        const failures = results.filter(
          (result): result is PromiseRejectedResult => result.status === "rejected"
        )
        if (failures.length) {
          setFailedNetworks((n) => Math.max(n, failures.length))
          toast.add({
            type: "error",
            title: `Balances did not load in ${failures.length} network${failures.length > 1 ? "s" : ""}`,
            description: errorMessage(failures[0].reason),
          })
        }
        setGeneration((n) => n + 1)
      })
    },
    [accounts, enabledNetworks, push, spaceId]
  )

  const accountsKey = accounts.map((a) => `${a.id}:${accountNetworks(a).join(",")}`).join("|")
  React.useEffect(() => {
    if (!accountsQuery.isSuccess) return
    // Opening the streams is the external system this effect syncs with; the
    // pending counter it bumps is part of that.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    run({ main: true })
    // Only a different set of wallets or networks needs a new full pass.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [accountsKey, accountsQuery.isSuccess, enabledNetworks])

  React.useEffect(
    () => () => {
      mainStream.current?.abort()
      for (const c of side.current) c.abort()
      if (frame.current !== null) cancelAnimationFrame(frame.current)
    },
    []
  )

  const refreshBalances = React.useCallback(
    (options?: { accountIds?: string[]; networkIds?: string[] }) => {
      if (!options?.accountIds && !options?.networkIds) run({ main: true, refresh: true })
      else run({ ...options, refresh: true, main: false })
    },
    [run]
  )

  const upsertAccount = React.useCallback(
    (account: Account) => {
      queryClient.setQueryData<{ accounts: Account[] }>(queryKeys.accounts(spaceId), (data) => {
        const list = data?.accounts ?? []
        const exists = list.some((item) => item.id === account.id)
        return {
          accounts: exists
            ? list.map((item) => (item.id === account.id ? account : item))
            : [...list, account],
        }
      })
    },
    [queryClient, spaceId]
  )

  // Prices for what is actually held on mainnets, after each balance pass.
  const priceIds = React.useMemo(() => {
    const ids = new Set<string>()
    for (const [key, balance] of balances) {
      const network = networkById.get(key.slice(0, key.indexOf(":")))
      if (!network || network.testnet || !network.enabled) continue
      if (!balance.error && isPositive(balance.amount)) ids.add(balance.asset_id)
    }
    return [...ids].sort()
    // Recomputed per completed pass, not per streamed line.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [generation, networkById])
  const pricesQuery = useQuery({
    queryKey: queryKeys.prices(priceIds),
    queryFn: () => networksApi.prices(priceIds),
    enabled: priceIds.length > 0,
    staleTime: 5 * 60_000,
    placeholderData: (previous) => previous,
  })
  const prices = React.useMemo(
    () => new Map((pricesQuery.data?.quotes ?? []).map((quote) => [quote.asset_id, quote])),
    [pricesQuery.data]
  )

  const accountById = React.useMemo(
    () => new Map(accounts.map((account) => [account.id, account])),
    [accounts]
  )

  const holdings = React.useMemo(() => {
    const list: Holding[] = []
    for (const [key, balance] of balances) {
      const network = networkById.get(key.slice(0, key.indexOf(":")))
      const account = accountById.get(balance.account_id)
      const asset = assetById.get(balance.asset_id)
      if (!network?.enabled || !account || !asset) continue
      if (!accountNetworks(account).includes(network.id)) continue
      if (balance.error || !isPositive(balance.amount)) continue
      const quote = network.testnet ? undefined : prices.get(asset.id)
      const amount = toNumber(balance.amount)
      const current = quote ? toNumber(quote.current_usd) : null
      const previous = quote?.has_previous_24h ? toNumber(quote.previous_24h_usd) : null
      list.push({
        account,
        network,
        asset,
        balance,
        usd: current === null ? null : amount * current,
        previousUsd: previous === null ? null : amount * previous,
      })
    }
    return list.sort((a, b) => (b.usd ?? -1) - (a.usd ?? -1))
  }, [balances, networkById, accountById, assetById, prices])

  const portfolio = React.useMemo<Portfolio>(() => {
    let totalUsd = 0
    let previousUsd = 0
    let historyComplete = true
    let unpriced = 0
    let hasMainnet = false
    for (const holding of holdings) {
      if (holding.network.testnet) continue
      hasMainnet = true
      if (holding.usd === null) {
        unpriced += 1
        continue
      }
      totalUsd += holding.usd
      if (holding.previousUsd === null) historyComplete = false
      else previousUsd += holding.previousUsd
    }
    const previous = historyComplete && previousUsd > 0 ? previousUsd : null
    return {
      totalUsd,
      previousUsd: previous,
      change24h: previous ? ((totalUsd - previous) / previous) * 100 : null,
      unpriced,
      failedNetworks,
      hasMainnet,
    }
  }, [holdings, failedNetworks])

  const value: WalletData = {
    accounts,
    accountsLoading: accountsQuery.isPending || !assetsReady,
    assets,
    assetById,
    balances,
    balancesLoading: pending > 0 || accountsQuery.isPending,
    prices,
    pricesLoading: pricesQuery.isFetching,
    pricesStale: pricesQuery.data?.stale ?? false,
    holdings,
    portfolio,
    refreshBalances,
    upsertAccount,
  }

  return <WalletContext.Provider value={value}>{children}</WalletContext.Provider>
}
