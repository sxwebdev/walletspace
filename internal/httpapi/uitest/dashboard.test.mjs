import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";

const source = readFileSync(new URL("../ui/views/dashboard.js", import.meta.url), "utf8")
  .replace(/^import[\s\S]*?from "[^\"]+";\n/gm, "")
  .replace(/^export /gm, "");

function dashboard() {
  const state = { currentSpaceID: "space", assets: [], prices: new Map() };
  const context = vm.createContext({
    state, console, document: { createElement: () => ({ isConnected: true }) },
    pendingGasRefuel: () => null,
    setTimeout: () => 1, clearTimeout() {},
    escapeHTML: (text) => String(text), addressGroups: (text) => text,
    shortAddress: (text) => text, modal: (options) => { context.dialog = options; },
    crypto: { randomUUID: () => "conversion-intent" },
    FormData: class {
      constructor(form) { this.form = form; }
      get(name) { return this.form.elements[name]?.value; }
    },
  });
  vm.runInContext(source, context);
  return context;
}

function mount(context, action = "wrap") {
  const node = () => ({ hidden: false, disabled: false, textContent: "", innerHTML: "", handlers: {},
    addEventListener(type, handler) { this.handlers[type] = handler; },
    replaceChildren() { this.innerHTML = ""; },
  });
  const fields = Object.fromEntries(["action", "asset_id", "to", "amount"].map((name) => [name, node()]));
  fields.action.value = action;
  fields.to.value = "0xwallet";
  fields.amount.value = "1.25";
  const nodes = Object.fromEntries(["[data-max]", "[data-estimate-button]", "[data-sign]", "[data-error]", "[data-estimate]", "[data-refuel]"].map((name) => [name, node()]));
  const form = Object.assign(node(), { elements: fields,
    querySelector(selector) { return nodes[selector] || fields[selector.match(/name="(.*?)"/)?.[1]]; },
  });
  context.dialog.onMount({ querySelector: () => form }, () => {});
  return { form, fields, nodes };
}

const native = { id: "native", network_id: "eth", kind: "native", symbol: "ETH" };
const wrapped = { id: "weth", network_id: "eth", kind: "erc20", symbol: "WETH", contract: "0xwrapper" };
const network = { id: "eth", family: "evm", name: "Ethereum", chain_id: "1", native: { symbol: "ETH" }, wrapped_native: { symbol: "WETH", contract: "0xwrapper" } };

test("conversion selects the source asset and invalidates a quote on direction change", async () => {
  const context = dashboard();
  context.state.assets = [native, wrapped];
  const requests = [];
  context.estimateTransfer = async (...args) => {
    requests.push(args[2]);
    return { amount: args[2].amount, fee: "0.001" };
  };
  context.setBusy = () => {};
  context.showSend({ id: "wallet", addresses: { evm: "0xwallet" } }, network, true);
  assert.match(context.dialog.content, /ETH → WETH/);
  assert.match(context.dialog.content, /WETH → ETH/);
  const { form, fields, nodes } = mount(context);
  assert.equal(fields.asset_id.value, "native");
  await form.handlers.submit({ preventDefault() {} });
  assert.equal(requests[0].action, "wrap");
  assert.equal(requests[0].to, "0xwallet");
  assert.equal(nodes["[data-sign]"].hidden, false);
  assert.match(nodes["[data-estimate]"].innerHTML, /ETH → WETH/);
  fields.action.value = "unwrap";
  fields.action.handlers.change();
  form.handlers.change();
  assert.equal(fields.asset_id.value, "weth");
  assert.equal(nodes["[data-sign]"].hidden, true);
  await form.handlers.submit({ preventDefault() {} });
  assert.equal(requests[1].action, "unwrap");
  assert.equal(requests[1].asset_id, "weth");
  assert.match(nodes["[data-estimate]"].innerHTML, /WETH → ETH/);
});

test("an old estimate cannot arm signing after the direction changes", async () => {
  const context = dashboard();
  context.state.assets = [native, wrapped];
  let resolve;
  context.estimateTransfer = () => new Promise((done) => { resolve = done; });
  context.setBusy = () => {};
  context.showSend({ id: "wallet", addresses: { evm: "0xwallet" } }, network, true);
  const { form, fields, nodes } = mount(context);
  const pending = form.handlers.submit({ preventDefault() {} });
  fields.action.value = "unwrap";
  fields.action.handlers.change();
  form.handlers.change();
  resolve({ fee: "0.001" });
  await pending;
  assert.equal(nodes["[data-sign]"].hidden, true);
});

test("USD values label unavailable, zero, cached and testnet balances", () => {
  const context = dashboard();
  context.state.prices.set("native", { current_usd: "2000" });
  assert.match(context.balanceUSD({ amount: "1.25" }, native, network), /\$2,500.00/);
  assert.match(context.balanceUSD({ amount: "0" }, wrapped, network), /\$0.00/);
  assert.match(context.balanceUSD({ amount: "1" }, wrapped, network), /USD unavailable/);
  assert.match(context.balanceUSD({ amount: "1", stale: true }, native, network), /≈ \$2,000.00/);
  assert.match(context.balanceUSD({ amount: "1" }, native, { testnet: true }), /Testnet/);
});

test("missing native gas offers refuel and only arms the token after a fresh estimate", async () => {
  const context = dashboard();
  context.state.assets = [native, wrapped];
  let funded = false;
  context.estimateTransfer = async () => ({ amount: "1.25", fee: "0.001", fee_shortfall: funded ? "0" : "0.001" });
  context.showGasRefuel = async (options) => { context.refuelOptions = options; };
  context.setBusy = () => {};
  context.showSend({ id: "wallet", addresses: { evm: "0xwallet" } }, network);
  const { form, fields, nodes } = mount(context);
  fields.asset_id.value = "weth";
  await form.handlers.submit({ preventDefault() {} });
  assert.equal(nodes["[data-sign]"].hidden, true);
  assert.equal(context.refuelOptions.body.to, "0xwallet");
  funded = true;
  await context.refuelOptions.onReady();
  assert.equal(nodes["[data-sign]"].hidden, false);
  assert.match(nodes["[data-estimate]"].innerHTML, /Amount: 1.25 WETH/);
});

test("delivery of gas never authorizes a token transfer edited in the meantime", async () => {
  const context = dashboard();
  context.state.assets = [native, wrapped];
  context.estimateTransfer = async () => ({ fee: "0.001", fee_shortfall: "0.001" });
  context.showGasRefuel = async (options) => { context.refuelOptions = options; };
  context.setBusy = () => {};
  context.showSend({ id: "wallet", addresses: { evm: "0xwallet" } }, network);
  const { form, fields, nodes } = mount(context);
  fields.asset_id.value = "weth";
  await form.handlers.submit({ preventDefault() {} });
  fields.to.value = "0xdifferent";
  form.handlers.input();
  await context.refuelOptions.onReady();
  assert.equal(nodes["[data-sign]"].hidden, true);
});
