package ptyserver

import (
	"sync"
	"testing"
	"time"
)

func TestReadBinderLoneReadBinds(t *testing.T) {
	var mu sync.Mutex
	var got []string
	b := newReadBinder(20*time.Millisecond, func(p string) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	})

	b.observe("/a.jsonl")
	time.Sleep(60 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "/a.jsonl" {
		t.Fatalf("lone read should bind once to /a.jsonl, got %v", got)
	}
}

func TestReadBinderBurstIsIgnored(t *testing.T) {
	var mu sync.Mutex
	var got []string
	b := newReadBinder(20*time.Millisecond, func(p string) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	})

	// A picker scan: several distinct files read in a tight burst.
	for _, p := range []string{"/a.jsonl", "/b.jsonl", "/c.jsonl"} {
		b.observe(p)
	}
	time.Sleep(60 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("multi-file burst (scan) must not bind, got %v", got)
	}
}

func TestReadBinderRepeatedSameFileBinds(t *testing.T) {
	var mu sync.Mutex
	var got []string
	b := newReadBinder(20*time.Millisecond, func(p string) {
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
	})

	// Binding often reads the same file a couple of times (open + reads);
	// that's still one distinct file → one bind.
	b.observe("/a.jsonl")
	b.observe("/a.jsonl")
	time.Sleep(60 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "/a.jsonl" {
		t.Fatalf("repeated same-file read should bind once, got %v", got)
	}
}
