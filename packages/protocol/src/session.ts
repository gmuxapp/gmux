import { z } from 'zod'

// Schema v2 — matches gmuxd's API response (GET /v1/sessions, session-upsert SSE)

export const SessionStatusSchema = z.object({
  active: z.boolean(),
  error: z.boolean().optional().default(false),
  interrupted: z.boolean().optional().default(false),
}).nullable()

export const SessionSchema = z.object({
  id: z.string().min(1),
  peer: z.string().optional(),
  created_at: z.string().optional(),
  command: z.array(z.string()).optional(),
  cwd: z.string().optional(),
  workspace_root: z.string().optional(),
  remotes: z.record(z.string()).optional(),
  adapter: z.string().default('shell'),
  // Drive mode (ADR 0033): how gmux hosts this harness. Absent on the
  // wire means terminal (the pre-mode shape); 'acp' sessions have no PTY
  // and render the conversation view instead of a terminal.
  drive_mode: z.enum(['terminal', 'acp']).optional().default('terminal'),
  // Session this one was spawned from (e.g. `gmux edit` invoked as
  // $EDITOR inside an existing session). The UI places the child
  // directly under its parent in the sidebar.
  parent_session_id: z.string().optional(),
  // Immutable provenance: the session from which this one was launched.
  launched_from_session_id: z.string().optional(),
  // True when the adapter exposes gmux's conversation-backed semantic-agent
  // capability. Both endpoints must be true for a task-family edge.
  semantic_agent: z.boolean().optional().default(false),
  alive: z.boolean(),
  pid: z.number().optional().nullable(),
  exit_code: z.number().optional().nullable(),
  started_at: z.string().optional(),
  exited_at: z.string().optional().nullable(),
  title: z.string().optional(),
  subtitle: z.string().optional(),
  status: SessionStatusSchema.optional().nullable(),
  unread: z.boolean().optional().default(false),
  // Opaque runner-owned result identity. Read acknowledgements must name the
  // token they actually observed so a delayed read cannot clear a newer
  // completion, including across runner replacement.
  unread_token: z.string().optional().default(''),
  resumable: z.boolean().optional().default(false),
  // Absolute path of the agent conversation file this session holds, as
  // reported by the agent hook (ADR 0011). Two live sessions sharing one
  // conversation_file means the same conversation is open in multiple tabs;
  // the UI surfaces that as an "open elsewhere" warning.
  conversation_file: z.string().optional(),
  // RFC3339 timestamp of the last time this session produced *unseen*
  // result output (including a newer result while already unread). Set by the owning daemon; the UI uses it
  // as the activity-feed sort key so sessions float up when the agent
  // (or shell/editor) produces something you haven't looked at.
  // Deliberately NOT bumped by your own input (going active) or by exit/
  // error. Brand-new sessions arrive unset; the first unread transition
  // stamps it. A future last_input_at could track the user side. See
  // the store.Session docstring on LastOutputAt for the exact bump set.
  last_output_at: z.string().optional(),
  socket_path: z.string().optional(),
  terminal_cols: z.number().int().positive().optional(),
  terminal_rows: z.number().int().positive().optional(),
  slug: z.string().optional(),
  runner_version: z.string().optional(),
  binary_hash: z.string().optional(),
  // Project assignment stamps populated by the session's origin host.
  // Drive sidebar bucketing (ADR 0002): a session is rendered under
  // (peer, project_slug) iff project_slug is non-empty. project_index
  // is the session's authoritative position inside that project.
  project_slug: z.string().optional(),
  project_index: z.number().int().nonnegative().optional(),
})

export const AttachResponseSchema = z.object({
  transport: z.enum(['websocket']),
  ws_path: z.string(),
  socket_path: z.string().optional(),
})

export const SessionSummarySchema = SessionSchema
export type SessionSummary = z.infer<typeof SessionSchema>
export type Session = z.infer<typeof SessionSchema>
export type SessionStatus = z.infer<typeof SessionStatusSchema>
export type AttachResponse = z.infer<typeof AttachResponseSchema>
