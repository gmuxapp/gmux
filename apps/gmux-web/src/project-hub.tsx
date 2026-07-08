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
// Both are formatted relative to the project's canonical folder (see
// cwd-format.ts): a descendant reads as `./sub`, the project root as
// `.`, an unrelated path as its abbreviated absolute form.
//
// Worktree grouping (the legacy `~/dev/gmux/.grove/<name>` headings)
// is intentionally dropped: activity-first sectioning is more useful
// day-to-day, and cwd-per-row preserves the disambiguation worktree
// grouping previously provided.

import type { Session, ProjectItem } from './types'
import { buildProjectTopology } from './projects'
import { sessionPath } from './routing'
import { relativeCwd, hubCwdLabel } from './cwd-format'
import { LaunchButton } from './launcher'
import {
  sessions, projects, peers, localPeerNames, partitionForProject,
  localHostLabel, folders,
} from './store'
import { HostSuffix } from './host-suffix'
import { SessionRow } from './session-row'
import { Section } from './home'

function projectRemote(p: ProjectItem | undefined): string | undefined {
  return p?.match?.find(r => r.remote)?.remote
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

  // Canonical launch dir for the project-level "+", taken from the same
  // folder the sidebar uses: launchCwd is the first match-rule path for
  // owned projects, peer_projects.launch_cwd for references. buildProjectFolders
  // emits a folder per project item, so this resolves whenever the
  // project exists; '' (default dir) otherwise.
  const canonicalCwd = folders.value.find(
    f => f.slug === projectSlug && (f.peer ?? '') === (projectPeer ?? ''),
  )?.launchCwd ?? ''

  // Adaptive cwd disambiguator: a project whose sessions all live in
  // one cwd shows that cwd as a subtitle (clean, no repetition); a
  // multi-cwd project shows cwd per row (lets the user pick the
  // right worktree). Empty cwds are filtered: they're a placeholder
  // for sessions whose cwd hasn't resolved yet, not a meaningful
  // shared root.
  const uniqueCwds = new Set(allSessions.map(s => s.cwd).filter(Boolean))
  const sharedCwd = uniqueCwds.size === 1 ? [...uniqueCwds][0] : null

  // Shared cwd shown as a subtitle: express it relative to the
  // canonical folder so a single-worktree project reads as `./sub`
  // rather than a long absolute path. Empty (== canonical) keeps the
  // absolute form, since the header only carries the slug and the
  // path is still useful context there.
  const sharedCwdLabel = sharedCwd
    ? (relativeCwd(sharedCwd, canonicalCwd) || sharedCwd)
    : null

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
      // Multi-cwd disambiguator: express each cwd relative to the
      // canonical folder ('./' for a session at the project root, blank
      // for an unresolved cwd so its segment is dropped).
      cwdLabel={hubCwdLabel(s.cwd, canonicalCwd)}
      showHost={mixedHosts}
      onClose={() => onCloseSession(s)}
    />
  )

  return (
    <div class="page">
      <header class="hub-header">
        <a class="hub-back" href="/" aria-label="Back to home">
          <span class="hub-back-arrow" aria-hidden="true">←</span>
          Home
        </a>
        <div class="hub-title-row">
          <h2 class="hub-title">
            {projectSlug}
            <HostSuffix peer={projectPeer ?? localHostLabel.value} local={!projectPeer} />
          </h2>
          <LaunchButton
            // Project-level "+": launch in the canonical dir, consistent
            // with the sidebar project-row button.
            cwd={canonicalCwd}
            peer={projectPeer}
            className="hub-header-launch"
          />
        </div>
        {(remote || sharedCwd) && (
          <div class="hub-subtitle">
            {sharedCwdLabel && <span class="hub-cwd">{sharedCwdLabel}</span>}
            {remote && sharedCwd && <span class="hub-subtitle-sep"> · </span>}
            {remote && <span class="hub-remote">{remote}</span>}
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
    <div class="hub-empty">
      <span>No sessions yet in {projectSlug}</span>
    </div>
  )
}
