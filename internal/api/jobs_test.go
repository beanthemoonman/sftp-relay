package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sftp-relay/internal/db"
	"sftp-relay/internal/events"
	"sftp-relay/internal/jobs"
)

func seedServer(t *testing.T, store *db.DB) int64 {
	t.Helper()
	id, err := store.CreateServer(context.Background(), db.Server{
		Name: "src", Host: "files.example", Port: 22, Username: "u",
		AuthType: "key", PrivateKey: "k", HostKey: "hk",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func seedJob(t *testing.T, store *db.DB, serverID int64, status string) int64 {
	t.Helper()
	id, err := store.CreateJob(context.Background(), db.Job{
		ServerID: serverID, Kind: "file", RemotePath: "/pub/f",
		DestPath: "/volume1/media", Status: status, TotalBytes: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func decodeBody[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %s: %v", w.Body.String(), err)
	}
	return v
}

func TestListJobsFiltersAndPages(t *testing.T) {
	h, store := newTestAPI(t)
	server := seedServer(t, store)
	for range 3 {
		seedJob(t, store, server, jobs.StatusQueued)
	}
	seedJob(t, store, server, jobs.StatusDone)

	type page struct {
		Jobs       []jobs.View `json:"jobs"`
		NextCursor int64       `json:"next_cursor"`
	}

	w := do(t, h, http.MethodGet, "/api/jobs", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", w.Code, w.Body)
	}
	all := decodeBody[page](t, w)
	if len(all.Jobs) != 4 {
		t.Fatalf("got %d jobs, want 4", len(all.Jobs))
	}
	if all.Jobs[0].ID <= all.Jobs[1].ID {
		t.Error("jobs should come back newest first")
	}
	if all.NextCursor != 0 {
		t.Errorf("next_cursor = %d, want 0 on the last page", all.NextCursor)
	}
	if all.Jobs[0].ServerName != "src" {
		t.Errorf("server_name = %q, want the joined name", all.Jobs[0].ServerName)
	}

	queued := decodeBody[page](t, do(t, h, http.MethodGet, "/api/jobs?status=queued", "", true))
	if len(queued.Jobs) != 3 {
		t.Errorf("status filter returned %d jobs, want 3", len(queued.Jobs))
	}

	first := decodeBody[page](t, do(t, h, http.MethodGet, "/api/jobs?limit=2", "", true))
	if len(first.Jobs) != 2 || first.NextCursor != first.Jobs[1].ID {
		t.Fatalf("first page = %+v", first)
	}
	next := decodeBody[page](t, do(t, h, http.MethodGet,
		fmt.Sprintf("/api/jobs?limit=2&cursor=%d", first.NextCursor), "", true))
	if len(next.Jobs) != 2 || next.Jobs[0].ID >= first.Jobs[1].ID {
		t.Errorf("second page = %+v", next)
	}

	// Nonsense paging values fall back to the default rather than erroring.
	if w := do(t, h, http.MethodGet, "/api/jobs?limit=abc&cursor=abc", "", true); w.Code != http.StatusOK {
		t.Errorf("bad paging params = %d, want 200", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/api/jobs?limit=99999", "", true); w.Code != http.StatusOK {
		t.Errorf("oversized limit = %d, want 200", w.Code)
	}
}

func TestGetJob(t *testing.T) {
	h, store := newTestAPI(t)
	id := seedJob(t, store, seedServer(t, store), jobs.StatusQueued)

	w := do(t, h, http.MethodGet, fmt.Sprintf("/api/jobs/%d", id), "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", w.Code, w.Body)
	}
	if got := decodeBody[jobs.View](t, w); got.ID != id || got.Status != jobs.StatusQueued {
		t.Errorf("job = %+v", got)
	}
	if w := do(t, h, http.MethodGet, "/api/jobs/4040", "", true); w.Code != http.StatusNotFound {
		t.Errorf("missing job = %d, want 404", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/api/jobs/abc", "", true); w.Code != http.StatusBadRequest {
		t.Errorf("bad id = %d, want 400", w.Code)
	}
}

func TestDeleteJobOnlyWhenFinished(t *testing.T) {
	h, store := newTestAPI(t)
	server := seedServer(t, store)
	live := seedJob(t, store, server, jobs.StatusQueued)
	finished := seedJob(t, store, server, jobs.StatusDone)

	if w := do(t, h, http.MethodDelete, fmt.Sprintf("/api/jobs/%d", live), "", true); w.Code != http.StatusConflict {
		t.Errorf("deleting a queued job = %d, want 409", w.Code)
	}
	if w := do(t, h, http.MethodDelete, fmt.Sprintf("/api/jobs/%d", finished), "", true); w.Code != http.StatusNoContent {
		t.Errorf("deleting a finished job = %d, want 204", w.Code)
	}
	if w := do(t, h, http.MethodDelete, "/api/jobs/4040", "", true); w.Code != http.StatusNotFound {
		t.Errorf("deleting a missing job = %d, want 404", w.Code)
	}
}

func TestCancelAndRetryThroughTheAPI(t *testing.T) {
	h, store := newTestAPI(t)
	server := seedServer(t, store)
	id := seedJob(t, store, server, jobs.StatusQueued)

	w := do(t, h, http.MethodPost, fmt.Sprintf("/api/jobs/%d/cancel", id), "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("cancel = %d: %s", w.Code, w.Body)
	}
	if got := decodeBody[jobs.View](t, w); got.Status != jobs.StatusCancelled {
		t.Errorf("status after cancel = %q", got.Status)
	}
	// Cancelling twice is a conflict, not a silent success.
	if w := do(t, h, http.MethodPost, fmt.Sprintf("/api/jobs/%d/cancel", id), "", true); w.Code != http.StatusConflict {
		t.Errorf("second cancel = %d, want 409", w.Code)
	}

	w = do(t, h, http.MethodPost, fmt.Sprintf("/api/jobs/%d/retry", id), "", true)
	if w.Code != http.StatusCreated {
		t.Fatalf("retry = %d: %s", w.Code, w.Body)
	}
	clone := decodeBody[jobs.View](t, w)
	if clone.ID == id || clone.Status != jobs.StatusQueued {
		t.Errorf("clone = %+v", clone)
	}
	if w := do(t, h, http.MethodPost, fmt.Sprintf("/api/jobs/%d/retry", clone.ID), "", true); w.Code != http.StatusConflict {
		t.Errorf("retrying a queued job = %d, want 409", w.Code)
	}
	for _, target := range []string{"/api/jobs/4040/cancel", "/api/jobs/4040/retry"} {
		if w := do(t, h, http.MethodPost, target, "", true); w.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", target, w.Code)
		}
	}
}

func TestJobLog(t *testing.T) {
	h, store := newTestAPI(t)
	id := seedJob(t, store, seedServer(t, store), jobs.StatusRunning)

	w := do(t, h, http.MethodGet, fmt.Sprintf("/api/jobs/%d/log", id), "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("empty log = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"lines":[]`) {
		t.Errorf("empty log body = %s, want an empty array not null", w.Body)
	}

	if err := store.AppendJobLog(context.Background(), id, "connecting", 10); err != nil {
		t.Fatal(err)
	}
	got := decodeBody[struct {
		Lines []string `json:"lines"`
	}](t, do(t, h, http.MethodGet, fmt.Sprintf("/api/jobs/%d/log", id), "", true))
	if len(got.Lines) != 1 || got.Lines[0] != "connecting" {
		t.Errorf("log = %q", got.Lines)
	}
	if w := do(t, h, http.MethodGet, "/api/jobs/4040/log", "", true); w.Code != http.StatusNotFound {
		t.Errorf("log for a missing job = %d, want 404", w.Code)
	}
}

func TestCreateJobsValidatesInput(t *testing.T) {
	h, store := newTestAPI(t)
	server := seedServer(t, store)

	tests := []struct {
		name string
		body string
		want int
	}{
		{"not json", "{", http.StatusBadRequest},
		{"no items", `{"server_id":1,"dest_path":"/volume1/media","items":[]}`, http.StatusBadRequest},
		{"no server", `{"dest_path":"/x","items":[{"path":"/a"}]}`, http.StatusBadRequest},
		{
			name: "unknown server",
			body: `{"server_id":999,"dest_path":"/volume1/media","items":[{"path":"/a"}]}`,
			want: http.StatusNotFound,
		},
		{
			// No allowed roots are configured, so the validator fails closed.
			name: "destination outside the allow-list",
			body: fmt.Sprintf(`{"server_id":%d,"dest_path":"/etc","items":[{"path":"/a"}]}`, server),
			want: http.StatusBadRequest,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if w := do(t, h, http.MethodPost, "/api/jobs", tc.body, true); w.Code != tc.want {
				t.Errorf("got %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}
}

func TestEventsStreamsSnapshotThenDeltas(t *testing.T) {
	h, store, hub := newTestAPIFull(t)
	id := seedJob(t, store, seedServer(t, store), jobs.StatusQueued)

	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(user, pass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	// Without this nginx would hold every event until the job ended.
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}

	sc := bufio.NewScanner(resp.Body)
	readFrame := func() (event, data string) {
		t.Helper()
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "" && event != "":
				return event, data
			}
		}
		t.Fatalf("stream ended early: %v", sc.Err())
		return "", ""
	}

	typ, data := readFrame()
	if typ != events.TypeSnapshot {
		t.Fatalf("first frame = %q, want a snapshot", typ)
	}
	if !strings.Contains(data, fmt.Sprintf(`"id":%d`, id)) {
		t.Errorf("snapshot does not contain the queued job: %s", data)
	}

	hub.Publish(events.Event{Type: events.TypeLog, JobID: id, Data: "hello"})
	typ, data = readFrame()
	if typ != events.TypeLog || !strings.Contains(data, "hello") {
		t.Errorf("delta frame = %q / %s", typ, data)
	}

	cancel()
}

func TestNextCursor(t *testing.T) {
	rows := []db.Job{{ID: 9}, {ID: 8}}
	tests := []struct {
		name  string
		rows  []db.Job
		limit int
		want  int64
	}{
		{"full page", rows, 2, 8},
		{"partial page", rows, 5, 0},
		{"empty page", nil, 5, 0},
	}
	for _, tc := range tests {
		if got := nextCursor(tc.rows, tc.limit); got != tc.want {
			t.Errorf("%s: nextCursor = %d, want %d", tc.name, got, tc.want)
		}
	}
}
