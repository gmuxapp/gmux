import { signal } from '@preact/signals'

/** The family whose drawer is open, by root id — null when none is.
 *
 * One fact with one owner: whoever wants the drawer (header trigger,
 * sidebar indicator) writes the root here, and the header shows the
 * drawer while the selected session belongs to that family. The
 * comparison *is* the old "wait until navigation lands" logic — a
 * sidebar press on another family writes the root and navigates, and
 * the drawer appears exactly when the header catches up, with no
 * request to deliver, consume, or re-arm. */
export const familyDrawerRoot = signal<string | null>(null)
