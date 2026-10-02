# VORTECH

A browser-based, open-world cybersecurity simulation platform. Players
work as security professionals inside a living fictional enterprise
(NEXORA Industries). They take on authorised engagements against real,
isolated technical environments running on K3s. This is not a CTF: there
are no flags and no scoreboard.

| Path | Contents |
|---|---|
| [`backend/`](backend/README.md) | Go modular monolith: API, worker, migrations |
| `infrastructure/` | Terraform: K3s, platform, monitoring, range foundation *(phase 9)* |
| `scenarios/` | Scenario-as-code definitions *(phase 3+)* |

The frontend (React, Three.js, xterm.js) is developed separately.

Getting started: see [backend/README.md](backend/README.md).
