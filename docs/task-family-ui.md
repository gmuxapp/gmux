# Task-family UI integration seam

A task family is defined by one mutable edge: `parent_session_id`. A session
with no current parent is a root. That edge is the only behavioral one: it
drives sidebar grouping, recursive dismissal, active-subagent budget roots and
depth, and completion-notification suppression.

`launched_from_session_id` is immutable launch provenance. It has no automatic
behavior. Its only consumer is the web menu's **Return to family** suggestion,
which reparents a root back to its launch parent.

A resolved parent relationship is a family edge when its direct parent carries
`semantic_agent: true`; the child may be an agent or any other process session.
Missing parents and children of shells, editors, or terminal helpers remain
presentation roots and cannot be hidden accidentally.

`gmux promote <id>` severs the current edge and makes a session a root;
`gmux reparent <id> <parent-id>` moves it under another session. The web UI
exposes the same pair in the session's `⋮` menu ("Promote to root" /
"Return to family", `promotionAction` in `family.ts`), offered only for
daemon-owned sessions and only while the return target resolves locally as a
semantic agent. Promote is blocked (visible, disabled, with the reason) when no
project places the session: an unplaced root has no sidebar row and no routable
URL, and the daemon deliberately gives parentage no say in project matching.
Return to family is blocked the same way when the resulting family root has no
stamp-backed placement, so an outside-project parent cannot strand the selected
child. Promotion re-roots the active-subagent budget under the promoted session
and removes its notification suppressor. Both mutations are local-only;
self-parenting, ancestor cycles, and cross-peer reassignment are rejected
transactionally.

The daemon derives `semantic_agent` from the existing
`adapter.ConversationSource` capability. It covers the conversation-backed Pi,
Claude, and Codex adapters without a frontend adapter-name list. Shell remains
false.

## Header and panel presentation

The header speaks one control language: ghost icon buttons (borderless,
`--bg-hover` on hover, sized to the ⋮ menu trigger). For family members
the title row is the ancestor breadcrumb — `[family icon] ●root › ●parent
› title` — where each crumb is a ghost link carrying that ancestor's live
`sessionDotState` dot; the current session stays a plain bold title. Depth > 3
collapses the middle to a static `…`. On narrow screens the crumbs wrap onto
their own row above the title. The family trigger (3-node tree SVG) toggles the
panel; there is no cwd, member count, or separate parent/root button.

The panel is a non-modal popover in the ⋮ dropdown's visual language. It closes
on outside pointerdown, Escape, or its trigger; clicking a row navigates without
closing it. The root is always present, followed by a line-budgeted family tree
with per-level `+N more` / `show fewer` folds. Ordinary tree order is recency of
`last_output_at ?? created_at`; state does not otherwise reorder the tree.

Outside the panel a root row stands in for its whole family and has at most one
subordinate row, led by a static family button (the family icon) that opens the
panel. While a descendant is selected, the selected member's row sits beside the
icon; otherwise the icon-number summary lives inside the button, shown when the
family has reportable activity. Both states share one
row height, so selection never shifts the list. Root selection never restores a
previously viewed member; a member that needs a persistent row can be promoted
to a root.

For agent members, `familyDotById` aggregates the highest-precedence dot onto
the presentation root, and `unreadCount` adds unread descendants (alive or
retained-dead) to their folder-visible root. Process unread contributes to
neither aggregation; running processes use the separate `$` summary described
below.

### Proposed process filter and glyph language

Agent turn state and process lifecycle are different axes. A completed process
may have unseen output, and it may have exited non-zero, but neither fact makes
it an agent “waiting on you” or an overview error: the agent that launched the
command owns its outcome. Like a task runner, the family overview presents a
process as either **running** or **finished**. Exit codes remain available on the
process's own terminal surface, not in family summaries or filters.

The panel's filter bar has these controls when their population exists:

- **all** — unchanged: show the ordinary family tree, agents and processes
  together, subject to the panel's normal line budget and folds;
- **N error** — inactive agents with unread error outcomes (`waiting-error`);
- **N waiting** — inactive unread agents without error (`waiting`);
- **N active** — active agents, including transient retry/rate-limit errors;
- **$ N running** — when one or more processes are running; or
- **$ processes** — when process sessions exist but none is running.

The root is excluded from these counts, as it is from the standard family
numbers today; its own state remains visible on its row. “Processes exist”
includes retained finished sessions for as long as they remain in the family
snapshot. A process is running exactly while `alive && status.active`; every
other process in the snapshot is finished for this overview, including a live
shell at its prompt and a process that exited non-zero.

The last two labels are two presentations of one **processes** filter. Pressing
either shows all process sessions, not only the running subset. When present,
the number is specifically the number currently running; it is not the number
of rows the filter will return. The process control is never attention-styled.
The filters do partition the descendants for overview purposes: error, waiting,
and active contain agents only; processes contains processes only. An agent in
error follows the existing state precedence and appears in one agent-state
filter.

The processes view is a flat task list rather than a family tree. Each row names
its parent agent as secondary context so repeated commands from different
agents remain distinguishable. It uses `last_output_at ?? created_at`, newest
first, under two subheadings. The selected process is pinned before the row
budget; if it falls outside its section's recency slice, it leads that section
and the remaining admitted rows retain recency order:

1. **Running · N** — running processes;
2. **Finished · N** — non-running processes.

Omit an empty section. Thus a family with no running processes opens directly
on **Finished**, while a family whose processes are all running has no
**Finished** heading. The ordinary **all** tree remains structural; this flat,
running-first rule applies only to the processes filter.

The existing total member-row budget still bounds the processes view. Running
rows are admitted first, then finished rows. Each nonempty section keeps its
heading and, when folded, its own `+N more` control; headings do not consume the
member-row budget. Expanding a section reveals all of that section in the same
way as expanding a tree level today. A folded Finished section therefore
remains visible and reachable even when running processes consume the initial
row budget.

`$` always identifies a process; a filled dot identifies agent state. Process
error, unread, active-agent, and waiting-agent colors never recolor `$` or add a
family-overview status marker. Its color has surface-specific presence
semantics:

- in summaries outside the filter bar, render a cyan `$` only when at least one
  process is currently running; render no process glyph when none is running;
- on the **processes** filter control, `$` is cyan while at least one process is
  running and gray otherwise. The gray form says “process history is
  available,” not “a gray process state”;
- wherever an individual process is represented by `$`, including **all** and
  **processes** rows, keep the glyph cyan whether the process is running or
  finished.

Accessible summary text follows the same presence rule: announce “N running
processes” only when at least one is running. The gray control's accessible
name is “Processes”; the cyan counting form is “Processes, N running,” so both
lead with the population the control opens rather than implying its number is
the result count.

An agent-owned family process may remain unread internally for notification
delivery and explicit consumption, but its unread state never contributes to
family/root attention presentation—even if a prior command remains unread
while that process starts another. In particular, it contributes neither to
`familyDotById` nor to the family's roll-up in the sidebar's `unreadCount`, and
it produces no waiting count, filled waiting dot, process summary, unread
section, or unread marker in the processes view. A standalone process still
owns a visible sidebar row and badges its own unread output normally. Running
family processes contribute only the running-process summary, not an
agent-style aggregate dot.

Bulk actions continue to follow the active filter. Waiting and error retain
**Mark all read** for agents. The processes filter has no bulk action: it is a
task history/type view, not an attention queue.

## Attention and consumption

Unread is independent of durable error outcome. Every completed agent turn
records unread until a consumer reads or acts on that session. Process commands
normally do the same, with one supervision exception: a successful process-child
exit is durably auto-acknowledged when its current direct parent has a live local
runner. The completion commit records the child as read, so suppression is not
merely a notification-delivery decision; family roll-ups and later readers also
observe no unread result.

The exception is deliberately strict and one hop. Semantic agent children,
failed or interrupted processes, and children whose current direct parent is
dead, missing, or remote retain unread. A promoted child has no parent and also
retains unread. The coordinator decides from the durable parent edge and local
runner registry at completion; concurrent reparenting forces it to re-read and
retry. This is an instantaneous, one-shot supervision policy: a parent that
dies just after the child exits does not restore unread, and a parent that
becomes live later does not retroactively clear it.

Reading clears unread only. An acknowledged inactive error has no presentation
state; an active error remains a hollow red current-status ring and is not
family attention.

`gmux wait` (on success), `gmux tail`, `gmux agent logs`, prompts, steering,
raw sends, and web interaction consume unread. The family panel's bulk
**mark read** clears a retained pile in one action.

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
