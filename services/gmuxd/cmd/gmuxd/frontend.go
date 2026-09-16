package main

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
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
	"sync"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/httpz"
)

//go:embed all:web
var webFS embed.FS

// spaHandler serves the embedded frontend as a single-page application.
// Static files are served directly; all other paths fall back to index.html
// so client-side routing works.
//
// precompressed enables serving the build-time .gz siblings of the bundle
// (see scripts/embed-web.sh) to clients that accept gzip. Callers pass the
// compression switch for the network mux and false for the Unix-socket mux,
// so turning compression off really turns every compressed byte off and the
// socket never serves an encoded body.
//
// When GMUXD_DEV_PROXY is set (e.g. "http://localhost:5173"), all frontend
// requests are reverse-proxied to that URL instead. This lets the vite dev
// server handle HMR while gmuxd handles API, WebSocket, and Tailscale auth.
func spaHandler(precompressed bool) http.Handler {
	if devProxy := os.Getenv("GMUXD_DEV_PROXY"); devProxy != "" {
		return devProxyHandler(devProxy)
	}
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("embedded web directory missing: " + err.Error())
	}
	return embeddedHandler(sub, precompressed)
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

func embeddedHandler(sub fs.FS, precompressed bool) http.Handler {
	fileServer := http.FileServer(http.FS(sub))
	var pre *precompressedAssets
	if precompressed {
		pre = &precompressedAssets{fs: sub}
	}

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
			if pre != nil && pre.serve(w, r, fsPath) {
				return
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}

// precompressedAssets serves the build-time gzip sibling of an embedded
// asset. The bundle is immutable for the life of the process, so the
// sibling's ETag is computed once per path and cached.
//
// Why a sibling and not the middleware: the 926 KB bundle is the single
// largest response the daemon serves and it never changes between builds.
// A build-time gzip -9 costs the daemon zero deflate CPU per request and
// compresses better than the middleware's streaming level 6. When the
// sibling is absent (a build without the gzip step) the request falls
// through to the file server and the middleware compresses it on the fly.
type precompressedAssets struct {
	fs   fs.FS
	etag sync.Map // fsPath -> string
}

func (p *precompressedAssets) serve(w http.ResponseWriter, r *http.Request, fsPath string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !httpz.AcceptsGzip(r) {
		return false
	}
	data, err := fs.ReadFile(p.fs, fsPath+".gz")
	if err != nil {
		return false
	}
	h := w.Header()
	if ct := mime.TypeByExtension(path.Ext(fsPath)); ct != "" {
		h.Set("Content-Type", ct)
	} else {
		// Never let ServeContent sniff a Content-Type from gzip bytes.
		h.Set("Content-Type", "application/octet-stream")
	}
	h.Set("Content-Encoding", "gzip")
	httpz.AddVaryAcceptEncoding(h)
	if r.Header.Get("Range") == "" {
		// ServeContent leaves Content-Length unset when Content-Encoding is
		// present (it cannot know the encoded size); here the encoded bytes
		// are the whole body, so declare them and avoid chunking.
		h.Set("Content-Length", strconv.Itoa(len(data)))
	}
	// An ETag identifies a representation, so the gzip sibling gets its own
	// (a validator for the identity body must never match this one).
	h.Set("ETag", p.etagFor(fsPath, data))
	// ServeContent handles HEAD, If-None-Match (304), Content-Length and
	// Range. A range addresses the selected representation — here the gzip
	// bytes — which is what a client resuming this exact download expects;
	// the compression middleware never touches a Range response. The zero
	// modtime suppresses Last-Modified: the embed FS has none.
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	return true
}

func (p *precompressedAssets) etagFor(fsPath string, data []byte) string {
	if v, ok := p.etag.Load(fsPath); ok {
		return v.(string)
	}
	sum := sha256.Sum256(data)
	tag := `"` + hex.EncodeToString(sum[:8]) + `-gz"`
	p.etag.Store(fsPath, tag)
	return tag
}
