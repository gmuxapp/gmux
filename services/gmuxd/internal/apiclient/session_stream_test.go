package apiclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/sseclient"
)

func TestEventsExplicitlyNegotiatesBoundedSessionStream(t *testing.T) {
	var as, stream string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		as = r.URL.Query().Get("as")
		stream = r.URL.Query().Get("session_stream")
		w.Header().Set("Content-Type", "text/event-stream")
	}))
	defer server.Close()
	err := New(server.URL).Events().Subscribe(context.Background(), nil, func(sseclient.Event) {})
	if !errors.Is(err, sseclient.ErrStreamEnded) {
		t.Fatalf("Subscribe=%v", err)
	}
	if as != "peer" || stream != "3" {
		t.Fatalf("query as=%q session_stream=%q", as, stream)
	}
}
