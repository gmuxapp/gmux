package adapters

import "testing"

func TestCodexExchangeProjectionVisibleMessagesOnly(t *testing.T) {
	path := writeCodexJSONL(t,
		`{"type":"session_meta","payload":{"id":"abc"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>x</environment_context>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"do it"}]}}`,
		`{"type":"event_msg","payload":{"type":"agent_reasoning","text":"private chain of thought"}}`,
		`{"type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"also private"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"working"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"secret output"}}`,
		`{"type":"compacted","payload":{"message":"hidden summary"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
	)
	ex, err := NewCodex().RenderConversationExchanges(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 || ex[0].User != "do it" || ex[0].Iterations != 2 || ex[0].Terminal != "done" {
		t.Fatalf("%+v", ex)
	}
}

func TestCodexExchangeMalformedPartialAndToolOnlyTerminal(t *testing.T) {
	path := writeCodexJSONL(t,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"not terminal"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"refusal","text":"not visible"}]}}`,
		`{"type":"response_item"`,
	)
	ex, err := NewCodex().RenderConversationExchanges(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 || ex[0].Iterations != 2 || ex[0].Terminal != "" {
		t.Fatalf("%+v", ex)
	}
}

func TestCodexConversationDoesNotDuplicateEventMessages(t *testing.T) {
	path := writeCodexJSONL(t,
		`{"type":"event_msg","payload":{"type":"user_message","message":"hello"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"world"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"world"}]}}`,
	)
	messages, err := NewCodex().RenderConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Text != "hello" || messages[1].Text != "world" {
		t.Fatalf("%+v", messages)
	}
}
