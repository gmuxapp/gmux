// Project hub: activity-first view scoped to one project.
//
// Mirrors the home dashboard's three-section layout (Waiting /
// Active / All sessions) but filtered to this project's sessions. The
// shared <SessionRow> renders without a project label (it's in the
// header) and without a host suffix unless the project spans multiple
// hosts (devcontainer case).
//
// Adaptive cwd:
//   - If every session shares a single cwd, render it as a subtitle
//     under the project title and omit it from rows.
//   - Otherwise show each session's cwd on its row, since worktree
//     identity is the most useful disambiguator in multi-cwd projects.
//
// Worktree grouping (the legacy `~/dev/gmux/.grove/<name>` headings)
// is intentionally dropped: activity-first sectioning is more useful
// day-to-day, and cwd-per-row preserves the disambiguation worktree
// grouping previously provided.

import type { Session, ProjectItem } from './types'
import { buildProjectTopology } from './projects'
import { sessionPath } from './routing'
import { LaunchButton } from './launcher'
import {
  sessions, projects, peers, localPeerNames, partitionForProject,
  localHostLabel,
} from './store'
import { HostSuffix } from './host-suffix'
import { SessionRow } from './session-row'
import { Section } from './home'

function projectRemote(p: ProjectItem | undefined): string | undefined {
  return p?.match?.find(r => r.remote)?.remote
}

function projectFirstPath(p: ProjectItem | undefined): string | undefined {
  return p?.match?.find(r => r.path)?.path
}

interface ProjectHubProps {
  projectSlug: string
  projectPeer?: string
  onCloseSession: (session: Session) => void
}

export function ProjectHub({ projectSlug, projectPeer, onCloseSession }: ProjectHubProps) {
  const projectsVal = projects.value
  const project = projectsVal.find(p =>
    p.slug === projectSlug && (p.peer ?? '') === (projectPeer ?? ''),
  )

  // Topology is still the right input shape: it correctly resolves
  // devcontainer (Local-peer) sessions onto their parent's project.
  // We flatten its output rather than render it as a tree; the
  // three-section pattern handles the navigation, not topology.
  const hosts = buildProjectTopology(
    projectSlug, sessions.value, projectsVal, peers.value,
    projectPeer,
    (name) => localPeerNames.value.has(name),
  )
  const allSessions = hosts.flatMap(h => h.folders.flatMap(f => f.sessions))
  const remote = projectRemote(project)

  // Adaptive cwd disambiguator: a project whose sessions all live in
  // one cwd shows that cwd as a subtitle (clean, no repetition); a
  // multi-cwd project shows cwd per row (lets the user pick the
  // right worktree). Empty cwds are filtered: they're a placeholder
  // for sessions whose cwd hasn't resolved yet, not a meaningful
  // shared root.
  const uniqueCwds = new Set(allSessions.map(s => s.cwd).filter(Boolean))
  const sharedCwd = uniqueCwds.size === 1 ? [...uniqueCwds][0] : null

  // Multi-host iff sessions disagree on .peer. Local-peer
  // (devcontainer) sessions in a local project's folder are the
  // common case: they make the folder mixed and earn a host suffix
  // on their row.
  const uniqueHosts = new Set(allSessions.map(s => s.peer ?? ''))
  const mixedHosts = uniqueHosts.size > 1

  const { needsAttention, running, rest } = partitionForProject(allSessions)

  const renderRow = (s: Session) => (
    <SessionRow
      key={s.id}
      session={s}
      href={sessionPath(projectSlug, s, projectPeer)}
      showCwd={!sharedCwd}
      cwdLabel={s.cwd}
      showHost={mixedHosts}
      onClose={() => onCloseSession(s)}
    />
  )

  return (
    <div class="page">
      <header class="mb-5">
        <a class="inline-flex items-center gap-1 text-[11px] font-semibold uppercase tracking-[0.06em] text-text-muted no-underline mb-1 transition-colors duration-120 hover:text-text focus-visible:text-text" href="/" aria-label="Back to home">
          <span class="text-[13px] leading-none" aria-hidden="true">←</span>
          Home
        </a>
        <div class="flex items-baseline justify-between gap-3">
          <h2 class="text-lg font-semibold text-text m-0">
            {projectSlug}
            <HostSuffix peer={projectPeer ?? localHostLabel.value} local={!projectPeer} />
          </h2>
          <LaunchButton
            sessions={allSessions}
            fallbackCwd={sharedCwd ?? projectFirstPath(project) ?? ''}
            peer={projectPeer}
            className="shrink-0"
          />
        </div>
        {(remote || sharedCwd) && (
          <div class="text-[12px] text-text-muted mt-1">
            {sharedCwd && <span class="font-[JetBrains_Mono,_SF_Mono,_monospace] text-[11px]">{sharedCwd}</span>}
            {remote && sharedCwd && <span class="opacity-60"> · </span>}
            {remote && <span class="font-[JetBrains_Mono,_SF_Mono,_monospace] text-[11px]">{remote}</span>}
          </div>
        )}
      </header>

      {allSessions.length === 0 ? (
        <EmptyProject projectSlug={projectSlug} />
      ) : (
        <>
          {needsAttention.length > 0 && (
            <Section title="Waiting">
              {needsAttention.map(renderRow)}
            </Section>
          )}
          {running.length > 0 && (
            <Section title="Active">
              {running.map(renderRow)}
            </Section>
          )}
          {rest.length > 0 && (
            <Section title="All sessions">
              {rest.map(renderRow)}
            </Section>
          )}
        </>
      )}
    </div>
  )
}

// Empty-state placeholder for projects with no sessions. The launch
// affordance is already in the page header, so this block only
// carries the descriptor text: a second launch button here would
// duplicate the one above it.
function EmptyProject({ projectSlug }: { projectSlug: string }) {
  return (
    <div class="hub-empty flex items-center gap-3 py-3.5 px-4 border border-border rounded-lg text-text-muted text-[13px] mt-4">
      <span>No sessions yet in {projectSlug}</span>
    </div>
  )
}
