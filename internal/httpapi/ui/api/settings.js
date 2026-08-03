import { request, revisionHeaders } from "./client.js";

export const getSettings = (signal) => request("/api/settings", { signal }).then((r) => r.data);
export const saveGeneral = (body, revision) =>
  request("/api/settings/general", {
    method: "PATCH",
    body,
    headers: revisionHeaders(revision),
  }).then((r) => r.data);
export const saveSecurity = (body, revision) =>
  request("/api/settings/security", {
    method: "PATCH",
    body,
    headers: revisionHeaders(revision),
  }).then((r) => r.data);
export const saveDiscovery = (body, revision) =>
  request("/api/settings/node-discovery", {
    method: "PATCH",
    body,
    headers: revisionHeaders(revision),
  }).then((r) => r.data);
export const getNetworkSettings = (signal) =>
  request("/api/settings/networks", { signal }).then((r) => r.data);
export const saveNetwork = (networkID, body, revision) =>
  request(`/api/settings/networks/${networkID}`, {
    method: "PUT",
    body,
    headers: revisionHeaders(revision),
  }).then((r) => r.data);
export const deleteNetwork = (networkID, revision) =>
  request(`/api/settings/networks/${networkID}/override`, {
    method: "DELETE",
    body: {},
    headers: revisionHeaders(revision),
  }).then((r) => r.data);
export const listAssets = (networkID, signal) =>
  request(`/api/settings/assets?network_id=${encodeURIComponent(networkID)}`, { signal })
    .then((r) => r.data.assets);
export const addAsset = (networkID, contract) =>
  request("/api/settings/assets", {
    method: "POST",
    body: { network_id: networkID, contract },
  }).then((r) => r.data);
export const removeAsset = (assetID) =>
  request(`/api/settings/assets/${encodeURIComponent(assetID)}`, {
    method: "DELETE",
    body: {},
  });
