// Package discovery — runner HTTP-over-unix-socket helper.
//
// gmuxd talks to each runner over a per-session AF_UNIX socket using
// HTTP. Every call site here is short: a single request and
// response, then done. AF_UNIX dials cost ~10 µs of kernel work
// (no network round-trip to amortize), so connection pooling earns
// almost nothing while introducing a lifecycle (idle pool, drain,
// orphan-conn races) that has to be reasoned about and tested.
//
// One helper, runnerRequest. Each call dials, requests, closes.
// req.Close = true tells the transport to close the underlying FD
// when the response body is closed instead of returning it to the
// idle pool, so a per-call transport has nothing to leak when it
// goes out of scope.
//
// This package exists because three call sites previously
// open-coded the same &http.Transport{DialContext: unix-dial}
// closure and discarded the transport after each request. Closing
// the response body returned the connection to the abandoned
// transport's idle pool instead of closing the FD; on a busy
// daemon this exhausted RLIMIT_NOFILE within hours. See #197.
package discovery

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// runnerDialTimeout bounds the time spent connecting to a runner's
// Unix socket. The socket is local, so any wait beyond a couple of
// seconds means the runner is wedged or gone.
const runnerDialTimeout = 2 * time.Second

// runnerRequestTimeout bounds end-to-end request time. Matches the
// historical timeout the per-call clients used.
const runnerRequestTimeout = 3 * time.Second

// newUnixTransport returns an *http.Transport that dials socketPath
// for every connection regardless of the request URL's host. The
// transport is single-use; req.Close on the request guarantees the
// connection closes with the response, so the transport has no
// idle pool to manage and is safe to discard.
func newUnixTransport(socketPath string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: runnerDialTimeout}
			return d.DialContext(ctx, "unix", socketPath)
		},
	}
}

// runnerRequest issues a single HTTP request to the runner at
// socketPath and returns the response. The request is marked
// Close=true so the connection closes with the response body
// rather than being pooled.
//
// The caller is responsible for closing resp.Body. ctx bounds the
// total request lifetime; the runner timeout (3 s) is nested under
// it, so a caller with a tighter ctx still wins. body may be nil
// for parameterless GET / POST.
//
// Use any host placeholder in the URL; the transport ignores it.
func runnerRequest(ctx context.Context, socketPath, method, urlPath string, body io.Reader) (*http.Response, error) {
	// Nest the runner timeout under the caller's context. The
	// effective deadline is the earlier of the two.
	// http.NewRequestWithContext panics on a nil ctx, matching
	// stdlib convention for the programmer-error case.
	ctx, cancel := context.WithTimeout(ctx, runnerRequestTimeout)
	// We can't defer cancel: the response body must outlive this
	// function, and cancel needs to fire when the body is closed
	// (wired below via cancelOnCloseBody).

	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+urlPath, body)
	if err != nil {
		cancel()
		return nil, err
	}
	// Mark the connection for closure after the response. With
	// this set, the transport closes the underlying FD when the
	// body is closed, instead of returning it to the idle pool.
	req.Close = true

	tr := newUnixTransport(socketPath)
	client := &http.Client{Transport: tr}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelOnCloseBody calls cancel() exactly once when the underlying
// body is closed, releasing the timeout context's resources.
// sync.Once gives both idempotency and race-detector safety in case
// a caller's defer-Close pattern composes with another Close call
// from a different goroutine.
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.cancel)
	return err
}
