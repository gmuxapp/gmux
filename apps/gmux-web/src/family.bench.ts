import { bench, describe } from 'vitest'
import { createFamilyIndex, projectFamily } from './family'
import { makeSession } from './test-helpers'

const agent = (id: string, parent?: string) => makeSession({
  id,
  cwd: '/perf',
  title: id,
  parent_session_id: parent,
  semantic_agent: true,
})

// Mirrors the user-blocking corpus: 1,000 sessions, with 500 children of one
// task-family root and 499 unrelated sessions.
const root = agent('root')
const children = Array.from({ length: 500 }, (_, i) => agent(`child-${i}`, root.id))
const sessions = [
  root,
  ...children,
  ...Array.from({ length: 499 }, (_, i) => agent(`other-${i}`)),
]

describe('family projection (1,000 sessions / 500 children)', () => {
  bench('classify a changed session snapshot', () => {
    createFamilyIndex(sessions)
  })

  const index = createFamilyIndex(sessions)
  bench('project the selected child panel', () => {
    projectFamily(children[250], index)
  })

})
