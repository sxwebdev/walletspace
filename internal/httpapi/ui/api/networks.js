import { request, streamNDJSON } from "./client.js";

export const listNetworks = (signal) =>
  request("/api/networks", { signal }).then((r) => r.data.networks);

export const networkHealth = (networkID, signal) =>
  request(`/api/networks/${networkID}/health`, { signal }).then((r) => r.data);

export const streamBalances = (
  spaceID,
  networkID,
  { signal, refresh = false, accountIDs = [], onValue },
) =>
  streamNDJSON(
    `/api/spaces/${spaceID}/networks/${networkID}/balances/stream?${new URLSearchParams([
      ...(refresh ? [["refresh", "1"]] : []),
      ...accountIDs.map((id) => ["account_id", id]),
    ])}`,
    { signal, onValue },
  );
