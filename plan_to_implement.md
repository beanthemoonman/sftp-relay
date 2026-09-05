# Plan to Implement — sftp-relay

Ten phases, each independently runnable and verifiable. Do not start a phase before
its predecessor's exit criteria pass — several later phases are hard to debug if the
foundations are shaky.

---

## Phase 0 — Skeleton and build pipeline — **DONE (2026-09-04)**

**Goal:** an empty container that runs on the Pi and answers a health check.

- [x] `go mod init`, package layout per CLAUDE.md
- [x] `cmd/relay/main.go`: chi router, `/api/health`, graceful shutdown on SIGTERM
- [x] `log/slog` JSON logging, level from `LOG_LEVEL`
- [x] Config loader: `.env` for `AUTH_USER`, `AUTH_PASS`, `DB_PATH`, `NAS_SSH_KEY_PATH`,
  `LOG_LEVEL`, `PORT`
- [x] Multi-stage Dockerfile targeting `linux/arm64`, Alpine, non-root user (uid 1000,
  `cap_net_bind_service` on nginx so it can bind `:80` unprivileged)
- [x] nginx in-container: listen `:80`, `proxy_pass` to `127.0.0.1:8080`, `proxy_buffering off`
- [x] Entrypoint script: start nginx, `exec` the Go binary as PID 1's child
- [x] `docker-compose.yml` with volumes for `/data` and the read-only NAS key

**Exit:** `curl http://pi/api/health` returns 200 from the container running on the Pi.

**Exit test result:** passed locally, not yet on the Pi. Image built with
`--build-arg TARGETARCH=amd64` and run as a container; `GET /api/health` through nginx
returned `200 {"status":"ok"}`, container runs as `uid=1000(relay)`, `SIGTERM` produced a
clean `shutting down` log line. `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...`
succeeds, so the arm64 binary is proven; **running the arm64 image on the real Pi is
still outstanding** and is what formally closes this phase.

---

## Phase 1 — Persistence — **DONE (2026-09-04)**

**Goal:** SQLite schema, migrations, settings.

- [x] `modernc.org/sqlite`, WAL mode, busy timeout (5s), foreign keys on, single writer conn
- [x] Numbered `.sql` migrations applied at startup, tracked in a `schema_migrations` table
- [x] Tables: `servers`, `jobs`, `job_log`, `settings`
- [x] Settings seeded with defaults: concurrency 2, segments 4, history retention 90 days,
  allowed destination roots empty (must be configured before any job can run)
- [x] Repository layer with context-taking methods; explicit `JOIN` syntax throughout

**Exit:** container starts on an empty volume, creates the DB, and restarts idempotently.

**Exit test result:** passed. Fresh volume → `app.db`, `-wal`, `-shm` created and health
200; `docker restart` → health 200 again with no migration re-run. Unit tests assert
`journal_mode=wal`, seeded defaults, a settings value surviving reopen with
`schema_migrations` still at one row, cascade delete, job-log ring buffering, and 20
concurrent writers. `go test -race ./...` clean in a linux container; coverage
`internal/config` 91.2%, `internal/db` 83.5%.

---

## Phase 2 — Auth — **DONE (2026-09-04)**

**Goal:** nothing is reachable unauthenticated.

- [x] Basic auth middleware over all `/api/*` including `/api/events`
- [x] `subtle.ConstantTimeCompare` for both fields (each field hashed with SHA-256
  first, so the comparison is length-independent); blank `AUTH_USER`/`AUTH_PASS`
  authorise nothing
- [x] Exempt `/api/health` so container healthchecks don't need credentials
- [x] Static React assets sit behind auth too — the middleware guards a chi group and
  the static handler joins that group in Phase 9

**Exit:** unauthenticated requests get 401 with `WWW-Authenticate`; SSE works once
authenticated (browsers send basic auth on `EventSource` requests automatically).

**Exit test result:** passed. In the running container, `GET /api/servers` with no
credentials returns 401 with `Www-Authenticate: Basic realm="sftp-relay",
charset="UTF-8"`; wrong credentials also 401; correct credentials 200; `/api/health`
returns 200 unauthenticated. `httptest` covers all ten authenticated routes for the
missing-credential case and six wrong-credential shapes. `/api/events` does not exist
yet, so the SSE half of this criterion is verified in Phase 8, not here.

---

## Phase 3 — Server CRUD and remote browsing — **DONE (2026-09-04)**

**Goal:** define SFTP servers and walk their directory trees from the container.

- [x] CRUD handlers for `servers`; credentials stored plaintext per the agreed posture.
  They are write-only on the wire: responses carry `has_password` / `has_private_key`
  flags, and a blank credential field on `PUT` keeps the stored value.
- [x] `internal/sftpclient`: dial with `x/crypto/ssh`, wrap in `pkg/sftp`
- [x] Host key handling: capture on first connect, store, verify thereafter;
  `ErrHostKeyChanged` surfaces as a 502 carrying remediation text
- [x] Connection pool keyed by server id, idle eviction after 2 minutes
- [x] `GET /api/servers/{id}/browse?path=` returns name, size, mode, modtime, is_dir;
  sorted directories-first; paths cleaned and forced absolute before use
- [x] `POST /api/servers/{id}/test` — fresh connection, list the default path, report
  timing; a failure comes back as `ok:false` rather than an HTTP error
- [x] Size pre-computation helper: stat for files, walk for directories. The walk is
  **sequential**, not the concurrent bounded walk originally sketched — it is metadata
  only and the remote round-trip dominates. Marked with a `ponytail:` comment naming
  the upgrade path.

**Exit:** browse a real remote server end to end via curl, including a deep directory.

**Exit test result:** passed against an in-process SSH+SFTP server, **not yet against a
real remote server**. The unit tests run a genuine SSH handshake and SFTP protocol
exchange (`sftp.InMemHandler`) and assert directories-first ordering, sizes including a
zero-byte file, recursive size totals, host-key capture, host-key-change rejection,
connection reuse, idle eviction, transparent redial, wrong password, unparseable key
and connection refused. Server CRUD was exercised through the running container with
curl. Browsing a real remote SFTP server over the LAN is outstanding and belongs to the
manual verification section of the definition of done.

---

## Phase 4 — NAS connection and destination browsing — **DONE (2026-09-04)**

**Goal:** browse the NAS and validate destinations.

- [x] `internal/nas`: SSH to the NAS using the mounted key, SFTP subsystem on top; the
  host key is captured on first connect into the `nas_host_key` setting and pinned after
- [x] Long-lived connection with a 30s keepalive and automatic reconnect on a dead session
- [x] `GET /api/nas/browse?path=` — directories only, plus free/total space via
  `StatVFS` where the NAS supports it. An empty `path` lists the allowed roots, which is
  where the destination picker starts.
- [x] `POST /api/nas/mkdir`
- [x] **Path validator** (`nas.CheckPath`): NFC-normalise, require absolute, clean,
  check containment, resolve symlinks on the NAS, check containment again. Fails closed
  when no roots are configured. For a path that does not exist yet (mkdir) it resolves
  the nearest existing ancestor instead.
- [x] Startup probe: resolves the absolute path of `lftp` on the NAS (PATH first, then
  the usual Entware and Synology locations), caches it in the `lftp_path` setting, and
  logs an error carrying `opkg install lftp` remediation if it is absent. It runs in the
  background so an offline or unconfigured NAS cannot stop the UI coming up.

**Exit:** browse NAS folders, create one, and get a clean rejection for `../../etc`.

**Exit test result:** passed against a fake NAS (in-process SSH, in-memory SFTP and a
canned exec handler), **not yet against the real Synology**. Tests cover root listing,
directories-only browsing, parent breadcrumbs, `mkdir` including nested creation,
rejection of `/volume1/media/../../etc/evil`, host-key capture and change rejection,
reconnect after a dropped session, the lftp probe both finding and not finding lftp,
missing and malformed key files, and a closed client refusing work. The validator is
table-driven over traversal, `..` escapes, prefix-but-not-child (`/volume1/mediafoo`
against an allowed `/volume1/media`), symlinks pointing inside a root, across roots and
outside, unicode NFD to NFC, trailing slashes, duplicate separators, the empty string,
null bytes, relative paths, the filesystem root and an empty allow-list. `CheckPath`,
`normalise` and `contained` sit at 100% statement coverage. Through the container,
`GET /api/nas/browse` with no roots configured returns
`400 {"error":"nas: no allowed destination roots are configured"}`.

---

## Phase 5 — lftp execution (the risky part — spike it first)

**Goal:** one hardcoded job transfers real bytes onto the NAS.

- Ephemeral workspace: SFTP a `0600` key file and a POSIX-sh lftp script into
  `$NAS_TMP/job-<id>/`
- Generated script writes its PID to a pidfile and uses `trap` to remove the workspace
  on any exit path
- One-shot `ssh` exec per job; combined stdout/stderr streamed back
- Command shapes for file (`pget -n -c`) and directory (`mirror --continue --parallel`)
- Exit code propagated; stderr captured into `job_log`

**Exit:** a file and a directory both land on the NAS with correct sizes, and the
temp workspace is gone afterwards — including after a forced kill.

---

## Phase 6 — Progress parsing

**Goal:** trustworthy percentage, speed and ETA.

- Line scanner over the streamed output; regexes for lftp's transfer status lines
- Reconcile against the pre-computed total from Phase 3
- Fallback: if no parseable progress for ~10 s, poll destination size over SFTP every
  5 s and derive progress from that
- Coalesce to at most one update per job per second; keep in memory, flush to DB on
  the same cadence
- Parser unit tests against captured output samples from at least two lftp versions

**Exit:** progress advances monotonically and lands on 100% for both job kinds.

---

## Phase 7 — Queue and lifecycle

**Goal:** a real job system.

- Worker pool sized from settings, resizable at runtime without restart
- State machine: `queued → running → done|failed|cancelled`, plus `interrupted`
- `POST /api/jobs` accepts a batch (one server, many items, one destination); expands
  into individual job rows
- Cancel: fresh SSH exec sending `TERM` to the recorded PID, then cleanup
- Retry: clones the job row, relies on `--continue`/`-c` to resume
- Startup recovery: `running` → `interrupted` → requeued
- History retention sweep on a ticker

**Exit:** queue five jobs, cancel one mid-flight, restart the container, and watch the
interrupted job resume rather than restart.

---

## Phase 8 — SSE

**Goal:** the UI learns about changes without polling.

- `internal/events` hub: subscribe/unsubscribe, per-client bounded buffer, slow
  clients dropped rather than allowed to grow memory
- `/api/events` sends a full snapshot on connect, then deltas
- Event types per CLAUDE.md; heartbeat comment every 20 s to defeat idle timeouts
- Verify `proxy_buffering off` and `X-Accel-Buffering: no` actually work through nginx

**Exit:** two browser tabs plus a phone all see the same live progress; killing the
Wi-Fi and reconnecting resyncs cleanly.

---

## Phase 9 — Frontend

**Goal:** the app people actually touch.

- Vite + React + TS + Tailwind; TanStack Query for server state
- Routes: Browse, Queue, History, Servers, Settings
- Browse: server picker → remote pane → destination picker → confirm. Split-pane on
  desktop, sequential steps on mobile.
- Multi-select with a persistent selection bar showing count and total size
- Queue: live cards with progress, speed, ETA, cancel
- Servers: CRUD forms with a Test Connection action and inline result
- Settings: NAS config, allowed roots, concurrency, segments, retention
- `EventSource` hook feeding the query cache; automatic reconnect with backoff
- Build output embedded via `embed.FS` and served by Go

**Exit:** full flow completed from a phone with no console errors.

---

## Phase 10 — Hardening

- Credential redaction verified in every log path
- `GOMEMLIMIT` / `GOGC=50` tuned; RSS measured under a 5-job load
- Healthcheck in compose; restart policy `unless-stopped`
- README: NAS prerequisites (lftp install, SSH key setup, allowed roots), backup of
  `/data/app.db`
- Error surfaces reviewed: every failure the user can cause should produce a message
  that says what to do next

---

## Risk register

| Risk | Mitigation |
|---|---|
| lftp missing or off-`PATH` on Synology | Startup probe with an explicit remediation message |
| lftp output format varies by version | Size-polling fallback; parser tested against multiple versions |
| Orphaned lftp processes on the NAS | pidfile + `trap` cleanup + a startup sweep of stale workspaces |
| Plaintext credentials in SQLite | Accepted; document it in the README so future-you isn't surprised |
| SSE broken by nginx buffering | Explicitly configured and verified in Phase 8 |
| Pure-Go SQLite write contention | Progress held in memory, flushed at most 1 Hz |
| arm64 cross-compilation | No CGO anywhere; enforced by `CGO_ENABLED=0` in the build |

## Suggested order of attack

Phase 5 is the only phase with real unknowns. Consider spiking it as a throwaway
script immediately after Phase 0 — confirm you can drive lftp on your NAS over SSH
and get usable output back — before investing in Phases 1–4.
