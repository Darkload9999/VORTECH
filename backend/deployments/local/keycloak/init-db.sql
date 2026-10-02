-- Idempotently creates Keycloak's own role and database on the shared
-- development PostgreSQL server. Run by the keycloak-db-init compose service
-- on every start (init scripts in docker-entrypoint-initdb.d would only run
-- on an empty volume).
--
-- psql -v kcpass=<password> -f init-db.sql

SELECT format('CREATE ROLE keycloak LOGIN PASSWORD %L', :'kcpass')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'keycloak')
\gexec

SELECT 'CREATE DATABASE keycloak OWNER keycloak'
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'keycloak')
\gexec
