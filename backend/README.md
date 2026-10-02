# VORTECH Backend

Go control plane for the VORTECH open-world cybersecurity simulation platform:
authoritative world state, real-platform identity (Keycloak), cyber-range
orchestration on K3s, real-time events and the terminal gateway.

It is a **modular monolith**: one codebase, a few binaries, strict internal
module boundaries, no microservices.

| Binary | Role |
|---|---|
| `api` | Public HTTP/WebSocket API (behind OpenResty) |
| `worker` | Asynchronous jobs: range provisioning, reconciliation, cleanup |
| `migrate` | Schema migrations (`up`, `down`, `status`, `version`, `create`) |
| `scenario` | Validate / import scenario-as-code (`validate`, `import [-activate]`, `schema`) |
| `devtoken` | **Dev only**: logs a seeded user in to the local Keycloak (real PKCE flow) and prints a token. Not included in the image. |

`api`, `worker`, `migrate` and `scenario` are built into a single distroless image; the entrypoint selects the role.

## Status

| Phase | Scope | State |
|---|---|---|
| 1 | Foundation: config, logging, PostgreSQL, migrations, sqlc, health, shutdown, Docker, compose | **done** |
| 2 | Keycloak OIDC/PKCE, JWT validation, player mapping, RBAC, audit, `/me`, OpenAPI | **done** |
| 3a | Scenario-as-code (JSON Schema + importer), NEXORA seed, company/employee/asset/world APIs, digital-twin graph | **done** |
| 3b | Interactions (server-validated), player progress, zone unlocks, career levels, `/me/progress` | next |
| 4–9 | Real-time, ranges, terminal, telemetry, storage/observability, infrastructure | planned |

## Requirements

- Go 1.26+
- Docker with Compose v2 and BuildKit (buildx)
- PostgreSQL **18+** (provided by compose; migrations use `uuidv7()`)
- sqlc is optional: `make sqlc` uses a local binary if present, otherwise the pinned `sqlc/sqlc` image

## Quick start

```bash
cd backend
make env              # create .env from .env.example
make deps-up          # PostgreSQL 18 (127.0.0.1:55432) + Keycloak 26 (127.0.0.1:8180/auth)
make seed             # validate + import + activate the NEXORA world
make run-api          # applies migrations (development) and serves on HTTP_ADDR
```

```bash
curl -s localhost:8080/api/v1/ready    # {"data":{"status":"ready","checks":{"keycloak":"up","postgres":"up"}}}
curl -s -H "Authorization: Bearer $(make -s dev-token AS=player1)" localhost:8080/api/v1/me
curl -s -H "Authorization: Bearer $(make -s dev-token AS=player1)" localhost:8080/api/v1/world/objects/finance_pc_04
```

Run everything in containers instead (migrate job → api + worker):

```bash
make compose-up
make compose-down
```

If ports 8080, 8180 or 55432 are taken on your machine, change `HTTP_ADDR`,
`API_HOST_PORT`, `KEYCLOAK_HOST_PORT` or `POSTGRES_HOST_PORT` in `.env`.

## Make targets

Run `make help` for the full list. The important ones:

| Target | Purpose |
|---|---|
| `make check` | gofmt check, vet, staticcheck, unit tests (race), sqlc staleness check |
| `make test` | Unit tests with the race detector |
| `make test-integration` | Integration + end-to-end auth tests against compose PostgreSQL and Keycloak (`make deps-up` first) |
| `make vuln` | govulncheck |
| `make build` | Build `bin/api`, `bin/worker`, `bin/migrate` |
| `make docker-build` | Versioned container image `vortech/backend:<git describe>` |
| `make sqlc` | Regenerate `internal/database/db` from `queries/*.sql` |
| `make migrate-create NAME=add_players` | Scaffold `migrations/0000N_add_players.sql` |
| `make migrate-up` / `migrate-down` / `migrate-status` | Manage the schema |
| `make db-reset` | Destroy and recreate the local database |
| `make deps-up` / `keycloak-reset` | Start dependencies / re-import the dev realm |
| `make dev-token AS=admin1` | Print an access token for a seeded dev user |
| `make seed` / `scenario-validate` | Import + activate NEXORA / validate a scenario (`SCENARIO=dir`) |

## Layout

```
cmd/
  api/          composition root for HTTP routes + main
  worker/       background worker main (job handlers arrive in phase 5)
  migrate/      schema migration CLI
  scenario/     scenario-as-code validate/import CLI
internal/
  config/       env configuration, validated at startup (fail fast)
  logging/      slog JSON/text, request-ID enrichment, secret redaction
  requestid/    correlation ID context helpers
  httpx/        JSON envelope + error format, middleware, router, client IP
  health/       liveness, readiness (concurrent, timed, cached), draining
  server/       http.Server lifecycle and graceful shutdown
  database/     pgx pool, goose migrator; db/ is sqlc-generated (do not edit)
  auth/         JWKS cache, access-token verifier, roles/permissions, Protect middleware
  auth/authtest fake OIDC issuer for tests
  player/       Keycloak subject → user/player mapping, GET /me
  audit/        append-only audit records (in-transaction or best-effort)
  devauth/      dev-only scripted PKCE login (tests, devtoken)
  scenario/     scenario loader: YAML → JSON Schema → cross-reference checks → importer
  company/      active-world resolver, GET /company, /company/departments
  employee/     employee directory (fictional identities, schedules)
  asset/        asset inventory and the digital-twin graph (Cytoscape.js)
  world/        zones, locations, objects, Three.js object → asset resolution
  buildinfo/    version metadata injected via -ldflags
  department/ npc/
  interaction/ engagement/ career/ event/ websocket/ terminal/
  cyberrange/ telemetry/ notification/ storage/ cache/
                domain modules; each doc.go states its boundary and phase
api/            openapi.json (served at /api/v1/openapi.json, checked by tests)
migrations/     goose SQL migrations, embedded into every binary
queries/        sqlc query definitions
configs/        non-secret configuration templates (later phases)
deployments/    local/compose.yaml, local/keycloak/ (dev realm import, DB bootstrap)
tests/          integration tests (build tag `integration`)
```

`internal/cyberrange` is the spec's "range" module (`range` is a Go keyword).

## API conventions

Versioned under `/api/v1`. Success responses use an envelope:

```json
{ "data": { ... }, "meta": { ... } }
```

Errors always use:

```json
{
  "error": {
    "code": "RANGE_CAPACITY_EXCEEDED",
    "message": "No range capacity is currently available.",
    "request_id": "01a0facb-34c1-7499-a850-d144d0eb10f0",
    "details": { }
  }
}
```

Unknown routes and wrong methods return `NOT_FOUND` / `METHOD_NOT_ALLOWED` in
the same format. Internal errors and panics return an opaque `INTERNAL_ERROR`;
details and stack traces are logged only.

Every response carries `X-Request-ID`. A well-formed incoming ID (e.g. from
OpenResty's `$request_id`) is adopted; anything else is replaced.

### Health

| Endpoint | Meaning | Checks |
|---|---|---|
| `GET /api/v1/health` | Process is alive | none: never fails because a dependency is down |
| `GET /api/v1/ready` | Instance should receive traffic | PostgreSQL ping, Keycloak signing keys loaded; `503 SERVICE_DRAINING` during shutdown |

Readiness checks run concurrently with a per-check timeout and are cached for
`READINESS_CACHE_TTL`, so probes can't turn into database load. Failure
reasons are logged, not returned. The worker serves `/health` and `/ready` on
`WORKER_HTTP_ADDR`, which must never be exposed publicly.

## Authentication & authorization

**Keycloak owns real platform identities** (`real-user@example.com`). Fictional
NEXORA identities (`alex@nexora.local`) are game data in PostgreSQL and are
never Keycloak users. Go never handles passwords.

```
Browser ── Authorization Code + PKCE (S256) ──▶ Keycloak  (realm vortech, client vortech-web)
   │                                               │ signs RS256 access token (5 min)
   └── Authorization: Bearer <access token> ──▶ Go API
                                                   ├─ verify: JWKS signature, iss, aud=vortech-api,
                                                   │          azp=vortech-web, typ=Bearer, exp/nbf/iat
                                                   ├─ map sub → users / players   (create on first sight, audited)
                                                   └─ Protect(permission)          (403 + audit on denial)
```

**Token verification** (`internal/auth`):
- Asymmetric algorithms only. `none` and `HS*` are rejected, which blocks
  signature stripping and algorithm-confusion forgeries.
- Compact JWS only, 8 KiB size limit, `kid` required. The key's declared `alg` must match the token's.
- Exact issuer match, audience must contain `vortech-api`, `azp` must be
  `vortech-web`, and `typ` must be `Bearer`, so ID and refresh tokens are refused.
  `exp` is mandatory, and 30 s of clock skew is tolerated.
- Signing keys load from JWKS in the background and refresh every 15 min, or on
  an unknown `kid` (at most once every 10 s, so random `kid`s can't flood
  Keycloak). A failed refresh never drops the keys already loaded. Readiness
  reports `keycloak: down` until keys are loaded.
- Tokens are accepted only from the `Authorization` header, never from query strings or cookies.

**Identity mapping** (`internal/player`). The first request of a Keycloak
account creates a `users` row keyed by `sub`, and, for `PLAYER`, a `players`
row. Both are written in the same transaction as their audit records. Username,
email and name are re-synced from the token. Mappings are cached in memory for
`AUTH_IDENTITY_CACHE_TTL` (2 min), which also bounds how long a suspension
(`users.status = 'suspended'`) takes to apply. The cache moves to Valkey when
the API scales out.

**Roles and permissions.** Keycloak realm roles are `PLAYER`, `INSTRUCTOR`,
`SCENARIO_CREATOR` and `ADMIN`; new accounts get `PLAYER`. Handlers check
*permissions*, not roles:

| Permission | PLAYER | INSTRUCTOR | SCENARIO_CREATOR | ADMIN |
|---|:-:|:-:|:-:|:-:|
| `profile:read:own` | ✓ | ✓ | ✓ | ✓ |
| `world:read` (company, employees, assets, world) | ✓ | ✓ | ✓ | ✓ |
| `scenario:play`, `range:use:own`, `evidence:manage:own` | ✓ | | | |
| `student:read`, `scenario:assign`, `progress:review` | | ✓ | | ✓ |
| `scenario:create`, `scenario:edit`, `scenario:publish` | | | ✓ | ✓ |
| `platform:administer` | | | | ✓ |

`*:own` permissions also require an ownership check in the owning module.
Game progression (e.g. "unlocked HQ Floor 4") lives in PostgreSQL, never in
Keycloak. Every route except the probes and `/api/v1/openapi.json` is
registered through `Authenticator.Protect(permission, handler)`, and a test
fails if a route is reachable anonymously or missing from the OpenAPI document.

**Errors.**

| Status | Code | Meaning |
|---|---|---|
| 401 | `UNAUTHORIZED` | No bearer token |
| 401 | `INVALID_TOKEN` | Bad signature or claims; re-authenticate |
| 401 | `TOKEN_EXPIRED` | Refresh the token and retry |
| 403 | `FORBIDDEN` | Missing permission (audited as `authz.denied`) |
| 403 | `ACCOUNT_SUSPENDED` | Account is suspended |

401 responses carry an RFC 6750 `WWW-Authenticate` challenge.

### Local Keycloak

`make deps-up` runs Keycloak 26 in dev mode at
<http://localhost:8180/auth>. It is served under `/auth`, like production
behind OpenResty, and uses its own `keycloak` database on the compose
PostgreSQL. The realm is imported from
[`deployments/local/keycloak/realm/vortech-realm.json`](deployments/local/keycloak/realm/vortech-realm.json)
on first start; `make keycloak-reset` re-imports it after edits. The admin
console is at `/auth/admin` (dev credentials `admin` / `admin_dev_only`).

Realm settings:
- `vortech-web`: public client, Authorization Code + PKCE S256 only. Implicit,
  password, device and CIBA grants are disabled. Redirects go to
  `http://localhost:5173/*` only, and there is no offline-access scope.
- `vortech-api`: bearer-only audience. An audience mapper adds it to access tokens.
- Tokens: access tokens last 5 min. Refresh tokens rotate on every use and cannot be reused.
- Accounts: brute-force protection is on, and the password policy requires at
  least 12 characters plus history.
- MFA: TOTP is available, and WebAuthn passkeys are available (passwordless,
  with discoverable credentials).

Seeded **development-only** users. Passwords are in the realm file;
`devtoken` reads them from there:

| User | Roles |
|---|---|
| `player1` | PLAYER |
| `instructor1` | PLAYER, INSTRUCTOR |
| `creator1` | PLAYER, SCENARIO_CREATOR |
| `admin1` | PLAYER, ADMIN |
| `outsider1` | none (authenticates, but every endpoint returns 403) |

### Frontend integration

- Use an OIDC library with PKCE, e.g. `oidc-client-ts` or `keycloak-js`.
  Configure it with authority `http://localhost:8180/auth/realms/vortech`,
  client `vortech-web`, `response_type=code` and `scope=openid`.
- Keep tokens in memory, not `localStorage`, and send `Authorization: Bearer`.
  Refresh when the API returns `TOKEN_EXPIRED` or shortly before expiry.
- In development, have Vite proxy `/api` (and later `/ws`) to the Go API so the
  browser sees one origin, as with OpenResty in production. The API sets no CORS headers.
- `GET /api/v1/me` returns roles and permissions for UI gating only. The server
  re-checks every request.
- Generate types from `/api/v1/openapi.json`, e.g. with `openapi-typescript`.

### Production Keycloak (phase 9)

Phase 9 delivers a production realm and deployment. It must:
- use HTTPS issuer URLs (the config enforces this) and real redirect URIs;
- run Keycloak in `start` mode with its database credentials from Kubernetes Secrets;
- keep `/auth/admin` off the public internet (OpenResty denies it);
- disable self-registration, or restrict it as required;
- enforce MFA for `ADMIN`, with an authentication flow that requires
  OTP/WebAuthn for that role. Optionally, the API can also require a step-up `acr`
  for administrative permissions.

## Company, world and digital twin

The world is defined in scenario-as-code under [`../scenarios`](../scenarios/README.md).
`make seed` imports NEXORA: 8 departments, 31 NPC employees with fictional
`@nexora.local` identities and weekly schedules, 78 assets including 10
networks, 342 graph edges, and 9 zones with 14 locations and 26 interactive
objects. All endpoints require `world:read` and are scoped to the **active**
scenario's company. They return `503 WORLD_NOT_ACTIVE` if none is active.

| Endpoint | Purpose |
|---|---|
| `GET /api/v1/company` | Active company, scenario version, size |
| `GET /api/v1/company/departments` | Departments with heads and headcount |
| `GET /api/v1/employees?department=&q=&limit=&cursor=` | Directory (keyset-paginated) |
| `GET /api/v1/employees/{id}` | Detail: fictional identities and groups, schedule, reports, owned assets |
| `GET /api/v1/assets?type=&department=&q=&limit=&cursor=` | Asset inventory |
| `GET /api/v1/assets/{id}` | Asset detail: networks, owner, location, Three.js objects |
| `GET /api/v1/assets/graph?root=<kind>:<uuid>&depth=` | Digital-twin subgraph as Cytoscape.js `{nodes, edges}` |
| `GET /api/v1/world` | Zone hierarchy |
| `GET /api/v1/world/zones/{id}` | Locations, objects (with assets) and NPCs in a zone |
| `GET /api/v1/world/objects/{key}` | Resolve a Three.js object name to its placement and asset |

**Physical → digital mapping.** `GET /api/v1/world/objects/finance_pc_04` resolves to:
- asset `HQ-FIN-PC-04`, hostname `FIN-PC04`, IP `10.20.30.44`;
- department FIN, owner `EMP-0018` (Alex Morgan, identity `alex@nexora.local`);
- network `NET-HQ-FINANCE` (`10.20.30.0/24`, the finance VLAN).

**Pagination.** List endpoints return `meta: {limit, next_cursor}`. Pass
`next_cursor` back as `?cursor=` until it is `null`. Searches with `q` match
substrings case-insensitively, and LIKE wildcards in `q` are matched literally.

**Graph.** Nodes are employees, fictional identities and assets, with ids
`<kind>:<uuid>`. Edges cover the eleven relationship types (OWNS, MEMBER_OF,
HOSTED_ON, …). Without `root` the whole company graph is returned, capped at
2000 nodes. With `root`, traversal follows edges in both directions up to
`depth` hops (at most 4).

NPC `persona` text is an authoring note and is never returned by the API.

## Configuration

All configuration comes from environment variables. See
[`.env.example`](.env.example) for every variable with its default. Startup
fails, listing every problem at once, if configuration is invalid. Secrets are
never logged: the startup config log redacts the database password, and any
log attribute whose key looks like a credential (`password`, `token`,
`secret`, `authorization`, `cookie`, …) is replaced with `[REDACTED]`.

## Database

- **pgx/v5 pool** with server-side `statement_timeout` and
  `idle_in_transaction_session_timeout`, lifetime jitter and UTC sessions.
  Startup retries with backoff for `DATABASE_STARTUP_TIMEOUT`, then fails.
- **Migrations** (goose, embedded). They run on a dedicated connection with no
  statement timeout but a 15 s `lock_timeout`, so a blocked DDL fails rather
  than stalling live traffic. A PostgreSQL advisory lock serialises concurrent
  migrators.
  - development/test: `DATABASE_AUTO_MIGRATE=true` by default.
  - staging/production: `false` by default. Run `migrate up` as a release job.
    The API and worker **refuse to start** if migrations are pending.
  - Write migrations expand/contract so version N-1 code tolerates schema N.
- **sqlc** generates type-safe queries into `internal/database/db`. Generated
  code is committed. `make sqlc-check` fails if it is stale.
- `audit_logs` is append-only: a trigger rejects `UPDATE` and `DELETE`.

## Graceful shutdown

On SIGTERM/SIGINT the process:

1. marks itself draining, so `/ready` returns 503;
2. keeps serving for `SHUTDOWN_DRAIN_DELAY` while load balancers deregister it;
3. stops accepting connections and waits up to `SHUTDOWN_TIMEOUT` for
   in-flight requests;
4. closes the PostgreSQL pool and exits.

A second signal terminates immediately. In Kubernetes, set
`terminationGracePeriodSeconds` above `SHUTDOWN_DRAIN_DELAY + SHUTDOWN_TIMEOUT`.
Restarting the API never destroys ranges (phase 5 adds reconciliation).

## Container image

- Multi-stage build, `CGO_ENABLED=0`, `-trimpath`, stripped, version via `-ldflags`
- `gcr.io/distroless/static-debian13:nonroot`: no shell, UID 65532
- Works with a read-only root filesystem, all capabilities dropped and `no-new-privileges`
- `api -healthcheck` / `worker -healthcheck` provide container health checks without curl

## Testing

```bash
make test                         # unit tests
make db-up && make test-integration
```

Integration tests create a uniquely named database for each run (through
`TEST_DATABASE_URL`, which needs `CREATEDB`) and drop it afterwards. They
cover migration up/down reversibility, concurrent migrators, sqlc queries and
keyset pagination, audit-log immutability and constraints, statement timeouts,
and readiness against a real database.
