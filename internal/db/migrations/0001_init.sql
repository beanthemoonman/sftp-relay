CREATE TABLE servers (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    name                TEXT    NOT NULL,
    host                TEXT    NOT NULL,
    port                INTEGER NOT NULL DEFAULT 22,
    username            TEXT    NOT NULL,
    auth_type           TEXT    NOT NULL CHECK (auth_type IN ('password', 'key')),
    password            TEXT    NOT NULL DEFAULT '',
    private_key         TEXT    NOT NULL DEFAULT '',
    passphrase          TEXT    NOT NULL DEFAULT '',
    host_key            TEXT    NOT NULL DEFAULT '',
    default_remote_path TEXT    NOT NULL DEFAULT '/',
    created_at          TEXT    NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT    NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE jobs (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    server_id          INTEGER NOT NULL REFERENCES servers (id) ON DELETE CASCADE,
    kind               TEXT    NOT NULL CHECK (kind IN ('file', 'dir')),
    remote_path        TEXT    NOT NULL,
    dest_path          TEXT    NOT NULL,
    status             TEXT    NOT NULL CHECK (status IN
                                               ('queued', 'running', 'paused', 'done', 'failed', 'cancelled',
                                                'interrupted')),
    total_bytes        INTEGER NOT NULL DEFAULT 0,
    transferred_bytes  INTEGER NOT NULL DEFAULT 0,
    speed_bps          INTEGER NOT NULL DEFAULT 0,
    eta_seconds        INTEGER NOT NULL DEFAULT 0,
    exit_code          INTEGER,
    error              TEXT    NOT NULL DEFAULT '',
    created_at         TEXT    NOT NULL DEFAULT (datetime('now')),
    started_at         TEXT,
    finished_at        TEXT
);

CREATE INDEX idx_jobs_status ON jobs (status, id DESC);

CREATE TABLE job_log (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id INTEGER NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    ts     TEXT    NOT NULL DEFAULT (datetime('now')),
    line   TEXT    NOT NULL
);

CREATE INDEX idx_job_log_job ON job_log (job_id, id);

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

INSERT INTO settings (key, value)
VALUES ('concurrency', '2'),
       ('segments', '4'),
       ('history_retention_days', '90'),
       ('allowed_dest_roots', ''),
       ('nas_host', ''),
       ('nas_user', ''),
       ('nas_tmp', '/tmp'),
       ('lftp_path', '');
