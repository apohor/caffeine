# Installation

Caffeine ships as a single multi-arch Docker image (`linux/amd64` +
`linux/arm64`) published to
[GHCR](https://github.com/apohor/caffeine/pkgs/container/caffeine).
Any always-on box on the same network as your Meticulous machine will
work — NAS, Raspberry Pi 4/5, old Mac mini, home server.

- [Requirements](#requirements)
- [Docker (recommended)](#docker-recommended)
- [Docker Compose](#docker-compose)
- [Synology NAS](#synology-nas)
- [Configuration reference](#configuration-reference)
- [AI providers](#ai-providers)
- [PWA install & Web Push](#pwa-install--web-push)
- [Exposing Caffeine beyond your LAN](#exposing-caffeine-beyond-your-lan)
- [Upgrading](#upgrading)
- [Backup & restore](#backup--restore)
- [Uninstall](#uninstall)

## Requirements

- A [Meticulous](https://meticuloushome.com) espresso machine reachable
  over HTTP on your LAN.
- A host capable of running Docker (x86-64 or ARM64).
- ~60 MB of disk for the image, plus whatever your shot history grows
  to (typically < 100 MB for years of daily use).

Caffeine talks to the machine over plain HTTP on the LAN — no cloud,
no Meticulous account, no outbound dependency beyond the AI provider
you pick (optional).

## Docker (recommended)

```bash
docker run -d --name caffeine \
  -p 8080:8080 \
  -v caffeine-data:/data \
  -e MACHINE_URL=http://<your-machine-ip> \
  -e TZ=America/New_York \
  --restart unless-stopped \
  ghcr.io/apohor/caffeine:latest
```

Open <http://localhost:8080> (or `http://<host-ip>:8080` from another
device on your LAN).

**First-run checklist**

1. **Settings → AI shot analysis** — pick a provider, paste an API key.
   Your next shot gets an automatic critique.
2. **Settings → Notifications** — enable push for *shot finished* and
   *analysis ready*. On iPhone / iPad, install to the home screen first
   (Share → Add to Home Screen), then flip the toggle.
3. **Settings → Preheat schedule** — set a cron-style warm-up time so
   the machine is pull-ready when you walk in.

### Available image tags

| Tag | Source |
|---|---|
| `latest` | Most recent release tag |
| `vX.Y.Z` / `X.Y` | Git tag `vX.Y.Z` |
| `main` | Latest `main` branch |
| `main-<sha>` | Pinned to a specific commit |

Pin to a `vX.Y.Z` tag in production; the `latest` tag moves on every
release.

## Docker Compose

```yaml
services:
  caffeine:
    image: ghcr.io/apohor/caffeine:latest
    container_name: caffeine
    ports: ["8080:8080"]
    environment:
      MACHINE_URL: http://<your-machine-ip>
      TZ: America/New_York
    volumes:
      - caffeine-data:/data
    restart: unless-stopped

volumes:
  caffeine-data:
```

```bash
docker compose up -d
docker compose logs -f caffeine
```

## Synology NAS

A ready-to-paste Container Manager project lives in
[deploy/synology/docker-compose.yml](../deploy/synology/docker-compose.yml).

1. **Container Manager → Project → Create**
   - Project name: `caffeine`
   - Path: `/volume1/docker/caffeine`
   - Source: *Create docker-compose.yml* — paste the file.
2. Edit `MACHINE_URL` and `TZ` to match your environment.
3. **Build → Run**, then open `http://<nas-ip>:8080`.

Why a named volume and not a bind mount? DSM applies its own ACLs
under `/volume1/docker` that block the distroless image's nonroot
user (uid 65532) from writing `caffeine.db`. Docker-managed volumes
sidestep that — no `chown` dance required.

## Configuration reference

Everything that isn't an AI API key is set via environment variables.
AI keys are managed from the Settings page and stored in the SQLite
database on the `/data` volume.

| Variable | Default | Purpose |
|---|---|---|
| `MACHINE_URL` | `http://meticulous.local` | Base URL of the Meticulous machine. |
| `ADDR` | `:8080` | HTTP listen address. |
| `DATA_DIR` | `/data` (container) | Where SQLite lives. Mount a volume here. |
| `SYNC_INTERVAL` | `15m` | How often to reconcile with the machine's `/history`. Live capture is instant; this is a safety net. |
| `TZ` | *system* | IANA zone (embedded zoneinfo; no OS timezone package needed). Drives preheat schedules. |
| `VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY` | *auto* | Override the auto-generated Web Push keypair. Leave unset to let Caffeine manage it. |
| `VAPID_SUBJECT` | `mailto:caffeine@localhost` | The `Subscriber` claim push services see. |
| `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY` | — | *Seed* values for a fresh database only. The Settings UI is the source of truth after first boot. |

## AI providers

Analysis, coaching, and shot-to-shot comparison are optional — the
rest of the app works fine without. When enabled, Caffeine supports
three providers with the same UI:

- **OpenAI** (Chat Completions)
- **Anthropic** (Messages)
- **Google Gemini** (Generative Language)

Pick provider + model in **Settings → AI shot analysis**; the model
dropdown is populated live from the provider's own catalogue so you
don't have to guess model IDs. Provider / model / key can all be
changed at any time — the analyzer rebuilds in place, no restart.

Keys never leave the server. The browser only sees `{ has_key: true }`;
the secret itself stays in SQLite on your `/data` volume.

## PWA install & Web Push

Caffeine is a Progressive Web App: *Add to Home Screen* on iPhone /
iPad / Android / desktop Chrome gives you a full-screen install with a
proper icon, offline app-shell, and Web Push notifications.

**For push to work on iOS**, Apple requires two things:

1. iOS 16.4 or later.
2. Caffeine must be installed to the home screen **and** reached over
   HTTPS (localhost is exempt on desktop but not on iOS).

Enable per-device under **Settings → Notifications**. Each browser /
phone subscribes independently and can opt individual notification
kinds in or out. The VAPID keypair is generated on first run and
persisted in SQLite — no `openssl` dance.

## Exposing Caffeine beyond your LAN

For iOS push and installable PWA to work end-to-end, Caffeine must be
reachable over HTTPS. Easiest setups:

- **Synology reverse proxy** with Let's Encrypt (Control Panel →
  Login Portal → Advanced → Reverse Proxy).
- **Tailscale Funnel** or **Cloudflare Tunnel** — HTTPS without
  opening ports on your router.
- **Caddy / nginx / Traefik** in front of the container on a VPS.

> **There is no built-in auth.** If you expose Caffeine publicly, put
> it behind a reverse-proxy auth layer (Authelia, Cloudflare Access,
> basic auth, …) until native auth lands.

## Upgrading

```bash
# Compose
docker compose pull && docker compose up -d

# Bare docker
docker pull ghcr.io/apohor/caffeine:latest
docker rm -f caffeine
# re-run the same `docker run …` command
```

Your `/data` volume persists across restarts and upgrades. Schema
migrations run automatically on boot.

## Backup & restore

Caffeine keeps everything in the `/data` volume:

- `caffeine.db` — SQLite database (shots, settings, AI keys,
  push subs, preheat schedule, cached AI output).
- `profile-images/` — cached profile renderings from the machine.

**Backup** (while the container is stopped, to avoid a half-written
WAL):

```bash
docker compose stop caffeine
# named volume:
docker run --rm -v caffeine_caffeine-data:/src -v $PWD:/dst alpine \
  tar -C /src -czf /dst/caffeine-backup.tgz .
docker compose start caffeine
```

On Synology: the named volume lives at
`/volume1/@docker/volumes/caffeine_caffeine-data/_data/` — back it up
from DSM File Station while the project is stopped.

**Restore:** shut the container down, replace the volume contents,
start it again. No migration needed.

## Uninstall

```bash
docker compose down -v       # removes the container AND the data volume
# or, bare docker:
docker rm -f caffeine
docker volume rm caffeine-data
```
