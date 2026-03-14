import { z } from 'zod'

// Schema v2 — matches gmuxr's GET /meta and gmuxd's store.Session

export const StatusStateSchema = z.enum([
  'active',
  'attention',
  'success',
  'error',
  'paused',
  'info',
])

export const SessionStatusSchema = z.object({
  label: z.string(),
  state: StatusStateSchema,
  icon: z.string().optional(),
}).nullable()

export const SessionSchema = z.object({
  id: z.string().min(1),
  created_at: z.string().optional(),
  command: z.array(z.string()).optional(),
  cwd: z.string().optional(),
  kind: z.string().default('generic'),
  alive: z.boolean(),
  pid: z.number().optional().nullable(),
  exit_code: z.number().optional().nullable(),
  started_at: z.string().optional(),
  exited_at: z.string().optional().nullable(),
  title: z.string().optional(),
  subtitle: z.string().optional(),
  status: SessionStatusSchema.optional().nullable(),
  unread: z.boolean().optional().default(false),
  socket_path: z.string().optional(),
})

export const AttachResponseSchema = z.object({
  transport: z.enum(['websocket']),
  ws_path: z.string(),
  socket_path: z.string().optional(),
})

// Legacy aliases for backward compatibility during migration
export const SessionSummarySchema = SessionSchema
export type SessionSummary = z.infer<typeof SessionSchema>
export type Session = z.infer<typeof SessionSchema>
export type SessionStatus = z.infer<typeof SessionStatusSchema>
export type StatusState = z.infer<typeof StatusStateSchema>
export type AttachResponse = z.infer<typeof AttachResponseSchema>
