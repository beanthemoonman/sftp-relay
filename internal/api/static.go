package api

import (
	"errors"
	"io/fs"
	"net/http"
	"strings"

	"sftp-relay/web"
)

// staticHandler serves the embedded React bundle. It sits inside the
// authenticated group, so the UI is no more reachable than the API is.
func staticHandler() (http.Handler, error) {
	sub, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, errors.New("static: dist/index.html is missing; run npm run build in web/")
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			// The catch-all must not answer an unknown API route with HTML: a
			// client would parse a 200 page as a successful response.
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(sub, name); err != nil {
			// Unknown path: hand back the shell and let the client route it.
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		// Asset filenames are fixed rather than content-hashed, so they must be
		// revalidated; on a LAN that costs one 304 per load.
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}), nil
}
