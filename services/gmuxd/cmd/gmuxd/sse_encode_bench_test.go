package main

import (
	"fmt"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
)

// realisticSessionsPayload mirrors the ~660–720 B/session rows measured by
// the system resource profile.
func realisticSessionsPayload(n int) *wire.SessionsPayload {
	rows := make([]wire.Session, n)
	for i := range rows {
		id := fmt.Sprintf("%08x%08x", i, i*2654435761)
		rows[i] = wire.Session{
			ID: id, CreatedAt: "2026-08-30T12:00:00Z", Command: []string{"gmux", "agent", "--resume", id},
			Cwd: fmt.Sprintf("/home/user/dev/project-%02d/src", i%40), Adapter: "pi",
			WorkspaceRoot: fmt.Sprintf("/home/user/dev/project-%02d", i%40),
			Remotes:       map[string]string{"origin": fmt.Sprintf("github.com/example/project-%02d", i%40)},
			Alive:         i%10 == 0, Pid: 10000 + i, Title: fmt.Sprintf("Fix the flaky retry loop in worker %d", i),
			Subtitle: "waiting for review", Status: &wire.Status{Active: i%10 == 0},
			Unread: i%5 == 0, UnreadToken: fmt.Sprintf("tok-%d", i), Resumable: i%10 != 0,
			SocketPath: "/run/user/1000/gmux/" + id + ".sock", TerminalCols: 190, TerminalRows: 48,
			Slug: fmt.Sprintf("fix-flaky-retry-%d", i), ConversationRef: "/home/user/.pi/sessions/" + id + ".jsonl",
			RunnerVersion: "2.1.0", BinaryHash: "sha256:0011223344556677", ProjectSlug: fmt.Sprintf("project-%02d", i%40),
			ProjectIndex: i % 20, LastOutputAt: "2026-08-30T12:34:56Z",
		}
	}
	return &wire.SessionsPayload{Sessions: rows}
}

// BenchmarkSSEBroadcastEncode measures the encode work one broadcast costs
// across k concurrent subscribers (the storm hot path: every committed
// mutation pays this once per coalesced pass).
func BenchmarkSSEBroadcastEncode(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		payload := realisticSessionsPayload(n)
		for _, subs := range []int{1, 10} {
			b.Run(fmt.Sprintf("proto2/n=%d/subs=%d", n, subs), func(b *testing.B) {
				b.ReportAllocs()
				var epoch uint64
				for b.Loop() {
					epoch++
					memo := newSessionEncodeMemo(epoch, payload)
					for s := 0; s < subs; s++ {
						if _, err := memo.Proto2(false, nil); err != nil {
							b.Fatal(err)
						}
					}
				}
			})
			b.Run(fmt.Sprintf("proto3/n=%d/subs=%d", n, subs), func(b *testing.B) {
				b.ReportAllocs()
				var epoch uint64
				for b.Loop() {
					epoch++
					memo := newSessionEncodeMemo(epoch, payload)
					for s := 0; s < subs; s++ {
						if _, err := memo.Proto3(false, nil); err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}
