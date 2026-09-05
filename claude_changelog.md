# Claude changelog

## 2026-09-04 — Phases 0 and 1

Built the skeleton, the container pipeline and persistence.

- `internal/config`: `.env` loader (real environment wins), `PORT`, `DB_PATH`,
  `AUTH_USER`, `AUTH_PASS`, `NAS_SSH_KEY_PATH`, `LOG_LEVEL` → `slog.Level`.
  Hand-rolled 20-line parser rather than a dependency.
- `cmd/relay/main.go`: slog JSON logging, chi router, `GET /api/health` (pings the DB),
  `SIGTERM`/`SIGINT` graceful shutdown with a 10s drain.
- `internal/db`: `modernc.org/sqlite` with WAL, 5s busy timeout, foreign keys on and a
  single writer connection; embedded numbered migrations applied once each inside a
  transaction and recorded in `schema_migrations`.
- `internal/db/migrations/0001_init.sql`: `servers`, `jobs`, `job_log`, `settings` with
  status/kind CHECK constraints, `ON DELETE CASCADE` from jobs and logs, and seeded
  settings (concurrency 2, segments 4, retention 90 days, empty allowed roots).
- `internal/db/repo.go`: context-taking repository — server CRUD, job create/get/list/
  status/progress, ring-buffered job log, settings get/set. Job queries use explicit
  `INNER JOIN` to carry the server name.
- `deploy/`: multi-stage Dockerfile (`CGO_ENABLED=0`, arm64 by default, Alpine, non-root
  uid 1000, `cap_net_bind_service` on nginx), `nginx.conf` with `proxy_buffering off` for
  SSE later, `entrypoint.sh` (nginx as a daemon, `exec relay`), `docker-compose.yml`
  with the `/data` volume and read-only NAS key mount, `.env.example`.
- `README.md`: status, run instructions, `.env` reference, DB backup/recovery, accepted
  risks (plaintext credentials, no TLS, single user).
- Tests: `internal/config` 91.2% coverage, `internal/db` 83.5%; `go test -race ./...`
  clean in a linux container (the Windows host has no gcc, so `-race` cannot run there).

Verified: image built and run locally, `/api/health` returned 200 through nginx, restart
idempotent on the same volume, clean shutdown on SIGTERM, arm64 cross-compile succeeds.

Not done: `golangci-lint`, `gosec` and `govulncheck` are not installed on this machine,
so those gates are unrun. The arm64 image has not yet run on the real Pi, which is the
last thing standing between Phase 0 and formal closure.

## 2026-09-04 — Phases 2, 3 and 4

Auth, remote SFTP browsing and the NAS destination side.

- `internal/api`: the whole HTTP surface for these phases. Basic auth middleware over
  a chi group covering every route except `/api/health`; both fields SHA-256'd then
  compared with `subtle.ConstantTimeCompare`, and a blank configured user or password
  authorises nothing. Server CRUD, connection test, remote browse, NAS browse, NAS
  mkdir, settings get/put. Domain errors map to status codes in one place
  (`ErrNotFound` 404, path errors 400, NAS unconfigured 503, host key changed 502,
  deadline 504).
- Credentials are write-only on the wire: `serverView` reports `has_password` /
  `has_private_key`, and a blank credential field on `PUT` keeps the stored value.
- `internal/sftpclient`: SSH + SFTP to remote servers. Connection pool keyed by server
  id with a janitor evicting connections idle for two minutes, transparent redial on a
  dead session, host key captured on first connect and pinned after, password and
  (optionally passphrase-protected) key auth, directories-first listings, and a
  recursive size pre-computation for the future progress bar.
- `internal/nas`: SSH + SFTP to the Synology with a 30s keepalive and reconnect,
  directory-only browsing with `StatVFS` free space, `mkdir`, command exec, and the
  lftp startup probe (PATH then the usual Entware/Synology locations) which caches
  `lftp_path` and reports `opkg install lftp` when it comes up empty. The probe runs in
  the background so an offline NAS does not delay startup.
- `internal/nas/path.go`: the single path validator. NFC-normalise, require absolute,
  clean, containment check, resolve symlinks on the NAS, containment check again; the
  nearest existing ancestor is used for a path that does not exist yet. Fails closed
  with no roots configured.
- `internal/db`: added `SetServerHostKey`; migration `0002_nas_settings.sql` seeds
  `nas_host_key` and `nas_port` with `INSERT OR IGNORE` so existing databases are safe.
- `cmd/relay/main.go`: wires the pool, the NAS client and the API, and kicks off the
  lftp probe. An unset `AUTH_USER`/`AUTH_PASS` now logs a warning at startup.
- Tests: real SSH handshakes and SFTP protocol exchanges against in-process servers
  (`sftp.InMemHandler`) for both `internal/sftpclient` and `internal/nas`, rather than
  stubs. `goleak` in `TestMain` for both. A credential-redaction test asserts that a
  password, private key and passphrase never appear in log output across create, list,
  test, browse and update — including the error paths.
- Docs: README gained an API section, a settings table, a destination-safety section
  and real NAS prerequisites; CLAUDE.md records the host-key pinning decision, the
  single-validator rule and the current settings keys.

Verified: `gofmt`/`go vet` clean, arm64 `CGO_ENABLED=0` build clean, `go test -race`
clean in a linux container, three consecutive clean runs, `goleak` clean. Coverage
`internal/...` 85.4% total (api 84.4%, config 91.2%, db 81.5%, nas 85.0%, sftpclient
89.9%); the path validator functions themselves are at 100%. Container smoke test:
`/api/health` 200 unauthenticated, `/api/servers` 401 with a `Www-Authenticate`
challenge unauthenticated and 401 with wrong credentials, 200 with correct ones,
server create/list/delete through curl, restart idempotent, migration 0002 applied.

Not done: still no run against a real remote SFTP server or the real Synology — both
exit criteria are recorded in the plan as passing against in-process servers only.
`golangci-lint`, `gosec` and `govulncheck` remain uninstalled on this machine. The Pi
deployment and the 24-hour soak are still outstanding from Phase 0.

## Phases 5–8 — lftp execution, progress parsing, the queue, and SSE (2026-09-04)

Built the whole transfer path: jobs now really run.

- `internal/nas/lftp.go`: the ephemeral workspace and the one-shot exec. `render` is
  a pure function producing every file plus the command, so the escaping, the script
  shape and the "no credentials on a command line" rule are unit-testable without a
  NAS. Files are chmodded before content is written. The remote server's pinned host
  key becomes a `known_hosts` file so `StrictHostKeyChecking=yes` on the NAS enforces
  the same pin the Pi does. `Cancel` TERMs the recorded PID, `Sweep` clears stale
  workspaces at boot, `Size` measures the destination for the progress fallback.
- Password-authenticated remote servers turned out to need `sshpass` on the NAS:
  lftp reaches `sftp://` by spawning `ssh`, which cannot take a password
  non-interactively. `sshpass_path` is probed and cached (migration `0003`), and a
  job for such a server fails with the `opkg install sshpass` remediation rather than
  hanging on a prompt. Key auth needs none of it. A passphrase-protected key is
  decrypted on the Pi so the passphrase never reaches the NAS.
- `internal/nas/progress.go`: the output parser. Each field is matched independently
  because lftp's format drifts by version; fixtures for two versions live in
  `internal/nas/testdata`.
- `internal/jobs`: the state machine (terminal `done`/`failed`/`cancelled`, retry
  clones rather than revives), the tracker (1 Hz coalescing, monotonic bytes,
  10 s staleness before the destination-size fallback), and the manager — a
  one-second scheduler tick that re-reads `concurrency`, which is what makes it
  resizable at runtime. Restart recovery marks `running` as `interrupted` and treats
  `interrupted` as runnable, so the status stays visible while the job resumes.
- `internal/events`: the SSE hub. 64-event buffer per subscriber, slow subscribers
  dropped rather than buffered, progress coalesced to one event per job per second.
- `internal/api/jobs.go`: the jobs endpoints and `/api/events`. The stream sends a
  full snapshot on connect and nothing is replayed; `X-Accel-Buffering: no` pairs
  with the `proxy_buffering off` already in nginx.
- `internal/db`: `SetJobTotal`, `RunnableJobs`, `InterruptRunningJobs`, `DeleteJob`,
  `PurgeJobs`, and cursor paging on `ListJobs`.

Testing seams rather than interfaces: the manager holds four function fields
(`transfer`, `remoteSize`, `destSize`, `killRemote`, `sweep`) pointing at the real
NAS and pool, which tests replace to drive whole job lifecycles over channels with
no SSH server and no sleeps.

Verified: `gofmt`/`go vet` clean, arm64 `CGO_ENABLED=0` build clean, `go test -race`
clean in a linux container, three consecutive clean runs. Coverage `internal/...`
85.8% total (api 83.3%, config 91.2%, db 80.4%, events 100%, jobs 84.7%, nas 87.0%,
sftpclient 89.9%). The four packages held to 90%+ are there: `ParseProgress`,
`CanTransition`/`Transition`/`Terminal`, `CheckPath`/`normalise`/`contained` and the
whole events hub are all at 100%.

Not done: no run against the real Synology or a real remote SFTP server; the
container-restart-mid-transfer and two-tabs-plus-a-phone exit criteria are verified
in unit form only and remain E2E/manual items. `golangci-lint`, `gosec` and
`govulncheck` are still not installed on this machine. The Pi deployment and the
24-hour soak are still outstanding from Phase 0.
