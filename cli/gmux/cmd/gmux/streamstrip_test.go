package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
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
		{"8-bit csi stripped", []string{"a\x9b31mred\x9b0mb"}, "aredb"},
		{"8-bit osc stripped", []string{"a\x9dtitle\x9cb"}, "ab"},
		{"8-bit strings stripped", []string{"a\x90dcs\x9cb\x98sos\x9cc\x9epm\x9cd\x9fapc\x9ce"}, "abcde"},
		{"8-bit st terminates 7-bit strings", []string{"a\x1b]title\x9cb\x1bPdata\x9cc"}, "abc"},
		{"standalone c1 stripped", []string{"a\x85b\x9cc"}, "abc"},
		{"utf-8 c1-valued continuation preserved", []string{"a\xc2\x9db"}, "a\xc2\x9db"},
		// The reason this type exists: sequences split across Write calls.
		{"csi split across writes", []string{"a\x1b[3", "1mred\x1b[", "0mb\n"}, "aredb\n"},
		{"esc at chunk boundary", []string{"a\x1b", "[1mb"}, "ab"},
		{"crlf split across writes", []string{"one\r", "\ntwo"}, "one\ntwo"},
		{"osc split across writes", []string{"\x1b]0;ti", "tle\x07ok"}, "ok"},
		{"osc st split across writes", []string{"\x1b]0;title\x1b", "\\ok"}, "ok"},
		{"dcs split across writes", []string{"before\x1b", "Pq?sixel-", "data\x1b", "\\after"}, "beforeafter"},
		{"8-bit csi split across writes", []string{"a\x9b", "31", "mred"}, "ared"},
		{"8-bit osc and st split across writes", []string{"a\x9d", "title", "\x9c", "b"}, "ab"},
		{"8-bit st ends split 7-bit string", []string{"a\x1b]ti", "tle", "\x9c", "b"}, "ab"},
		{"utf-8 continuation split across writes", []string{"a\xc2", "\x9db"}, "a\xc2\x9db"},
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

func TestANSIStrippingWriterUTF8C1Continuations(t *testing.T) {
	cases := []struct {
		name string
		text []byte
	}{
		{
			"two-byte leads",
			[]byte{0xc2, 0x90, 0xc2, 0x98, 0xc2, 0x9b, 0xc2, 0x9c, 0xc2, 0x9d, 0xc2, 0x9e, 0xc2, 0x9f},
		},
		{
			"three-byte leads",
			[]byte{0xe1, 0x90, 0x98, 0xe1, 0x9b, 0x9c, 0xe1, 0x9d, 0x9e, 0xe1, 0x9f, 0x80},
		},
		{
			"four-byte leads",
			[]byte{0xf1, 0x90, 0x98, 0x9b, 0xf1, 0x9c, 0x9d, 0x9e, 0xf1, 0x9f, 0x80, 0x80},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := append(append([]byte("before-"), tc.text...), []byte("-after")...)
			if !utf8.Valid(input) {
				t.Fatal("test input is not valid UTF-8")
			}
			assertANSIStrippedAcrossChunkings(t, input, input)
		})
	}
}

func TestANSIStrippingWriterUTF8InsideControlStrings(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
	}{
		{
			"OSC",
			[]byte("before\x1b]title-Ü-still-title\x07after"),
		},
		{
			"generic string",
			[]byte("before\x1bPdata-Ü-still-data\x1b\\after"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertANSIStrippedAcrossChunkings(t, tc.input, []byte("beforeafter"))
		})
	}
}

func assertANSIStrippedAcrossChunkings(t *testing.T, input, want []byte) {
	t.Helper()

	chunkings := []struct {
		name   string
		chunks [][]byte
	}{
		{"whole write", [][]byte{input}},
	}
	byteChunks := make([][]byte, len(input))
	for i := range input {
		byteChunks[i] = input[i : i+1]
	}
	chunkings = append(chunkings, struct {
		name   string
		chunks [][]byte
	}{"byte at a time", byteChunks})
	for cut := 1; cut < len(input); cut++ {
		chunkings = append(chunkings, struct {
			name   string
			chunks [][]byte
		}{fmt.Sprintf("two chunks at %d", cut), [][]byte{input[:cut], input[cut:]}})
	}

	for _, chunking := range chunkings {
		t.Run(chunking.name, func(t *testing.T) {
			var out strings.Builder
			w := newANSIStrippingWriter(&out)
			for _, chunk := range chunking.chunks {
				n, err := w.Write(chunk)
				if err != nil || n != len(chunk) {
					t.Fatalf("Write(%q) = (%d, %v), want (%d, nil)", chunk, n, err, len(chunk))
				}
			}
			got := []byte(out.String())
			if string(got) != string(want) {
				t.Fatalf("got %q, want %q", got, want)
			}
			if !utf8.Valid(got) {
				t.Fatalf("output is not valid UTF-8: % x", got)
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
