import { z } from 'zod'

// Schema v2 — matches gmux runner's GET /meta and gmuxd's store.Session

export const SessionStatusSchema = z.object({
  label: z.string(),
  working: z.boolean(),
  error: z.boolean().optional().default(false),
}).nullable()

export const SessionSchema = z.object({
  id: z.string().min(1),
  created_at: z.string().optional(),
  command: z.array(z.string()).optional(),
  cwd: z.string().optional(),
  workspace_root: z.string().optional(),
  kind: z.string().default('shell'),
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
  resume_key: z.string().optional(),
  socket_path: z.string().optional(),
  terminal_cols: z.number().int().positive().optional(),
  terminal_rows: z.number().int().positive().optional(),
  shell_title: z.string().optional(),
  adapter_title: z.string().optional(),
  binary_hash: z.string().optional(),
  stale: z.boolean().optional().default(false),
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
