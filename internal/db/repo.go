package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotFound is returned when a lookup by id matches no row.
var ErrNotFound = errors.New("db: not found")

type Server struct {
	ID                int64
	Name              string
	Host              string
	Port              int
	Username          string
	AuthType          string // password | key
	Password          string
	PrivateKey        string
	Passphrase        string
	HostKey           string
	DefaultRemotePath string
}

type Job struct {
	ID               int64
	ServerID         int64
	ServerName       string // joined from servers
	Kind             string // file | dir
	RemotePath       string
	DestPath         string
	Status           string
	TotalBytes       int64
	TransferredBytes int64
	SpeedBPS         int64
	ETASeconds       int64
	ExitCode         sql.NullInt64
	Error            string
	CreatedAt        string
	StartedAt        sql.NullString
	FinishedAt       sql.NullString
}

type scanner interface{ Scan(...any) error }

const serverCols = `id, name, host, port, username, auth_type, password, private_key,
	passphrase, host_key, default_remote_path`

func scanServer(row scanner) (Server, error) {
	var s Server
	err := row.Scan(&s.ID, &s.Name, &s.Host, &s.Port, &s.Username, &s.AuthType, &s.Password,
		&s.PrivateKey, &s.Passphrase, &s.HostKey, &s.DefaultRemotePath)
	return s, err
}

func (d *DB) ListServers(ctx context.Context) ([]Server, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+serverCols+` FROM servers ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("db: list servers: %w", err)
	}
	defer rows.Close()
	var out []Server
	for rows.Next() {
		s, err := scanServer(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan server: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list servers: %w", err)
	}
	return out, nil
}

func (d *DB) GetServer(ctx context.Context, id int64) (Server, error) {
	s, err := scanServer(d.QueryRowContext(ctx, `SELECT `+serverCols+` FROM servers WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Server{}, fmt.Errorf("db: server %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Server{}, fmt.Errorf("db: get server %d: %w", id, err)
	}
	return s, nil
}

func (d *DB) CreateServer(ctx context.Context, s Server) (int64, error) {
	res, err := d.ExecContext(ctx, `INSERT INTO servers
		(name, host, port, username, auth_type, password, private_key, passphrase, host_key, default_remote_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.Name, s.Host, s.Port, s.Username, s.AuthType, s.Password, s.PrivateKey,
		s.Passphrase, s.HostKey, s.DefaultRemotePath)
	if err != nil {
		return 0, fmt.Errorf("db: create server %q: %w", s.Name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("db: create server %q: %w", s.Name, err)
	}
	return id, nil
}

func (d *DB) UpdateServer(ctx context.Context, s Server) error {
	res, err := d.ExecContext(ctx, `UPDATE servers SET name = ?, host = ?, port = ?, username = ?,
		auth_type = ?, password = ?, private_key = ?, passphrase = ?, host_key = ?,
		default_remote_path = ?, updated_at = datetime('now') WHERE id = ?`,
		s.Name, s.Host, s.Port, s.Username, s.AuthType, s.Password, s.PrivateKey,
		s.Passphrase, s.HostKey, s.DefaultRemotePath, s.ID)
	if err != nil {
		return fmt.Errorf("db: update server %d: %w", s.ID, err)
	}
	return affected(res, fmt.Sprintf("server %d", s.ID))
}

func (d *DB) DeleteServer(ctx context.Context, id int64) error {
	res, err := d.ExecContext(ctx, `DELETE FROM servers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("db: delete server %d: %w", id, err)
	}
	return affected(res, fmt.Sprintf("server %d", id))
}

const jobCols = `j.id, j.server_id, s.name, j.kind, j.remote_path, j.dest_path, j.status,
	j.total_bytes, j.transferred_bytes, j.speed_bps, j.eta_seconds, j.exit_code, j.error,
	j.created_at, j.started_at, j.finished_at`

func scanJob(row scanner) (Job, error) {
	var j Job
	err := row.Scan(&j.ID, &j.ServerID, &j.ServerName, &j.Kind, &j.RemotePath, &j.DestPath,
		&j.Status, &j.TotalBytes, &j.TransferredBytes, &j.SpeedBPS, &j.ETASeconds,
		&j.ExitCode, &j.Error, &j.CreatedAt, &j.StartedAt, &j.FinishedAt)
	return j, err
}

// ListJobs returns jobs newest first; an empty status means all statuses.
func (d *DB) ListJobs(ctx context.Context, status string, limit int) ([]Job, error) {
	q := `SELECT ` + jobCols + ` FROM jobs AS j
		INNER JOIN servers AS s ON s.id = j.server_id
		WHERE (? = '' OR j.status = ?) ORDER BY j.id DESC LIMIT ?`
	rows, err := d.QueryContext(ctx, q, status, status, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list jobs: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan job: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: list jobs: %w", err)
	}
	return out, nil
}

func (d *DB) GetJob(ctx context.Context, id int64) (Job, error) {
	q := `SELECT ` + jobCols + ` FROM jobs AS j
		INNER JOIN servers AS s ON s.id = j.server_id WHERE j.id = ?`
	j, err := scanJob(d.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("db: job %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Job{}, fmt.Errorf("db: get job %d: %w", id, err)
	}
	return j, nil
}

func (d *DB) CreateJob(ctx context.Context, j Job) (int64, error) {
	res, err := d.ExecContext(ctx, `INSERT INTO jobs
		(server_id, kind, remote_path, dest_path, status, total_bytes)
		VALUES (?, ?, ?, ?, ?, ?)`,
		j.ServerID, j.Kind, j.RemotePath, j.DestPath, j.Status, j.TotalBytes)
	if err != nil {
		return 0, fmt.Errorf("db: create job for server %d: %w", j.ServerID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("db: create job for server %d: %w", j.ServerID, err)
	}
	return id, nil
}

// SetJobStatus moves a job to status, stamping started_at/finished_at as appropriate.
func (d *DB) SetJobStatus(ctx context.Context, id int64, status, errMsg string, exitCode *int) error {
	res, err := d.ExecContext(ctx, `UPDATE jobs SET status = ?, error = ?, exit_code = ?,
		started_at = CASE WHEN ? = 'running' THEN datetime('now') ELSE started_at END,
		finished_at = CASE WHEN ? IN ('done', 'failed', 'cancelled') THEN datetime('now') ELSE finished_at END
		WHERE id = ?`, status, errMsg, exitCode, status, status, id)
	if err != nil {
		return fmt.Errorf("db: set job %d status %s: %w", id, status, err)
	}
	return affected(res, fmt.Sprintf("job %d", id))
}

// SetJobProgress writes a progress snapshot. Callers flush at most once per second.
func (d *DB) SetJobProgress(ctx context.Context, id, transferred, speed, eta int64) error {
	res, err := d.ExecContext(ctx,
		`UPDATE jobs SET transferred_bytes = ?, speed_bps = ?, eta_seconds = ? WHERE id = ?`,
		transferred, speed, eta, id)
	if err != nil {
		return fmt.Errorf("db: set job %d progress: %w", id, err)
	}
	return affected(res, fmt.Sprintf("job %d", id))
}

// AppendJobLog adds a log line and trims the job's log to the newest keep lines.
func (d *DB) AppendJobLog(ctx context.Context, jobID int64, line string, keep int) error {
	if _, err := d.ExecContext(ctx,
		`INSERT INTO job_log (job_id, line) VALUES (?, ?)`, jobID, line); err != nil {
		return fmt.Errorf("db: append log for job %d: %w", jobID, err)
	}
	if _, err := d.ExecContext(ctx, `DELETE FROM job_log WHERE job_id = ? AND id NOT IN
		(SELECT id FROM job_log WHERE job_id = ? ORDER BY id DESC LIMIT ?)`,
		jobID, jobID, keep); err != nil {
		return fmt.Errorf("db: trim log for job %d: %w", jobID, err)
	}
	return nil
}

func (d *DB) JobLog(ctx context.Context, jobID int64) ([]string, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT line FROM job_log WHERE job_id = ? ORDER BY id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("db: job %d log: %w", jobID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, fmt.Errorf("db: scan job %d log: %w", jobID, err)
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: job %d log: %w", jobID, err)
	}
	return out, nil
}

func (d *DB) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("db: settings: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("db: scan setting: %w", err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: settings: %w", err)
	}
	return out, nil
}

func (d *DB) SetSettings(ctx context.Context, kv map[string]string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: set settings: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op
	for k, v := range kv {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return fmt.Errorf("db: set setting %q: %w", k, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: set settings: %w", err)
	}
	return nil
}

func affected(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("db: %s: %w", what, err)
	}
	if n == 0 {
		return fmt.Errorf("db: %s: %w", what, ErrNotFound)
	}
	return nil
}

// SetServerHostKey records the host key observed on first connect.
func (d *DB) SetServerHostKey(ctx context.Context, id int64, hostKey string) error {
	res, err := d.ExecContext(ctx,
		`UPDATE servers SET host_key = ?, updated_at = datetime('now') WHERE id = ?`, hostKey, id)
	if err != nil {
		return fmt.Errorf("db: set server %d host key: %w", id, err)
	}
	return affected(res, fmt.Sprintf("server %d", id))
}
