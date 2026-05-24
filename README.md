# Replay

Open-source, agent-first, event-driven Playwright test execution and triage platform. Run end-to-end tests, watch results stream in real-time, and let an LLM agent triage failures with full access to your scripts, artifacts, and repo.

Replay is built on **pgmqtt** (PostgreSQL with an embedded MQTT broker) — every run, result, and live update flows through the broker's change-data-capture stream.

## 5-Minute Quickstart

```bash
git clone https://github.com/RayElg/replay.git
cd replay
cp .env.example .env
make dev
```

Open [http://localhost:3000](http://localhost:3000). On first boot the UI redirects to **/setup** where you create the first admin user. After that, log in at **/login**.

Set `ANTHROPIC_API_KEY` in `.env` before `make dev` to enable agent chat. Without it the UI still works; the agent panel returns 503.

### Try it

Replay doesn't ship with a bundled test target — you point it at your own app and
import or paste your own Playwright specs. To see the whole pipeline end-to-end
without writing anything first, seed a self-contained example that runs against
`example.com`:

```bash
make example
```

This creates an **"Example (example.com)"** environment and an **"Example: example.com"**
script (one test that passes, one that fails on purpose) in the Default Project, then
queues a run. Watch it stream in at [http://localhost:3000](http://localhost:3000): one
test goes green, one goes red, and — if `ANTHROPIC_API_KEY` is set — the agent triages
the failure and posts a verdict. Re-run `make example` any time; it's idempotent.

(`make example` needs `make dev` running first so the runner is up to execute the run.)

## Architecture

```
┌─────────┐     ┌──────────────┐     ┌────────┐
│   UI    │────▶│ Control Plane│────▶│ Runner │
│ Next.js │◀────│   (Go/chi)   │◀────│  (Go)  │
└─────────┘     └──────┬───────┘     └───┬────┘
                       │                  │
                ┌──────┴───────┐   ┌─────┴─────┐
                │   pgmqtt     │   │   MinIO    │
                │  PG + MQTT   │   │ (S3-compat)│
                └──────────────┘   └───────────┘
```

- **pgmqtt** — PostgreSQL with an embedded MQTT broker. Row changes publish as MQTT messages via CDC topic mappings; the UI and runners subscribe.
- **Control plane** — Go API server. Manages runs, scripts, environments, integrations, auth, audit, and the agent.
- **Runner** — Pulls test jobs via MQTT shared subscriptions, executes Playwright, uploads artifacts.
- **UI** — Next.js frontend with real-time updates via MQTT-over-WebSocket.
- **MinIO** — S3-compatible object store for artifacts (videos, traces, screenshots).

## Auth

Replay uses password-based auth for browser sessions. On first boot the UI
redirects to `/setup` to create the first admin user (or set
`REPLAY_BOOTSTRAP_USER_EMAIL`+`REPLAY_BOOTSTRAP_USER_PASSWORD` to seed one
non-interactively).

API keys (`rplx_...`) work for non-browser callers (runners, CI webhooks).
Mint one with `control-plane bootstrap-key`.

## CLI subcommands

```bash
control-plane bootstrap-key                 # print a new API key once
control-plane create-user --email a@b.com   # create or update a user (interactive password)
control-plane reset-password --email a@b.com  # set a new password directly
control-plane invite --email a@b.com        # mint an invite URL: $REPLAY_EXTERNAL_URL/invite/{token}
control-plane reset-link --email a@b.com    # mint a reset URL: $REPLAY_EXTERNAL_URL/reset/{token}
control-plane gen-broker-jwt-key            # mint an Ed25519 key for pgmqtt Enterprise trusted-broker mode
control-plane seed-example [--run]          # seed the example.com demo env+script (and optionally queue a run)
control-plane rewrap-secrets [--dry-run]    # re-encrypt stored secrets under the current REPLAY_ENCRYPT_KEY
control-plane --migrate-only                # run migrations then exit
```

### Rotating `REPLAY_ENCRYPT_KEY`

At-rest secrets (environment variable values, integration tokens) are encrypted
with AES-256-GCM under `REPLAY_ENCRYPT_KEY`. Every stored value records an
8-character fingerprint of the key that sealed it, so rotation is non-destructive:

1. Generate a new key: `openssl rand -hex 32`.
2. On the **control plane and every runner**, set `REPLAY_ENCRYPT_KEY` to the new
   key and `REPLAY_ENCRYPT_KEY_PREVIOUS` to the old one (comma- or
   whitespace-separated; list more than one if you're catching up across several
   rotations), then restart. New writes use the new key; reads of values still
   sealed under the old key keep working through the previous-key slot.
3. Re-encrypt everything under the new key:
   `docker compose run --rm control-plane control-plane rewrap-secrets`
   (run with `--dry-run` first to see the counts). It refuses and exits non-zero
   if any value can't be decrypted — that means a key is still missing from
   `REPLAY_ENCRYPT_KEY_PREVIOUS`.
4. Once `rewrap-secrets` reports everything is sealed under the primary key, drop
   `REPLAY_ENCRYPT_KEY_PREVIOUS` and restart.

If `REPLAY_ENCRYPT_KEY` is unset, secret values are stored as **plaintext** at
rest; the environment editor shows a warning when that's the case.

`invite` and `reset-link` are the self-hosted equivalent of the "send invite"
and "forgot password" buttons most SaaS apps ship. Replay doesn't bundle an
email backend — the operator runs the command, copies the URL it prints, and
delivers it to the user out-of-band (Slack, email, in person). The URL itself
is good for 7 days (invites) or 1 hour (resets); the invitee/user sets their
own password when they open it.

`REPLAY_EXTERNAL_URL` must be set for `invite`/`reset-link` so the printed
URL points at the right host.

## GitHub integration

Replay can pull scripts directly from a GitHub repo so the repo stays the
canonical home for test code while Replay handles execution, triage, and
history.

**Auth options.** Both flavours store the credential in
`integrations.encrypted_token` (encrypted under `REPLAY_ENCRYPT_KEY`) and use
the same per-request flow:

| Mode | `config.auth_kind` | Stored credential | When to use |
|---|---|---|---|
| Personal access token | `pat` (default) | The token itself | Self-hosted single-tenant. Fast to set up. Coarser scopes — the token has the user's permissions across everything they can see. |
| GitHub App installation | `app` | PEM-encoded RSA private key | Multi-tenant deployments. Fine-grained, per-install permissions. Installation tokens auto-rotate every hour. |

For App mode, also set `config.app_id` and `config.installation_id` on the
integration row. The control-plane mints a 10-minute signed JWT, exchanges it
for a 1-hour installation token via `POST /app/installations/{id}/access_tokens`,
caches the result, and refreshes when ≤5 min remain.

**Importing scripts.** Open Settings → Scripts → **Import**, pick the GitHub
integration, browse the repo tree, and tick the `.spec.ts` files you want. Each
import creates a `scripts` row with the file's contents snapshotted plus the
`source_*` linkage. The runner still executes scripts out of the DB; the link
is what lets you re-sync without re-importing.

**Resync.** Each linked script gets a **Sync from repo** button on the editor
panel and exposes `POST /api/scripts/{id}/sync` + `GET /api/scripts/{id}/sync-status`
for programmatic callers.

**Auto-sync on push.** Replay can resync linked scripts automatically when the
repo changes. Add a GitHub webhook pointing at
`{REPLAY_EXTERNAL_URL}/api/webhooks/github` (content type `application/json`,
event **push**) and set a secret. Tell Replay the secret either per-repo (the
**Webhook secret** field when connecting the integration) or globally via
`REPLAY_GITHUB_WEBHOOK_SECRET`. On each push, Replay verifies the
`X-Hub-Signature-256` HMAC and re-pulls any linked script whose `source_path`
was added/modified on the pushed branch. (Push only resyncs script content — it
does not trigger runs; use `POST /api/webhooks/run` for that.)

## Triage

When a run fails, the agent triages it automatically (the background loop runs
whenever `ANTHROPIC_API_KEY` is set):

- **Structured verdict.** The agent records a machine-readable verdict on the
  run — a classification (`real_failure`, `test_bug`, `flake`, `environment`, or
  `inconclusive`), a confidence (`low`/`medium`/`high`), and a one-line summary —
  shown as a badge in the run detail panel so you can scan failures at a glance.
- **PR comments.** When the verdict is a `real_failure` or `test_bug` at medium+
  confidence and the failing run is tied to an open GitHub PR, the agent posts
  its findings as a PR comment. Comments are upserted by a hidden marker (one per
  run, edited on rerun) so PRs never get spammed. Set
  `REPLAY_AUTO_PR_COMMENTS=false` to disable autonomous posting; the agent can
  still comment when you ask it to in chat. Requires a configured GitHub
  integration with write access to issues/PRs (a PAT with `repo`/`pull_requests`
  scope, or a GitHub App installation).

## Services

| Service        | URL                              | Purpose            |
|----------------|----------------------------------|--------------------|
| UI             | http://localhost:3000            | Web interface      |
| API            | http://localhost:8080            | REST API           |
| PostgreSQL     | localhost:5432                   | Database           |
| MQTT           | localhost:1883                   | Message broker     |
| MQTT WS        | localhost:9001                   | WebSocket (browser)|
| MinIO          | http://localhost:9000            | Object storage     |
| MinIO Console  | http://localhost:9091            | MinIO admin UI     |

## Make targets

| Target         | Description                          |
|----------------|--------------------------------------|
| `make dev`     | Build and start all services         |
| `make down`    | Stop all services                    |
| `make logs`    | Follow service logs                  |
| `make migrate` | Run migrations via control-plane     |
| `make test`    | Run Go tests                         |
| `make clean`   | Remove containers, volumes, and cache|
| `make build`   | Build Go binaries to `bin/`          |

## Deploy (self-hosted, single host)

The base `docker-compose.yml` is dev-flavored — all ports bind to loopback and
there is no TLS. For a real deployment, layer the `deploy/` overlay on top:

```bash
cp .env.example .env
$EDITOR .env       # uncomment the "Production overrides" block at the bottom
docker compose \
  -f docker-compose.yml \
  -f deploy/docker-compose.prod.yml \
  up -d
```

What the overlay changes:

- `restart: always` on every long-running service.
- A production UI build (`next build` + `next start`) instead of the base
  file's `next dev`, with `REPLAY_COOKIE_SECURE=true` and the browser's
  `wss://…/mqtt` broker URL baked in, so sessions and live updates work over
  HTTPS.
- Every service stays bound to `127.0.0.1` so a host-level reverse proxy can
  reach them. (If your proxy runs off-host, re-publish the ports you need on a
  private interface in the overlay.)

**Replay does not terminate TLS itself.** Put your own reverse proxy / load
balancer in front and have it:

- terminate TLS for `REPLAY_DOMAIN` — UI (`:3000`), the control-plane API
  (`/api/*`, `/healthz` → `:8080`), and the MQTT-over-WebSocket path
  (`/mqtt` → `pgmqtt:9001`); and
- terminate TLS for the artifacts host (MinIO `:9000`), preserving the original
  `Host`.

MinIO needs its own hostname because S3 presigned URLs are signed against the
`Host` header — a path prefix or a rewritten Host breaks signature
verification. Give it a dedicated name, pass Host through, and set
`PUBLIC_OBJECT_STORE=https://<that-host>/<bucket>` so the control-plane signs
URLs for the name the browser actually uses.

> **Note**: the prod overlay builds the UI as a production bundle. The browser's
> broker URL (`NEXT_PUBLIC_MQTT_WS_URL`) is inlined at build time, so if you
> change `REPLAY_DOMAIN` after the first deploy, rebuild the UI image:
> `docker compose -f docker-compose.yml -f deploy/docker-compose.prod.yml up -d --build ui`.

### First-boot checklist

There are three ways to get an initial admin user / API key; pick one:

| Goal                          | How                                                                                |
|-------------------------------|-------------------------------------------------------------------------------------|
| Interactive admin             | Leave `REPLAY_BOOTSTRAP_USER_*` empty, open `https://{REPLAY_DOMAIN}/setup`.       |
| Non-interactive admin         | Set `REPLAY_BOOTSTRAP_USER_EMAIL` + `REPLAY_BOOTSTRAP_USER_PASSWORD` before boot.  |
| API key for runners / CI      | `docker compose run --rm control-plane control-plane bootstrap-key`, paste into `REPLAY_BOOTSTRAP_API_KEY` (or hand to the caller). |

After boot:

1. Sign in at `/login`, confirm the dashboard loads with no errors.
2. Open browser devtools → confirm a `wss://{REPLAY_DOMAIN}/mqtt` connection is
   established (proves your proxy routes `/mqtt` to the broker correctly).
3. Trigger a run from the UI; confirm artifacts open from your artifacts host
   over HTTPS (proves presigned URLs and the Host pass-through are wired up).

### Enterprise mode (pgmqtt Enterprise)

Replay itself is fully open source; this is the one spot where it integrates a
**paid pgmqtt Enterprise** capability. By default the browser authenticates to
pgmqtt with a SCRAM-SHA-256 password validated against `pg_authid`, and your
reverse proxy carries the MQTT WebSocket on `/mqtt`. With a pgmqtt Enterprise
license you can flip to **trusted-broker mode**: the browser presents a
short-lived Ed25519 JWT as the MQTT CONNECT password and the broker enforces
per-workspace topic ACLs natively from the token's `sub_claims`. The `/mqtt`
proxy hop goes away — the browser dials pgmqtt's WSS listener on port 9002
directly, and pgmqtt terminates that TLS itself.

What you need:

1. A pgmqtt Enterprise license with the `jwt` feature (contact details in
   `docs/enterprise.md` in the pgmqtt repo).
2. An Ed25519 signing key. Mint one with:
   ```bash
   docker compose run --rm control-plane control-plane gen-broker-jwt-key
   ```
   Paste the printed `REPLAY_BROKER_JWT_PRIVATE_KEY=` line into `.env`.
3. Set the Enterprise overrides at the bottom of `.env`:
   ```
   REPLAY_MQTT_TRUSTED_BROKER=true
   REPLAY_PGMQTT_LICENSE_KEY=<your-license-token>
   NEXT_PUBLIC_MQTT_WS_URL=wss://replay.example.com:9002/mqtt
   ```
4. Give pgmqtt a TLS cert for `REPLAY_DOMAIN`. It terminates its own TLS on
   `:9002`, so drop a `cert.crt` + `cert.key` into `deploy/certs/` (bind-mounted
   read-only at `/certs`). Issue them however you already manage certs for your
   reverse proxy — your ACME client, your LB, etc.
5. Bring the stack up. Your reverse proxy still fronts UI/API/artifacts; pgmqtt
   handles the MQTT WSS directly:
   ```bash
   docker compose \
     -f docker-compose.yml \
     -f deploy/docker-compose.prod.yml \
     -f deploy/docker-compose.enterprise.yml \
     up -d
   # pgmqtt has to be restarted to bind the WSS listener after first boot,
   # and again whenever you rotate deploy/certs/cert.{crt,key}:
   docker compose restart pgmqtt
   ```

At control-plane boot, the autoconfig path probes `pgmqtt_license_status()`,
refuses to start if the license isn't active with the `jwt` feature, and
otherwise sets `pgmqtt.jwt_public_key` + `pgmqtt.jwt_required_ws=on` via
`ALTER SYSTEM`. Runners stay on SCRAM (TCP MQTT, unaffected by
`jwt_required_ws`), so this is a browser-side change only.

> **Postgres TLS** is a separate dimension that works without any code
> changes: set `DATABASE_URL=postgres://user:pw@host:5432/db?sslmode=verify-full&sslrootcert=/path/to/ca.crt`
> and pgx will negotiate TLS at the wire. pgmqtt's Enterprise-only MQTTS
> listener on port 8883 (for non-browser MQTT clients) is documented in
> `docs/enterprise.md` but not currently exposed by our overlays — file an
> issue if you need it.

### Backups

Replay's state lives in two volumes: `pgmqtt-data` and `minio-data`. A minimum
backup loop:

```bash
# Postgres logical backup — runs, scripts, integrations, auth, audit.
docker compose exec -T pgmqtt pg_dumpall -U "$POSTGRES_USER" \
  | gzip > "replay-pg-$(date +%F).sql.gz"

# MinIO artifact mirror (videos, traces, screenshots).
docker compose run --rm minio-init \
  mc mirror --overwrite local/replay-artifacts /backup/artifacts
```