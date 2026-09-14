package main

import (
	"embed"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
)

//go:embed all:web
var webFS embed.FS

// spaHandler serves the embedded frontend as a single-page application.
// Static files are served directly; all other paths fall back to index.html
// so client-side routing works.
//
// When GMUXD_DEV_PROXY is set (e.g. "http://localhost:5173"), all frontend
// requests are reverse-proxied to that URL instead. This lets the vite dev
// server handle HMR while gmuxd handles API, WebSocket, and Tailscale auth.
func spaHandler() http.Handler {
	if devProxy := os.Getenv("GMUXD_DEV_PROXY"); devProxy != "" {
		return devProxyHandler(devProxy)
	}
	return embeddedHandler()
}

func devProxyHandler(target string) http.Handler {
	u, err := url.Parse(target)
	if err != nil {
		log.Fatalf("GMUXD_DEV_PROXY: invalid URL %q: %v", target, err)
	}
	log.Printf("frontend: proxying to dev server at %s", target)
	proxy := httputil.NewSingleHostReverseProxy(u)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/ws/") {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

func embeddedHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("embedded web directory missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/ws/") {
			http.NotFound(w, r)
			return
		}

		fsPath := strings.TrimPrefix(path, "/")
		if fsPath == "" {
			fsPath = "index.html"
		}

		if _, err := fs.Stat(sub, fsPath); err == nil {
			if strings.HasPrefix(fsPath, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			// SPIKE R1: a build-time .gz sibling costs the daemon nothing at
			// request time (no deflate CPU, no per-request allocation) and
			// compresses at level 9 instead of the middleware's streaming 5.
			// Falls through to the middleware when the sibling is absent, so
			// a build without the gzip step still serves compressed bytes.
			if servePrecompressed(w, r, sub, fsPath) {
				return
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}

// servePrecompressed serves fsPath+".gz" when the client accepts gzip and
// the sibling exists. Reports whether it handled the request.
func servePrecompressed(w http.ResponseWriter, r *http.Request, sub fs.FS, fsPath string) bool {
	if !strings.Contains(strings.ToLower(r.Header.Get("Accept-Encoding")), "gzip") {
		return false
	}
	// Let the file server answer ranged requests from the identity file.
	if r.Header.Get("Range") != "" {
		return false
	}
	data, err := fs.ReadFile(sub, fsPath+".gz")
	if err != nil {
		return false
	}
	if ct := mime.TypeByExtension(path.Ext(fsPath)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Add("Vary", "Accept-Encoding")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method == http.MethodHead {
		return true
	}
	_, _ = w.Write(data)
	return true
}
