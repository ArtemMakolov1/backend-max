# MAX Studio backend

Go API for channel discovery, web-grounded post research, post drafts,
scheduling, GPT Image 2 generation, local media storage and
publishing/editing/deleting MAX channel messages.

This directory is a standalone repository. It does not import or build the
Astro/React frontend. A browser client may call the API directly during local
development, while production should normally expose the API and media through
an external HTTPS reverse proxy under the frontend's public origin.

## Run locally

Requirements: Go 1.25 or newer.

```sh
cp .env.example .env
# Add Yandex OAuth credentials or generate ADMIN_API_KEY with:
# openssl rand -hex 32
make dev
```

The API is served at `http://localhost:8080/api/v1`; immutable media is served
under `/media/{filename}`. SQLite data and media directories are created on
startup.

Secrets are read only from environment variables. Never put `MAX_BOT_TOKEN` or
`OPENAI_API_KEY` into the frontend, SQLite database, logs, or source control.
Any token pasted into a chat or issue must be rotated before use.

Useful repository-local commands:

```sh
make test       # unit tests
make test-race  # tests with the race detector
make vet        # Go static checks
make build      # bin/maxpilot
make docker-build
```

## Docker Compose

The repository includes a backend-only `compose.yaml`:

```sh
cp .env.example .env
# Fill ADMIN_API_KEY or the complete Yandex OAuth configuration in .env.
docker compose up --build
```

By default it publishes the API on `http://localhost:8080`. SQLite data and
media are stored in named volumes. A local certificate bundle can be mounted
from `certs/`; real PEM files are ignored by Git and must be provisioned by the
deployment environment.

The Compose service intentionally keeps `OAUTH_TRUST_X_REAL_IP=false`. Set it
to `true` only when a trusted reverse proxy overwrites `X-Real-IP` and direct
external access to the backend port is blocked.

## Frontend and reverse proxy

For local development, run the frontend separately on
`http://localhost:4321` and keep:

```dotenv
PUBLIC_BASE_URL=http://localhost:8080
FRONTEND_ORIGIN=http://localhost:4321
```

For production, the recommended topology is one public HTTPS origin: an
external ingress serves the frontend and proxies `/api/v1` and `/media` to this
service. Set `PUBLIC_BASE_URL` and `FRONTEND_ORIGIN` to that exact public origin,
and register the same origin plus `/api/v1/auth/yandex/callback` as the Yandex
Redirect URI. The frontend and backend may live in separate repositories and
containers; they only share the documented HTTP contract.

If two independent Compose projects are used, they do not share a network by
default. Attach their services to an explicit external Docker network or point
the reverse proxy at a published backend hostname. Do not rely on the service
name `backend` resolving across separate default networks.

## Yandex ID authentication

The management API accepts either a server-side Yandex session or the optional
`X-Admin-Key` break-glass credential. Configure Yandex OAuth with:

```dotenv
YANDEX_CLIENT_ID=...
YANDEX_CLIENT_SECRET=...
YANDEX_REDIRECT_URI=http://localhost:8080/api/v1/auth/yandex/callback
YANDEX_ALLOWED_USERS=123456789,editor@example.ru
AUTH_SESSION_TTL=12h
```

All three OAuth values and a non-empty comma-separated allowlist are required
together. Allowed entries are exact, case-insensitive Yandex IDs, app-scoped
PSUIDs, logins, or email addresses. Outside localhost, the Redirect URI must use
HTTPS and must exactly match the URI registered in the Yandex web application.

The Go server uses Authorization Code Flow with random `state` and PKCE S256.
OAuth states are one-time values with a ten-minute lifetime. The provider token
is used only to request `https://login.yandex.ru/info` and is then discarded.
The browser receives an opaque `HttpOnly`, `SameSite=Lax` session cookie; only
its SHA-256 digest and the minimal user profile are stored in SQLite. Logout
deletes the server-side session. `ADMIN_API_KEY` remains available for CLI and
emergency access and is accepted in parallel with a valid session.

The exact allowlist identity that admitted the account is rechecked for every
session request, so removing it and restarting the service revokes the session
immediately. OAuth starts are limited per verified client IP, with a much higher
process-wide emergency ceiling. The external TLS edge must disable access and
error logging for the exact callback path (or log `$uri` without `$args`) and
preserve `Cache-Control: no-store` plus `Referrer-Policy: no-referrer`. Set
`OAUTH_TRUST_X_REAL_IP=true` only when that trusted proxy overwrites the header
and direct access to the Go port is blocked. Set
`OAUTH_RATE_LIMIT_AT_EDGE=true` only when the edge enforces the verified
client-IP limit itself. An inner proxy cannot erase OAuth query parameters that
an outer proxy has already logged.

The server fails closed when neither authentication method is configured. For
an intentionally unauthenticated loopback-only development server, opt in with
`ALLOW_INSECURE_NO_AUTH=true`; the option is rejected on a non-loopback bind.

Public auth routes:

```text
GET  /api/v1/auth/yandex/start
GET  /api/v1/auth/yandex/callback
GET  /api/v1/auth/session
POST /api/v1/auth/logout
```

For cross-origin local development, requests use credential cookies and the API
allows them only for the exact `FRONTEND_ORIGIN`. Cookie-authenticated mutating
requests with a missing or different Origin are rejected.

## Publication calendar

`scheduled_at` is an RFC3339 timestamp with an explicit offset. The API converts
it to UTC before storing and returning it. A post can enter the calendar in any
of these ways:

- include `scheduled_at` in `POST /api/v1/posts` to create a scheduled post;
- call `POST /api/v1/posts/{id}/schedule` to schedule or postpone a draft,
  failed, or already scheduled post;
- send a future `scheduled_at` through `PATCH /api/v1/posts/{id}`; send `null`
  to cancel it;
- call `POST /api/v1/posts/{id}/cancel-schedule` to cancel explicitly.

Calendar operations require a future timestamp, an active channel, and valid
MAX-ready content. Scheduling itself is local and makes no MAX request. The
worker rechecks channel permissions only when the time arrives. It atomically
claims a post only while its status is still `scheduled` and its timestamp is
due, so a cancellation or postponement that wins the database race cannot be
published from an older worker snapshot. Scheduled list queries are returned in
publication-time order.

## AI research and post drafting

Set `OPENAI_API_KEY` on the server and optionally override
`OPENAI_RESEARCH_MODEL` (the default is `gpt-5.4-mini`). The protected endpoint
`POST /api/v1/research/generate` first calls the Responses API with the required
`web_search` tool and high search context, then uses Structured Outputs to turn
the cited report into a MAX-ready post. The OpenAI key is never sent to the
browser.

Example request:

```json
{
  "topic": "Как малому бизнесу использовать ИИ в 2026 году",
  "angle": "Практические сценарии без большой команды",
  "audience": "Владельцы малого бизнеса",
  "tone": "Деловой и понятный",
  "format": "markdown",
  "include_sources": true
}
```

The response contains `report`, a `sources` array of cited HTTPS pages, and a
strict `draft` object with `title`, `content`, `format`, and `image_prompt`.
Citation links in `report` are rendered as visible Markdown links. Source cards
are always returned; `include_sources` only controls whether the generated post
itself ends with a compact source list. Draft content is rejected if it exceeds
MAX's 4000 Unicode-character limit, so markup is never cut mid-structure.

The endpoint is covered by the same Yandex session or `X-Admin-Key` protection as
other management routes. Research has an overall three-minute deadline, accepts only `markdown`
or `html`, and does not publish anything to MAX.

MAX now uses `https://platform-api2.max.ru`. If its certificate chain is not in
the operating system trust store, set `MAX_CA_CERT_FILE` to a PEM bundle with
the required MinTsifry CA certificate. The bundle is appended to system roots;
TLS hostname and chain verification are never disabled.

For production channel discovery, configure an HTTPS MAX subscription pointing
to `POST /api/v1/webhooks/max` and use the same random value in the subscription
`secret` and `MAX_WEBHOOK_SECRET`. The endpoint validates
`X-Max-Bot-Api-Secret`.
