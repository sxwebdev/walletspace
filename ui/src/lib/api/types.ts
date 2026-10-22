export type Space = {
  id: string
  name: string
  locked: boolean
  seeded: boolean
  account_count: number
  created_at: string
  updated_at: string
}

export type NetworkFamily = "evm" | "tron" | "solana"

export type KeyCurve = "secp256k1" | "ed25519"

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
    tron_swap: boolean
    solana_swap: boolean
    solana_close_accounts: boolean
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
  security: {
    auto_lock: string
    confirm_sends: boolean
    send_grant_ttl: string
  }
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

export type AssetKind = "native" | "erc20" | "trc20" | "spl"

export type Asset = {
  id: string
  network_id: string
  kind: AssetKind
  name: string
  symbol: string
  decimals: number
  // The token contract; for an SPL token, its mint. Base58 values keep their
  // case.
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
  // Solana: echoed back on send as the approval. A zero price is omitted.
  // fee is the total, the token account rent included.
  compute_unit_limit?: number
  compute_unit_price_micro_lamports?: number
  creates_token_account?: boolean
  token_account_rent?: string
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
  nodes:
    | { label: string; status: "healthy" | "unhealthy"; last_checked: string }[]
    | null
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
    | {
        To: string
        Resource: TronResource
        Amount: string
        LockedUntil: string
      }[]
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

export type SwapAsset = "trx" | "usdt"

export type SwapRoute = {
  protocol: "v2" | "v3" | "v4"
  // Pool fee in millionths: 3000 is 0.3%.
  fee: number
  tick_spacing?: number
}

export type SwapStepKind = "approve" | "permit2" | "swap"

export type SwapStep = {
  kind: SwapStepKind
  contract: string
  energy: number
  // False when the energy is a typical figure: the swap cannot be simulated
  // before its approvals are on chain.
  measured: boolean
  fee: string
}

export type SwapQuote = {
  route: SwapRoute
  amount_out: string
  steps: SwapStep[]
  fee: string
  // amount_out less fee, both in the asset bought.
  value: string
}

export type SwapQuotes = {
  in: SwapAsset
  out: SwapAsset
  amount_in: string
  quotes: SwapQuote[] | null
  unavailable?: string[] | null
  shortfall: string
}

export type SwapQuoteBody = {
  in: SwapAsset
  amount: string
  unlimited: boolean
}

export type SwapBody = {
  in: SwapAsset
  amount: string
  min_out: string
  route: SwapRoute
  step: SwapStepKind
  unlimited: boolean
}

// An empty Solana token account that can be closed for its rent. The symbol
// is there only when the wallet knows the mint.
export type EmptyTokenAccount = {
  address: string
  mint: string
  symbol?: string
  rent: string
}

export type EmptyTokenAccounts = {
  network_id: string
  max_per_transaction: number
  accounts: EmptyTokenAccount[]
}

export type CloseAccountsEstimate = {
  network_id: string
  accounts: EmptyTokenAccount[]
  // SOL coming back to the wallet: the accounts' rent.
  returned: string
  fee: string
  fee_model: string
  compute_unit_limit: number
  // Omitted when 0.
  compute_unit_price_micro_lamports?: number
}

export type CloseAccountsBody = {
  accounts: string[]
  fee_model?: string
  fee?: string
  compute_unit_limit?: number
  compute_unit_price_micro_lamports?: number
}

export type SolanaOperationResult = {
  network_id: string
  tx_id: string
  status: OperationStatus
  warning?: string
}

export type SolanaSwapHop = {
  pool: string
  kind: string
  input_mint: string
  output_mint: string
}

export type SolanaSwapRoute = {
  hops: SolanaSwapHop[]
  lookup_tables?: string[]
}

export type SolanaSwapQuoteBody = {
  in_asset_id: string
  out_asset_id: string
  amount: string
  // 1..500
  slippage_bps: number
}

export type SolanaSwapQuote = {
  network_id: string
  amount_in: string
  // What the simulation of this swap paid out.
  amount_out: string
  min_out: string
  slippage_bps: number
  source: "raydium_api" | "discovery"
  api_route_rejected?: string
  quoted_out?: string
  price_impact_pct?: string
  // Absent for a two-hop route.
  quote_deviation_pct?: string
  route: SolanaSwapRoute
  intermediate_mint?: string
  intermediate_leftover?: string
  unwraps_wrapped_sol?: string
  message_version: string
  message_size: number
  fee_model: string
  fee: string
  compute_unit_limit: number
  compute_unit_price_micro_lamports?: number
  token_accounts_created: number
  token_account_rent?: string
  native_balance: string
}

// The approved swap: the quote's request, its minimum, route and fee echoed
// back exactly.
export type SolanaSwapBody = SolanaSwapQuoteBody & {
  min_out: string
  route: SolanaSwapRoute
  compute_unit_limit: number
  compute_unit_price_micro_lamports: number
  token_accounts_created: number
  token_account_rent?: string
  fee: string
}
