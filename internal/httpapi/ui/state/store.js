const listeners = new Set();

export const state = {
  spaces: [],
  currentSpaceID: sessionStorage.getItem("walletspace:space") || "",
  networks: [],
  accountFilter: sessionStorage.getItem("walletspace:account-filter") || "all",
  accounts: [],
  assets: [],
  balances: new Map(),
  balancesLoading: true,
  balanceFailures: 0,
  balanceGeneration: 0,
  prices: new Map(),
  pricesLoading: true,
  pricesStale: false,
  pricesError: "",
};

export function update(patch) {
  Object.assign(state, patch);
  if (patch.currentSpaceID !== undefined) {
    sessionStorage.setItem("walletspace:space", patch.currentSpaceID);
  }
  if (patch.accountFilter !== undefined) {
    sessionStorage.setItem("walletspace:account-filter", patch.accountFilter);
  }
  for (const listener of listeners) listener(state);
}

export function subscribe(listener) {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function currentSpace() {
  return state.spaces.find((item) => item.id === state.currentSpaceID);
}

export const balanceKey = (spaceID, networkID, accountID, assetID) =>
  `${spaceID}:${networkID}:${accountID}:${assetID}`;
