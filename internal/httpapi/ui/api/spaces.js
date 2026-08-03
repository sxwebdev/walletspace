import { request } from "./client.js";

export const listSpaces = (signal) => request("/api/spaces", { signal }).then((r) => r.data.spaces);
export const getSpace = (id, signal) => request(`/api/spaces/${id}`, { signal }).then((r) => r.data);
export const createSpace = (body, signal) =>
  request("/api/spaces", { method: "POST", body, signal }).then((r) => r.data);
export const renameSpace = (id, name) =>
  request(`/api/spaces/${id}`, { method: "PATCH", body: { name } }).then((r) => r.data);
export const unlockSpace = (id, password) =>
  request(`/api/spaces/${id}/unlock`, { method: "POST", body: { password } });
export const lockSpace = (id) =>
  request(`/api/spaces/${id}/lock`, { method: "POST", body: {} });
export const revealMnemonic = (id, password) =>
  request(`/api/spaces/${id}/mnemonic`, { method: "POST", body: { password } })
    .then((r) => r.data.mnemonic);
export const changeSpacePassword = (id, currentPassword, newPassword) =>
  request(`/api/spaces/${id}/change-password`, {
    method: "POST",
    body: { current_password: currentPassword, new_password: newPassword },
  });
export async function downloadBackup(id, name) {
  const response = await fetch(`/api/spaces/${id}/backup`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: "{}",
  });
  if (!response.ok) {
    const payload = await response.json().catch(() => ({}));
    throw new Error(payload.error || `HTTP ${response.status}`);
  }
  const url = URL.createObjectURL(await response.blob());
  const link = document.createElement("a");
  link.href = url;
  link.download = `${name || "walletspace"}-backup.json`;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}
