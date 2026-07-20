"use strict";

const PALETTE = ["#58a6ff", "#3fb950", "#d29922", "#f85149", "#bc8cff", "#39c5cf", "#ff7b72", "#a5d6ff"];
let threshold = 80;
let chart = null;
let chartMachineIDs = [];

const $ = (sel) => document.querySelector(sel);

async function getJSON(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error(url + " -> " + r.status);
  return r.json();
}

function colorFor(i) { return PALETTE[i % PALETTE.length]; }

// Draw a dashed horizontal reference line at the alert threshold.
const thresholdPlugin = {
  hooks: {
    draw: (u) => {
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
    height: 360,
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
    const when = new Date(a.ts * 1000).toLocaleString();
    const tg = a.telegram_ok ? "" : '<span class="tg-fail" title="Telegram delivery failed">TG ✗</span>';
    tr.innerHTML =
      `<td>${when}</td>` +
      `<td>${escapeHTML(a.machine_name)}</td>` +
      `<td>${EVENT_LABEL[a.type] || a.type}${tg}</td>` +
      `<td class="temp">${a.temp_c == null ? "—" : a.temp_c.toFixed(1) + "°"}</td>`;
    tb.appendChild(tr);
  }
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

async function refresh() {
  try {
    const [machines, history, alerts] = await Promise.all([
      getJSON("/api/machines"),
      getJSON("/api/history?ids=all"),
      getJSON("/api/alerts?limit=50"),
    ]);
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
function agentSnippet(id) {
  const base = location.origin;
  return [
    `SERVER_URL="${base}"`,
    `MACHINE_ID="${id}"`,
    "",
    "# cron (every minute):",
    "* * * * * /opt/how-hot-is-it/agent.sh",
  ].join("\n");
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
      $("#enroll-snippet").textContent = agentSnippet(m.id);
      form.hidden = true;
      result.hidden = false;
      refresh();
    } catch (err) {
      alert("Failed to create machine: " + err);
    }
  });

  $("#copy-btn").addEventListener("click", () => {
    navigator.clipboard.writeText($("#enroll-snippet").textContent);
    $("#copy-btn").textContent = "Copied!";
    setTimeout(() => ($("#copy-btn").textContent = "Copy"), 1500);
  });
}

async function deleteMachine(id, name) {
  if (!confirm(`Delete "${name}"? Its readings will be removed; past alerts stay in history.`)) return;
  await fetch("/api/machines/" + id, { method: "DELETE" });
  refresh();
}

async function init() {
  try {
    const cfg = await getJSON("/api/config");
    if (cfg.alert_threshold_c) threshold = cfg.alert_threshold_c;
  } catch (e) { /* keep default */ }
  setupDialog();
  await refresh();
  setInterval(refresh, 60000);
  window.addEventListener("resize", () => {
    if (chart) chart.setSize({ width: $("#chart").clientWidth, height: 360 });
  });
}

init();
