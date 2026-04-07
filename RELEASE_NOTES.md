Mutual peers no longer trigger exponential config request storms. The daemon now caches remote configurations on initial connection, replacing per-request HTTP calls that previously caused runaway CPU and load spikes. And other small fixes.

---

### Fixes
- prevent recursive config fetch storm, zombie daemons, and required peer tokens ([#119](https://github.com/gmuxapp/gmux/pull/119))
