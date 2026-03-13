import { serve } from '@hono/node-server'
import { fetchRequestHandler } from '@trpc/server/adapters/fetch'
import { Hono } from 'hono'
import { cors } from 'hono/cors'
import { createGmuxdClient } from './gmuxd/client.js'
import { appRouter } from './trpc/router.js'

const app = new Hono()

const gmuxdBaseUrl = process.env.GMUXD_BASE_URL ?? 'http://127.0.0.1:8790'
const gmuxd = createGmuxdClient(gmuxdBaseUrl)

app.use('/*', cors())

app.get('/health', async (c) => {
  const health = await gmuxd.health()
  return c.json({ ok: true, service: 'gmux-api', gmuxd: health })
})

app.all('/trpc/*', (c) => {
  return fetchRequestHandler({
    endpoint: '/trpc',
    req: c.req.raw,
    router: appRouter,
    createContext: async () => ({ gmuxd }),
  })
})

app.get('/api/events', async (c) => {
  const upstream = await fetch(`${gmuxdBaseUrl}/v1/events`, {
    headers: {
      accept: 'text/event-stream',
    },
  })

  if (!upstream.ok || !upstream.body) {
    return c.json(
      {
        ok: false,
        error: `failed to connect to gmuxd events: ${upstream.status}`,
      },
      502,
    )
  }

  return new Response(upstream.body, {
    headers: {
      'content-type': 'text/event-stream',
      'cache-control': 'no-cache',
      connection: 'keep-alive',
    },
  })
})

const port = Number(process.env.PORT ?? 8787)

serve({ fetch: app.fetch, port }, () => {
  console.log(`gmux-api listening on :${port}`)
  console.log(`gmux-api -> gmuxd ${gmuxdBaseUrl}`)
})
