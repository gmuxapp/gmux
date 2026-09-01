package conversations

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// fakeConvAdapter is a synthetic ConversationSource + ConversationDescriber +
// Resumer. Refs encode "id|title|cwd" so DescribeConversation is pure.
type fakeConvAdapter struct {
	name     string
	refs     []string
	delay    time.Duration // per-snapshot artificial scan duration
	inFlight *atomic.Int32 // shared across adapters: concurrent snapshots
	maxSeen  *atomic.Int32 // shared: high-water mark of inFlight

	// describeStarted/describeGate, when set, make DescribeConversation
	// announce entry and then block until the gate closes — a deterministic
	// stand-in for a slow file read/parse, used to interleave watcher events
	// with an in-flight scan.
	describeStarted chan string
	describeGate    chan struct{}
}

func (f *fakeConvAdapter) Name() string                        { return f.name }
func (f *fakeConvAdapter) Discover() bool                      { return true }
func (f *fakeConvAdapter) Match([]string) bool                 { return false }
func (f *fakeConvAdapter) Env(adapter.EnvContext) []string     { return nil }
func (f *fakeConvAdapter) SnapshotConversations(sink adapter.ConversationSink) {
	if f.inFlight != nil {
		n := f.inFlight.Add(1)
		for {
			m := f.maxSeen.Load()
			if n <= m || f.maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		defer f.inFlight.Add(-1)
	}
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	for _, ref := range f.refs {
		sink.Upsert(ref)
	}
}
func (f *fakeConvAdapter) WatchConversations(ctx context.Context, _ adapter.ConversationSink) error {
	<-ctx.Done()
	return ctx.Err()
}
func (f *fakeConvAdapter) DescribeConversation(ref string) (*adapter.ConversationInfo, error) {
	if f.describeStarted != nil {
		f.describeStarted <- ref
	}
	if f.describeGate != nil {
		<-f.describeGate
	}
	parts := strings.SplitN(ref, "|", 3)
	return &adapter.ConversationInfo{
		ID:      parts[0],
		Title:   parts[1],
		Slug:    adapter.Slugify(parts[1]),
		Cwd:     parts[2],
		Created: time.Unix(1700000000, 0),
	}, nil
}
func (f *fakeConvAdapter) ResumeCommand(info *adapter.ConversationInfo) []string {
	return []string{f.name, "--resume", info.ID}
}

func withFakeAdapters(t *testing.T, fakes []*fakeConvAdapter) {
	t.Helper()
	prev := allAdapters
	allAdapters = func() []adapter.Adapter {
		out := make([]adapter.Adapter, len(fakes))
		for i, f := range fakes {
			out[i] = f
		}
		return out
	}
	t.Cleanup(func() { allAdapters = prev })
}

func makeFakes(n, refsPer int, delay time.Duration) ([]*fakeConvAdapter, *atomic.Int32) {
	var inFlight, maxSeen atomic.Int32
	fakes := make([]*fakeConvAdapter, n)
	for i := range fakes {
		name := "fake" + string(rune('a'+i))
		refs := make([]string, refsPer)
		for j := range refs {
			id := name + "-conv-" + string(rune('0'+j))
			refs[j] = id + "|Title " + id + "|/tmp/" + name
		}
		fakes[i] = &fakeConvAdapter{name: name, refs: refs, delay: delay, inFlight: &inFlight, maxSeen: &maxSeen}
	}
	return fakes, &maxSeen
}

// Bounded concurrency: with 4 sources and 2 workers, at most 2 snapshot scans
// are ever in flight; onComplete fires exactly once after all of them, and
// only then does SnapshotComplete report true.
func TestStartSnapshotBoundedConcurrency(t *testing.T) {
	fakes, maxSeen := makeFakes(4, 3, 40*time.Millisecond)
	withFakeAdapters(t, fakes)

	idx := New()
	if idx.SnapshotComplete() {
		t.Fatal("fresh index must not report snapshot complete")
	}
	done := make(chan struct{})
	var adapterDone atomic.Int32
	idx.StartSnapshot(context.Background(), 2, func(string) { adapterDone.Add(1) }, func() { close(done) })

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartSnapshot did not complete")
	}
	if got := maxSeen.Load(); got > 2 {
		t.Fatalf("concurrency bound violated: %d scans in flight (want <= 2)", got)
	}
	if got := adapterDone.Load(); got != 4 {
		t.Fatalf("onAdapterIndexed calls = %d, want 4", got)
	}
	if !idx.SnapshotComplete() {
		t.Fatal("SnapshotComplete false after onComplete")
	}
	if idx.Count() != 12 {
		t.Fatalf("indexed %d conversations, want 12", idx.Count())
	}
}

// Differential: the async snapshot converges to exactly the state the
// synchronous blocking Snapshot produces for the same sources.
func TestStartSnapshotDifferentialWithBlockingSnapshot(t *testing.T) {
	fakes, _ := makeFakes(3, 5, 0)
	// Force a slug collision across adapters' shared logic within one adapter.
	fakes[0].refs = append(fakes[0].refs, "dup-1|Same Title|/tmp/x", "dup-2|Same Title|/tmp/y")
	withFakeAdapters(t, fakes)

	blocking := New()
	blocking.Snapshot()

	async := New()
	done := make(chan struct{})
	async.StartSnapshot(context.Background(), 2, nil, func() { close(done) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartSnapshot did not complete")
	}

	sortInfos := func(in []Info) []Info {
		sort.Slice(in, func(i, j int) bool {
			if in[i].Adapter != in[j].Adapter {
				return in[i].Adapter < in[j].Adapter
			}
			return in[i].ConversationID < in[j].ConversationID
		})
		return in
	}
	want, got := sortInfos(blocking.All()), sortInfos(async.All())
	// Collision suffix assignment is arrival-order-dependent (-2 goes to the
	// second arrival); with concurrent scans either dup may get the suffix.
	// Compare identity on everything except which twin got suffixed: both
	// twins must exist, with distinct keys drawn from the same suffix set.
	normalize := func(infos []Info) ([]Info, map[string][]string) {
		keys := map[string][]string{}
		out := make([]Info, 0, len(infos))
		for _, info := range infos {
			base := strings.TrimSuffix(strings.TrimSuffix(info.Key, "-2"), "-3")
			keys[info.Adapter+"/"+base] = append(keys[info.Adapter+"/"+base], info.Key)
			info.Key = base
			info.Slug = strings.TrimSuffix(strings.TrimSuffix(info.Slug, "-2"), "-3")
			out = append(out, info)
		}
		for _, v := range keys {
			sort.Strings(v)
		}
		return out, keys
	}
	wantN, wantKeys := normalize(want)
	gotN, gotKeys := normalize(got)
	if !reflect.DeepEqual(wantN, gotN) {
		t.Fatalf("async index state diverged from blocking snapshot:\nblocking: %+v\nasync:    %+v", wantN, gotN)
	}
	if !reflect.DeepEqual(wantKeys, gotKeys) {
		t.Fatalf("key/suffix sets diverged:\nblocking: %v\nasync:    %v", wantKeys, gotKeys)
	}
	// Resume commands must be resolvable for every ref in both.
	for _, f := range fakes {
		for _, ref := range f.refs {
			if len(async.LookupResumeCommand(f.name, ref)) == 0 {
				t.Fatalf("async index missing resume command for %s/%s", f.name, ref)
			}
		}
	}
}

// Watcher events racing the startup scan (the production topology: watchers
// start before StartSnapshot) must not duplicate or lose conversations —
// Upsert is keyed by adapter-native conversation ID.
func TestStartSnapshotConcurrentSourceEvents(t *testing.T) {
	fakes, _ := makeFakes(2, 20, 10*time.Millisecond)
	withFakeAdapters(t, fakes)

	idx := New()
	done := make(chan struct{})
	idx.StartSnapshot(context.Background(), 2, nil, func() { close(done) })

	// Simulate watchers re-upserting every ref plus brand-new refs while the
	// scan is running.
	extra := make(map[string][]string)
	for _, f := range fakes {
		extra[f.name] = []string{f.name + "-live-1|Live One|/tmp/live", f.name + "-live-2|Live Two|/tmp/live"}
	}
	var wg sync.WaitGroup
	for _, f := range fakes {
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func(f *fakeConvAdapter) {
				defer wg.Done()
				for _, ref := range append(append([]string(nil), f.refs...), extra[f.name]...) {
					idx.Scan(f, ref)
				}
			}(f)
		}
	}
	wg.Wait()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartSnapshot did not complete")
	}

	// 2 adapters x (20 snapshot + 2 live) unique conversation IDs.
	if idx.Count() != 44 {
		t.Fatalf("indexed %d conversations, want 44 (duplicate or lost entries)", idx.Count())
	}
	for _, f := range fakes {
		for _, ref := range append(append([]string(nil), f.refs...), extra[f.name]...) {
			id := strings.SplitN(ref, "|", 2)[0]
			if idx.LookupByConversationID(f.name, id) == "" {
				t.Fatalf("conversation %s/%s lost", f.name, id)
			}
		}
	}
}

// Reviewer F1 zombie schedule (PR #517): a watcher Remove landing between
// Scan's unlocked DescribeConversation and its commit must win — otherwise
// the scan resurrects a deleted conversation and no future event ever clears
// it. Both variants: the ref was never indexed (startup scan) and the ref was
// already indexed (rescan).
func TestScanRemoveRaceDeletionWins(t *testing.T) {
	const ref = "conv-1|Some Title|/tmp/x"

	run := func(t *testing.T, preIndex bool) {
		idx := New()
		plain := &fakeConvAdapter{name: "probe"}
		if preIndex {
			if key := idx.Scan(plain, ref); key == "" {
				t.Fatal("pre-index scan failed")
			}
		}

		gated := &fakeConvAdapter{name: "probe", describeStarted: make(chan string, 1), describeGate: make(chan struct{})}
		done := make(chan string)
		go func() { done <- idx.Scan(gated, ref) }()
		<-gated.describeStarted // scan is inside DescribeConversation, no lock held

		// The watcher observes the deletion mid-scan (indexSink.Remove path).
		removed := idx.RemoveByRef("probe", ref)
		if preIndex && !removed {
			t.Fatal("RemoveByRef did not find the pre-indexed entry")
		}

		close(gated.describeGate) // describe returns; scan tries to commit
		if key := <-done; key != "" {
			t.Fatalf("zombie: scan committed key %q after the ref was removed", key)
		}
		if n := idx.Count(); n != 0 {
			t.Fatalf("zombie: %d conversation(s) indexed after removal", n)
		}
		if idx.LookupByConversationID("probe", "conv-1") != "" {
			t.Fatal("zombie: conversation ID resolvable after removal")
		}
		if cmd := idx.LookupResumeCommand("probe", ref); len(cmd) != 0 {
			t.Fatalf("zombie: resume command %v cached after removal", cmd)
		}

		// A fresh scan after the removal (file re-created) must index again:
		// the generation guard only drops scans that predate the removal.
		if key := idx.Scan(plain, ref); key == "" {
			t.Fatal("post-removal rescan refused to index a re-created conversation")
		}
	}

	t.Run("unindexed-ref", func(t *testing.T) { run(t, false) })
	t.Run("pre-indexed-ref", func(t *testing.T) { run(t, true) })
}

// A context cancelled before scans run must leave the snapshot incomplete:
// the index may be partial and must not advertise authority over absence.
func TestStartSnapshotCancelledContext(t *testing.T) {
	fakes, _ := makeFakes(2, 2, 0)
	withFakeAdapters(t, fakes)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	idx := New()
	completed := make(chan struct{})
	idx.StartSnapshot(ctx, 1, nil, func() { close(completed) })

	select {
	case <-completed:
		t.Fatal("onComplete fired despite cancelled context")
	case <-time.After(200 * time.Millisecond):
	}
	if idx.SnapshotComplete() {
		t.Fatal("cancelled snapshot must not report complete")
	}
}
