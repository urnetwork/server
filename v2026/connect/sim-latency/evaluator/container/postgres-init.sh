#!/usr/bin/env bash

# Runs only in the pristine PostgreSQL container on its empty tmpfs cluster.
# The admin credential remains in that container; runners receive only the
# throwaway application credential written into their sanitized runtime vault.

set -Eeuo pipefail

: "${APP_DB_USER:?}"
: "${APP_DB_PASSWORD:?}"
: "${APP_DB_NAME:?}"

[[ "$APP_DB_USER" =~ ^[a-z][a-z0-9_]{0,62}$ ]] || {
    printf 'invalid APP_DB_USER\n' >&2
    exit 1
}
[[ "$APP_DB_NAME" =~ ^[a-z][a-z0-9_]{0,62}$ ]] || {
    printf 'invalid APP_DB_NAME\n' >&2
    exit 1
}
[[ "$APP_DB_PASSWORD" =~ ^[A-Za-z0-9._-]{24,128}$ ]] || {
    printf 'invalid APP_DB_PASSWORD\n' >&2
    exit 1
}

psql --set=ON_ERROR_STOP=1 \
    --set=app_user="$APP_DB_USER" \
    --set=app_password="$APP_DB_PASSWORD" \
    --username "$POSTGRES_USER" \
    --dbname postgres <<'SQL'
CREATE ROLE :"app_user" LOGIN PASSWORD :'app_password';
SQL

createdb \
    --username "$POSTGRES_USER" \
    --owner "$APP_DB_USER" \
    --encoding UTF8 \
    --locale en_US.UTF-8 \
    --template template0 \
    "$APP_DB_NAME"
