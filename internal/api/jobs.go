package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"sftp-relay/internal/db"
	"sftp-relay/internal/events"
	"sftp-relay/internal/jobs"
	"sftp-relay/internal/sftpclient"
)

// snapshotLimit caps the resync payload a reconnecting client receives.
const snapshotLimit = 100

// heartbeat keeps idle proxies and phone radios from dropping the stream.
const heartbeat = 20 * time.Second

func (a *API) listJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := intParam(q.Get("limit"), 50)
	if limit < 1 || limit > 500 {
		limit = 50
	}
	cursor := int64(intParam(q.Get("cursor"), 0))
	rows, err := a.store.ListJobs(r.Context(), q.Get("status"), limit, cursor)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobs":        viewsOf(rows),
		"next_cursor": nextCursor(rows, limit),
	})
}

func viewsOf(rows []db.Job) []jobs.View {
	out := make([]jobs.View, 0, len(rows))
	for _, j := range rows {
		out = append(out, jobs.ViewOf(j))
	}
	return out
}

// nextCursor is the id to pass back for the following page, or 0 at the end.
func nextCursor(rows []db.Job, limit int) int64 {
	if len(rows) < limit || len(rows) == 0 {
		return 0
	}
	return rows[len(rows)-1].ID
}

// jobsInput is the batch shape: one server, many items, one destination.
type jobsInput struct {
	ServerID int64  `json:"server_id"`
	DestPath string `json:"dest_path"`
	Items    []struct {
		Path  string `json:"path"`
		IsDir bool   `json:"is_dir"`
	} `json:"items"`
}

func (a *API) createJobs(w http.ResponseWriter, r *http.Request) {
	in, ok := decode[jobsInput](w, r)
	if !ok {
		return
	}
	if in.ServerID == 0 || len(in.Items) == 0 {
		writeStatus(w, http.StatusBadRequest,
			errors.New("server_id and at least one item are required"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), opTimeout)
	defer cancel()

	if _, err := a.store.GetServer(ctx, in.ServerID); err != nil {
		writeErr(w, err)
		return
	}
	// Every destination goes through the one validator, without exception.
	dest, err := a.nas.Validate(ctx, in.DestPath)
	if err != nil {
		writeErr(w, err)
		return
	}

	created := make([]jobs.View, 0, len(in.Items))
	for _, item := range in.Items {
		remote := sftpclient.CleanPath(item.Path)
		if remote == "/" {
			writeStatus(w, http.StatusBadRequest,
				errors.New("the remote root is not a valid transfer source"))
			return
		}
		kind := "file"
		if item.IsDir {
			kind = "dir"
		}
		id, err := a.store.CreateJob(ctx, db.Job{
			ServerID: in.ServerID, Kind: kind, RemotePath: remote,
			DestPath: dest, Status: jobs.StatusQueued,
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		row, err := a.store.GetJob(ctx, id)
		if err != nil {
			writeErr(w, err)
			return
		}
		a.jobs.Announce(ctx, id)
		created = append(created, jobs.ViewOf(row))
	}
	slog.Info("jobs queued", "count", len(created), "server_id", in.ServerID, "dest", dest)
	writeJSON(w, http.StatusCreated, map[string]any{"jobs": created})
}

func (a *API) getJob(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	row, err := a.store.GetJob(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jobs.ViewOf(row))
}

func (a *API) deleteJob(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	row, err := a.store.GetJob(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !jobs.Terminal(row.Status) {
		writeStatus(w, http.StatusConflict,
			fmt.Errorf("job %d is %s: cancel it before deleting it", id, row.Status))
		return
	}
	if err := a.store.DeleteJob(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) cancelJob(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), opTimeout)
	defer cancel()
	if err := a.jobs.Cancel(ctx, id); err != nil {
		writeErr(w, err)
		return
	}
	row, err := a.store.GetJob(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, jobs.ViewOf(row))
}

func (a *API) retryJob(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	newID, err := a.jobs.Retry(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	row, err := a.store.GetJob(r.Context(), newID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, jobs.ViewOf(row))
}

func (a *API) jobLog(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	if _, err := a.store.GetJob(r.Context(), id); err != nil {
		writeErr(w, err)
		return
	}
	lines, err := a.store.JobLog(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	if lines == nil {
		lines = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

// events is the single SSE stream. Nothing is replayed on reconnect: the client
// gets a full snapshot instead, which is both simpler and always correct.
func (a *API) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeStatus(w, http.StatusInternalServerError,
			errors.New("this server cannot stream responses"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// nginx buffers proxied responses by default, which would hold every event
	// until the job ended. Both this and proxy_buffering off are needed.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	id, ch := a.hub.Subscribe()
	defer a.hub.Unsubscribe(id)

	rows, err := a.store.ListJobs(r.Context(), "", snapshotLimit, 0)
	if err != nil {
		slog.Error("events: building the snapshot", "err", err)
		return
	}
	var seq int64
	if !writeEvent(w, &seq, events.Event{Type: events.TypeSnapshot, Data: viewsOf(rows)}) {
		return
	}
	flusher.Flush()

	ping := time.NewTicker(heartbeat)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e, open := <-ch:
			if !open {
				return // the hub dropped us for falling behind
			}
			if !writeEvent(w, &seq, e) {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeEvent renders one SSE frame. It reports false once the client is gone.
func writeEvent(w http.ResponseWriter, seq *int64, e events.Event) bool {
	body, err := json.Marshal(e)
	if err != nil {
		slog.Error("events: encoding an event", "type", e.Type, "err", err)
		return true
	}
	*seq++
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", *seq, e.Type, body)
	return err == nil
}

func intParam(v string, fallback int) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}
