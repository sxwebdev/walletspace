import { useParams } from "react-router"

import { useSession } from "@/features/session/session"
import { accountAddress, accountNetworks, useWallet } from "@/features/wallet/wallet-data"

export function useAccountRoute() {
  const { accountId, networkId } = useParams()
  const { networkById } = useSession()
  const { accounts, accountsLoading } = useWallet()
  const account = accounts.find((item) => item.id === accountId)
  const ids = account ? accountNetworks(account) : []
  const network = networkById.get(networkId ?? ids[0] ?? "")
  const bound = Boolean(account && network && ids.includes(network.id))
  return {
    account,
    network: bound ? network : undefined,
    address: account && network ? accountAddress(account, network) : "",
    loading: accountsLoading,
  }
}
