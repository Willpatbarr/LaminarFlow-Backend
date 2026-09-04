// Package frontend serves the built React bundle for every route the API does
// not claim.
//
// The deployment is same-origin by decision (Architecture notes §2): a split
// origin breaks the cookie-based session auth Safari and Chrome increasingly
// refuse to send cross-site, and leaves the backend unreachable from any
// browser not already on the tailnet. One process, one port, both halves.
package frontend

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// notBuilt is served when the bundle is missing - a fresh clone that has never
// run scripts/build-frontend.sh embeds only the directory marker.
//
// A blank 404 here is the confusing case: the server is healthy, the API works,
// and the browser shows nothing. Saying so directly costs one const.
const notBuilt = `<!doctype html>
<meta charset="utf-8">
<title>Frontend not built</title>
<body style="font-family:system-ui;max-width:34rem;margin:4rem auto;line-height:1.6">
<h1>Frontend not built</h1>
<p>The Go server is running and the API is available, but no frontend bundle is
embedded in this binary.</p>
<pre>./scripts/build-frontend.sh &amp;&amp; go build ./...</pre>
<p>Or point <code>FRONTEND_DIR</code> at a directory containing a built bundle.</p>
</body>`

// indexFile is the SPA entry point and the fallback for unmatched routes.
const indexFile = "index.html"

// New returns a handler serving files from fsys, falling back to index.html.
//
// The fallback is what makes client-side routing survive a refresh: a browser
// asking for /projects/42 directly must receive the app shell, not a 404, and
// let the router resolve the path once it boots.
func New(fsys fs.FS) http.Handler {
	return &handler{files: fsys}
}

type handler struct {
	files fs.FS
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" {
		name = indexFile
	}

	// fs.ValidPath rejects "..", absolute paths, and anything else that could
	// escape the bundle. http.FileServer does this for you; doing the lookup by
	// hand means doing this by hand too.
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}

	if h.serveFile(w, r, name, name != indexFile) {
		return
	}

	// A missing file that looks like an asset is a 404, not a fallback. This is
	// the difference between a useful error and a baffling one: serving HTML in
	// answer to a .js request makes the browser report "Unexpected token '<'",
	// which points nowhere near the stale hash that actually caused it.
	//
	// Client routes are extensionless by convention, so the extension is the
	// signal. /docs/readme.md would 404 under this rule; that is the right
	// trade against silently breaking every asset 404.
	if path.Ext(name) != "" && name != indexFile {
		http.NotFound(w, r)
		return
	}

	// Unmatched route: hand back the app shell so the client router can resolve
	// it. Deliberately 200, not 404 - the route may well be real to the SPA.
	if h.serveFile(w, r, indexFile, false) {
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(notBuilt))
}

// serveFile writes name from the bundle and reports whether it existed.
//
// hashed says the filename carries a content hash, as everything Vite emits
// into assets/ does. Those can be cached forever because a change produces a
// new filename; index.html cannot, because its name never changes and it is
// what points at the new hashes.
func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, name string, hashed bool) bool {
	f, err := h.files.Open(name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "read bundle", http.StatusInternalServerError)
			return true
		}
		return false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		return false
	}

	seeker, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "bundle file is not seekable", http.StatusInternalServerError)
		return true
	}

	if hashed {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// The shell must not be cached, or a browser keeps loading the old one
		// and asking for asset hashes that no longer exist.
		w.Header().Set("Cache-Control", "no-cache")
	}

	// ServeContent sets Content-Type from the extension and handles range
	// requests and conditional GETs.
	http.ServeContent(w, r, info.Name(), info.ModTime(), seeker)
	return true
}
