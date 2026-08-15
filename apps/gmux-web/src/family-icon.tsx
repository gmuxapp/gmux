/** The family mark: several nodes, one structure. Shared by every
 * surface that summarizes a family — the header's trigger and the
 * sidebar's activity line — so "this is about the family beneath"
 * is always said with the same picture. */
export function FamilyIcon({ class: cls }: { class?: string }) {
  return (
    <svg class={cls} viewBox="0 0 16 16" aria-hidden="true">
      <path d="M8 5.5v2M8 7.5c0 1.5-3.5 1-3.5 3M8 7.5c0 1.5 3.5 1 3.5 3" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round" />
      <circle cx="8" cy="3.75" r="1.9" fill="none" stroke="currentColor" stroke-width="1.3" />
      <circle cx="4.5" cy="12" r="1.9" fill="none" stroke="currentColor" stroke-width="1.3" />
      <circle cx="11.5" cy="12" r="1.9" fill="none" stroke="currentColor" stroke-width="1.3" />
    </svg>
  )
}
