// Home page: host status, project overview, quick-launch.

import type { Session, PeerInfo, Folder } from './types'
import type { HealthData } from './use-session-data'
import type { LauncherDef } from './launcher'
import { launchSession } from './launcher'
import { useState } from 'preact/hooks'

interface HomeProps {
  health: HealthData | null
  peers: PeerInfo[]
  folders: Folder[]
  sessions: Session[]
  launchers: LauncherDef[]
}

/** Mask tailnet name for privacy: "gmux.angler-map.ts.net" → "gmux.an****.ts.net" */
function maskTailnet(fqdn: string): string {
  return fqdn.replace(/(\.\w{2})[^.]*(?=\.ts\.net)/, '$1****')
}

export function Home({ health, peers, folders, sessions, launchers }: HomeProps) {
  const localSessions = sessions.filter(s => !s.peer && s.alive).length
  const hostname = health?.hostname ?? 'local'
  const tsFqdn = health?.tailscale_url?.replace('https://', '')

  return (
    <div class="home">
      {/* ── Hosts ── */}
      <section class="home-hosts">
        <h2 class="home-section-title">Hosts</h2>
        <div class="home-host-grid">
          <LocalHostCard
            hostname={hostname}
            tsFqdn={tsFqdn}
            version={health?.version}
            sessionCount={localSessions}
          />
          {peers.map(p => (
            <PeerCard key={p.name} peer={p} />
          ))}
        </div>
      </section>

      {/* ── Projects ── */}
      {folders.length > 0 && (
        <section class="home-projects">
          <h2 class="home-section-title">Projects</h2>
          <div class="home-project-grid">
            {folders.map(f => <ProjectCard key={f.path} folder={f} />)}
          </div>
        </section>
      )}

      {/* ── Quick launch (no project context) ── */}
      <QuickLaunch launchers={launchers} />
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

function PeerCard({ peer }: { peer: PeerInfo }) {
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
