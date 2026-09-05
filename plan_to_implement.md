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

## Phase 5 — lftp execution — **DONE (2026-09-04)**

**Goal:** one hardcoded job transfers real bytes onto the NAS.

- [x] Ephemeral workspace: SFTP a `0600` key file and a POSIX-sh lftp script into
  `$NAS_TMP/job-<id>/`. The directory is `0700`; every file is chmodded *before* its
  content is written, so a credential is never briefly world-readable.
- [x] Generated script writes lftp's PID (not the shell's) to a pidfile and uses
  `trap cleanup EXIT INT TERM HUP` to remove the workspace on any ordinary exit path
- [x] One-shot `ssh` exec per job; combined stdout/stderr streamed back, split on
  `
` as well as `
` so lftp's in-place progress updates arrive as separate lines
- [x] Command shapes for file (`pget -n -c`) and directory (`mirror --continue --parallel`)
- [x] Exit code propagated; non-progress output captured into `job_log`
- [x] The remote server's pinned host key is written into the workspace as a
  `known_hosts` file, so `StrictHostKeyChecking=yes` on the NAS enforces the same pin
- [x] `Sweep` removes stale `job-*` workspaces at startup — a SIGKILL outruns the trap
- [x] Password-authenticated servers use `sshpass -f` as part of the connect-program,
  probed and cached as `sshpass_path`; without it the job fails with the install
  command rather than hanging on a prompt

**Exit:** a file and a directory both land on the NAS with correct sizes, and the
temp workspace is gone afterwards — including after a forced kill.

**Exit test result:** passed against the fake NAS (in-process SSH + in-memory SFTP +
canned exec), **not yet against the real Synology**. The workspace really is written
over SFTP and the modes requested are asserted (dir `0700`, `cmds`/`key`/`known_hosts`
`0600`, `run.sh` `0700`); exit codes propagate; a cancelled context aborts the stream.
Workspace rendering is a pure function and is tested exhaustively: both command
shapes, lftp and POSIX-sh quoting (spaces, quotes, backslashes, unicode, apostrophes),
non-default ports in `known_hosts`, segment/parallel clamping, a passphrase-protected
key being decrypted on the Pi, and six rejection paths. A dedicated test asserts the
generated command line and the runner script contain **no** password or key material
for both auth types. `rm -rf` of the workspace after a SIGKILL is the startup sweep's
job and is verified only by the sweep test here; the real forced-kill case belongs to
E2E.

---

## Phase 6 — Progress parsing — **DONE (2026-09-04)**

**Goal:** trustworthy percentage, speed and ETA.

- [x] Line scanner over the streamed output; each field matched independently rather
  than trying to match a whole line, because the format drifts between lftp versions
- [x] Reconciled against the pre-computed total from Phase 3; a percentage with a
  known total beats a per-file byte count during a mirror, and bytes never go backwards
- [x] Fallback: if nothing parseable arrives for 10 s, poll the destination size over
  SFTP every 5 s and derive progress from that
- [x] Coalesced to at most one update per job per second, held in memory and flushed
  to the DB on the same cadence
- [x] Parser unit tests against captured output from two lftp versions

**Exit:** progress advances monotonically and lands on 100% for both job kinds.

**Exit test result:** passed. `ParseProgress` is at 100% statement coverage over a
14-case table plus three captured fixtures in `internal/nas/testdata`: the 4.8-era
`at N (P%)` form with `
` updates, the 4.9-era `got N of M` form with interleaved
stderr, a zero-byte file, a non-UTF-8 filename, and output that yields no parseable
progress at all. Both transfer fixtures reach 100% and end on a summary line; the
unparseable one yields nothing, which is exactly when the size-poll fallback takes
over. The tracker's coalescing, monotonicity, staleness threshold and forced final
flush are asserted with a hand-cranked clock — no sleeps anywhere.

---

## Phase 7 — Queue and lifecycle — **DONE (2026-09-04)**

**Goal:** a real job system.

- [x] Worker pool sized from settings, resizable at runtime without restart: a
  one-second scheduler tick re-reads `concurrency` every time
- [x] State machine: `queued → running → done|failed|cancelled`, plus `interrupted`
  and `paused`. `done`, `failed` and `cancelled` are terminal.
- [x] `POST /api/jobs` accepts a batch (one server, many items, one destination) and
  expands into individual job rows; the destination goes through `nas.CheckPath`
- [x] Cancel: the row is marked cancelled first, then `TERM` is sent to the recorded
  PID on the NAS and the job context is cancelled, so the worker cannot overwrite it
- [x] Retry: clones the job row, relying on `--continue`/`-c` to resume
- [x] Startup recovery: `running` → `interrupted`, and `interrupted` is runnable
- [x] History retention sweep on a ticker

**Exit:** queue five jobs, cancel one mid-flight, restart the container, and watch the
interrupted job resume rather than restart.

**Exit test result:** passed in unit form, **not yet as a container restart**. The
state machine asserts all 49 from/to pairs against a hand-restated table, plus unknown
statuses and self-transitions, at 100% coverage. The manager is driven through a
channel-synchronised harness with the NAS and SFTP seams replaced: completion, a
non-zero exit code, a transfer error, FIFO ordering with peak concurrency pinned to
the configured 1, cancel of both a running and a queued job (with the NAS `TERM`
asserted), cancel of a finished job rejected, retry cloning and refusing a live job,
restart recovery resuming a `running` row with its partial bytes intact, `Stop`
recording running jobs as `interrupted`, and the retention sweep sparing live jobs.
The real container-restart-mid-flight scenario is an E2E item.

---

## Phase 8 — SSE — **DONE (2026-09-04)**

**Goal:** the UI learns about changes without polling.

- [x] `internal/events` hub: subscribe/unsubscribe, per-client 64-event buffer, slow
  clients dropped rather than allowed to grow memory
- [x] `/api/events` sends a full snapshot on connect, then deltas
- [x] Event types per CLAUDE.md; heartbeat comment every 20 s to defeat idle timeouts
- [x] `X-Accel-Buffering: no` set on the response; `proxy_buffering off` was already
  in `deploy/nginx.conf` from Phase 0

**Exit:** two browser tabs plus a phone all see the same live progress; killing the
Wi-Fi and reconnecting resyncs cleanly.

**Exit test result:** passed in unit form, **not yet with real browsers**. The hub is
at 100% coverage: fan-out to multiple subscribers, idempotent unsubscribe, progress
coalescing asserted on both sides of the one-second boundary with an injected clock,
terminal events never coalesced and clearing the window, a slow subscriber dropped
after exactly 64 buffered events while a fast one keeps going, and 800 concurrent
subscribe/publish/unsubscribe cycles. The handler is tested over a real HTTP
connection: `Content-Type: text/event-stream`, `X-Accel-Buffering: no`, a snapshot
frame containing the queued job, then a live delta. Two tabs and a phone, and the
nginx round trip, are E2E and manual items.

---

## Phase 9 — Frontend — **DONE (2026-09-05)**

**Goal:** the app people actually touch.

- [x] Vite + React + TS + Tailwind (v4, via `@tailwindcss/vite` — no PostCSS config);
  TanStack Query for server state
- [x] Routes: Browse, Queue, History, Servers, Settings. `location.hash` and a lookup
  table rather than a router dependency — five flat screens, no nested routes.
- [x] Browse: server picker → remote pane → destination picker → confirm. Split-pane
  on desktop (`sm:grid-cols-2`), a Source/Destination toggle on mobile.
- [x] Multi-select with a persistent selection bar showing count and total size.
  Directories are counted separately from the byte total, because their size is not
  known until the server walks them.
- [x] Queue: live cards with progress, speed, ETA, cancel, queue position, and an
  expandable per-job log. History carries retry and delete.
- [x] Servers: CRUD forms with a Test Connection action and inline result. Credential
  fields are blank on edit — blank means "keep what is stored".
- [x] Settings: NAS config, allowed roots, concurrency, segments, retention, plus the
  probed `lftp`/`sshpass` paths and a Clear button for the pinned NAS host key
- [x] `EventSource` hook feeding the query cache; explicit reconnect with backoff
  (1 s doubling to 30 s) and a live/disconnected dot in the header
- [x] Build output embedded via `embed.FS` and served by Go, inside the authenticated
  route group. The SPA fallback answers unknown paths with the shell but leaves
  unknown `/api/*` paths as a 404 rather than serving HTML to an API client.

**Exit:** full flow completed from a phone with no console errors.

**Exit test result:** built and served, **not yet driven from a phone**. The image
builds the bundle in a node stage and the container serves it: `GET /` is 401
unauthenticated and 200 with credentials, `/assets/app.js` likewise, and `/api/events`
delivers its snapshot frame immediately through nginx. Bundle is 206 KB raw / 65 KB
gzipped, well under the 500 KB gate. Vitest covers the formatting and path helpers,
the selection-bar aggregation, and the SSE hook: snapshot resync, delta folding, log
buffering, the disconnected indicator, reconnect backoff doubling, and no reconnect
after unmount — 27 tests, driven with fake timers and a stub `EventSource`, no sleeps.
Go-side tests assert the static routes, the auth on them, the SPA fallback and the
`/api/*` 404. The phone run-through is a manual item.

## Phase 10 — Hardening — **PARTLY DONE (2026-09-05)**

- [x] Credential redaction verified in every log path — the security-gate test drives
  create, list, test, browse and update with logging at debug and asserts no password,
  private key or passphrase reaches the log, and now also that none is echoed in a
  response body
- [x] `GOMEMLIMIT=48MiB` / `GOGC=50` set in the image
- [ ] RSS measured under a 5-job load — idle RSS in the running container is 14.6 MiB;
  the loaded measurement needs a reachable NAS
- [x] Healthcheck in the image, restart policy `unless-stopped` in compose, and a
  `TARGETARCH` build arg so the same compose file builds an amd64 image for a PC
- [x] README: the UI, the frontend dev loop, NAS prerequisites, `.env`, backup and
  recovery, accepted risks
- [x] Error surfaces reviewed: `writeErr` maps every domain error onto a status code,
  and the UI renders the server's message inline on every mutation and query rather
  than swallowing it

**Outstanding and deliberately not claimed:** the load-time RSS measurement, the Pi
deployment and 24-hour soak, `golangci-lint`/`gosec`/`govulncheck` (still not
installed on this machine), the Playwright E2E suite from the definition of done, and
every manual item — real Synology, real remote SFTP server, a phone on the LAN, a
1 GB transfer.

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
