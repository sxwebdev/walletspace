import { request, revisionHeaders, streamNDJSON } from "@/lib/api/client"
import type {
  Asset,
  Balance,
  DoctorSnapshot,
  Network,
  NetworkOverrides,
  Operation,
  PriceQuote,
  RefuelQuotes,
  RefuelState,
  Settings,
  Transaction,
  TransferBody,
  TransferEstimate,
  TronDeployBody,
  TronDeployEstimate,
  TronDeployResult,
  TronOperationResult,
  TronResource,
  TronResources,
} from "@/lib/api/types"

const enc = encodeURIComponent
const scoped = (spaceId: string, networkId: string) =>
  `/api/spaces/${enc(spaceId)}/networks/${enc(networkId)}`

export const networksApi = {
  list: () => request<{ networks: Network[] }>("/api/networks"),
  doctor: () => request<DoctorSnapshot>("/api/doctor"),
  prices: (assetIds: string[]) => {
    const query = new URLSearchParams()
    for (const id of assetIds) query.append("asset_id", id)
    return request<{ quotes: PriceQuote[]; stale: boolean }>(
      `/api/prices?${query}`
    )
  },
  assets: (networkId: string) =>
    request<{ assets: Asset[] }>(
      `/api/settings/assets?network_id=${enc(networkId)}`
    ),
}

export function streamBalances(
  spaceId: string,
  networkId: string,
  options: {
    refresh?: boolean
    accountIds?: string[]
    signal?: AbortSignal
    onValue: (balance: Balance) => void
  }
) {
  const query = new URLSearchParams()
  if (options.refresh) query.set("refresh", "1")
  for (const id of options.accountIds ?? []) query.append("account_id", id)
  const suffix = query.size ? `?${query}` : ""
  return streamNDJSON<Balance>(
    `${scoped(spaceId, networkId)}/balances/stream${suffix}`,
    { signal: options.signal, onValue: options.onValue }
  )
}

type Approved = Pick<
  TransferEstimate,
  "fee_model" | "gas_limit" | "max_fee_per_gas" | "max_priority_fee_per_gas"
>

export const transfersApi = {
  estimate: (spaceId: string, networkId: string, body: TransferBody) =>
    request<TransferEstimate>(`${scoped(spaceId, networkId)}/transfers/estimate`, {
      method: "POST",
      body,
    }),
  send: (
    spaceId: string,
    networkId: string,
    body: TransferBody & Approved,
    idempotencyKey: string
  ) =>
    request<Operation>(`${scoped(spaceId, networkId)}/transfers`, {
      method: "POST",
      body,
      headers: { "Idempotency-Key": idempotencyKey },
    }),
  transaction: (spaceId: string, networkId: string, txId: string) =>
    request<Transaction>(
      `${scoped(spaceId, networkId)}/transactions/${enc(txId)}`
    ),
  refuelQuotes: (
    spaceId: string,
    networkId: string,
    body: TransferBody,
    source?: { accountId: string; networkId: string }
  ) => {
    const query = new URLSearchParams()
    if (source) {
      query.set("source_account_id", source.accountId)
      query.set("source_network_id", source.networkId)
    }
    const suffix = query.size ? `?${query}` : ""
    return request<RefuelQuotes>(
      `${scoped(spaceId, networkId)}/gas-refuel/quotes${suffix}`,
      { method: "POST", body }
    )
  },
  refuelSend: (spaceId: string, networkId: string, planId: string) =>
    request<Operation>(`${scoped(spaceId, networkId)}/gas-refuel/${enc(planId)}`, {
      method: "POST",
      body: {},
    }),
  refuelStatus: (spaceId: string, networkId: string, planId: string) =>
    request<RefuelState>(
      `${scoped(spaceId, networkId)}/gas-refuel/${enc(planId)}`
    ),
}

export type TronAction =
  | "stake"
  | "unstake"
  | "delegate"
  | "reclaim"
  | "withdraw"
  | "cancel-unstakes"

export type TronActionBody =
  | Record<string, never>
  | { resource: TronResource; amount: string }
  | { resource: TronResource; amount: string; to: string; all?: boolean }

export const tronApi = {
  resources: (spaceId: string, networkId: string, accountId: string) =>
    request<TronResources>(
      `${scoped(spaceId, networkId)}/accounts/${enc(accountId)}/resources`
    ),
  operate: (
    spaceId: string,
    networkId: string,
    accountId: string,
    action: TronAction,
    body: TronActionBody,
    idempotencyKey: string
  ) =>
    request<TronOperationResult>(
      `${scoped(spaceId, networkId)}/accounts/${enc(accountId)}/${action}`,
      { method: "POST", body, headers: { "Idempotency-Key": idempotencyKey } }
    ),
  deployEstimate: (
    spaceId: string,
    networkId: string,
    accountId: string,
    body: TronDeployBody
  ) =>
    request<TronDeployEstimate>(
      `${scoped(spaceId, networkId)}/accounts/${enc(accountId)}/deploy-estimate`,
      { method: "POST", body }
    ),
  deploy: (
    spaceId: string,
    networkId: string,
    accountId: string,
    body: TronDeployBody,
    idempotencyKey: string
  ) =>
    request<TronDeployResult>(
      `${scoped(spaceId, networkId)}/accounts/${enc(accountId)}/deploy`,
      { method: "POST", body, headers: { "Idempotency-Key": idempotencyKey } }
    ),
}

export type EndpointInput = { url: string; headers?: Record<string, string> }

export const settingsApi = {
  get: () => request<Settings>("/api/settings"),
  patchGeneral: (
    revision: string,
    body: { server: Settings["server"]; ui: Settings["ui"] }
  ) =>
    request<Settings>("/api/settings/general", {
      method: "PATCH",
      body,
      headers: revisionHeaders(revision),
    }),
  patchSecurity: (
    revision: string,
    body: { auto_lock: string; confirm_sends: boolean; send_grant_ttl: string }
  ) =>
    request<Settings>("/api/settings/security", {
      method: "PATCH",
      body,
      headers: revisionHeaders(revision),
    }),
  patchDiscovery: (revision: string, body: Settings["node_discovery"]) =>
    request<Settings>("/api/settings/node-discovery", {
      method: "PATCH",
      body,
      headers: revisionHeaders(revision),
    }),
  networks: () => request<NetworkOverrides>("/api/settings/networks"),
  putNetwork: (
    revision: string,
    networkId: string,
    body: {
      enabled: boolean | null
      endpoints: EndpointInput[]
      explorer: { address: string; tx: string; block: string } | null
      discovery_enabled: boolean | null
    }
  ) =>
    request<NetworkOverrides>(`/api/settings/networks/${enc(networkId)}`, {
      method: "PUT",
      body,
      headers: revisionHeaders(revision),
    }),
  resetNetwork: (revision: string, networkId: string) =>
    request<NetworkOverrides>(
      `/api/settings/networks/${enc(networkId)}/override`,
      { method: "DELETE", body: {}, headers: revisionHeaders(revision) }
    ),
  testRPC: (networkId: string, endpoints: EndpointInput[]) =>
    request<{ valid: boolean; network_id: string }>(
      `/api/settings/networks/${enc(networkId)}/rpc/test`,
      { method: "POST", body: { endpoints } }
    ),
  addAsset: (network_id: string, contract: string) =>
    request<Asset>("/api/settings/assets", {
      method: "POST",
      body: { network_id, contract },
    }),
  deleteAsset: (assetId: string) =>
    request<unknown>(`/api/settings/assets/${enc(assetId)}`, {
      method: "DELETE",
      body: {},
    }),
}
