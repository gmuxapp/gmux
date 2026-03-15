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

    async getConfig() {
      const json = await getJson('/v1/config')
      return json.data
    },

    async launchSession(opts: { launcher_id?: string; command?: string[]; cwd?: string }) {
      const response = await fetch(`${normalizedBaseUrl}/v1/launch`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(opts),
      })

      if (!response.ok) {
        throw new Error(`gmuxd launch failed: ${response.status} ${response.statusText}`)
      }

      return response.json()
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

    async dismissSession(sessionId: string) {
      const response = await fetch(`${normalizedBaseUrl}/v1/sessions/${sessionId}/dismiss`, {
        method: 'POST',
      })

      if (!response.ok) {
        throw new Error(`gmuxd dismiss failed: ${response.status} ${response.statusText}`)
      }

      const json = await response.json()
      return json
    },

    async setResizeOwner(sessionId: string, deviceId: string, cols: number, rows: number) {
      const response = await fetch(`${normalizedBaseUrl}/v1/sessions/${sessionId}/resize-owner`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ device_id: deviceId, cols, rows }),
      })

      if (!response.ok) {
        throw new Error(`gmuxd resize-owner failed: ${response.status} ${response.statusText}`)
      }

      return response.json()
    },

    async resumeSession(sessionId: string) {
      const response = await fetch(`${normalizedBaseUrl}/v1/sessions/${sessionId}/resume`, {
        method: 'POST',
      })

      if (!response.ok) {
        throw new Error(`gmuxd resume failed: ${response.status} ${response.statusText}`)
      }

      return response.json()
    },
  }
}
