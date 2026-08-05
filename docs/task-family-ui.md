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

## Header and panel presentation

The header speaks one control language: ghost icon buttons (borderless,
`--bg-hover` on hover, sized to the ⋮ menu trigger). For family members
the title row is the ancestor breadcrumb — `[family icon] ●root › ●parent
› title` — where each crumb is a ghost link carrying that ancestor's live
`sessionDotState` dot; the current session stays a plain bold title (its
state lives in the status chip). Depth > 3 collapses the middle to a
static `…`. On narrow screens the crumbs wrap onto their own row above
the title. The family trigger (3-node tree SVG) shows the family's
aggregated dot as a corner badge and toggles the panel; there is no cwd,
no member count, and no separate parent/root buttons.

The panel is a non-modal popover in the ⋮ dropdown's visual language
(width `max-content` clamped 320–440px on desktop, full-width sheet on
mobile). It closes on outside mousedown, Escape, or the trigger; clicking
a row navigates without closing it. Content is a counts line (`N error ·
N working · N unread · N total`, dot-precedence tallies) plus the family
tree. Every children level is one flat list in sidebar activity-mode
order: recency of `last_output_at ?? created_at`, no status buckets, no
alive/dead distinction — state is only the row's five-state dot. The
panel renders live; like the sidebar, a row moves only when new output
arrives. Levels are capped at 15 rows behind a two-state `+N more` /
`show fewer` summary keyed per parent (recency does the triage: unread
members have recent output by definition, so they surface within the
cap while long-dead noise sinks below the fold).

Outside the panel a root row stands in for its whole family:
`familyDotById` aggregates the highest-precedence member dot onto the
presentation root, and `unreadCount` adds alive unread descendants to
their folder-visible root (the count keeps its existing alive gate).
Processes render with a `$` glyph in place of the dot.

## Unresolved capability seam

`ConversationSource` describes conversation-backed agents rather than being a
dedicated family-membership marker. A future semantic agent with no persistent
conversation source will present as a root. If that case arrives, the adapter
package should gain a dedicated semantic-agent capability and all semantic
consumers should migrate together; do not patch it in the frontend with
adapter-name inference.
