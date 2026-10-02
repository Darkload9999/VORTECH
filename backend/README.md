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

All three are built into a single distroless image; the entrypoint selects the role.

## Status

| Phase | Scope | State |
|---|---|---|
| 1 | Foundation: config, logging, PostgreSQL, migrations, sqlc, health, shutdown, Docker, compose | **done** |
| 2 | Keycloak OIDC/PKCE, JWT validation, player mapping, RBAC, `/me` | next |
| 3–9 | Company/world, real-time, ranges, terminal, telemetry, storage/observability, infrastructure | planned |

## Requirements

- Go 1.26+
- Docker with Compose v2 and BuildKit (buildx)
- PostgreSQL **18+** (provided by compose; migrations use `uuidv7()`)
- sqlc is optional: `make sqlc` uses a local binary if present, otherwise the pinned `sqlc/sqlc` image

## Quick start

```bash
cd backend
make env              # create .env from .env.example
make db-up            # PostgreSQL 18 on 127.0.0.1:55432
make run-api          # applies migrations (development) and serves on HTTP_ADDR
```

```bash
curl -s localhost:8080/api/v1/health   # {"data":{"status":"ok"}}
curl -s localhost:8080/api/v1/ready    # {"data":{"status":"ready","checks":{"postgres":"up"}}}
```

Run everything in containers instead (migrate job → api + worker):

```bash
make compose-up
make compose-down
```

If ports 8080 or 55432 are taken on your machine, change `HTTP_ADDR`,
`API_HOST_PORT` or `POSTGRES_HOST_PORT` in `.env`.

## Make targets

Run `make help` for the full list. The important ones:

| Target | Purpose |
|---|---|
| `make check` | gofmt check, vet, staticcheck, unit tests (race), sqlc staleness check |
| `make test` | Unit tests with the race detector |
| `make test-integration` | Integration tests against compose PostgreSQL (`make db-up` first) |
| `make vuln` | govulncheck |
| `make build` | Build `bin/api`, `bin/worker`, `bin/migrate` |
| `make docker-build` | Versioned container image `vortech/backend:<git describe>` |
| `make sqlc` | Regenerate `internal/database/db` from `queries/*.sql` |
| `make migrate-create NAME=add_players` | Scaffold `migrations/0000N_add_players.sql` |
| `make migrate-up` / `migrate-down` / `migrate-status` | Manage the schema |
| `make db-reset` | Destroy and recreate the local database |

## Layout

```
cmd/
  api/          composition root for HTTP routes + main
  worker/       background worker main (job handlers arrive in phase 5)
  migrate/      schema migration CLI
internal/
  config/       env configuration, validated at startup (fail fast)
  logging/      slog JSON/text, request-ID enrichment, secret redaction
  requestid/    correlation ID context helpers
  httpx/        JSON envelope + error format, middleware, router, client IP
  health/       liveness, readiness (concurrent, timed, cached), draining
  server/       http.Server lifecycle and graceful shutdown
  database/     pgx pool, goose migrator; db/ is sqlc-generated (do not edit)
  buildinfo/    version metadata injected via -ldflags
  auth/ player/ company/ department/ employee/ asset/ world/ npc/
  interaction/ engagement/ career/ event/ websocket/ terminal/
  cyberrange/ telemetry/ notification/ storage/ cache/
                domain modules; each doc.go states its boundary and phase
migrations/     goose SQL migrations, embedded into every binary
queries/        sqlc query definitions
configs/        non-secret configuration templates (later phases)
deployments/    local/compose.yaml for development
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
| `GET /api/v1/ready` | Instance should receive traffic | PostgreSQL ping; `503 SERVICE_DRAINING` during shutdown |

Readiness checks run concurrently with a per-check timeout and are cached for
`READINESS_CACHE_TTL`, so probes can't turn into database load. Failure
reasons are logged, not returned. The worker serves `/health` and `/ready` on
`WORKER_HTTP_ADDR`, which must never be exposed publicly.

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
