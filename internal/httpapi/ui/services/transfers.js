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
