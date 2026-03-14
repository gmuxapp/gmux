import { initTRPC } from '@trpc/server'
import { z } from 'zod'
import type { GmuxdClient } from '../gmuxd/client.js'

type TrpcContext = {
  gmuxd: GmuxdClient
}

const t = initTRPC.context<TrpcContext>().create()

export const appRouter = t.router({
  health: t.procedure.query(async ({ ctx }) => {
    const health = await ctx.gmuxd.health()
    return { ok: true, gmuxd: health }
  }),

  config: t.procedure.query(async ({ ctx }) => {
    return ctx.gmuxd.getConfig()
  }),

  sessions: t.router({
    list: t.procedure.query(async ({ ctx }) => {
      return ctx.gmuxd.listSessions()
    }),

    attach: t.procedure
      .input(
        z.object({
          sessionId: z.string().min(1),
        }),
      )
      .mutation(async ({ ctx, input }) => {
        return ctx.gmuxd.attachSession(input.sessionId)
      }),

    kill: t.procedure
      .input(
        z.object({
          sessionId: z.string().min(1),
        }),
      )
      .mutation(async ({ ctx, input }) => {
        return ctx.gmuxd.killSession(input.sessionId)
      }),

    launch: t.procedure
      .input(
        z.object({
          launcher_id: z.string().optional(),
          command: z.array(z.string()).optional(),
          cwd: z.string().optional(),
        }),
      )
      .mutation(async ({ ctx, input }) => {
        return ctx.gmuxd.launchSession(input)
      }),
  }),
})

export type AppRouter = typeof appRouter
