The daemon no longer rescans every adapter session file every 30
seconds. Conversation discovery is now driven directly by filesystem
events, so an idle gmuxd does no periodic work. Heavy users with
many large pi/claude/codex sessions will see CPU drop from steady
background burn to effectively zero when nothing is happening.

<!-- highlights-end -->

### Features
- **(daemon)** drop periodic conversations index scan in favor of watcher ([#179](https://github.com/gmuxapp/gmux/pull/179))
- **(cli)** print session id for --no-attach with deterministic handshake ([#184](https://github.com/gmuxapp/gmux/pull/184))

### Fixes
- **(cli)** default TERM to xterm-256color and report real version to children ([#180](https://github.com/gmuxapp/gmux/pull/180))
- **(web)** bake real version into frontend during release builds ([#182](https://github.com/gmuxapp/gmux/pull/182))
