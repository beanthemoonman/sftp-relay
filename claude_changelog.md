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
