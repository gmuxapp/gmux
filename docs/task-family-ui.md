# Task-family UI integration seam

Task-family presentation uses durable `launch_parent_id` (wire:
`parent_session_id`) without mutating launch provenance. `promoted_to_root`
breaks only the presentation edge.

A resolved launch-parent relationship is a family edge only when both the child
and its direct parent carry `semantic_agent: true`. Missing parents, shells,
editors, and terminal helpers therefore remain presentation roots and cannot be
hidden accidentally.

The daemon derives `semantic_agent` from the existing
`adapter.ConversationSource` capability. This is the same semantic distinction
used by notification suppression and covers the conversation-backed Pi,
Claude, and Codex adapters without a frontend adapter-name list. Shell remains
false.

## Unresolved capability seam

`ConversationSource` describes conversation-backed agents rather than being a
dedicated family-membership marker. A future semantic agent with no persistent
conversation source will present as a root. If that case arrives, the adapter
package should gain a dedicated semantic-agent capability and all semantic
consumers should migrate together; do not patch it in the frontend with
adapter-name inference.
