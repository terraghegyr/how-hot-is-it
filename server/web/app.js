"use strict";

const PALETTE = ["#58a6ff", "#3fb950", "#d29922", "#f85149", "#bc8cff", "#39c5cf", "#ff7b72", "#a5d6ff"];
// Alert threshold (°C). Not hardcoded — sourced from GET /api/config on every
// refresh, so it always reflects the server's configured alert_threshold_c
// (and picks up a config change after a server restart without a page reload).
let threshold = null;
let chart = null;
let chartMachineIDs = [];

const $ = (sel) => document.querySelector(sel);

async function getJSON(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error(url + " -> " + r.status);
  return r.json();
}

function colorFor(i) { return PALETTE[i % PALETTE.length]; }

// Shorter chart on phones so it fits above the panel without scrolling.
function chartHeight() { return window.innerWidth <= 560 ? 240 : 360; }

// Draw a dashed horizontal reference line at the alert threshold.
const thresholdPlugin = {
  hooks: {
    draw: (u) => {
      if (threshold == null) return; // config not loaded yet
      const y = u.valToPos(threshold, "y", true);
      const ctx = u.ctx;
      ctx.save();
      ctx.strokeStyle = "#f85149";
      ctx.setLineDash([5, 4]);
      ctx.lineWidth = 1;
      ctx.beginPath();
      ctx.moveTo(u.bbox.left, y);
      ctx.lineTo(u.bbox.left + u.bbox.width, y);
      ctx.stroke();
      ctx.restore();
    },
  },
};

function makeChart(names) {
  if (chart) { chart.destroy(); chart = null; }
  const series = [{}];
  names.forEach((name, i) => {
    series.push({
      label: name,
      stroke: colorFor(i),
      width: 2,
      spanGaps: false,
    });
  });
  const opts = {
    width: $("#chart").clientWidth || 800,
    height: chartHeight(),
    scales: { x: { time: true } },
    axes: [
      { stroke: "#8b909a", grid: { stroke: "#2a2e37" } },
      { stroke: "#8b909a", grid: { stroke: "#2a2e37" }, values: (u, vals) => vals.map((v) => v + "°") },
    ],
    series,
    plugins: [thresholdPlugin],
  };
  chart = new uPlot(opts, [[]], $("#chart"));
}

function fmtTemp(v) { return v == null ? "—" : v.toFixed(1) + "°C"; }

function dotClass(m) {
  const now = Date.now() / 1000;
  if (m.latest_c == null || m.latest_ts == null) return "grey";
  if (now - m.latest_ts > 600) return "grey"; // stale (>10 min)
  if (threshold == null) return "green"; // config not loaded yet
  if (m.latest_c >= threshold) return "red";
  if (m.latest_c >= threshold - 10) return "amber";
  return "green";
}

function renderMachines(machines) {
  const tb = $("#machines tbody");
  tb.innerHTML = "";
  if (machines.length === 0) {
    tb.innerHTML = '<tr><td colspan="3" style="color:var(--muted)">No machines yet — add one.</td></tr>';
    return;
  }
  for (const m of machines) {
    const tr = document.createElement("tr");
    tr.innerHTML =
      `<td><span class="dot ${dotClass(m)}"></span>${escapeHTML(m.name)}</td>` +
      `<td class="temp">${fmtTemp(m.latest_c)}</td>` +
      `<td><button class="danger" data-id="${m.id}">✕</button></td>`;
    tr.querySelector("button").addEventListener("click", () => deleteMachine(m.id, m.name));
    tb.appendChild(tr);
  }
}

const EVENT_LABEL = {
  breach: "🔥 breach",
  recovery: "✅ recovery",
  stale: "⚠️ stale",
  stale_recovery: "📡 reporting again",
};

function renderAlerts(alerts) {
  const tb = $("#alerts tbody");
  tb.innerHTML = "";
  if (alerts.length === 0) {
    tb.innerHTML = '<tr><td colspan="4" style="color:var(--muted)">No alerts yet.</td></tr>';
    return;
  }
  for (const a of alerts) {
    const tr = document.createElement("tr");
    const when = fmtAlertTime(a.ts);
    const tg = a.telegram_ok ? "" : '<span class="tg-fail" title="Telegram delivery failed">TG ✗</span>';
    tr.innerHTML =
      `<td class="nowrap">${when}</td>` +
      `<td class="nowrap">${escapeHTML(a.machine_name)}</td>` +
      `<td>${EVENT_LABEL[a.type] || a.type}${tg}</td>` +
      `<td class="temp">${a.temp_c == null ? "—" : a.temp_c.toFixed(1) + "°"}</td>`;
    tb.appendChild(tr);
  }
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

// Compact local time "M/D HH:MM" — the full locale string is too wide for the panel.
function fmtAlertTime(ts) {
  const d = new Date(ts * 1000);
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getMonth() + 1}/${d.getDate()} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

async function refresh() {
  try {
    const [cfg, machines, history, alerts] = await Promise.all([
      getJSON("/api/config"),
      getJSON("/api/machines"),
      getJSON("/api/history?ids=all"),
      getJSON("/api/alerts?limit=50"),
    ]);
    if (typeof cfg.alert_threshold_c === "number") threshold = cfg.alert_threshold_c;
    renderMachines(machines);
    renderAlerts(alerts);

    // Map history series ids to names via the machines list.
    const nameByID = {};
    machines.forEach((m) => (nameByID[m.id] = m.name));
    const names = history.ids.map((id) => nameByID[id] || id);

    const idsChanged = names.length !== chartMachineIDs.length ||
      history.ids.some((id, i) => id !== chartMachineIDs[i]);
    if (!chart || idsChanged) {
      chartMachineIDs = history.ids.slice();
      makeChart(names);
    }
    // history.data is already uPlot columnar: [ [ts...], [series1...], ... ]
    chart.setData(history.data.length ? history.data : [[]]);
  } catch (e) {
    console.error(e);
  }
}

// ---- enrollment dialog ----
// The agent's config env vars, pasted into the top of agent.sh.
function envSnippet(id) {
  return [`SERVER_URL="${location.origin}"`, `MACHINE_ID="${id}"`].join("\n");
}

// Cron entries. Cron's finest granularity is one minute, so "every 30s" is two
// entries — one on the minute and one offset by 30s.
function cronSnippet() {
  return [
    "* * * * * /opt/how-hot-is-it/agent.sh",
    "* * * * * sleep 30; /opt/how-hot-is-it/agent.sh",
  ].join("\n");
}

// Wire a Copy button to copy the text of its target <pre>, with brief feedback.
function wireCopy(btn) {
  btn.addEventListener("click", () => {
    const text = $("#" + btn.dataset.copy).textContent;
    navigator.clipboard.writeText(text);
    const orig = btn.textContent;
    btn.textContent = "Copied!";
    setTimeout(() => (btn.textContent = orig), 1500);
  });
}

function setupDialog() {
  const dlg = $("#add-dialog");
  const form = $("#add-form");
  const result = $("#enroll-result");

  $("#add-btn").addEventListener("click", () => {
    form.hidden = false;
    result.hidden = true;
    $("#machine-name").value = "";
    dlg.showModal();
    $("#machine-name").focus();
  });
  $("#add-cancel").addEventListener("click", () => dlg.close());
  $("#enroll-close").addEventListener("click", () => dlg.close());

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const name = $("#machine-name").value.trim();
    if (!name) return;
    try {
      const m = await (await fetch("/api/machines", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ name }),
      })).json();
      $("#enroll-env").textContent = envSnippet(m.id);
      $("#enroll-cron").textContent = cronSnippet();
      form.hidden = true;
      result.hidden = false;
      refresh();
    } catch (err) {
      alert("Failed to create machine: " + err);
    }
  });

  document.querySelectorAll("[data-copy]").forEach(wireCopy);
}

// Styled in-app confirmation (native confirm() looks out of place with the UI).
// Resolves true if the user confirms, false on Cancel or Esc.
function showConfirm(message) {
  return new Promise((resolve) => {
    const dlg = $("#confirm-dialog");
    $("#confirm-msg").textContent = message;
    const done = (result) => {
      $("#confirm-ok").removeEventListener("click", onOk);
      $("#confirm-cancel").removeEventListener("click", onCancel);
      dlg.removeEventListener("cancel", onCancel);
      if (dlg.open) dlg.close();
      resolve(result);
    };
    const onOk = () => done(true);
    const onCancel = () => done(false);
    $("#confirm-ok").addEventListener("click", onOk);
    $("#confirm-cancel").addEventListener("click", onCancel);
    dlg.addEventListener("cancel", onCancel); // Esc key
    dlg.showModal();
  });
}

async function deleteMachine(id, name) {
  const ok = await showConfirm(`Delete "${name}"? Its readings will be removed; past alerts stay in history.`);
  if (!ok) return;
  await fetch("/api/machines/" + id, { method: "DELETE" });
  refresh();
}

// ---- tabs (Machines / Alerts) ----
function setupTabs() {
  const tabs = document.querySelectorAll(".tab");
  tabs.forEach((tab) => {
    tab.addEventListener("click", () => {
      tabs.forEach((t) => {
        const on = t === tab;
        t.classList.toggle("active", on);
        t.setAttribute("aria-selected", on ? "true" : "false");
      });
      $("#tab-machines").hidden = tab.dataset.tab !== "machines";
      $("#tab-alerts").hidden = tab.dataset.tab !== "alerts";
    });
  });
}

// Poll interval (ms). Agents report every 30s, so the dashboard polls at the
// same cadence — the simplest form of "live". See README for why this stays
// polling rather than SSE/WebSockets.
const REFRESH_MS = 30000;

async function init() {
  setupTabs();
  setupDialog();
  await refresh(); // pulls config (threshold), machines, history, alerts
  setInterval(refresh, REFRESH_MS);
  window.addEventListener("resize", () => {
    if (chart) chart.setSize({ width: $("#chart").clientWidth, height: chartHeight() });
  });
}

init();
