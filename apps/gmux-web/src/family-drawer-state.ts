import { signal } from '@preact/signals'

/** A sidebar family button can ask the header-owned drawer to open.
 * Carry the root rather than a boolean: navigation and rendering are
 * asynchronous, so the request waits until the header belongs to the
 * family that was actually pressed instead of flashing the old one. */
export const familyDrawerRequest = signal<string | null>(null)

export function requestFamilyDrawer(rootId: string) {
  // Clear first so pressing the same already-open family after closing
  // it is still a new signal update.
  familyDrawerRequest.value = null
  familyDrawerRequest.value = rootId
}
