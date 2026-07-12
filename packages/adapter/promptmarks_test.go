package adapter

import (
	"reflect"
	"strings"
	"testing"
)

// feedAll runs one Feed call per chunk and returns the reported
// transition sequence (true = working).
func feedAll(t *testing.T, chunks ...string) []bool {
	t.Helper()
	var got []bool
	tr := NewPromptMarkTracker(func(working bool) { got = append(got, working) })
	for _, c := range chunks {
		tr.Feed([]byte(c))
	}
	return got
}

func TestPromptMarkTrackerTransitions(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		want   []bool
	}{
		{
			name:   "first prompt reports idle",
			chunks: []string{"\x1b]133;A\x07$ "},
			want:   []bool{false},
		},
		{
			name: "full prompt cycle: busy on C, idle on next A",
			chunks: []string{
				"\x1b]133;A\x07$ ",                 // prompt
				"\x1b]133;C\x07compiling\n",        // command starts
				"\x1b]133;D;0\x07\x1b]133;A\x07$ ", // finished + next prompt
			},
			want: []bool{false, true, false},
		},
		{
			name: "C and A in one chunk still produce the full pulse",
			chunks: []string{
				"\x1b]133;A\x07$ ",
				"\x1b]133;C\x07hi\n\x1b]133;D;0\x07\x1b]133;A\x07$ ",
			},
			want: []bool{false, true, false},
		},
		{
			name:   "ST terminator recognized",
			chunks: []string{"\x1b]133;C\x1b\\"},
			want:   []bool{true},
		},
		{
			name:   "D alone reports idle (integrations that skip A)",
			chunks: []string{"\x1b]133;C\x07out\x1b]133;D;127\x07"},
			want:   []bool{true, false},
		},
		{
			name:   "repeated idle marks dedupe",
			chunks: []string{"\x1b]133;A\x07", "\x1b]133;A\x07", "\x1b]133;D\x07"},
			want:   []bool{false},
		},
		{
			name:   "B marks are ignored",
			chunks: []string{"\x1b]133;A\x07prompt> \x1b]133;B\x07"},
			want:   []bool{false},
		},
		{
			name:   "kitty-style params after the mark letter",
			chunks: []string{"\x1b]133;A;k=s\x07"},
			want:   []bool{false},
		},
		{
			name:   "sequence split across chunks",
			chunks: []string{"out\x1b]13", "3;C", "\x07more"},
			want:   []bool{true},
		},
		{
			name:   "terminator split across chunks (ESC then backslash)",
			chunks: []string{"\x1b]133;A\x1b", "\\$ "},
			want:   []bool{false},
		},
		{
			name:   "unrelated OSC ignored",
			chunks: []string{"\x1b]0;my title\x07\x1b]2;other\x1b\\"},
			want:   nil,
		},
		{
			name:   "long unrelated OSC payload does not hide later marks",
			chunks: []string{"\x1b]0;" + strings.Repeat("x", 4096) + "\x07\x1b]133;C\x07"},
			want:   []bool{true},
		},
		{
			name:   "133 prefix with junk instead of mark letter ignored",
			chunks: []string{"\x1b]133;Zoo\x07\x1b]133;AB\x07"},
			want:   nil,
		},
		{
			name:   "CSI sequences between marks are skipped",
			chunks: []string{"\x1b]133;A\x07\x1b[2J\x1b[1;1H\x1b]133;C\x07"},
			want:   []bool{false, true},
		},
		{
			name:   "aborted OSC followed by a new OSC (ESC ])",
			chunks: []string{"\x1b]0;unterminated\x1b]133;C\x07"},
			want:   []bool{true},
		},
		{
			name:   "plain output produces nothing",
			chunks: []string{"hello world\nno escapes here\n"},
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := feedAll(t, tt.chunks...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("transitions = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPromptMarkTrackerByteAtATime pins the streaming property in the
// most hostile chunking: every byte arrives in its own Feed call.
func TestPromptMarkTrackerByteAtATime(t *testing.T) {
	stream := "\x1b]133;A\x07$ \x1b]133;C\x07work\x1b]133;D;0\x1b\\\x1b]133;A\x07$ "
	var got []bool
	tr := NewPromptMarkTracker(func(working bool) { got = append(got, working) })
	for i := 0; i < len(stream); i++ {
		tr.Feed([]byte{stream[i]})
	}
	want := []bool{false, true, false}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("transitions = %v, want %v", got, want)
	}
}

// TestPromptMarkTrackerPayloadBounded guards the memory bound: an OSC
// spammed with megabytes of payload must not grow the tracker's buffer.
func TestPromptMarkTrackerPayloadBounded(t *testing.T) {
	tr := NewPromptMarkTracker(nil)
	tr.Feed([]byte("\x1b]0;"))
	for i := 0; i < 1000; i++ {
		tr.Feed([]byte(strings.Repeat("y", 1024)))
	}
	if cap(tr.payload) > promptPayloadCap {
		t.Errorf("payload cap grew to %d, want <= %d", cap(tr.payload), promptPayloadCap)
	}
}
