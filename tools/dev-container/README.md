# gmux 2.0 dev-test container

Runs the **locally-built** gmux 2.0 (central-store / ADR 0026) daemon in a
container so we can exercise the real UI, SQLite state, and restart/crash
recovery without touching the host gmux daemon.

Fully isolated from the host: own state volume, own token, mapped to host
loopback **8795** (the host daemon owns 8790). It does not auto-update from
GitHub releases — it runs exactly the binaries you build.

## Build & run

From the repo root:

```bash
./scripts/build.sh                                              # bin/gmuxd + bin/gmux (embeds UI)
docker compose -f tools/dev-container/compose.yaml up -d --build
```

Open the dashboard: <http://127.0.0.1:8795/>

Token (dev-only, in `compose.yaml`):

```
d04b098ce128dcd49c189816a907a01a53b1b2fcb19552a79a2577d97fd7a56d
```

## Iterate after a code change

```bash
./scripts/build.sh && docker compose -f tools/dev-container/compose.yaml up -d --build
```

## pi agent + auth

The image bakes in Node and `pi` (pinned to match the host) so the pi adapter
is discovered and shows in the UI's `+` menu. pi's credentials are NOT baked
in; seed them once from the host into the persistent `/root/.pi` volume:

```bash
docker exec gmux-2x mkdir -p /root/.pi/agent
docker cp ~/.pi/agent/auth.json          gmux-2x:/root/.pi/agent/auth.json
docker cp ~/.pi/agent/auth-profiles.json gmux-2x:/root/.pi/agent/auth-profiles.json
docker cp ~/.pi/agent/settings.json      gmux-2x:/root/.pi/agent/settings.json
```

The volume persists across restarts/recreates, so this is a one-time step.

## Remote access (Tailscale)

`host.toml` lives on the persistent `gmux2x-config` volume with
`[tailscale] enabled = true`, and the device registers as `gmux-2x`
(`GMUXD_TS_HOSTNAME`). On first enable, approve the login URL printed in
`docker logs gmux-2x` (it expires — restart the daemon to refresh it), then:

```bash
docker exec gmux-2x gmux remote   # shows the https://gmux-2x.<tailnet>.ts.net URL
```

tsnet registration state lives on the `gmux2x-state` volume, so it survives
restarts — no need to re-approve.

## Poke around inside

```bash
docker exec -it gmux-2x bash
# inside: create a session, inspect state
gmux ls
gmux daemon state check
ls -la /root/.local/state/gmux
```

## Restart / crash-recovery testing

State is on the `gmux2x-state` volume, so it survives restarts — this is how
we reproduce the original unread-on-restart scenario against the real daemon:

```bash
docker compose -f tools/dev-container/compose.yaml restart   # clean restart
docker kill gmux-2x && docker compose -f tools/dev-container/compose.yaml up -d   # hard crash
```

## Tear down

```bash
docker compose -f tools/dev-container/compose.yaml down          # keep state volumes
docker compose -f tools/dev-container/compose.yaml down -v       # wipe state (fresh 2.0)
```
