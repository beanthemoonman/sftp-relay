# sftp-relay

Browse remote SFTP servers from a phone and pull files down onto a Synology NAS.
Runs as one Docker container on a Raspberry Pi. Bytes never pass through the Pi —
the NAS does the transfer with `lftp`, the Pi only sends control commands.

Architecture, conventions and the security posture live in [CLAUDE.md](CLAUDE.md);
the build order lives in [plan_to_implement.md](plan_to_implement.md).

## Status

Phases 0–4 are done: the container runs, persists to SQLite, requires basic auth,
manages SFTP servers and browses them, and browses the NAS destination tree with
an allow-listed path validator. Transfers themselves (Phase 5 onward) are not
built yet, and there is no UI — everything below is driven with `curl`.

## Running it

```sh
cp deploy/.env.example deploy/.env   # then edit it
docker compose -f deploy/docker-compose.yml up -d --build
curl http://<pi>:8088/api/health     # {"status":"ok"}
```

The image targets `linux/arm64`; build it on the Pi, or with
`docker buildx build --platform linux/arm64`. For a local x86 test:
`docker build -f deploy/Dockerfile --build-arg TARGETARCH=amd64 -t sftp-relay .`

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
| `allowed_dest_roots` | empty | Comma or newline separated. **Empty means no destination is valid** — nothing can be written until you set this. |
| `concurrency` | `2` | Concurrent jobs |
| `segments` | `4` | lftp segments per transfer |
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
3. **Allowed roots.** Set `allowed_dest_roots` before anything can be written. Until
   then every destination is rejected, deliberately.
4. **Ownership.** Files land on the NAS owned by the SSH user you configured. If that
   is not the user your media apps run as, fix it there rather than in the relay.
