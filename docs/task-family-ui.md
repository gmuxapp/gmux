# Task-family UI integration seam

Task-family presentation uses durable `launch_parent_id` (wire:
`parent_session_id`). Normal lifecycle and placement operations never mutate
this launch provenance; the explicit `gmux session parent` operation may
reassign or clear the direct edge. `promoted_to_root` breaks only the
presentation edge and never erases the stored parent.

A resolved launch-parent relationship is a family edge when its direct parent
carries `semantic_agent: true`; the child may be an agent or any other process
session. Missing parents and children of shells, editors, or terminal helpers
remain presentation roots and cannot be hidden accidentally. Reparenting to a
non-agent is therefore valid stored provenance but presentation-inert.

`gmux session promote|demote` exposes the existing sticky promotion state.
`gmux session parent <id> <parent-id>` (or `--clear`) changes the direct parent
used by this projection and by child-notification suppression. Both rows must
exist on the local daemon; self-parenting, ancestor cycles, and cross-peer
reassignment are rejected transactionally.

The daemon derives `semantic_agent` from the existing
`adapter.ConversationSource` capability. This is the same semantic distinction
used by notification suppression and covers the conversation-backed Pi,
Claude, and Codex adapters without a frontend adapter-name list. Shell remains
false.

## Drawer presentation (noise reduction)

Every children list in the drawer (the top-level sibling list included) is
partitioned into attention → working → idle → finished groups, classified
by the `sessionDotState` precedence so a row's dot and its group never
disagree: error and unread demand attention (unread is not alive-gated —
unseen output pings until viewed, dead or not), active work is working,
and only quiet sessions land in idle/finished. A node sorts into the
highest-urgency bucket found in its entire subtree, so collapsed summaries
can never hide urgent descendants. Groups are capped (attention ∞,
working 20, idle 10, finished 3) behind `+N …` summary rows with
per-(parent, bucket) expansion that resets when the drawer closes. The
projection is frozen against ordinary live updates while the drawer is
open — dots update in place, rows never re-sort under the cursor — and
re-projects from current facts on the two explicit user actions: selecting
another session and expanding/collapsing a group.

Outside the drawer a root row stands in for its whole family:
`familyDotById` aggregates the highest-precedence member dot onto the
presentation root, and `unreadCount` adds alive unread descendants to their
folder-visible root (the count keeps its existing alive gate). Processes
render with a `$` glyph and are excluded from the header pill's
`Agents · N` count.

## Unresolved capability seam

`ConversationSource` describes conversation-backed agents rather than being a
dedicated family-membership marker. A future semantic agent with no persistent
conversation source will present as a root. If that case arrives, the adapter
package should gain a dedicated semantic-agent capability and all semantic
consumers should migrate together; do not patch it in the frontend with
adapter-name inference.
