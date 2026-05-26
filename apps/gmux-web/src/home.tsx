// Home page: projects overview.
// Reads shared data from the store (signals).

import { health, folders } from './store'
import { HostSuffix } from './host-suffix'
import type { Folder } from './types'

export function Home() {
  const healthVal = health.value
  const foldersVal = folders.value

  return (
    <div class="home">
      {/* ── Projects ── */}
      {foldersVal.length > 0 && (
        <section class="home-projects">
          <h2 class="home-section-title">Projects</h2>
          <div class="home-project-grid">
            {foldersVal.map(f => <ProjectCard key={f.key} folder={f} />)}
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
  const href = f.peer ? `/@${f.peer}/${f.slug}` : `/${f.slug}`
  return (
    <a class="home-project-card" href={href}>
      <div class="home-project-name">
        {f.name}
        <HostSuffix peer={f.peer} />
      </div>
      <div class="home-project-count">
        {alive > 0 && <span class="home-project-alive">{alive} alive</span>}
        {alive > 0 && resumable > 0 && <span class="home-project-rest"> · </span>}
        {resumable > 0 && <span class="home-project-rest">{resumable} resumable</span>}
        {alive === 0 && resumable === 0 && <span class="home-project-rest">no sessions</span>}
      </div>
    </a>
  )
}

