# VORTECH

[![CI](https://github.com/Darkload9999/VORTECH/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/Darkload9999/VORTECH/actions/workflows/ci.yml)

A browser-based, open-world cybersecurity simulation platform. Players
work as security professionals inside a living fictional enterprise
(NEXORA Industries). They take on authorised engagements against real,
isolated technical environments running on K3s. This is not a CTF: there
are no flags and no scoreboard.

| Path | Contents |
|---|---|
| [`backend/`](backend/README.md) | Go modular monolith: API, worker, migrations |
| `infrastructure/` | Terraform: K3s, platform, monitoring, range foundation *(phase 9)* |
| [`scenarios/`](scenarios/README.md) | Scenario-as-code: the fictional NEXORA Industries world |

The frontend (React, Three.js, xterm.js) is developed separately.

Getting started: see [backend/README.md](backend/README.md).

## Contributing

1. Branch from `main`, using `feat/…`, `fix/…`, `docs/…` or `chore/…`.
2. Before pushing, run `make check` in `backend/`. If you touched the database
   or auth, also run `make deps-up && make test-integration`.
3. Open a pull request. CI runs lint, static analysis, a sqlc staleness check,
   unit and PostgreSQL integration tests, govulncheck and an image build.
   Merge only when it is green.
4. Never commit secrets. `.env` is git-ignored, and the passwords in
   `backend/deployments/local` are development-only placeholders.

Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/)
(`feat(auth): …`, `fix(db): …`).

