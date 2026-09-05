#!/usr/bin/env bash

# Configure and preflight the local PostgreSQL/Redis contract used by server
# integration tests. Source this before a direct `go test` invocation; test.sh
# sources it automatically.

test_env_error() {
    printf 'test environment: %s\n' "$*" >&2
}

test_env_has_local_resource() {
    local root="$1"
    local resource_name="$2"
    [[ -f "$root/local/$resource_name" ]]
}

test_env_find_resource() {
    local root="$1"
    local resource_name="$2"
    local candidate
    for candidate in "$root/$resource_name" "$root/local/$resource_name"; do
        if [[ -f "$candidate" ]]; then
            TEST_ENV_RESOURCE_PATH="$candidate"
            return 0
        fi
    done
    test_env_error "required resource is missing: $root/{,local/}$resource_name"
    return 1
}

test_env_trim() {
    local value="$1"
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    TEST_ENV_SCALAR="$value"
}

test_env_read_scalar() {
    local resource_path="$1"
    local key="$2"
    local line
    local value
    local double_quoted_scalar='^"(.*)"([[:space:]]+#.*)?$'
    local single_quoted_scalar="^'(.*)'([[:space:]]+#.*)?$"
    while IFS= read -r line || [[ -n "$line" ]]; do
        if [[ "$line" =~ ^[[:space:]]*${key}[[:space:]]*:[[:space:]]*(.*)$ ]]; then
            value="${BASH_REMATCH[1]}"
            test_env_trim "$value"
            value="$TEST_ENV_SCALAR"

            # A YAML comment starts outside a quoted scalar. Unwrap either YAML
            # quote style only as a complete scalar; partial quotes fail closed
            # later as an invalid authority instead of being guessed at.
            if [[ "$value" =~ $double_quoted_scalar ]]; then
                value="${BASH_REMATCH[1]}"
            elif [[ "$value" =~ $single_quoted_scalar ]]; then
                value="${BASH_REMATCH[1]}"
            else
                value="${value%%[[:space:]]#*}"
                test_env_trim "$value"
                value="$TEST_ENV_SCALAR"
            fi
            TEST_ENV_SCALAR="$value"
            return 0
        fi
    done < "$resource_path"
    test_env_error "required key '$key' is missing from $resource_path"
    return 1
}

test_env_expand_scalar() {
    local value="$1"
    local variable_name
    local replacement
    local template
    local prefix
    local suffix
    while [[ "$value" =~ \{\{[[:space:]]*env:([a-zA-Z_][a-zA-Z0-9_]*)[[:space:]]*\}\} ]]; do
        variable_name="${BASH_REMATCH[1]}"
        template="${BASH_REMATCH[0]}"
        replacement="${!variable_name:-}"
        if [[ -z "$replacement" ]]; then
            test_env_error "resource requires environment variable $variable_name"
            return 1
        fi
        # Quoting the replacement operand inside Bash's ${value/a/b} syntax
        # inserts those quote characters into the result. Split around the
        # already-matched literal template and concatenate instead.
        prefix="${value%%"$template"*}"
        suffix="${value#*"$template"}"
        value="${prefix}${replacement}${suffix}"
    done
    TEST_ENV_SCALAR="$value"
}

test_env_split_authority() {
    local authority="$1"
    if [[ "$authority" =~ ^\[(.*)\]:([0-9]+)$ ]]; then
        TEST_ENV_HOST="${BASH_REMATCH[1]}"
        TEST_ENV_PORT="${BASH_REMATCH[2]}"
    elif [[ "$authority" =~ ^([^:]+):([0-9]+)$ ]]; then
        TEST_ENV_HOST="${BASH_REMATCH[1]}"
        TEST_ENV_PORT="${BASH_REMATCH[2]}"
    else
        test_env_error "invalid service authority in local test resource"
        return 1
    fi
}

test_env_probe_service() {
    local service_name="$1"
    local host="$2"
    local port="$3"
    local probe_command="${WARP_TEST_ENV_TCP_PROBE:-}"
    if [[ -n "$probe_command" ]]; then
        if [[ ! -x "$probe_command" ]]; then
            test_env_error "configured TCP probe is not executable: $probe_command"
            return 1
        fi
        if ! "$probe_command" "$service_name" "$host" "$port"; then
            test_env_error "$service_name is unreachable at $host:$port"
            return 1
        fi
        return 0
    fi

    # Bash's /dev/tcp is not portable and can violate guarded-descriptor rules
    # after a successful connect. Zero-I/O mode checks only a bounded connect.
    if ! nc -z -w 3 -- "$host" "$port" </dev/null >/dev/null 2>&1; then
        test_env_error "$service_name is unreachable at $host:$port; start ./local/run-local.sh"
        return 1
    fi
}

test_env_validate_launcher() {
    local postgres_host="$1"
    local postgres_port="$2"
    local redis_host="$3"
    local redis_port="$4"
    if [[ "${WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES:-0}" == "1" ]]; then
        return 0
    fi
    if ! local_run_attestation_validate \
        "$TEST_ENV_RUN_LOCAL_LOCK_DIR" \
        "$TEST_ENV_HOSTS_FILE" \
        "$postgres_host" \
        "$postgres_port" \
        "$redis_host" \
        "$redis_port" \
        "$TEST_ENV_HOSTS_MARKER_BEGIN" \
        "$TEST_ENV_HOSTS_MARKER_END"; then
        test_env_error \
            "launcher-managed local services are not ready; remove legacy aliases only after verifying their owner," \
            "then start ./local/run-local.sh and wait for 'Local environment is up'"
        return 1
    fi
}

test_env_configure() {
    local source_path="${BASH_SOURCE[0]}"
    local source_dir
    local server_dir
    local urnetwork_home
    local portable_root
    local local_state_file

    if [[ "$source_path" == */* ]]; then
        source_dir="${source_path%/*}"
    else
        source_dir="."
    fi
    server_dir="$(cd -- "$source_dir" >/dev/null 2>&1 && pwd)" || {
        test_env_error "cannot resolve the server directory"
        return 1
    }
    urnetwork_home="${server_dir%/*}"
    portable_root="$server_dir/local/testdata"
    local_state_file="$server_dir/local/run-local-state.sh"

    if [[ ! -f "$local_state_file" ]]; then
        test_env_error "local launcher state helper is missing: $local_state_file"
        return 1
    fi
    source "$local_state_file" || {
        test_env_error "could not load local launcher state helper: $local_state_file"
        return 1
    }

    if [[ -n "${WARP_TEST_ENV_TEST_HOSTS_FILE:-}" ||
          -n "${WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR:-}" ]]; then
        if [[ -z "${WARP_TEST_ENV_TCP_PROBE:-}" ||
              -z "${WARP_TEST_ENV_TEST_HOSTS_FILE:-}" ||
              -z "${WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR:-}" ]]; then
            test_env_error "test-only local-state paths require each other and WARP_TEST_ENV_TCP_PROBE"
            return 1
        fi
        TEST_ENV_HOSTS_FILE="$WARP_TEST_ENV_TEST_HOSTS_FILE"
        TEST_ENV_RUN_LOCAL_LOCK_DIR="$WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR"
    else
        TEST_ENV_HOSTS_FILE="/etc/hosts"
        TEST_ENV_RUN_LOCAL_LOCK_DIR="/tmp/urnetwork-server-run-local.lock"
    fi
    TEST_ENV_HOSTS_MARKER_BEGIN="# >>> urnetwork local-env (server/local/run-local.sh) >>>"
    TEST_ENV_HOSTS_MARKER_END="# <<< urnetwork local-env (server/local/run-local.sh) <<<"

    if [[ -n "${WARP_ENV:-}" && "$WARP_ENV" != "local" ]]; then
        test_env_error "refusing WARP_ENV=$WARP_ENV; integration tests require WARP_ENV=local"
        return 1
    fi
    export WARP_ENV="local"
    export WARP_SERVICE="test"
    export WARP_DOMAIN="bringyour.com"
    export WARP_BLOCK="test"
    export WARP_VERSION="0.0.0"
    export BRINGYOUR_POSTGRES_HOSTNAME="${BRINGYOUR_POSTGRES_HOSTNAME:-local-pg.bringyour.com}"
    export BRINGYOUR_REDIS_HOSTNAME="${BRINGYOUR_REDIS_HOSTNAME:-local-redis.bringyour.com}"

    case "${WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES:-0}" in
        0) ;;
        1)
            if [[ "${WARP_TEST_ENV_USE_PORTABLE_RESOURCES:-0}" != "1" ]]; then
                test_env_error \
                    "WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES=1 requires" \
                    "WARP_TEST_ENV_USE_PORTABLE_RESOURCES=1"
                return 1
            fi
            ;;
        *)
            test_env_error "WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES must be 0 or 1"
            return 1
            ;;
    esac

    if [[ "${WARP_TEST_ENV_USE_PORTABLE_RESOURCES:-0}" == "1" ]]; then
        export WARP_VAULT_HOME="$portable_root/vault"
        export WARP_CONFIG_HOME="$portable_root/config"
    elif [[ -z "${WARP_VAULT_HOME:-}" ]]; then
        if [[ -n "${WARP_HOME:-}" ]] &&
            test_env_has_local_resource "$WARP_HOME/vault" pg.yml &&
            test_env_has_local_resource "$WARP_HOME/vault" redis.yml; then
            export WARP_VAULT_HOME="$WARP_HOME/vault"
        elif test_env_has_local_resource "$urnetwork_home/vault" pg.yml &&
            test_env_has_local_resource "$urnetwork_home/vault" redis.yml; then
            export WARP_VAULT_HOME="$urnetwork_home/vault"
        else
            export WARP_VAULT_HOME="$portable_root/vault"
        fi
    fi

    if [[ -z "${WARP_CONFIG_HOME:-}" ]]; then
        if [[ -n "${WARP_HOME:-}" ]] &&
            test_env_has_local_resource "$WARP_HOME/config" db.yml &&
            test_env_has_local_resource "$WARP_HOME/config" redis.yml; then
            export WARP_CONFIG_HOME="$WARP_HOME/config"
        elif test_env_has_local_resource "$urnetwork_home/config" db.yml &&
            test_env_has_local_resource "$urnetwork_home/config" redis.yml; then
            export WARP_CONFIG_HOME="$urnetwork_home/config"
        else
            export WARP_CONFIG_HOME="$portable_root/config"
        fi
    fi
}

test_env_preflight() {
    local command_name
    local pg_resource_path
    local redis_resource_path
    local pg_authority
    local redis_authority
    local pg_host
    local pg_port
    local redis_host
    local redis_port

    for command_name in go grep find dirname sort awk; do
        if ! command -v "$command_name" >/dev/null 2>&1; then
            test_env_error "missing prerequisite: $command_name"
            return 1
        fi
    done
    if [[ -z "${WARP_TEST_ENV_TCP_PROBE:-}" ]] && ! command -v nc >/dev/null 2>&1; then
        test_env_error "missing prerequisite: nc (required for bounded TCP service probes)"
        return 1
    fi

    test_env_find_resource "$WARP_VAULT_HOME" pg.yml || return $?
    pg_resource_path="$TEST_ENV_RESOURCE_PATH"
    test_env_find_resource "$WARP_VAULT_HOME" redis.yml || return $?
    redis_resource_path="$TEST_ENV_RESOURCE_PATH"
    test_env_find_resource "$WARP_CONFIG_HOME" db.yml || return $?
    test_env_find_resource "$WARP_CONFIG_HOME" redis.yml || return $?

    test_env_read_scalar "$pg_resource_path" authority || return $?
    test_env_expand_scalar "$TEST_ENV_SCALAR" || return $?
    pg_authority="$TEST_ENV_SCALAR"
    test_env_split_authority "$pg_authority" || return $?
    pg_host="$TEST_ENV_HOST"
    pg_port="$TEST_ENV_PORT"
    if [[ "$pg_host" != "$BRINGYOUR_POSTGRES_HOSTNAME" ]]; then
        test_env_error "PostgreSQL authority host $pg_host does not match BRINGYOUR_POSTGRES_HOSTNAME"
        return 1
    fi

    test_env_read_scalar "$redis_resource_path" authority || return $?
    test_env_expand_scalar "$TEST_ENV_SCALAR" || return $?
    redis_authority="$TEST_ENV_SCALAR"
    test_env_split_authority "$redis_authority" || return $?
    redis_host="$TEST_ENV_HOST"
    redis_port="$TEST_ENV_PORT"
    if [[ "$redis_host" != "$BRINGYOUR_REDIS_HOSTNAME" ]]; then
        test_env_error "Redis authority host $redis_host does not match BRINGYOUR_REDIS_HOSTNAME"
        return 1
    fi

    test_env_validate_launcher "$pg_host" "$pg_port" "$redis_host" "$redis_port" || return $?
    test_env_probe_service postgres "$pg_host" "$pg_port" || return $?
    test_env_probe_service redis "$redis_host" "$redis_port" || return $?
}

test_env_main() {
    test_env_configure || return $?
    test_env_preflight || return $?
}

test_env_main
test_env_status=$?
if [[ $test_env_status -ne 0 ]]; then
    if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
        exit "$test_env_status"
    fi
    return "$test_env_status"
fi
