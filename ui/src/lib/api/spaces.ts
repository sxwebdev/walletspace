import { download, request } from "@/lib/api/client"
import type {
  Account,
  BundleImport,
  KeyCurve,
  NetworkFamily,
  Space,
  FirstSync,
  SyncOverview,
  SyncStatus,
} from "@/lib/api/types"

const base = (spaceId: string) => `/api/spaces/${encodeURIComponent(spaceId)}`

// True when the first sync of an unlock or a restore leaves the space
// possibly short of what other computers wrote: it did not finish in time, or
// files are still not downloaded.
export function firstSyncIncomplete(sync: FirstSync | undefined): boolean {
  return !!sync && (!sync.finished || sync.pending > 0)
}

// What the person is told when it does.
export const firstSyncIncompleteMessage = {
  title: "Not everything has arrived from your other computers yet",
  description: "See Settings → Backup & sync.",
}

export const spacesApi = {
  list: () => request<{ spaces: Space[] }>("/api/spaces"),
  get: (spaceId: string) =>
    request<{ space: Space; accounts: Account[] }>(base(spaceId)),
  create: (body: {
    name: string
    password: string
    mnemonic?: string
    bip39_passphrase?: string
    first?: boolean
  }) =>
    request<{
      space: Space
      accounts: Account[]
      mnemonic_generated: boolean
      mnemonic?: string
      sync?: FirstSync
    }>("/api/spaces", { method: "POST", body }),
  rename: (spaceId: string, name: string) =>
    request<Space>(base(spaceId), { method: "PATCH", body: { name } }),
  unlock: (spaceId: string, password: string) =>
    request<{ unlocked: boolean; space: Space; sync?: FirstSync }>(
      `${base(spaceId)}/unlock`,
      {
        method: "POST",
        body: { password },
      }
    ),
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
  // Several spaces in one file for another machine, each behind its password.
  exportBundle: (spaces: { id: string; password: string }[]) =>
    download("/api/spaces/export", { method: "POST", body: { spaces } }),
  importBundle: (file: Blob) =>
    request<BundleImport>("/api/spaces/import", {
      method: "POST",
      rawBody: file,
    }),
  // mnemonic is the phrase this space got on another computer, or "" for a
  // new one.
  addRecoveryPhrase: (spaceId: string, password: string, mnemonic = "") =>
    request<{ mnemonic: string }>(`${base(spaceId)}/recovery-phrase`, {
      method: "POST",
      body: { password, mnemonic },
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

export const syncApi = {
  overview: () => request<SyncOverview>("/api/sync"),
  now: (spaceId: string) =>
    request<SyncStatus>(`${base(spaceId)}/sync`, { method: "POST", body: {} }),
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
