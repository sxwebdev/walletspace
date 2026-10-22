import { request } from "./client.js";

const base = (spaceID, networkID) => `/api/spaces/${spaceID}/networks/${networkID}`;

export const estimateTransfer = (spaceID, networkID, body, signal) =>
  request(`${base(spaceID, networkID)}/transfers/estimate`, {
    method: "POST",
    body,
    signal,
  }).then((r) => r.data);

export const sendTransfer = (spaceID, networkID, body, idempotencyKey, signal) =>
  request(`${base(spaceID, networkID)}/transfers`, {
    method: "POST",
    body,
    signal,
    headers: { "Idempotency-Key": idempotencyKey },
  }).then((r) => r.data);

export const transactionStatus = (spaceID, networkID, txID, signal) =>
  request(`${base(spaceID, networkID)}/transactions/${txID}`, { signal }).then((r) => r.data);

export const quoteGasRefuel = (spaceID, networkID, body, source, signal) => {
  const query = new URLSearchParams();
  if (source?.accountID) query.set("source_account_id", source.accountID);
  if (source?.networkID) query.set("source_network_id", source.networkID);
  return request(`${base(spaceID, networkID)}/gas-refuel/quotes?${query}`, {
    method: "POST", body, signal,
  }).then((r) => r.data);
};

export const sendGasRefuel = (spaceID, networkID, planID, signal) =>
  request(`${base(spaceID, networkID)}/gas-refuel/${planID}`, {
    method: "POST", body: {}, signal,
  }).then((r) => r.data);

export const gasRefuelStatus = (spaceID, networkID, planID, signal) =>
  request(`${base(spaceID, networkID)}/gas-refuel/${planID}`, { signal }).then((r) => r.data);
