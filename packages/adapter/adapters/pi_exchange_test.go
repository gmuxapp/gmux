package adapters

import "testing"

func TestPiExchangeProjectionWalksLatestPersistedBranch(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"session","id":"root"}`,
		`{"type":"message","id":"u1","parentId":"root","message":{"role":"user","content":"first"}}`,
		`{"type":"message","id":"a1","parentId":"u1","message":{"role":"assistant","content":[{"type":"text","text":"old answer"}]}}`,
		`{"type":"message","id":"u-abandoned","parentId":"a1","message":{"role":"user","content":"abandoned"}}`,
		`{"type":"message","id":"a-abandoned","parentId":"u-abandoned","message":{"role":"assistant","content":"wrong leaf"}}`,
		`{"type":"message","id":"u2","parentId":"a1","message":{"role":"user","content":"  latest\nquestion  "}}`,
		`{"type":"message","id":"a-tool","parentId":"u2","message":{"role":"assistant","content":[{"type":"thinking","thinking":"x"},{"type":"toolCall","name":"bash","arguments":{}}]}}`,
		`{"type":"message","id":"a2","parentId":"a-tool","message":{"role":"assistant","content":[{"type":"text","text":" final\nanswer "}]}}`,
	)
	ex, err := NewPi().RenderConversationExchanges(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 2 {
		t.Fatalf("%+v", ex)
	}
	if ex[0].User != "first" || ex[0].Iterations != 1 || ex[0].Terminal != "old answer" {
		t.Fatalf("first=%+v", ex[0])
	}
	if ex[1].User != "  latest\nquestion  " || ex[1].Iterations != 2 || ex[1].Terminal != " final\nanswer " {
		t.Fatalf("latest=%+v", ex[1])
	}
}

func TestPiExchangePreservesWhitespaceOnlyUserBoundary(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"message","message":{"role":"user","content":"first"}}`,
		`{"type":"message","message":{"role":"assistant","content":"one"}}`,
		`{"type":"message","message":{"role":"user","content":"  \n\t"}}`,
		`{"type":"message","message":{"role":"assistant","content":"two"}}`)
	ex, err := NewPi().RenderConversationExchanges(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 2 || ex[1].User != "  \n\t" || ex[1].Terminal != "two" {
		t.Fatalf("%+v", ex)
	}
}

func TestPiExchangeTerminalNeverReachesBackPastToolOnlyResponse(t *testing.T) {
	path := writeTempJSONL(t,
		`{"type":"message","id":"u","message":{"role":"user","content":"go"}}`,
		`{"type":"message","id":"a1","message":{"role":"assistant","content":"not terminal"}}`,
		`{"type":"message","id":"a2","message":{"role":"assistant","content":[{"type":"toolCall","name":"bash","arguments":{}}]}}`)
	ex, err := NewPi().RenderConversationExchanges(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 || ex[0].Iterations != 2 || ex[0].Terminal != "" {
		t.Fatalf("%+v", ex)
	}
}
