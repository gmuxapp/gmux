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

// resultWindow bounds a result selection to ONE turn: the turn the waiter
// actually observed.
//
// Selecting "the newest assistant prose" at read time is not enough, even
// server-side. Between the observation that closed our turn and the read, the
// agent can be prompted again: a complete newer turn would make the waiter
// print somebody else's answer, and a newer turn that has only produced a user
// message so far would make it silently omit its own (the snapshot selector
// stops at the newest user message). Both are attribution failures, and neither
// is fixable by reading faster.
//
// The bound is an index into the rendered conversation, taken when the waiter
// first observed its turn — no message IDs, no turn tokens, no stored turn
// metadata. WHICH index depends on what was observed, and both cases are
// load-bearing:
//
//   - a turn that was **already in progress** at mark time (a wait that
//     subscribes mid-turn, a steer, a follow-up queued behind a running turn)
//     binds to that turn's USER BOUNDARY: the index just past the newest user
//     message. Prose the turn had already persisted is inside our window, which
//     it must be — binding to the message count instead loses the answer of any
//     such turn whose tail is tool-only, and reports nothing for a wait that
//     completed perfectly well.
//   - a turn observed **starting** (a fresh inactive→active edge, or a prompt
//     delivered into an idle agent) binds to the message COUNT. Its content is
//     by definition what comes after, and the previous turn's answer — which
//     the user boundary would include when our own user message has not been
//     persisted yet — must stay outside.
//
// bound == false means snapshot semantics: the caller has no turn to bind to
// (a wait that found the turn already closed, or `gmux agent output`, which is
// explicitly a snapshot), and the newest-prose-in-the-current-turn selector is
// the honest answer.
//
// unreadable records that the conversation could not be read AT MARK TIME, which
// is not the same as an empty conversation: binding such a window to 0 would let
// an earlier turn's answer inside it as soon as storage became readable. Such a
// window yields no automatic result at all.
type resultWindow struct {
	start      int
	bound      bool
	unreadable bool
}

// snapshotWindow is the unbound window: "no turn of mine to attribute".
func snapshotWindow() resultWindow { return resultWindow{} }

// markResultWindow captures a turn's starting watermark. turnInProgress selects
// which bound is taken (see resultWindow). It is store-only (no runner, no
// resume) and runs at most once per observed turn start.
func markResultWindow(ctx context.Context, store *centralstore.Store, sessionID string, turnInProgress bool) resultWindow {
	msgs, ok := renderSessionConversation(ctx, store, sessionID)
	if !ok {
		// No safe boundary exists: refuse to attribute anything rather than
		// bind to 0 and risk claiming an older turn's answer later.
		return resultWindow{bound: true, unreadable: true}
	}
	if turnInProgress {
		return resultWindow{start: currentTurnStart(msgs), bound: true}
	}
	return resultWindow{start: len(msgs), bound: true}
}

// currentTurnStart returns the index just past the newest user message — where
// the content of the turn that is running right now begins. With no user
// message at all (an agent that has spoken unprompted), the whole conversation
// is the current turn.
func currentTurnStart(msgs []adapter.ConversationMessage) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return i + 1
		}
	}
	return 0
}

// renderSessionConversation reads and renders a session's adapter-owned
// conversation. ok is false for every "nothing to read" shape: no store row,
// non-renderer adapter, no conversation ref, unreadable file.
func renderSessionConversation(ctx context.Context, store *centralstore.Store, sessionID string) ([]adapter.ConversationMessage, bool) {
	if store == nil {
		return nil, false
	}
	row, ok, err := store.Session(ctx, centralstore.SessionID(sessionID))
	if err != nil || !ok || row.ConversationRef == "" {
		return nil, false
	}
	renderer, ok := adapters.FindByAdapter(row.Adapter).(adapter.ConversationRenderer)
	if !ok {
		return nil, false
	}
	msgs, err := renderer.RenderConversation(row.ConversationRef)
	if errors.Is(err, os.ErrNotExist) {
		// Pi reports the path for a brand-new conversation before creating the
		// JSONL file. That is a readable conversation with zero messages, not an
		// unsafe unknown boundary: everything later written to this path belongs
		// after this watermark. Treating ENOENT as unreadable makes the first
		// synchronous prompt exit 0 while omitting its answer.
		return []adapter.ConversationMessage{}, true
	}
	if err != nil {
		return nil, false
	}
	return msgs, true
}

// conversationReadable reports whether this session has a conversation gmux can
// read at all. It answers the CLI's hint question ("is `gmux agent output`
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

// latestAgentMessageIn reads the agent's final message for a result-bearing
// response (a resolved `gmux wait`, a synchronous prompt), attributed to the
// window's turn.
//
// It is the same read `scope=message` performs, through the same selector: one
// store lookup plus one adapter render, no runner call and no resume, so it is
// safe on a session that has just died. Every "nothing to show" case — no store
// row, non-renderer adapter, no conversation ref, unreadable conversation, no
// assistant prose inside our window — collapses to "", because a result-bearing
// wait must stay quiet rather than fail: those sessions are legitimately
// waitable, and `gmux agent output` reports the reason on demand.
func latestAgentMessageIn(ctx context.Context, store *centralstore.Store, sessionID string, window resultWindow) string {
	msgs, ok := renderSessionConversation(ctx, store, sessionID)
	if !ok {
		return ""
	}
	if window.bound && window.unreadable {
		// The conversation was unreadable when this turn was first observed, so
		// nothing in it can be attributed to the turn. `gmux agent output` is
		// still there for an explicit, snapshot-scoped read.
		return ""
	}
	var text string
	var found bool
	if window.bound {
		text, found = assistantProseInWindow(msgs, window.start)
	} else {
		text, found = latestAssistantProse(msgs)
	}
	if !found {
		return ""
	}
	return text
}

// assistantProseInWindow selects the newest assistant prose belonging to the
// turn that started at start.
//
// Both edges are load-bearing:
//
//   - start excludes everything that existed before our turn opened, so a
//     previous turn's answer can never be reported as ours (which is what makes
//     an empty window an honest "nothing to show" rather than a stale hit);
//   - the scan stops at the first user message INSIDE the window that is not
//     the leading one, because that message starts a LATER turn. Without it, a
//     turn prompted between our close observation and this read would have its
//     prose returned as ours.
//
// A leading user message is skipped rather than treated as a boundary: when the
// watermark is taken before delivery (the synchronous prompt), our own prompt is
// the first thing that lands in the window.
//
// A start past the end (a rotated or truncated conversation) yields an empty
// window and therefore no result, which is the conservative answer: we can no
// longer prove what our turn said.
func assistantProseInWindow(msgs []adapter.ConversationMessage, start int) (string, bool) {
	if start < 0 {
		start = 0
	}
	if start > len(msgs) {
		return "", false
	}
	window := msgs[start:]
	first := 0
	if len(window) > 0 && window[0].Role == "user" {
		first = 1
	}
	end := len(window)
	for i := first; i < len(window); i++ {
		if window[i].Role == "user" {
			end = i
			break
		}
	}
	for i := end - 1; i >= first; i-- {
		if window[i].Role != "assistant" {
			continue
		}
		if prose := strings.TrimRight(window[i].Prose, "\n"); prose != "" {
			return prose, true
		}
	}
	return "", false
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
