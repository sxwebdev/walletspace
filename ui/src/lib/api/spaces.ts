import { download, request } from "@/lib/api/client"
import type { Account, KeyCurve, NetworkFamily, Space } from "@/lib/api/types"

const base = (spaceId: string) => `/api/spaces/${encodeURIComponent(spaceId)}`

export const spacesApi = {
  list: () => request<{ spaces: Space[] }>("/api/spaces"),
  get: (spaceId: string) =>
    request<{ space: Space; accounts: Account[] }>(base(spaceId)),
  create: (body: {
    name: string
    password: string
    mnemonic?: string
    bip39_passphrase?: string
    imported_only?: boolean
    first?: boolean
  }) =>
    request<{
      space: Space
      accounts: Account[]
      mnemonic_generated: boolean
      mnemonic?: string
    }>("/api/spaces", { method: "POST", body }),
  rename: (spaceId: string, name: string) =>
    request<Space>(base(spaceId), { method: "PATCH", body: { name } }),
  unlock: (spaceId: string, password: string) =>
    request<{ unlocked: boolean }>(`${base(spaceId)}/unlock`, {
      method: "POST",
      body: { password },
    }),
  lock: (spaceId: string) =>
    request<{ locked: boolean }>(`${base(spaceId)}/lock`, {
      method: "POST",
      body: {},
    }),
  changePassword: (
    spaceId: string,
    current_password: string,
    new_password: string
  ) =>
    request<{ changed: boolean }>(`${base(spaceId)}/change-password`, {
      method: "POST",
      body: { current_password, new_password },
    }),
  revealMnemonic: (spaceId: string, password: string) =>
    request<{ mnemonic: string }>(`${base(spaceId)}/mnemonic`, {
      method: "POST",
      body: { password },
    }),
  backup: (spaceId: string, password: string) =>
    download(`${base(spaceId)}/backup`, { method: "POST", body: { password } }),
  confirmSend: (spaceId: string, password: string, signal?: AbortSignal) =>
    request<{ expires_at: string }>(`${base(spaceId)}/confirm-send`, {
      method: "POST",
      body: { password },
      signal,
    }),
}

export const accountsApi = {
  list: (spaceId: string) =>
    request<{ accounts: Account[] }>(`${base(spaceId)}/accounts`),
  derive: (spaceId: string, body: { label?: string; network_ids: string[] }) =>
    request<Account>(`${base(spaceId)}/accounts/derive`, {
      method: "POST",
      body,
    }),
  import: (
    spaceId: string,
    body: {
      curve: KeyCurve
      private_key: string
      label?: string
      network_ids: string[]
    }
  ) =>
    request<{ account: Account }>(`${base(spaceId)}/accounts/import`, {
      method: "POST",
      body,
    }),
  rename: (spaceId: string, accountId: string, label: string) =>
    request<Account>(
      `${base(spaceId)}/accounts/${encodeURIComponent(accountId)}`,
      { method: "PATCH", body: { label } }
    ),
  delete: (spaceId: string, accountId: string, password: string) =>
    request<{ deleted: boolean }>(
      `${base(spaceId)}/accounts/${encodeURIComponent(accountId)}`,
      { method: "DELETE", body: { password } }
    ),
  bindNetwork: (spaceId: string, accountId: string, network_id: string) =>
    request<Account>(
      `${base(spaceId)}/accounts/${encodeURIComponent(accountId)}/networks`,
      { method: "POST", body: { network_id } }
    ),
  unbindNetwork: (spaceId: string, accountId: string, networkId: string) =>
    request<Account>(
      `${base(spaceId)}/accounts/${encodeURIComponent(accountId)}/networks/${encodeURIComponent(networkId)}`,
      { method: "DELETE", body: {} }
    ),
  exportPrivateKey: (
    spaceId: string,
    accountId: string,
    family: NetworkFamily,
    password: string
  ) =>
    // A Solana key comes back as a base58 64-byte keypair, the others as hex.
    request<{ private_key: string; family: NetworkFamily }>(
      `${base(spaceId)}/accounts/${encodeURIComponent(accountId)}/private-key`,
      { method: "POST", body: { family, password } }
    ),
}
