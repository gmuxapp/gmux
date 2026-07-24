package main

import (
	"context"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

func TestBootstrapCloseJoinsOwnedBlockingWorker(t *testing.T) {
	st, err := centralstore.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	eps := EndpointSourceFunc(func(context.Context) ([]string, error) { close(entered); <-release; return nil, nil })
	b, err := newBootstrap(BootstrapConfig{Store: st, Runners: &bootstrapRunners{metas: map[string]sessioncoord.RunnerMeta{}, blocked: map[string]bool{}}, Converter: &wire.Converter{}, Endpoints: eps})
	if err != nil {
		t.Fatal(err)
	}
	ticks := make(chan time.Time, 1)
	b.StartOwnedTriggers(TriggerConfig{Tick: ticks})
	ticks <- time.Now()
	<-entered
	done := make(chan struct{})
	go func() { b.Close(); close(done) }()
	select {
	case <-done:
		t.Fatal("Close returned before owned worker joined")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not join worker")
	}
}
