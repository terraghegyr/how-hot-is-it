# HOWHOTISIT

A lightweight CPU temperature watcher. A tiny shell script agent runs on each host via cron and pushes the
max CPU temperature to a single static Go server, which stores 24 h of history in
SQLite, renders a live dashboard, and fires Telegram alerts on overheating,
recovery, and reporting outages.

> (I'm running Proxmox on second-hand old office desktop. Grafana is great but I only watch temperature and for that, it is too much both capability wise and resource usage wise. So I made this.)

> (Entirely Claude coded. Throughly but only tested in my environement: Proxmox 8.4.19, Intel Core i7-8700 CPU)

> This is very simple system especially the agent side since it is only just a bash script, but use at your own risk.

- **Agent** — one shell script (`agent.sh`). Needs only `lm-sensors`,
  `curl`, and `awk`. Runs on the host (not in Docker — it needs host sensors).
- **Server** — one static Go binary (stdlib `net/http` + pure-Go
  `modernc.org/sqlite`, so `CGO_ENABLED=0` builds and ships `FROM scratch`).
- **Frontend** — vanilla JS + [uPlot](https://github.com/leeoniya/uPlot)
  (vendored, embedded via `go:embed`). No npm, no bundler, no build step.

## Quick start (server)

Start from the template (compose file is git-ignored):

```sh
cp docker-compose.yml.example docker-compose.yml   # then adapt to your host
mkdir -p data
cp server/config.example.json data/config.json     # edit as needed
docker compose up -d
```

Open http://localhost:8080 (default). The `./data` bind mount holds `config.json` and
`howhot.db`. The container clock equals the host clock; all timestamps are
server-assigned (agents drift).

### Behind a reverse proxy (Traefik, Caddy, nginx, …)

> I'm running this behind Traefik but other reverse proxy stuff should work no matter.

Adapt your local `docker-compose.yml`: usually drop the published `ports:`,
attach the service to the proxy's network, and add the proxy's labels/route to
container port `8080`. No app config is needed — the dashboard builds each
agent's `SERVER_URL` from the URL you browse it at (`location.origin`), so a
proxy hostname like `https://howhot.example.com` is what the Add-machine popup
shows. Just make sure your agent hosts can actually reach that URL.

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
   generated 8-char ID and shows two paste-ready blocks (env values + cron).
2. Copy the agent onto the host and make it executable:

   ```sh
   sudo mkdir -p /opt/how-hot-is-it
   sudo cp agent.sh /opt/how-hot-is-it/agent.sh
   sudo chmod +x /opt/how-hot-is-it/agent.sh
   ```

3. Paste the `SERVER_URL` / `MACHINE_ID` block into the config section at the top
   of `/opt/how-hot-is-it/agent.sh`.
4. Add the cron entries. Cron's finest granularity is one minute, so reporting
   every 30 s uses two entries — one on the minute and one offset by 30 s:

   ```sh
   ( crontab -l 2>/dev/null
     echo '* * * * * /opt/how-hot-is-it/agent.sh'
     echo '* * * * * sleep 30; /opt/how-hot-is-it/agent.sh'
   ) | crontab -
   ```

5. Within a minute the machine flips from grey to live on the dashboard.

Unknown machine IDs posting to `/api/report` get a `404` and are discarded —
there is no auto-enrollment. Deleting a machine removes its readings and alert
state but keeps its past alerts in history (under the name it had).

Needless to say, if you want to "uninstall" this. Just delete CRON entry and then delete script.

### Lastly...

Since this just stores temperature data, web has no auth. Any configuration will be done via config file. 
But if you are security-paranoid, feel free to sit this behind reverse proxy with auth (like Traefik & Authentik).

---

# Dev stuff

## Layout

```
server/     Go module: the binary, embedded web/ dashboard, Dockerfile
            (Go *_test.go files live here beside the code they test —
             Go requires tests in the same package)
agent.sh    host-side shell agent
test/       agent shell tests + sensor fixtures (test-agent.sh, testdata/)
docker-compose.yml.example, Makefile   build/run orchestration (root)
```

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

The alert state machine is evaluated **on every reading as it is ingested** (so a
short spike between ticks is never missed) and also on a 60 s maintenance tick
(which drives the time-based transitions and cleanup below):

- **Breach** — temp ≥ threshold: one 🔥 message, then re-notified at most every
  30 min while it stays over.
- **Recovery** — temp drops below `threshold − 3 °C` (hysteresis prevents
  flapping): one ✅ message.
- **Stale** — a machine that reported before but has been silent for 10 min: one
  ⚠️ message; it clears with a 📡 message when it reports again (this and the
  re-notify timer are what the 60 s tick handles — no reading arrives to trigger
  them).

Ingest-time evaluation runs in the background so a slow Telegram send never blocks
the agent's POST. Readings older than 24 h are pruned each tick; the alert history
is capped at the most recent 500 rows. The chart buckets readings to 60 s and
shows the **max** temp per bucket, so a spike that tripped an alert is always
visible, and the y-axis always includes the threshold line.

## Agent

As below code snipped shows, you only need to post Machine ID and Temperature to /api/report endpoint every 30 secs so if you want to use other sensors (Linux, Mac, Windows whatever), you can just create custom scripts. If you feel like it, please push your script to this repository!

```bash
	curl -fsS -m 5 -X POST "$SERVER_URL/api/report" \
		-H 'Content-Type: application/json' \
		-d "{\"machine_id\":\"$MACHINE_ID\",\"temp_c\":$TEMP}" >/dev/null 2>&1
```

## Dashboard refresh

The dashboard **polls** `GET /api/config`, `/api/machines`, `/api/history`, and
`/api/alerts` every 30 s (matching the agent report cadence) and redraws — the
alert **threshold line and colour coding come from `alert_threshold_c` in the
config on every poll**, nothing is hardcoded in the page. Polling was chosen over
a push channel (SSE/WebSockets) on purpose: push would add long-lived
connections and a broadcast hub on the server for a dashboard where 30 s
freshness is plenty. If you ever want true real-time, an SSE endpoint is the
smallest add — but it isn't needed for this workload.

## Development & tests

Everything meaningful is testable headless — no sensors hardware, no Telegram
network, no browser.

```sh
make test     # go vet + go test ./... + agent shell tests
make build    # local binary
make docker   # build the scratch image
```

- The alert state machine is a pure function (`server/alert.go`) with an injected clock —
  no `time.Now()` and no `time.Sleep` in tests.
- The Telegram sender uses an overridable base URL so tests point it at an
  `httptest.Server`.
- SQLite tests use a temp-file DB per test.
- `agent.sh`'s `parse_temp` reads `sensors -j` from stdin so fixtures
  (`test/testdata/sensors-*.json`) can be piped in; `test/test-agent.sh` runs
  these and `shellcheck` if it is installed.
- Go `*_test.go` files stay under `server/` alongside the code — Go requires
  tests in the same package to reach unexported identifiers. Only the agent's
  shell tests and fixtures live under `test/`.
