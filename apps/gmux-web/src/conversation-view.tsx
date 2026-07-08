/**
 * Conversation view (ADR 0021): the boundary where the Preact app hands off to
 * the @assistant-ui/react island.
 *
 * This file is deliberately tiny. It owns the framework-agnostic store
 * (conversation.ts) and the live ACP WebSocket, then mounts a *real React 18*
 * root (conversation-island.tsx) into a plain DOM node. Preact renders the
 * container; React owns everything inside it. The two runtimes never share a
 * tree — the only crossing is props passed at mount and imperative re-renders
 * when the props change.
 *
 * Why a manual React root rather than preact/compat: assistant-ui relies on
 * genuine React 18 semantics (useSyncExternalStore, concurrent rendering,
 * context identity) that the compat shim reproduces imperfectly. See
 * vite.config.ts (`reactAliasesEnabled: false`).
 */
import { useEffect, useRef } from 'preact/hooks'
import { createRoot, type Root } from 'react-dom/client'
import { createElement } from 'react'
import {
  type ConversationStore,
  createConversationStore,
  connectConversation,
  sendSessionInput,
} from './conversation'
import { ConversationIsland } from './conversation-island'

interface Props {
  sessionId: string
  /** gmux "working" status of the session; surfaced as the composer indicator. */
  working?: boolean
  /** Injectable for tests; defaults to a fresh store wired to the live WS. */
  store?: ConversationStore
  /** When false, skip opening the WebSocket (tests pass a pre-fed store). */
  connect?: boolean
}

export function ConversationView({ sessionId, working, store: injected, connect = true }: Props) {
  const hostRef = useRef<HTMLDivElement>(null)
  const rootRef = useRef<Root | null>(null)
  const storeRef = useRef<ConversationStore>()
  if (!storeRef.current) storeRef.current = injected ?? createConversationStore()

  // Live ACP stream → store. Store lifetime matches this component.
  useEffect(() => {
    if (!connect) return
    const conn = connectConversation(sessionId, storeRef.current!)
    return () => conn.close()
  }, [sessionId, connect])

  // Create the React root once, tear it down on unmount.
  useEffect(() => {
    if (!hostRef.current) return
    const root = createRoot(hostRef.current)
    rootRef.current = root
    return () => {
      rootRef.current = null
      root.unmount()
    }
  }, [])

  // (Re)render the island whenever the session changes. Runs after the mount
  // effect above on first commit, so the root always exists here. The composer
  // sends via the §6 HTTP keystroke path keyed by sessionId — no dependency on
  // a mounted terminal.
  useEffect(() => {
    rootRef.current?.render(
      createElement(ConversationIsland, {
        store: storeRef.current!,
        working,
        sessionId,
        onSend: (data: string) => void sendSessionInput(sessionId, data),
      }),
    )
  }, [sessionId, working])

  return <div ref={hostRef} class="conversation-view" />
}
