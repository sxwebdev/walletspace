import { request } from "./client.js";

export const listAccounts = (spaceID, signal) =>
  request(`/api/spaces/${spaceID}/accounts`, { signal }).then((r) => r.data.accounts);
export const deriveAccount = (spaceID, label) =>
  request(`/api/spaces/${spaceID}/accounts/derive`, { method: "POST", body: { label } }).then((r) => r.data);
export const importAccount = (spaceID, privateKey, label) =>
  request(`/api/spaces/${spaceID}/accounts/import`, {
    method: "POST",
    body: { curve: "secp256k1", private_key: privateKey, label },
  }).then((r) => r.data.account);
export const renameAccount = (spaceID, accountID, label) =>
  request(`/api/spaces/${spaceID}/accounts/${accountID}`, {
    method: "PATCH",
    body: { label },
  }).then((r) => r.data);
export const exportPrivateKey = (spaceID, accountID, family) =>
  request(`/api/spaces/${spaceID}/accounts/${accountID}/private-key`, {
    method: "POST",
    body: { family },
  }).then((r) => r.data.private_key);
