---
bump: patch
---

### Unify slug and resume_key into a single field

- **Removed the `slug` field from sessions.** The `resume_key` field now
  serves as both the resumption identifier and the URL routing slug.
  The frontend falls back to `session.id.slice(0, 8)` when `resume_key`
  is empty (fresh launches before file attribution).
- **Simplified `resolveSlug` to `ensureUniqueResumeKey`.** The old
  function maintained two fields in sync; the new one only enforces
  uniqueness on `resume_key` within `(kind, peer)`.
- **Adapter SSE meta events** now write to `resume_key` directly instead
  of going through a separate `slug` field that gets copied.
