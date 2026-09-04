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
	if applied != 1 {
		t.Errorf("schema_migrations rows = %d, want 1", applied)
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

	running, err := d.ListJobs(ctx, "running", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 {
		t.Errorf("running jobs = %d, want 1", len(running))
	}
	if done, _ := d.ListJobs(ctx, "done", 10); len(done) != 0 {
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
		"ListJobs":       func() error { _, err := d.ListJobs(ctx, "", 10); return err },
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
