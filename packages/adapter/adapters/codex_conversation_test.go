package adapters

import (
	"encoding/json"
	"testing"
)

const codexCompletedResponse = `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":42}},"rate_limits":null}}`

func TestCodexExchangeProjectionVisibleMessagesOnly(t *testing.T) {
	path := writeCodexJSONL(t,
		`{"type":"session_meta","payload":{"id":"abc"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\nx\n</environment_context>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"do it"}]}}`,
		`{"type":"event_msg","payload":{"type":"agent_reasoning","text":"private chain of thought"}}`,
		`{"type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"also private"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"working"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"secret output"}}`,
		codexCompletedResponse,
		`{"type":"event_msg","payload":{"type":"exec_approval_request","command":"danger"}}`,
		`{"type":"event_msg","payload":{"type":"plan_update","plan":["hidden"]}}`,
		`{"type":"compacted","payload":{"message":"hidden summary"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
		codexCompletedResponse,
	)
	ex, err := NewCodex().RenderConversationExchanges(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 || ex[0].User != "do it" || ex[0].Iterations != 2 || ex[0].Terminal != "done" {
		t.Fatalf("%+v", ex)
	}
}

func TestCodexResponseBoundaries(t *testing.T) {
	t.Run("message and multiple tools are one response", func(t *testing.T) {
		path := writeCodexJSONL(t,
			codexUser("go"),
			codexAssistant("I will inspect"),
			`{"type":"response_item","payload":{"type":"function_call","name":"one"}}`,
			`{"type":"response_item","payload":{"type":"function_call","name":"two"}}`,
			codexCompletedResponse,
		)
		ex, err := NewCodex().RenderConversationExchanges(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(ex) != 1 || ex[0].Iterations != 1 || ex[0].Terminal != "I will inspect" {
			t.Fatalf("%+v", ex)
		}
	})

	t.Run("tool-only response counts", func(t *testing.T) {
		path := writeCodexJSONL(t,
			codexUser("go"),
			`{"type":"response_item","payload":{"type":"function_call","name":"one"}}`,
			codexCompletedResponse,
		)
		ex, err := NewCodex().RenderConversationExchanges(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(ex) != 1 || ex[0].Iterations != 1 || ex[0].Terminal != "" {
			t.Fatalf("%+v", ex)
		}
	})

	t.Run("tool-only final response clears earlier prose", func(t *testing.T) {
		path := writeCodexJSONL(t,
			codexUser("go"), codexAssistant("I will inspect"), codexCompletedResponse,
			`{"type":"response_item","payload":{"type":"function_call_output","output":"hidden"}}`,
			`{"type":"response_item","payload":{"type":"function_call","name":"again"}}`,
			codexCompletedResponse,
		)
		ex, err := NewCodex().RenderConversationExchanges(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(ex) != 1 || ex[0].Iterations != 2 || ex[0].Terminal != "" {
			t.Fatalf("%+v", ex)
		}
	})

	t.Run("null token count is not completion", func(t *testing.T) {
		path := writeCodexJSONL(t,
			codexUser("go"), codexAssistant("done"),
			`{"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":null}}`,
			codexCompletedResponse,
		)
		ex, err := NewCodex().RenderConversationExchanges(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(ex) != 1 || ex[0].Iterations != 1 || ex[0].Terminal != "done" {
			t.Fatalf("%+v", ex)
		}
	})
}

func TestCodexContextualUserRegistryIsExcludedByPublicRenderer(t *testing.T) {
	// One public-renderer fixture per persisted role-user form mirrored in
	// codexContextualUserMatchers. These are generated shapes, not invented
	// XML namespaces.
	contexts := []struct{ name, text string }{
		{"AGENTS instructions", "# AGENTS.md instructions for /tmp/project\n\n<INSTRUCTIONS>\nhidden\n</INSTRUCTIONS>"},
		{"environment", "<environment_context>\n<cwd>/tmp</cwd>\n</environment_context>"},
		{"permissions", "<permissions>\nhidden\n</permissions>"},
		{"legacy permissions", "<permissions instructions>hidden</permissions instructions>"},
		{"skill", "<skill>\nsecret instructions\n</skill>"},
		{"skills instructions", "<skills_instructions>\nhidden\n</skills_instructions>"},
		{"user shell", "<user_shell_command>\n<command>secret</command>\n</user_shell_command>"},
		{"turn aborted", "<turn_aborted>\nhidden\n</turn_aborted>"},
		{"subagent notification", "<subagent_notification>\n{\"status\":\"done\"}\n</subagent_notification>"},
		{"hook prompt", "<hook_prompt hook_run_id=\"abc-123\">\nhidden hook prompt\n</hook_prompt>"},
		{"modern internal context", "<codex_internal_context source=\"memory_1\">\nhidden\n</codex_internal_context>"},
		{"legacy goal context", "<goal_context>\nhidden\n</goal_context>"},
		{"recommended plugins", "<recommended_plugins>\nhidden\n</recommended_plugins>"},
		{"legacy unified-exec warning", "Warning: The maximum number of unified exec processes you can keep open is 60 and you currently have 61 processes open. Reuse older processes or close them to prevent automatic pruning of old processes"},
		{"legacy apply-patch warning", "Warning: apply_patch was requested via exec_command. Use the apply_patch tool instead of exec_command."},
		{"legacy model-mismatch warning", "Warning: Your account was flagged for potentially high-risk cyber activity and this request was routed to gpt-5.2 as a fallback. To regain access, apply for trusted access."},
	}
	for _, context := range contexts {
		t.Run(context.name, func(t *testing.T) {
			path := writeCodexJSONL(t,
				codexUser(context.text), codexUser("real prompt"),
				codexAssistant("answer"), codexCompletedResponse,
			)
			ex, err := NewCodex().RenderConversationExchanges(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(ex) != 1 || ex[0].User != "real prompt" || ex[0].Terminal != "answer" {
				t.Fatalf("%s leaked or erased boundary: %+v", context.name, ex)
			}
		})
	}
}

func TestCodexContextLookalikesRemainPublicUserPrompts(t *testing.T) {
	prompts := []string{
		"<permissions please explain the deployment steps",
		"<external_ticket>summarize this ticket</external_ticket>",
		"<hook_prompt>explain this element</hook_prompt>",
		"<codex_internal_context source=\"INVALID\">explain this</codex_internal_context>",
	}
	for _, prompt := range prompts {
		t.Run(prompt, func(t *testing.T) {
			path := writeCodexJSONL(t, codexUser(prompt), codexAssistant("answer"), codexCompletedResponse)
			ex, err := NewCodex().RenderConversationExchanges(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(ex) != 1 || ex[0].User != prompt {
				t.Fatalf("legitimate prompt erased: %+v", ex)
			}
		})
	}
}

func TestCodexImageInputsEstablishUserBoundaries(t *testing.T) {
	t.Run("image only", func(t *testing.T) {
		path := writeCodexJSONL(t,
			`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,secret"}]}}`,
			codexAssistant("seen"), codexCompletedResponse,
		)
		ex, err := NewCodex().RenderConversationExchanges(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(ex) != 1 || ex[0].User != "[image]" || ex[0].Terminal != "seen" {
			t.Fatalf("image exchange = %#v", ex)
		}
		messages, err := NewCodex().RenderConversation(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 2 || messages[0].Text != "[image]" || messages[0].Prose != "" {
			t.Fatalf("image conversation = %#v", messages)
		}
	})

	t.Run("mixed text and image", func(t *testing.T) {
		path := writeCodexJSONL(t,
			`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect"},{"type":"input_image","image_url":"https://example.invalid/secret"}]}}`,
			codexAssistant("seen"), codexCompletedResponse,
		)
		ex, err := NewCodex().RenderConversationExchanges(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(ex) != 1 || ex[0].User != "inspect\n\n[image]" {
			t.Fatalf("mixed image exchange = %#v", ex)
		}
	})
}

func TestCodexConversationRendersToolCallsWithoutResults(t *testing.T) {
	path := writeCodexJSONL(t,
		codexUser("go"),
		`{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":\"go test ./...\"}"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","output":"private payload"}}`,
		codexCompletedResponse,
	)
	messages, err := NewCodex().RenderConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Text != `[tool] shell {"command":"go test ./..."}` || messages[1].Prose != "" {
		t.Fatalf("tool conversation = %#v", messages)
	}
}

func TestCodexExchangeMalformedPartialAndLegacyFallback(t *testing.T) {
	path := writeCodexJSONL(t,
		codexUser("go"),
		codexAssistant("not terminal"),
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
		codexUser("hello"),
		`{"type":"event_msg","payload":{"type":"agent_message","message":"world"}}`,
		codexAssistant("world"), codexCompletedResponse,
	)
	messages, err := NewCodex().RenderConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Text != "hello" || messages[1].Text != "world" {
		t.Fatalf("%+v", messages)
	}
}

// Compatibility only: current Codex classifies assistant content deltas as
// transient and does not persist them. Older rollouts that did persist them
// can still expose trailing partial prose without duplicating a canonical item.
func TestCodexConversationLegacyPersistedDeltaCompatibility(t *testing.T) {
	path := writeCodexJSONL(t,
		codexUser("go"),
		`{"type":"event_msg","payload":{"type":"agent_message_content_delta","item_id":"1","delta":"part "}}`,
		`{"type":"event_msg","payload":{"type":"agent_message_content_delta","item_id":"1","delta":"way"}}`,
	)
	ex, err := NewCodex().RenderConversationExchanges(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 1 || ex[0].Iterations != 1 || ex[0].Terminal != "part way" {
		t.Fatalf("%+v", ex)
	}

	path = writeCodexJSONL(t,
		codexUser("go"),
		`{"type":"event_msg","payload":{"type":"agent_message_content_delta","item_id":"1","delta":"part"}}`,
		codexAssistant("complete"),
	)
	messages, err := NewCodex().RenderConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Text != "complete" {
		t.Fatalf("%+v", messages)
	}
}

func codexUser(text string) string {
	return `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":` + quoteJSON(text) + `}]}}`
}

func codexAssistant(text string) string {
	return `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":` + quoteJSON(text) + `}]}}`
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
