import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { AnimatePresence, motion } from "motion/react"
import { Trash2Icon } from "lucide-react"

import { FormError } from "@/components/common/form-error"
import { TokenAvatar } from "@/components/common/token-avatar"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { toast } from "@/components/ui/toast"
import { queryKeys, useSession } from "@/features/session/session"
import { SectionHeader } from "@/features/settings/settings"
import { NetworkSelect } from "@/features/wallet/wallet-dialogs"
import { networksApi, settingsApi } from "@/lib/api/chain"
import { errorMessage } from "@/lib/api/client"
import { shortAddress } from "@/lib/format"

export function AssetsSection() {
  const { networks } = useSession()
  const queryClient = useQueryClient()
  const [networkId, setNetworkId] = React.useState(networks[0]?.id ?? "")
  const [contract, setContract] = React.useState("")
  const assets = useQuery({
    queryKey: queryKeys.assets(networkId),
    queryFn: () => networksApi.assets(networkId),
    enabled: Boolean(networkId),
  })
  const add = useMutation({
    mutationFn: () => settingsApi.addAsset(networkId, contract.trim()),
    onSuccess: (asset) => {
      setContract("")
      void queryClient.invalidateQueries({ queryKey: queryKeys.assets(networkId) })
      toast.add({ type: "success", title: `${asset.symbol} added` })
    },
  })
  const remove = useMutation({
    mutationFn: (assetId: string) => settingsApi.deleteAsset(assetId),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.assets(networkId) })
      toast.add({ title: "Asset deleted" })
    },
    onError: (error) => toast.add({ type: "error", title: "Not deleted", description: errorMessage(error) }),
  })
  const network = networks.find((n) => n.id === networkId)

  return (
    <div className="flex flex-col gap-6">
      <SectionHeader
        title="Assets"
        description="The contract is checked on the chosen network; symbol and decimals are read on-chain."
      />
      <form
        autoComplete="off"
        onSubmit={(e) => {
          e.preventDefault()
          if (contract.trim()) add.mutate()
        }}
      >
        <FieldGroup>
          <Field>
            <FieldLabel>Network</FieldLabel>
            <NetworkSelect networks={networks} value={networkId} onChange={setNetworkId} />
          </Field>
          <Field>
            <FieldLabel htmlFor="asset-contract">{network?.family === "tron" ? "TRC-20" : "ERC-20"} contract</FieldLabel>
            <div className="flex gap-2">
              <Input
                id="asset-contract"
                spellCheck={false}
                className="font-mono text-xs"
                placeholder={network?.family === "tron" ? "T…" : "0x…"}
                value={contract}
                onChange={(e) => setContract(e.target.value)}
              />
              <Button type="submit" disabled={!contract.trim() || add.isPending}>
                {add.isPending && <Spinner data-icon="inline-start" />}
                Add
              </Button>
            </div>
          </Field>
          <FormError message={add.isError ? errorMessage(add.error) : null} />
        </FieldGroup>
      </form>
      <ul className="flex flex-col divide-y rounded-2xl border">
        {assets.isPending && (
          <li className="p-3">
            <Skeleton className="h-8 w-full" />
          </li>
        )}
        <AnimatePresence initial={false}>
          {assets.data?.assets.map((asset) => (
            <motion.li
              key={asset.id}
              layout
              initial={{ opacity: 0 }}
              animate={{ opacity: 1 }}
              exit={{ opacity: 0, height: 0 }}
              className="flex items-center gap-3 px-3 py-2.5"
            >
              <TokenAvatar symbol={asset.symbol} />
              <span className="flex min-w-0 flex-1 flex-col">
                <span className="truncate text-sm font-medium">
                  {asset.symbol} <span className="font-normal text-muted-foreground">{asset.name}</span>
                </span>
                <span className="truncate font-mono text-xs text-muted-foreground">
                  {asset.contract ? shortAddress(asset.contract, 8, 6) : "Native asset"} · {asset.kind}
                </span>
              </span>
              {asset.configured ? (
                <Button
                  variant="ghost"
                  size="icon-sm"
                  aria-label={`Delete ${asset.symbol}`}
                  onClick={() => remove.mutate(asset.id)}
                  disabled={remove.isPending}
                >
                  <Trash2Icon />
                </Button>
              ) : (
                <Badge variant="outline">Built-in</Badge>
              )}
            </motion.li>
          ))}
        </AnimatePresence>
      </ul>
    </div>
  )
}
