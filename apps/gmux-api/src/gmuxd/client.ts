import {
  AttachResponseSchema,
  SessionSummarySchema,
  successEnvelope,
} from '@gmux/protocol'
import { z } from 'zod'

const HealthDataSchema = z.object({
  service: z.string(),
  node_id: z.string().optional(),
})

const ListSessionsEnvelopeSchema = successEnvelope(z.array(SessionSummarySchema))
const AttachEnvelopeSchema = successEnvelope(AttachResponseSchema)
const HealthEnvelopeSchema = successEnvelope(HealthDataSchema)

export type GmuxdClient = ReturnType<typeof createGmuxdClient>

export function createGmuxdClient(baseUrl: string) {
  const normalizedBaseUrl = baseUrl.replace(/\/$/, '')

  async function getJson(path: string) {
    const response = await fetch(`${normalizedBaseUrl}${path}`)
    if (!response.ok) {
      throw new Error(`gmuxd request failed: ${response.status} ${response.statusText}`)
    }
    return response.json()
  }

  return {
    async health() {
      const json = await getJson('/v1/health')
      return HealthEnvelopeSchema.parse(json).data
    },

    async listSessions() {
      const json = await getJson('/v1/sessions')
      return ListSessionsEnvelopeSchema.parse(json).data
    },

    async attachSession(sessionId: string) {
      const response = await fetch(`${normalizedBaseUrl}/v1/sessions/${sessionId}/attach`, {
        method: 'POST',
      })

      if (!response.ok) {
        throw new Error(`gmuxd attach failed: ${response.status} ${response.statusText}`)
      }

      const json = await response.json()
      return AttachEnvelopeSchema.parse(json).data
    },

    async killSession(sessionId: string) {
      const response = await fetch(`${normalizedBaseUrl}/v1/sessions/${sessionId}/kill`, {
        method: 'POST',
      })

      if (!response.ok) {
        throw new Error(`gmuxd kill failed: ${response.status} ${response.statusText}`)
      }

      const json = await response.json()
      return json
    },
  }
}
