package discovery

// runnerhttp.go — HTTP-over-AF_UNIX helper for talking to runners.
//
// gmuxd talks to each runner over a per-session Unix socket using
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
// This file exists because three call sites previously open-coded
// the same &http.Transport{DialContext: unix-dial} closure and
// discarded the transport after each request. Closing the response
// body returned the connection to the abandoned transport's idle
// pool instead of closing the FD; on a busy daemon this exhausted
// RLIMIT_NOFILE within hours. See #197.

import (
	"context"
	"io"
	"net"
	"net/http"
	"time"
)

// runnerDialTimeout bounds the time spent connecting to a runner's
// Unix socket. The socket is local, so any wait beyond a couple of
// seconds means the runner is wedged or gone.
const runnerDialTimeout = 2 * time.Second

// runnerRequestTimeout bounds end-to-end request time, including
// connect, redirects, and reading the response body. Matches the
// historical timeout the per-call clients used.
const runnerRequestTimeout = 3 * time.Second

// runnerRequest issues a single HTTP request to the runner at
// socketPath and returns the response. The request is marked
// Close=true so the connection closes with the response body
// rather than being pooled.
//
// urlPath is the path portion only (e.g. "/meta"); the host is
// fixed because the transport dials socketPath unconditionally.
// The caller is responsible for closing resp.Body. ctx bounds the
// request lifetime; the runner timeout (3 s) is applied via
// http.Client.Timeout, which composes with a tighter caller ctx
// (earlier deadline wins) and cancels its internal timer when the
// body is closed. body may be nil for parameterless GET / POST.
func runnerRequest(ctx context.Context, socketPath, method, urlPath string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+urlPath, body)
	if err != nil {
		return nil, err
	}
	// Mark the connection for closure after the response. With
	// this set, the transport closes the underlying FD when the
	// body is closed, instead of returning it to the idle pool.
	req.Close = true

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: runnerDialTimeout}
				return d.DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: runnerRequestTimeout,
	}
	return client.Do(req)
}
