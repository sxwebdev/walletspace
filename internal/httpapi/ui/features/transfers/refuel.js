import { quoteGasRefuel, sendGasRefuel, gasRefuelStatus } from "../../services/transfers.js";
import { withSendConfirmation } from "../spaces/dialogs.js";
import { state, balanceKey } from "../../state/store.js";
import { escapeHTML, addressGroups } from "../../components/ui.js";

const pendingKey = (spaceID, networkID, accountID) => `walletspace:refuel:${spaceID}:${networkID}:${accountID}`;
export const pendingGasRefuel = (spaceID, networkID, accountID) =>
  sessionStorage.getItem(pendingKey(spaceID, networkID, accountID));

// Keep native amounts exact even above JavaScript's safe integer range.
export function nativeAmount(raw, decimals) {
  const digits = String(raw).padStart(decimals + 1, "0");
  const whole = decimals ? digits.slice(0, -decimals) : digits;
  const fraction = decimals ? digits.slice(-decimals).replace(/0+$/, "") : "";
  return `${whole}${fraction ? `.${fraction}` : ""}`;
}

const usd = (value) => new Intl.NumberFormat("en-US", {
  style: "currency", currency: "USD", maximumFractionDigits: 4,
}).format(Number(value));

// The form owns this panel. Removing it stops UI updates, while the durable
// backend plan and the pending id survive a page reload without another deposit.
export async function showGasRefuel({ container, spaceID, account, network, body, estimate, signal, onReady, planID, onResume }) {
  const storageKey = pendingKey(spaceID, network.id, account.id);
  const live = () => container.isConnected && !signal?.aborted;
  const statusBox = () => container.querySelector("[data-refuel-status]");
  const report = (text) => { if (live() && statusBox()) statusBox().textContent = text; };
  let activePlan;
  let funding = false;
  let generation = 0;
  container.innerHTML = `<div class="notice">
    <strong>Not enough ${escapeHTML(network.native.symbol)} for this transfer</strong>
    <p>We can top up this wallet from another wallet, then send your token on ${escapeHTML(network.name)}.</p>
    ${estimate ? `<p>Native balance: ${escapeHTML(estimate.native_balance)} · transfer fee budget: ${escapeHTML(estimate.fee)} · missing: ${escapeHTML(estimate.fee_shortfall)} ${escapeHTML(network.native.symbol)}</p>` : ""}
    <p>A 10% fee reserve and any provider minimum are included below. The final remainder depends on the mined fee.</p>
    <div data-refuel-choices></div>
    <div class="error-text" role="status" data-refuel-status></div>
  </div>`;

  const choices = container.querySelector("[data-refuel-choices]");
  const showPlan = (plan) => {
    activePlan = plan;
    const sourceNetwork = state.networks.find((item) => item.id === plan.source_network_id);
    const symbol = sourceNetwork?.native.symbol || "";
    const amount = nativeAmount(plan.quote.deposit, sourceNetwork?.native.decimals || 18);
    choices.querySelector("[data-refuel-detail]").innerHTML = `
      <p><strong>${escapeHTML(plan.source_label || plan.source_account_id)} · ${escapeHTML(sourceNetwork?.name || plan.source_network_id)}</strong></p>
      <p>Pay: ${escapeHTML(String(amount))} ${escapeHTML(symbol)} + up to ${escapeHTML(plan.source_estimate.fee)} ${escapeHTML(symbol)} source gas.<br>
      <strong>Total up to: ${escapeHTML(plan.total_source)} ${escapeHTML(symbol)}${plan.total_usd ? ` · ≈ ${usd(plan.total_usd)}` : " · USD unavailable"}</strong></p>
      <p>Receive: ${escapeHTML(plan.expected)} ${escapeHTML(network.native.symbol)} in this wallet.<br>
      Estimated remainder after the token transfer: ${escapeHTML(plan.remainder)} ${escapeHTML(network.native.symbol)}.<br>
      ${plan.quote.minimum_limited ? "The provider minimum is larger than the gas needed for this transfer.<br>" : ""}
      Route: ${escapeHTML(plan.quote.provider)} · quote expires ${escapeHTML(new Date(plan.quote.expires_at).toLocaleTimeString())}.</p>
      <details><summary>Check addresses</summary>
        <p>Source: <span class="mono address-full">${addressGroups(plan.source_address)}</span><br>
        Gas recipient: <span class="mono address-full">${addressGroups(plan.recipient)}</span><br>
        Token recipient: <span class="mono address-full">${addressGroups(plan.target.to)}</span></p>
      </details>`;
  };

  const monitor = async (id) => {
    report("Waiting for the gas top-up. This may take several minutes; do not pay again.");
    const deadline = Date.now() + 10 * 60_000;
    while (live() && Date.now() < deadline) {
      try {
        const status = await gasRefuelStatus(spaceID, network.id, id, signal);
        if (!live()) return;
        if (status.status === "delivered") {
          sessionStorage.removeItem(storageKey);
          report("Gas received. Rechecking the token transfer fee…");
          try { await onReady(status.plan); } catch (cause) { report(cause.message); }
          return;
        }
        if (["failed", "cancelled"].includes(status.status)) {
          sessionStorage.removeItem(storageKey);
          report(status.status === "cancelled" ? "The provider cancelled this top-up. Check its refund before paying again." : "The source transaction failed. Calculate a new quote to retry.");
          return;
        }
        report(status.status === "not_started" ? "The top-up has not been submitted. Resume the same quote to check or send it." : "Top-up in progress. Waiting for confirmed delivery; no second payment will be submitted.");
      } catch (cause) {
        if (cause.name === "AbortError") return;
        report(`${cause.message}. The top-up id is saved; checking again without resending.`);
      }
      await new Promise((resolve) => setTimeout(resolve, 5000));
    }
    report("Still waiting. Reopen this transfer and resume the saved top-up to check its status.");
  };

  const submitPlan = async (plan) => {
    if (funding) return;
    funding = true;
    // Save before the request: a lost response is always retried as this plan.
    sessionStorage.setItem(storageKey, plan.id);
    const button = choices.querySelector("[data-refuel-send]");
    if (button) button.disabled = true;
    report("Confirm the gas top-up. The token transfer will be reviewed again after delivery.");
    try {
      await withSendConfirmation(spaceID, () => {
        if (!live()) throw new Error("The transfer form changed. Reopen it to resume this top-up.");
        return sendGasRefuel(spaceID, network.id, plan.id, signal);
      });
      if (live()) await monitor(plan.id);
    } catch (cause) {
      // Keep the plan id even after an HTTP error: the backend alone knows
      // whether a transaction reached a node. Retry the same plan, never re-quote.
      report(cause.message);
      if (button && live()) { button.disabled = false; button.textContent = "Resume this top-up"; }
    } finally { funding = false; }
  };

  if (planID) {
    try {
      const result = await gasRefuelStatus(spaceID, network.id, planID, signal);
      if (!live()) return;
      onResume?.(result.plan);
      choices.innerHTML = '<div data-refuel-detail></div><button class="button primary" type="button" data-refuel-send>Resume this top-up</button>';
      showPlan(result.plan);
      choices.querySelector("[data-refuel-send]").addEventListener("click", () => submitPlan(activePlan));
      if (result.status !== "not_started") await monitor(planID);
    } catch (cause) { report(cause.message); }
    return;
  }

  const sources = [];
  for (const donor of state.accounts) {
    for (const sourceNetwork of state.networks) {
      if (!sourceNetwork.enabled || sourceNetwork.testnet || sourceNetwork.family !== "evm" ||
          !donor.network_ids?.includes(sourceNetwork.id) || (donor.id === account.id && sourceNetwork.id === network.id)) continue;
      const balance = state.balances.get(balanceKey(spaceID, sourceNetwork.id, donor.id, `${sourceNetwork.id}:native`));
      if (balance?.error || !(Number(balance?.amount) > 0)) continue;
      sources.push({ accountID: donor.id, networkID: sourceNetwork.id,
        label: `${donor.label || donor.id} · ${sourceNetwork.name} · ${balance.amount} ${sourceNetwork.native.symbol}` });
    }
  }
  choices.innerHTML = `<label class="field"><span>Pay gas from</span><select data-refuel-source>
    <option value="">Compare available wallets</option>${sources.map((source, index) => `<option value="${index}">${escapeHTML(source.label)}</option>`).join("")}
    </select></label><button class="button" type="button" data-refuel-refresh>Calculate top-up options</button>
    <div data-refuel-list></div><div data-refuel-detail></div>
    <button class="button primary" type="button" data-refuel-send hidden disabled>Top up gas, then continue</button>`;
  const sourceSelect = choices.querySelector("[data-refuel-source]");
  const sendButton = choices.querySelector("[data-refuel-send]");
  let armTimer;
  const calculate = async () => {
    if (funding) return;
    const mine = ++generation;
    activePlan = undefined;
    clearTimeout(armTimer);
    sendButton.hidden = true;
    sendButton.disabled = true;
    choices.querySelector("[data-refuel-list]").replaceChildren();
    choices.querySelector("[data-refuel-detail]").replaceChildren();
    report("Checking balances, source fees and exact-output top-up quotes…");
    try {
      const result = await quoteGasRefuel(spaceID, network.id, body, sources[sourceSelect.value], signal);
      if (!live() || mine !== generation) return;
      if (!(Number(result.estimate.fee_shortfall) > 0)) { await onReady(); return; }
      if (!result.quotes.length) {
        report(`No affordable top-up route found. ${result.unavailable?.join("; ") || "Connect another funded EVM wallet to this space."}`);
        return;
      }
      const list = choices.querySelector("[data-refuel-list]");
      list.innerHTML = `<label class="field"><span>Top-up options, lowest known USD cost first</span><select data-refuel-plan>${result.quotes.map((plan, i) => {
        const donorNetwork = state.networks.find((item) => item.id === plan.source_network_id);
        return `<option value="${i}">${escapeHTML(plan.source_label || plan.source_account_id)} · ${escapeHTML(donorNetwork?.name || plan.source_network_id)} · ${escapeHTML(plan.total_source)} ${escapeHTML(donorNetwork?.native.symbol || "")}${plan.total_usd ? ` ≈ ${usd(plan.total_usd)}` : ""}</option>`;
      }).join("")}</select></label>`;
      const selectPlan = (index) => {
        showPlan(result.quotes[index]);
        clearTimeout(armTimer);
        sendButton.hidden = false;
        sendButton.disabled = true;
        armTimer = setTimeout(() => { if (live()) sendButton.disabled = false; }, 600);
      };
      list.querySelector("[data-refuel-plan]").addEventListener("change", (event) => selectPlan(event.target.value));
      selectPlan(0);
      report(result.sources_total > result.sources_checked ? `Compared ${result.sources_checked} sources. Select a specific wallet to check another source.` : "Review the route and total cost before signing the top-up.");
    } catch (cause) {
      if (mine !== generation || !live()) return;
      if (cause.payload?.pending_plan_id) {
        sessionStorage.setItem(storageKey, cause.payload.pending_plan_id);
        await showGasRefuel({container,spaceID,account,network,body,estimate,signal,onReady,planID:cause.payload.pending_plan_id,onResume});
        return;
      }
      report(cause.message);
    }
  };
  choices.querySelector("[data-refuel-refresh]").addEventListener("click", calculate);
  sourceSelect.addEventListener("change", calculate);
  sendButton.addEventListener("click", () => {
    if (!activePlan) return;
    if (Date.parse(activePlan.quote.expires_at) <= Date.now() && sessionStorage.getItem(storageKey) !== activePlan.id) { report("Quote expired. Calculate new top-up options."); sendButton.disabled = true; return; }
    submitPlan(activePlan);
  });
  await calculate();
}
