- **Smarter scrollback storage.** Loading spinners and screen clears no longer
  waste scrollback buffer space. Spinner frames that overwrite each other via
  carriage return are collapsed to just the final frame, and screen clears
  (`ESC[2J` / `ESC[3J`) discard pre-clear content from the buffer.

- **Fixed mobile keyboard word replacement.** iOS autocorrect and Android
  predictive text would concatenate the corrected word after the original
  instead of replacing it. Replacement events are now intercepted and
  translated into the correct terminal backspace + retype sequence.

- **Fixed scroll position drifting when scrollback is full.** When the user
  was scrolled up and new output arrived in synchronized update blocks,
  the viewport would gradually drift as old lines were evicted from the
  scrollback buffer. The scroll position now correctly tracks content
  through scrollback eviction.

- **Workspace-aware sidebar grouping.** Sessions in jj workspaces or git
  worktrees that share the same repository are now grouped under a single
  folder in the sidebar. For example, sessions running in
  `~/dev/gmux/.grove/teak` and `~/dev/gmux` collapse into one "gmux" group
  instead of appearing as separate folders.
