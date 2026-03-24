---
bump: minor
---

- **Network listener for containers and VPNs.** gmuxd can now bind to a network address beyond localhost, protected by a bearer token. Set `GMUXD_LISTEN=10.0.0.5` or add `[network] listen = "10.0.0.5"` to your config file. Browsers get a login page; programmatic clients use the `Authorization: Bearer` header. Run `gmuxd auth-link` to see the token and a ready-to-scan URL for mobile devices.
