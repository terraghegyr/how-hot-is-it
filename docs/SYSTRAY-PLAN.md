# how-hot-is-it-tray — macOS Menu Bar App Plan

Companion to the how-hot-is-it server: a tiny Go menu bar app that shows current CPU temps at a glance, with native macOS notifications on threshold breach. No webview, no window — the existing browser dashboard remains the place for graphs.

## Guiding constraints

- **One small Go module**, separate repo/dir from the server (or `tray/` subdir in the same repo — pick same-repo `tray/` for shared context).
- **Read-only client.** The tray app only calls `GET /api/machines` and (optionally) `GET /api/alerts`. Machine add/delete stays in the web dashboard — no config UI, no write endpoints from the tray.
- **Library: `fyne.io/systray`** (maintained fork of getlantern/systray). No Fyne GUI toolkit, just the systray package.
- **Config: env vars with defaults**, matching the project's hardcode-friendly philosophy: `HOWHOT_URL` (default `http://localhost:8080`), `HOWHOT_POLL_SECONDS` (default 60). No config file.
- **Notifications via `osascript -e 'display notification ...'`** — zero extra dependencies, native macOS banners. (Caveat below.)
- No login-item magic in code; autostart is a LaunchAgent plist (template provided, user installs manually).

## ⚠️ Build/dev environment reality (Claude Code Cloud is Linux)

`fyne.io/systray` on macOS requires **cgo (Cocoa)** — a darwin binary **cannot be cross-compiled from the Linux cloud sandbox** without osxcross gymnastics. Structure the work accordingly:

- Claude Code Cloud: writes all code, runs unit tests for the platform-independent logic on Linux, ensures `go vet` clean.
- **Final `go build` happens on the user's Mac** (`make build` locally, requires Xcode CLT: `xcode-select --install`). Document this as step 1 of the README.
- To keep `go test ./...` green on Linux: put all systray/osascript calls in files with `//go:build darwin` tags behind a small interface (`type UI interface { SetTitle(string); Notify(title, body string); ... }`); logic packages depend on the interface only. A `//go:build !darwin` stub satisfies compilation for tests.

## Behavior spec

**Menu bar title** (the whole point):
- Shows the hottest machine: `62°` (integer, degree sign, no "C" — menu bar space is precious).
- Any machine ≥ threshold → prefix flame: `🔥 84°`.
- Any machine stale (per server's live/stale flag) and none breaching → `⚠️ 62°`.
- Server unreachable → title `--` (and one notification, see below).

**Dropdown menu:**
```
web-01      61°
nas         74°
attic-box   ⚠️ stale
──────────────
Open dashboard        → open <HOWHOT_URL> in default browser (`open` command)
Pause notifications ✓ → toggle, in-memory only
──────────────
Quit
```
Machine rows are display-only (disabled menu items). Rebuild menu items on each poll; systray supports updating item titles in place — prefer updating over recreate to avoid flicker.

**Poll loop:** every `HOWHOT_POLL_SECONDS`, `GET /api/machines`, 5 s HTTP timeout. Failures don't crash; show `--` and retry next tick.

**Client-side notification edge detection** (don't rely on Telegram — this is the local channel):
- Per machine, track previous state (ok / breach / stale / unknown) in memory.
- ok→breach: notify "🔥 {name}: {temp}°C". breach→ok: "✅ {name} back to {temp}°C".
- live→stale: "⚠️ {name} stopped reporting". stale→live: no notification (title change is enough).
- reachable→unreachable server: one notification; no repeat until it recovers.
- No re-notification loop needed client-side (server/Telegram handles nagging); tray notifies on transitions only.
- "Pause notifications" suppresses all notify calls but title/menu keep updating.
- On startup, first poll establishes baseline state WITHOUT notifications (avoid a notification storm when launching while something is already hot).

**Threshold source:** the server currently doesn't expose `alert_threshold_c`. **Required tiny server change:** include `"alert_threshold_c": N` in the `GET /api/machines` response envelope (one line; version-bump the server). The tray app must not hardcode its own threshold — single source of truth. Tray also reads per-machine live/stale from this response (server already computes staleness; don't reimplement 10-min logic client-side).

## Repo layout (inside the existing repo)

```
tray/
├── main.go            # wiring: systray lifecycle, poll loop
├── client.go          # HTTP client for /api/machines (pure, testable)
├── state.go           # transition/edge-detection state machine (pure, testable)
├── format.go          # title/menu string formatting (pure, testable)
├── ui_darwin.go       # //go:build darwin — systray + osascript impl
├── ui_stub.go         # //go:build !darwin — no-op impl for Linux tests
├── testdata/          # canned /api/machines JSON fixtures
├── com.howhotisit.tray.plist   # LaunchAgent template
└── Makefile           # build (darwin, CGO_ENABLED=1), test, install-agent
```

## macOS specifics / footguns (encode in README + code comments)

1. **Dock icon:** a plain binary launched via LaunchAgent won't show a Dock icon as long as no window is created — systray-only is fine. If a Dock icon ever appears, the fix is wrapping into a minimal `.app` bundle with `LSUIElement=true`; **do not** build bundle machinery preemptively. Ship the plain binary first.
2. **osascript notification limits:** banners come from "Script Editor" identity, are non-clickable, and macOS may require the user to allow notifications for Script Editor in System Settings → Notifications on first run. Document this. If it proves too janky in practice, the fallback is `terminal-notifier` (brew) — note as an option, don't implement.
3. **LaunchAgent:** template plist with `RunAtLoad=true`, `KeepAlive=false` (systray apps shouldn't respawn-loop if the user quits), `EnvironmentVariables` block for `HOWHOT_URL`. `make install-agent` copies to `~/Library/LaunchAgents/` and runs `launchctl load`.
4. **Menu bar title width:** keep title ≤ ~6 chars; never put machine names in the title.
5. `fyne.io/systray` must run on the main thread via its `systray.Run(onReady, onExit)` — the poll loop goes in a goroutine started from `onReady`. Getting this backwards is the classic systray bug.

## Testing (runs on Linux in Claude Code Cloud)

1. **state.go**: table-driven tests for every transition pair incl. startup-baseline suppression, pause behavior, unreachable dedupe.
2. **format.go**: hottest-machine selection (ties, single machine, all stale, empty list), flame/warning prefix rules, `--` on unreachable.
3. **client.go**: `httptest.Server` with fixtures; malformed JSON → error not panic; timeout respected; threshold field parsed.
4. Build gates: `go vet ./...` and `go test ./...` pass on Linux (stub UI); `GOOS=darwin go vet` for the darwin files is nice-to-have but cgo makes it unreliable cross-platform — acceptable to skip.
5. **Manual (on the Mac):** binary builds with Xcode CLT; title updates live; notification permission flow; LaunchAgent starts it at login; Quit actually exits.

## Out of scope

- Windows/Linux tray support (build tags leave the door open; don't implement)
- Machine add/delete from the tray
- Graphs/sparklines in the dropdown
- .app bundle, code signing, notarization, Sparkle updates
- Config file or preferences window

## Acceptance checks

1. `make test` green in Claude Code Cloud (Linux).
2. On the Mac: `make build` produces a binary; launching it shows `62°`-style title within one poll.
3. Heating a machine past threshold (or lowering server threshold temporarily) → 🔥 prefix + exactly one banner; recovery → ✅ banner once.
4. Killing the server → `--` + one banner; restarting server → recovers silently.
5. Launching while a machine is already over threshold → 🔥 title, **zero** notifications.
6. "Open dashboard" opens the browser; "Pause notifications" silences banners while title keeps updating; Quit exits cleanly with no orphan process.
7. LaunchAgent template works: present at login after `make install-agent`.
