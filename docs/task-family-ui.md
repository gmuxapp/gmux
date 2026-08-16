# Task-family UI integration seam

Task-family presentation uses durable `parent_session_id` as its organizational
edge. `launched_from_session_id` is write-once launch history and is never sent
to the frontend. `promoted_to_root` breaks only the presentation edge; it does
not erase either stored fact.

A resolved parent relationship is a family edge when its direct parent carries
`semantic_agent: true`; the child may be an agent or any other process session.
Missing parents and children of shells, editors, or terminal helpers remain
presentation roots and cannot be hidden accidentally.

`gmux session promote|demote` exposes the sticky presentation override; the
web UI exposes the same pair in the session's `⋮` menu ("Promote to root" /
"Return to family", `promotionAction` in `family.ts`), offered only for
daemon-owned sessions and only while the demote target — the current
organizational parent — resolves locally as a semantic agent. Promote is
additionally blocked (visible, disabled, with the reason) when no project
places the session: an unplaced promoted root has no sidebar row and no
routable URL, and the daemon deliberately gives parentage no say in project
matching. Promotion also re-roots the active-subagent budget under the
promoted session; demotion is offered only when the post-demotion family root
has the same stamp-backed sidebar placement, so an outside-project parent cannot
strand the selected child. Notification suppression is untouched by both
promote and reparent — it follows the immutable launch parent (see below).
`gmux session reparent <id> <parent-id>` (or `--clear`) changes the direct
parent used by this projection, recursive dismissal, and the active-subagent
budget. Both rows must exist on the local daemon; self-parenting, ancestor
cycles, and cross-peer reassignment are rejected transactionally.

The daemon derives `semantic_agent` from the existing
`adapter.ConversationSource` capability. It covers the conversation-backed Pi,
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
presentation root, and `unreadCount` adds unread descendants (alive or
retained-dead) to their folder-visible root. Processes render with a `$` glyph
in place of the dot.

## Attention and consumption

Unread is independent of family presentation: every completed agent turn or
process command records unread until a consumer reads or acts on that session.
Notification delivery is suppressed only when the direct launch parent is
active at the committed completion instant. Agent activity is semantic turn
activity; terminal activity currently comes from the runner's OSC 133 prompt
cycle (active command to idle prompt). This parent check is one hop and one
shot—later parent activity never retro-delivers a suppressed notification.

`gmux wait` (on success), `gmux tail`, `gmux agent logs`, prompts, steering,
raw sends, and web interaction consume unread. To remediate
retained child piles created by versions where waits did not consume, run:

```sh
gmux read --family <root-id>
```

The focused-session notification check intentionally has no inactivity timer;
a future idle-delivery policy can use the existing presence interaction stamp
without changing unread semantics.

## Unresolved capability seam

`ConversationSource` describes conversation-backed agents rather than being a
dedicated family-membership marker. A future semantic agent with no persistent
conversation source will present as a root. If that case arrives, the adapter
package should gain a dedicated semantic-agent capability and all semantic
consumers should migrate together; do not patch it in the frontend with
adapter-name inference.
