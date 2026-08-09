package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionstream"
)

type transactionWriter struct {
	header    http.Header
	body      bytes.Buffer
	deadlines []time.Time
	flushes   int
	onFlush   func(int)
}

func (w *transactionWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (*transactionWriter) WriteHeader(int)               {}
func (w *transactionWriter) Write(p []byte) (int, error) { return w.body.Write(p) }
func (w *transactionWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}
func (w *transactionWriter) FlushError() error {
	w.flushes++
	if w.onFlush != nil {
		w.onFlush(w.flushes)
	}
	return nil
}

func TestSessionTransactionUsesOneDeadlineAndChecksCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := &transactionWriter{onFlush: func(n int) {
		if n == 1 {
			cancel()
		}
	}}
	events := []sessionstream.Event{{Type: "one", Data: []byte(`{"n":1}`)}, {Type: "two", Data: []byte(`{"n":2}`)}}
	err := sendSSETransaction(ctx, http.NewResponseController(w), w, events)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if w.flushes != 1 || bytes.Contains(w.body.Bytes(), []byte("event: two")) {
		t.Fatalf("flushes=%d body=%s", w.flushes, w.body.String())
	}
	if len(w.deadlines) != 2 || w.deadlines[0].IsZero() || !w.deadlines[1].IsZero() {
		t.Fatalf("deadlines=%v, want one transaction deadline then clear", w.deadlines)
	}
}
