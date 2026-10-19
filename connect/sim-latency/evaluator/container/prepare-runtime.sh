#!/usr/bin/env bash

# Create a minimal throwaway config/vault tree for smoke-test fixtures only.
# Production mounts the frozen host config/local and vault/local directories
# directly and evaluator.sh deliberately never invokes this helper.

set -Eeuo pipefail
umask 077

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
readonly GEOLITE_FIXTURE="$SCRIPT_DIR/testdata/runtime/geolite2.mmdb.b64"

for command in base64 find install openssl sha256sum; do
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

# Fail before creating any output if the immutable fixture bundle is incomplete
# or changed. This is a tiny synthetic database, never a live GeoLite download.
[ -f "$GEOLITE_FIXTURE" ] && [ ! -L "$GEOLITE_FIXTURE" ] || {
    printf 'synthetic GeoLite fixture is missing or unsafe\n' >&2
    exit 1
}
fixture_sha256="$(base64 --decode "$GEOLITE_FIXTURE" | sha256sum)" || {
    printf 'synthetic GeoLite fixture is malformed\n' >&2
    exit 1
}
[ "${fixture_sha256%% *}" = 33734fa8dbd3b3a7dfa9cffdec0d7dcefc18abd8bd1857c76094b2cee18d41fb ] || {
    printf 'synthetic GeoLite fixture digest mismatch\n' >&2
    exit 1
}

install -d -m 0700 \
    "$runtime_root/vault" \
    "$runtime_root/vault/local" \
    "$runtime_root/vault/local/tls" \
    "$runtime_root/vault/local/tls/sim.example" \
    "$runtime_root/config" \
    "$runtime_root/config/local" \
    "$runtime_root/config/local/mmdb"

# Every cryptographic value below is a per-stage throwaway. It exists only so
# the self-contained simulator can exercise the real authentication and proxy
# paths without receiving any production vault mount.
openssl genpkey \
    -algorithm EC \
    -pkeyopt ec_paramgen_curve:P-256 \
    -out "$runtime_root/vault/local/jwt-key.pem" \
    >/dev/null 2>&1
# The real Connect constructor loads allowed_hosts even for its H1 simulator.
# Keep its normal TLS loader usable with an independently generated test pair;
# neither self-sign fallback nor production TLS policy is changed.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout "$runtime_root/vault/local/tls/sim.example/sim.example.key" \
    -out "$runtime_root/vault/local/tls/sim.example/sim.example.crt" \
    -days 2 -subj '/CN=sim.example' -addext 'subjectAltName=DNS:sim.example' \
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
printf '%s\n' 'allowed_hosts: [sim.example]' > "$runtime_root/config/local/tls.yml"
# Empty synthetic GeoLite2-City data, bundled beside this helper so relocated
# host-tool releases never need a repository checkout or a live config tree.
base64 --decode "$GEOLITE_FIXTURE" > "$runtime_root/config/local/mmdb/geolite2.mmdb"
chmod 0400 \
    "$runtime_root"/vault/local/*.yml \
    "$runtime_root"/vault/local/jwt-key.pem \
    "$runtime_root"/config/local/*.yml \
    "$runtime_root/vault/local/tls/sim.example/sim.example.crt" \
    "$runtime_root/vault/local/tls/sim.example/sim.example.key" \
    "$runtime_root/config/local/mmdb/geolite2.mmdb"

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
    config/local/mmdb \
    config/local/mmdb/geolite2.mmdb \
    config/local/redis.yml \
    config/local/settings.yml \
    config/local/tls.yml \
    vault \
    vault/local \
    vault/local/client.yml \
    vault/local/jwt-key.pem \
    vault/local/jwt.yml \
    vault/local/password.yml \
    vault/local/pg.yml \
    vault/local/proxy.yml \
    vault/local/redis.yml \
    vault/local/tls \
    vault/local/tls/sim.example \
    vault/local/tls/sim.example/sim.example.crt \
    vault/local/tls/sim.example/sim.example.key \
    vault/local/wireguard.yml | sort)"
[ "$actual_tree" = "$expected_tree" ] || {
    printf 'runtime tree does not match the local-only allowlist\n' >&2
    exit 1
}
