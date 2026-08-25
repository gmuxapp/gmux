package vt

import "testing"

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
	t.Run("wide", func(t *testing.T) {
		e := NewEmulator(4, 2)
		write(t, e, "abc界X")
		if !e.Wrapped(0) {
			t.Fatal("wide boundary did not wrap")
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
	t.Run("resize preserves surviving rows", func(t *testing.T) {
		e := NewEmulator(4, 3)
		write(t, e, "abcde")
		e.Resize(8, 4)
		if !e.Wrapped(0) {
			t.Fatal("wider resize lost provenance")
		}
		e.Resize(3, 2)
		if !e.Wrapped(0) {
			t.Fatal("narrower resize lost surviving provenance")
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
