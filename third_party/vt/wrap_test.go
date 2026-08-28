package vt

import (
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
)

func write(t *testing.T, e *Emulator, s string) {
	t.Helper()
	if _, err := e.WriteString(s); err != nil {
		t.Fatal(err)
	}
	// Flush a pending non-ASCII grapheme without changing terminal state.
	e.flushGrapheme()
}

func TestWrapConfirmedOnlyWhenPhantomConsumed(t *testing.T) {
	t.Run("pending phantom is not wrap", func(t *testing.T) {
		e := NewEmulator(4, 2)
		write(t, e, "abcd")
		if e.Wrapped(0) {
			t.Fatal("full last cell is only pending")
		}
	})
	t.Run("next grapheme confirms wrap", func(t *testing.T) {
		e := NewEmulator(4, 2)
		write(t, e, "abcde")
		if !e.Wrapped(0) {
			t.Fatal("consumed phantom was not recorded")
		}
	})
	for _, cancel := range []string{"\rX", "\x1b[DX"} {
		t.Run("cancel", func(t *testing.T) {
			e := NewEmulator(4, 2)
			write(t, e, "abcd"+cancel)
			if e.Wrapped(0) {
				t.Fatal("cursor control must cancel pending phantom")
			}
		})
	}
	t.Run("autowrap disabled", func(t *testing.T) {
		e := NewEmulator(4, 2)
		write(t, e, "\x1b[?7labcde")
		if e.Wrapped(0) {
			t.Fatal("DECAWM off recorded a wrap")
		}
	})
}

func TestWrapScrollsIntoScrollback(t *testing.T) {
	e := NewEmulator(4, 2)
	write(t, e, "abcdefghi") // consuming the second phantom scrolls its row.
	sb := e.Scrollback()
	if sb.Len() != 1 || !sb.Wrapped(0) {
		t.Fatalf("scrollback len/wrap = %d/%v", sb.Len(), sb.Wrapped(0))
	}
	if !e.Wrapped(0) {
		t.Fatal("first visible row should be the other wrapped row")
	}
}

func TestWrapWideAndCombiningBoundary(t *testing.T) {
	t.Run("wide cell is preserved after forced wrap", func(t *testing.T) {
		for width := 4; width <= 10; width++ {
			e := NewEmulator(width, 3)
			prefix := strings.Repeat("a", width-1)
			write(t, e, prefix+"界X")
			for x := 0; x < width-1; x++ {
				assertCell(t, e.CellAt(x, 0), "a", 1)
			}
			if c := e.CellAt(width-1, 0); c == nil || !c.IsZero() {
				t.Fatalf("width %d skipped cell = %#v, want zero", width, c)
			}
			if !e.Wrapped(0) {
				t.Fatalf("width %d forced row is not wrapped", width)
			}
			assertCell(t, e.CellAt(0, 1), "界", 2)
			assertCell(t, e.CellAt(1, 1), "", 0)
			assertCell(t, e.CellAt(2, 1), "X", 1)
		}
	})
	t.Run("combining", func(t *testing.T) {
		e := NewEmulator(4, 2)
		write(t, e, "abc"+"e\u0301"+"X")
		if !e.Wrapped(0) {
			t.Fatal("combining grapheme boundary did not wrap")
		}
	})
}

func TestWrapBookkeepingForRows(t *testing.T) {
	t.Run("insert delete and erase", func(t *testing.T) {
		e := NewEmulator(4, 4)
		write(t, e, "abcde")
		write(t, e, "\x1b[1;1H\x1b[L")
		if e.Wrapped(0) || !e.Wrapped(1) {
			t.Fatal("insert line did not move wrap bit")
		}
		write(t, e, "\x1b[1;1H\x1b[M")
		if !e.Wrapped(0) || e.Wrapped(1) {
			t.Fatal("delete line did not move wrap bit")
		}
		write(t, e, "\x1b[1;1H\x1b[2K")
		if e.Wrapped(0) {
			t.Fatal("erase line did not clear wrap bit")
		}
	})
	t.Run("reverse index", func(t *testing.T) {
		e := NewEmulator(4, 3)
		write(t, e, "abcde\x1b[1;1H\x1bM")
		if e.Wrapped(0) || !e.Wrapped(1) {
			t.Fatal("reverse index did not move wrap bit with row")
		}
	})
	t.Run("vertical margins", func(t *testing.T) {
		e := NewEmulator(4, 4)
		write(t, e, "\x1b[2;4r\x1b[2;1Habcde")
		if !e.Wrapped(1) {
			t.Fatal("expected wrapped row inside margin")
		}
		write(t, e, "\x1b[2;1H\x1b[M")
		if e.Wrapped(1) {
			t.Fatal("delete in margin did not move/clear bit")
		}
		if e.ScrollbackLen() != 1 || !e.Scrollback().Wrapped(0) {
			t.Fatal("margin scroll did not carry wrap provenance into scrollback")
		}
	})
	t.Run("horizontal margins invalidate affected provenance", func(t *testing.T) {
		e := NewEmulator(4, 3)
		write(t, e, "abcde")
		e.scr.setHorizontalMargins(1, 4)
		write(t, e, "\x1b[1;2H\x1b[M")
		if e.Wrapped(0) {
			t.Fatal("partial-width row operation retained ambiguous wrap")
		}
	})
}

func TestScrollbackPushNPreservesWrap(t *testing.T) {
	screen := NewScreen(4, 2)
	screen.setWrapped(0, true)
	screen.SetCell(0, 0, nil)
	sb := NewScrollback(2)
	sb.pushN(screen, 0, 1)
	if sb.Len() != 1 || !sb.Wrapped(0) {
		t.Fatalf("pushN lost source wrap bit: len=%d wrapped=%v", sb.Len(), sb.Wrapped(0))
	}
}

func TestScrollbackWrapTruncation(t *testing.T) {
	sb := NewScrollback(2)
	sb.PushWrapped(nil, true)
	sb.PushWrapped(nil, false)
	sb.PushWrapped(nil, true)
	if sb.Len() != 2 || sb.Wrapped(0) || !sb.Wrapped(1) {
		t.Fatalf("truncated wrap flags are misaligned: len=%d flags=%v,%v", sb.Len(), sb.Wrapped(0), sb.Wrapped(1))
	}
}

func assertCell(t *testing.T, cell *uv.Cell, content string, width int) {
	t.Helper()
	if cell == nil || cell.Content != content || cell.Width != width {
		t.Fatalf("cell = %#v, want content %q width %d", cell, content, width)
	}
}

func TestScrollbackPushWrappedTrailingBlanks(t *testing.T) {
	line := uv.NewLine(4)
	line.Set(0, &uv.Cell{Content: "x", Width: 1})
	sb := NewScrollback(10)
	sb.PushWrapped(line, true)
	sb.PushWrapped(line, false)
	if got := len(sb.Line(0)); got != 4 {
		t.Fatalf("wrapped row length = %d, want 4", got)
	}
	if got := len(sb.Line(1)); got != 1 {
		t.Fatalf("unwrapped row length = %d, want 1", got)
	}
}

func TestNormalBufferWidthReflow(t *testing.T) {
	t.Run("grow removes synthetic wide skip", func(t *testing.T) {
		e := NewEmulator(4, 4)
		write(t, e, "abc界X")
		e.Resize(8, 4)
		assertCell(t, e.CellAt(0, 0), "a", 1)
		assertCell(t, e.CellAt(1, 0), "b", 1)
		assertCell(t, e.CellAt(2, 0), "c", 1)
		assertCell(t, e.CellAt(3, 0), "界", 2)
		assertCell(t, e.CellAt(4, 0), "", 0)
		assertCell(t, e.CellAt(5, 0), "X", 1)
		if e.Wrapped(0) {
			t.Fatal("grown logical line remained wrapped")
		}
	})

	t.Run("shrink recreates synthetic wide skip", func(t *testing.T) {
		e := NewEmulator(10, 4)
		write(t, e, "12345678界X")
		e.Resize(9, 4)
		for x := 0; x < 8; x++ {
			assertCell(t, e.CellAt(x, 0), string(rune('1'+x)), 1)
		}
		if c := e.CellAt(8, 0); c == nil || !c.IsZero() {
			t.Fatalf("new skipped cell = %#v, want zero", c)
		}
		if !e.Wrapped(0) {
			t.Fatal("early-wide row not marked wrapped after shrink")
		}
		assertCell(t, e.CellAt(0, 1), "界", 2)
		assertCell(t, e.CellAt(1, 1), "", 0)
		assertCell(t, e.CellAt(2, 1), "X", 1)
	})

	t.Run("combining and ZWJ graphemes survive forced wrap and reflow", func(t *testing.T) {
		e := NewEmulator(5, 4)
		write(t, e, "abcd")
		e.handleGrapheme("e\u0301", 1)
		e.handleGrapheme("👩‍💻", 2)
		e.handleGrapheme("X", 1)
		assertCell(t, e.CellAt(3, 0), "d", 1)
		assertCell(t, e.CellAt(4, 0), "é", 1)
		assertCell(t, e.CellAt(0, 1), "👩‍💻", 2)
		e.Resize(9, 4)
		assertCell(t, e.CellAt(4, 0), "é", 1)
		assertCell(t, e.CellAt(5, 0), "👩‍💻", 2)
		assertCell(t, e.CellAt(6, 0), "", 0)
		assertCell(t, e.CellAt(7, 0), "X", 1)
	})

	t.Run("text spaces wide style cursor and blank lines", func(t *testing.T) {
		e := NewEmulator(8, 8)
		write(t, e, "界 ab cdZ\x1b[44m \x1b[0mQ\r\n\r\ntail")
		e.Resize(11, 8)
		lines := emulatorLogicalLines(e)
		if len(lines) < 3 || lines[0] != "界 ab cdZ Q" || lines[1] != "" || lines[2] != "tail" {
			t.Fatalf("reflowed logical lines = %q", lines)
		}
		foundStyledBlank := false
		for y := 0; y < e.Height(); y++ {
			for x := 0; x < e.Width(); x++ {
				if c := e.CellAt(x, y); c != nil && c.Content == " " && c.Style.Bg != nil {
					foundStyledBlank = true
				}
			}
		}
		if !foundStyledBlank {
			t.Fatal("styled blank was lost during reflow")
		}
		if got := e.CursorPosition(); got != uv.Pos(4, 2) {
			t.Fatalf("cursor = %+v, want (4,2)", got)
		}
	})

	t.Run("scrollback visible boundary is one logical line", func(t *testing.T) {
		e := NewEmulator(8, 2)
		e.SetScrollbackSize(20)
		text := "one two three four five six seven"
		write(t, e, text)
		if e.ScrollbackLen() == 0 || !e.Scrollback().Wrapped(e.ScrollbackLen()-1) {
			t.Fatal("fixture does not cross the scrollback boundary")
		}
		e.Resize(13, 3)
		if got := strings.Join(emulatorLogicalLines(e), "|"); got != text {
			t.Fatalf("reflow text = %q, want %q", got, text)
		}
		for i, line := range e.Scrollback().Lines() {
			if e.Scrollback().Wrapped(i) && len(line) != 13 {
				t.Fatalf("scrollback wrapped row %d width = %d", i, len(line))
			}
		}
	})

	t.Run("scrollback maximum trims oldest reflowed rows", func(t *testing.T) {
		e := NewEmulator(6, 2)
		e.SetScrollbackSize(2)
		write(t, e, "abcdefghijklmnopqrstuv")
		e.Resize(3, 2)
		if e.ScrollbackLen() != 2 {
			t.Fatalf("scrollback len = %d, want max 2", e.ScrollbackLen())
		}
	})
}

func BenchmarkReflow2000ScrollbackLines(b *testing.B) {
	for b.Loop() {
		b.StopTimer()
		e := NewEmulator(80, 25)
		e.SetScrollbackSize(2000)
		for range 2025 {
			_, _ = e.WriteString("the quick brown fox jumps over the lazy dog\r\n")
		}
		b.StartTimer()
		e.Resize(79, 25)
	}
}

func emulatorLogicalLines(e *Emulator) []string {
	type row struct {
		line    uv.Line
		wrapped bool
	}
	var rows []row
	for i, line := range e.Scrollback().Lines() {
		rows = append(rows, row{line, e.Scrollback().Wrapped(i)})
	}
	last := e.CursorPosition().Y
	for y := e.Height() - 1; y >= 0; y-- {
		if lineContentLen(e.scr.buf.Line(y)) > 0 {
			last = max(last, y)
			break
		}
	}
	for y := 0; y <= last; y++ {
		rows = append(rows, row{e.scr.buf.Line(y), e.Wrapped(y)})
	}
	var out []string
	var b strings.Builder
	for _, row := range rows {
		limit := lineContentLen(row.line)
		if row.wrapped {
			limit = len(row.line)
		}
		for i := 0; i < limit; i++ {
			if row.line[i].Width > 0 {
				b.WriteString(row.line[i].Content)
			}
		}
		if !row.wrapped {
			out = append(out, strings.TrimRight(b.String(), " "))
			b.Reset()
		}
	}
	return out
}

func TestWrapBufferResetAndResize(t *testing.T) {
	t.Run("alternate buffer local", func(t *testing.T) {
		e := NewEmulator(4, 2)
		write(t, e, "abcde")
		write(t, e, "\x1b[?1049habcde")
		if !e.Wrapped(0) {
			t.Fatal("alternate buffer missing own wrap")
		}
		write(t, e, "\x1b[?1049l")
		if !e.Wrapped(0) {
			t.Fatal("normal buffer wrap was lost")
		}
		if e.Scrollback() == nil {
			t.Fatal("normal scrollback unavailable")
		}
	})
	t.Run("height-only resize preserves surviving rows", func(t *testing.T) {
		e := NewEmulator(4, 3)
		write(t, e, "abcde")
		e.Resize(4, 4)
		if !e.Wrapped(0) {
			t.Fatal("taller resize lost provenance")
		}
		e.Resize(4, 2)
		if !e.Wrapped(0) {
			t.Fatal("shorter resize lost surviving provenance")
		}
	})
	t.Run("width resize clears both buffers", func(t *testing.T) {
		e := NewEmulator(4, 2)
		write(t, e, "abcde")
		write(t, e, "\x1b[?1049habcde")
		if !e.Wrapped(0) {
			t.Fatal("alternate buffer missing wrap before resize")
		}
		e.Resize(8, 2)
		if e.Wrapped(0) {
			t.Fatal("alternate buffer retained wrap across width change")
		}
		write(t, e, "\x1b[?1049l")
		if e.Wrapped(0) {
			t.Fatal("normal buffer retained wrap across width change")
		}
	})
	t.Run("RIS clears both buffers", func(t *testing.T) {
		e := NewEmulator(4, 2)
		write(t, e, "abcde\x1bc")
		if e.Wrapped(0) {
			t.Fatal("RIS retained wrap")
		}
	})
	t.Run("ED3 clears screen and scrollback metadata", func(t *testing.T) {
		e := NewEmulator(4, 2)
		write(t, e, "abcdefghi")
		write(t, e, "\x1b[3J")
		if e.ScrollbackLen() != 0 || e.Wrapped(0) {
			t.Fatal("ED3 retained wrap state")
		}
	})
}
