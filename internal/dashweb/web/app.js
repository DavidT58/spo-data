/* SPO dashboard frontend: hash routing over two JSON endpoints, refreshed every
   60 s. No external dependencies. */
"use strict";

const REFRESH_MS = 60_000;
let refreshTimer = null;

const $view = document.getElementById("view");
const $tiles = document.getElementById("epoch-tiles");
const $footer = document.getElementById("footer-status");

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

function renderTiles(epoch, totals) {
  $tiles.replaceChildren();
  if (!epoch) return;
  const tiles = [
    ["Epoch", `${epoch.number}`, `${epoch.progress_pct.toFixed(0)}%`],
    ["Scheduled", totals.scheduled, ""],
    ["Produced", totals.produced, ""],
    ["Missed", totals.missed, ""],
    ["Upcoming", totals.pending, ""],
  ];
  for (const [label, value, extra] of tiles) {
    const v = el("div", { class: "value" }, String(value));
    if (extra) v.append(" ", el("small", {}, extra));
    $tiles.append(el("div", { class: "tile" }, el("div", { class: "label" }, label), v));
  }
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
  const totals = { scheduled: 0, produced: 0, missed: 0, pending: 0 };
  for (const op of data.operators || []) {
    for (const p of op.pools) {
      totals.scheduled += p.scheduled;
      totals.produced += p.produced;
      totals.missed += p.missed;
      totals.pending += p.pending;
    }
  }

  const frag = document.createDocumentFragment();
  if (data.epoch) {
    frag.append(el("p", { class: "note" },
      `Epoch ${data.epoch.number}: ${fmtUTC(data.epoch.start_time)} → ${fmtUTC(data.epoch.end_time)}`));
  }
  for (const op of data.operators || []) {
    frag.append(el("h2", { class: "operator" }, op.name));
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
  return { apply() { renderTiles(data.epoch, totals); $view.replaceChildren(frag); } };
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
  try {
    const m = hash.match(/^#\/pool\/(.+)$/);
    const frag = m ? await buildPool(decodeURIComponent(m[1])) : await buildOverview();
    if (seq !== routeSeq) return; // superseded by a newer navigation
    frag.apply();
    lastGoodHash = hash;
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
