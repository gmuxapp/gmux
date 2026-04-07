### Peering reliability
Mutual peers no longer trigger exponential config request storms. The daemon now caches remote configurations on initial connection, replacing per-request HTTP calls that previously caused runaway CPU and load spikes.
### Graceful shutdown
Background services now terminate cleanly alongside the HTTP server on `/v1/shutdown` or `SIGTERM`. The main process actively awaits all goroutines, eliminating lingering `gmuxd` instances that previously doubled outbound connections during restarts.
### Configuration flexibility
Token fields in `host.toml` are now fully optional, enabling native Tailscale `WhoIs` peer routing. Config validation no longer blocks tokenless setups, and existing secret-based configurations remain fully compatible.

---

### Fixes
- prevent recursive config fetch storm, zombie daemons, and required peer tokens ([#119](https://github.com/gmuxapp/gmux/pull/119))
