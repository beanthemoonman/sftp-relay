package db

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func open(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.db")
	d, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d, path
}

func TestOpenAppliesMigrationsAndSeedsDefaults(t *testing.T) {
	d, _ := open(t)
	ctx := context.Background()

	var mode string
	if err := d.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}

	got, err := d.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"concurrency": "2", "segments": "4", "history_retention_days": "90", "allowed_dest_roots": "",
	} {
		if got[k] != want {
			t.Errorf("setting %s = %q, want %q", k, got[k], want)
		}
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	d, path := open(t)
	ctx := context.Background()
	if err := d.SetSettings(ctx, map[string]string{"concurrency": "7"}); err != nil {
		t.Fatal(err)
	}
	var first int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if first == 0 {
		t.Fatal("no migrations were applied")
	}
	d.Close()

	again, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()

	s, err := again.Settings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s["concurrency"] != "7" {
		t.Errorf("concurrency = %q after restart, want 7 (migration re-ran?)", s["concurrency"])
	}
	var applied int
	if err := again.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != first {
		t.Errorf("schema_migrations rows = %d after restart, want %d (a migration re-ran)", applied, first)
	}
}

func TestServerAndJobRoundTrip(t *testing.T) {
	d, _ := open(t)
	ctx := context.Background()

	id, err := d.CreateServer(ctx, Server{
		Name: "nas-source", Host: "example.test", Port: 22, Username: "u",
		AuthType: "password", Password: "s3cret", DefaultRemotePath: "/",
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := d.GetServer(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if s.Host != "example.test" || s.Password != "s3cret" {
		t.Fatalf("round trip mismatch: %+v", s)
	}

	s.Host = "moved.test"
	if err := d.UpdateServer(ctx, s); err != nil {
		t.Fatal(err)
	}
	if s, _ = d.GetServer(ctx, id); s.Host != "moved.test" {
		t.Errorf("host = %q after update", s.Host)
	}

	jobID, err := d.CreateJob(ctx, Job{
		ServerID: id, Kind: "file", RemotePath: "/a/b.iso", DestPath: "/volume1/media",
		Status: "queued", TotalBytes: 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetJobStatus(ctx, jobID, "running", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := d.SetJobProgress(ctx, jobID, 512, 100, 5); err != nil {
		t.Fatal(err)
	}
	j, err := d.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if j.ServerName != "nas-source" || j.TransferredBytes != 512 || !j.StartedAt.Valid {
		t.Fatalf("job round trip mismatch: %+v", j)
	}

	running, err := d.ListJobs(ctx, "running", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 {
		t.Errorf("running jobs = %d, want 1", len(running))
	}
	if done, _ := d.ListJobs(ctx, "done", 10, 0); len(done) != 0 {
		t.Errorf("done jobs = %d, want 0", len(done))
	}

	// Deleting the server cascades to its jobs.
	if err := d.DeleteServer(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetJob(ctx, jobID); !errors.Is(err, ErrNotFound) {
		t.Errorf("job after server delete: %v, want ErrNotFound", err)
	}
}

func TestMissingRowsReportNotFound(t *testing.T) {
	d, _ := open(t)
	ctx := context.Background()
	if _, err := d.GetServer(ctx, 404); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetServer: %v", err)
	}
	if err := d.DeleteServer(ctx, 404); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteServer: %v", err)
	}
	if err := d.SetJobStatus(ctx, 404, "done", "", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetJobStatus: %v", err)
	}
}

func TestJobLogIsRingBuffered(t *testing.T) {
	d, _ := open(t)
	ctx := context.Background()
	sid, err := d.CreateServer(ctx, Server{Name: "s", Host: "h", Port: 22, Username: "u", AuthType: "key"})
	if err != nil {
		t.Fatal(err)
	}
	jid, err := d.CreateJob(ctx, Job{ServerID: sid, Kind: "dir", RemotePath: "/a", DestPath: "/b", Status: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"one", "two", "three", "four"} {
		if err := d.AppendJobLog(ctx, jid, line, 2); err != nil {
			t.Fatal(err)
		}
	}
	lines, err := d.JobLog(ctx, jid)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "three" || lines[1] != "four" {
		t.Errorf("log = %v, want [three four]", lines)
	}
}

func TestConcurrentWrites(t *testing.T) {
	d, _ := open(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d.CreateServer(ctx, Server{
				Name: string(rune('a' + i)), Host: "h", Port: 22, Username: "u", AuthType: "key",
			}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	servers, err := d.ListServers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 20 {
		t.Errorf("servers = %d, want 20", len(servers))
	}
}

// TestClosedDBErrors covers the failure branch of every query helper at once.
func TestClosedDBErrors(t *testing.T) {
	d, _ := open(t)
	ctx := context.Background()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	calls := map[string]func() error{
		"ListServers":    func() error { _, err := d.ListServers(ctx); return err },
		"GetServer":      func() error { _, err := d.GetServer(ctx, 1); return err },
		"CreateServer":   func() error { _, err := d.CreateServer(ctx, Server{AuthType: "key"}); return err },
		"UpdateServer":   func() error { return d.UpdateServer(ctx, Server{ID: 1, AuthType: "key"}) },
		"DeleteServer":   func() error { return d.DeleteServer(ctx, 1) },
		"ListJobs":       func() error { _, err := d.ListJobs(ctx, "", 10, 0); return err },
		"GetJob":         func() error { _, err := d.GetJob(ctx, 1); return err },
		"CreateJob":      func() error { _, err := d.CreateJob(ctx, Job{Kind: "file", Status: "queued"}); return err },
		"SetJobStatus":   func() error { return d.SetJobStatus(ctx, 1, "done", "", nil) },
		"SetJobProgress": func() error { return d.SetJobProgress(ctx, 1, 1, 1, 1) },
		"AppendJobLog":   func() error { return d.AppendJobLog(ctx, 1, "x", 10) },
		"JobLog":         func() error { _, err := d.JobLog(ctx, 1); return err },
		"Settings":       func() error { _, err := d.Settings(ctx); return err },
		"SetSettings":    func() error { return d.SetSettings(ctx, map[string]string{"a": "b"}) },
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s on a closed DB: want error, got nil", name)
		}
	}
}

func TestOpenRejectsUnusablePath(t *testing.T) {
	if _, err := Open(context.Background(), t.TempDir()); err == nil {
		t.Fatal("opening a directory as a database should fail")
	}
}

// seedQueue creates a server and one job per status, returning their ids.
func seedQueue(t *testing.T, d *DB, statuses ...string) (int64, []int64) {
	t.Helper()
	ctx := context.Background()
	sid, err := d.CreateServer(ctx, Server{
		Name: "src", Host: "h", Port: 22, Username: "u", AuthType: "key",
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 0, len(statuses))
	for _, status := range statuses {
		id, err := d.CreateJob(ctx, Job{
			ServerID: sid, Kind: "file", RemotePath: "/a", DestPath: "/b", Status: status,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return sid, ids
}

func TestRunnableJobsIncludesInterruptedOnes(t *testing.T) {
	d, _ := open(t)
	ctx := context.Background()
	_, ids := seedQueue(t, d, "done", "queued", "running", "interrupted", "cancelled", "queued")

	got, err := d.RunnableJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{ids[1], ids[3], ids[5]}
	if len(got) != len(want) {
		t.Fatalf("runnable = %d jobs, want %d: %+v", len(got), len(want), got)
	}
	for i, j := range got {
		if j.ID != want[i] {
			t.Errorf("position %d = job %d, want %d (oldest first)", i, j.ID, want[i])
		}
		if j.ServerName != "src" {
			t.Errorf("job %d has no joined server name", j.ID)
		}
	}
	if limited, err := d.RunnableJobs(ctx, 1); err != nil || len(limited) != 1 {
		t.Errorf("limit 1 returned %d jobs, err = %v", len(limited), err)
	}
	if none, err := d.RunnableJobs(ctx, 0); err != nil || len(none) != 0 {
		t.Errorf("limit 0 returned %d jobs, err = %v", len(none), err)
	}
}

func TestInterruptRunningJobs(t *testing.T) {
	d, _ := open(t)
	ctx := context.Background()
	_, ids := seedQueue(t, d, "running", "running", "queued", "done")

	n, err := d.InterruptRunningJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("interrupted %d jobs, want 2", n)
	}
	for _, id := range ids[:2] {
		j, err := d.GetJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if j.Status != "interrupted" || j.Error == "" {
			t.Errorf("job %d = %q / %q", id, j.Status, j.Error)
		}
	}
	if j, _ := d.GetJob(ctx, ids[2]); j.Status != "queued" {
		t.Errorf("a queued job was disturbed: %q", j.Status)
	}
	// Running it again on a clean queue changes nothing.
	if n, err := d.InterruptRunningJobs(ctx); err != nil || n != 0 {
		t.Errorf("second sweep: %d, %v", n, err)
	}
}

func TestDeleteJobAndPurge(t *testing.T) {
	d, _ := open(t)
	ctx := context.Background()
	_, ids := seedQueue(t, d, "done", "failed", "cancelled", "queued", "running")

	if err := d.DeleteJob(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetJob(ctx, ids[0]); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted job still present: %v", err)
	}
	if err := d.DeleteJob(ctx, 4040); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteJob on a missing row: %v", err)
	}

	// Nothing is old enough yet.
	if n, err := d.PurgeJobs(ctx, 30); err != nil || n != 0 {
		t.Fatalf("premature purge removed %d rows, err = %v", n, err)
	}
	if _, err := d.ExecContext(ctx,
		`UPDATE jobs SET created_at = datetime('now', '-90 days')`); err != nil {
		t.Fatal(err)
	}
	n, err := d.PurgeJobs(ctx, 30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("purged %d rows, want the two remaining finished ones", n)
	}
	for _, id := range ids[3:] {
		if _, err := d.GetJob(ctx, id); err != nil {
			t.Errorf("live job %d was purged: %v", id, err)
		}
	}
}

func TestSetJobTotal(t *testing.T) {
	d, _ := open(t)
	ctx := context.Background()
	_, ids := seedQueue(t, d, "queued")

	if err := d.SetJobTotal(ctx, ids[0], 4096); err != nil {
		t.Fatal(err)
	}
	if j, _ := d.GetJob(ctx, ids[0]); j.TotalBytes != 4096 {
		t.Errorf("total_bytes = %d, want 4096", j.TotalBytes)
	}
	if err := d.SetJobTotal(ctx, 4040, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetJobTotal on a missing row: %v", err)
	}
}

func TestListJobsPagesByCursor(t *testing.T) {
	d, _ := open(t)
	ctx := context.Background()
	_, ids := seedQueue(t, d, "queued", "queued", "queued", "queued")

	first, err := d.ListJobs(ctx, "", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].ID != ids[3] || first[1].ID != ids[2] {
		t.Fatalf("first page = %+v", first)
	}
	second, err := d.ListJobs(ctx, "", 2, first[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || second[0].ID != ids[1] {
		t.Fatalf("second page = %+v", second)
	}
	if last, _ := d.ListJobs(ctx, "", 2, second[1].ID); len(last) != 0 {
		t.Errorf("page past the end = %+v", last)
	}
}
