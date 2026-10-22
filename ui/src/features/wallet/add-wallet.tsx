import * as React from "react"
import { useMutation } from "@tanstack/react-query"
import { ShieldAlertIcon } from "lucide-react"
import { useNavigate } from "react-router"

import { FormError } from "@/components/common/form-error"
import { PasswordInput } from "@/components/common/password-input"
import { ScreenHeader } from "@/components/layout/screen-header"
import { Screen } from "@/components/layout/widget"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { toast } from "@/components/ui/toast"
import { useSession } from "@/features/session/session"
import { NetworkSelect } from "@/features/wallet/wallet-dialogs"
import { accountNetworks, useWallet } from "@/features/wallet/wallet-data"
import { errorMessage } from "@/lib/api/client"
import { accountsApi } from "@/lib/api/spaces"
import { keyCurve } from "@/lib/family"

// Mirrors the server: the smallest index not yet used by a derived wallet
// bound to that network.
function nextIndex(accounts: ReturnType<typeof useWallet>["accounts"], networkId: string) {
  const used = new Set(
    accounts
      .filter((a) => a.kind === "derived" && accountNetworks(a).includes(networkId) && typeof a.index === "number")
      .map((a) => a.index!)
  )
  let i = 0
  while (used.has(i)) i += 1
  return i
}

export function AddWallet() {
  const { space, enabledNetworks, networkById } = useSession()
  const { accounts, upsertAccount, refreshBalances } = useWallet()
  const navigate = useNavigate()
  const [tab, setTab] = React.useState<"derive" | "import">("derive")
  const [networkId, setNetworkId] = React.useState(enabledNetworks[0]?.id ?? "")
  const [label, setLabel] = React.useState("")
  const [privateKey, setPrivateKey] = React.useState("")
  const family = networkById.get(networkId)?.family ?? "evm"

  const done = (accountId: string) => {
    refreshBalances({ accountIds: [accountId], networkIds: [networkId] })
    navigate(`/wallet/${accountId}/${networkId}`, { replace: true })
  }

  const derive = useMutation({
    mutationFn: () => accountsApi.derive(space!.id, { network_id: networkId, label: label.trim() }),
    onSuccess: (account) => {
      upsertAccount(account)
      toast.add({ type: "success", title: "Wallet created" })
      done(account.id)
    },
  })
  const importKey = useMutation({
    mutationFn: () =>
      accountsApi.import(space!.id, {
        curve: keyCurve[family],
        network_id: networkId,
        private_key: privateKey.trim(),
        label: label.trim(),
      }),
    onSuccess: ({ account }) => {
      setPrivateKey("")
      upsertAccount(account)
      toast.add({ type: "success", title: "Key imported", description: "Remember to back up the space." })
      done(account.id)
    },
    onError: () => setPrivateKey(""),
  })
  const mutation = tab === "derive" ? derive : importKey
  const index = nextIndex(accounts, networkId)

  return (
    <Screen
      header={<ScreenHeader title="Add wallet" back="/" />}
      footer={
        <Button type="submit" form="add-wallet" size="lg" className="w-full" disabled={!networkId || mutation.isPending}>
          {mutation.isPending && <Spinner data-icon="inline-start" />}
          {tab === "derive" ? "Create wallet" : "Import"}
        </Button>
      }
    >
      <form
        id="add-wallet"
        autoComplete="off"
        className="flex flex-col gap-5 pt-1"
        onSubmit={(e) => {
          e.preventDefault()
          mutation.mutate()
        }}
      >
        <Tabs value={tab} onValueChange={(v) => setTab(v as "derive" | "import")}>
          <TabsList className="w-full">
            <TabsTrigger value="derive">New wallet</TabsTrigger>
            <TabsTrigger value="import">Import key</TabsTrigger>
          </TabsList>
        </Tabs>
        <FieldGroup>
          <Field>
            <FieldLabel>Network</FieldLabel>
            <NetworkSelect
              networks={enabledNetworks}
              value={networkId}
              onChange={(id) => {
                // A key typed for one family is in the wrong format for another.
                if (networkById.get(id)?.family !== family) setPrivateKey("")
                setNetworkId(id)
              }}
            />
            {tab === "derive" && (
              <FieldDescription>
                Derivation index {index} on this network. Indexes count separately per network.
              </FieldDescription>
            )}
          </Field>
          {tab === "import" &&
            (family === "solana" ? (
              <Field>
                <FieldLabel htmlFor="private-key">Keypair</FieldLabel>
                {/* A password field strips the line breaks of a pasted
                    multi-line JSON array; the array stays valid JSON. */}
                <PasswordInput
                  id="private-key"
                  autoComplete="new-password"
                  placeholder="Base58 keypair or [12, 34, …]"
                  value={privateKey}
                  onChange={(e) => setPrivateKey(e.target.value)}
                  required
                  className="font-mono"
                />
                <FieldDescription>
                  The base58 keypair Phantom or Solflare export, or the JSON array of 64 numbers from a solana-keygen
                  file.
                </FieldDescription>
              </Field>
            ) : (
              <Field>
                <FieldLabel htmlFor="private-key">Private key</FieldLabel>
                <PasswordInput
                  id="private-key"
                  autoComplete="new-password"
                  placeholder="hex, with or without 0x"
                  value={privateKey}
                  onChange={(e) => setPrivateKey(e.target.value)}
                  required
                  className="font-mono"
                />
              </Field>
            ))}
          <Field>
            <FieldLabel htmlFor="wallet-name">Name</FieldLabel>
            <Input
              id="wallet-name"
              placeholder={tab === "derive" ? `Account ${index}` : "Imported wallet"}
              value={label}
              onChange={(e) => setLabel(e.target.value)}
            />
          </Field>
          {tab === "import" && (
            <Alert>
              <ShieldAlertIcon />
              <AlertDescription>
                An imported key cannot be recovered from the space’s phrase. Keep a backup of the space or of the key.
              </AlertDescription>
            </Alert>
          )}
          <FormError message={mutation.isError ? errorMessage(mutation.error) : null} />
        </FieldGroup>
      </form>
    </Screen>
  )
}
