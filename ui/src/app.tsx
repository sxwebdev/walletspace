import * as React from "react"
import { AnimatePresence, motion } from "motion/react"
import { LinkIcon, TriangleAlertIcon } from "lucide-react"
import { Navigate, Route, Routes, useLocation } from "react-router"

import { Logo } from "@/components/common/logo"
import { easeOut } from "@/components/common/motion"
import { Widget } from "@/components/layout/widget"
import { Button } from "@/components/ui/button"
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty"
import { Spinner } from "@/components/ui/spinner"
import { Home } from "@/features/home/home"
import { NodesScreen } from "@/features/nodes/nodes"
import { SendConfirmationProvider } from "@/features/security/send-confirmation"
import { SessionProvider, useSession, useSessionQueries } from "@/features/session/session"
import { SettingsScreen } from "@/features/settings/settings"
import { CreateSpace, NewSpaceChoice, Welcome } from "@/features/spaces/create-space"
import { LockScreen } from "@/features/spaces/lock-screen"
import { SpaceSettings } from "@/features/spaces/space-settings"
import { AddWallet } from "@/features/wallet/add-wallet"
import { PickWallet } from "@/features/wallet/pick-wallet"
import { ReceiveScreen } from "@/features/wallet/receive"
import { SendScreen } from "@/features/wallet/send/send"
import { DeployScreen } from "@/features/wallet/tron/deploy"
import { TronResourcesScreen } from "@/features/wallet/tron/resources"
import { WalletProvider } from "@/features/wallet/wallet-data"
import { WalletScreen } from "@/features/wallet/wallet-screen"
import { errorMessage, hasToken, NO_TOKEN_MESSAGE } from "@/lib/api/client"

export function App() {
  if (!hasToken) {
    return (
      <Widget>
        <Problem icon={LinkIcon} title="Open Walletspace from its link" description={NO_TOKEN_MESSAGE} />
      </Widget>
    )
  }
  return <Session />
}

function Session() {
  const { spaces, networks, settings } = useSessionQueries()
  const location = useLocation()
  const wide = location.pathname.startsWith("/settings")

  if (spaces.isPending || networks.isPending) {
    return (
      <Widget>
        <div className="flex flex-1 flex-col items-center justify-center gap-4">
          <motion.div
            initial={{ scale: 0.9, opacity: 0 }}
            animate={{ scale: 1, opacity: 1 }}
            transition={{ duration: 0.4, ease: easeOut }}
          >
            <Logo className="size-14 rounded-3xl" />
          </motion.div>
          <Spinner className="text-muted-foreground" />
        </div>
      </Widget>
    )
  }

  const failure = spaces.error ?? networks.error
  if (failure || !spaces.data || !networks.data) {
    return (
      <Widget>
        <Problem
          icon={TriangleAlertIcon}
          title="Walletspace did not load"
          description={errorMessage(failure ?? "No response")}
          action={<Button onClick={() => window.location.reload()}>Try again</Button>}
        />
      </Widget>
    )
  }

  return (
    <SessionProvider
      spaces={spaces.data.spaces}
      networks={networks.data.networks}
      settings={settings.data}
    >
      <Widget wide={wide}>
        <Shell />
      </Widget>
    </SessionProvider>
  )
}

// Routes reachable without an unlocked space: nothing in them reads the vault.
const openRoutes = ["/settings", "/spaces/new", "/create"]

function Shell() {
  const { spaces, space } = useSession()
  const location = useLocation()
  const open = openRoutes.some((prefix) => location.pathname.startsWith(prefix))

  let stage: "welcome" | "locked" | "open" | "unlocked"
  if (spaces.length === 0) stage = "welcome"
  else if (space?.locked) stage = open ? "open" : "locked"
  else stage = "unlocked"

  return (
    <AnimatePresence mode="wait" initial={false}>
      <motion.div
        key={stage === "unlocked" ? `unlocked:${space?.id}` : stage}
        className="flex min-h-0 flex-1 flex-col"
        initial={{ opacity: 0, scale: 0.98 }}
        animate={{ opacity: 1, scale: 1 }}
        exit={{ opacity: 0, scale: 0.98 }}
        transition={{ duration: 0.25, ease: easeOut }}
      >
        {stage === "welcome" && (
          <Animated>
            <Route path="/" element={<Welcome />} />
            <Route path="/create" element={<CreateSpace first />} />
            <Route path="/settings/*" element={<SettingsScreen />} />
            <Route path="*" element={<Navigate to="/" replace />} />
          </Animated>
        )}
        {stage === "locked" && <LockScreen key={space?.id} />}
        {stage === "open" && (
          <Animated>
            <Route path="/settings/*" element={<SettingsScreen />} />
            <Route path="/spaces/new" element={<NewSpaceChoice />} />
            <Route path="/create" element={<CreateSpace first={false} />} />
          </Animated>
        )}
        {stage === "unlocked" && (
          <WalletProvider>
            <SendConfirmationProvider>
              <Animated>
                <Route path="/" element={<Home />} />
                <Route path="/send" element={<PickWallet mode="send" />} />
                <Route path="/receive" element={<PickWallet mode="receive" />} />
                <Route path="/add-wallet" element={<AddWallet />} />
                <Route path="/wallet/:accountId" element={<WalletScreen />} />
                <Route path="/wallet/:accountId/:networkId" element={<WalletScreen />} />
                <Route path="/wallet/:accountId/:networkId/send" element={<SendScreen mode="send" />} />
                <Route path="/wallet/:accountId/:networkId/wrap" element={<SendScreen mode="wrap" />} />
                <Route path="/wallet/:accountId/:networkId/receive" element={<ReceiveScreen />} />
                <Route path="/wallet/:accountId/:networkId/resources" element={<TronResourcesScreen />} />
                <Route path="/wallet/:accountId/:networkId/deploy" element={<DeployScreen />} />
                <Route path="/space" element={<SpaceSettings />} />
                <Route path="/spaces/new" element={<NewSpaceChoice />} />
                <Route path="/create" element={<CreateSpace first={false} />} />
                <Route path="/nodes" element={<NodesScreen />} />
                <Route path="/settings/*" element={<SettingsScreen />} />
                <Route path="*" element={<Navigate to="/" replace />} />
              </Animated>
            </SendConfirmationProvider>
          </WalletProvider>
        )}
      </motion.div>
    </AnimatePresence>
  )
}

function depth(pathname: string) {
  return pathname.split("/").filter(Boolean).length
}

// Screens slide like a stack: deeper paths come in from the right, going back
// slides them out the other way.
function Animated({ children }: { children: React.ReactNode }) {
  const location = useLocation()
  const [previous, setPrevious] = React.useState(location.pathname)
  const [d, setDirection] = React.useState(1)
  if (previous !== location.pathname) {
    setDirection(depth(location.pathname) >= depth(previous) ? 1 : -1)
    setPrevious(location.pathname)
  }
  // Settings sections change inside one screen; they are not a new layer.
  const key = location.pathname.startsWith("/settings") ? "/settings" : location.pathname
  // Both screens are absolutely positioned in one frame while they cross, so
  // the outgoing one never pushes the incoming one down.
  return (
    <div className="relative min-h-0 flex-1 overflow-hidden">
      <AnimatePresence initial={false} custom={d}>
        <motion.div
          key={key}
          custom={d}
          variants={{
            enter: (dir: number) => ({ x: dir * 48, opacity: 0 }),
            center: { x: 0, opacity: 1 },
            exit: (dir: number) => ({ x: dir * -48, opacity: 0 }),
          }}
          initial="enter"
          animate="center"
          exit="exit"
          transition={{ duration: 0.26, ease: easeOut }}
          className="absolute inset-0 flex flex-col bg-background"
        >
          <Routes location={location}>{children}</Routes>
        </motion.div>
      </AnimatePresence>
    </div>
  )
}

function Problem({
  icon: Icon,
  title,
  description,
  action,
}: {
  icon: typeof LinkIcon
  title: string
  description: string
  action?: React.ReactNode
}) {
  return (
    <Empty className="flex-1">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <Icon />
        </EmptyMedia>
        <EmptyTitle>{title}</EmptyTitle>
        <EmptyDescription>{description}</EmptyDescription>
      </EmptyHeader>
      {action && <EmptyContent>{action}</EmptyContent>}
    </Empty>
  )
}
