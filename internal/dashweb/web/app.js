/* SPO dashboard frontend: hash routing over the JSON endpoints, refreshed every
   60 s. No external dependencies. */
"use strict";

const REFRESH_MS = 60_000;
let refreshTimer = null;

const $view = document.getElementById("view");
const $tiles = document.getElementById("epoch-tiles");
const $footer = document.getElementById("footer-status");
const $rfBadge = document.getElementById("rf-badge");

// --- helpers ---

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") node.className = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v);
  }
  for (const c of children.flat()) {
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

function fmtUTC(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  return d.toISOString().replace("T", " ").slice(0, 16) + " UTC";
}

function fmtRelative(iso) {
  if (!iso) return "";
  const diff = (new Date(iso) - Date.now()) / 1000;
  const abs = Math.abs(diff);
  let out;
  if (abs < 90) out = `${Math.round(abs)}s`;
  else if (abs < 5400) out = `${Math.round(abs / 60)}m`;
  else if (abs < 129600) out = `${(abs / 3600).toFixed(1)}h`;
  else out = `${(abs / 86400).toFixed(1)}d`;
  return diff >= 0 ? `in ${out}` : `${out} ago`;
}

// Status badge: dot + text, meaning never carried by color alone.
const SCHED_BADGES = {
  ok: ["good", "schedule ok"],
  pending: ["neutral", "computing…"],
  key_missing: ["serious", "key missing"],
  key_stale: ["serious", "key stale"],
  cli_error: ["serious", "cli error"],
};
const SLOT_BADGES = {
  produced: ["good", "✓ produced"],
  missed: ["critical", "✕ missed"],
  pending: ["neutral", "upcoming"],
};

function badge(map, key) {
  const [cls, label] = map[key] || ["neutral", key];
  return el("span", { class: `badge ${cls}` }, label);
}

async function fetchJSON(path) {
  const res = await fetch(path, { cache: "no-store" });
  if (!res.ok) throw new Error(`${path}: HTTP ${res.status}`);
  return res.json();
}

// --- overview ---

function fmtAp3x(n) {
  return n.toLocaleString("en-US", { maximumFractionDigits: n >= 100 ? 0 : 1 });
}
function fmtUsd(n) {
  return "$" + n.toLocaleString("en-US", { maximumFractionDigits: n >= 100 ? 0 : 2 });
}

// Header tiles: [label, value (text or Node), small extra text].
function setTiles(tiles) {
  $tiles.replaceChildren(...tiles.map(([label, value, extra]) => {
    const v = el("div", { class: "value" }, value);
    if (extra) v.append(" ", el("small", {}, extra));
    return el("div", { class: "tile" }, el("div", { class: "label" }, label), v);
  }));
}

function renderTiles(epoch, totals, price) {
  if (!epoch) return setTiles([]);
  const tiles = [
    ["Epoch", `${epoch.number}`, `${epoch.progress_pct.toFixed(0)}%`],
    ["Scheduled", totals.scheduled, ""],
    ["Produced", totals.produced, ""],
    ["Missed", totals.missed, ""],
    ["Upcoming", totals.pending, ""],
  ];
  if (totals.est > 0) {
    tiles.push(["Est. earned", `${fmtAp3x(totals.est)} AP3X`, price > 0 ? fmtUsd(totals.est * price) : ""]);
  }
  if (price > 0) {
    tiles.push(["AP3X", `$${price}`, ""]);
  }
  setTiles(tiles);
}

function poolRow(p) {
  const flagged = p.missed > 0;
  let status;
  if (flagged) {
    status = el("span", { class: "badge critical" }, `${p.missed} missed`);
  } else {
    status = badge(SCHED_BADGES, p.schedule_status);
  }
  const anomalies = p.anomalies == null ? "n/a" : String(p.anomalies);
  const next = p.next_slot_time
    ? el("span", { title: fmtUTC(p.next_slot_time) }, fmtRelative(p.next_slot_time))
    : el("span", { class: "muted" }, "—");
  const row = el(
    "tr",
    { class: `rowlink${flagged ? " flagged" : ""}`, onclick: () => (location.hash = `#/pool/${p.pool_id}`) },
    el("td", {}, el("a", { href: `#/pool/${p.pool_id}` }, p.name)),
    el("td", {}, status),
    el("td", { class: "num" }, p.scheduled),
    el("td", { class: "num" }, p.produced),
    el("td", { class: "num" }, p.missed),
    el("td", { class: "num" }, p.pending),
    el("td", {}, next),
    el("td", { class: "num" }, anomalies),
  );
  return row;
}

async function buildOverview() {
  const data = await fetchJSON("/api/overview");
  const price = data.price_usdt || 0;
  const totals = { scheduled: 0, produced: 0, missed: 0, pending: 0, est: 0 };
  for (const op of data.operators || []) {
    for (const p of op.pools) {
      totals.scheduled += p.scheduled;
      totals.produced += p.produced;
      totals.missed += p.missed;
      totals.pending += p.pending;
      totals.est += p.est_rewards || 0;
    }
  }

  const frag = document.createDocumentFragment();
  if (data.epoch) {
    frag.append(el("p", { class: "note" },
      `Epoch ${data.epoch.number}: ${fmtUTC(data.epoch.start_time)} → ${fmtUTC(data.epoch.end_time)}`));
  }
  for (const op of data.operators || []) {
    const opScheduled = op.pools.reduce((n, p) => n + p.scheduled, 0);
    const opProduced = op.pools.reduce((n, p) => n + p.produced, 0);
    const opEst = op.pools.reduce((n, p) => n + (p.est_rewards || 0), 0);
    let sums = `${opScheduled} scheduled · ${opProduced} produced`;
    if (opEst > 0) {
      sums += ` · ≈${fmtAp3x(opEst)} AP3X`;
      if (price > 0) sums += ` (${fmtUsd(opEst * price)})`;
    }
    frag.append(el("h2", { class: "operator" },
      op.name, " ",
      el("span", { class: "op-sums" }, sums)));
    const tbody = el("tbody", {}, op.pools.map(poolRow));
    frag.append(el("div", { class: "tablewrap" },
      el("table", {},
        el("thead", {}, el("tr", {},
          el("th", {}, "Pool"), el("th", {}, "Status"),
          el("th", { class: "num" }, "Scheduled"), el("th", { class: "num" }, "Produced"),
          el("th", { class: "num" }, "Missed"), el("th", { class: "num" }, "Upcoming"),
          el("th", {}, "Next slot"), el("th", { class: "num" }, "Anomalies"),
        )),
        tbody)));
  }
  return { apply() { renderTiles(data.epoch, totals, price); $view.replaceChildren(frag); } };
}

// --- pool detail ---

async function buildPool(poolID) {
  const p = await fetchJSON(`/api/pool/${encodeURIComponent(poolID)}`);

  const frag = document.createDocumentFragment();
  frag.append(el("a", { class: "backlink", href: "#/" }, "← all pools"));
  frag.append(el("h2", { class: "pool-title" }, `${p.name} `, el("span", { class: "hash" }, p.pool_id)));
  frag.append(el("p", { class: "note" }, `operator ${p.operator} · epoch ${p.epoch} · `, badge(SCHED_BADGES, p.schedule_status)));
  if (p.schedule_error) frag.append(el("div", { class: "error-box" }, p.schedule_error));

  frag.append(el("h3", {}, `Slots this epoch (${p.slots.length})`));
  if (p.slots.length === 0) {
    frag.append(el("p", { class: "note" }, "No leadership slots recorded for this epoch."));
  } else {
    frag.append(el("div", { class: "tablewrap" }, el("table", {},
      el("thead", {}, el("tr", {},
        el("th", {}, "Time (UTC)"), el("th", {}, "When"), el("th", { class: "num" }, "Slot"),
        el("th", {}, "Status"), el("th", {}, "Block / battle"),
      )),
      el("tbody", {}, p.slots.map((sl) => {
        let detail = "";
        if (sl.block_hash) detail = el("span", { class: "hash", title: sl.block_hash }, sl.block_hash.slice(0, 16) + "…");
        else if (sl.battle_leader) detail = el("span", { class: "hash", title: sl.battle_leader }, `lost to ${sl.battle_leader.slice(0, 14)}…`);
        return el("tr", {},
          el("td", {}, fmtUTC(sl.time)),
          el("td", {}, fmtRelative(sl.time)),
          el("td", { class: "num" }, sl.slot),
          el("td", {}, badge(SLOT_BADGES, sl.status)),
          el("td", {}, detail));
      })))));
  }

  if (p.unscheduled_blocks.length > 0) {
    frag.append(el("h3", {}, `Blocks outside the schedule (${p.unscheduled_blocks.length})`));
    frag.append(el("div", { class: "tablewrap" }, el("table", {},
      el("thead", {}, el("tr", {},
        el("th", {}, "Time (UTC)"), el("th", { class: "num" }, "Slot"), el("th", {}, "Block"), el("th", {}, "Classification"),
      )),
      el("tbody", {}, p.unscheduled_blocks.map((b) =>
        el("tr", {},
          el("td", {}, fmtUTC(b.time)),
          el("td", { class: "num" }, b.slot),
          el("td", {}, el("span", { class: "hash", title: b.block_hash }, b.block_hash.slice(0, 16) + "…")),
          el("td", {}, b.anomaly ? badge({ a: ["serious", "anomaly"] }, "a") : badge({ u: ["neutral", "awaiting schedule"] }, "u"))))))));
  }

  frag.append(el("h3", {}, "Epoch history"));
  if (p.history.length === 0) {
    frag.append(el("p", { class: "note" }, "History accumulates from launch (plus backfilled actuals)."));
  } else {
    frag.append(el("div", { class: "tablewrap" }, el("table", {},
      el("thead", {}, el("tr", {},
        el("th", { class: "num" }, "Epoch"), el("th", { class: "num" }, "Scheduled"),
        el("th", { class: "num" }, "Produced"), el("th", { class: "num" }, "Missed"),
        el("th", { class: "num" }, "Expected"), el("th", { class: "num" }, "Luck"), el("th", {}, "Source"),
      )),
      el("tbody", {}, p.history.map((h) =>
        el("tr", {},
          el("td", { class: "num" }, h.epoch),
          el("td", { class: "num" }, h.actuals_only ? "—" : h.scheduled),
          el("td", { class: "num" }, h.produced),
          el("td", { class: "num" }, h.actuals_only ? "—" : h.missed),
          el("td", { class: "num" }, h.expected ? h.expected.toFixed(1) : "—"),
          el("td", { class: "num" }, h.luck_pct != null ? `${h.luck_pct.toFixed(0)}%` : "—"),
          el("td", {}, h.actuals_only
            ? el("span", { class: "muted" }, "actuals only")
            : (h.finalized ? "final" : el("span", { class: "muted" }, "in progress")))))))));
  }

  return { apply() { $tiles.replaceChildren(); $view.replaceChildren(frag); } };
}

// --- reward flow ---
// The snapshot comes from the reward-flow daemon (spo-infra src/reward-flow):
// amounts in AP3X, times in unix seconds, tx hashes but no addresses.

// The page sends no Referer (index.html), so an explorer does not learn the
// dashboard's address.
const EXPLORERS = {
  prime: (tx) => `https://apexscan.org/en/transaction/${tx}`,
  nexus: (tx) => `https://explorer.nexus.mainnet.apexfusion.org/tx/${tx}`,
  layerzero: (tx) => `https://layerzeroscan.com/tx/${tx}`,
};
const RF_STATES = {
  running: ["good", "running"],
  paused: ["critical", "PAUSED"],
  stale: ["serious", "stale"],
  off: ["neutral", "no data"],
};
const STEP_LABELS = {
  withdraw: "pool withdrawal", hop: "hop", credit: "LBank credit", lbank_withdraw: "LBank withdrawal",
  bridge: "bridge request", nexus_payout: "Reactor payout", oft: "LayerZero send", base_delivery: "Base delivery",
};
const STEP_BADGES = {
  waiting: ["neutral", "waiting"], signed: ["warning", "signed"], submitted: ["warning", "submitted"],
  pending: ["warning", "pending"], done: ["good", "done"], failed: ["critical", "failed"],
};
const PART_LABELS = { hop: "in hop wallet", deposited: "waiting for LBank credit", credited: "credited" };
const EVENT_LABELS = {
  reward_withdrawal: "pool withdrawal", payment_sweep: "payment.addr sweep", hop: "hop",
  lbank_deposit: "LBank deposit", lbank_credit: "LBank credit", lbank_withdrawal: "LBank withdrawal",
  bridge_request: "bridge request", nexus_received: "Nexus payout", oft_send: "LayerZero send",
  base_received: "Base delivery",
};
const FEE_NAMES = { prime: "Prime", lbank: "LBank", bridge: "bridge", layerzero: "LayerZero" };

const iso = (sec) => (sec ? new Date(sec * 1000).toISOString() : null);

function when(sec) {
  if (!sec) return el("span", { class: "muted" }, "—");
  return el("span", { title: fmtUTC(iso(sec)) }, fmtRelative(iso(sec)));
}

function fmtAmt(n, digits = 2) {
  if (n == null) return "—";
  return n.toLocaleString("en-US", { minimumFractionDigits: digits, maximumFractionDigits: digits });
}

function fmtAge(sec) {
  if (sec < 90) return `${Math.max(0, Math.round(sec))} s`;
  if (sec < 5400) return `${Math.round(sec / 60)} min`;
  return `${(sec / 3600).toFixed(1)} h`;
}

function txLink(chain, tx, label) {
  if (!tx || !EXPLORERS[chain]) return "";
  return el("a", { class: "hash", href: EXPLORERS[chain](tx), target: "_blank", rel: "noreferrer noopener", title: `${chain}: ${tx}` },
    label || tx.replace(/^0x/, "").slice(0, 8) + "…");
}

function rfState(resp) {
  if (!resp || !resp.available) return "off";
  if (resp.stale) return "stale";
  return resp.snapshot.state.paused ? "paused" : "running";
}

function setRfBadge(resp) {
  $rfBadge.replaceChildren(badge(RF_STATES, rfState(resp)));
}

const num = (v) => el("td", { class: "num" }, v);

// headers: [label, numeric?]
function table(headers, rows) {
  return el("div", { class: "tablewrap" }, el("table", {},
    el("thead", {}, el("tr", {}, headers.map(([label, isNum]) => el("th", isNum ? { class: "num" } : {}, label)))),
    el("tbody", {}, rows)));
}

function balanceRows(bals) {
  const sum = (k) => (bals.some((b) => b[k] != null) ? bals.reduce((n, b) => n + (b[k] || 0), 0) : null);
  const total = { name: "total", fees: {} };
  for (const k of ["rewards_waiting", "rewards_withdrawn", "sweep", "in_hops", "waiting_credit", "on_lbank",
    "to_base", "on_base", "fees_total", "left_in_wallets"]) total[k] = sum(k);
  for (const t of Object.keys(FEE_NAMES)) total.fees[t] = bals.reduce((n, b) => n + b.fees[t], 0);
  const ap3x = (n) => (n == null ? "—" : fmtAp3x(n));
  const row = (b, isTotal) => el("tr", isTotal ? { class: "total" } : {},
    el("td", {}, b.name, isTotal ? "" : el("span", { class: "sub", title: `${b.pools} pools; ${b.weeks_per_withdrawal} week(s) of rewards per LBank withdrawal` },
      `${b.groups.join(" + ")} · ${b.weeks_per_withdrawal} wk`)),
    num(ap3x(b.rewards_waiting)),
    num(el("span", { title: `rewards ${fmtAmt(b.rewards_withdrawn)} + payment.addr sweep ${fmtAmt(b.sweep)}` },
      ap3x(b.rewards_withdrawn + b.sweep))),
    num(ap3x(b.in_hops)), num(ap3x(b.waiting_credit)), num(ap3x(b.on_lbank)), num(ap3x(b.to_base)),
    num(ap3x(b.on_base)),
    num(el("span", { title: Object.entries(FEE_NAMES).map(([t, name]) => `${name} ${fmtAmt(b.fees[t])}`).join(" · ") },
      fmtAmt(b.fees_total))),
    num(fmtAmt(b.left_in_wallets)),
    el("td", {}, isTotal ? "" : when(b.next_lbank_at)));
  return [...bals.map((b) => row(b, false)), row(total, true)];
}

function flowRows(f) {
  const st = f.step;
  const parts = f.parts ? Object.entries(f.parts).map(([k, n]) => `${n} ${PART_LABELS[k] || k}`).join(", ") : "";
  let next = "";
  if (st && st.state === "waiting") next = el("span", {}, "next ", when(st.not_before));
  else if (st) next = el("span", {}, "since ", when(st.started_at || st.updated_at));
  const rows = [el("tr", st && st.error ? { class: "flagged" } : {},
    el("td", {}, f.id, f.manual ? el("span", { class: "sub" }, f.canary ? "canary" : "manual") : ""),
    el("td", {}, f.balance),
    el("td", {}, f.pool || "—"),
    num(fmtAmt(f.amount)),
    el("td", {}, st ? STEP_LABELS[st.kind] || st.kind : "—", parts ? el("span", { class: "sub" }, parts) : ""),
    el("td", {}, st ? badge(STEP_BADGES, st.state) : ""),
    el("td", {}, next),
    el("td", { class: "links" }, f.txs.map((t) => txLink(t.chain, t.tx, STEP_LABELS[t.step] || t.step))))];
  if (st && st.error) rows.push(el("tr", { class: "step-error" }, el("td", { colspan: "8" }, st.error)));
  return rows;
}

function eventRow(e) {
  const links = [txLink(e.tx_chain, e.tx)];
  if (e.kind === "oft_send") links.push(txLink("layerzero", e.tx, "LayerZero"));
  return el("tr", {},
    el("td", {}, fmtUTC(iso(e.at))),
    el("td", {}, EVENT_LABELS[e.kind] || e.kind),
    el("td", {}, e.balance || ""),
    el("td", {}, e.pool || ""),
    el("td", {}, e.flow || ""),
    num(e.amount == null ? "—" : `${fmtAmt(e.amount)} ${e.asset || ""}`),
    num(fmtAmt(e.fee)),
    el("td", { class: "links" }, links));
}

async function buildRewardFlow() {
  const resp = await fetchJSON("/api/reward-flow");
  const frag = document.createDocumentFragment();
  if (!resp.available) {
    frag.append(el("div", { class: "error-box" }, resp.error || "No reward-flow data."));
    return { apply() { setRfBadge(resp); setTiles([]); $view.replaceChildren(frag); } };
  }
  const s = resp.snapshot;
  const bals = s.balances;
  const total = (k) => bals.reduce((n, b) => n + (b[k] || 0), 0);
  const moved = total("rewards_withdrawn") + total("sweep");
  const fees = total("fees_total");
  const waiting = bals.some((b) => b.rewards_waiting != null) ? total("rewards_waiting") : null;
  const tiles = [
    ["Reward flow", badge(RF_STATES, rfState(resp)), s.state.auto ? "auto on" : "auto OFF"],
    ["Mode", s.state.mode, ""],
    ["Waiting in pools", waiting == null ? "—" : fmtAp3x(waiting), "AP3X"],
    ["In progress", fmtAp3x(total("in_hops") + total("waiting_credit") + total("on_lbank") + total("to_base")), "AP3X"],
    ["On Base", fmtAp3x(total("on_base")), "bAP3X"],
    ["Fees", fmtAmt(fees), moved > 0 ? `${(fees / moved * 100).toFixed(1)}% of withdrawn` : ""],
    ["Snapshot", fmtAge(resp.age_seconds), s.revision],
  ];

  if (s.state.paused) {
    frag.append(el("div", { class: "error-box" },
      `PAUSED${s.state.paused_at ? " since " + fmtUTC(iso(s.state.paused_at)) : ""}: ${s.state.pause_reason}\n` +
      "No new step starts; steps already on chain finish. After the check: rf resume (david-main-fr-0)."));
  }
  if (resp.stale) {
    frag.append(el("div", { class: "error-box" },
      `No new snapshot for ${fmtAge(resp.age_seconds)}: the reward-flow daemon is stopped or stuck ` +
      "(docker logs reward-flow on david-main-fr-0)."));
  }

  frag.append(el("h3", {}, "Balances (AP3X)"));
  frag.append(table([["Balance"], ["In pools", 1], ["Withdrawn", 1], ["In hops", 1], ["LBank credit", 1],
    ["On LBank", 1], ["LBank → Base", 1], ["On Base", 1], ["Fees", 1], ["Left", 1], ["Next LBank"]],
  balanceRows(bals)));
  frag.append(el("p", { class: "note" },
    `In pools: rewards in the pools' reward accounts, read ${s.rewards_read_at ? fmtRelative(iso(s.rewards_read_at)) : "—"}. ` +
    "In hops: in hop wallets. LBank credit: deposits that LBank has not credited yet. " +
    "LBank → Base: LBank payouts on their way, minus the fees paid so far. " +
    "Left: gas reserves in the Nexus wallets and funds of closed flows; a larger value means stuck funds. " +
    "Next LBank: the next LBank withdrawal slot."));

  frag.append(el("h3", {}, `Active flows (${s.flows.length})`));
  if (s.flows.length === 0) {
    frag.append(el("p", { class: "note" }, "No active flow."));
  } else {
    frag.append(table([["Flow"], ["Balance"], ["Pool"], ["AP3X", 1], ["Step"], ["State"], ["When"], ["Txs"]],
      s.flows.flatMap(flowRows)));
  }

  const pools = [...s.pools].sort((a, b) => (a.next_at || Infinity) - (b.next_at || Infinity));
  frag.append(el("h3", {}, `Pools (${pools.length}), by next withdrawal`));
  frag.append(table([["Pool"], ["Group"], ["Balance"], ["In pool", 1], ["Withdrawals", 1], ["Last withdrawal"],
    ["Last amount", 1], ["Withdrawn total", 1], ["Next withdrawal"], ["Active flow"]],
  pools.map((p) => el("tr", {},
    el("td", {}, p.ticker), el("td", {}, p.group), el("td", {}, p.balance),
    num(p.rewards_waiting == null ? "—" : fmtAp3x(p.rewards_waiting)),
    num(p.withdrawals), el("td", {}, when(p.last_at)),
    num(p.last_amount == null ? "—" : fmtAp3x(p.last_amount)),
    num(fmtAp3x(p.withdrawn)), el("td", {}, when(p.next_at)), el("td", {}, p.active_flow || "")))));

  frag.append(el("h3", {}, "Per month (AP3X)"));
  if (s.months.length === 0) {
    frag.append(el("p", { class: "note" }, "No ledger rows yet."));
  } else {
    frag.append(table([["Month"], ["Balance"], ["Rewards", 1], ["Sweep", 1], ["Delivered to Base", 1],
      ...Object.values(FEE_NAMES).map((name) => [`${name} fees`, 1]), ["All fees", 1]],
    s.months.map((m) => el("tr", {},
      el("td", {}, m.month), el("td", {}, m.balance), num(fmtAmt(m.rewards)), num(fmtAmt(m.sweep)),
      num(fmtAmt(m.delivered)), ...Object.keys(FEE_NAMES).map((t) => num(fmtAmt(m.fees[t]))),
      num(fmtAmt(m.fees_total))))));
  }

  frag.append(el("h3", {}, `Ledger, last ${s.events.length} rows`));
  if (s.events.length === 0) {
    frag.append(el("p", { class: "note" }, "No ledger rows yet."));
  } else {
    frag.append(table([["Time (UTC)"], ["Event"], ["Balance"], ["Pool"], ["Flow"], ["Amount", 1], ["Fee", 1], ["Tx"]],
      s.events.map(eventRow)));
  }

  return { apply() { setRfBadge(resp); setTiles(tiles); $view.replaceChildren(frag); } };
}

// --- routing + refresh ---

// Monotonic token: a slow response from an earlier navigation must never paint
// over the view the user has since navigated to.
let routeSeq = 0;
// Last hash that rendered successfully: background-refresh failures keep the
// current view (error goes to the footer) instead of blanking the page.
let lastGoodHash = null;

async function route() {
  const hash = location.hash || "#/";
  const seq = ++routeSeq;
  const rf = hash === "#/reward-flow";
  for (const a of document.querySelectorAll(".nav a")) {
    if (a.getAttribute("href") === (rf ? "#/reward-flow" : "#/")) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
  try {
    const m = hash.match(/^#\/pool\/(.+)$/);
    let frag;
    if (m) frag = await buildPool(decodeURIComponent(m[1]));
    else if (rf) frag = await buildRewardFlow();
    else frag = await buildOverview();
    if (seq !== routeSeq) return; // superseded by a newer navigation
    frag.apply();
    lastGoodHash = hash;
    if (!rf) fetchJSON("/api/reward-flow").then(setRfBadge, () => setRfBadge(null));
    const health = await fetchJSON("/api/health").catch(() => null);
    if (health && seq === routeSeq) {
      const sync = health.node_synced ? "node synced" : "NODE NOT SYNCED";
      $footer.textContent = `tip slot ${health.tip_slot} · ${sync} · ${health.pool_count} pools · refreshed ${new Date().toISOString().slice(11, 19)} UTC`;
    }
  } catch (err) {
    if (seq !== routeSeq) return;
    if (hash === lastGoodHash) {
      // Same view, transient refresh failure: keep the stale-but-useful page.
      $footer.textContent = `refresh failed: ${err.message} (showing last good data)`;
      return;
    }
    $view.replaceChildren(el("p", { class: "error-box" }, `Failed to load: ${err.message}`));
  }
}

function scheduleRefresh() {
  clearInterval(refreshTimer);
  refreshTimer = setInterval(route, REFRESH_MS);
}

window.addEventListener("hashchange", () => { route(); scheduleRefresh(); });
route();
scheduleRefresh();
