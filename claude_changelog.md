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

## Phase 9 — Frontend, and Phase 10 — Hardening (2026-09-05)

Built the React UI and wired it into the binary.

`web/` is Vite + React 18 + TS + Tailwind v4 (through `@tailwindcss/vite`, so there is
no PostCSS config and no `tailwind.config.js`). Five screens — Browse, Queue, History,
Servers, Settings — reached through `location.hash` and a lookup table rather than a
router dependency, because they are flat and there are five of them. Bottom nav on
mobile, top nav from `sm:` up.

- `src/api.ts` — the wire types and one `fetch` wrapper that turns a non-2xx into an
  `Error` carrying the server's own message, which is what every screen renders.
- `src/useEvents.ts` — the single `EventSource`. Frames are folded into the TanStack
  Query cache (`["jobs"]`, `["job-log", id]`) so components never subscribe to the
  stream themselves. Reconnect is explicit, 1 s doubling to 30 s, rather than relying
  on EventSource's own retry, because we want the backoff and the header's
  live/disconnected dot.
- `src/format.ts` — bytes, speed, ETA, breadcrumbs and the selection aggregation. The
  selection bar counts folders separately from the byte total: a directory's size is
  not known until the server walks it, so folding a zero into a total would be a lie.
- Browse is split-pane on desktop and a Source/Destination toggle on mobile; the
  selection persists across directories because it is keyed by full path.
- Servers leaves credential fields blank on edit, matching the API's "blank keeps the
  stored value" contract, so a secret never round-trips through the browser.

Serving: `web/embed.go` embeds `dist` with the `all:` prefix, and
`internal/api/static.go` serves it from inside the authenticated route group — the UI
is exactly as reachable as the API. The SPA fallback deliberately refuses to answer an
unknown `/api/*` path with HTML; an API client would parse a 200 page as success.
Asset names are fixed rather than content-hashed (rebuilds overwrite in place, so
`dist` never accumulates stale files and the committed `.gitkeep` survives), which is
why responses carry `Cache-Control: no-cache`.

The Dockerfile gained a node stage that builds the bundle before the Go stage copies
it in; a `.dockerignore` keeps `node_modules` out of the context. `docker-compose.yml`
gained a `TARGETARCH` build arg so the same file builds an amd64 image on a PC.

Hardening: the existing credential-redaction gate now also asserts that no password,
key or passphrase is echoed in a response body, not just kept out of the logs.

Verified: `gofmt -l`, `go vet` and `go test -race ./...` all clean in a linux
container; coverage `internal/...` — api 84.2%, config 91.2%, db 80.0%, events 100%,
jobs 85.3%, nas 84.7%, sftpclient 89.9%. Frontend `tsc --noEmit` clean, 27 Vitest
tests green, bundle 65 KB gzipped. The amd64 image runs here: `/` is 401
unauthenticated and 200 with credentials, `/assets/app.js` serves, `/api/events`
delivers its snapshot immediately through nginx, idle RSS 14.6 MiB.

Not done: RSS under a five-job load, the Pi deployment and 24-hour soak, the
Playwright E2E suite, `golangci-lint`/`gosec`/`govulncheck` (still not installed), and
every manual item — real Synology, real remote SFTP server, a phone on the LAN.

## 2026-09-05 — Review fixes

Addressed the findings from a full pass against CLAUDE.md and the definition of
done:

- **Mirror parallelism decoupled from concurrency.** A new `parallel` setting
  (migration `0004`, default 2) drives `mirror --parallel`; the manager no longer
  reuses `concurrency` for it. Wired through Settings UI, README and CLAUDE.md.
- **`nas.Client.disconnect` no longer leaks.** It now closes and nils both the
  SFTP and SSH clients even when one `Close` reports an error, instead of
  returning early and leaving a stale session behind.
- **SSE snapshot no longer shrinks the list.** `snapshotLimit` is now 200,
  matching the client's jobs-list limit, so a reconnect cannot overwrite a larger
  cache with a smaller one.
- **Ignored `Close` errors now use `_`.** `db.Open`, `config.loadDotEnv`,
  `sftpclient.conn.close` and every `rows.Close` in the repo go through an
  explicit discard (`_ =` / a `closeRows` helper) rather than a bare call.
- **DB calls take deadlines.** The pure-DB handlers (`listServers`, server CRUD,
  settings get/put, jobs list/get/delete/retry/log) now run under a 10s
  `dbTimeout`, matching the DoD's "every DB call takes a deadline".
- **`putSettings` validates numeric settings.** `concurrency`, `segments`,
  `parallel`, `history_retention_days` and `nas_port` reject malformed values
  with a 400 instead of silently falling back to defaults.
- **Doc/comment cleanup.** Removed the stale "(later)" in `main.go` and documented
  that the SSE hub's `lastSent` map stays bounded to in-flight jobs.

Verified: `gofmt -l` clean, `go vet` clean, `go build ./...` clean, `go test
./...` green (see `plan_to_implement.md` coverage notes still stand).


## Diagnosed live container: SFTP auth rejection and wrong NAS roots

- Inspected the running `sftp-relay` container. Logging is in fact working —
  `docker logs` carries structured `ERROR` lines for every failed request; they
  were just buried under nginx access lines. `docker logs sftp-relay 2>&1 | grep '"level"'`
  isolates them.
- Verified via `GET /api/servers` that server 1 stores a private key
  (`has_private_key: true`). It was never lost — the edit form deliberately blanks
  credential fields and the API treats blank as "keep stored".
- Added a `slog.Info` in `sftpclient.authMethods` recording the type and
  SHA256 fingerprint of the public key being offered. A rejected key was
  previously undiagnosable: the handshake error names no key. Fingerprints are
  public by definition, so this does not widen the credential-redaction surface.
- With that in place, confirmed the whatbox failure is remote-side: a valid
  `ssh-ed25519` key is parsed and offered, and the server refuses it — the
  matching public key is absent from the remote `authorized_keys`.
- Confirmed both configured `allowed_dest_roots` (`/volume1/MoonStorage/`,
  `/volume2/MoonStorage2/`) do not exist on the NAS; NAS browsing fails with
  `file does not exist` for both. Configuration, not a code fault.
- Rebuilt and redeployed the image.

## Traced the whatbox auth failure to the wrong key being stored

- Compared the fingerprint now emitted by `sftpclient.authMethods` against local
  key files. The key stored for the whatbox server is byte-identical to
  `deploy/nas_key` (`SHA256:Vkx2nmmbt0F+0kYOgDsFkGpaAVctGNbYi+rC/hRt54c`) — the
  NAS key had been pasted into the remote-server form, so whatbox was being
  offered a key it has never authorised.
- The machine holds a second, distinct ed25519 key at `~/.ssh/id_ed25519`
  (`SHA256:lHnhl/krYIwnd2xKp/PxefApfiFIhGgj7Un34bc9Qjk`), and `known_hosts`
  carries four whatbox entries, indicating that is the key whatbox knows.
- Reviewed `updateServer` and `db.UpdateServer`: the update path is correct and
  persists a non-blank key, so no code fault is implicated. Logs confirm a
  `server updated` at 16:48:50 followed by a test still offering the NAS key,
  i.e. the save carried a blank or unchanged key field.
- No code change this turn. The fingerprint log added previously was what made
  the misconfiguration visible; it stays.

## Corrected the earlier NAS-roots conclusion

- The previous entry's claim that `/volume1/MoonStorage` and
  `/volume2/MoonStorage2` do not exist was wrong — both are present over an SSH
  shell. The failing call is `sc.ReadDir` in `nas.Client.Browse`, which runs over
  SFTP, while every NAS operation that succeeded (lftp probe, workspace sweep)
  runs over the exec channel. DSM's SFTP service chroots to the shared folders,
  so the same directory is addressed as `/MoonStorage` over SFTP. Pending the
  user's `sftp upmoon@moonstore` output to confirm; if so, `allowed_dest_roots`
  and `nas_tmp` both need chroot-relative values.

## lftp exits 127 — probe now proves lftp actually runs

- `ProbeLftp` no longer trusts `[ -x path ]`. It runs `lftp -v` for `lftp` on PATH and
  each Synology candidate, taking the first that actually executes. An Entware binary
  missing its shared libraries is present and executable and still exits 127 — the old
  probe cached it happily and every job died with "lftp exited with status 127".
- A failed probe now clears the cached `lftp_path` (and `ProbeSSHPass` clears
  `sshpass_path`), so a stale path can't keep feeding jobs a broken binary.
- `ProbeSSHPass` validates with `sshpass -V` the same way.
- Error text updated: "no working lftp on the NAS — `lftp -v` did not run", with the
  missing-shared-libraries hint alongside the `opkg install lftp` remediation.
- Test: `TestProbeLftpMissingGivesRemediation` now seeds a stale `lftp_path` and
  asserts the failed probe clears it.
- Added `/bin/lftp` to `lftpCandidates` — that is where it lives on this Synology
  (DSM ships it in `/bin`, not under Entware).

## First real transfer against the live Synology — four bugs deep

Deployed with `docker compose up -d --build` (amd64, running on the PC) and drove real
jobs against the real NAS and the real remote server. Each fix below was found by the
previous one becoming visible.

1. **SFTP chroot vs. shell paths.** Synology chroots SFTP to the share root, so
   `/MoonStorage2/lftp-scripts` over SFTP is `/volume2/MoonStorage2/lftp-scripts` to a
   shell. Every generated shell command pointed at a path that did not exist:
   `/bin/sh .../run.sh` exited 127 and the boot sweep deleted nothing (three stale
   workspaces had accumulated). Added `nas.ShellPath`, which probes `/volumeN/<share>`
   once per share and caches the prefix; wired into the job script, its destination,
   `Cancel` and `Sweep`. SFTP-side paths are unchanged.
2. **`sess.Stderr = io.Discard`.** Anything failing before `run.sh`'s `exec 2>&1` — the
   127 above included — vanished, leaving an empty job log and a bare exit code. Both
   streams now scan into the log. This is what turned every later step into a
   one-attempt diagnosis.
3. **Share ACLs override `chmod`.** The uploaded key came out `-rwxrwxrwx`, and ssh
   ignores a world-readable private key, so auth failed with `Permission denied
   (publickey)`. The workspace is now two directories: a staging dir inside the share
   (all SFTP can reach) and a runtime `/tmp/sftp-relay-job-<id>` created by the script
   at `0700` with the credentials copied in at `0600`. Both are removed by the trap;
   the sweep covers both. `Cancel` reads the pidfile from the runtime dir.
4. **Key format.** The stored key had no trailing newline; OpenSSH answered
   `Load key: invalid format` and fell through to no authentication. `normalizePEM`
   strips CRLF and guarantees the final newline on every key written to the NAS.
5. **Progress pinned at zero.** `xfer:use-temp-file yes` means the destination is
   `.in.<name>` until the very end, so the size-poll fallback measured a file that did
   not exist yet. `nas.Size` now falls back to the temp name.

**Verified end to end:** a 635 MB file moved from the remote server to
`/MoonStorage2/tmp/...`, exit code 0, 100%, full size on disk, both workspaces gone,
no orphaned lftp process on the NAS.

Tests added: `TestShellPathResolvesAnSFTPChroot` (per-share caching, independent
shares, unchrooted passthrough), `TestNormalizePEM` (five cases),
`TestSizeCountsAnInFlightTempFile`. `render` now takes staging and runtime dirs.
`go build`, `go vet`, `go test ./...` all clean.

Known deviation: files land on the NAS as `-rwxrwxrwx` because the share's ACL wins.
Not fixed — that is the share's configuration, not ours.

## GitHub CI
Added `.github/workflows/ci.yml`: a `go` job (gofmt check, `go vet`, `go test -race ./...`,
CGO-free linux/arm64 build) and a `web` job (`npm ci`, vitest, `vite build` incl. `tsc --noEmit`).
Left out golangci-lint (no `.golangci.yml` in repo), eslint (not installed despite the npm script),
and the E2E suite (fake-NAS compose stack doesn't exist yet).

## CI: linting wired in
- Added `.golangci.yml` (v2 format): errcheck, staticcheck, govet, ineffassign, revive, gosec.
  Exclusions are narrow: deferred/`t.Cleanup` `Close()` calls, revive's doc-comment rules
  (single-binary app, not a library), gosec in tests, `web/node_modules`.
- Fixed what it found for real: `_ =` on the connection-teardown `Close()` calls in
  `internal/nas/nas.go` and `internal/sftpclient/sftpclient.go`, on `tx.Rollback()` in
  `internal/db/db.go` and `os.Unsetenv` in `internal/config/config_test.go`; `#nosec G304`
  justification on the `.env` open; `//nolint:errcheck,gosec` on the best-effort SIGTERM;
  tagged switch in `sftpclient_test.go`; blank-import comment on the sqlite driver.
  Repo is now `golangci-lint run` clean.
- Added eslint to `web` (`eslint`, `@eslint/js`, `typescript-eslint`,
  `eslint-plugin-react-hooks`) with a flat `eslint.config.js`. Replaced the three
  production `any`s (`Frame.data` -> `unknown`, `send` -> generic) rather than muting the
  rule; `react-hooks/set-state-in-effect` is off with a reason, `no-explicit-any` off in tests.
- CI now runs `golangci-lint-action@v8` and `npm run lint`.
- Still not in CI: the E2E suite — the `sftp-source` / `fake-nas` / `relay` compose stack
  and the Playwright specs from the DoD do not exist yet.

## E2E plan
Wrote `e2e_plan.md` — a handoff document for building the Playwright E2E suite in a fresh
session: the three-container topology (`sftp-source` / `fake-nas` / `relay`), the file
layout under `test/e2e/`, fixture tree, API-driven setup (no DB surgery), all 16 DoD
scenarios grouped into spec files, five increments, the CI job, and the known gotchas
(ShellPath untested without a chrooted fake NAS, named volume for the restart-resume
scenario, `.in.<name>` temp files, host-key pinning across `down -v`).

## 2026-09-05 — End-to-end suite (e2e_plan.md, increments A–E)

Built the Playwright E2E suite the definition of done demands, all five increments, and
proved it with three consecutive clean runs.

**The stack** (`test/e2e/docker-compose.yml`): a `keygen` init service that generates the
SSH keys into the bind-mounted `test/e2e/keys`, a `seed` init service that generates the
fixture tree (including the 200 MB blob and a `SHA256SUMS` manifest) into a named volume,
`sftp-source` (atmoz/sftp, a key user and a password user), `fake-nas` (alpine + sshd +
lftp + sshpass, `/volume1/media` destination volume) and `relay`, built from
`deploy/Dockerfile` with `TARGETARCH=amd64`. `E2E_PORT` defaults to 8088 and can be moved
so the suite runs beside a dev stack.

Four things about the stack are load-bearing and were each learned the hard way:

- Only *directories* are bind-mounted out of `./keys`. Compose creates a missing bind
  source as a directory, and every container is created before `keygen` runs, so mounting
  `./keys/nas_key` as a file handed the relay an empty folder.
- `adduser -D` leaves `!` in `/etc/shadow`, which sshd reads as a locked account and
  refuses public-key auth for. `sed 's|^nas:!:|nas:*:|'` fixes it.
- `openssh-server` does not bring the ssh *client*, and lftp reaches `sftp://` by
  spawning `ssh`. Without `openssh-client` every transfer failed with `sh: ssh: not found`.
- lftp is moved to `/opt/bin/lftp`, off the non-interactive `PATH`, so the startup probe
  is exercised rather than trivially satisfied.

**Throttling.** Two containers on a loopback move 200 MB in well under a second, which
leaves the progress, cancel, restart-resume and multi-client scenarios nothing in flight
to observe — and `segments=1`, the remedy the plan suggested, does not change that. `tc`
was tried first and is unavailable: Docker Desktop's kernel ships no `act_police` module.
`fake-nas/lftp-wrapper.sh` now sits in front of the real binary and prepends
`set net:limit-total-rate` when `/tmp/e2e-throttle` exists; with no such file it is
`exec` plus argv and nothing else. `throttle()` / `unthrottle()` drive it from the specs.

**Setup.** `global-setup.ts` configures everything through the app's own API — never
SQLite — then restarts the relay once, because the container boots before the NAS is
configured and the startup tool probe skips itself. The restart runs the real boot path:
pinning `nas_host_key`, sweeping stale workspaces, and resolving lftp off the thin PATH.
Setup fails loudly there rather than inside the first transfer spec.

**19 tests**, every DoD §2 scenario plus two extras:

- `auth` — 401 on missing, wrong password and wrong user; `/api/health` open; UI served
- `servers` — CRUD through the form, including the connection test pinning the host key
- `browse` — three levels deep and back by breadcrumb, directories first, sizes and dates,
  NAS destination browsing, folder creation, and `../../etc/evil` refused visibly
- `transfer` — big.bin with progress observed mid-flight and a matching sha256 on the NAS
  volume, a directory mirror with nesting, a five-item batch, and the zero-byte and
  spaces-and-unicode filenames
- `lifecycle` — concurrency capped at 2 with queue positions rendered, cancel leaving no
  lftp and no workspace, restart-resume proving `pget -c` resumes rather than truncating,
  a nonexistent path failing readably with retry offered, and a stopped NAS erroring
  rather than hanging (finishing by proving the boot sweep clears what `docker stop`
  SIGKILLed past the cleanup trap)
- `resilience` — the relay restarted under a live stream, the disconnected indicator, the
  backoff reconnect and the snapshot resync; two browser contexts on the same progress;
  the 375px browse-to-queue flow with no horizontal scroll and no card behind the tab bar
- `cleanup` — its own Playwright project, dependent on the rest, asserting zero orphaned
  workspaces, zero lftp processes and at most the one expected NAS SSH connection

**App changes:** six `data-testid` hooks only — the job card (`data-job-id`,
`data-status`, `data-percent`), the server row, the two browse panes, the selection bar
and the SSE indicator. `Card` now forwards arbitrary div props to carry them. No
behavioural change; no E2E finding required one.

**One phantom bug** cost real time and is worth recording: the cancel spec asserted the
card turned `cancelled` in place. It does not — `cancelled` is terminal, so the row
leaves the Queue screen for History. The backend was correct throughout. A second
false alarm: `pgrep -f lftp.real` matched the `sh -c` that `docker compose exec` runs it
in; the bracket trick (`'[l]ftp.real'`) fixes it.

**CI:** an `e2e` job in `.github/workflows/ci.yml`, gated on `go` and `web`, Chromium
only, uploading `test-results/`, the HTML report and container logs on failure.

Updated `CLAUDE.md` (layout plus an E2E section), `README.md` (how to run it),
`plan_to_implement.md` (the E2E block closed out) and `e2e_plan.md` (boxes ticked plus an
"as built" section recording every deviation).
