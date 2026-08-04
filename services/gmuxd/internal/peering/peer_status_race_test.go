package peering

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
)

// TestFetchProjectsStatusCallbackConcurrent exercises the status read used by
// fetchProjects while the connection loop updates that status. Run with
// -race: the callback path must take the same lock as every other status read.
func TestFetchProjectsStatusCallbackConcurrent(t *testing.T) {
	var response atomic.Uint64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := response.Add(1)
		fmt.Fprintf(w, `{"configured":[{"slug":"project-%d"}]}`, n)
	}))
	defer server.Close()

	var callbacks atomic.Int64
	peer := newPeer(config.PeerConfig{Name: "box", URL: server.URL}, nil, func(_ string, status Status) {
		callbacks.Add(int64(status) + 1)
	})
	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for {
			select {
			case <-stop:
				return
			default:
				peer.setStatus(StatusConnecting)
				peer.setStatus(StatusDisconnected)
			}
		}
	}()

	for range 50 {
		peer.fetchProjects(context.Background())
	}
	close(stop)
	writer.Wait()
	if callbacks.Load() == 0 {
		t.Fatal("status callback was not exercised")
	}
}
