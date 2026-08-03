import { request } from "./client.js";

export const listAccounts = (spaceID, signal) =>
  request(`/api/spaces/${spaceID}/accounts`, { signal }).then((r) => r.data.accounts);
export const deriveAccount = (spaceID, networkID, label) =>
  request(`/api/spaces/${spaceID}/accounts/derive`, {
    method: "POST",
    body: { network_id: networkID, label },
  }).then((r) => r.data);
export const importAccount = (spaceID, networkID, privateKey, label) =>
  request(`/api/spaces/${spaceID}/accounts/import`, {
    method: "POST",
    body: { curve: "secp256k1", network_id: networkID, private_key: privateKey, label },
  }).then((r) => r.data.account);
export const bindAccountNetwork = (spaceID, accountID, networkID) =>
  request(`/api/spaces/${spaceID}/accounts/${accountID}/networks`, {
    method: "POST",
    body: { network_id: networkID },
  }).then((r) => r.data);
export const renameAccount = (spaceID, accountID, label) =>
  request(`/api/spaces/${spaceID}/accounts/${accountID}`, {
    method: "PATCH",
    body: { label },
  }).then((r) => r.data);
export const exportPrivateKey = (spaceID, accountID, family, password) =>
  request(`/api/spaces/${spaceID}/accounts/${accountID}/private-key`, {
    method: "POST",
    body: { family, password },
  }).then((r) => r.data.private_key);
