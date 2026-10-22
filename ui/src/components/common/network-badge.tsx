import { Badge } from "@/components/ui/badge"
import type { Network } from "@/lib/api/types"

export function NetworkBadge({ network }: { network: Network }) {
  return (
    <Badge variant="outline" className="gap-1 font-normal">
      {network.name}
      {network.testnet && <span className="text-warning">· test</span>}
    </Badge>
  )
}

export function networkLabel(network: Network) {
  return network.testnet ? `${network.name} · testnet` : network.name
}
