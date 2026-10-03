# Scenarios (scenario-as-code)

A scenario describes a fictional enterprise and its world declaratively. The
backend validates it and imports it into PostgreSQL, and the API serves the
**active** scenario's world to players.

```
scenarios/<slug>/
├── scenario.yaml     apiVersion, slug, name, semantic version
├── company.yaml      the organisation (email domain must use a reserved TLD)
├── departments.yaml  departments, parent departments, heads
├── employees.yaml    employees (NPCs), fictional identities, weekly schedules, service accounts
├── network.yaml      network segments (imported as assets of type "network")
├── assets.yaml       devices, servers, applications, groups, cloud + explicit graph edges
├── world.yaml        zones → locations → interactive Three.js objects
└── ranges.yaml       (optional) range templates: workloads + allowed network flows
```

`ranges.yaml` declares the isolated infrastructure players work against. Each
template lists its workloads (pinned image, non-root UID, CPU, memory and
storage, read-only root with explicit writable paths, and exactly one
`terminal: true` workstation) and the only network flows allowed between
them; everything else is denied. Values such as `"secret:db_password"` are
generated per range and stored only in a Kubernetes Secret. Removing a
template deactivates it rather than deleting it, so existing ranges keep
their history.

Scenarios contain **no flags**: progression comes from engagements and
objectives, not CTF-style answers. Everything is fictional. Company email
domains must end in `.local`, `.test`, `.example`, `.internal` or `.invalid`,
and the schema rejects anything else.

## Workflow

```bash
cd backend
```
```bash
make scenario-validate SCENARIO=../scenarios/nexora
```
```bash
make seed
```

Validation runs in two stages, and every problem is reported in one pass:

1. **Schema.** Each file is checked against the JSON Schema
   ([`backend/internal/scenario/schema/scenario.schema.json`](../backend/internal/scenario/schema/scenario.schema.json),
   also printed by `make scenario-schema`). This covers types, enums, code
   patterns and unknown keys.
2. **Cross-references.** The importer checks that:
   - every referenced code exists;
   - codes, IPs and hostnames are unique;
   - department, manager and zone hierarchies have no cycles;
   - host IPs fall inside their network's CIDR;
   - doors lead to zones, and only doors may;
   - interactions are allowed for each object kind;
   - documents with `READ_DOCUMENT` have content;
   - schedules don't overlap;
   - at least one zone is open to new players.

`scenario import` writes everything in one transaction:
- Entities are **upserted by their codes**, so IDs survive re-imports and
  player progress tied to zones isn't lost.
- Anything removed from the files is pruned.
- Identical content is a no-op, decided by a SHA-256 over the files.
- `-activate` publishes the scenario and makes it the served world.
- Imports are audited (`scenario.imported`, `scenario.activated`).

## Conventions

- Codes are UPPER_CASE (`HQ-FIN-PC-04`, `FINANCE_DEPT`). Usernames and object
  keys are lowercase (`alex`, `finance_pc_04`).
- An object `key` is the **name of the Three.js object** in the scene; the
  frontend resolves it with `GET /api/v1/world/objects/{key}`.
- Graph edges come from structured fields wherever possible:
  - `owner` produces OWNS;
  - `network` produces CONNECTED_TO;
  - `hosted_on` produces HOSTED_ON;
  - `depends_on` produces DEPENDS_ON;
  - `manager` produces MANAGES;
  - an employee's `identity` produces HAS_IDENTITY;
  - identity `groups` produce MEMBER_OF;
  - identity `access` produces HAS_ACCESS_TO.

  Use `relationships:` in `assets.yaml` only for anything else.
- In `{ … }` flow mappings, quote values that contain commas.
- Top-level keys starting with `x-` are ignored after anchor expansion, so use
  them to hold YAML anchors, e.g. shared schedules.
- An employee's `persona` is an authoring note for the NPC/dialogue engine and
  is **never** exposed to players. Their `greeting` is.
