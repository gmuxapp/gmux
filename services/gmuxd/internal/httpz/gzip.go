// Package httpz holds transport-level HTTP middleware for the daemon's
// network listeners (TCP + tsnet): response compression.
//
// Why here and not on the Unix socket: the socket serves the local CLI over
// a loopback-equivalent transport where compression is pure CPU cost. The
// bytes that matter are the ones a phone pulls over Tailscale, and both the
// TCP and tsnet listeners share one authed handler (serve_central.go). The
// socket mux is not wrapped, and the precompressed bundle siblings
// (frontend.go) are likewise enabled for the network mux only, so no
// response on the socket ever carries Content-Encoding.
//
// Shape of the wrapper:
//
//   - Negotiation is per response, decided at WriteHeader time from the
//     Content-Type the handler chose. Handlers stay unaware of encoding.
//   - Flush means "flush the compressor, then the connection", so an SSE
//     event boundary is still a byte boundary on the wire: gzip's
//     Z_SYNC_FLUSH ends the deflate block and the browser's decoder emits
//     the event immediately. No added latency, no event coalescing.
//   - FlushError propagates the connection's write error (a tripped write
//     deadline) exactly as the bare net/http writer does, so the SSE loop's
//     `rc.Flush()` stall detection keeps working through the wrapper.
//   - The compressor lives for the whole response, so a long-lived SSE
//     stream keeps its dictionary across events; the 60th snapshot batch
//     compresses against the 59th.
//   - Upgrade requests (WebSocket) are never wrapped: they hijack the
//     connection and own their own framing. This is deliberately a different
//     mechanism from WebSocket permessage-deflate, which #242 enabled and
//     #279 had to revert because one mobile browser negotiated the extension
//     and then could not decode the frames. HTTP Content-Encoding: gzip is
//     decoded by the browser's ordinary HTTP stack — the same code path every
//     gzip-serving website exercises — not by a WebSocket extension.
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
//	level 7: 494 KB (6.5x) 24.3 ms
//	level 9: 465 KB (6.9x) 53.1 ms
//
// Known stdlib wart, measured: compress/flate levels 1-6 emit a *stored*
// (uncompressed) block for a flushed write shorter than the ~262-byte match
// lookahead, so a 93-byte `session-activity` frame costs ~103 bytes on the
// wire — a 10% expansion on <1% of the traffic. Levels 7-9 compress those
// frames ~7x but cost 23-170% more CPU on the large frames that carry the
// bytes (klauspost/compress behaves identically; it is the same encoder).
// Revisit if the stream ever becomes mostly sub-262-byte frames.
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

// AcceptsGzip reports whether the request's Accept-Encoding admits gzip
// (present and not refused with q=0).
func AcceptsGzip(r *http.Request) bool {
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
// level-6 response.
var writerPools [gzip.BestCompression + 1]sync.Pool

func getWriter(w io.Writer, level int) *gzip.Writer {
	if v := writerPools[level].Get(); v != nil {
		zw := v.(*gzip.Writer)
		zw.Reset(w)
		return zw
	}
	zw, err := gzip.NewWriterLevel(w, level)
	if err != nil { // unreachable: level is validated by GzipLevel
		zw = gzip.NewWriter(w)
	}
	return zw
}

func putWriter(zw *gzip.Writer, level int) {
	zw.Reset(io.Discard)
	writerPools[level].Put(zw)
}

// Gzip returns middleware that compresses eligible responses at DefaultLevel.
func Gzip(next http.Handler) http.Handler { return GzipLevel(next, DefaultLevel) }

// GzipLevel is Gzip with an explicit deflate level in [1, 9]; other values
// fall back to DefaultLevel.
func GzipLevel(next http.Handler, level int) http.Handler {
	if level < gzip.BestSpeed || level > gzip.BestCompression {
		level = DefaultLevel
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A Range request wants byte offsets in the *identity* representation;
		// compressing the selected range (or a 206 with Content-Range) would
		// corrupt it. http.FileServer serves ranges for the embedded assets.
		// HEAD has no body to compress, so it is passed through as well: the
		// handler sees the same writer for GET and HEAD, and net/http
		// discards whatever it writes.
		if isUpgrade(r) {
			next.ServeHTTP(w, r)
			return
		}
		if !AcceptsGzip(r) || r.Header.Get("Range") != "" || r.Method == http.MethodHead {
			// Still an Accept-Encoding-dependent representation: a cache
			// must not hand this identity body to a gzip-accepting client.
			AddVaryAcceptEncoding(w.Header())
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w, level: level}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

// AddVaryAcceptEncoding adds Vary: Accept-Encoding once.
func AddVaryAcceptEncoding(h http.Header) {
	if !slices.Contains(h.Values("Vary"), "Accept-Encoding") {
		h.Add("Vary", "Accept-Encoding")
	}
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
	wroteHeader bool // handler committed a status (through us)
	headerSent  bool // status actually written to the underlying writer
	status      int
	// pending holds a small body whose encoding is still undecided: the
	// response is compressible by type but declared no length, so we buffer
	// up to minCompressLength bytes before choosing. A 72-byte JSON error
	// envelope would otherwise grow to 89 bytes on the wire. Streaming
	// content types (SSE) never wait: their first event is tiny but the
	// stream is not, so they commit to gzip at WriteHeader.
	pending bool
	buf     []byte
}

// Unwrap lets http.ResponseController reach the real writer for
// SetWriteDeadline (the SSE path sets one per frame) and Hijack.
func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipWriter) WriteHeader(status int) {
	if status < 200 {
		// Informational responses (1xx) do not commit the final header set.
		g.ResponseWriter.WriteHeader(status)
		return
	}
	if g.wroteHeader {
		return // net/http also ignores a second WriteHeader
	}
	g.wroteHeader = true
	g.status = status
	h := g.Header()
	// The representation depends on Accept-Encoding whether or not this
	// particular response ended up compressed; caches must key on it.
	AddVaryAcceptEncoding(h)
	switch {
	case status == http.StatusNoContent, status == http.StatusNotModified,
		status == http.StatusPartialContent:
		// No body, or a body the client must not re-decode/re-offset.
	case h.Get("Content-Encoding") != "":
		// Handler already encoded (precompressed assets).
	case !compressible(h.Get("Content-Type")):
	case declaredLengthBelow(h, minCompressLength):
	case h.Get("Content-Length") == "" && !streaming(h.Get("Content-Type")):
		// Undeclared length: wait for the body to prove itself.
		g.pending = true
		return
	default:
		g.commitGzip()
		return
	}
	g.sendHeader()
}

// streaming reports content types whose responses are open-ended and
// flushed piecemeal, so no body-length heuristic applies.
func streaming(ct string) bool {
	return strings.HasPrefix(strings.ToLower(ct), "text/event-stream")
}

func (g *gzipWriter) sendHeader() {
	if g.headerSent {
		return
	}
	g.headerSent = true
	g.ResponseWriter.WriteHeader(g.status)
}

// commitGzip switches the response to gzip and drains any pending bytes.
func (g *gzipWriter) commitGzip() {
	g.pending = false
	h := g.Header()
	h.Set("Content-Encoding", "gzip")
	// The compressed length is unknown until the body is written, and for
	// SSE there is no end at all.
	h.Del("Content-Length")
	// A gzip stream is not byte-range addressable.
	h.Del("Accept-Ranges")
	g.zw = getWriter(g.ResponseWriter, g.level)
	g.sendHeader()
	if len(g.buf) > 0 {
		_, _ = g.zw.Write(g.buf)
		g.buf = nil
	}
}

// commitIdentity ends a pending decision in favour of the identity body:
// the response finished under minCompressLength bytes.
func (g *gzipWriter) commitIdentity() {
	g.pending = false
	g.sendHeader()
	if len(g.buf) > 0 {
		_, _ = g.ResponseWriter.Write(g.buf)
		g.buf = nil
	}
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
	if g.pending {
		g.buf = append(g.buf, p...)
		if len(g.buf) >= minCompressLength {
			g.commitGzip()
		}
		return len(p), nil
	}
	if g.zw != nil {
		return g.zw.Write(p)
	}
	return g.ResponseWriter.Write(p)
}

// Flush ends the current deflate block (Z_SYNC_FLUSH) before flushing the
// connection. Every buffered byte therefore becomes decodable by the client
// at the moment the handler asked for it — the SSE per-event contract.
func (g *gzipWriter) Flush() { _ = g.FlushError() }

// FlushError is Flush with the connection's error. http.ResponseController
// prefers this method over Flush, so a write deadline that trips while
// flushing surfaces to the handler exactly as it does without the wrapper.
// The SSE loop relies on that return value to drop a stalled subscriber.
func (g *gzipWriter) FlushError() error {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	if g.pending {
		// A handler that flushes is streaming; stop waiting for a length.
		g.commitGzip()
	}
	g.sendHeader()
	var err error
	if g.zw != nil {
		err = g.zw.Flush()
	}
	switch f := g.ResponseWriter.(type) {
	case interface{ FlushError() error }:
		if e := f.FlushError(); err == nil {
			err = e
		}
	case http.Flusher:
		f.Flush()
	}
	return err
}

// Hijack keeps the writer transparent to anything that takes the connection
// over (nothing compressible does today, since upgrades are not wrapped).
func (g *gzipWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := g.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	g.close()
	return hj.Hijack()
}

// close finishes the response: settles a pending decision and writes the
// gzip trailer. Called when the handler returns.
func (g *gzipWriter) close() {
	if g.pending {
		g.commitIdentity()
	}
	if g.zw == nil {
		return
	}
	_ = g.zw.Close() // writes the gzip trailer
	putWriter(g.zw, g.level)
	g.zw = nil
}
