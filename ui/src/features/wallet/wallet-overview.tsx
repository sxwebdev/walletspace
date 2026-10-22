import * as React from "react"
import { motion } from "motion/react"
import {
  ChevronRightIcon,
  EllipsisIcon,
  KeyRoundIcon,
  PencilIcon,
  PlusIcon,
  Trash2Icon,
} from "lucide-react"
import { Navigate, useNavigate, useParams } from "react-router"

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
import { Skeleton } from "@/components/ui/skeleton"
import { useSession } from "@/features/session/session"
import {
  ConnectNetworkDialog,
  connectableNetworks,
  DeleteWalletDialog,
  ExportKeyDialog,
  RenameWalletDialog,
} from "@/features/wallet/wallet-dialogs"
import {
  accountAddress,
  accountNetworks,
  accountTitle,
  useWallet,
  type Holding,
} from "@/features/wallet/wallet-data"
import type { Network } from "@/lib/api/types"
import { familyName } from "@/lib/family"
import { formatUSD } from "@/lib/format"

// One wallet across every network it is bound to: the same key holds
// different funds on each of them, so each gets its own balance. Send and
// Receive belong to a network and live one level down, on its screen.
export function WalletOverview() {
  const { accountId } = useParams()
  const { enabledNetworks, networkById } = useSession()
  const { accounts, accountsLoading, holdings, balancesLoading } = useWallet()
  const navigate = useNavigate()
  const [dialog, setDialog] = React.useState<
    "rename" | "connect" | "export" | "delete" | null
  >(null)

  const account = accounts.find((item) => item.id === accountId)
  if (accountsLoading) return null
  if (!account) return <Navigate to="/" replace />

  const canConnect = connectableNetworks(account, enabledNetworks).length > 0
  const networks = accountNetworks(account)
    .map((id) => networkById.get(id))
    .filter((n): n is Network => Boolean(n))
  const rows = networks
    .map((network) => {
      const own = holdings.filter(
        (h) => h.account.id === account.id && h.network.id === network.id
      )
      return { network, own, usd: sumUsd(own) }
    })
    .sort(
      (a, b) =>
        Number(!a.network.enabled) - Number(!b.network.enabled) ||
        Number(a.network.testnet) - Number(b.network.testnet) ||
        (b.usd ?? -1) - (a.usd ?? -1) ||
        a.network.name.localeCompare(b.network.name)
    )
  const all = rows.flatMap((r) => r.own)
  const total = sumUsd(all)
  const unpriced = all.filter((h) => !h.network.testnet && h.usd === null)
  const families = [...new Set(networks.map((n) => n.family))]

  return (
    <Screen
      header={
        <ScreenHeader
          title={accountTitle(account)}
          back="/"
          action={
            <DropdownMenu>
              <DropdownMenuTrigger
                render={
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    aria-label="Wallet menu"
                  />
                }
              >
                <EllipsisIcon />
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" className="w-60">
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
                  <DropdownMenuItem
                    variant="destructive"
                    onClick={() => setDialog("export")}
                  >
                    <KeyRoundIcon />
                    Export private key
                  </DropdownMenuItem>
                  <DropdownMenuItem
                    variant="destructive"
                    onClick={() => setDialog("delete")}
                  >
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
        <Badge variant={account.kind === "imported" ? "outline" : "secondary"}>
          {account.kind === "imported"
            ? "Imported"
            : `Derived #${account.index ?? "?"}`}
        </Badge>
        <div className="text-3xl font-semibold tracking-tight tabular">
          {balancesLoading && all.length === 0 ? (
            <Skeleton className="h-9 w-32 rounded-xl" />
          ) : (
            <>
              {unpriced.length > 0 && (
                <span className="text-muted-foreground">≈ </span>
              )}
              {formatUSD(total ?? 0)}
            </>
          )}
        </div>
        <div className="flex flex-col items-center gap-0.5">
          {families.map((family) => (
            <span key={family} className="flex items-center gap-1.5">
              {families.length > 1 && (
                <span className="text-xs text-muted-foreground">
                  {familyName[family]}
                </span>
              )}
              <Address
                value={accountAddress(
                  account,
                  networks.find((n) => n.family === family)!
                )}
              />
            </span>
          ))}
        </div>
      </div>

      {networks.length === 0 ? (
        <div className="flex flex-col items-center gap-3 py-4 text-center">
          <p className="text-sm text-muted-foreground">
            This wallet is not connected to a network yet.
          </p>
          {canConnect && (
            <Button onClick={() => setDialog("connect")}>
              Connect a network
            </Button>
          )}
        </div>
      ) : (
        <ul className="-mx-2 flex flex-col">
          {rows.map(({ network, own, usd }, i) => (
            <motion.li
              key={network.id}
              variants={listItem}
              initial="hidden"
              animate="show"
              custom={i}
            >
              <button
                type="button"
                onClick={() => navigate(`/wallet/${account.id}/${network.id}`)}
                className="flex w-full items-center gap-3 rounded-2xl px-2 py-2.5 text-left transition-colors hover:bg-muted/60"
              >
                <TokenAvatar
                  symbol={network.native.symbol}
                  className="size-9"
                />
                <span className="flex min-w-0 flex-1 flex-col">
                  <span className="truncate text-sm font-medium">
                    {network.name}
                  </span>
                  <span className="truncate text-xs text-muted-foreground">
                    {!network.enabled
                      ? "Disabled in the settings"
                      : own.length > 0
                        ? own.map((h) => h.asset.symbol).join(", ")
                        : "No assets"}
                  </span>
                </span>
                <span className="text-right text-sm font-medium tabular">
                  {!network.enabled ? null : balancesLoading &&
                    own.length === 0 ? (
                    <Skeleton className="h-4 w-14" />
                  ) : network.testnet ? (
                    <span className="text-xs text-muted-foreground">
                      Testnet
                    </span>
                  ) : usd !== null ? (
                    formatUSD(usd)
                  ) : own.length > 0 ? (
                    <span className="text-xs text-muted-foreground">
                      No price
                    </span>
                  ) : (
                    <span className="text-muted-foreground">
                      {formatUSD(0)}
                    </span>
                  )}
                </span>
                <ChevronRightIcon className="size-4 text-muted-foreground" />
              </button>
            </motion.li>
          ))}
          {canConnect && (
            <li>
              <button
                type="button"
                onClick={() => setDialog("connect")}
                className="flex w-full items-center gap-3 rounded-2xl px-2 py-2.5 text-left text-sm font-medium text-muted-foreground transition-colors hover:bg-muted/60 hover:text-foreground"
              >
                <span className="flex size-9 shrink-0 items-center justify-center rounded-full border border-dashed">
                  <PlusIcon className="size-4" />
                </span>
                Connect a network
              </button>
            </li>
          )}
        </ul>
      )}

      <RenameWalletDialog
        account={account}
        open={dialog === "rename"}
        onOpenChange={(o) => setDialog(o ? "rename" : null)}
      />
      <ConnectNetworkDialog
        account={account}
        open={dialog === "connect"}
        onOpenChange={(o) => setDialog(o ? "connect" : null)}
        onConnected={(n) => navigate(`/wallet/${account.id}/${n.id}`)}
      />
      <ExportKeyDialog
        account={account}
        open={dialog === "export"}
        onOpenChange={(o) => setDialog(o ? "export" : null)}
      />
      <DeleteWalletDialog
        account={account}
        open={dialog === "delete"}
        onOpenChange={(o) => setDialog(o ? "delete" : null)}
        onExport={() => setDialog("export")}
      />
    </Screen>
  )
}

// The USD value of mainnet holdings, or null when none of them has a price.
function sumUsd(holdings: Holding[]) {
  const priced = holdings.filter((h) => !h.network.testnet && h.usd !== null)
  return priced.length > 0
    ? priced.reduce((sum, h) => sum + (h.usd ?? 0), 0)
    : null
}
