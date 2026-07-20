# how-hot-is-it

A lightweight CPU temperature watcher for low-resource servers in rooms without
full-time AC. A tiny POSIX shell agent runs on each host via cron and pushes the
max CPU temperature to a single static Go server, which stores 24 h of history in
SQLite, renders a live dashboard, and fires Telegram alerts on overheating,
recovery, and reporting outages.

- **Agent** — one POSIX shell script (`agent.sh`). Needs only `lm-sensors`,
  `curl`, and `awk`. Runs on the host (not in Docker — it needs host sensors).
- **Server** — one static Go binary (stdlib `net/http` + pure-Go
  `modernc.org/sqlite`, so `CGO_ENABLED=0` builds and ships `FROM scratch`).
- **Frontend** — vanilla JS + [uPlot](https://github.com/leeoniya/uPlot)
  (vendored, embedded via `go:embed`). No npm, no bundler, no build step.

## Quick start (server)

```sh
cp config.example.json data/config.json   # edit as needed
docker compose up -d
```

Open http://localhost:8080. The `./data` bind mount holds `config.json` and
`howhot.db`. The container clock equals the host clock; all timestamps are
server-assigned (agents drift).

### Run without Docker

```sh
make run          # builds and runs with ./config.json and ./howhot.db
```

## Configuration — `config.json`

```json
{
  "telegram_bot_token": "123456:ABC...",
  "telegram_chat_id": "-100123456789",
  "alert_threshold_c": 80,
  "listen_port": 8080
}
```

- An empty `telegram_bot_token`/`telegram_chat_id` **disables Telegram delivery**;
  everything else (dashboard, history, alert records) still works. Every alert
  transition is written to the history table regardless — Telegram is only the
  delivery channel.
- `telegram_api_base` (or the `TELEGRAM_API_BASE` env var) overrides the Telegram
  API base URL; used by the tests to point at a fake endpoint.
- Restart the server to apply config changes.

## Enrolling a machine

1. On the dashboard click **Add machine**, enter a name. The server returns a
   generated 8-char ID and shows a paste-ready snippet.
2. Copy the `SERVER_URL` and `MACHINE_ID` lines into `agent.sh` on the host.
3. Install the agent and add the cron entry:

   ```sh
   sudo install -D agent.sh /opt/how-hot-is-it/agent.sh
   # edit SERVER_URL and MACHINE_ID at the top of the script
   ( crontab -l 2>/dev/null; echo '* * * * * /opt/how-hot-is-it/agent.sh' ) | crontab -
   ```

4. Within a minute the machine flips from grey to live on the dashboard.

Unknown machine IDs posting to `/api/report` get a `404` and are discarded —
there is no auto-enrollment. Deleting a machine removes its readings and alert
state but keeps its past alerts in history (under the name it had).

## API

| Method & path | Purpose |
| --- | --- |
| `POST /api/report` | Agent pushes `{"machine_id","temp_c"}` |
| `GET /api/machines` | List machines with latest temp |
| `POST /api/machines` | Create a machine `{"name"}` |
| `DELETE /api/machines/{id}` | Delete a machine (keeps its alert history) |
| `GET /api/history?ids=all` | 24 h of readings, uPlot columnar format with `null` gaps |
| `GET /api/alerts?limit=50` | Alert history, newest first (`limit` max 500) |

## Alerting

Evaluated every 60 s against the latest reading per machine:

- **Breach** — temp ≥ threshold: one 🔥 message, then re-notified at most every
  30 min while it stays over.
- **Recovery** — temp drops below `threshold − 3 °C` (hysteresis prevents
  flapping): one ✅ message.
- **Stale** — a machine that reported before but has been silent for 10 min: one
  ⚠️ message; it clears with a 📡 message when it reports again.

Readings older than 24 h are pruned each tick; the alert history is capped at the
most recent 500 rows.

## Development & tests

Everything meaningful is testable headless — no sensors hardware, no Telegram
network, no browser.

```sh
make test     # go vet + go test ./... + agent shell tests
make build    # local binary
make docker   # build the scratch image
```

- The alert state machine is a pure function (`alert.go`) with an injected clock —
  no `time.Now()` and no `time.Sleep` in tests.
- The Telegram sender uses an overridable base URL so tests point it at an
  `httptest.Server`.
- SQLite tests use a temp-file DB per test.
- `agent.sh`'s `parse_temp` reads `sensors -j` from stdin so fixtures
  (`testdata/sensors-*.json`) can be piped in; `test-agent.sh` runs these and
  `shellcheck` if it is installed.

## Manual checks (not automated)

These need real hardware, network, or a browser and are verified by hand:

1. `agent.sh` on real Intel (coretemp) and AMD (k10temp) hosts.
2. A real Telegram message delivered **from inside the scratch container** (CA
   certs are copied into the image for this).
3. Two machines rendering as two series in one chart with a working legend toggle
   and threshold line.
4. Killing the network mid-POST leaves no hung cron processes (`curl -m 5`).
