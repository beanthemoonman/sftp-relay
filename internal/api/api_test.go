package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sftp-relay/internal/config"
	"sftp-relay/internal/db"
	"sftp-relay/internal/events"
	"sftp-relay/internal/jobs"
	"sftp-relay/internal/nas"
	"sftp-relay/internal/sftpclient"
)

const (
	user = "relay"
	pass = "hunter2"
)

func newTestAPI(t *testing.T) (http.Handler, *db.DB) {
	h, store, _ := newTestAPIFull(t)
	return h, store
}

// newTestAPIFull also hands back the event hub, for the SSE tests.
func newTestAPIFull(t *testing.T) (http.Handler, *db.DB, *events.Hub) {
	t.Helper()
	store, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	pool := sftpclient.NewPool(store, time.Minute, time.Second)
	nasClient := nas.New(store, filepath.Join(t.TempDir(), "no-such-key"), time.Second)
	t.Cleanup(func() {
		pool.Close()
		nasClient.Close()
		store.Close()
	})
	hub := events.NewHub()
	manager := jobs.New(store, pool, nasClient, hub)
	h := New(store, pool, nasClient, manager, hub, config.Config{AuthUser: user, AuthPass: pass})
	return h, store, hub
}

func do(t *testing.T, h http.Handler, method, target, body string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if auth {
		r.SetBasicAuth(user, pass)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHealthNeedsNoCredentials(t *testing.T) {
	h, _ := newTestAPI(t)
	w := do(t, h, http.MethodGet, "/api/health", "", false)
	if w.Code != http.StatusOK {
		t.Fatalf("health = %d, want 200", w.Code)
	}
	if got := w.Body.String(); !strings.Contains(got, `"ok"`) {
		t.Errorf("health body = %s", got)
	}
}

func TestAuthGuardsEveryOtherRoute(t *testing.T) {
	h, _ := newTestAPI(t)
	routes := []struct{ method, target string }{
		{http.MethodGet, "/api/servers"},
		{http.MethodPost, "/api/servers"},
		{http.MethodPut, "/api/servers/1"},
		{http.MethodDelete, "/api/servers/1"},
		{http.MethodPost, "/api/servers/1/test"},
		{http.MethodGet, "/api/servers/1/browse"},
		{http.MethodGet, "/api/nas/browse"},
		{http.MethodPost, "/api/nas/mkdir"},
		{http.MethodGet, "/api/jobs"},
		{http.MethodPost, "/api/jobs"},
		{http.MethodGet, "/api/jobs/1"},
		{http.MethodDelete, "/api/jobs/1"},
		{http.MethodPost, "/api/jobs/1/cancel"},
		{http.MethodPost, "/api/jobs/1/retry"},
		{http.MethodGet, "/api/jobs/1/log"},
		{http.MethodGet, "/api/events"},
		{http.MethodGet, "/api/settings"},
		{http.MethodPut, "/api/settings"},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.target, func(t *testing.T) {
			w := do(t, h, rt.method, rt.target, "{}", false)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("no credentials: got %d, want 401", w.Code)
			}
			if got := w.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic ") {
				t.Errorf("WWW-Authenticate = %q, want a Basic challenge", got)
			}
		})
	}
}

func TestAuthRejectsWrongCredentials(t *testing.T) {
	h, _ := newTestAPI(t)
	tests := []struct{ name, u, p string }{
		{"wrong password", user, "nope"},
		{"wrong user", "someone", pass},
		{"both wrong", "someone", "nope"},
		{"empty", "", ""},
		{"password as user", pass, user},
		{"prefix of the password", user, pass[:3]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
			r.SetBasicAuth(tt.u, tt.p)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("got %d, want 401", w.Code)
			}
		})
	}
}

func TestUnconfiguredCredentialsAuthoriseNothing(t *testing.T) {
	for _, tt := range []struct{ name, u, p string }{
		{"no user", "", pass},
		{"no password", user, ""},
		{"neither", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if credsMatch(tt.u, tt.p, tt.u, tt.p) {
				t.Error("blank configured credentials authorised a request")
			}
		})
	}
	if !credsMatch(user, pass, user, pass) {
		t.Error("matching credentials were rejected")
	}
}

func TestServerCRUD(t *testing.T) {
	h, store := newTestAPI(t)

	body := `{"name":"src","host":"example.test","port":2222,"username":"u",
		"auth_type":"password","password":"s3cret","default_remote_path":"/data"}`
	w := do(t, h, http.MethodPost, "/api/servers", body, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	var created serverView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 || created.Port != 2222 || !created.HasPassword {
		t.Fatalf("created = %+v", created)
	}
	if strings.Contains(w.Body.String(), "s3cret") {
		t.Error("the response echoed the password back")
	}

	w = do(t, h, http.MethodGet, "/api/servers", "", true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"src"`) {
		t.Fatalf("list = %d: %s", w.Code, w.Body)
	}

	// A blank password on update must keep the stored one.
	upd := `{"name":"src2","host":"example.test","port":22,"username":"u","auth_type":"password"}`
	w = do(t, h, http.MethodPut, "/api/servers/"+itoa(created.ID), upd, true)
	if w.Code != http.StatusOK {
		t.Fatalf("update = %d: %s", w.Code, w.Body)
	}
	stored, err := store.GetServer(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "src2" || stored.Password != "s3cret" {
		t.Errorf("stored = %+v, want name src2 with the password kept", stored)
	}

	w = do(t, h, http.MethodDelete, "/api/servers/"+itoa(created.ID), "", true)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", w.Code, w.Body)
	}
	if w = do(t, h, http.MethodDelete, "/api/servers/"+itoa(created.ID), "", true); w.Code != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", w.Code)
	}
}

func TestServerValidation(t *testing.T) {
	h, _ := newTestAPI(t)
	tests := []struct{ name, body string }{
		{"missing name", `{"host":"h","username":"u","auth_type":"key"}`},
		{"missing host", `{"name":"n","username":"u","auth_type":"key"}`},
		{"missing username", `{"name":"n","host":"h","auth_type":"key"}`},
		{"bad auth type", `{"name":"n","host":"h","username":"u","auth_type":"magic"}`},
		{"port out of range", `{"name":"n","host":"h","username":"u","auth_type":"key","port":70000}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if w := do(t, h, http.MethodPost, "/api/servers", tt.body, true); w.Code != http.StatusBadRequest {
				t.Errorf("got %d, want 400: %s", w.Code, w.Body)
			}
		})
	}
	if w := do(t, h, http.MethodPost, "/api/servers", "not json", true); w.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: got %d, want 400", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/api/servers/abc/browse", "", true); w.Code != http.StatusBadRequest {
		t.Errorf("bad id: got %d, want 400", w.Code)
	}
	if w := do(t, h, http.MethodGet, "/api/servers/999/browse", "", true); w.Code != http.StatusNotFound {
		t.Errorf("missing server: got %d, want 404", w.Code)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	h, _ := newTestAPI(t)
	w := do(t, h, http.MethodGet, "/api/settings", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d", w.Code)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["concurrency"] != "2" {
		t.Errorf("concurrency = %q, want 2", got["concurrency"])
	}

	w = do(t, h, http.MethodPut, "/api/settings", `{"allowed_dest_roots":"/volume1/media"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("put = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "/volume1/media") {
		t.Errorf("put response did not echo the new value: %s", w.Body)
	}
}

// The NAS is unreachable in tests, so these assert the error mapping rather
// than the happy path — that belongs to the E2E stack.
func TestNASRoutesReportConfigurationErrors(t *testing.T) {
	h, _ := newTestAPI(t)

	w := do(t, h, http.MethodGet, "/api/nas/browse", "", true)
	if w.Code != http.StatusBadRequest {
		t.Errorf("browse with no roots = %d, want 400: %s", w.Code, w.Body)
	}

	if w = do(t, h, http.MethodPut, "/api/settings",
		`{"allowed_dest_roots":"/volume1/media"}`, true); w.Code != http.StatusOK {
		t.Fatalf("configuring roots: %d", w.Code)
	}

	// With roots but no NAS host, the root list still renders (it is local),
	// while anything needing the connection reports "not configured".
	w = do(t, h, http.MethodGet, "/api/nas/browse", "", true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "/volume1/media") {
		t.Errorf("root listing = %d: %s", w.Code, w.Body)
	}
	w = do(t, h, http.MethodGet, "/api/nas/browse?path=/volume1/media", "", true)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("browse without a NAS host = %d, want 503: %s", w.Code, w.Body)
	}
	w = do(t, h, http.MethodPost, "/api/nas/mkdir", `{"path":"/etc/evil"}`, true)
	if w.Code != http.StatusBadRequest {
		t.Errorf("mkdir outside the roots = %d, want 400: %s", w.Code, w.Body)
	}
	w = do(t, h, http.MethodPost, "/api/nas/mkdir", `{`, true)
	if w.Code != http.StatusBadRequest {
		t.Errorf("mkdir with bad JSON = %d, want 400", w.Code)
	}
}

func TestServerTestReportsFailureWithoutFailingTheRequest(t *testing.T) {
	h, _ := newTestAPI(t)
	body := `{"name":"dead","host":"127.0.0.1","port":1,"username":"u","auth_type":"password","password":"x"}`
	w := do(t, h, http.MethodPost, "/api/servers", body, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	var created serverView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	w = do(t, h, http.MethodPost, "/api/servers/"+itoa(created.ID)+"/test", "", true)
	if w.Code != http.StatusOK {
		t.Fatalf("test = %d, want 200 with ok:false: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"ok":false`) {
		t.Errorf("test body = %s, want ok:false", w.Body)
	}
}

func TestBrowseUnreachableServer(t *testing.T) {
	h, _ := newTestAPI(t)
	body := `{"name":"dead","host":"127.0.0.1","port":1,"username":"u","auth_type":"key","private_key":"nope"}`
	w := do(t, h, http.MethodPost, "/api/servers", body, true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	var created serverView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	w = do(t, h, http.MethodGet, "/api/servers/"+itoa(created.ID)+"/browse?path=/x", "", true)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("browse = %d, want 500: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "nope") {
		t.Error("the error response leaked key material")
	}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

// TestCredentialsNeverReachTheLogs is a security gate: passwords, private keys
// and passphrases must not appear in any log line, including error paths.
func TestCredentialsNeverReachTheLogs(t *testing.T) {
	const (
		secretPass = "sup3r-s3cret-password"
		secretKey  = "-----BEGIN OPENSSH PRIVATE KEY-----\nZm9vYmFy\n-----END OPENSSH PRIVATE KEY-----"
		secretPhr  = "my-passphrase-value"
	)
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	h, _ := newTestAPI(t)
	body, err := json.Marshal(serverInput{
		Name: "secret-server", Host: "127.0.0.1", Port: 1, Username: "u",
		AuthType: "key", Password: secretPass, PrivateKey: secretKey, Passphrase: secretPhr,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := do(t, h, http.MethodPost, "/api/servers", string(body), true)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	var created serverView
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := itoa(created.ID)

	// Exercise the paths that touch credentials, including the failing ones.
	list := do(t, h, http.MethodGet, "/api/servers", "", true)
	do(t, h, http.MethodPost, "/api/servers/"+id+"/test", "", true)
	do(t, h, http.MethodGet, "/api/servers/"+id+"/browse?path=/", "", true)
	do(t, h, http.MethodPut, "/api/servers/"+id, string(body), true)
	do(t, h, http.MethodGet, "/api/settings", "", true)

	for _, secret := range []string{secretPass, secretKey, secretPhr, "Zm9vYmFy"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("a credential reached the logs: %s", logs.String())
		}
		// The same material must not come back on the wire either: the UI
		// re-sends a blank field to mean "keep what is stored".
		for _, body := range []string{w.Body.String(), list.Body.String()} {
			if strings.Contains(body, secret) {
				t.Errorf("a credential was echoed in a response: %s", body)
			}
		}
	}
	if logs.Len() == 0 {
		t.Fatal("nothing was logged, so the assertion proves nothing")
	}
}
