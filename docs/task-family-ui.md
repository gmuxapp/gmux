# Task-family UI integration seam

Task-family presentation uses durable `launch_parent_id` (wire:
`parent_session_id`) without mutating launch provenance. `promoted_to_root`
breaks only the presentation edge.

A resolved launch-parent relationship is a family edge when its direct parent
carries `semantic_agent: true`; the child may be an agent or any other process
session. Missing parents and children of shells, editors, or terminal helpers
remain presentation roots and cannot be hidden accidentally.

The daemon derives `semantic_agent` from the existing
`adapter.ConversationSource` capability. This is the same semantic distinction
used by notification suppression and covers the conversation-backed Pi,
Claude, and Codex adapters without a frontend adapter-name list. Shell remains
false.

## Drawer presentation (noise reduction)

Every children list in the drawer (the top-level sibling list included) is
partitioned into attention → working → idle → finished groups, ordered by
the `sessionDotState` precedence so a row's dot and its group never
disagree; everything dead is `finished` regardless of unread. A node sorts
into the highest-urgency bucket found in its entire subtree, so collapsed
summaries can never hide live work. Groups are capped (attention ∞,
working 20, idle 10, finished 3) behind `+N …` summary rows with
per-(parent, bucket) expansion that resets when the drawer closes. The
projection is frozen while the drawer is open: dots update in place, but
rows never re-sort under the cursor.

Outside the drawer a root row stands in for its whole family:
`familyDotById` aggregates the highest-precedence member dot onto the
presentation root, and `unreadCount` adds alive unread descendants to their
folder-visible root. Dead members never contribute (a never-viewed dead
child is noise, not attention). Processes render with a `$` glyph and are
excluded from the header pill's `Agents · N` count.

## Unresolved capability seam

`ConversationSource` describes conversation-backed agents rather than being a
dedicated family-membership marker. A future semantic agent with no persistent
conversation source will present as a root. If that case arrives, the adapter
package should gain a dedicated semantic-agent capability and all semantic
consumers should migrate together; do not patch it in the frontend with
adapter-name inference.
