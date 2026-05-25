/**
 * ImageViewer — in-browser image viewer for image files.
 *
 * Opened when the user clicks an image file in the file tree.
 * Serves the image via GET /v1/fs/{slug}/raw, which returns raw bytes
 * with the correct Content-Type header.
 */

import { useState } from 'preact/hooks'

export interface ImageViewerProps {
  projectSlug: string
  filePath: string
}

export function ImageViewer({ projectSlug, filePath }: ImageViewerProps) {
  const [error, setError] = useState(false)
  const fileName = filePath.split('/').pop() ?? filePath
  const src = `/v1/fs/${encodeURIComponent(projectSlug)}/raw?path=${encodeURIComponent(filePath)}`

  return (
    <div class="image-viewer-panel">
      {/* Header */}
      <div class="main-header image-viewer-header">
        <div class="main-header-left">
          <div class="main-header-title">{fileName}</div>
          <div class="main-header-meta">
            <span class="main-header-cwd">{filePath}</span>
          </div>
        </div>
      </div>

      {/* Body */}
      <div class="image-viewer-body">
        {error ? (
          <div class="state-message">
            <div class="state-icon" style={{ color: 'var(--status-error)' }}>⚠</div>
            <div class="state-title">Failed to load image</div>
            <div class="state-subtitle">{filePath}</div>
          </div>
        ) : (
          <img
            class="image-viewer-img"
            src={src}
            alt={fileName}
            onError={() => setError(true)}
          />
        )}
      </div>
    </div>
  )
}
