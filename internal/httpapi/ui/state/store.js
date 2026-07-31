const listeners = new Set();

export const state = {
  spaces: [],
  currentSpaceID: sessionStorage.getItem("walletspace:space") || "",
  networks: [],
  currentNetworkID: sessionStorage.getItem("walletspace:network") || "",
  accountFilter: sessionStorage.getItem("walletspace:account-filter") || "all",
  accounts: [],
  assets: [],
  balances: new Map(),
  balanceGeneration: 0,
};

export function update(patch) {
  Object.assign(state, patch);
  if (patch.currentSpaceID !== undefined) {
    sessionStorage.setItem("walletspace:space", patch.currentSpaceID);
  }
  if (patch.currentNetworkID !== undefined) {
    sessionStorage.setItem("walletspace:network", patch.currentNetworkID);
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

export function currentNetwork() {
  return state.networks.find((item) => item.id === state.currentNetworkID);
}

export const balanceKey = (spaceID, networkID, accountID, assetID) =>
  `${spaceID}:${networkID}:${accountID}:${assetID}`;
