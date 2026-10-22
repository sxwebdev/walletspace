export type Space = {
  id: string
  name: string
  locked: boolean
  seeded: boolean
  account_count: number
  created_at: string
  updated_at: string
}

export type NetworkFamily = "evm" | "tron"

export type Network = {
  id: string
  name: string
  short_name: string
  family: NetworkFamily
  chain_id: string
  native: { symbol: string; decimals: number }
  testnet: boolean
  enabled: boolean
  explorer: { address: string; tx: string; block: string }
  rpc_fallbacks: string[]
  capabilities: {
    native_transfer: boolean
    token_transfer: boolean
    tron_resources: boolean
    tron_deploy: boolean
  }
  wrapped_native?: { symbol: string; contract: string }
}

export type AccountKind = "derived" | "imported"

export type Account = {
  id: string
  label: string
  kind: AccountKind
  family?: NetworkFamily
  network_ids?: string[]
  addresses: Partial<Record<NetworkFamily, string>>
  index?: number
  derivation_profile?: string
  key_ref?: string
  fingerprint?: string
  imported_at?: string
  created_at: string
  updated_at: string
}

export type Settings = {
  schema_version: number
  server: { addr: string; open_browser: boolean }
  security: { auto_lock: string; confirm_sends: boolean; send_grant_ttl: string }
  node_discovery: {
    enabled: boolean
    url: string
    refresh_interval: string
    request_timeout: string
    allow_insecure_rpc: boolean
  }
  ui: { last_space_id: string }
  revision: string
  restart_required: string[]
}

export type AssetKind = "native" | "erc20" | "trc20"

export type Asset = {
  id: string
  network_id: string
  kind: AssetKind
  name: string
  symbol: string
  decimals: number
  contract?: string
  configured?: boolean
}

export type Balance = {
  account_id: string
  asset_id: string
  amount?: string
  stale?: boolean
  error?: string
}

export type PriceQuote = {
  asset_id: string
  current_usd: string
  previous_24h_usd: string
  has_previous_24h: boolean
  timestamp: string
}

export type TransferEstimate = {
  network_id: string
  amount: string
  fee: string
  gas_limit?: number
  fee_model?: string
  max_fee_per_gas?: string
  max_priority_fee_per_gas?: string
  warning?: string
  native_balance?: string
  fee_shortfall?: string
  additional_fee?: string
  estimated_fee?: string
}

export type TransferBody = {
  action?: "wrap" | "unwrap"
  account_id: string
  asset_id: string
  to: string
  amount: string
}

export type OperationStatus =
  | "building"
  | "broadcasting"
  | "pending"
  | "confirmed"
  | "failed"
  | "broadcast_unknown"

export type Operation = {
  key: string
  request_hash: string
  network_id: string
  tx_hash?: string
  status: OperationStatus
  created_at: string
  updated_at: string
}

export type Transaction = {
  network_id: string
  hash: string
  status: "pending" | "confirmed" | "failed"
}

export type HealthStatus = "checking" | "healthy" | "degraded" | "unavailable"

export type DoctorNetwork = {
  network_id: string
  status: HealthStatus
  healthy: number
  total: number
  last_checked: string
  nodes: { label: string; status: "healthy" | "unhealthy"; last_checked: string }[] | null
}

export type DoctorSnapshot = {
  status: HealthStatus
  healthy: number
  total: number
  failed_nodes: number
  last_checked: string
  networks: DoctorNetwork[] | null
}

export type NetworkOverride = {
  enabled?: boolean
  endpoints?: { url: string; has_headers?: boolean }[]
  explorer?: { address: string; tx: string; block: string }
  discovery_enabled?: boolean
}

export type NetworkOverrides = {
  networks: Record<string, NetworkOverride>
  revision: string
}

export type RefuelPlan = {
  id: string
  network_id: string
  target: { account_id: string; asset_id: string; to: string; amount: string }
  recipient: string
  source_network_id: string
  source_account_id: string
  source_address: string
  source_label: string
  quote: {
    provider: string
    to: string
    data: string
    deposit: string
    expected: string
    deposit_usd: string
    source_price_usd: string
    minimum_limited: boolean
    expires_at: string
  }
  source_estimate: TransferEstimate
  target_estimate: TransferEstimate
  required: string
  expected: string
  remainder: string
  total_source: string
  total_usd?: string
  started: boolean
  finished: boolean
  created_at: string
}

export type RefuelQuotes = {
  estimate: TransferEstimate
  quotes: RefuelPlan[]
  unavailable?: string[]
  sources_checked?: number
  sources_total?: number
}

export type RefuelState = {
  status: "not_started" | "pending" | "failed" | "delivered" | "cancelled"
  plan: RefuelPlan
  operation?: Operation
}

export type TronResource = "bandwidth" | "energy"

export type TronResources = {
  bandwidth: { available: string; total: string }
  energy: { available: string; total: string }
  staked_bandwidth: string
  staked_energy: string
  unstaking: string
  withdrawable_now: string
  bandwidth_per_trx: string
  energy_per_trx: string
  can_delegate_bandwidth: string
  can_delegate_energy: string
  unstake_slots: number
  // The Go structs carry no json tags, so these keys are PascalCase.
  pending: { Resource: TronResource; Amount: string; ExpireAt: string }[] | null
  delegations:
    | { To: string; Resource: TronResource; Amount: string; LockedUntil: string }[]
    | null
}

export type TronOperationResult = {
  network_id: string
  tx_id: string
  status: OperationStatus
  warning?: string
}

export type TronDeployBody = {
  name: string
  abi: string
  bytecode: string
  constructor_params: string
  fee_limit: string
  consume_user_resource_percent: number
  origin_energy_limit: number
}

export type TronDeployEstimate = {
  energy: string
  bandwidth: string
  fee: string
  min_fee_limit: string
  shortfall: string
}

export type TronDeployResult = {
  network_id: string
  tx_id: string
  address?: string
  confirmed?: boolean
  failure?: string
  energy_used?: number
  fee?: string
  status?: OperationStatus
}
