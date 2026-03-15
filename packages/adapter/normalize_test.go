package adapter

import "testing"

func TestNormalizeScrollback(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{
			name: "plain text",
			raw:  []byte("hello world"),
			want: "hello world",
		},
		{
			name: "ANSI colors",
			raw:  []byte("\x1b[1m\x1b[34mHere's how to fix that:\x1b[0m"),
			want: "Here's how to fix that:",
		},
		{
			name: "CSI sequences",
			raw:  []byte("\x1b[2J\x1b[H\x1b[?2004hsome text\x1b[0m"),
			want: "some text",
		},
		{
			name: "OSC title",
			raw:  []byte("\x1b]0;my title\x07visible text"),
			want: "visible text",
		},
		{
			name: "whitespace collapse",
			raw:  []byte("  hello   \n\n  world  "),
			want: "hello world",
		},
		{
			name: "control chars stripped",
			raw:  []byte("hello\x00\x01\x02world"),
			want: "helloworld",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeScrollback(tt.raw)
			if got != tt.want {
				t.Errorf("NormalizeScrollback() = %q, want %q", got, tt.want)
			}
		})
	}
}
