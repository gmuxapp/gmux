package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/httpz"
)

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// bundleFS is a miniature embed dir: a hashed JS asset with a build-time
// .gz sibling, a small manifest without one, and index.html.
func bundleFS(t *testing.T) (fstest.MapFS, string) {
	js := strings.Repeat("function f(){return 42}\n", 200)
	return fstest.MapFS{
		"index.html":             {Data: []byte("<!doctype html><script src=/assets/index-abc.js></script>" + strings.Repeat("<meta name=x content=y>", 40))},
		"assets/index-abc.js":    {Data: []byte(js)},
		"assets/index-abc.js.gz": {Data: gz(t, js)},
		"manifest.json":          {Data: []byte(`{"name":"gmux"}`)},
	}, js
}

// The production wiring: middleware outside, SPA handler inside.
func assetServer(t *testing.T, precompressed bool) (*httptest.Server, string) {
	fsys, js := bundleFS(t)
	srv := httptest.NewServer(httpz.Gzip(embeddedHandler(fsys, precompressed)))
	t.Cleanup(srv.Close)
	return srv, js
}

func rawFetch(t *testing.T, method, url string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestPrecompressedAssetServed(t *testing.T) {
	srv, js := assetServer(t, true)
	resp := rawFetch(t, http.MethodGet, srv.URL+"/assets/index-abc.js", map[string]string{"Accept-Encoding": "gzip, deflate, br"})
	wire, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q", got)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") && !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/javascript") {
		t.Fatalf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	if got := resp.Header.Values("Vary"); len(got) != 1 || got[0] != "Accept-Encoding" {
		t.Fatalf("Vary = %v (middleware must not duplicate the handler's)", got)
	}
	etag := resp.Header.Get("ETag")
	if !strings.HasSuffix(etag, `-gz"`) {
		t.Fatalf("ETag = %q, want a gzip-representation tag", etag)
	}
	if resp.Header.Get("Content-Length") != itoa(len(wire)) {
		t.Fatalf("Content-Length %q != wire %d", resp.Header.Get("Content-Length"), len(wire))
	}
	if !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
	// Exactly one gzip layer, level-9 sibling bytes verbatim, decodes to the source.
	zr, err := gzip.NewReader(bytes.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	decoded, _ := io.ReadAll(zr)
	if string(decoded) != js {
		t.Fatalf("decoded asset differs (%d vs %d bytes)", len(decoded), len(js))
	}
	if !bytes.Equal(wire, gz(t, js)) {
		t.Fatal("wire bytes are not the precompressed sibling verbatim")
	}

	// Conditional request against the gzip ETag → 304.
	resp304 := rawFetch(t, http.MethodGet, srv.URL+"/assets/index-abc.js", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": etag})
	if resp304.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match: status %d", resp304.StatusCode)
	}

	// HEAD mirrors GET's headers with no body.
	head := rawFetch(t, http.MethodHead, srv.URL+"/assets/index-abc.js", map[string]string{"Accept-Encoding": "gzip"})
	if head.Header.Get("Content-Encoding") != "gzip" || head.Header.Get("ETag") != etag || head.Header.Get("Content-Length") != itoa(len(wire)) {
		t.Fatalf("HEAD headers: %v", head.Header)
	}

	// A range addresses the gzip representation.
	rng := rawFetch(t, http.MethodGet, srv.URL+"/assets/index-abc.js", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-9"})
	part, _ := io.ReadAll(rng.Body)
	if rng.StatusCode != http.StatusPartialContent || rng.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(part, wire[:10]) {
		t.Fatalf("range: status=%d ce=%q len=%d", rng.StatusCode, rng.Header.Get("Content-Encoding"), len(part))
	}
}

func TestIdentityAssetUnchanged(t *testing.T) {
	srv, js := assetServer(t, true)
	resp := rawFetch(t, http.MethodGet, srv.URL+"/assets/index-abc.js", map[string]string{"Accept-Encoding": "identity"})
	body, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Encoding") != "" || string(body) != js {
		t.Fatalf("identity client got ce=%q len=%d", resp.Header.Get("Content-Encoding"), len(body))
	}
	if resp.Header.Get("ETag") != "" {
		// The identity file server sets no validator; the gzip tag must not leak onto it.
		t.Fatalf("identity ETag = %q", resp.Header.Get("ETag"))
	}
	if !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("identity Vary = %q", resp.Header.Get("Vary"))
	}
}

func TestAssetWithoutSiblingFallsThroughToMiddleware(t *testing.T) {
	srv, js := assetServer(t, true)
	resp := rawFetch(t, http.MethodGet, srv.URL+"/", map[string]string{"Accept-Encoding": "gzip"})
	wire, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("index.html not compressed on the fly: %v", resp.Header)
	}
	zr, err := gzip.NewReader(bytes.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	decoded, _ := io.ReadAll(zr)
	if !strings.Contains(string(decoded), "index-abc.js") {
		t.Fatalf("decoded index.html = %q", decoded)
	}
	_ = js
	// Tiny JSON with a declared length under the threshold stays identity.
	small := rawFetch(t, http.MethodGet, srv.URL+"/manifest.json", map[string]string{"Accept-Encoding": "gzip"})
	io.Copy(io.Discard, small.Body)
	if small.Header.Get("Content-Encoding") != "" {
		t.Fatal("15-byte manifest was compressed")
	}
}

func TestPrecompressedDisabledWithCompression(t *testing.T) {
	fsys, js := bundleFS(t)
	// Compression off: no middleware, no siblings — exactly the pre-change wire.
	srv := httptest.NewServer(embeddedHandler(fsys, false))
	defer srv.Close()
	resp := rawFetch(t, http.MethodGet, srv.URL+"/assets/index-abc.js", map[string]string{"Accept-Encoding": "gzip"})
	body, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Content-Encoding") != "" || string(body) != js {
		t.Fatalf("compression off still served gzip: %v", resp.Header)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
