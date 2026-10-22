import { motion } from "motion/react"
import { ChevronRightIcon } from "lucide-react"
import { useNavigate } from "react-router"

import { listItem } from "@/components/common/motion"
import { ScreenHeader } from "@/components/layout/screen-header"
import { Screen } from "@/components/layout/widget"
import { useSession } from "@/features/session/session"
import { accountAddress, accountNetworks, accountTitle, useWallet } from "@/features/wallet/wallet-data"
import { formatUSD, shortAddress } from "@/lib/format"

// Send and Receive from the home screen start by choosing the wallet and the
// network, since one key can live on several networks.
export function PickWallet({ mode }: { mode: "send" | "receive" }) {
  const { networkById } = useSession()
  const { accounts, holdings } = useWallet()
  const navigate = useNavigate()

  const rows = accounts.flatMap((account) =>
    accountNetworks(account)
      .map((id) => networkById.get(id))
      .filter((network) => network?.enabled)
      .map((network) => {
        const own = holdings.filter((h) => h.account.id === account.id && h.network.id === network!.id)
        return {
          account,
          network: network!,
          usd: own.reduce((sum, h) => sum + (h.usd ?? 0), 0),
          assets: own.length,
        }
      })
  )
  if (mode === "send") rows.sort((a, b) => b.usd - a.usd || b.assets - a.assets)

  return (
    <Screen header={<ScreenHeader title={mode === "send" ? "Send from" : "Receive to"} back="/" />}>
      <ul className="-mx-2 flex flex-col pt-1">
        {rows.map((row, i) => (
          <motion.li
            key={`${row.account.id}:${row.network.id}`}
            variants={listItem}
            initial="hidden"
            animate="show"
            custom={i}
          >
            <button
              type="button"
              onClick={() => navigate(`/wallet/${row.account.id}/${row.network.id}/${mode}`)}
              className="flex w-full items-center gap-3 rounded-2xl px-2 py-2.5 text-left transition-colors hover:bg-muted/60"
            >
              <span className="flex size-9 shrink-0 items-center justify-center rounded-full bg-muted text-sm font-semibold">
                {accountTitle(row.account).slice(0, 1).toUpperCase()}
              </span>
              <span className="flex min-w-0 flex-1 flex-col">
                <span className="truncate text-sm font-medium">{accountTitle(row.account)}</span>
                <span className="truncate text-xs text-muted-foreground">
                  {row.network.name}
                  {row.network.testnet && " · testnet"} · {shortAddress(accountAddress(row.account, row.network))}
                </span>
              </span>
              {mode === "send" && !row.network.testnet && (
                <span className="text-sm tabular text-muted-foreground">{formatUSD(row.usd)}</span>
              )}
              <ChevronRightIcon className="size-4 text-muted-foreground" />
            </button>
          </motion.li>
        ))}
        {rows.length === 0 && (
          <p className="py-10 text-center text-sm text-muted-foreground">No wallet is connected to an enabled network.</p>
        )}
      </ul>
    </Screen>
  )
}
