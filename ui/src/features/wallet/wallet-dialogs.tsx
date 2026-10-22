import * as React from "react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { TriangleAlertIcon } from "lucide-react"
import { useNavigate } from "react-router"

import { FormError } from "@/components/common/form-error"
import { networkLabel } from "@/components/common/network-badge"
import { PasswordInput } from "@/components/common/password-input"
import { SecretReveal } from "@/components/common/secret-reveal"
import { Alert, AlertDescription } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Spinner } from "@/components/ui/spinner"
import { toast } from "@/components/ui/toast"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { queryKeys, useSession } from "@/features/session/session"
import { accountNetworks, accountTitle, useWallet } from "@/features/wallet/wallet-data"
import { errorMessage } from "@/lib/api/client"
import { accountsApi } from "@/lib/api/spaces"
import type { Account, Network, NetworkFamily } from "@/lib/api/types"

type DialogProps = { account: Account; open: boolean; onOpenChange: (open: boolean) => void }

// Each dialog keeps its form state in a body that only exists while the dialog
// is open, so reopening always starts clean and a typed secret never lingers.
export function RenameWalletDialog({ account, open, onOpenChange }: DialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        {open && <RenameWalletBody account={account} onDone={() => onOpenChange(false)} />}
      </DialogContent>
    </Dialog>
  )
}

function RenameWalletBody({ account, onDone }: { account: Account; onDone: () => void }) {
  const { space } = useSession()
  const { upsertAccount } = useWallet()
  const [label, setLabel] = React.useState(account.label)
  const rename = useMutation({
    mutationFn: () => accountsApi.rename(space!.id, account.id, label.trim()),
    onSuccess: (updated) => {
      upsertAccount(updated)
      onDone()
    },
  })
  return (
        <form
          className="flex flex-col gap-5"
          onSubmit={(e) => {
            e.preventDefault()
            rename.mutate()
          }}
        >
          <DialogHeader>
            <DialogTitle>Rename wallet</DialogTitle>
          </DialogHeader>
          <FieldGroup>
            <Field>
              <FieldLabel htmlFor="wallet-label">Name</FieldLabel>
              <Input
                id="wallet-label"
                value={label}
                placeholder={accountTitle({ ...account, label: "" })}
                onChange={(e) => setLabel(e.target.value)}
                autoFocus
              />
            </Field>
            <FormError message={rename.isError ? errorMessage(rename.error) : null} />
          </FieldGroup>
          <DialogFooter>
            <Button type="submit" disabled={rename.isPending}>
              {rename.isPending && <Spinner data-icon="inline-start" />}
              Save
            </Button>
          </DialogFooter>
        </form>
  )
}

export function connectableNetworks(account: Account, enabled: Network[]) {
  const bound = accountNetworks(account)
  return enabled.filter(
    (network) =>
      !bound.includes(network.id) &&
      (account.kind === "imported" || !account.family || account.family === network.family)
  )
}

export function ConnectNetworkDialog({
  account,
  open,
  onOpenChange,
  onConnected,
}: DialogProps & { onConnected?: (network: Network) => void }) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        {open && (
          <ConnectNetworkBody
            account={account}
            onDone={(network) => {
              onOpenChange(false)
              onConnected?.(network)
            }}
          />
        )}
      </DialogContent>
    </Dialog>
  )
}

function ConnectNetworkBody({ account, onDone }: { account: Account; onDone: (network: Network) => void }) {
  const { space, enabledNetworks, networkById } = useSession()
  const { upsertAccount, refreshBalances } = useWallet()
  const options = connectableNetworks(account, enabledNetworks)
  const [networkId, setNetworkId] = React.useState(options[0]?.id ?? "")
  const legacy = accountNetworks(account).length === 0
  const connect = useMutation({
    mutationFn: () => accountsApi.bindNetwork(space!.id, account.id, networkId),
    onSuccess: (updated) => {
      upsertAccount(updated)
      refreshBalances({ accountIds: [updated.id], networkIds: [networkId] })
      const network = networkById.get(networkId)
      toast.add({ type: "success", title: `Connected to ${network?.name ?? networkId}` })
      if (network) onDone(network)
    },
  })
  return (
        <form
          className="flex flex-col gap-5"
          onSubmit={(e) => {
            e.preventDefault()
            if (networkId) connect.mutate()
          }}
        >
          <DialogHeader>
            <DialogTitle>Connect a network</DialogTitle>
            <DialogDescription>
              {legacy
                ? "This is an old record with no network. Choose only the network it was actually created in."
                : "The same key becomes available for balances and transfers on the chosen network."}
            </DialogDescription>
          </DialogHeader>
          {legacy && (
            <Alert variant="destructive">
              <TriangleAlertIcon />
              <AlertDescription>The choice fixes the address family of the record. Check it first.</AlertDescription>
            </Alert>
          )}
          <FieldGroup>
            <Field>
              <FieldLabel>Network</FieldLabel>
              <NetworkSelect networks={options} value={networkId} onChange={setNetworkId} />
            </Field>
            <FormError message={connect.isError ? errorMessage(connect.error) : null} />
          </FieldGroup>
          <DialogFooter>
            <Button type="submit" disabled={!networkId || connect.isPending}>
              {connect.isPending && <Spinner data-icon="inline-start" />}
              Connect
            </Button>
          </DialogFooter>
        </form>
  )
}

export function NetworkSelect({
  networks,
  value,
  onChange,
  id,
}: {
  networks: Network[]
  value: string
  onChange: (value: string) => void
  id?: string
}) {
  const mainnets = networks.filter((n) => !n.testnet)
  const testnets = networks.filter((n) => n.testnet)
  const byId = new Map(networks.map((n) => [n.id, n]))
  return (
    <Select value={value} onValueChange={(v) => onChange(String(v))}>
      <SelectTrigger id={id} className="w-full">
        <SelectValue placeholder="Choose a network">
          {(v: string) => (byId.get(v) ? networkLabel(byId.get(v)!) : "Choose a network")}
        </SelectValue>
      </SelectTrigger>
      <SelectContent>
        {mainnets.length > 0 && (
          <SelectGroup>
            {mainnets.map((network) => (
              <SelectItem key={network.id} value={network.id}>
                {network.name}
              </SelectItem>
            ))}
          </SelectGroup>
        )}
        {testnets.length > 0 && (
          <SelectGroup>
            {testnets.map((network) => (
              <SelectItem key={network.id} value={network.id}>
                {network.name}
                <span className="text-muted-foreground">· testnet</span>
              </SelectItem>
            ))}
          </SelectGroup>
        )}
      </SelectContent>
    </Select>
  )
}

export function ExportKeyDialog({ account, open, onOpenChange }: DialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">{open && <ExportKeyBody account={account} />}</DialogContent>
    </Dialog>
  )
}

function ExportKeyBody({ account }: { account: Account }) {
  const { space } = useSession()
  const families: NetworkFamily[] =
    account.kind === "derived" && account.family ? [account.family] : ["evm", "tron"]
  const [family, setFamily] = React.useState<NetworkFamily>(account.family ?? "evm")
  const [password, setPassword] = React.useState("")
  const reveal = useMutation({
    mutationFn: () => accountsApi.exportPrivateKey(space!.id, account.id, family, password),
    onSettled: () => setPassword(""),
  })

  return (
      <>
        <DialogHeader>
          <DialogTitle>Export private key</DialogTitle>
          <DialogDescription>
            {account.kind === "imported"
              ? "An imported key is shared between Tron and EVM."
              : `A derived wallet uses the ${(account.family ?? "legacy").toUpperCase()} BIP44 path.`}
          </DialogDescription>
        </DialogHeader>
        {reveal.data ? (
          <SecretReveal value={reveal.data.private_key} label="Private key" />
        ) : (
          <form
            className="flex flex-col gap-5"
            autoComplete="off"
            onSubmit={(e) => {
              e.preventDefault()
              if (password) reveal.mutate()
            }}
          >
            <Alert variant="destructive">
              <TriangleAlertIcon />
              <AlertDescription>A private key is full control over the funds. Do not show it to anyone.</AlertDescription>
            </Alert>
            <FieldGroup>
              {families.length > 1 && (
                <Field>
                  <FieldLabel>Key format</FieldLabel>
                  <ToggleGroup
                    value={[family]}
                    onValueChange={(v) => v[0] && setFamily(v[0] as NetworkFamily)}
                    variant="outline"
                    spacing={0}
                  >
                    <ToggleGroupItem value="evm">EVM</ToggleGroupItem>
                    <ToggleGroupItem value="tron">Tron</ToggleGroupItem>
                  </ToggleGroup>
                </Field>
              )}
              <Field>
                <FieldLabel htmlFor="export-password">Space password</FieldLabel>
                <PasswordInput
                  id="export-password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                  autoFocus
                  required
                />
              </Field>
              <FormError message={reveal.isError ? errorMessage(reveal.error) : null} />
            </FieldGroup>
            <DialogFooter>
              <Button type="submit" variant="destructive" disabled={!password || reveal.isPending}>
                {reveal.isPending && <Spinner data-icon="inline-start" />}
                Reveal the key
              </Button>
            </DialogFooter>
          </form>
        )}
      </>
  )
}

export function DeleteWalletDialog({
  account,
  open,
  onOpenChange,
  onExport,
}: DialogProps & { onExport: () => void }) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        {open && <DeleteWalletBody account={account} onExport={onExport} />}
      </DialogContent>
    </Dialog>
  )
}

function DeleteWalletBody({ account, onExport }: { account: Account; onExport: () => void }) {
  const { space } = useSession()
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const imported = account.kind === "imported"
  const [password, setPassword] = React.useState("")
  // An imported key exists nowhere else in the space; deleting it has to be
  // acknowledged, not just confirmed with the password.
  const [acknowledged, setAcknowledged] = React.useState(!imported)
  const remove = useMutation({
    mutationFn: () => accountsApi.delete(space!.id, account.id, password),
    onSuccess: () => {
      queryClient.setQueryData<{ accounts: Account[] }>(queryKeys.accounts(space!.id), (data) =>
        data ? { accounts: data.accounts.filter((item) => item.id !== account.id) } : data
      )
      toast.add({ type: "success", title: `${accountTitle(account)} deleted` })
      navigate("/", { replace: true })
    },
    onSettled: () => setPassword(""),
  })

  return (
    <form
      className="flex flex-col gap-5"
      autoComplete="off"
      onSubmit={(e) => {
        e.preventDefault()
        if (password && acknowledged) remove.mutate()
      }}
    >
      <DialogHeader>
        <DialogTitle>Delete {accountTitle(account)}?</DialogTitle>
        <DialogDescription>
          {imported
            ? "The private key is removed from this space for good."
            : `It can be restored later: create a new wallet on the same network and derivation index ${account.index ?? "?"} comes back with the same address.`}
        </DialogDescription>
      </DialogHeader>
      {imported && (
        <Alert variant="destructive">
          <TriangleAlertIcon />
          <AlertDescription>
            <span>
              Without a copy of the key, any funds at this address are lost.{" "}
              <button type="button" className="font-medium underline underline-offset-3" onClick={onExport}>
                Export the key first
              </button>
            </span>
          </AlertDescription>
        </Alert>
      )}
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor="delete-password">Space password</FieldLabel>
          <PasswordInput
            id="delete-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoFocus
            required
          />
        </Field>
        {imported && (
          <Field orientation="horizontal">
            <Checkbox
              id="delete-ack"
              checked={acknowledged}
              onCheckedChange={(v) => setAcknowledged(Boolean(v))}
            />
            <FieldLabel htmlFor="delete-ack" className="font-normal">
              I have a copy of the key or no longer need it
            </FieldLabel>
          </Field>
        )}
        <FormError message={remove.isError ? errorMessage(remove.error) : null} />
      </FieldGroup>
      <DialogFooter>
        <Button type="submit" variant="destructive" disabled={!password || !acknowledged || remove.isPending}>
          {remove.isPending && <Spinner data-icon="inline-start" />}
          Delete wallet
        </Button>
      </DialogFooter>
    </form>
  )
}
