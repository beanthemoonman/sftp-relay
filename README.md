# sftp-relay

Browse remote SFTP servers from a phone and pull files down onto a Synology NAS.
Runs as one Docker container on a Raspberry Pi. Bytes never pass through the Pi —
the NAS does the transfer with `lftp`, the Pi only sends control commands.

Architecture, conventions and the security posture live in [CLAUDE.md](CLAUDE.md);
the build order lives in [plan_to_implement.md](plan_to_implement.md).

## Status

Phases 0 (skeleton, container) and 1 (SQLite persistence) are done. The container
starts, creates its database and answers `/api/health`. Nothing else is wired yet.

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

Documented in full when Phase 4/5 land. Short version: `lftp` must be installed on
the Synology (Entware `opkg install lftp`), the Pi's public key must be in the NAS
user's `authorized_keys`, and destination roots must be allow-listed in Settings
before any job can run.
