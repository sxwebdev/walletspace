import { useQuery } from "@tanstack/react-query"
import { motion } from "motion/react"
import { CoinsIcon, NetworkIcon, RadarIcon, ServerCogIcon, ShieldIcon } from "lucide-react"
import { useNavigate, useParams } from "react-router"

import { easeOut } from "@/components/common/motion"
import { ScreenHeader } from "@/components/layout/screen-header"
import { Badge } from "@/components/ui/badge"
import { Skeleton } from "@/components/ui/skeleton"
import { AssetsSection } from "@/features/settings/assets-section"
import { DiscoverySection, GeneralSection, SecuritySection } from "@/features/settings/general-sections"
import { NetworksSection } from "@/features/settings/networks-section"
import { queryKeys } from "@/features/session/session"
import { settingsApi } from "@/lib/api/chain"
import { errorMessage } from "@/lib/api/client"
import { cn } from "cn"

const sections = [
  { id: "general", label: "General", icon: ServerCogIcon },
  { id: "security", label: "Security", icon: ShieldIcon },
  { id: "networks", label: "Networks & RPC", icon: NetworkIcon },
  { id: "discovery", label: "Node discovery", icon: RadarIcon },
  { id: "assets", label: "Assets", icon: CoinsIcon },
] as const

type SectionId = (typeof sections)[number]["id"]

export function SettingsScreen() {
  const params = useParams()
  const navigate = useNavigate()
  const requested = params["*"]?.split("/")[0]
  const section: SectionId = sections.some((s) => s.id === requested) ? (requested as SectionId) : "general"
  const settings = useQuery({ queryKey: queryKeys.settings, queryFn: settingsApi.get })

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <ScreenHeader
        title="Settings"
        back="/"
        action={
          settings.data ? (
            <Badge variant="outline" className="hidden font-mono font-normal sm:inline-flex">
              rev {settings.data.revision.slice(0, 8)}
            </Badge>
          ) : null
        }
      />
      <div className="flex min-h-0 flex-1 flex-col sm:flex-row">
        <nav className="flex shrink-0 gap-1 overflow-x-auto px-3 pb-2 sm:w-52 sm:flex-col sm:overflow-visible sm:pb-0">
          {sections.map((item) => (
            <button
              key={item.id}
              type="button"
              onClick={() => navigate(`/settings/${item.id}`, { replace: true })}
              data-active={item.id === section}
              className={cn(
                "relative flex shrink-0 items-center gap-2.5 rounded-2xl px-3 py-2 text-sm font-medium whitespace-nowrap text-muted-foreground transition-colors hover:text-foreground",
                "data-[active=true]:text-foreground"
              )}
            >
              {item.id === section && (
                <motion.span
                  layoutId="settings-nav"
                  className="absolute inset-0 rounded-2xl bg-muted"
                  transition={{ duration: 0.25, ease: easeOut }}
                />
              )}
              <item.icon className="relative size-4" />
              <span className="relative">{item.label}</span>
            </button>
          ))}
        </nav>
        <div className="min-h-0 flex-1 overflow-y-auto overscroll-contain px-5 pb-6 sm:pl-4">
          {settings.isPending ? (
            <div className="flex flex-col gap-3 pt-1">
              <Skeleton className="h-6 w-40" />
              <Skeleton className="h-24 w-full" />
            </div>
          ) : settings.isError ? (
            <p className="pt-4 text-sm text-destructive">{errorMessage(settings.error)}</p>
          ) : (
            <motion.div
              key={section}
              initial={{ opacity: 0, y: 6 }}
              animate={{ opacity: 1, y: 0 }}
              transition={{ duration: 0.25, ease: easeOut }}
            >
              {section === "general" && <GeneralSection settings={settings.data} />}
              {section === "security" && <SecuritySection settings={settings.data} />}
              {section === "discovery" && <DiscoverySection settings={settings.data} />}
              {section === "networks" && <NetworksSection />}
              {section === "assets" && <AssetsSection />}
            </motion.div>
          )}
        </div>
      </div>
    </div>
  )
}

export function SectionHeader({ title, description }: { title: string; description: string }) {
  return (
    <div className="flex flex-col gap-1 pb-5">
      <h2 className="text-base font-semibold">{title}</h2>
      <p className="text-sm text-muted-foreground">{description}</p>
    </div>
  )
}
