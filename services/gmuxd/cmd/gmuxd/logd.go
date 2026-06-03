package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// logdWriter tees log output to a local writer and a remote logd HTTP endpoint.
// The remote POST is fire-and-forget: failures are written to the local writer
// and never block the caller.
type logdWriter struct {
	local  io.Writer
	url    string
	client *http.Client
}

// newLogdWriter returns an io.Writer that writes synchronously to local and
// asynchronously POSTs each write to url with source label "backend".
func newLogdWriter(local io.Writer, url string) io.Writer {
	return &logdWriter{
		local: local,
		url:   url,
		client: &http.Client{
			Timeout: 3 * time.Second,
		},
	}
}

func (w *logdWriter) Write(p []byte) (int, error) {
	n, err := w.local.Write(p)

	// Copy p — the caller may reuse the buffer after Write returns.
	buf := make([]byte, len(p))
	copy(buf, p)

	go func() {
		req, rerr := http.NewRequestWithContext(context.Background(), http.MethodPost, w.url, bytes.NewReader(buf))
		if rerr != nil {
			fmt.Fprintf(w.local, "logd: build request: %v\n", rerr)
			return
		}
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set("X-Log-Source", "backend")
		resp, rerr := w.client.Do(req)
		if rerr != nil {
			fmt.Fprintf(w.local, "logd: post: %v\n", rerr)
			return
		}
		resp.Body.Close()
	}()

	return n, err
}
