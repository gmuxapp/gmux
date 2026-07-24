package main

import (
	"context"
	"testing"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

func TestResolveResumeDirCentralFallbackAndErrors(t *testing.T) {
	ctx := context.Background()
	st, err := centralstore.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	row := centralstore.Session{ID: "s", CWD: "/definitely/missing"}
	dir, fallback, err := resolveResumeDirCentral(ctx, st, row)
	if err != nil || dir != home || !fallback {
		t.Fatalf("dir=%q fallback=%v err=%v", dir, fallback, err)
	}
	cwd := t.TempDir()
	row.CWD = cwd
	dir, fallback, err = resolveResumeDirCentral(ctx, st, row)
	if err != nil || dir != cwd || fallback {
		t.Fatalf("cwd dir=%q fallback=%v err=%v", dir, fallback, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveResumeDirCentral(ctx, st, row); err == nil {
		t.Fatal("closed store error suppressed")
	}
}
