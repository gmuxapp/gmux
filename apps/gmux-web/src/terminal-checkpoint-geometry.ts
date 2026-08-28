export interface TerminalGeometry {
  cols: number
  rows: number
}

interface CheckpointGeometryMetadata {
  cols?: unknown
  rows?: unknown
}

/** Resolve the geometry at which a checkpoint frame was rendered. */
export function resolveCheckpointGeometry(
  metadata: CheckpointGeometryMetadata | null,
  sessionCols?: number | null,
  cachedCols?: number | null,
): TerminalGeometry | null {
  if (!metadata || !Number.isInteger(metadata.rows) || (metadata.rows as number) <= 0) return null
  const declaredCols = Number.isInteger(metadata.cols) && (metadata.cols as number) > 0
    ? metadata.cols as number
    : null
  const cols = declaredCols ?? sessionCols ?? cachedCols ?? 80
  if (!Number.isInteger(cols) || cols <= 0) return null
  return { cols, rows: metadata.rows as number }
}
