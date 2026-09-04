#!/usr/bin/env bash

# Create a minimal throwaway config/vault tree for smoke-test fixtures only.
# Production mounts the frozen host config/local and vault/local directories
# directly and evaluator.sh deliberately never invokes this helper.

set -Eeuo pipefail
umask 077

for command in find install openssl; do
    command -v "$command" >/dev/null 2>&1 || {
        printf 'missing command: %s\n' "$command" >&2
        exit 1
    }
done

if [ "$#" -ne 1 ]; then
    printf 'usage: prepare-runtime.sh EMPTY_OUTPUT_DIRECTORY\n' >&2
    exit 2
fi

runtime_root="$1"
: "${EVALUATION_DB_USER:?}"
: "${EVALUATION_DB_PASSWORD:?}"
: "${EVALUATION_DB_NAME:?}"
: "${EVALUATION_REDIS_PASSWORD:?}"
: "${EVALUATION_DB_MIN_CONNECTIONS:?}"
: "${EVALUATION_DB_MAX_CONNECTIONS:?}"
: "${EVALUATION_REDIS_MIN_CONNECTIONS:?}"
: "${EVALUATION_REDIS_MAX_CONNECTIONS:?}"

[ -d "$runtime_root" ] && [ -z "$(find "$runtime_root" -mindepth 1 -print -quit)" ] || {
    printf 'runtime output must be an existing empty directory\n' >&2
    exit 1
}
[[ "$EVALUATION_DB_USER" =~ ^[a-z][a-z0-9_]{0,62}$ ]]
[[ "$EVALUATION_DB_NAME" =~ ^[a-z][a-z0-9_]{0,62}$ ]]
[[ "$EVALUATION_DB_PASSWORD" =~ ^[A-Za-z0-9._-]{24,128}$ ]]
[[ "$EVALUATION_REDIS_PASSWORD" =~ ^[A-Za-z0-9._-]{24,128}$ ]]
[[ "$EVALUATION_DB_MIN_CONNECTIONS" =~ ^[1-9][0-9]*$ ]]
[[ "$EVALUATION_DB_MAX_CONNECTIONS" =~ ^[1-9][0-9]*$ ]]
[[ "$EVALUATION_REDIS_MIN_CONNECTIONS" =~ ^[1-9][0-9]*$ ]]
[[ "$EVALUATION_REDIS_MAX_CONNECTIONS" =~ ^[1-9][0-9]*$ ]]
[ "$EVALUATION_DB_MIN_CONNECTIONS" -le "$EVALUATION_DB_MAX_CONNECTIONS" ]
[ "$EVALUATION_REDIS_MIN_CONNECTIONS" -le "$EVALUATION_REDIS_MAX_CONNECTIONS" ]

install -d -m 0700 \
    "$runtime_root/vault/local" \
    "$runtime_root/config/local"

# Every cryptographic value below is a per-stage throwaway. It exists only so
# the self-contained simulator can exercise the real authentication and proxy
# paths without receiving any production vault mount.
openssl genpkey \
    -algorithm EC \
    -pkeyopt ec_paramgen_curve:P-256 \
    -out "$runtime_root/vault/local/jwt-key.pem" \
    >/dev/null 2>&1
client_ip_hash_pepper="$(openssl rand -hex 32)"
password_pepper="$(openssl rand -hex 32)"
proxy_secret="$(openssl rand -hex 32)"
wireguard_handoff_key="$(openssl rand -hex 32)"

printf '%s\n' \
    'tls_key_paths:' \
    '  - jwt-key.pem' \
    > "$runtime_root/vault/local/jwt.yml"

printf '%s\n' \
    "client_ip_hash_pepper: \"$client_ip_hash_pepper\"" \
    > "$runtime_root/vault/local/client.yml"

printf '%s\n' \
    'password:' \
    "  pepper: \"$password_pepper\"" \
    > "$runtime_root/vault/local/password.yml"

printf '%s\n' \
    'secrets:' \
    "  - \"$proxy_secret\"" \
    > "$runtime_root/vault/local/proxy.yml"

printf '%s\n' \
    "handoff_encryption_key: \"$wireguard_handoff_key\"" \
    > "$runtime_root/vault/local/wireguard.yml"

printf '%s\n' \
    'authority: "postgres:5432"' \
    "user: \"$EVALUATION_DB_USER\"" \
    "password: \"$EVALUATION_DB_PASSWORD\"" \
    "db: \"$EVALUATION_DB_NAME\"" \
    > "$runtime_root/vault/local/pg.yml"

printf '%s\n' \
    'authority: "redis:6379"' \
    "password: \"$EVALUATION_REDIS_PASSWORD\"" \
    'db: 0' \
    'cluster: false' \
    > "$runtime_root/vault/local/redis.yml"

printf '%s\n' \
    "min_connections: $EVALUATION_DB_MIN_CONNECTIONS" \
    "max_connections: $EVALUATION_DB_MAX_CONNECTIONS" \
    > "$runtime_root/config/local/db.yml"

install -m 0400 /dev/null "$runtime_root/config/local/db_maintenance.yml"

printf '%s\n' \
    "min_connections: $EVALUATION_REDIS_MIN_CONNECTIONS" \
    "max_connections: $EVALUATION_REDIS_MAX_CONNECTIONS" \
    > "$runtime_root/config/local/redis.yml"

printf '%s\n' 'all: {}' > "$runtime_root/config/local/settings.yml"
chmod 0400 \
    "$runtime_root"/vault/local/*.yml \
    "$runtime_root"/vault/local/jwt-key.pem \
    "$runtime_root"/config/local/*.yml

# Keep this tree exact: its two leaf directories are mounted independently.
# Rejecting links and unexpected entries here prevents a future preparer change
# from silently widening the container-visible configuration boundary.
[ -z "$(find "$runtime_root" -mindepth 1 ! -type d ! -type f -print -quit)" ] || {
    printf 'runtime contains a non-regular entry\n' >&2
    exit 1
}
actual_tree="$(find "$runtime_root" -mindepth 1 -printf '%P\n' | sort)"
expected_tree="$(printf '%s\n' \
    config \
    config/local \
    config/local/db.yml \
    config/local/db_maintenance.yml \
    config/local/redis.yml \
    config/local/settings.yml \
    vault \
    vault/local \
    vault/local/client.yml \
    vault/local/jwt-key.pem \
    vault/local/jwt.yml \
    vault/local/password.yml \
    vault/local/pg.yml \
    vault/local/proxy.yml \
    vault/local/redis.yml \
    vault/local/wireguard.yml | sort)"
[ "$actual_tree" = "$expected_tree" ] || {
    printf 'runtime tree does not match the local-only allowlist\n' >&2
    exit 1
}
