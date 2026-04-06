// Home page: host status, project overview, quick-launch.
// Reads shared data from the store (signals).

import { useState } from 'preact/hooks'
import { health, peers, folders, sessions, launchers } from './store'
import type { Folder } from './types'
import type { LauncherDef } from './launcher'
import { launchSession } from './launcher'

/** Mask tailnet name for privacy: "gmux.angler-map.ts.net" → "gmux.an****.ts.net" */
function maskTailnet(fqdn: string): string {
  return fqdn.replace(/(\.\w{2})[^.]*(?=\.ts\.net)/, '$1****')
}

export function Home() {
  const healthVal = health.value
  const peersVal = peers.value
  const foldersVal = folders.value
  const sessionsVal = sessions.value
  const launchersVal = launchers.value

  const localSessions = sessionsVal.filter(s => !s.peer && s.alive).length
  const hostname = healthVal?.hostname ?? 'local'
  const tsFqdn = healthVal?.tailscale_url?.replace('https://', '')

  return (
    <div class="home">
      {/* ── Hosts ── */}
      <section class="home-hosts">
        <h2 class="home-section-title">Hosts</h2>
        <div class="home-host-grid">
          <LocalHostCard
            hostname={hostname}
            tsFqdn={tsFqdn}
            version={healthVal?.version}
            sessionCount={localSessions}
          />
          {peersVal.map(p => (
            <PeerCard key={p.name} peer={p} />
          ))}
        </div>
      </section>

      {/* ── Projects ── */}
      {foldersVal.length > 0 && (
        <section class="home-projects">
          <h2 class="home-section-title">Projects</h2>
          <div class="home-project-grid">
            {foldersVal.map(f => <ProjectCard key={f.path} folder={f} />)}
          </div>
        </section>
      )}

      {/* ── Quick launch (no project context) ── */}
      <QuickLaunch launchers={launchersVal} />
    </div>
  )
}

function ProjectCard({ folder: f }: { folder: Folder }) {
  const alive = f.sessions.filter(s => s.alive).length
  const resumable = f.sessions.filter(s => !s.alive && s.resumable).length
  return (
    <a class="home-project-card" href={`/${f.path}`}>
      <div class="home-project-name">{f.name}</div>
      <div class="home-project-count">
        {alive > 0 && <span class="home-project-alive">{alive} alive</span>}
        {alive > 0 && resumable > 0 && <span class="home-project-rest"> · </span>}
        {resumable > 0 && <span class="home-project-rest">{resumable} resumable</span>}
        {alive === 0 && resumable === 0 && <span class="home-project-rest">no sessions</span>}
      </div>
    </a>
  )
}

function LocalHostCard({
  hostname, tsFqdn, version, sessionCount,
}: {
  hostname: string
  tsFqdn?: string
  version?: string
  sessionCount: number
}) {
  return (
    <div class="home-host-card">
      <div class="home-host-top">
        <span class="home-host-status connected" />
        <span class="home-host-name">{hostname}</span>
        <span class="home-host-badge">local</span>
      </div>
      <div class="home-host-details">
        {tsFqdn && <div class="home-host-detail">{maskTailnet(tsFqdn)}</div>}
        {version && <div class="home-host-detail">v{version}</div>}
        <div class="home-host-detail">{sessionCount} active session{sessionCount === 1 ? '' : 's'}</div>
      </div>
    </div>
  )
}

function PeerCard({ peer }: { peer: import('./types').PeerInfo }) {
  const aliveSessions = peer.session_count
  return (
    <div class="home-host-card">
      <div class="home-host-top">
        <span class={`home-host-status ${peer.status}`} />
        <span class="home-host-name">{peer.name}</span>
        <span class="home-host-badge">peer</span>
      </div>
      <div class="home-host-details">
        <div class="home-host-detail">{peer.url.replace(/^https?:\/\//, '')}</div>
        <div class="home-host-detail">
          {peer.status === 'connected'
            ? `${aliveSessions} active session${aliveSessions === 1 ? '' : 's'}`
            : peer.status}
        </div>
      </div>
    </div>
  )
}

function QuickLaunch({ launchers }: { launchers: LauncherDef[] }) {
  const [launching, setLaunching] = useState<string | null>(null)

  const handleLaunch = (id: string) => {
    setLaunching(id)
    launchSession(id).finally(() => setLaunching(null))
  }

  const defaultLauncher = launchers.find(l => l.id === 'shell') ?? launchers[0]
  const others = launchers.filter(l => l !== defaultLauncher)
  if (!defaultLauncher) return null

  return (
    <section class="home-quick-launch">
      <h2 class="home-section-title">Quick launch</h2>
      <div class="home-launch-row">
        <button
          class={`home-launch-btn ${launching === defaultLauncher.id ? 'launching' : ''}`}
          onClick={() => handleLaunch(defaultLauncher.id)}
          disabled={launching !== null}
        >
          {defaultLauncher.label}
        </button>
        {others.map(l => (
          <button
            key={l.id}
            class={`home-launch-btn ${launching === l.id ? 'launching' : ''} ${!l.available ? 'unavailable' : ''}`}
            onClick={() => handleLaunch(l.id)}
            disabled={launching !== null || !l.available}
          >
            {l.label}
          </button>
        ))}
      </div>
      <div class="home-launch-hint">
        or <code>gmux {'<command>'}</code> from any terminal
      </div>
    </section>
  )
}
