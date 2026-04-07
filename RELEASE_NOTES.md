### Sleep recovery
Peers automatically reconnect and resync after system suspend, clearing stale connections without daemon restarts.
### Project matching
`projects.json` replaces separate `remote` and `paths` fields with a unified `match` array. Supports exact matching, per-host scoping, and automatic `~` canonicalization. Existing configs migrate on load.
### Remote setup and CLI
The `gmuxd remote` command now uses an interactive flow that correctly handles Tailscale hostname changes and config writes. Added `gmuxd log-path` for log streaming and improved daemon restart messaging.

---

### Features
- unified match rules with project onboarding and modal redesign ([#110](https://github.com/gmuxapp/gmux/pull/110))

### Fixes
- OG meta tags, mock terminal rendering, remove stale hero.png ([#101](https://github.com/gmuxapp/gmux/pull/101))
- clickable discovered project rows, document projects.json ([#103](https://github.com/gmuxapp/gmux/pull/103))
- reconnect peers after system sleep ([#104](https://github.com/gmuxapp/gmux/pull/104))
- improve Tailscale remote access setup and CLI output ([#111](https://github.com/gmuxapp/gmux/pull/111))

### Docs
- simplify landing page CTA, add Discord to getting started ([#112](https://github.com/gmuxapp/gmux/pull/112))
