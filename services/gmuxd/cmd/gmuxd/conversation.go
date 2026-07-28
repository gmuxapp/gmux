package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// conversationScopeMessage is the query value selecting ADR 0027's
// semantic read: the latest final assistant message, nothing else.
const conversationScopeMessage = "message"

// conversationScopeHeader is echoed on a message-scope response.
//
// It exists for version skew: a gmuxd that predates this scope ignores
// the query parameter entirely and answers 200 with the FULL transcript.
// Without a positive marker, a client asking for one message would print
// the whole conversation and call it the agent's answer. The header lets
// the CLI recognize an old daemon and say so.
const conversationScopeHeader = "X-Gmux-Conversation-Scope"

// Stable error codes for the message scope.
const (
	// codeUnsupportedAdapter — this session's adapter cannot reconstruct a
	// conversation at all, so there is no such thing as its latest
	// assistant message. Explicitly an error: answering 200 with an empty
	// body would read as "the agent said nothing".
	codeUnsupportedAdapter = "unsupported_adapter"
	// codeNoMessage — the conversation exists and is readable, but holds no
	// assistant prose yet (a brand-new session, or a turn that has so far
	// only produced tool calls). Distinct from no_conversation, which is
	// about the storage, and from an empty success, which would be a lie.
	codeNoMessage = "no_message"
)

// conversationMessageScopeCentral serves
// GET /v1/sessions/{id}/conversation?scope=message.
//
// It reads adapter-owned storage only: no runner call, no resume, no
// liveness requirement, so it answers for a dead retained session the
// same way it answers for a live one. That is the whole point of reading
// the agent's output out of band from driving the agent.
func conversationMessageScopeCentral(w http.ResponseWriter, r *http.Request, sess centralstore.Session) {
	// tail counts transcript messages; with scope=message the answer is exactly
	// one message. Silently ignoring the parameter would let a caller believe
	// they had constrained something, so its mere PRESENCE is the error —
	// including `?tail=`, which is a caller who meant to pass a number.
	if _, ok := r.URL.Query()["tail"]; ok {
		writeError(w, http.StatusBadRequest, "bad_request", "tail does not apply to scope=message")
		return
	}
	// The adapter check comes first: "this adapter has no conversation
	// model" is a permanent, actionable answer, while "no conversation ref
	// yet" is a transient one. Reporting the transient shape for a shell
	// session would suggest waiting for something that can never arrive.
	renderer, ok := adapters.FindByAdapter(sess.Adapter).(adapter.ConversationRenderer)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, codeUnsupportedAdapter,
			fmt.Sprintf("adapter %q does not expose agent messages", sess.Adapter))
		return
	}
	if sess.ConversationRef == "" {
		writeError(w, http.StatusNotFound, "no_conversation", "session has no conversation")
		return
	}
	msgs, err := renderer.RenderConversation(sess.ConversationRef)
	// ENOENT is a conversation with no messages yet, not a lost one: pi reports
	// the transcript path before it writes the first line, so a brand-new
	// session (or one whose first turn is still running) lands here. Saying
	// "gone" would tell the caller their transcript had been destroyed. Same
	// distinction, same wording as the transcript scope.
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "no_conversation", "conversation has no messages yet")
		return
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "no_conversation", "conversation is gone")
		return
	}
	text, found := latestAssistantProse(msgs)
	if !found {
		writeError(w, http.StatusNotFound, codeNoMessage, "the agent has not produced a message yet")
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(conversationScopeHeader, conversationScopeMessage)
	_, _ = w.Write([]byte(text + "\n"))
}

// Message-type vocabulary for the transcript scope's `types` filter (ADR
// 0027's 2026-07-28 amendment, retrieval verbs). The types are gmux's, not
// pi's: they name what a caller wants to READ, and the classification below is
// the only place that maps an adapter's rendering onto them.
//
//   - user  — a user message (a prompt, a steer, a merged follow-up).
//   - agent — an assistant message that carries prose (what the agent SAID),
//     even when the same message also rendered tool calls.
//   - tool  — an assistant message that rendered tool calls and nothing else.
//
// There is deliberately no `thinking`: no adapter renders thinking blocks
// today (pi's renderer drops them), so serving the type would answer every
// request with silence. The CLI refuses the flag by name instead.
const (
	messageTypeUser  = "user"
	messageTypeAgent = "agent"
	messageTypeTool  = "tool"
)

// conversationMessageType classifies one rendered message.
//
// Classification is per MESSAGE, not per line: a pi assistant message routinely
// mixes prose with [tool] one-liners, and splitting it would invent messages
// the adapter never asserted. So a mixed message is `agent` and is rendered
// whole — the filter chooses which messages are shown, never how they read.
func conversationMessageType(m adapter.ConversationMessage) string {
	if m.Role == "user" {
		return messageTypeUser
	}
	if strings.TrimSpace(m.Prose) != "" {
		return messageTypeAgent
	}
	return messageTypeTool
}

// parseConversationTypes validates a `types=` list. An empty string means the
// default set (user + agent prose): the conversation, without the machinery.
//
// An unknown type is refused rather than dropped: a caller who asked for
// `--thinkin` and got a silently narrowed transcript would read the absence as
// "the agent thought nothing".
func parseConversationTypes(raw string) (map[string]bool, error) {
	if raw == "" {
		return map[string]bool{messageTypeUser: true, messageTypeAgent: true}, nil
	}
	allowed := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		t := strings.TrimSpace(part)
		switch t {
		case messageTypeUser, messageTypeAgent, messageTypeTool:
			allowed[t] = true
		case "":
			return nil, errors.New("types must not contain an empty entry")
		default:
			return nil, fmt.Errorf("unknown message type %q; expected user, agent or tool", t)
		}
	}
	return allowed, nil
}

func filterConversationMessages(msgs []adapter.ConversationMessage, allowed map[string]bool) []adapter.ConversationMessage {
	out := make([]adapter.ConversationMessage, 0, len(msgs))
	for _, m := range msgs {
		if allowed[conversationMessageType(m)] {
			out = append(out, m)
		}
	}
	return out
}

// conversationWireMessage is the machine contract of `format=json`
// (`gmux agent logs --json`): the rendered message, plus the type it was
// filtered on and the prose subset so a consumer can separate what the agent
// said from what it did without re-parsing the rendering.
//
// Every field is always present, Prose included: the documented shape is
// {role, type, text, prose}, and omitting prose for a tool-only message would
// make `.prose` null there and "" elsewhere — two spellings of the same fact,
// in a contract two verbs (`logs --json` and `status --json`) must agree on.
type conversationWireMessage struct {
	Role  string `json:"role"`
	Type  string `json:"type"`
	Text  string `json:"text"`
	Prose string `json:"prose"`
}

func conversationWireMessages(msgs []adapter.ConversationMessage) []conversationWireMessage {
	out := make([]conversationWireMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, conversationWireMessage{
			Role:  m.Role,
			Type:  conversationMessageType(m),
			Text:  strings.TrimRight(m.Text, "\n"),
			Prose: strings.TrimRight(m.Prose, "\n"),
		})
	}
	return out
}

// conversationReadable reports whether this session has a conversation gmux can
// read at all. It answers the CLI's hint question ("is `gmux agent status`
// meaningful here?") without rendering anything: a shell or a Claude/Codex
// session must not be told to read a semantic conversation that will 404.
func conversationReadable(ctx context.Context, store *centralstore.Store, sessionID string) bool {
	if store == nil {
		return false
	}
	row, ok, err := store.Session(ctx, centralstore.SessionID(sessionID))
	if err != nil || !ok || row.ConversationRef == "" {
		return false
	}
	_, isRenderer := adapters.FindByAdapter(row.Adapter).(adapter.ConversationRenderer)
	return isRenderer
}

// singleQueryValue returns the value of an at-most-once query parameter: ""
// when the key is absent, an error when it is repeated or present with an
// empty value.
//
// url.Values.Get() collapses all three cases into a string, which is how an
// empty or contradictory selector ends up silently resolving to a default.
func singleQueryValue(q url.Values, key string) (string, error) {
	values, ok := q[key]
	if !ok {
		return "", nil
	}
	if len(values) > 1 {
		return "", fmt.Errorf("%s must be given at most once", key)
	}
	if values[0] == "" {
		return "", fmt.Errorf("%s must not be empty", key)
	}
	return values[0], nil
}

// latestAssistantProse selects the latest final assistant message from a
// rendered transcript.
//
// "Latest assistant message" is not the same as "last element with
// role=assistant": a pi assistant message mixes prose with compact
// [tool] lines, and a turn's tail can be a tool-only message. So the
// scan walks backwards for the newest assistant message that carries
// prose (adapter-declared, see ConversationMessage.Prose).
//
// The scan stops at the newest user message instead of running to the
// start of the file. The latest user message is the cheapest honest
// turn boundary available without inventing turn metadata: an assistant
// message before it answered a different request, and reporting it as
// the current answer is exactly the stale-output failure this read
// exists to avoid. A turn that has so far produced only tool calls
// therefore reports no_message rather than the previous answer.
//
// It deliberately does NOT fall back to Text when Prose is empty. Text
// is the transcript rendering; returning it here is how a `[tool] bash
// {...}` line ends up presented as the agent's answer.
func latestAssistantProse(msgs []adapter.ConversationMessage) (string, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return "", false
		}
		if msgs[i].Role != "assistant" {
			continue
		}
		if prose := strings.TrimRight(msgs[i].Prose, "\n"); prose != "" {
			return prose, true
		}
	}
	return "", false
}

// formatConversationMarkdown renders adapter-neutral conversation messages.
func formatConversationMarkdown(msgs []adapter.ConversationMessage) []byte {
	var b bytes.Buffer
	for i, msg := range msgs {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "## %s\n\n%s\n", roleHeading(msg.Role), strings.TrimRight(msg.Text, "\n"))
	}
	return b.Bytes()
}

func roleHeading(role string) string {
	switch role {
	case "user":
		return "User"
	case "assistant":
		return "Assistant"
	case "":
		return "Message"
	default:
		return strings.ToUpper(role[:1]) + role[1:]
	}
}
