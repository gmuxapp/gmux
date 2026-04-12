// Home page: host status, project overview, quick-launch per host.
// Reads shared data from the store (signals).

import { useState } from 'preact/hooks'
import { health, peers, folders, sessions, launchers as launchersSignal, defaultLauncher as defaultLauncherSignal, launchSession } from './store'
import { PeerLabel } from './peer-label'
import type { Folder, LauncherDef } from './types'
import { launchersForPeer } from './launcher'

/** Strip protocol and trailing slash for display: "https://foo.bar/" → "foo.bar" */
function displayHost(url: string): string {
  return url.replace(/^https?:\/\//, '').replace(/\/+$/, '')
}

export function Home() {
  const healthVal = health.value
  const peersVal = peers.value
  const foldersVal = folders.value
  const sessionsVal = sessions.value
  const localAlive = sessionsVal.filter(s => !s.peer && s.alive).length
  const hostname = healthVal?.hostname ?? 'local'
  const tsUrl = healthVal?.tailscale_url

  const localLaunchers = launchersSignal.value
  const localDefault = defaultLauncherSignal.value
  const peerLaunchers = (peer: string) =>
    launchersForPeer(localLaunchers, localDefault, peersVal, peer).launchers

  return (
    <div class="home">
      {/* ── Hosts ── */}
      <section class="home-hosts">
        <h2 class="home-section-title">Hosts</h2>
        <div class="home-host-grid">
          <HostCard
            name={hostname}
            status="connected"
            url={tsUrl}
            details={[
              healthVal?.version
                ? healthVal.update_available
                  ? `v${healthVal.version} \u2192 v${healthVal.update_available}`
                  : `v${healthVal.version}`
                : undefined,
              `${localAlive} active session${localAlive === 1 ? '' : 's'}`,
            ]}
            launchers={localLaunchers}
          />
          {peersVal.map(p => (
            <HostCard
              key={p.name}
              name={p.name}
              status={p.status}
              url={p.url}
              details={[
                p.version ? `v${p.version}` : undefined,
                p.status === 'connected'
                  ? `${p.session_count} active session${p.session_count === 1 ? '' : 's'}`
                  : p.status === 'offline'
                    ? 'offline'
                    : p.last_error ?? 'disconnected',
              ]}
              launchers={p.status === 'connected' ? peerLaunchers(p.name) : []}
              peer={p.name}
            />
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

      <footer class="home-footer">
        <span class="home-footer-version">Frontend v{__GMUX_VERSION__}</span>
        {healthVal?.version && healthVal.version !== __GMUX_VERSION__ && (
          <button class="home-footer-reload" onClick={() => location.reload()}>
            reload to update
          </button>
        )}
        {healthVal?.update_available && (
          <a
            class="home-footer-update"
            href="https://gmux.app/changelog/"
            target="_blank"
          >
            v{healthVal.update_available} available
          </a>
        )}
      </footer>
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

function HostCard({
  name, status, url, details, launchers, peer,
}: {
  name: string
  status: string
  url?: string
  details: (string | undefined)[]
  launchers: LauncherDef[]
  peer?: string
}) {
  const [launching, setLaunching] = useState<string | null>(null)
  const linked = status === 'connected' && url

  const handleLaunch = (id: string) => {
    setLaunching(id)
    launchSession(id, peer ? { peer } : undefined).finally(() => setLaunching(null))
  }

  return (
    <div class="home-host-card">
      <div class="home-host-top">
        <span class={`home-host-status ${status}`} />
        {peer && <PeerLabel name={name} />}
        <span class="home-host-name">{name}</span>
      </div>
      <div class="home-host-details">
        {url && (
          linked
            ? <a class="home-host-detail home-host-link" href={url} target="_blank" rel="noopener">{displayHost(url)}</a>
            : <div class="home-host-detail">{displayHost(url)}</div>
        )}
        {details.filter(Boolean).map((d, i) => (
          <div key={i} class="home-host-detail">{d}</div>
        ))}
      </div>
      {launchers.length > 0 && (
        <div class="home-host-launchers">
          {launchers.map(l => (
            <button
              key={l.id}
              class={`home-launch-btn${launching === l.id ? ' launching' : ''}${!l.available ? ' unavailable' : ''}`}
              onClick={() => handleLaunch(l.id)}
              disabled={launching !== null || !l.available}
            >
              {l.label}
            </button>
          ))}
        </div>
      )}
    </div>
  )
}
