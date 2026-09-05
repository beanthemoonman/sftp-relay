# CLAUDE.md

Project context for `sftp-relay`

## Notes from Management (The Developer)

- End your turn by saying Bada Bing!
- Append what you did during your turn to the end of the file claude_changelog.md.
- If anything you did means that now something in claude.md is incorrect, please update it.
- When you complete part of the plan, update `plan_to_implement.md` in the same turn: tick the
  boxes, flip the phase/increment status, and record the exit-test result. The plan file must
  never drift behind what's actually built.

## Definition of Done
@definition_of_done.md

## What this is

A self-hosted web app that browses remote SFTP servers and pulls files down onto a
Synology NAS. It runs as a single Docker container on a **Raspberry Pi**, not on the
NAS. The Pi is the control plane; the NAS does the actual transferring.

Two distinct SSH/SFTP relationships exist and must never be conflated:

| Relationship | Purpose | Implemented with |
|---|---|---|
| Container → remote SFTP servers | Browsing / listing / sizing | `pkg/sftp` in Go |
| Container → NAS | Destination browsing + running `lftp` | `x/crypto/ssh` + `pkg/sftp` in Go |
| NAS → remote SFTP servers | The actual bulk transfer | `lftp`, executed on the NAS |

Bytes never flow through the Pi. The Pi only ever moves listings, control commands
and progress text.

## Stack

- **Backend:** Go 1.23+, `net/http` + `chi` router, no CGO anywhere
- **SFTP:** `github.com/pkg/sftp` over `golang.org/x/crypto/ssh`
- **DB:** `modernc.org/sqlite` (pure Go — CGO is not an option, we cross-compile for arm64)
- **Frontend:** React 18 + TypeScript + Vite + Tailwind, TanStack Query, `EventSource` for SSE
- **Serving:** nginx inside the container listens on `:80` and reverse-proxies everything
  to the Go process on `127.0.0.1:8080`. Go serves both `/api/*` and the built React
  bundle (embedded via `embed.FS`). nginx is the front door; Go serves the app.
- **Container:** single image, multi-stage build, `linux/arm64`, Alpine base
- **Transfers:** `lftp` on the NAS (must be installed there — Entware/`opkg` or a
  community package; the app surfaces a clear error if `lftp -v` fails)

## Hard constraints

1. **Pure Go only.** No CGO. Anything requiring it is disqualified — the build targets
   arm64 and cross-compiling with CGO is not worth the pain.
2. **Low memory.** Target RSS under ~60 MB. Stream listings, never buffer transfer
   output wholesale, cap SSE fan-out buffers, set `GOGC=50` and `GOMEMLIMIT`.
3. **LAN only.** No TLS, no public exposure, no CSRF machinery. HTTP basic auth is
   the entire auth story and that is deliberate.
4. **Mobile-first UI.** The primary use case is queueing a download from a phone.
   Desktop is the wider variant, not the other way round.

## Security posture (deliberate, do not "fix" without asking)

- Remote server credentials — passwords and private keys — are stored **plaintext in
  SQLite**. This is an accepted risk for a LAN-only single-user tool. Do not add
  an encryption layer unprompted.
- Basic auth credentials for the web UI come from `.env` (`AUTH_USER`, `AUTH_PASS`),
  not the DB, so a corrupt DB can't lock you out.
- The NAS SSH key is mounted read-only into the container; its path comes from `.env`.
- **Credentials must never appear on a command line.** `lftp -u user,password` is
  visible in the NAS process list. Instead: write an ephemeral `0600` script and key
  file into the NAS temp dir over SFTP, reference it from the lftp script, and delete
  both in a shell `trap` so cleanup survives a crash.
- All NAS destination paths are validated against a configured allow-list of roots
  after `filepath.Clean` and symlink resolution. Reject anything that escapes.
  `nas.CheckPath` is the single implementation; every handler taking a NAS path
  goes through it, and it fails closed when no roots are configured.
- Host keys — remote servers and the NAS alike — are captured on first connect
  and pinned thereafter. A changed key is an error, never a silent accept.
  Clearing the stored key (server form, or the `nas_host_key` setting) is the
  deliberate way to re-trust a host.

## Job execution model

Per job the worker does:

1. Load server + credentials from SQLite.
2. Pre-compute the total byte count via `pkg/sftp` (stat for a file, walk for a
   directory). This is what makes an accurate progress bar possible.
3. Open an SSH connection to the NAS.
4. SFTP an ephemeral key file (`0600`) and a generated lftp script into `$NAS_TMP/job-<id>/`.
5. `ssh nas 'sh -c "trap cleanup EXIT; echo $$ > pidfile; lftp -f script"'` as a
   **one-shot exec per job** — no persistent shell.
6. Stream combined stdout/stderr back over the session, parse it for progress, and
   fall back to polling destination size over SFTP if parsing yields nothing for
   ~10 s (lftp output format varies by version).
7. Record exit code, final byte count, duration.

Command shapes:

- File: `pget -n {segments} -c "remote" -o "local"`
- Directory: `mirror --continue --parallel={n} --use-pget-n={segments} "remote" "local"`
- Prelude: `set cmd:fail-exit yes; set net:max-retries 3; set net:timeout 30;`
  `set xfer:use-temp-file yes; set xfer:clobber yes; set sftp:auto-confirm no;`
  `set sftp:connect-program "ssh -a -x -o StrictHostKeyChecking=yes -o UserKnownHostsFile={ws}/known_hosts -o BatchMode=yes -p {port} -i {ws}/key -o IdentitiesOnly=yes"`

The remote server's pinned host key is written into the workspace as a `known_hosts`
file, so `StrictHostKeyChecking=yes` on the NAS enforces the same pin the Pi does.

**Password-authenticated remote servers need `sshpass` on the NAS.** lftp reaches
`sftp://` by spawning `ssh`, and ssh will not take a password from anywhere we can
reach non-interactively. The connect-program becomes `sshpass -f {ws}/pass ssh …`.
`sshpass_path` is probed at startup like `lftp_path`; when it is missing, queueing
such a job fails with the `opkg install sshpass` remediation. Key authentication
avoids the dependency and is the preferred setup. A passphrase-protected key is
decrypted on the Pi and written to the workspace unencrypted — ssh on the NAS cannot
be prompted — so the passphrase itself never leaves the Pi.

**Cancellation:** closing the SSH session does not reliably kill `lftp`. The job writes
its PID to a file; cancel opens a fresh exec and sends `TERM` to that PID.

**Restart recovery:** on boot, any job in `running` is moved to `interrupted`, and
the scheduler treats `interrupted` as runnable alongside `queued` — so the status
stays visible in the UI while the job resumes. `pget -c` and `mirror --continue`
resume rather than restart. A stale-workspace sweep (`rm -rf $NAS_TMP/job-*`) also
runs at boot, because a SIGKILL outruns the cleanup trap.

**Scheduling:** a single loop ticks once per second, re-reads `concurrency` and
starts as many waiting jobs as the setting allows. Re-reading every tick is what
makes concurrency resizable at runtime without a restart.

**State machine:** `internal/jobs` owns it. `done`, `failed` and `cancelled` are
terminal — retry clones the row rather than reviving it, so history stays honest.
Every status change goes through `jobs.Transition`.

**Concurrency:** N concurrent jobs (config, default 2), each with M lftp segments
(config, default 4). Both are DB settings, changeable at runtime.

## Data model (SQLite, WAL mode)

- `servers` — id, name, host, port, username, auth_type (`password`|`key`), password,
  private_key, passphrase, host_key, default_remote_path, created_at, updated_at
- `jobs` — id, server_id, kind (`file`|`dir`), remote_path, dest_path, status
  (`queued`|`running`|`paused`|`done`|`failed`|`cancelled`|`interrupted`), total_bytes,
  transferred_bytes, speed_bps, eta_seconds, exit_code, error, started_at, finished_at
- `job_log` — job_id, ts, line (ring-buffered, capped per job)
- `settings` — key/value: `nas_host`, `nas_user`, `nas_port`, `nas_host_key`,
  `nas_tmp`, `lftp_path`, `sshpass_path`, `allowed_dest_roots`, `concurrency`,
  `segments`, `history_retention_days`

Migrations are plain numbered `.sql` files applied in order at startup.

## API surface

```
GET    /api/health
GET    /api/servers                     POST /api/servers
PUT    /api/servers/{id}                DELETE /api/servers/{id}
POST   /api/servers/{id}/test
GET    /api/servers/{id}/browse?path=
GET    /api/nas/browse?path=            POST /api/nas/mkdir
GET    /api/jobs?status=&limit=&cursor= POST /api/jobs
GET    /api/jobs/{id}                   DELETE /api/jobs/{id}
POST   /api/jobs/{id}/cancel            POST /api/jobs/{id}/retry
GET    /api/jobs/{id}/log
GET    /api/settings                    PUT /api/settings
GET    /api/events                      (SSE)
```

`POST /api/jobs` accepts a batch: one server, many items, one destination.

## SSE contract

Single `/api/events` stream, event types: `job.created`, `job.progress`, `job.done`,
`job.failed`, `job.log`. Progress events are coalesced server-side to at most one per
job per second. Client reconnects with `Last-Event-ID`; server replays nothing but
sends a full snapshot on connect so the UI can resync.

## Layout

```
cmd/relay/main.go
internal/config      env + settings loading
internal/db          sqlite, migrations, queries
internal/sftpclient  remote server browsing, connection pool
internal/nas         ssh/sftp to the NAS, lftp invocation, progress parsing
internal/jobs        queue, worker pool, state machine
internal/events      SSE hub
internal/api         handlers, basic auth middleware, static serving
web/                 React app (built into web/dist, embedded)
deploy/              Dockerfile, nginx.conf, docker-compose.yml, .env.example
```

## Conventions

- Errors wrapped with `fmt.Errorf("...: %w", err)`; no naked returns of `err` across
  package boundaries.
- `log/slog`, JSON handler, level from env. Never log credential fields — add them to
  a redaction list.
- Contexts everywhere; every SSH/SFTP operation takes a deadline.
- SQL uses explicit `JOIN` syntax. No implicit joins in the `FROM` clause.
- Table-driven tests. The lftp output parser and the path validator get thorough unit
  tests — they are the two places bugs are most costly.
- Frontend: no global state library. TanStack Query for server state, `useState`/context
  for the rest.

## Gotchas

- nginx buffers SSE by default. `proxy_buffering off;` and `X-Accel-Buffering: no`.
- Synology's default shell is ash, not bash. Generated remote scripts must be POSIX sh.
- `lftp` on Synology often lives outside the default `PATH` for non-interactive SSH.
  Resolve its absolute path once at startup and cache it in settings.
- Synology file ownership: downloads land as the SSH user. Surface that in the UI so
  a wrong-owner surprise is diagnosable.
- `modernc.org/sqlite` is slower than the C build under write contention. Keep writes
  batched — progress updates go to memory and flush to DB at most once per second.
