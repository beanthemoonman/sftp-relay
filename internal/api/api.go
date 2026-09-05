// Package api wires the HTTP surface: basic auth, server CRUD, remote browsing
// and NAS destination browsing.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"sftp-relay/internal/config"
	"sftp-relay/internal/db"
	"sftp-relay/internal/events"
	"sftp-relay/internal/jobs"
	"sftp-relay/internal/nas"
	"sftp-relay/internal/sftpclient"
)

// opTimeout bounds every SSH/SFTP-backed request. Browsing a cold server over a
// slow link is the worst case, hence 30s rather than something tighter.
const opTimeout = 30 * time.Second

// dbTimeout bounds a pure SQLite read/write. SSH/SFTP calls already use
// opTimeout; local DB work is fast but should still not hang a handler forever.
const dbTimeout = 10 * time.Second

// numericSettings are the settings that must be whole numbers. putSettings
// rejects a malformed value here rather than letting the read path silently
// fall back to a default, which would hide a typo like "nas_port":"twenty-two".
var numericSettings = map[string]bool{
	"concurrency": true, "segments": true, "parallel": true,
	"history_retention_days": true, "nas_port": true,
}

type API struct {
	store *db.DB
	pool  *sftpclient.Pool
	nas   *nas.Client
	jobs  *jobs.Manager
	hub   *events.Hub
	cfg   config.Config
}

// New returns the fully-routed handler. /api/health is the only unauthenticated
// route, so container healthchecks need no credentials.
func New(store *db.DB, pool *sftpclient.Pool, nasClient *nas.Client,
	manager *jobs.Manager, hub *events.Hub, cfg config.Config) http.Handler {
	a := &API{store: store, pool: pool, nas: nasClient, jobs: manager, hub: hub, cfg: cfg}

	r := chi.NewRouter()
	r.Get("/api/health", a.health)

	r.Group(func(r chi.Router) {
		r.Use(a.basicAuth)

		r.Get("/api/servers", a.listServers)
		r.Post("/api/servers", a.createServer)
		r.Put("/api/servers/{id}", a.updateServer)
		r.Delete("/api/servers/{id}", a.deleteServer)
		r.Post("/api/servers/{id}/test", a.testServer)
		r.Get("/api/servers/{id}/browse", a.browseServer)

		r.Get("/api/nas/browse", a.browseNAS)
		r.Post("/api/nas/mkdir", a.mkdirNAS)

		r.Get("/api/jobs", a.listJobs)
		r.Post("/api/jobs", a.createJobs)
		r.Get("/api/jobs/{id}", a.getJob)
		r.Delete("/api/jobs/{id}", a.deleteJob)
		r.Post("/api/jobs/{id}/cancel", a.cancelJob)
		r.Post("/api/jobs/{id}/retry", a.retryJob)
		r.Get("/api/jobs/{id}/log", a.jobLog)

		r.Get("/api/events", a.events)

		r.Get("/api/settings", a.getSettings)
		r.Put("/api/settings", a.putSettings)

		// The React bundle is the catch-all, behind the same auth as the API.
		// A build without it still serves the API; only the UI is missing.
		if static, err := staticHandler(); err != nil {
			slog.Error("static assets unavailable", "err", err)
		} else {
			r.Handle("/*", static)
		}
	})
	return r
}

// basicAuth guards everything except the health check, the embedded React
// bundle included.
func (a *API) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || !credsMatch(user, pass, a.cfg.AuthUser, a.cfg.AuthPass) {
			w.Header().Set("WWW-Authenticate", `Basic realm="sftp-relay", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// credsMatch compares in constant time. Hashing first keeps the comparison
// length-independent, so it leaks neither field's length.
func credsMatch(gotUser, gotPass, wantUser, wantPass string) bool {
	if wantUser == "" || wantPass == "" {
		return false // an unconfigured password must never authorise anything
	}
	gu, wu := sha256.Sum256([]byte(gotUser)), sha256.Sum256([]byte(wantUser))
	gp, wp := sha256.Sum256([]byte(gotPass)), sha256.Sum256([]byte(wantPass))
	userOK := subtle.ConstantTimeCompare(gu[:], wu[:])
	passOK := subtle.ConstantTimeCompare(gp[:], wp[:])
	return userOK&passOK == 1
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.store.PingContext(ctx); err != nil {
		slog.Error("health: database unreachable", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// serverView is the wire shape of a server. Credentials are write-only: they
// are stored in plaintext by design, but there is no reason to echo them back.
type serverView struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	Host              string `json:"host"`
	Port              int    `json:"port"`
	Username          string `json:"username"`
	AuthType          string `json:"auth_type"`
	HostKey           string `json:"host_key"`
	DefaultRemotePath string `json:"default_remote_path"`
	HasPassword       bool   `json:"has_password"`
	HasPrivateKey     bool   `json:"has_private_key"`
}

func viewOf(s db.Server) serverView {
	return serverView{
		ID: s.ID, Name: s.Name, Host: s.Host, Port: s.Port, Username: s.Username,
		AuthType: s.AuthType, HostKey: s.HostKey, DefaultRemotePath: s.DefaultRemotePath,
		HasPassword: s.Password != "", HasPrivateKey: s.PrivateKey != "",
	}
}

// serverInput is what the UI sends. Blank credential fields on an update mean
// "keep what is stored", so the form never has to round-trip a secret.
type serverInput struct {
	Name              string `json:"name"`
	Host              string `json:"host"`
	Port              int    `json:"port"`
	Username          string `json:"username"`
	AuthType          string `json:"auth_type"`
	Password          string `json:"password"`
	PrivateKey        string `json:"private_key"`
	Passphrase        string `json:"passphrase"`
	HostKey           string `json:"host_key"`
	DefaultRemotePath string `json:"default_remote_path"`
}

func (in serverInput) validate() error {
	switch {
	case in.Name == "":
		return errors.New("name is required")
	case in.Host == "":
		return errors.New("host is required")
	case in.Username == "":
		return errors.New("username is required")
	case in.AuthType != "password" && in.AuthType != "key":
		return fmt.Errorf("auth_type must be password or key, got %q", in.AuthType)
	case in.Port < 0 || in.Port > 65535:
		return fmt.Errorf("port %d is out of range", in.Port)
	}
	return nil
}

func (in serverInput) apply(s db.Server) db.Server {
	s.Name, s.Host, s.Username = in.Name, in.Host, in.Username
	s.AuthType, s.HostKey, s.DefaultRemotePath = in.AuthType, in.HostKey, in.DefaultRemotePath
	s.Port = in.Port
	if s.Port == 0 {
		s.Port = 22
	}
	if in.Password != "" {
		s.Password = in.Password
	}
	if in.PrivateKey != "" {
		s.PrivateKey = in.PrivateKey
	}
	if in.Passphrase != "" {
		s.Passphrase = in.Passphrase
	}
	return s
}

func (a *API) listServers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	servers, err := a.store.ListServers(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]serverView, 0, len(servers))
	for _, s := range servers {
		out = append(out, viewOf(s))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) createServer(w http.ResponseWriter, r *http.Request) {
	in, ok := decode[serverInput](w, r)
	if !ok {
		return
	}
	if err := in.validate(); err != nil {
		writeStatus(w, http.StatusBadRequest, err)
		return
	}
	s := in.apply(db.Server{})
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	id, err := a.store.CreateServer(ctx, s)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.ID = id
	slog.Info("server created", "server_id", id, "name", s.Name, "host", s.Host)
	writeJSON(w, http.StatusCreated, viewOf(s))
}

func (a *API) updateServer(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	in, ok := decode[serverInput](w, r)
	if !ok {
		return
	}
	if err := in.validate(); err != nil {
		writeStatus(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	existing, err := a.store.GetServer(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	updated := in.apply(existing)
	if err := a.store.UpdateServer(ctx, updated); err != nil {
		writeErr(w, err)
		return
	}
	slog.Info("server updated", "server_id", id, "name", updated.Name)
	writeJSON(w, http.StatusOK, viewOf(updated))
}

func (a *API) deleteServer(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	if err := a.store.DeleteServer(ctx, id); err != nil {
		writeErr(w, err)
		return
	}
	slog.Info("server deleted", "server_id", id)
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) testServer(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), opTimeout)
	defer cancel()
	s, err := a.store.GetServer(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	res, err := a.pool.Test(ctx, s)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "description": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *API) browseServer(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), opTimeout)
	defer cancel()
	s, err := a.store.GetServer(ctx, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = s.DefaultRemotePath
	}
	dir = sftpclient.CleanPath(dir)
	entries, err := a.pool.List(ctx, s, dir)
	if err != nil {
		writeErr(w, err)
		return
	}
	parent := ""
	if dir != "/" {
		parent = sftpclient.CleanPath(dir + "/..")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": dir, "parent": parent, "entries": entries,
	})
}

func (a *API) browseNAS(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), opTimeout)
	defer cancel()
	listing, err := a.nas.Browse(ctx, r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, listing)
}

func (a *API) mkdirNAS(w http.ResponseWriter, r *http.Request) {
	in, ok := decode[struct {
		Path string `json:"path"`
	}](w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), opTimeout)
	defer cancel()
	created, err := a.nas.Mkdir(ctx, in.Path)
	if err != nil {
		writeErr(w, err)
		return
	}
	slog.Info("nas directory created", "path", created)
	writeJSON(w, http.StatusCreated, map[string]string{"path": created})
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	s, err := a.store.Settings(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (a *API) putSettings(w http.ResponseWriter, r *http.Request) {
	in, ok := decode[map[string]string](w, r)
	if !ok {
		return
	}
	for k, v := range in {
		if numericSettings[k] && strings.TrimSpace(v) != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err != nil || n < 0 {
				writeStatus(w, http.StatusBadRequest,
					fmt.Errorf("setting %q must be a non-negative number, got %q", k, v))
				return
			}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), dbTimeout)
	defer cancel()
	if err := a.store.SetSettings(ctx, in); err != nil {
		writeErr(w, err)
		return
	}
	slog.Info("settings updated", "keys", len(in))
	a.getSettings(w, r)
}

func idParam(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeStatus(w, http.StatusBadRequest, fmt.Errorf("bad id: %w", err))
		return 0, false
	}
	return id, true
}

func decode[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&v); err != nil {
		writeStatus(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return v, false
	}
	return v, true
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("writing response", "err", err)
	}
}

func writeStatus(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// writeErr maps a domain error onto a status code. Error text is returned as-is
// because this is a single-user LAN tool and a vague error helps nobody; no
// error path here carries credential material.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrNotFound):
		writeStatus(w, http.StatusNotFound, err)
	case errors.Is(err, nas.ErrOutsideRoots), errors.Is(err, nas.ErrNoRoots):
		writeStatus(w, http.StatusBadRequest, err)
	case errors.Is(err, jobs.ErrIllegalTransition):
		writeStatus(w, http.StatusConflict, err)
	case errors.Is(err, nas.ErrNoHostKey), errors.Is(err, nas.ErrNoSSHPass):
		writeStatus(w, http.StatusBadRequest, err)
	case errors.Is(err, nas.ErrNotConfigured):
		writeStatus(w, http.StatusServiceUnavailable, err)
	case errors.Is(err, nas.ErrHostKeyChanged), errors.Is(err, sftpclient.ErrHostKeyChanged):
		writeStatus(w, http.StatusBadGateway, err)
	case errors.Is(err, context.DeadlineExceeded):
		writeStatus(w, http.StatusGatewayTimeout, err)
	default:
		slog.Error("request failed", "err", err)
		writeStatus(w, http.StatusInternalServerError, err)
	}
}
