package main

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestANSIStrippingWriter(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"plain", []string{"hello\n"}, "hello\n"},
		{"crlf normalised", []string{"one\r\ntwo\r\n"}, "one\ntwo\n"},
		{"bare cr collapsed", []string{"50%\rdone\n"}, "50%done\n"},
		{"csi stripped", []string{"\x1b[31mred\x1b[0m\n"}, "red\n"},
		{"osc bel stripped", []string{"\x1b]0;title\x07text\n"}, "text\n"},
		{"osc st stripped", []string{"\x1b]0;title\x1b\\text\n"}, "text\n"},
		{"dcs stripped", []string{"before\x1bPq?sixel-data\x1b\\after"}, "beforeafter"},
		{"string controls stripped", []string{"a\x1bXprivate\x1b\\b\x1b^private\x1b\\c\x1b_private\x1b\\d"}, "abcd"},
		{"two-byte escape stripped", []string{"\x1b=x\n"}, "x\n"},
		// The reason this type exists: sequences split across Write calls.
		{"csi split across writes", []string{"a\x1b[3", "1mred\x1b[", "0mb\n"}, "aredb\n"},
		{"esc at chunk boundary", []string{"a\x1b", "[1mb"}, "ab"},
		{"crlf split across writes", []string{"one\r", "\ntwo"}, "one\ntwo"},
		{"osc split across writes", []string{"\x1b]0;ti", "tle\x07ok"}, "ok"},
		{"osc st split across writes", []string{"\x1b]0;title\x1b", "\\ok"}, "ok"},
		{"dcs split across writes", []string{"before\x1b", "Pq?sixel-", "data\x1b", "\\after"}, "beforeafter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			w := newANSIStrippingWriter(&out)
			for _, chunk := range tc.chunks {
				n, err := w.Write([]byte(chunk))
				if err != nil || n != len(chunk) {
					t.Fatalf("Write(%q) = (%d, %v), want (%d, nil)", chunk, n, err, len(chunk))
				}
			}
			if out.String() != tc.want {
				t.Errorf("got %q, want %q", out.String(), tc.want)
			}
		})
	}
}

func TestANSIStrippingWriterRejectsShortWrite(t *testing.T) {
	w := newANSIStrippingWriter(shortWriter{})
	if _, err := w.Write([]byte("text")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Write error = %v, want io.ErrShortWrite", err)
	}
}
