<!--
Optional prose for the next release. Edit this file to add a high-level
summary, topic paragraphs, or migration notes. Its content is injected
into the changelog section between the version heading and the grouped
bullet lists.

This file is the single source of truth for release prose: edit it on
`main` before merging the PR that ships the noteworthy change, or on
the `release/next` branch up until the release ships (then run
`scripts/regen-release-notes.sh` to refresh the generated files).

The release workflow clears this file automatically after a successful
release. Leave empty for releases that don't need prose; the
auto-generated bullet list is enough for patch-only releases.

Format: plain markdown. Supports headings, paragraphs, and lists.
Example:

    Peers now automatically reconnect after system suspend, no restart
    needed.

    ### Project matching
    `projects.json` replaces separate `remote` and `paths` fields with a
    unified `match` array.
-->
