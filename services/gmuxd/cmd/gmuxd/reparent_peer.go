package main

import (
	"encoding/json"
	"fmt"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
)

// codeCrossPeer — the requested parent lives on a different daemon than the
// child. A family is one daemon's structure (ADR 0026): `parent_session_id`
// drives that daemon's ordering scopes, recursive dismissal, and notification
// suppression, none of which can span hosts. This is the one genuine refusal
// in the reparent family; it is not a forwarding limitation.
const codeCrossPeer = "cross_peer"

// rewritePeerReparentBody translates a reparent request addressed to a
// peer-owned child from the viewer's namespace into the owning daemon's own
// namespace, so the hub can forward it instead of refusing it.
//
// Promoting to root IS reparenting to null (single-axis families, #505), and
// for a peer session that is a same-peer operation: the session never leaves
// its daemon, only its parent pointer changes. The same holds for moving a
// peer child under another parent on that same peer. What cannot work — and
// stays refused — is a family spanning two daemons, in either direction.
//
// childPeer is the peer name the child ID resolved to. Returns the body to
// forward, or a non-empty (code, message) pair to refuse with.
//
// Unknown keys are preserved so a later, additive field on this route keeps
// working across a hub/spoke version skew: only the parent reference is
// rewritten.
func rewritePeerReparentBody(body []byte, childPeer string) (forward []byte, code, message string) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "bad_request", "invalid JSON"
	}
	raw, present := payload["parent_session_id"]
	if !present {
		return nil, "bad_request", "parent_session_id is required (use null to clear)"
	}
	// Promote-to-root: no reference to translate, nothing cross-peer about it.
	if string(raw) == "null" {
		return body, "", ""
	}
	var parentID string
	if err := json.Unmarshal(raw, &parentID); err != nil || parentID == "" {
		return nil, "bad_request", "parent_session_id must be a session id or null"
	}
	parentOriginal, parentPeer := peering.ParseID(parentID)
	if parentPeer != childPeer {
		where := "this daemon"
		if parentPeer != "" {
			where = fmt.Sprintf("peer %q", parentPeer)
		}
		return nil, codeCrossPeer, fmt.Sprintf(
			"cross-peer reparenting is not supported: the child is owned by peer %q and the requested parent by %s; a task family cannot span daemons",
			childPeer, where)
	}
	rewritten, err := json.Marshal(parentOriginal)
	if err != nil {
		return nil, "bad_request", "parent_session_id must be a session id or null"
	}
	payload["parent_session_id"] = rewritten
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, "bad_request", "invalid request body"
	}
	return out, "", ""
}
