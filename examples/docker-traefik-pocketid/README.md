# gmux behind Traefik with PocketID authentication

HTTPS reverse proxy with OIDC authentication. Traefik handles TLS
(Let's Encrypt), PocketID handles login, and Traefik injects the gmux
bearer token into forwarded requests so you don't have to manage it
separately.

## How it works

```
browser → Traefik (HTTPS) → PocketID (OIDC auth) → gmux (HTTP + token)
```

1. User visits `https://gmux.example.com`
2. Traefik's ForwardAuth middleware calls PocketID
3. If not logged in, PocketID redirects to its login page
4. After login, Traefik adds the `Authorization: Bearer <token>` header
   and forwards the request to gmux
5. gmux sees a valid token and serves the request

The gmux bearer token is injected by Traefik via a headers middleware.
Users never see or manage it.

## Setup

### 1. Create directories and generate secrets

```bash
mkdir -p data/{workspace,gmux-state,pocket-id,traefik}
touch data/traefik/acme.json && chmod 600 data/traefik/acme.json

# Generate the gmux auth token
openssl rand -hex 32 > data/gmux-state/auth-token
chmod 600 data/gmux-state/auth-token
```

### 2. Configure environment

```bash
cp .env.example .env
```

Edit `.env`:
- Set your `DOMAIN`, `ACME_EMAIL`, and DNS provider credentials
- Set `GMUX_TOKEN` to the contents of `data/gmux-state/auth-token`
- Leave `OIDC_CLIENT_ID` and `OIDC_CLIENT_SECRET` empty for now

### 3. Start Traefik and PocketID

```bash
docker compose up -d traefik pocket-id
```

### 4. Create an OIDC client in PocketID

1. Open `https://auth.example.com` and complete initial setup
2. Go to Settings → Admin → OIDC Clients → Add Client
3. Set the callback URL to `https://gmux.example.com/_auth/callback`
4. Copy the client ID and secret into `.env`

### 5. Start everything

```bash
docker compose up -d
```

Open `https://gmux.example.com`. You'll be redirected to PocketID
for login, then back to gmux.

## Security notes

- **HTTPS everywhere.** Traefik terminates TLS with a valid Let's
  Encrypt certificate. Traffic between Traefik and gmux stays inside
  the Docker network (never leaves the host).
- **Double auth.** PocketID controls who can reach gmux (OIDC login).
  The bearer token is a second layer that gmux enforces on every
  request. Both must pass.
- **Token is not exposed to users.** Traefik injects it via a headers
  middleware. Users authenticate through PocketID only.

## Customization

- **Different DNS provider:** change the `dnschallenge.provider` in the
  Traefik command and the corresponding env var. See
  [Traefik ACME docs](https://doc.traefik.io/traefik/https/acme/).
- **Different OIDC provider:** replace PocketID with Authelia, Authentik,
  Keycloak, or any OIDC provider. The ForwardAuth middleware works the same.
