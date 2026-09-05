# sftp-relay

Browse remote SFTP servers from a phone and pull files down onto a Synology NAS.
Runs as one Docker container on a Raspberry Pi. Bytes never pass through the Pi —
the NAS does the transfer with `lftp`, the Pi only sends control commands.

Architecture, conventions and the security posture live in [CLAUDE.md](CLAUDE.md);
the build order lives in [plan_to_implement.md](plan_to_implement.md).

## Status

Phases 0–10 are built: the container runs, persists to SQLite, requires basic auth,
manages SFTP servers and browses them, browses the NAS destination tree with an
allow-listed path validator, runs real transfers — a worker pool drives `lftp` on
the NAS, parses its output for progress, and streams live updates over SSE — and
serves a React UI for all of it. What remains is verification against the real
Synology, the real Pi and a real phone; see `plan_to_implement.md`.

## The UI

Open `http://<pi>:8088/` and log in with `AUTH_USER`/`AUTH_PASS`. Five screens,
mobile first: **Browse** (pick a server, select files, pick a NAS destination,
queue them), **Queue** (live progress, speed, ETA, cancel), **History** (finished
jobs, retry, delete, per-job log), **Servers** (CRUD plus Test Connection) and
**Settings** (NAS host, allowed roots, concurrency, segments, retention).

The bundle is embedded in the binary and served by Go behind the same basic auth
as the API, so there is nothing extra to deploy. A dot in the header is green while
the event stream is live and amber while it reconnects.

### Working on the frontend

```sh
cd web
npm install
npm run dev      # Vite on :5173, /api proxied to a locally running relay
npm run build    # writes web/dist, which go:embed picks up
npm test         # vitest
```

`go build` needs `web/dist` to exist; the committed `web/dist/.gitkeep` is what
makes a clean checkout build without node installed. The Docker image builds the
bundle itself in a node stage, so nothing needs to be committed.

## Running it

```sh
cp deploy/.env.example deploy/.env   # then edit it
docker compose -f deploy/docker-compose.yml up -d --build
curl http://<pi>:8088/api/health     # {"status":"ok"}
```

The image targets `linux/arm64`; build it on the Pi, or with
`docker buildx build --platform linux/arm64`. For a local x86 test:
`TARGETARCH=amd64 docker compose -f deploy/docker-compose.yml up -d --build`.

The compose file bind-mounts the NAS private key from `NAS_SSH_KEY_PATH_HOST`. That
file must exist before the first `up`, or Docker will helpfully create a directory
in its place.

## `.env` reference

| Variable | Default | Purpose |
|---|---|---|
| `AUTH_USER` / `AUTH_PASS` | — | Basic auth for the web UI. Read from the environment, not the DB, so a corrupt DB cannot lock you out. |
| `DB_PATH` | `/data/app.db` | SQLite file, on the `relay-data` volume |
| `NAS_SSH_KEY_PATH` | `/keys/nas_key` | Private key for the NAS, mounted read-only |
| `NAS_SSH_KEY_PATH_HOST` | — | Host path of that key, bind-mounted by compose |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `PORT` | `8080` | Go listener behind nginx |

## API

Everything except `/api/health` needs basic auth, so container healthchecks work
without credentials and nothing else does.

```
GET    /api/health                       no auth
GET    /api/servers                      POST   /api/servers
PUT    /api/servers/{id}                 DELETE /api/servers/{id}
POST   /api/servers/{id}/test            GET    /api/servers/{id}/browse?path=
GET    /api/nas/browse?path=             POST   /api/nas/mkdir
GET    /api/jobs?status=&limit=&cursor=  POST   /api/jobs
GET    /api/jobs/{id}                    DELETE /api/jobs/{id}
POST   /api/jobs/{id}/cancel             POST   /api/jobs/{id}/retry
GET    /api/jobs/{id}/log
GET    /api/events                       server-sent events
GET    /api/settings                     PUT    /api/settings
```

Server credentials are write-only: responses report `has_password` and
`has_private_key` rather than the values, and sending a blank `password`,
`private_key` or `passphrase` on `PUT` keeps whatever is stored.

```sh
R=http://<pi>:8088; A=-urelay:yourpassword

curl -s $A -X POST $R/api/servers -d '{
  "name":"seedbox","host":"seedbox.example","port":22,"username":"me",
  "auth_type":"password","password":"...","default_remote_path":"/downloads"}'

curl -s $A -X POST $R/api/servers/1/test
curl -s $A "$R/api/servers/1/browse?path=/downloads"
```

`POST /api/jobs` takes a batch: one server, many items, one destination. Set
`is_dir` from the browse listing — it decides between `pget` and `mirror`.

```sh
curl -s $A -X POST $R/api/jobs -d '{
  "server_id":1, "dest_path":"/volume1/media/films",
  "items":[{"path":"/downloads/a.mkv","is_dir":false},
           {"path":"/downloads/season 1","is_dir":true}]}'

curl -s $A "$R/api/jobs?status=running"
curl -s $A -X POST $R/api/jobs/3/cancel
curl -s $A -X POST $R/api/jobs/3/retry      # clones the job; lftp resumes
curl -s $A "$R/api/jobs/3/log"
curl -sN $A $R/api/events                    # snapshot, then live deltas
```

`/api/events` sends a full `snapshot` on connect and then `job.created`,
`job.progress`, `job.done`, `job.failed` and `job.log` deltas. Progress is
coalesced to at most one event per job per second, and a client that cannot keep
up is dropped rather than buffered — it reconnects and resyncs from the snapshot.

## Settings

Settings live in the database, not `.env`, and are changed at runtime:

```sh
curl -s $A -X PUT $R/api/settings -d '{
  "nas_host":"192.168.1.20","nas_user":"relay",
  "allowed_dest_roots":"/volume1/media,/volume1/backups"}'
```

| Key | Default | Meaning |
|---|---|---|
| `nas_host` / `nas_user` / `nas_port` | — / — / `22` | How to reach the Synology |
| `nas_host_key` | captured | Pinned on first connect; clear it to re-trust a host |
| `nas_tmp` | `/tmp` | Where per-job workspaces are created on the NAS |
| `lftp_path` | probed | Absolute path of `lftp`, resolved at startup |
| `sshpass_path` | probed | Absolute path of `sshpass`; only needed for password-authenticated remote servers |
| `allowed_dest_roots` | empty | Comma or newline separated. **Empty means no destination is valid** — nothing can be written until you set this. |
| `concurrency` | `2` | Concurrent jobs |
| `segments` | `4` | lftp pget segments per file |
| `parallel` | `2` | Files transferred in parallel by a `mirror` |
| `history_retention_days` | `90` | Job history sweep |

## Destination safety

Every NAS path — browse, mkdir and later every job destination — goes through one
validator. It normalises unicode, requires an absolute path, cleans `..`, checks the
result is inside an allowed root, resolves symlinks on the NAS and checks again. A
symlink pointing out of a root is rejected, and `/volume1/mediafoo` is not treated as
inside `/volume1/media`. With no roots configured it rejects everything.

Host keys, for remote servers and the NAS alike, are captured on first connect and
pinned. A changed key is a hard error with instructions, never a silent accept.

## Backup and recovery

Everything durable is `/data/app.db` (plus its `-wal`/`-shm` siblings). Back it up
with the container stopped, or with `sqlite3 app.db ".backup out.db"` while running.
Losing it loses server definitions and job history only — re-add the servers and the
app rebuilds the rest. On an empty volume the schema is created automatically at
startup, and migrations are idempotent, so restarts are safe.

## Accepted risks (deliberate, LAN-only tool)

- **Remote server credentials are stored in plaintext** in SQLite. Anyone with the
  volume has them. There is no encryption at rest.
- **No TLS.** Basic auth over plain HTTP on the LAN is the entire auth model.
- **Single user.** No accounts, roles or sessions.

If any of that is unacceptable for your network, this is not the tool for you.

## NAS prerequisites

1. **SSH access.** Enable SSH on the Synology, then put the Pi's public key in the
   NAS user's `~/.ssh/authorized_keys`. The matching private key is bind-mounted
   read-only into the container at `NAS_SSH_KEY_PATH`. Synology is strict about
   permissions: `chmod 700 ~/.ssh`, `chmod 600 ~/.ssh/authorized_keys`, and the home
   directory itself must not be group-writable, or `sshd` silently refuses the key.
2. **lftp.** Install it with Entware (`opkg install lftp`) or a community package.
   The relay probes for it at startup — `command -v lftp` first, then the usual
   locations (`/opt/bin`, `/usr/local/bin`, `/usr/bin`, `/volume1/@entware/opt/bin`) —
   and caches the absolute path, because non-interactive SSH on Synology has a very
   thin `PATH`. If it is not found, the log says so with the install command.
3. **sshpass — only for password-authenticated remote servers.** `lftp` drives
   `sftp://` by shelling out to `ssh`, and `ssh` cannot be given a password
   non-interactively. If any remote server uses password authentication, install
   `sshpass` on the NAS (`opkg install sshpass`); the relay probes for it the same
   way it probes for `lftp`. Key authentication needs none of this and is preferred.
4. **Allowed roots.** Set `allowed_dest_roots` before anything can be written. Until
   then every destination is rejected, deliberately.
5. **Ownership.** Files land on the NAS owned by the SSH user you configured. If that
   is not the user your media apps run as, fix it there rather than in the relay.

## End-to-end tests

`test/e2e/` runs the whole thing in containers — a seeded SFTP source, a fake Synology
with `sshd`, `lftp` and `sshpass`, and the shipped relay image — and drives the real UI
with Playwright. No mocked API, and every transfer scenario ends in a `sha256sum` inside
the fake NAS.

```sh
cd test/e2e
npm ci
npx playwright install --with-deps chromium
docker compose up -d --build       # builds the relay image from this repo
npx playwright test
docker compose down -v             # between runs: the resume and cleanup specs are stateful
```

Set `E2E_PORT=8089` (in both commands) to run beside a dev stack already using 8088.
Keys and the 200 MB fixture are generated on first `up` and are gitignored — nothing
under `test/e2e/keys/` is ever committed. `retries: 0` is deliberate: a flake here is a
real race in `internal/jobs`.

The same suite runs in CI as the `e2e` job, gated on the Go and web jobs passing.
