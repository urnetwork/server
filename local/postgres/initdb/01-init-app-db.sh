#!/usr/bin/env bash
#
# Provisions the application role + database described by vault/local/pg.yml.
#
# The official postgres entrypoint runs this once, on first cluster init (empty
# data volume), connected over the local socket as the $POSTGRES_USER superuser.
# The credentials arrive as APP_DB_* env vars, which ../../run-local.sh reads
# from vault/local/pg.yml so this stays a single source of truth.
#
# The role is granted CREATEDB deliberately: server/test_util.go creates a fresh
# `test_<...>` database (OWNER=<app user>) for every test and drops it at
# teardown, so the application role must be able to CREATE/DROP its own
# databases -- it is not a superuser.
#
# To re-run this (e.g. after changing pg.yml), recreate the data volume:
#   ./run-local.sh --fresh   (or: docker compose -f local/docker-compose.yml down -v)

set -euo pipefail

: "${APP_DB_USER:?}"
: "${APP_DB_PASSWORD:?}"
: "${APP_DB_NAME:?}"

# Create (or update) the application login role. CREATE ROLE can't be guarded
# with IF NOT EXISTS, so wrap it in a DO block. \$\$ becomes a literal $$ tag.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres <<SQL
DO \$\$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '${APP_DB_USER}') THEN
    CREATE ROLE "${APP_DB_USER}" LOGIN CREATEDB PASSWORD '${APP_DB_PASSWORD}';
  ELSE
    ALTER ROLE "${APP_DB_USER}" LOGIN CREATEDB PASSWORD '${APP_DB_PASSWORD}';
  END IF;
END
\$\$;
SQL

# Create the base application database if it doesn't already exist.
# template0 + explicit UTF8/en_US.UTF-8 mirrors how the test harness creates its
# per-test databases (en_US.UTF-8 is present in the debian postgres image).
if ! psql -tAX -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
      -c "SELECT 1 FROM pg_database WHERE datname = '${APP_DB_NAME}'" | grep -q '^1$'; then
  createdb --username "$POSTGRES_USER" \
    --owner "$APP_DB_USER" \
    --encoding UTF8 \
    --locale en_US.UTF-8 \
    --template template0 \
    "$APP_DB_NAME"
fi

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres \
  -c "GRANT ALL PRIVILEGES ON DATABASE \"${APP_DB_NAME}\" TO \"${APP_DB_USER}\";"

echo "[init] role '${APP_DB_USER}' and database '${APP_DB_NAME}' are ready"
