import { request } from "./client.js";

export const listPrices = (assetIDs, signal) => {
  const query = new URLSearchParams(assetIDs.map((assetID) => ["asset_id", assetID]));
  return request(`/api/prices?${query}`, { signal }).then((response) => response.data);
};
