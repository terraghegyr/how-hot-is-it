# how-hot-is-it — Implementation Plan (v3: Go + Docker + alert history + tests)

Lightweight CPU temperature watcher for low-resource servers in rooms without full-time AC. Shell-script agent + single static Go binary server, deployed in Docker.

## Guiding constraints (do not violate)

- **No over-engineering.** No ORM, no migrations framework, no frontend framework/build step, no auth, no TLS handling (reverse proxy's job if ever needed).
- **Agent = one POSIX shell script.** Dependencies: `lm-sensors`, `curl`, `awk`/`grep`. Nothing else. Runs via cron on the host (NOT in Docker — it needs host sensors).
- **Server = one static Go binary.** Stdlib `net/http` + exactly two dependencies:
  - `modernc.org/sqlite` — pure-Go SQLite. **Do NOT use `mattn/go-sqlite3`**: it requires CGO, which breaks the trivially-static `CGO_ENABLED=0` build and forces musl/glibc gymnastics in Docker.
  - uPlot (frontend, vendored — see Frontend section).
- **Frontend = vanilla JS + uPlot, embedded via `go:embed`.** No Svelte, no npm, no bundler. Rationale recorded below so nobody "helpfully" adds a build step.
- **Go version: 1.26** (latest stable; Go has no LTS track — support = two newest majors, so pin `go 1.26` in `go.mod` and `golang:1.26-alpine` in the Dockerfile).
- **Retention fixed at 24 hours** for readings. Hardcoded.
- **Server config = exactly one JSON file** (`config.json`, bind-mounted into the container).
- **Agent config = env vars hardcoded at the top of the script.**

## Frontend decision record (why not Svelte)

The UI is three things: a multi-series line chart, an enrollment modal, and a machine list with add/delete. That is well within vanilla JS territory (~200–300 lines). Svelte would work and its output is static, but it adds node_modules, a bundler, a build stage in Docker, and version churn — permanent maintenance cost for a UI with ~3 interactive elements. The one piece genuinely painful to hand-roll is a good multi-series time chart with tooltips/legend, so that piece gets a library:

- **uPlot** (~50 KB min, zero deps, purpose-built for time series, handles multi-series + legend + hover cursor natively). Vendor the two files (`uPlot.iife.min.js`, `uPlot.min.css`) into `web/vendor/` and serve via `go:embed` — works offline, no CDN.
- Everything else (modal, list, fetch polling) = vanilla JS in one `app.js`.

If requirements ever grow past ~5 views, revisit; until then a framework is dead weight.

## Architecture

```
[agent.sh via host cron, every 1 min]
    sensors -j → parse max CPU temp → curl POST
        ↓
[how-hot-is-it container :8080]  (Go binary, scratch image)
    ├── POST   /api/report                ← agent pushes {machine_id, temp_c}
    ├── GET    /api/machines              ← list machines + latest temp + live/stale
    ├── POST   /api/machines              ← create machine (enrollment) {name}
    ├── DELETE /api/machines/{id}         ← delete machine + its readings + alert state
    ├── GET    /api/history?ids=a,b,c     ← last 24h for one or more machines
    ├── GET    /api/alerts?limit=50       ← alert history, newest first
    ├── GET    /                          ← embedded SPA-ish single page
    └── goroutine: prune >24h + stale detection + alert evaluation
        ↓ (threshold breach / recovery / stale)
[Telegram Bot API sendMessage]
```

## Repo layout

```
how-hot-is-it/
├── agent.sh
├── main.go                  # or split: main.go, store.go, alert.go, api.go — max ~4 files
├── web/
│   ├── index.html
│   ├── app.js
│   ├── style.css
│   └── vendor/uPlot.iife.min.js, uPlot.min.css
├── go.mod / go.sum
├── config.example.json
├── Dockerfile
├── docker-compose.yml
└── README.md
```

## Docker

Multi-stage, final image FROM scratch (pure-Go build makes this possible):

```dockerfile
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /how-hot-is-it .

FROM scratch
COPY --from=build /how-hot-is-it /how-hot-is-it
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/how-hot-is-it", "-config", "/data/config.json", "-db", "/data/howhot.db"]
```

`docker-compose.yml`: one service, `./data:/data` bind mount (holds `config.json` + `howhot.db`), `restart: unless-stopped`. Note in README: container clock = host clock; timestamps are server-assigned.

Caveat to verify during implementation: `scratch` has no CA certs, and Telegram is HTTPS — copy `/etc/ssl/certs/ca-certificates.crt` from the build stage into the final image (`COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/`). This is a classic scratch-image footgun; the acceptance checks include a real Telegram send from inside the container.

## Data model (SQLite via modernc.org/sqlite, WAL mode)

```sql
CREATE TABLE machines (
  id TEXT PRIMARY KEY,          -- 8-char hex from crypto/rand
  name TEXT NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE readings (
  machine_id TEXT NOT NULL,
  ts INTEGER NOT NULL,          -- unix seconds, SERVER-assigned (agents drift)
  temp_c REAL NOT NULL
);
CREATE INDEX idx_readings ON readings(machine_id, ts);

CREATE TABLE alert_state (
  machine_id TEXT PRIMARY KEY,
  alerting INTEGER NOT NULL DEFAULT 0,
  last_notified INTEGER NOT NULL DEFAULT 0,
  stale_notified INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE alerts (            -- alert history (see retention note)
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  machine_id TEXT NOT NULL,
  machine_name TEXT NOT NULL,    -- denormalized so history survives machine deletion
  ts INTEGER NOT NULL,
  type TEXT NOT NULL,            -- 'breach' | 'recovery' | 'stale' | 'stale_recovery'
  temp_c REAL,                   -- NULL for stale events
  telegram_ok INTEGER NOT NULL   -- 1 if sendMessage succeeded (or alerting disabled), 0 on send failure
);
```

Every state-machine transition writes an `alerts` row **regardless of whether Telegram is configured** — the history is the source of truth, Telegram is just a delivery channel. Re-notifications (the 30-min repeats) do NOT create new rows; only transitions do.

**Alert history retention: count-capped at last 500 rows, not time-capped.** Deliberate deviation from the 24 h readings rule — alerts are sparse and "did it overheat last Tuesday?" is exactly what history is for; 500 rows is still a trivially small table. Prune in the same maintenance tick: `DELETE FROM alerts WHERE id NOT IN (SELECT id FROM alerts ORDER BY id DESC LIMIT 500)`.

Schema bootstrap: `CREATE TABLE IF NOT EXISTS` on startup. No migration tooling.

DELETE machine = transaction deleting from `machines`, `readings`, `alert_state`. **`alerts` rows are kept** (machine_name is denormalized for this reason) so history remains meaningful after a machine is retired.

## Server config — `config.json`

```json
{
  "telegram_bot_token": "123456:ABC...",
  "telegram_chat_id": "-100123456789",
  "alert_threshold_c": 80,
  "listen_port": 8080
}
```

Empty token → alerting disabled, everything else works. Restart to apply changes.

## Agent — `agent.sh`

Unchanged from v1:

```sh
#!/bin/sh
# ==== config (edit these) ====
SERVER_URL="http://192.168.1.10:8080"
MACHINE_ID="paste-id-from-ui"
# =============================

TEMP=$(sensors -j 2>/dev/null | <parse>)
[ -z "$TEMP" ] && exit 1
curl -fsS -m 5 -X POST "$SERVER_URL/api/report" \
  -H 'Content-Type: application/json' \
  -d "{\"machine_id\":\"$MACHINE_ID\",\"temp_c\":$TEMP}" >/dev/null
```

- Parse `sensors -j`, take **max** across `Package id 0` / `Tctl` / core inputs; must work unmodified on Intel (coretemp) and AMD (k10temp).
- Fallback: max of `/sys/class/thermal/thermal_zone*/temp` ÷ 1000.
- `curl -m 5`, fail-silent on network errors (drop sample; next minute retries naturally). Never hang cron.
- Cron line: `* * * * * /opt/how-hot-is-it/agent.sh`

## Alerting logic (goroutine, evaluates every 60 s)

1. Latest temp ≥ threshold AND `alerting=0` → "🔥 {name}: {temp}°C (threshold {t}°C)", set alerting=1.
2. Still over → re-notify at most every 30 min (hardcoded).
3. Below `threshold − 3 °C` (hardcoded hysteresis) → "✅ {name} back to {temp}°C", alerting=0.
4. **Stale**: machine with ≥1 historical reading but nothing in 10 min → one "⚠️ {name} stopped reporting"; `stale_notified` flag resets when it reports again. (An overheating box that dies is otherwise indistinguishable from a quiet healthy one.)

Telegram send = one `net/http` POST, 5 s timeout, failures logged and recorded in the `alerts` row (`telegram_ok=0`). **Telegram API base URL must be a variable defaulting to `https://api.telegram.org` and overridable (config field or env `TELEGRAM_API_BASE`)** — required for testing against a fake endpoint.

## Dashboard UI (embedded, vanilla JS + uPlot)

- **Main view**: one uPlot chart, 24 h window, **one series per machine**, shared time axis, legend with per-machine toggle (uPlot gives this for free), horizontal threshold reference line, hover tooltip showing all series values.
- **Machine list** beside/below chart: name, current temp color-coded (green / amber ≥ threshold−10 / red ≥ threshold / grey = stale), delete button with `confirm()` — native confirm is fine, no modal library.
- **Enrollment popup**: "Add machine" button → `<dialog>` element (native, no library) → name input → POST → response shows copy-paste block with the two `agent.sh` lines + cron line + "Copy" button. Machine appears grey until first report arrives.
- **Alert history panel** below the machine list: table of last 50 events from `GET /api/alerts?limit=50` — columns: time (local), machine name, event (🔥 breach / ✅ recovery / ⚠️ stale / 📡 reporting again), temp, and a small "TG ✗" marker when `telegram_ok=0` so failed deliveries are visible. Refreshes on the same 60 s tick. No pagination UI; `limit` param (max 500) exists for curl users.
- Data flow: `GET /api/machines` + `GET /api/history?ids=all` + `GET /api/alerts?limit=50` every 60 s via `setInterval`; redraw chart with `uplot.setData()`.
- `/api/history` returns columnar arrays (uPlot's native format: `[timestamps, series1, series2, ...]` aligned) to avoid client-side reshaping. Gaps (stale periods) = `null` values so the line breaks visibly instead of interpolating across an outage.

## Enrollment flow (must match exactly)

1. Dashboard → "Add machine" → name → server returns generated ID → popup shows paste-ready `SERVER_URL` / `MACHINE_ID` lines + cron line.
2. User pastes into `agent.sh`.
3. User adds cron entry.
4. First POST flips machine from grey to live.

Unknown `machine_id` on `/api/report` → 404, discarded. No auto-enroll.

## Retention

In the same 60 s goroutine tick: `DELETE FROM readings WHERE ts < now-86400`. 1,440 rows/machine/day — size is a non-issue; no VACUUM logic.

## Testing (developed entirely in Claude Code Cloud — design for headless, no hardware)

The dev environment has no lm-sensors hardware, no Telegram network access, and no browser. Structure the code so everything meaningful is testable with `go test ./...` and `sh` alone.

**Design-for-test requirements (bake into the code, not just the tests):**
- Inject a clock: alert/staleness/pruning logic takes a `now func() time.Time` (or equivalent), never calls `time.Now()` directly in business logic. Tests must never `time.Sleep`.
- Telegram sender behind a tiny interface (or just the overridable base URL above) so tests point it at an `httptest.Server` that records requests.
- Alert state machine implemented as a pure function/struct method: `(state, latestReading, now) → (newState, []event)`. This is the single most bug-prone part (hysteresis, 30-min re-notify, stale transitions) and must be testable without HTTP or SQLite.
- SQLite tests use a temp-file or `:memory:` DB per test — modernc.org/sqlite supports both, no external service.
- Agent script: parsing must be a function that reads `sensors -j` JSON from **stdin** (e.g. `parse_temp()` consuming `cat`), so tests can pipe fixture files instead of running `sensors`.

**Test suites:**
1. **Unit — alert state machine** (highest priority): breach fires once; sustained breach re-notifies at exactly 30 min, not before; recovery only below threshold−3; flapping around the threshold inside the hysteresis band produces zero extra events; stale after 10 min silence fires once; report after stale emits `stale_recovery`; every transition writes an `alerts` row, re-notifies don't.
2. **Unit — retention**: readings older than 24 h pruned, newer kept; alerts table capped at 500 by id; machine deletion removes readings/state but keeps alerts rows with the denormalized name.
3. **API — `httptest`**: full enrollment round-trip (POST machine → report → machines shows live); report with unknown ID → 404 and no row; DELETE cascade; `/api/history` returns uPlot columnar format with `null` gaps for a fabricated outage window; `/api/alerts` ordering (newest first) and `limit` clamping; malformed JSON body → 400.
4. **Integration — fake Telegram**: `httptest.Server` as Telegram base URL; drive readings through the real ingest path with a fake clock; assert exact message count for breach → sustained → recovery (3 messages over a simulated 90 min); fake server returning 500 → `telegram_ok=0` recorded and system keeps running.
5. **Agent — shell**: run `shellcheck agent.sh`; pipe fixture files (`testdata/sensors-intel.json` with coretemp/`Package id 0`, `testdata/sensors-amd.json` with k10temp/`Tctl`, plus a malformed/empty fixture) through the parse function and assert the extracted max temp; empty input → non-zero exit. Runner is a plain `test-agent.sh` invoked from CI/`go test` via `os/exec` or a Makefile target.
6. **Build gate**: `CGO_ENABLED=0 go build` must succeed (guards against accidentally importing a CGO dependency); `go vet ./...` clean.

`Makefile` (or `justfile`) targets: `test` (all of the above), `build`, `docker`. Claude Code Cloud runs `make test` as the acceptance loop.

**Not tested automatically** (document as manual checks in README): real `sensors` output on physical Intel/AMD hosts, real Telegram delivery, Docker scratch CA-cert behavior, visual chart rendering. These stay in the acceptance checklist below.



- Auth/API keys, HTTPS, rate limiting
- Frontend frameworks, npm, bundlers
- Multiple sensors per machine (max CPU temp only)
- Configurable retention, downsampling
- Windows/macOS agents
- Agent in Docker (needs host lm-sensors)
- Editing machine names (delete + re-add is fine)

## Acceptance checks

0. `make test` passes in the Claude Code Cloud sandbox (all suites above); `CGO_ENABLED=0` build and `go vet` clean.
1. `docker compose up` on a clean host starts the server; image is <20 MB.
2. `agent.sh` works unmodified on Intel (coretemp) and AMD (k10temp) hosts. *(manual)*
3. Telegram message successfully sends **from inside the scratch container** (CA certs check). *(manual)*
4. Two machines reporting → both series render in one chart with working legend toggle and threshold line. *(manual/visual)*
5. Threshold breach → exactly one message; sustained breach → ≤1 message/30 min; recovery → one message — and each transition appears in the alert history panel with correct type and temp.
6. Deleting a machine removes its series, readings, and alert state; its agent then gets 404s; its past alerts remain visible in history under its old name.
7. No readings older than ~24 h; alerts table never exceeds 500 rows.
8. Killing network mid-POST leaves no hung cron processes on the agent host. *(manual)*
9. Telegram send failure (fake 500) is marked "TG ✗" in the alert history and does not crash or block ingest.
