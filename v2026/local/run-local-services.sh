#!/usr/bin/env bash

# Read-only application readiness checks for the already-owned local containers.

# A persistent PostgreSQL volume ignores later APP_DB_* initialization values.
# Authenticate with the selected resource, never the container's old environment,
# before claiming that a healthy listener is ready for integration tests.
local_postgres_require_application_access() {
  local container="$1" user="$2" password="$3" database="$4"
  local can_create_database
  # Stdin survives sudo without putting the password in Docker's arguments.
  # The container name uses its bridge address: the image trusts localhost.
  if ! can_create_database="$(
    printf '%s\0' "$password" |
      "${DOCKER[@]}" exec -i "$container" bash -c '
        IFS= read -r -d "" PGPASSWORD || exit 1
        export PGPASSWORD
        export PGCONNECT_TIMEOUT=5
        export PGOPTIONS="-c statement_timeout=5000"
        exec psql -X --no-password -qAt -v ON_ERROR_STOP=1 \
          --host "$3" --port 5432 --username "$1" --dbname "$2" \
          --command "SELECT rolcreatedb FROM pg_roles WHERE rolname = current_user;"
      ' local-postgres-probe "$user" "$database" "$container" 2>/dev/null
  )"; then
    printf '%s\n' 'run-local services: PostgreSQL application authentication failed; use the same local pg.yml profile for the launcher and tests. An existing volume retains its initialized credentials; do not erase it just to switch profiles.' >&2
    return 1
  fi
  if [[ "$can_create_database" != t ]]; then
    printf '%s\n' 'run-local services: PostgreSQL application role must have CREATEDB before integration tests can run' >&2
    return 1
  fi
}
