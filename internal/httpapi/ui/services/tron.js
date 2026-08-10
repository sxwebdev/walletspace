import { request } from "./client.js";

const accountBase = (spaceID, networkID, accountID) =>
  `/api/spaces/${spaceID}/networks/${networkID}/accounts/${accountID}`;

export const resources = (spaceID, networkID, accountID, signal) =>
  request(`${accountBase(spaceID, networkID, accountID)}/resources`, { signal }).then((r) => r.data);

export const stakingOperation = (spaceID, networkID, accountID, action, body, idempotencyKey, signal) =>
  request(`${accountBase(spaceID, networkID, accountID)}/${action}`, {
    method: "POST",
    body,
    headers: { "Idempotency-Key": idempotencyKey },
    signal,
  }).then((r) => r.data);

export const estimateDeploy = (spaceID, networkID, accountID, body, signal) =>
  request(`${accountBase(spaceID, networkID, accountID)}/deploy-estimate`, {
    method: "POST",
    body,
    signal,
  }).then((r) => r.data);

export const deployContract = (spaceID, networkID, accountID, body, idempotencyKey, signal) =>
  request(`${accountBase(spaceID, networkID, accountID)}/deploy`, {
    method: "POST",
    body,
    headers: { "Idempotency-Key": idempotencyKey },
    signal,
  }).then((r) => r.data);
