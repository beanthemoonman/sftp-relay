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
