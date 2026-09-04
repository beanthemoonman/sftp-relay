# Definition of Done — sftp-relay

This applies at three levels. A change is not merged, a phase is not closed, and a
release is not cut until the relevant section passes in full. "Mostly done" is not a
state this document recognises.

---

## 1. Per change (every PR)

### Code

- [ ] Compiles clean: `go build ./...` and `go vet ./...` with zero output
- [ ] `golangci-lint run` passes (errcheck, staticcheck, gosec, ineffassign, revive)
- [ ] `gofmt -l .` returns nothing
- [ ] `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...` succeeds — if it needs
      CGO it does not ship
- [ ] Frontend: `tsc --noEmit` clean, `eslint` clean, `vite build` succeeds
- [ ] No `TODO`, `FIXME`, commented-out code, or stray `fmt.Println` / `console.log`
- [ ] Every error crossing a package boundary is wrapped with context
- [ ] Every SSH/SFTP/DB call takes a `context.Context` with a deadline
- [ ] SQL uses explicit `JOIN` syntax

### Unit tests — mandatory, no exceptions

- [ ] New or changed logic has table-driven unit tests covering the happy path, at
      least two failure paths, and boundary conditions
- [ ] Overall line coverage ≥ 75% for `internal/...`
- [ ] These four packages are held to ≥ 90% and a PR that drops them below is blocked:
  - `internal/nas` — **path validator**. Must include: `..` traversal, absolute paths,
    symlinks pointing outside an allowed root, unicode normalisation, trailing
    slashes, empty string, root itself, a path that is a prefix-string match but not a
    real child (`/volume1/mediafoo` vs allowed `/volume1/media`)
  - `internal/nas` — **lftp output parser**. Must run against captured fixture output
    from at least two lftp versions, and must include: partial lines, interleaved
    stderr, non-UTF8 bytes, a directory mirror with per-file progress, zero-byte
    files, and output that yields no parseable progress at all
  - `internal/jobs` — **state machine**. Every legal transition asserted, and every
    illegal transition asserted to be rejected
  - `internal/events` — **SSE hub**. Subscriber add/remove under concurrency, slow
    subscriber dropped rather than buffering unboundedly, coalescing to ≤ 1 progress
    event per job per second
- [ ] Concurrency-touching tests run under `-race`; the full suite passes with
      `go test -race ./...`
- [ ] Tests are deterministic: no `time.Sleep` as synchronisation, no reliance on
      wall-clock ordering, no network access. Three consecutive runs pass.
- [ ] Frontend: unit tests (Vitest + Testing Library) for the SSE hook's reconnect
      and resync behaviour, path/size formatting helpers, and the selection-bar
      aggregation logic

### Integration tests

- [ ] DB layer tested against a real temp SQLite file, not a mock — including
      migration apply, re-apply idempotency, and WAL behaviour under concurrent writes
- [ ] SFTP browsing tested against a real containerised SFTP server, not a stub
- [ ] API handlers tested through `httptest` with the auth middleware in place,
      asserting 401 on missing and wrong credentials

### Review and hygiene

- [ ] Reviewed by a human who ran it, not just read it
- [ ] Commit messages describe why, not what
- [ ] No credential, key, or `.env` content in the diff; secret scan clean

---

## 2. End-to-end tests — mandatory before any phase closes

E2E runs against a docker-compose stack that reproduces the real topology. The Pi/NAS
split is just SSH, so it is fully reproducible in CI:

- `sftp-source` — a containerised SFTP server seeded with a known fixture tree
  (a small file, a 200 MB file, a nested directory, a file with spaces and unicode in
  the name, a zero-byte file)
- `fake-nas` — a container running `sshd` + `lftp` with a mounted destination volume,
  standing in for the Synology
- `relay` — the application image under test

Playwright drives the browser against the real UI. No mocked API.

### Mandatory E2E scenarios

- [ ] **Auth** — unauthenticated request is rejected; correct credentials grant access
- [ ] **Server CRUD** — add a server, test the connection, edit it, delete it
- [ ] **Browse** — navigate the remote tree three levels deep and back via breadcrumb;
      verify sizes and modified dates render; verify directories sort first
- [ ] **Destination browse** — navigate the NAS tree, create a folder, and confirm a
      path outside the allowed roots is rejected with a visible error
- [ ] **Single file download** — queue it, watch progress advance, confirm it reaches
      100%, then assert the file exists on the fake-NAS volume with a matching checksum
- [ ] **Directory download** — recursive mirror; assert the full tree arrives with
      matching checksums and correct nesting
- [ ] **Multi-select batch** — select five items, queue in one action, all complete
- [ ] **Concurrency** — queue more jobs than the configured worker count; assert only
      N run at once and the rest show a queue position
- [ ] **Cancel** — cancel mid-transfer; assert the job reports cancelled, no `lftp`
      process remains on the fake-NAS, and the temp workspace is gone
- [ ] **Resume after restart** — start a large transfer, restart the relay container
      mid-flight, assert the job is requeued as interrupted and **resumes** (partial
      bytes retained) rather than restarting from zero
- [ ] **Failure surfacing** — point a job at a nonexistent remote path; assert it fails
      with a readable error, the error is visible in the UI, and retry is offered
- [ ] **Unreachable NAS** — stop the fake-NAS mid-job; assert a clear error rather than
      a hang, and that the app recovers when it comes back
- [ ] **SSE resilience** — kill the event stream; assert the client reconnects with
      backoff, resyncs from the snapshot, and shows the disconnected indicator meanwhile
- [ ] **Multi-client** — two browser contexts see identical live progress
- [ ] **Mobile viewport** — the full browse-to-queue flow completes at 375px with no
      horizontal scroll and no overlapping controls
- [ ] **Cleanup** — after a full run, assert zero orphaned temp workspaces, zero
      orphaned processes, and zero leaked SSH connections on the fake-NAS

E2E must pass three consecutive runs. A flaky E2E test is a failing E2E test — quarantine
is not an option here, because the flake is usually a real race in the job system.

---

## 3. Non-functional gates

- [ ] **Memory:** RSS measured under five concurrent jobs stays under 60 MB. Recorded
      in the PR when the change touches jobs, SSE, or SFTP pooling.
- [ ] **No goroutine leaks:** `go.uber.org/goleak` in `TestMain` for
      `internal/jobs`, `internal/events`, `internal/sftpclient`, `internal/nas`
- [ ] **No connection leaks:** SFTP pool asserts idle eviction actually closes sockets
- [ ] **Image builds for `linux/arm64`** and the built image starts on the Pi
- [ ] **Cold start** on an empty volume creates the DB and serves the UI in under 5s
- [ ] Frontend bundle under 500 KB gzipped
- [ ] SSE verified to stream through nginx unbuffered — a job's progress updates arrive
      within 2s, not in one burst at the end

## 4. Security gates

- [ ] `gosec` clean, or every suppression individually justified in a comment
- [ ] `govulncheck ./...` clean; `npm audit` has no high or critical findings
- [ ] **Credential redaction test:** a test asserts that server passwords, private keys,
      and passphrases never appear in any log output at any level, including error paths
- [ ] **No credential on a command line:** a test asserts the generated remote command
      contains no password or key material — only file references
- [ ] Remote temp workspace is `0600`/`0700` and removed on every exit path, verified
      including the SIGKILL case
- [ ] Path validator is exercised by every handler that accepts a NAS path — no handler
      constructs a destination path without going through it
- [ ] Basic auth comparison is constant-time

## 5. Documentation

- [ ] `CLAUDE.md` updated if any architectural decision changed
- [ ] README covers: NAS prerequisites (installing lftp, resolving its path, SSH key
      setup, allowed roots), `.env` reference, backup of `/data/app.db`, and how to
      recover from a lost DB
- [ ] Any accepted risk (plaintext credentials, no TLS, LAN-only) is stated in the
      README, not just in internal docs
- [ ] New env vars added to `.env.example` with comments
- [ ] Migrations documented and reversible or explicitly marked one-way

## 6. Manual verification

Automation cannot catch everything here. Before a phase closes:

- [ ] Exercised against the **real** Synology and a **real** remote SFTP server, not
      only the fake-NAS
- [ ] Exercised from an actual phone on the actual LAN
- [ ] File ownership on the NAS after download is checked and documented
- [ ] A transfer over 1 GB completed successfully with accurate progress throughout

---

## 7. Phase-level Definition of Done

A phase in `plan_to_implement.md` is closed only when:

- [ ] Its stated exit criteria are demonstrated, not asserted
- [ ] All sections above pass for the phase's changes
- [ ] The phase's functionality has at least one E2E scenario from section 2
- [ ] The container is deployed to the Pi and left running for 24 hours without a
      restart, memory climb, or error-log accumulation

## 8. Release Definition of Done

- [ ] Every phase closed
- [ ] Full E2E suite green three consecutive runs
- [ ] Fresh install from an empty volume verified end to end
- [ ] Upgrade from the previous version verified with existing data and jobs in flight
- [ ] Rollback verified: previous image starts against the new DB, or the breakage is
      documented with a manual recovery procedure
- [ ] Version tagged; image tagged with both the version and `latest`

---

## 9. Explicitly not required

Stated so nobody gold-plates and nobody later thinks these were forgotten:

- TLS, certificate management, or HTTPS redirect — LAN only, by decision
- Multi-user accounts, RBAC, or session management — basic auth is the whole model
- CSRF tokens — no cookie-based auth exists to attack
- Encryption of credentials at rest — accepted risk, documented
- Internationalisation, accessibility beyond keyboard navigation and sane contrast,
  or browser support below current-generation Chrome, Safari and Firefox
- Horizontal scaling, clustering, or any multi-instance concern
