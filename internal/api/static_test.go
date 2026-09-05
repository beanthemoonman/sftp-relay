package api

import (
	"net/http"
	"strings"
	"testing"
)

// The bundle is embedded at build time, so these run against the real dist
// directory. They assert routing and auth, not the contents of the app.
func TestStaticBundle(t *testing.T) {
	h, _ := newTestAPI(t)

	cases := []struct {
		name   string
		path   string
		auth   bool
		status int
	}{
		{"root needs credentials", "/", false, http.StatusUnauthorized},
		{"asset needs credentials", "/assets/app.js", false, http.StatusUnauthorized},
		{"root serves the shell", "/", true, http.StatusOK},
		{"asset is served", "/assets/app.js", true, http.StatusOK},
		{"unknown path falls back to the shell", "/queue", true, http.StatusOK},
		{"unknown api path is still a 404", "/api/nope", true, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, h, http.MethodGet, c.path, "", c.auth)
			if w.Code != c.status {
				t.Fatalf("GET %s = %d, want %d", c.path, w.Code, c.status)
			}
		})
	}
}

func TestStaticShellIsHTML(t *testing.T) {
	h, _ := newTestAPI(t)
	w := do(t, h, http.MethodGet, "/", "", true)
	if body := w.Body.String(); !strings.Contains(body, `id="root"`) {
		t.Fatalf("root body does not look like the app shell: %q", body[:min(len(body), 200)])
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache (asset names are not hashed)", got)
	}
}
