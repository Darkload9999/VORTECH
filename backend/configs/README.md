# configs

Non-secret configuration templates consumed by the backend in later phases
(for example the Keycloak realm export for local development in phase 2).

Secrets never live here. Runtime configuration comes from environment
variables (see `../.env.example`); in K3s those come from ConfigMaps and
Secrets, with SOPS + age planned for Git-managed encrypted secrets.
