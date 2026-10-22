import * as React from "react"
import { motion } from "motion/react"
import { cn } from "cn"
import { ArrowDownLeftIcon, ArrowUpRightIcon, PlusIcon, RefreshCwIcon, WalletIcon } from "lucide-react"
import { useNavigate } from "react-router"

import { AnimatedNumber, FadeIn, listItem } from "@/components/common/motion"
import { TokenAvatar } from "@/components/common/token-avatar"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty"
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { HomeHeader } from "@/features/home/home-header"
import { useSession } from "@/features/session/session"
import {
  accountAddress,
  accountNetworks,
  accountTitle,
  useWallet,
  type Holding,
} from "@/features/wallet/wallet-data"
import type { Account } from "@/lib/api/types"
import { formatAmount, formatPercent, formatUSD, shortAddress } from "@/lib/format"
import { sessionGet, sessionSet } from "@/lib/storage"

const FILTER_KEY = "walletspace:network-filter"
const TAB_KEY = "walletspace:home-tab"

export function Home() {
  const { networkById, enabledNetworks } = useSession()
  const wallet = useWallet()
  const [filter, setFilterState] = React.useState(() => sessionGet(FILTER_KEY) ?? "all")
  const [tab, setTabState] = React.useState(() => sessionGet(TAB_KEY) ?? "wallets")
  const setFilter = (value: string) => {
    sessionSet(FILTER_KEY, value)
    setFilterState(value)
  }
  const setTab = (value: string) => {
    sessionSet(TAB_KEY, value)
    setTabState(value)
  }

  const activeFilter = filter === "all" || networkById.has(filter) ? filter : "all"
  const usedNetworks = enabledNetworks.filter((network) =>
    wallet.accounts.some((account) => accountNetworks(account).includes(network.id))
  )

  const holdings = wallet.holdings.filter(
    (h) => activeFilter === "all" || h.network.id === activeFilter
  )
  const accounts = wallet.accounts.filter(
    (account) => activeFilter === "all" || accountNetworks(account).includes(activeFilter)
  )

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <HomeHeader />
      <div className="min-h-0 flex-1 overflow-y-auto overscroll-contain">
        <BalanceHero />
        <QuickActions hasAccounts={wallet.accounts.length > 0} />
        <div className="sticky top-0 z-10 flex items-center justify-between gap-2 bg-background/90 px-5 py-2 backdrop-blur">
          <Tabs value={tab} onValueChange={(value) => setTab(String(value))}>
            <TabsList>
              <TabsTrigger value="wallets">Wallets</TabsTrigger>
              <TabsTrigger value="assets">Assets</TabsTrigger>
            </TabsList>
          </Tabs>
          {usedNetworks.length > 1 && (
            <Select value={activeFilter} onValueChange={(value) => setFilter(String(value))}>
              <SelectTrigger size="sm" className="max-w-40">
                <SelectValue>
                  {(value: string) => (value === "all" ? "All networks" : networkById.get(value)?.name)}
                </SelectValue>
              </SelectTrigger>
              <SelectContent align="end">
                <SelectGroup>
                  <SelectItem value="all">All networks</SelectItem>
                  {usedNetworks.map((network) => (
                    <SelectItem key={network.id} value={network.id}>
                      {network.name}
                    </SelectItem>
                  ))}
                </SelectGroup>
              </SelectContent>
            </Select>
          )}
        </div>
        <div className="px-3 pb-5">
          {wallet.accountsLoading ? (
            <ListSkeleton />
          ) : wallet.accounts.length === 0 ? (
            <NoWallets />
          ) : tab === "wallets" ? (
            <WalletList accounts={accounts} networkFilter={activeFilter} />
          ) : (
            <AssetList holdings={holdings} loading={wallet.balancesLoading} />
          )}
        </div>
      </div>
    </div>
  )
}

function BalanceHero() {
  const { portfolio, balancesLoading, pricesLoading, refreshBalances, holdings } = useWallet()
  const calculating = (balancesLoading || pricesLoading) && portfolio.totalUsd === 0
  const testnetOnly = !portfolio.hasMainnet && holdings.length > 0
  const change = portfolio.change24h
  return (
    <div className="flex flex-col items-center gap-2 px-5 pt-6 pb-5 text-center">
      <div className="flex items-center gap-1 text-xs font-medium text-muted-foreground">
        Total balance
        <Button
          variant="ghost"
          size="icon-xs"
          onClick={() => refreshBalances()}
          aria-label="Refresh balances"
          disabled={balancesLoading}
        >
          <RefreshCwIcon className={cn(balancesLoading && "animate-spin")} />
        </Button>
      </div>
      {calculating ? (
        <Skeleton className="h-10 w-44 rounded-xl" />
      ) : (
        <div className="text-4xl font-semibold tracking-tight tabular">
          {portfolio.unpriced > 0 || portfolio.failedNetworks > 0 ? (
            <span className="text-muted-foreground">≈ </span>
          ) : null}
          <AnimatedNumber value={portfolio.totalUsd} format={formatUSD} />
        </div>
      )}
      <div className="flex h-5 items-center gap-2 text-xs text-muted-foreground">
        {change !== null ? (
          <Badge variant={change >= 0 ? "secondary" : "destructive"} className="tabular">
            {formatPercent(change)} · 24h
          </Badge>
        ) : testnetOnly ? (
          <span>Testnet balances have no USD value</span>
        ) : !calculating && portfolio.hasMainnet ? (
          <span>24h change unavailable</span>
        ) : null}
        {portfolio.unpriced > 0 && <span>{portfolio.unpriced} without a price</span>}
      </div>
    </div>
  )
}

function QuickActions({ hasAccounts }: { hasAccounts: boolean }) {
  const navigate = useNavigate()
  const actions = [
    { label: "Receive", icon: ArrowDownLeftIcon, to: "/receive", disabled: !hasAccounts },
    { label: "Send", icon: ArrowUpRightIcon, to: "/send", disabled: !hasAccounts },
    { label: "Add wallet", icon: PlusIcon, to: "/add-wallet", disabled: false },
  ]
  return (
    <div className="flex justify-center gap-6 px-5 pb-5">
      {actions.map((action, i) => (
        <FadeIn key={action.label} delay={0.05 * i}>
          <button
            type="button"
            disabled={action.disabled}
            onClick={() => navigate(action.to)}
            className="group flex w-16 flex-col items-center gap-1.5 text-xs font-medium disabled:opacity-40"
          >
            <span className="flex size-12 items-center justify-center rounded-full bg-primary text-primary-foreground transition-transform group-hover:scale-105 group-active:scale-95 group-disabled:scale-100">
              <action.icon className="size-5" />
            </span>
            {action.label}
          </button>
        </FadeIn>
      ))}
    </div>
  )
}

function NoWallets() {
  const navigate = useNavigate()
  return (
    <Empty className="py-10">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <WalletIcon />
        </EmptyMedia>
        <EmptyTitle>No wallets yet</EmptyTitle>
        <EmptyDescription>Create a wallet on a network, or import a private key.</EmptyDescription>
      </EmptyHeader>
      <EmptyContent>
        <Button onClick={() => navigate("/add-wallet")}>
          <PlusIcon data-icon="inline-start" />
          Add wallet
        </Button>
      </EmptyContent>
    </Empty>
  )
}

function ListSkeleton() {
  return (
    <div className="flex flex-col gap-1 px-2 pt-1">
      {[0, 1, 2].map((i) => (
        <div key={i} className="flex items-center gap-3 py-2.5">
          <Skeleton className="size-9 rounded-full" />
          <div className="flex flex-1 flex-col gap-1.5">
            <Skeleton className="h-3.5 w-28" />
            <Skeleton className="h-3 w-20" />
          </div>
          <Skeleton className="h-4 w-16" />
        </div>
      ))}
    </div>
  )
}

function WalletList({ accounts, networkFilter }: { accounts: Account[]; networkFilter: string }) {
  const { networkById } = useSession()
  const { holdings, balancesLoading } = useWallet()
  const navigate = useNavigate()

  if (accounts.length === 0) {
    return <p className="py-10 text-center text-sm text-muted-foreground">No wallets on this network.</p>
  }

  return (
    <ul className="flex flex-col">
      {accounts.map((account, i) => {
        const ids = accountNetworks(account)
        const own = holdings.filter(
          (h) => h.account.id === account.id && (networkFilter === "all" || h.network.id === networkFilter)
        )
        const usd = own.reduce((sum, h) => sum + (h.usd ?? 0), 0)
        const firstNetwork = networkById.get(networkFilter !== "all" ? networkFilter : ids[0] ?? "")
        const address = firstNetwork ? accountAddress(account, firstNetwork) : Object.values(account.addresses)[0]
        const target = firstNetwork ? `/wallet/${account.id}/${firstNetwork.id}` : `/wallet/${account.id}`
        return (
          <motion.li key={account.id} variants={listItem} initial="hidden" animate="show" custom={i}>
            <button
              type="button"
              onClick={() => navigate(target)}
              className="flex w-full items-center gap-3 rounded-2xl px-2 py-2.5 text-left transition-colors hover:bg-muted/60"
            >
              <span className="flex size-9 shrink-0 items-center justify-center rounded-full bg-muted text-sm font-semibold">
                {accountTitle(account).slice(0, 1).toUpperCase()}
              </span>
              <span className="flex min-w-0 flex-1 flex-col">
                <span className="flex items-center gap-1.5 truncate text-sm font-medium">
                  {accountTitle(account)}
                  {account.kind === "imported" && (
                    <Badge variant="outline" className="h-4 px-1.5 text-[10px] font-normal">
                      Imported
                    </Badge>
                  )}
                </span>
                <span className="truncate text-xs text-muted-foreground">
                  {address ? shortAddress(address) : "No network"}
                  {ids.length > 1 && networkFilter === "all" && ` · ${ids.length} networks`}
                  {ids.length === 1 && firstNetwork && ` · ${firstNetwork.name}`}
                </span>
              </span>
              <span className="text-right text-sm font-medium tabular">
                {balancesLoading && own.length === 0 ? (
                  <Skeleton className="h-4 w-14" />
                ) : own.some((h) => h.usd !== null) ? (
                  formatUSD(usd)
                ) : own.length > 0 ? (
                  <span className="text-xs text-muted-foreground">{own.length} assets</span>
                ) : (
                  <span className="text-muted-foreground">{formatUSD(0)}</span>
                )}
              </span>
            </button>
          </motion.li>
        )
      })}
    </ul>
  )
}

function AssetList({ holdings, loading }: { holdings: Holding[]; loading: boolean }) {
  const navigate = useNavigate()
  // One row per asset on a network, summed across the space's wallets.
  const rows = React.useMemo(() => {
    const map = new Map<string, { holding: Holding; amount: number; usd: number | null; wallets: number }>()
    for (const h of holdings) {
      const key = `${h.network.id}:${h.asset.id}`
      const row = map.get(key)
      const amount = Number(h.balance.amount ?? 0)
      if (!row) map.set(key, { holding: h, amount, usd: h.usd, wallets: 1 })
      else {
        row.amount += amount
        row.usd = row.usd === null || h.usd === null ? row.usd ?? h.usd : row.usd + h.usd
        row.wallets += 1
      }
    }
    return [...map.values()].sort((a, b) => (b.usd ?? -1) - (a.usd ?? -1))
  }, [holdings])

  if (rows.length === 0) {
    return loading ? (
      <ListSkeleton />
    ) : (
      <p className="py-10 text-center text-sm text-muted-foreground">No assets with a balance yet.</p>
    )
  }

  return (
    <ul className="flex flex-col">
      {rows.map((row, i) => (
        <motion.li key={`${row.holding.network.id}:${row.holding.asset.id}`} variants={listItem} initial="hidden" animate="show" custom={i}>
          <button
            type="button"
            onClick={() =>
              row.wallets === 1 && navigate(`/wallet/${row.holding.account.id}/${row.holding.network.id}`)
            }
            className="flex w-full items-center gap-3 rounded-2xl px-2 py-2.5 text-left transition-colors hover:bg-muted/60"
          >
            <TokenAvatar symbol={row.holding.asset.symbol} className="size-9" />
            <span className="flex min-w-0 flex-1 flex-col">
              <span className="truncate text-sm font-medium">{row.holding.asset.symbol}</span>
              <span className="truncate text-xs text-muted-foreground">
                {row.holding.network.name}
                {row.wallets > 1 && ` · ${row.wallets} wallets`}
              </span>
            </span>
            <span className="flex flex-col items-end">
              <span className="text-sm font-medium tabular">
                {row.holding.network.testnet ? (
                  <span className="text-xs text-muted-foreground">Testnet</span>
                ) : row.usd === null ? (
                  "—"
                ) : (
                  formatUSD(row.usd)
                )}
              </span>
              <span className="text-xs text-muted-foreground tabular">
                {row.wallets === 1 ? formatAmount(row.holding.balance.amount) : formatAmount(String(row.amount))}
              </span>
            </span>
          </button>
        </motion.li>
      ))}
    </ul>
  )
}
