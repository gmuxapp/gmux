import { z } from 'zod'

// Schema v2 — matches gmuxd's API response (GET /v1/sessions, session-upsert SSE)

export const SessionStatusSchema = z.object({
  working: z.boolean(),
  error: z.boolean().optional().default(false),
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
  alive: z.boolean(),
  pid: z.number().optional().nullable(),
  exit_code: z.number().optional().nullable(),
  started_at: z.string().optional(),
  exited_at: z.string().optional().nullable(),
  title: z.string().optional(),
  subtitle: z.string().optional(),
  status: SessionStatusSchema.optional().nullable(),
  unread: z.boolean().optional().default(false),
  resumable: z.boolean().optional().default(false),
  // Absolute path of the agent conversation file this session holds, as
  // reported by the agent hook (ADR 0011). Two live sessions sharing one
  // conversation_file means the same conversation is open in multiple tabs;
  // the UI surfaces that as an "open elsewhere" warning.
  conversation_file: z.string().optional(),
  // RFC3339 timestamp of the most recent noteworthy state transition
  // (exited, unread on, working on, error on). Set by the owning
  // daemon; the UI uses it to populate the "Recent" section on the
  // home dashboard and as a sort key. Brand-new sessions arrive with
  // this unset; the first follow-up transition stamps it. See the
  // store.Session docstring on LastActivityAt for the exact bump set.
  last_activity_at: z.string().optional(),
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
