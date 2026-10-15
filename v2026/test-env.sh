#!/usr/bin/env bash

# Configure and preflight the local PostgreSQL/Redis contract used by server
# integration tests. Source this before a direct `go test` invocation; test.sh
# sources it automatically.
#
# A successful preflight exports WARP_TEST_ENV_ATTESTED_ADDRS, the ip:port
# endpoints it validated. Go test processes dial PostgreSQL and Redis only at
# those endpoints for the whole run, because a service name that re-resolves
# after the launcher exits falls through to whatever answers next: public DNS
# answered the local service names with 127.0.0.1 until those records were
# deleted on 2026-10-14, and a stale cache or a hosts edit still can. Any
# failure leaves the variable unset, and the integration fixtures then refuse
# to start.
#
# In managed-local mode a successful preflight also exports
# WARP_TEST_ENV_LAUNCHER_VAULT_HOME: the vault it selected by the launcher's own
# rules, whose local pg.yml and redis.yml authorities it checked against the
# launcher attestation. Nothing proves the launcher started from that vault;
# credentials from another one fail to authenticate. A Go test binary reads
# only fixture vaults; from that vault it reads just pg.yml, pg_maintenance.yml
# and redis.yml in its real local directory, while the attestation stays valid
# and its dial guard attests the launcher's endpoints, and every other resource
# from local/testdata/vault (env_test_vault.go). That is the sanctioned way to
# run DB tests on a developer machine whose launcher uses a real vault's local
# credentials. Do not set URNETWORK_TEST_REAL_VAULT=1 for DB tests.

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
    for candidate in \
        "$root/$resource_name" \
        "$root/local/$resource_name" \
        "$root/all/$resource_name"; do
        if [[ -f "$candidate" ]]; then
            TEST_ENV_RESOURCE_PATH="$candidate"
            return 0
        fi
    done
    test_env_error "required resource is missing: $root/{,local/,all/}$resource_name"
    return 1
}

# Finds a nested resource (mmdb/places.yml) the way the Go resolver's
# versionLookup does: at every directory level the path continues either
# through its next literal component or through a semantic-version directory,
# so config/all/mmdb/2026.9.23/places.yml answers for mmdb/places.yml.
# Version-directory symlinks are not followed, matching the resolver.
test_env_find_versioned_resource() {
    local directory="$1"
    local resource_name="$2"
    local version_path

    if [[ "$resource_name" == */* ]]; then
        if [[ -d "$directory/${resource_name%%/*}" ]] &&
            test_env_find_versioned_resource "$directory/${resource_name%%/*}" "${resource_name#*/}"; then
            return 0
        fi
    elif [[ -f "$directory/$resource_name" ]]; then
        TEST_ENV_RESOURCE_PATH="$directory/$resource_name"
        return 0
    fi
    while IFS= read -r version_path; do
        if test_env_is_semver_name "${version_path##*/}" &&
            test_env_find_versioned_resource "$version_path" "$resource_name"; then
            return 0
        fi
    done < <(find -H "$directory" -mindepth 1 -maxdepth 1 -type d -print 2>/dev/null | LC_ALL=C sort)
    return 1
}

test_env_find_nested_resource() {
    local root="$1"
    local resource_name="$2"
    local home
    for home in "$root" "$root/local" "$root/all"; do
        if [[ -d "$home" ]] && test_env_find_versioned_resource "$home" "$resource_name"; then
            return 0
        fi
    done
    test_env_error "required resource is missing: $root/{,local/,all/}$resource_name"
    return 1
}

test_env_int64_decimal() {
    local value="$1"
    local LC_ALL=C

    while [[ "${#value}" -gt 1 && "$value" == 0* ]]; do
        value="${value#0}"
    done
    if [[ "${#value}" -lt 19 ]]; then
        return 0
    fi
    if [[ "${#value}" -gt 19 ]]; then
        return 1
    fi
    [[ "$value" < 9223372036854775807 || "$value" == 9223372036854775807 ]]
}

test_env_is_semver_name() {
    local name="$1"
    local pattern='^([0-9]+)\.([0-9]+)\.([0-9]+)(-([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?)?(\+([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?)?$'
    local major
    local minor
    local patch

    [[ "$name" =~ $pattern ]] || return 1
    major="${BASH_REMATCH[1]}"
    minor="${BASH_REMATCH[2]}"
    patch="${BASH_REMATCH[3]}"
    test_env_int64_decimal "$major" &&
        test_env_int64_decimal "$minor" &&
        test_env_int64_decimal "$patch"
}

test_env_tls_validate_pair_location() {
    local directory="$1"
    local host_name="$2"
    local cert_path="$directory/$host_name.crt"
    local key_path="$directory/$host_name.key"
    local cert_present=0
    local key_present=0

    if [[ -f "$cert_path" ]]; then
        [[ -r "$cert_path" ]] || {
            test_env_error "TLS certificate is not readable: $cert_path"
            return 1
        }
        cert_present=1
    fi
    if [[ -f "$key_path" ]]; then
        [[ -r "$key_path" ]] || {
            test_env_error "TLS key is not readable: $key_path"
            return 1
        }
        key_present=1
    fi
    if [[ "$cert_present" != "$key_present" ]]; then
        test_env_error "TLS certificate/key pair is incomplete: $directory/$host_name.{crt,key}"
        return 1
    fi
    if [[ "$cert_present" == 1 ]]; then
        TEST_ENV_TLS_PAIR_FOUND=1
    fi
}

test_env_tls_validate_version_locations() {
    local root="$1"
    local host_name="$2"
    local component_index="$3"
    local literal_component
    local version_path
    local version_name

    if [[ "$component_index" == 0 ]]; then
        literal_component=tls
    elif [[ "$component_index" == 1 ]]; then
        literal_component="$host_name"
    else
        test_env_tls_validate_pair_location "$root" "$host_name" || return $?
    fi

    if [[ "$component_index" -lt 2 && -d "$root/$literal_component" ]]; then
        test_env_tls_validate_version_locations \
            "$root/$literal_component" \
            "$host_name" \
            "$((component_index + 1))" || return $?
    fi

    # The Go resolver permits a semantic-version directory at every path
    # level. Visit all of them; complete pairs at every visible location make
    # its independently ordered certificate/key candidate lists identical.
    while IFS= read -r version_path; do
        version_name="${version_path##*/}"
        if test_env_is_semver_name "$version_name"; then
            test_env_tls_validate_version_locations \
                "$version_path" \
                "$host_name" \
                "$component_index" || return $?
        fi
    done < <(find -H "$root" -mindepth 1 -maxdepth 1 -type d -print 2>/dev/null | LC_ALL=C sort)
}

test_env_tls_tree_complete() {
    local vault_root="$1"
    local host_name
    local TEST_ENV_TLS_PAIR_FOUND=0

    for host_name in ur.network bringyour.com main-connect.ur.network main-connect.bringyour.com; do
        TEST_ENV_TLS_PAIR_FOUND=0
        test_env_tls_validate_version_locations "$vault_root" "$host_name" 0 || return $?
        if [[ -d "$vault_root/local" ]]; then
            test_env_tls_validate_version_locations "$vault_root/local" "$host_name" 0 || return $?
        fi
        if [[ -d "$vault_root/all" ]]; then
            test_env_tls_validate_version_locations "$vault_root/all" "$host_name" 0 || return $?
        fi
        if [[ "$TEST_ENV_TLS_PAIR_FOUND" != 1 ]]; then
            return 1
        fi
    done
}

test_env_find_resource_tree() {
    local root="$1"
    local resource_name="$2"
    if [[ "$resource_name" == tls ]] && test_env_tls_tree_complete "$root"; then
        TEST_ENV_RESOURCE_PATH="$root"
        return 0
    fi
    test_env_error "required complete resource tree is missing: $root/{,local/,all/}$resource_name"
    return 1
}

# Reads the checked-in, non-executable resource boundary for a complete local
# server suite. Vault names are constrained to one path element before the
# normal root/local/all resolver is used. A config name may be a relative path
# of such elements (mmdb/places.yml, which ships in a dated directory beside
# its database) and resolves through version directories as the Go resolver
# does; no element may begin with a dot, so none can climb out of the root.
test_env_validate_suite_resource_manifest() {
    local manifest_path="$1"
    local vault_root="$2"
    local config_root="$3"
    local line
    local resource_kind
    local resource_name
    local resource_key
    local seen_resource_keys="|"
    local line_number=0
    local vault_resource_count=0
    local vault_tree_count=0
    local config_resource_count=0

    if [[ ! -f "$manifest_path" || -L "$manifest_path" || ! -r "$manifest_path" ]]; then
        test_env_error "suite resource manifest is not a readable regular file: $manifest_path"
        return 1
    fi
    while IFS= read -r line || [[ -n "$line" ]]; do
        line_number=$((line_number + 1))
        if [[ "$line_number" == 1 ]]; then
            if [[ "$line" != format=urnetwork-server-suite-resources-v1 ]]; then
                test_env_error "suite resource manifest has an invalid format: $manifest_path"
                return 1
            fi
            continue
        fi
        if [[ "${#line}" -gt 256 ]]; then
            test_env_error "suite resource manifest has an invalid entry: $manifest_path"
            return 1
        fi
        if [[ "$line" =~ ^(vault|vault_tree|config)=([a-zA-Z0-9][a-zA-Z0-9._-]*)$ ]]; then
            resource_kind="${BASH_REMATCH[1]}"
            resource_name="${BASH_REMATCH[2]}"
        elif [[ "$line" =~ ^(config)=([a-zA-Z0-9][a-zA-Z0-9._-]*(/[a-zA-Z0-9][a-zA-Z0-9._-]*)+)$ ]]; then
            resource_kind="${BASH_REMATCH[1]}"
            resource_name="${BASH_REMATCH[2]}"
        else
            test_env_error "suite resource manifest has an invalid entry: $manifest_path"
            return 1
        fi
        resource_key="$resource_kind:$resource_name"
        if [[ "$resource_kind" == vault_tree && "$resource_name" != tls ]]; then
            test_env_error "suite resource manifest has an unsupported vault tree: $resource_name"
            return 1
        fi
        if [[ "$seen_resource_keys" == *"|$resource_key|"* ]]; then
            test_env_error "suite resource manifest has a duplicate entry: $resource_key"
            return 1
        fi
        seen_resource_keys="${seen_resource_keys}${resource_key}|"
        if [[ "$resource_kind" == vault ]]; then
            vault_resource_count=$((vault_resource_count + 1))
            test_env_find_resource "$vault_root" "$resource_name" || return $?
        elif [[ "$resource_kind" == vault_tree ]]; then
            vault_tree_count=$((vault_tree_count + 1))
            test_env_find_resource_tree "$vault_root" "$resource_name" || return $?
        elif [[ "$resource_name" == */* ]]; then
            config_resource_count=$((config_resource_count + 1))
            test_env_find_nested_resource "$config_root" "$resource_name" || return $?
        else
            config_resource_count=$((config_resource_count + 1))
            test_env_find_resource "$config_root" "$resource_name" || return $?
        fi
    done < "$manifest_path"
    if [[ "$line_number" == 0 || "$vault_resource_count" == 0 || "$vault_tree_count" == 0 ||
          "$config_resource_count" == 0 ]]; then
        test_env_error "suite resource manifest is incomplete: $manifest_path"
        return 1
    fi
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

# Accepts the dotted IPv4 and colon-hex IPv6 shapes an attested endpoint may
# carry. The Go dial guard parses every exported endpoint strictly.
test_env_is_address_literal() {
    local host="$1"
    [[ "$host" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] && return 0
    [[ "$host" == *:* && "$host" =~ ^[0-9A-Fa-f:.]+$ ]]
}

# Matches 127.0.0.0/8 and ::1, including the IPv4-mapped and zero-padded
# spellings a resolver may print.
test_env_is_loopback_address() {
    local address="$1"
    [[ "$address" =~ ^127\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]] && return 0
    [[ "$address" =~ ^::[Ff]{4}:127\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]] && return 0
    [[ "$address" =~ ^[0:]*:0*1$ ]]
}

# Matches 0.0.0.0 and ::, which name no single service host.
test_env_is_unspecified_address() {
    local address="$1"
    [[ "$address" =~ ^0+(\.0+){3}$ || "$address" =~ ^[0:]+$ ]]
}

# Prints each address the host's ordinary resolver returns for a name, one per
# line. WARP_TEST_ENV_HOST_RESOLVER replaces it in hermetic harness tests.
test_env_resolve_host() {
    local host_name="$1"
    local resolver_command="${WARP_TEST_ENV_HOST_RESOLVER:-}"
    if [[ -n "$resolver_command" ]]; then
        if [[ ! -x "$resolver_command" ]]; then
            test_env_error "configured host resolver is not executable: $resolver_command"
            return 1
        fi
        "$resolver_command" "$host_name"
        return
    fi
    if command -v getent >/dev/null 2>&1; then
        getent ahosts "$host_name" | awk '{ print $1 }' | LC_ALL=C sort -u
        return "${PIPESTATUS[0]}"
    fi
    if command -v dscacheutil >/dev/null 2>&1; then
        dscacheutil -q host -a name "$host_name" |
            awk '$1 == "ip_address:" || $1 == "ipv6_address:" { print $2 }' |
            LC_ALL=C sort -u
        return "${PIPESTATUS[0]}"
    fi
    test_env_error "missing prerequisite: getent or dscacheutil (required to check service-name resolution)"
    return 1
}

# A launcher-managed name must resolve only to the launcher's dedicated address.
# Public DNS answered both local service names with 127.0.0.1 until 2026-10-14,
# and a stale cache or a hosts edit can still answer loopback, where a
# production data tunnel can listen, so a withdrawn or shadowed hosts mapping
# stops the run here instead of reaching whatever listens on loopback.
test_env_require_attested_resolution() {
    local service_name="$1"
    local host_name="$2"
    local port="$3"
    local attested_address="$4"
    local resolved
    local address
    local resolved_count=0

    if test_env_is_address_literal "$host_name"; then
        resolved="$host_name"
    elif ! resolved="$(test_env_resolve_host "$host_name")"; then
        test_env_error "$service_name host $host_name did not resolve"
        return 1
    fi
    while IFS= read -r address; do
        if [[ -z "$address" ]]; then
            continue
        fi
        resolved_count=$((resolved_count + 1))
        if test_env_is_loopback_address "$address"; then
            test_env_error \
                "$service_name host $host_name (port $port) resolves to loopback $address," \
                "where a production data tunnel can listen; refusing. Is ./local/run-local.sh still up?"
            return 1
        fi
        if [[ "$address" != "$attested_address" ]]; then
            test_env_error \
                "$service_name host $host_name (port $port) resolves to $address," \
                "not the launcher-attested $attested_address"
            return 1
        fi
    done <<< "$resolved"
    if [[ "$resolved_count" == 0 ]]; then
        test_env_error "$service_name host $host_name resolved to no address"
        return 1
    fi
}

# The launcher's readiness record names the only address its names may reach.
test_env_require_managed_resolution() {
    local postgres_host="$1"
    local postgres_port="$2"
    local redis_host="$3"
    local redis_port="$4"
    local attested_address="$LOCAL_RUN_READY_HOST_IP"

    if ! test_env_is_address_literal "$attested_address" ||
        test_env_is_loopback_address "$attested_address" ||
        test_env_is_unspecified_address "$attested_address"; then
        test_env_error "launcher-attested address $attested_address is not a dedicated non-loopback address"
        return 1
    fi
    test_env_require_attested_resolution postgres "$postgres_host" "$postgres_port" "$attested_address" || return $?
    test_env_require_attested_resolution redis "$redis_host" "$redis_port" "$attested_address" || return $?
    TEST_ENV_DIAL_ADDRESS="$attested_address"
}

# Unmanaged portable services have no launcher record. An address-literal
# authority attests itself, except that loopback is accepted only under the
# private portable root, which names its own 127.0.0.1 endpoints; elsewhere a
# loopback service may be a production data tunnel. A name is resolved once and
# its single address is pinned for the whole run; a name that resolves to
# loopback is refused, since public DNS answered the local service names that
# way until 2026-10-14 and a stale answer still can.
test_env_pin_unmanaged_service() {
    local service_name="$1"
    local host_name="$2"
    local resolved
    local address
    local addresses=()

    if test_env_is_address_literal "$host_name"; then
        if test_env_is_loopback_address "$host_name" && [[ -z "${WARP_TEST_ENV_PORTABLE_ROOT:-}" ]]; then
            test_env_error \
                "$service_name authority $host_name is loopback, where a production data tunnel can listen;" \
                "loopback services need the private portable root (WARP_TEST_ENV_PORTABLE_ROOT) with explicit" \
                "127.0.0.1 authorities"
            return 1
        fi
        TEST_ENV_DIAL_ADDRESS="$host_name"
        return 0
    fi
    if ! resolved="$(test_env_resolve_host "$host_name")"; then
        test_env_error "$service_name host $host_name did not resolve"
        return 1
    fi
    while IFS= read -r address; do
        if [[ -n "$address" ]]; then
            addresses+=("$address")
        fi
    done <<< "$resolved"
    if [[ "${#addresses[@]}" != 1 ]] || ! test_env_is_address_literal "${addresses[0]}"; then
        test_env_error \
            "$service_name host $host_name resolves to ${#addresses[@]} addresses;" \
            "an unmanaged service name must resolve to exactly one address to be pinned"
        return 1
    fi
    if test_env_is_loopback_address "${addresses[0]}"; then
        test_env_error \
            "$service_name host $host_name resolves to loopback ${addresses[0]}, where a production data" \
            "tunnel can listen; name loopback services by explicit 127.0.0.1 authorities through" \
            "WARP_TEST_ENV_PORTABLE_ROOT"
        return 1
    fi
    TEST_ENV_DIAL_ADDRESS="${addresses[0]}"
}

# The loopback ports a production data tunnel forwards
# (xops/bin/by-patch-main-data): PostgreSQL, PgBouncer and the Redis cluster
# nodes.
test_env_is_loopback_tunnel_port() {
    local port="$1"
    [[ "$port" =~ ^[0-9]{1,5}$ ]] || return 1
    (( 10#$port == 5432 || 10#$port == 6432 || (10#$port >= 6379 && 10#$port <= 6427) ))
}

# No mode may attest or probe a loopback endpoint on a tunnel port.
test_env_refuse_loopback_tunnel_endpoint() {
    local service_name="$1"
    local address="$2"
    local port="$3"
    if test_env_is_loopback_address "$address" && test_env_is_loopback_tunnel_port "$port"; then
        test_env_error \
            "$service_name endpoint $address:$port is a loopback production data tunnel port" \
            "(5432, 6432, 6379-6427); refusing"
        return 1
    fi
}

# Formats one attested endpoint the way the Go guard parses it.
test_env_format_endpoint() {
    local address="$1"
    local port="$2"
    if [[ "$address" == *:* ]]; then
        TEST_ENV_ENDPOINT="[$address]:$port"
    else
        TEST_ENV_ENDPOINT="$address:$port"
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

test_env_validate_suite_proxy_snapshot() {
    suite_proxy_attestation_validate_snapshot \
        "$TEST_ENV_SUITE_PROXY_STATE_DIR" \
        "$SUITE_PROXY_SNAPSHOT_OWNER_PID" \
        "$SUITE_PROXY_SNAPSHOT_OWNER_START_IDENTITY" \
        "$SUITE_PROXY_SNAPSHOT_OWNER_TOKEN" \
        "$SUITE_PROXY_SNAPSHOT_GENERATION" \
        "$SUITE_PROXY_SNAPSHOT_HOST_IP" \
        "$SUITE_PROXY_SNAPSHOT_POSTGRES_HOST" \
        "$SUITE_PROXY_SNAPSHOT_POSTGRES_PORT" \
        "$SUITE_PROXY_SNAPSHOT_REDIS_HOST" \
        "$SUITE_PROXY_SNAPSHOT_REDIS_PORT" \
        "$SUITE_PROXY_SNAPSHOT_POSTGRES_UPSTREAM_ID" \
        "$SUITE_PROXY_SNAPSHOT_REDIS_UPSTREAM_ID" \
        "$SUITE_PROXY_SNAPSHOT_IMAGE_ID" \
        "$SUITE_PROXY_SNAPSHOT_POSTGRES_PROXY_NAME" \
        "$SUITE_PROXY_SNAPSHOT_POSTGRES_PROXY_ID" \
        "$SUITE_PROXY_SNAPSHOT_REDIS_PROXY_NAME" \
        "$SUITE_PROXY_SNAPSHOT_REDIS_PROXY_ID"
}

test_env_configure() {
    local source_path="${BASH_SOURCE[0]}"
    local source_dir
    local server_dir
    local urnetwork_home
    local portable_root
    local private_authority
    local local_state_file
    local suite_proxy_state_dir="${WARP_TEST_ENV_SUITE_PROXY_STATE_DIR:-}"

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
    TEST_ENV_SUITE_RESOURCE_MANIFEST="$server_dir/local/suite-resource-manifest.txt"

    if [[ ! -f "$local_state_file" ]]; then
        test_env_error "local launcher state helper is missing: $local_state_file"
        return 1
    fi
    source "$local_state_file" || {
        test_env_error "could not load local launcher state helper: $local_state_file"
        return 1
    }

    TEST_ENV_SUITE_PROXY_MODE=0
    if [[ -n "$suite_proxy_state_dir" ]]; then
        TEST_ENV_SUITE_PROXY_MODE=1
        TEST_ENV_SUITE_PROXY_STATE_DIR="$suite_proxy_state_dir"
        if [[ "$suite_proxy_state_dir" != /* ]]; then
            test_env_error "WARP_TEST_ENV_SUITE_PROXY_STATE_DIR must be an absolute path"
            return 1
        fi
    fi

    if [[ -n "${WARP_TEST_ENV_TEST_HOSTS_FILE:-}" ||
          -n "${WARP_TEST_ENV_TEST_RUN_LOCAL_LOCK_DIR:-}" ]]; then
        if [[ "$TEST_ENV_SUITE_PROXY_MODE" == 1 ]]; then
            test_env_error "suite-proxy mode is mutually exclusive with managed-local state paths"
            return 1
        fi
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

    case "${WARP_TEST_ENV_USE_PORTABLE_RESOURCES:-0}" in
        0 | 1) ;;
        *)
            test_env_error "WARP_TEST_ENV_USE_PORTABLE_RESOURCES must be 0 or 1"
            return 1
            ;;
    esac

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

    # A disposable gate can supply its own daemon-assigned service ports.
    # This remains the explicit portable escape, with exact authorities and
    # the same service probes; it cannot redirect the managed local profile.
    if [[ -n "${WARP_TEST_ENV_PORTABLE_ROOT:-}" ]]; then
        if [[ "${WARP_TEST_ENV_USE_PORTABLE_RESOURCES:-0}" != 1 ||
              "${WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES:-0}" != 1 ]]; then
            test_env_error "a private portable root requires both portable service flags"
            return 1
        fi
        portable_root="$WARP_TEST_ENV_PORTABLE_ROOT"
        if [[ "$portable_root" != /* || ! -d "$portable_root" || -L "$portable_root" || ! -O "$portable_root" ]]; then
            test_env_error "private portable resource root must be an absolute owned physical directory"
            return 1
        fi
        if [[ "$(cd -- "$portable_root" && pwd -P)" != "$portable_root" ]]; then
            test_env_error "private portable resource root must not contain a path alias"
            return 1
        fi
        if [[ "$(stat -c '%a' "$portable_root")" != 700 ]]; then
            test_env_error "private portable resource root must have mode 700"
            return 1
        fi
        for private_authority in \
            "${WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY:-}" \
            "${WARP_TEST_ENV_PORTABLE_REDIS_AUTHORITY:-}"; do
            if [[ ! "$private_authority" =~ ^127\.0\.0\.1:([1-9][0-9]{0,4})$ ]] ||
                (( 10#${BASH_REMATCH[1]:-0} > 65535 )); then
                test_env_error "private portable services require explicit loopback host and port"
                return 1
            fi
            test_env_split_authority "$private_authority" || return $?
        done
        if [[ "$WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY" == "$WARP_TEST_ENV_PORTABLE_REDIS_AUTHORITY" ]]; then
            test_env_error "private portable services require distinct endpoints"
            return 1
        fi
    fi

    if [[ "$TEST_ENV_SUITE_PROXY_MODE" == 1 ]]; then
        if [[ "${WARP_TEST_ENV_USE_PORTABLE_RESOURCES:-0}" != 0 ||
              "${WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES:-0}" != 0 ]]; then
            test_env_error "suite-proxy mode is mutually exclusive with portable or unmanaged services"
            return 1
        fi
        if [[ -z "${WARP_VAULT_HOME:-}" || -z "${WARP_CONFIG_HOME:-}" ]]; then
            test_env_error "suite-proxy mode requires explicit WARP_VAULT_HOME and WARP_CONFIG_HOME"
            return 1
        fi
        if [[ "$WARP_VAULT_HOME" != /* || "$WARP_CONFIG_HOME" != /* ]]; then
            test_env_error "suite-proxy resource roots must be absolute paths"
            return 1
        fi
        export WARP_VAULT_HOME WARP_CONFIG_HOME
    else
        export BRINGYOUR_POSTGRES_HOSTNAME="${BRINGYOUR_POSTGRES_HOSTNAME:-local-pg.bringyour.com}"
        export BRINGYOUR_REDIS_HOSTNAME="${BRINGYOUR_REDIS_HOSTNAME:-local-redis.bringyour.com}"
    fi

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
    local pg_dial_address
    local redis_dial_address
    local pg_endpoint

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
    if [[ "$TEST_ENV_SUITE_PROXY_MODE" == 1 ]] && ! command -v ps >/dev/null 2>&1; then
        test_env_error "missing prerequisite: ps (required for suite-proxy owner validation)"
        return 1
    fi

    if [[ "$TEST_ENV_SUITE_PROXY_MODE" == 1 ]]; then
        if ! suite_proxy_attestation_validate "$TEST_ENV_SUITE_PROXY_STATE_DIR"; then
            test_env_error "repository-owned suite proxy is not ready"
            return 1
        fi
        if [[ -n "${BRINGYOUR_POSTGRES_HOSTNAME:-}" &&
              "$BRINGYOUR_POSTGRES_HOSTNAME" != "$SUITE_PROXY_SNAPSHOT_POSTGRES_HOST" ]]; then
            test_env_error "BRINGYOUR_POSTGRES_HOSTNAME conflicts with the attested suite proxy"
            return 1
        fi
        if [[ -n "${BRINGYOUR_REDIS_HOSTNAME:-}" &&
              "$BRINGYOUR_REDIS_HOSTNAME" != "$SUITE_PROXY_SNAPSHOT_REDIS_HOST" ]]; then
            test_env_error "BRINGYOUR_REDIS_HOSTNAME conflicts with the attested suite proxy"
            return 1
        fi
        export BRINGYOUR_POSTGRES_HOSTNAME="$SUITE_PROXY_SNAPSHOT_POSTGRES_HOST"
        export BRINGYOUR_REDIS_HOSTNAME="$SUITE_PROXY_SNAPSHOT_REDIS_HOST"
        test_env_validate_suite_resource_manifest \
            "$TEST_ENV_SUITE_RESOURCE_MANIFEST" \
            "$WARP_VAULT_HOME" \
            "$WARP_CONFIG_HOME" || return $?
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
    if [[ -n "${WARP_TEST_ENV_PORTABLE_ROOT:-}" ]]; then
        if [[ "$pg_authority" != "$WARP_TEST_ENV_PORTABLE_POSTGRES_AUTHORITY" ]]; then
            test_env_error "PostgreSQL authority differs from the private portable endpoint"
            return 1
        fi
        test_env_find_resource "$WARP_VAULT_HOME" pg_maintenance.yml || return $?
        if ! cmp -s "$pg_resource_path" "$TEST_ENV_RESOURCE_PATH"; then
            test_env_error "PostgreSQL maintenance resource differs from its private application resource"
            return 1
        fi
    fi
    test_env_split_authority "$pg_authority" || return $?
    pg_host="$TEST_ENV_HOST"
    pg_port="$TEST_ENV_PORT"
    if [[ "$TEST_ENV_SUITE_PROXY_MODE" == 1 ]]; then
        if [[ "$pg_host" != "$SUITE_PROXY_SNAPSHOT_POSTGRES_HOST" ||
              "$pg_port" != "$SUITE_PROXY_SNAPSHOT_POSTGRES_PORT" ]]; then
            test_env_error "PostgreSQL resource authority does not match the attested suite proxy endpoint"
            return 1
        fi
    elif [[ "$pg_host" != "$BRINGYOUR_POSTGRES_HOSTNAME" ]]; then
        test_env_error "PostgreSQL authority host $pg_host does not match BRINGYOUR_POSTGRES_HOSTNAME"
        return 1
    fi

    test_env_read_scalar "$redis_resource_path" authority || return $?
    test_env_expand_scalar "$TEST_ENV_SCALAR" || return $?
    redis_authority="$TEST_ENV_SCALAR"
    if [[ -n "${WARP_TEST_ENV_PORTABLE_ROOT:-}" && "$redis_authority" != "$WARP_TEST_ENV_PORTABLE_REDIS_AUTHORITY" ]]; then
        test_env_error "Redis authority differs from the private portable endpoint"
        return 1
    fi
    test_env_split_authority "$redis_authority" || return $?
    redis_host="$TEST_ENV_HOST"
    redis_port="$TEST_ENV_PORT"
    if [[ "$TEST_ENV_SUITE_PROXY_MODE" == 1 ]]; then
        if [[ "$redis_host" != "$SUITE_PROXY_SNAPSHOT_REDIS_HOST" ||
              "$redis_port" != "$SUITE_PROXY_SNAPSHOT_REDIS_PORT" ]]; then
            test_env_error "Redis resource authority does not match the attested suite proxy endpoint"
            return 1
        fi
    elif [[ "$redis_host" != "$BRINGYOUR_REDIS_HOSTNAME" ]]; then
        test_env_error "Redis authority host $redis_host does not match BRINGYOUR_REDIS_HOSTNAME"
        return 1
    fi

    # Each mode settles the one address per service its clients may dial. The
    # probes and the exported attestation use those literals, never a name that
    # could resolve somewhere else later in the run.
    if [[ "$TEST_ENV_SUITE_PROXY_MODE" == 1 ]]; then
        test_env_validate_suite_proxy_snapshot || {
            test_env_error "suite proxy state changed before service probes"
            return 1
        }
        # The snapshot binds both authorities to its direct host-IP endpoint.
        pg_dial_address="$pg_host"
        redis_dial_address="$redis_host"
    elif [[ "${WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES:-0}" == "1" ]]; then
        test_env_pin_unmanaged_service postgres "$pg_host" || return $?
        pg_dial_address="$TEST_ENV_DIAL_ADDRESS"
        test_env_pin_unmanaged_service redis "$redis_host" || return $?
        redis_dial_address="$TEST_ENV_DIAL_ADDRESS"
    else
        test_env_validate_launcher "$pg_host" "$pg_port" "$redis_host" "$redis_port" || return $?
        test_env_require_managed_resolution "$pg_host" "$pg_port" "$redis_host" "$redis_port" || return $?
        pg_dial_address="$TEST_ENV_DIAL_ADDRESS"
        redis_dial_address="$TEST_ENV_DIAL_ADDRESS"
    fi
    test_env_refuse_loopback_tunnel_endpoint postgres "$pg_dial_address" "$pg_port" || return $?
    test_env_refuse_loopback_tunnel_endpoint redis "$redis_dial_address" "$redis_port" || return $?
    test_env_probe_service postgres "$pg_dial_address" "$pg_port" || return $?
    test_env_probe_service redis "$redis_dial_address" "$redis_port" || return $?
    if [[ "$TEST_ENV_SUITE_PROXY_MODE" == 1 ]]; then
        test_env_validate_suite_proxy_snapshot || {
            test_env_error "suite proxy state changed during service probes"
            return 1
        }
    fi

    # The Go clients refuse every PostgreSQL and Redis dial outside these
    # endpoints for the rest of the run (test_env_dial_guard.go).
    test_env_format_endpoint "$pg_dial_address" "$pg_port"
    pg_endpoint="$TEST_ENV_ENDPOINT"
    test_env_format_endpoint "$redis_dial_address" "$redis_port"
    export WARP_TEST_ENV_ATTESTED_ADDRS="$pg_endpoint,$TEST_ENV_ENDPOINT"
}

# Names the vault selected by the launcher's own rules, whose PostgreSQL and
# Redis resource authorities the preflight just checked against the launcher
# attestation, for the Go test vault guard (env_test_vault.go). Only the
# managed-local mode has a launcher; under the suite proxy or the portable
# services a test reads its vault only if that vault is a fixture root.
test_env_export_launcher_vault() {
    if [[ "$TEST_ENV_SUITE_PROXY_MODE" == 1 ||
          "${WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES:-0}" == 1 ]]; then
        return 0
    fi
    export WARP_TEST_ENV_LAUNCHER_VAULT_HOME="$WARP_VAULT_HOME"
}

test_env_main() {
    # A failed preflight must not leave an earlier attestation exported.
    unset WARP_TEST_ENV_ATTESTED_ADDRS
    unset WARP_TEST_ENV_LAUNCHER_VAULT_HOME
    test_env_configure || return $?
    test_env_preflight || return $?
    test_env_export_launcher_vault
}

test_env_main
test_env_status=$?
if [[ $test_env_status -ne 0 ]]; then
    if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
        exit "$test_env_status"
    fi
    return "$test_env_status"
fi
