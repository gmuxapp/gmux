package sessionstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

type testRow struct {
	ID               string            `json:"id"`
	CreatedAt        string            `json:"created_at"`
	Command          []string          `json:"command"`
	Cwd              string            `json:"cwd"`
	Adapter          string            `json:"adapter"`
	WorkspaceRoot    string            `json:"workspace_root"`
	Remotes          map[string]string `json:"remotes"`
	SemanticAgent    bool              `json:"semantic_agent"`
	Alive            bool              `json:"alive"`
	PID              int               `json:"pid"`
	Title            string            `json:"title"`
	Subtitle         string            `json:"subtitle"`
	UnreadToken      string            `json:"unread_token"`
	SocketPath       string            `json:"socket_path"`
	Slug             string            `json:"slug"`
	ConversationFile string            `json:"conversation_file"`
	RunnerVersion    string            `json:"runner_version"`
	BinaryHash       string            `json:"binary_hash"`
	ProjectSlug      string            `json:"project_slug"`
	ProjectIndex     int               `json:"project_index"`
}

func fixtureRows(n int) []testRow {
	rows := make([]testRow, n)
	for i := range rows {
		id := fmt.Sprintf("%08x", i)
		rows[i] = testRow{
			ID: id, CreatedAt: "2026-08-09T12:00:00Z",
			Command: []string{"pi", "--mode", "rpc", "work on semantic session streaming"},
			Cwd:     fmt.Sprintf("/home/user/dev/project-%d", i%25), Adapter: "pi",
			WorkspaceRoot: fmt.Sprintf("/home/user/dev/project-%d", i%25),
			Remotes:       map[string]string{"origin": "github.com/gmuxapp/gmux"}, SemanticAgent: true,
			Alive: i%3 != 0, PID: 10000 + i, Title: fmt.Sprintf("Implement production task %d", i),
			Subtitle: "Working through tests and review feedback", UnreadToken: "token-" + id,
			SocketPath: "/run/user/1000/gmux/" + id + ".sock", Slug: fmt.Sprintf("production-task-%d", i),
			ConversationFile: "/home/user/.pi/agent/sessions/" + id + ".jsonl",
			RunnerVersion:    "2.1.0", BinaryHash: "abcdef0123456789",
			ProjectSlug: fmt.Sprintf("project-%d", i%25), ProjectIndex: i / 25,
		}
	}
	return rows
}

func TestLegacySnapshotReproducesDefaultScannerFailure(t *testing.T) {
	payload, err := json.Marshal(struct {
		Sessions []testRow `json:"sessions"`
	}{fixtureRows(1000)})
	if err != nil {
		t.Fatal(err)
	}
	frame := append([]byte("event: snapshot.sessions\ndata: "), payload...)
	frame = append(frame, '\n', '\n')
	scanner := bufio.NewScanner(bytes.NewReader(frame))
	for scanner.Scan() {
	}
	if scanner.Err() == nil || !strings.Contains(scanner.Err().Error(), "token too long") {
		t.Fatalf("legacy scanner error=%v, want token too long (payload=%d)", scanner.Err(), len(payload))
	}
}

func TestEncodeEmptyAndOneRow(t *testing.T) {
	for _, rows := range [][]testRow{nil, fixtureRows(1)} {
		events, err := Encode(7, rows, func(r testRow) string { return r.ID })
		if err != nil {
			t.Fatal(err)
		}
		want := 2
		if len(rows) == 1 {
			want = 3
		}
		if len(events) != want || events[0].Type != EventBegin || events[len(events)-1].Type != EventReady {
			t.Fatalf("events=%v, want begin/[batch]/ready", eventTypes(events))
		}
	}
}

func TestEncodeLargeCorpusBoundsEveryEventAndMeasures(t *testing.T) {
	rows := fixtureRows(1000)
	legacy, err := json.Marshal(struct {
		Sessions []testRow `json:"sessions"`
	}{rows})
	if err != nil {
		t.Fatal(err)
	}
	events, err := Encode(1, rows, func(r testRow) string { return r.ID })
	if err != nil {
		t.Fatal(err)
	}
	maxPayload, total := 0, 0
	var gotRows int
	for _, event := range events {
		if len(event.Data) > MaxEventPayload {
			t.Fatalf("%s payload=%d exceeds %d", event.Type, len(event.Data), MaxEventPayload)
		}
		lineBytes := len("data: ") + len(event.Data)
		if lineBytes >= bufio.MaxScanTokenSize {
			t.Fatalf("%s SSE data line=%d is not below Scanner's %d-byte default", event.Type, lineBytes, bufio.MaxScanTokenSize)
		}
		maxPayload = max(maxPayload, len(event.Data))
		total += len("event: ") + len(event.Type) + 1 + lineBytes + 2
		if event.Type == EventBatch {
			var batch Batch[testRow]
			if err := json.Unmarshal(event.Data, &batch); err != nil {
				t.Fatal(err)
			}
			gotRows += len(batch.Sessions)
		}
	}
	if gotRows != len(rows) {
		t.Fatalf("decoded rows=%d want %d", gotRows, len(rows))
	}
	oldTotal := len("event: snapshot.sessions\ndata: \n\n") + len(legacy)
	t.Logf("1000 rows: old max event=%d total=%d events=1; new max event=%d total=%d events=%d", len(legacy), oldTotal, maxPayload, total, len(events))
}

func TestEncodeQuarantinesOversizedRowAndCompletesReady(t *testing.T) {
	good := fixtureRows(1)[0]
	bad := fixtureRows(1)[0]
	bad.ID = strings.Repeat("unsafe-id", 40)
	bad.Command = []string{strings.Repeat("x", MaxEventPayload)}
	events, err := Encode(1, []testRow{bad, good}, func(r testRow) string { return r.ID })
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Type != EventBegin || events[len(events)-1].Type != EventReady {
		t.Fatalf("events=%v", eventTypes(events))
	}
	var gotRows int
	var diagnostic Error
	for _, event := range events {
		switch event.Type {
		case EventBatch:
			var batch Batch[testRow]
			if err := json.Unmarshal(event.Data, &batch); err != nil {
				t.Fatal(err)
			}
			gotRows += len(batch.Sessions)
		case EventError:
			if err := json.Unmarshal(event.Data, &diagnostic); err != nil {
				t.Fatal(err)
			}
		}
	}
	if gotRows != 1 || diagnostic.Code != "row_too_large" || diagnostic.Count != 1 || !strings.HasPrefix(diagnostic.ID, "sha256:") {
		t.Fatalf("rows=%d diagnostic=%+v", gotRows, diagnostic)
	}
	if len(streamData(t, diagnostic)) > MaxEventPayload {
		t.Fatal("diagnostic is not bounded")
	}
}

func TestEncodeDiagnosticSummaryPreservesTotalAboveDetailCap(t *testing.T) {
	rows := make([]testRow, 300)
	huge := strings.Repeat("x", MaxEventPayload)
	for i := range rows {
		rows[i] = fixtureRows(1)[0]
		rows[i].ID = fmt.Sprintf("oversized-%03d", i)
		rows[i].Command = []string{huge}
	}
	events, err := Encode(1, rows, func(r testRow) string { return r.ID })
	if err != nil {
		t.Fatal(err)
	}
	details, total := 0, 0
	var summary Error
	for _, event := range events {
		if event.Type != EventError {
			continue
		}
		var diagnostic Error
		if err := json.Unmarshal(event.Data, &diagnostic); err != nil {
			t.Fatal(err)
		}
		total += diagnostic.Count
		if diagnostic.ID != "" {
			details++
		} else {
			summary = diagnostic
		}
	}
	if details != maxDiagnostics || total != len(rows) || summary.Code != "diagnostics_suppressed" || summary.Count != 44 {
		t.Fatalf("details=%d total=%d summary=%+v", details, total, summary)
	}
}

func TestSenderLimitsAreStrictlyInsideReceiverLimits(t *testing.T) {
	if transactionWouldOverflow(MaxStagedRows-1, 0, 1) {
		t.Fatal("last supported row rejected")
	}
	if !transactionWouldOverflow(MaxStagedRows, 0, 1) {
		t.Fatal("row-count limit not enforced")
	}
	acceptedLimit := MaxStagedBytes - aggregateEnvelopeReserve
	if transactionWouldOverflow(0, acceptedLimit-1, 1) {
		t.Fatal("last supported byte rejected")
	}
	if !transactionWouldOverflow(0, acceptedLimit, 1) {
		t.Fatal("aggregate byte limit not enforced")
	}
	if aggregateEnvelopeReserve < MaxEventPayload {
		t.Fatal("no envelope safety margin")
	}
}

func streamData(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func BenchmarkInitialSync1000(b *testing.B) {
	rows := fixtureRows(1000)
	b.Run("legacy-single-event", func(b *testing.B) {
		for b.Loop() {
			data, err := json.Marshal(struct {
				Sessions []testRow `json:"sessions"`
			}{rows})
			if err != nil {
				b.Fatal(err)
			}
			_ = data
		}
	})
	b.Run("semantic-batches", func(b *testing.B) {
		for b.Loop() {
			events, err := Encode(1, rows, func(r testRow) string { return r.ID })
			if err != nil {
				b.Fatal(err)
			}
			_ = events
		}
	})
}

func eventTypes(events []Event) []string {
	out := make([]string, len(events))
	for i := range events {
		out[i] = events[i].Type
	}
	return out
}
