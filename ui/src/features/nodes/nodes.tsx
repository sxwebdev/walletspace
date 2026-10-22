import { motion } from "motion/react"

import { listItem } from "@/components/common/motion"
import { ScreenHeader } from "@/components/layout/screen-header"
import { Screen } from "@/components/layout/widget"
import { Badge } from "@/components/ui/badge"
import { Progress } from "@/components/ui/progress"
import { Skeleton } from "@/components/ui/skeleton"
import { healthLabel, HealthDot, useDoctor } from "@/features/home/doctor-status"
import { useSession } from "@/features/session/session"
import { errorMessage } from "@/lib/api/client"
import { formatRelative } from "@/lib/format"

export function NodesScreen() {
  const doctor = useDoctor()
  const { networkById } = useSession()
  const snapshot = doctor.data
  const networks = snapshot?.networks ?? []

  return (
    <Screen header={<ScreenHeader title="Node health" />}>
      <div className="flex flex-col gap-5 pt-2">
        <div className="flex flex-col items-center gap-2 text-center">
          <div className="flex items-center gap-2 text-lg font-semibold">
            <HealthDot status={doctor.isError ? "unavailable" : snapshot?.status} className="size-2.5" />
            {doctor.isError ? "Unavailable" : snapshot ? healthLabel[snapshot.status] : "Checking"}
          </div>
          <p className="text-sm text-muted-foreground">
            {doctor.isError
              ? errorMessage(doctor.error)
              : !snapshot || snapshot.status === "checking"
                ? "First pass over every RPC node…"
                : `${snapshot.healthy}/${snapshot.total} networks healthy · ${snapshot.failed_nodes} nodes unreachable · checked ${formatRelative(snapshot.last_checked)}`}
          </p>
        </div>
        <ul className="flex flex-col divide-y rounded-2xl border">
          {doctor.isPending &&
            [0, 1, 2, 3].map((i) => (
              <li key={i} className="flex items-center gap-3 p-3">
                <Skeleton className="h-4 flex-1" />
              </li>
            ))}
          {networks.map((item, i) => {
            const nodes = item.nodes ?? []
            const down = nodes.filter((n) => n.status !== "healthy")
            return (
              <motion.li
                key={item.network_id}
                variants={listItem}
                initial="hidden"
                animate="show"
                custom={i}
                className="flex flex-col gap-2 p-3"
              >
                <div className="flex items-center gap-2">
                  <HealthDot status={item.status} />
                  <span className="flex-1 truncate text-sm font-medium">
                    {networkById.get(item.network_id)?.name ?? item.network_id}
                  </span>
                  <Badge variant={item.status === "healthy" ? "secondary" : item.status === "checking" ? "outline" : "destructive"}>
                    {item.total > 0 ? `${item.healthy}/${item.total}` : healthLabel[item.status]}
                  </Badge>
                </div>
                {item.total > 0 && (
                  <Progress
                    value={(item.healthy / item.total) * 100}
                    className="**:data-[slot=progress-track]:h-1"
                  />
                )}
                {down.length > 0 && (
                  <span className="text-xs text-muted-foreground">Unreachable: {down.map((n) => n.label).join(", ")}</span>
                )}
              </motion.li>
            )
          })}
        </ul>
        <p className="text-center text-xs text-muted-foreground">
          Chain identity and reachability are checked in the background every minute. Node URLs are never shown here.
        </p>
      </div>
    </Screen>
  )
}
