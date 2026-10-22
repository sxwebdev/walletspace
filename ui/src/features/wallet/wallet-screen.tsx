import * as React from "react"
import { motion } from "motion/react"
import {
  ArrowDownLeftIcon,
  ArrowLeftRightIcon,
  ArrowUpRightIcon,
  BatteryChargingIcon,
  EllipsisIcon,
  ExternalLinkIcon,
  FileCodeIcon,
  KeyRoundIcon,
  PencilIcon,
  PlusIcon,
  Trash2Icon,
} from "lucide-react"
import { Navigate, useNavigate } from "react-router"

import { Address } from "@/components/common/address"
import { listItem } from "@/components/common/motion"
import { TokenAvatar } from "@/components/common/token-avatar"
import { ScreenHeader } from "@/components/layout/screen-header"
import { Screen } from "@/components/layout/widget"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { useSession } from "@/features/session/session"
import { useAccountRoute } from "@/features/wallet/use-account-route"
import {
  ConnectNetworkDialog,
  connectableNetworks,
  DeleteWalletDialog,
  ExportKeyDialog,
  RenameWalletDialog,
} from "@/features/wallet/wallet-dialogs"
import { accountNetworks, accountTitle, balanceKey, useWallet } from "@/features/wallet/wallet-data"
import type { Account, Asset, Balance, Network } from "@/lib/api/types"
import { isPositive, toNumber } from "@/lib/decimal"
import { formatAmount, formatUSD } from "@/lib/format"

export function useWalletAssets(account: Account | undefined, network: Network | undefined) {
  const { assets, balances, prices } = useWallet()
  if (!account || !network) return []
  const list = assets.get(network.id) ?? []
  return list
    .map((asset) => {
      const balance = balances.get(balanceKey(network.id, account.id, asset.id))
      const quote = network.testnet ? undefined : prices.get(asset.id)
      const usd = balance?.amount && quote ? toNumber(balance.amount) * toNumber(quote.current_usd) : null
      return { asset, balance, usd }
    })
    .filter(
      ({ asset, balance }) =>
        asset.kind === "native" || Boolean(balance?.error) || isPositive(balance?.amount)
    )
}

export function explorerUrl(template: string, key: "address" | "tx", value: string) {
  if (!template || !template.startsWith("https://")) return null
  return template.replace(`{${key}}`, encodeURIComponent(value))
}

export function WalletScreen() {
  const { account, network, address, loading } = useAccountRoute()
  const { enabledNetworks, networkById } = useSession()
  const navigate = useNavigate()
  const [dialog, setDialog] = React.useState<"rename" | "connect" | "export" | "delete" | null>(null)
  const rows = useWalletAssets(account, network)
  const { balancesLoading } = useWallet()

  if (loading) return null
  if (!account) return <Navigate to="/" replace />

  const ids = accountNetworks(account)
  const canConnect = connectableNetworks(account, enabledNetworks).length > 0
  const total = rows.reduce((sum, row) => sum + (row.usd ?? 0), 0)
  const base = network ? `/wallet/${account.id}/${network.id}` : ""
  const explorer = network ? explorerUrl(network.explorer.address, "address", address) : null

  const actions = network?.enabled
    ? [
        { label: "Send", icon: ArrowUpRightIcon, to: `${base}/send` },
        { label: "Receive", icon: ArrowDownLeftIcon, to: `${base}/receive` },
        ...(network.wrapped_native ? [{ label: "Wrap", icon: ArrowLeftRightIcon, to: `${base}/wrap` }] : []),
        ...(network.family === "tron"
          ? [
              { label: "Resources", icon: BatteryChargingIcon, to: `${base}/resources` },
              { label: "Deploy", icon: FileCodeIcon, to: `${base}/deploy` },
            ]
          : []),
      ]
    : []

  return (
    <Screen
      header={
        <ScreenHeader
          title={accountTitle(account)}
          back="/"
          action={
            <DropdownMenu>
              <DropdownMenuTrigger render={<Button variant="ghost" size="icon-sm" aria-label="Wallet menu" />}>
                <EllipsisIcon />
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-52">
                <DropdownMenuGroup>
                  <DropdownMenuItem onClick={() => setDialog("rename")}>
                    <PencilIcon />
                    Rename
                  </DropdownMenuItem>
                  {canConnect && (
                    <DropdownMenuItem onClick={() => setDialog("connect")}>
                      <PlusIcon />
                      Connect a network
                    </DropdownMenuItem>
                  )}
                </DropdownMenuGroup>
                <DropdownMenuSeparator />
                <DropdownMenuGroup>
                  <DropdownMenuItem variant="destructive" onClick={() => setDialog("export")}>
                    <KeyRoundIcon />
                    Export private key
                  </DropdownMenuItem>
                  <DropdownMenuItem variant="destructive" onClick={() => setDialog("delete")}>
                    <Trash2Icon />
                    Delete wallet
                  </DropdownMenuItem>
                </DropdownMenuGroup>
              </DropdownMenuContent>
            </DropdownMenu>
          }
        />
      }
    >
      <div className="flex flex-col items-center gap-3 pt-2 pb-6 text-center">
        <div className="flex items-center gap-1.5">
          <Badge variant={account.kind === "imported" ? "outline" : "secondary"}>
            {account.kind === "imported" ? "Imported" : `Derived #${account.index ?? "?"}`}
          </Badge>
          {ids.length > 0 && network && (
            <Select
              value={network.id}
              onValueChange={(value) => navigate(`/wallet/${account.id}/${value}`, { replace: true })}
            >
              <SelectTrigger size="sm">
                <SelectValue>{(v: string) => networkById.get(v)?.name ?? v}</SelectValue>
              </SelectTrigger>
              <SelectContent>
                <SelectGroup>
                  {ids.map((id) => (
                    <SelectItem key={id} value={id}>
                      {networkById.get(id)?.name ?? id}
                      {networkById.get(id)?.testnet && <span className="text-muted-foreground">· testnet</span>}
                    </SelectItem>
                  ))}
                </SelectGroup>
              </SelectContent>
            </Select>
          )}
        </div>
        {network ? (
          <>
            <div className="text-3xl font-semibold tracking-tight tabular">
              {network.testnet ? (
                <span className="text-xl text-muted-foreground">Testnet</span>
              ) : balancesLoading && rows.every((r) => !r.balance) ? (
                <Skeleton className="h-9 w-32 rounded-xl" />
              ) : (
                formatUSD(total)
              )}
            </div>
            <div className="flex items-center gap-1">
              <Address value={address} />
              {explorer && (
                <Tooltip>
                  <TooltipTrigger
                    render={
                      <Button
                        variant="ghost"
                        size="icon-xs"
                        render={<a href={explorer} target="_blank" rel="noreferrer noopener" />}
                        aria-label="Open in explorer"
                      />
                    }
                  >
                    <ExternalLinkIcon />
                  </TooltipTrigger>
                  <TooltipContent>Open in explorer</TooltipContent>
                </Tooltip>
              )}
            </div>
          </>
        ) : (
          <div className="flex flex-col items-center gap-3 py-4">
            <p className="text-sm text-muted-foreground">This wallet is not connected to a network yet.</p>
            {canConnect && <Button onClick={() => setDialog("connect")}>Connect a network</Button>}
          </div>
        )}
      </div>

      {network && !network.enabled && (
        <p className="pb-4 text-center text-sm text-muted-foreground">
          {network.name} is disabled in the settings.
        </p>
      )}

      {actions.length > 0 && (
        <div className="flex justify-center gap-4 pb-6">
          {actions.map((action) => (
            <button
              key={action.label}
              type="button"
              onClick={() => navigate(action.to)}
              className="group flex w-14 flex-col items-center gap-1.5 text-xs font-medium"
            >
              <span className="flex size-11 items-center justify-center rounded-full bg-secondary transition-transform group-hover:scale-105 group-active:scale-95">
                <action.icon className="size-[18px]" />
              </span>
              {action.label}
            </button>
          ))}
        </div>
      )}

      {network?.enabled && (
        <ul className="-mx-2 flex flex-col">
          {rows.map((row, i) => (
            <motion.li key={row.asset.id} variants={listItem} initial="hidden" animate="show" custom={i}>
              <AssetRow
                asset={row.asset}
                balance={row.balance}
                usd={row.usd}
                testnet={network.testnet}
                onClick={() => navigate(`${base}/send?asset=${encodeURIComponent(row.asset.id)}`)}
              />
            </motion.li>
          ))}
        </ul>
      )}

      <RenameWalletDialog account={account} open={dialog === "rename"} onOpenChange={(o) => setDialog(o ? "rename" : null)} />
      <ConnectNetworkDialog
        account={account}
        open={dialog === "connect"}
        onOpenChange={(o) => setDialog(o ? "connect" : null)}
        onConnected={(n) => navigate(`/wallet/${account.id}/${n.id}`, { replace: true })}
      />
      <ExportKeyDialog account={account} open={dialog === "export"} onOpenChange={(o) => setDialog(o ? "export" : null)} />
      <DeleteWalletDialog
        account={account}
        open={dialog === "delete"}
        onOpenChange={(o) => setDialog(o ? "delete" : null)}
        onExport={() => setDialog("export")}
      />
    </Screen>
  )
}

function AssetRow({
  asset,
  balance,
  usd,
  testnet,
  onClick,
}: {
  asset: Asset
  balance: Balance | undefined
  usd: number | null
  testnet: boolean
  onClick: () => void
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="flex w-full items-center gap-3 rounded-2xl px-2 py-2.5 text-left transition-colors hover:bg-muted/60"
    >
      <TokenAvatar symbol={asset.symbol} className="size-9" />
      <span className="flex min-w-0 flex-1 flex-col">
        <span className="truncate text-sm font-medium">{asset.symbol}</span>
        <span className="truncate text-xs text-muted-foreground">{asset.name}</span>
      </span>
      <span className="flex flex-col items-end">
        {!balance ? (
          <Skeleton className="h-4 w-16" />
        ) : balance.error ? (
          <Tooltip>
            <TooltipTrigger render={<span className="text-sm text-destructive" />}>Error</TooltipTrigger>
            <TooltipContent className="max-w-64">{balance.error}</TooltipContent>
          </Tooltip>
        ) : (
          <>
            <span className="text-sm font-medium tabular">
              {formatAmount(balance.amount ?? "0")}
              {balance.stale && <span className="text-muted-foreground"> · cached</span>}
            </span>
            <span className="text-xs text-muted-foreground tabular">
              {testnet ? "Testnet" : usd !== null ? formatUSD(usd) : isPositive(balance.amount) ? "No price" : formatUSD(0)}
            </span>
          </>
        )}
      </span>
    </button>
  )
}
