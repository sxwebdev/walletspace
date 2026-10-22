import { useQuery } from "@tanstack/react-query"
import { cn } from "cn"
import { useNavigate } from "react-router"

import { Button } from "@/components/ui/button"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { queryKeys } from "@/features/session/session"
import { networksApi } from "@/lib/api/chain"
import type { HealthStatus } from "@/lib/api/types"

export function useDoctor() {
  return useQuery({
    queryKey: queryKeys.doctor,
    queryFn: networksApi.doctor,
    refetchInterval: 15_000,
  })
}

export const healthLabel: Record<HealthStatus, string> = {
  checking: "Checking",
  healthy: "Healthy",
  degraded: "Degraded",
  unavailable: "Unavailable",
}

export function HealthDot({ status, className }: { status: HealthStatus | undefined; className?: string }) {
  return (
    <span className={cn("relative flex size-2", className)}>
      {status === "checking" && (
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-muted-foreground/60" />
      )}
      <span
        className={cn(
          "relative inline-flex size-2 rounded-full",
          status === "healthy" && "bg-success",
          status === "degraded" && "bg-warning",
          status === "unavailable" && "bg-destructive",
          (status === "checking" || !status) && "bg-muted-foreground"
        )}
      />
    </span>
  )
}

export function DoctorButton() {
  const doctor = useDoctor()
  const navigate = useNavigate()
  const status = doctor.isError ? "unavailable" : doctor.data?.status
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          <Button variant="ghost" size="icon-sm" onClick={() => navigate("/nodes")} aria-label="Node health" />
        }
      >
        <HealthDot status={status} />
      </TooltipTrigger>
      <TooltipContent>
        Nodes: {status ? healthLabel[status] : "Checking"}
        {doctor.data && doctor.data.status !== "checking" && ` · ${doctor.data.healthy}/${doctor.data.total}`}
      </TooltipContent>
    </Tooltip>
  )
}
