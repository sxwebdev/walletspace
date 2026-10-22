import { motion } from "motion/react"
import { QRCodeSVG } from "qrcode.react"
import { TriangleAlertIcon } from "lucide-react"
import { Navigate } from "react-router"

import { CopyButton } from "@/components/common/copy-button"
import { easeOut } from "@/components/common/motion"
import { ScreenHeader } from "@/components/layout/screen-header"
import { Screen } from "@/components/layout/widget"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Badge } from "@/components/ui/badge"
import { useAccountRoute } from "@/features/wallet/use-account-route"
import { accountTitle } from "@/features/wallet/wallet-data"
import { tokenStandard } from "@/lib/family"

export function ReceiveScreen() {
  const { account, network, address, loading } = useAccountRoute()
  if (loading) return null
  if (!account || !network) return <Navigate to="/" replace />

  return (
    <Screen header={<ScreenHeader title="Receive" />}>
      <div className="flex flex-col items-center gap-5 pt-4">
        <div className="flex flex-col items-center gap-1 text-center">
          <span className="text-sm font-medium">{accountTitle(account)}</span>
          <Badge variant="outline">
            {network.name}
            {network.testnet && " · testnet"}
          </Badge>
        </div>
        <motion.div
          initial={{ scale: 0.92, opacity: 0 }}
          animate={{ scale: 1, opacity: 1 }}
          transition={{ duration: 0.4, ease: easeOut }}
          className="rounded-3xl border bg-white p-4 shadow-sm"
        >
          {/* The QR is drawn as SVG elements, which the CSP allows. */}
          <QRCodeSVG value={address} size={200} marginSize={0} level="M" />
        </motion.div>
        <div className="flex w-full items-center gap-2 rounded-2xl bg-muted/50 p-3">
          <p className="flex-1 font-mono text-xs break-all">{address}</p>
          <CopyButton value={address} label="Copy address" variant="outline" />
        </div>
        <Alert>
          <TriangleAlertIcon />
          <AlertDescription>
            {network.family === "solana"
              ? `Send only ${network.native.symbol} and classic SPL tokens on ${network.name} to this address; Token-2022 tokens are not supported.`
              : `Send only ${network.native.symbol} and ${tokenStandard[network.family]} tokens on ${network.name} to this address.`}{" "}
            Assets sent from another network may be lost.
          </AlertDescription>
        </Alert>
      </div>
    </Screen>
  )
}
