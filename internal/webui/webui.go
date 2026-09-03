// Package webui serves the web client (D23).
//
// The assets are embedded rather than shipped as a second image, so the web
// client is the same artifact as everything else: one binary, one image, one
// version, a third component alongside control-plane and worker. D23 originally
// argued for a separate container on the grounds that go:embed makes the assets
// non-optional in the cluster image; measured, that is about 1.5 MB in a 12 MB
// binary, against a whole second release process. One artifact wins on its own
// terms.
//
// It also means `je web run` needs no Docker, which is a side effect and not
// the reason -- the web client has no claim on running natively (D23).
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// dist is written by `make web-build` and committed, which is what lets
// `go build ./cmd/je` produce a working binary on a machine with no npm.
//
//go:embed all:dist
var dist embed.FS

// Assets returns the built client, and whether it is the real one. A binary
// built before `make web-build` ran carries the placeholder, and saying so beats
// serving a blank page.
func Assets() (fs.FS, bool) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, false
	}
	_, err = fs.Stat(sub, "assets")
	return sub, err == nil
}

// Handler serves the client, proxying /v1 to the control plane.
//
// The proxy is what makes this a client rather than a peer: the browser talks to
// one origin, the web server forwards the API calls unchanged, and nothing here
// knows what any endpoint means. Every capability still comes from the control
// plane (D15), and this process holds no state and no database handle.
func Handler(controlPlane *url.URL) (http.Handler, bool, error) {
	assets, built := Assets()
	if !built {
		return placeholder{}, false, nil
	}

	proxy := httputil.NewSingleHostReverseProxy(controlPlane)
	files := http.FileServer(http.FS(assets))

	mux := http.NewServeMux()
	mux.Handle("/v1/", proxy)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// A single-page client owns its own routes, so anything that is not a
		// real file is the app itself rather than a 404.
		if _, err := fs.Stat(assets, strings.TrimPrefix(r.URL.Path, "/")); err != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
	return mux, true, nil
}

type placeholder struct{}

func (placeholder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>je</title>
<body style="font:14px ui-monospace,monospace;background:#0f1115;color:#e4e7ee;padding:40px">
<h1 style="font-size:16px">no web client in this binary</h1>
<p style="color:#8b93a5">This <code>je</code> was built without the client assets.
Build them and rebuild:</p>
<pre style="color:#6ea8fe">  make web-build
  make build</pre>
</body>`))
}
