// Package httpz holds transport-level HTTP middleware for the daemon's
// network listeners (TCP + tsnet). SPIKE (R1): response compression.
//
// Why here and not on the Unix socket: the socket serves the local CLI over
// a loopback-equivalent transport where compression is pure CPU cost. The
// bytes that matter are the ones a phone pulls over Tailscale, and both the
// TCP and tsnet listeners share one authed handler (serve_central.go).
//
// Shape of the wrapper:
//
//   - Negotiation is per response, decided at WriteHeader time from the
//     Content-Type the handler chose. Handlers stay unaware of encoding.
//   - `Flush` means "flush the compressor, then the connection", so an SSE
//     event boundary is still a byte boundary on the wire: gzip's
//     Z_SYNC_FLUSH ends the deflate block and the browser's decoder emits
//     the event immediately. No added latency, no event coalescing.
//   - The compressor lives for the whole response, so a long-lived SSE
//     stream keeps its dictionary across events; the 60th snapshot batch
//     compresses against the 59th.
//   - Upgrade requests (WebSocket) are never wrapped: they hijack the
//     connection and own their own framing (#242/#279 keep permessage-
//     deflate off deliberately).
package httpz

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// DefaultLevel is the deflate level used for responses. Measured on the
// operator's real 3.2 MB protocol-3 bootstrap, flush-per-event:
//
//	level 1: 580 KB (5.5x)  9.6 ms encode
//	level 5: 501 KB (6.4x) 17.6 ms
//	level 6: 495 KB (6.5x) 19.7 ms   <- default
//	level 9: 465 KB (6.9x) 53.1 ms
//
// Known stdlib wart, measured here: compress/flate levels 1-6 emit a
// *stored* (uncompressed) block for a flushed write shorter than the
// ~262-byte match lookahead, so an 93-byte `session-activity` frame costs
// ~103 bytes on the wire — a 10% expansion on 0.5% of the traffic. Levels
// 7-9 compress those frames ~7x but cost 23-170% more CPU on the large
// frames that carry the bytes. Revisit if the stream ever becomes mostly
// sub-262-byte frames.
const DefaultLevel = 6

// minCompressLength skips compressing responses that declare a length too
// small to win anything after the 18-byte gzip envelope.
const minCompressLength = 512

// compressibleTypes is matched against the response Content-Type prefix.
// application/octet-stream is in the list because that is what the
// scrollback broker serves (raw PTY bytes, the 1.2 MB median dead-session
// replay). If the daemon ever serves real binaries under that type this
// list needs a path carve-out instead.
var compressibleTypes = []string{
	"text/",
	"application/json",
	"application/javascript",
	"application/x-javascript",
	"application/octet-stream",
	"application/wasm",
	"image/svg+xml",
}

func compressible(ct string) bool {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.ToLower(strings.TrimSpace(ct))
	for _, p := range compressibleTypes {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	return false
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		// q=0 is an explicit refusal.
		if q, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			if v, err := strconv.ParseFloat(q, 64); err == nil && v == 0 {
				return false
			}
		}
		return true
	}
	return false
}

// Pools are per level: a gzip.Writer carries its level and Reset does not
// change it, so one shared pool would silently serve a level-1 writer to a
// level-6 response (it did, during this spike's first measurement run).
var writerPools [gzip.BestCompression + 1]sync.Pool

func poolIndex(level int) int {
	if level < gzip.BestSpeed || level > gzip.BestCompression {
		return gzip.DefaultCompression + 1 // unreachable for our levels
	}
	return level
}

func getWriter(w io.Writer, level int) *gzip.Writer {
	if v := writerPools[poolIndex(level)].Get(); v != nil {
		zw := v.(*gzip.Writer)
		zw.Reset(w)
		return zw
	}
	zw, err := gzip.NewWriterLevel(w, level)
	if err != nil { // level is validated by the caller
		zw = gzip.NewWriter(w)
	}
	return zw
}

func putWriter(zw *gzip.Writer, level int) {
	zw.Reset(io.Discard)
	writerPools[poolIndex(level)].Put(zw)
}

// Gzip returns middleware that compresses eligible responses.
func Gzip(next http.Handler) http.Handler { return GzipLevel(next, DefaultLevel) }

// GzipLevel is Gzip with an explicit deflate level (for benchmarks).
func GzipLevel(next http.Handler, level int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A Range request wants byte offsets in the *identity* representation;
		// compressing the selected range (or a 206 with Content-Range) would
		// corrupt it. http.FileServer serves ranges for the embedded assets.
		if !acceptsGzip(r) || isUpgrade(r) || r.Header.Get("Range") != "" {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w, level: level}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

func isUpgrade(r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get("Upgrade")) != "" {
		return true
	}
	for _, v := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(v), "upgrade") {
			return true
		}
	}
	return false
}

type gzipWriter struct {
	http.ResponseWriter
	level       int
	zw          *gzip.Writer
	wroteHeader bool
}

// Unwrap lets http.ResponseController reach the real writer for
// SetWriteDeadline (the SSE path sets one per frame) and Hijack.
func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipWriter) WriteHeader(status int) {
	if g.wroteHeader {
		g.ResponseWriter.WriteHeader(status)
		return
	}
	g.wroteHeader = true
	h := g.Header()
	if !slices.Contains(h.Values("Vary"), "Accept-Encoding") {
		h.Add("Vary", "Accept-Encoding")
	}
	switch {
	case status < 200, status == http.StatusNoContent, status == http.StatusNotModified,
		status == http.StatusPartialContent:
		// No body, or a body the client must not re-decode/re-offset.
	case h.Get("Content-Encoding") != "":
		// Handler already encoded (precompressed assets).
	case !compressible(h.Get("Content-Type")):
	case declaredLengthBelow(h, minCompressLength):
	default:
		h.Set("Content-Encoding", "gzip")
		// The compressed length is unknown until the body is written, and
		// for SSE there is no end at all.
		h.Del("Content-Length")
		// A gzip stream is not byte-range addressable.
		h.Del("Accept-Ranges")
		g.zw = getWriter(g.ResponseWriter, g.level)
	}
	g.ResponseWriter.WriteHeader(status)
}

func declaredLengthBelow(h http.Header, limit int) bool {
	v := h.Get("Content-Length")
	if v == "" {
		return false
	}
	n, err := strconv.Atoi(v)
	return err == nil && n < limit
}

func (g *gzipWriter) Write(p []byte) (int, error) {
	if !g.wroteHeader {
		// Mirror net/http: an implicit 200 sniffs Content-Type from the
		// first write if the handler did not set one.
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(p))
		}
		g.WriteHeader(http.StatusOK)
	}
	if g.zw != nil {
		return g.zw.Write(p)
	}
	return g.ResponseWriter.Write(p)
}

// Flush ends the current deflate block (Z_SYNC_FLUSH) before flushing the
// connection. Every buffered byte therefore becomes decodable by the client
// at the moment the handler asked for it — the SSE per-event contract.
func (g *gzipWriter) Flush() {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.zw != nil {
		_ = g.zw.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack keeps the writer transparent to anything that takes the connection
// over (nothing compressible does today, since upgrades are not wrapped).
func (g *gzipWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := g.ResponseWriter.(http.Hijacker); ok {
		if g.zw != nil {
			_ = g.zw.Close()
			putWriter(g.zw, g.level)
			g.zw = nil
		}
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (g *gzipWriter) close() {
	if g.zw == nil {
		return
	}
	_ = g.zw.Close() // writes the gzip trailer
	putWriter(g.zw, g.level)
	g.zw = nil
}
