package main

import (
	"errors"
	"testing"
)

func TestPutProjectsValidation(t *testing.T) {
	if _, err := decodeProjectState([]byte("{")); !errors.Is(err, errInvalidProjectJSON) {
		t.Fatalf("invalid JSON=%v", err)
	}
	if _, err := decodeProjectState([]byte(`{"version":4,"items":[{"slug":"same"},{"slug":"same"}]}`)); err == nil {
		t.Fatal("duplicate slug accepted")
	}
	if state, err := decodeProjectState([]byte(`{"version":4,"items":[{"slug":"one","match":[{"path":"/tmp"}]}]}`)); err != nil || len(state.Items) != 1 {
		t.Fatalf("valid state=%+v err=%v", state, err)
	}
}
