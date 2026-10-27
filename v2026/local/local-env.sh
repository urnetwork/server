#!/usr/bin/env bash
#
# local-env.sh -- attest a local process that is not a test to the running
# launcher, so it can dial the local PostgreSQL and Redis.
#
# Every WARP_ENV=local process dials PostgreSQL and Redis only at the endpoints
# named by WARP_TEST_ENV_ATTESTED_ADDRS, and refuses every dial without them
# (test_env_dial_guard.go). Source this from the bash shell that starts the
# process, after exporting the process's own WARP_* identity; the
# cli/*/run-local.sh, bringyourctl/run-local.sh and
# connect/sim-latency/run-local.sh scripts do:
#
#   source "$server_dir/local/local-env.sh" || exit 1
#
# It runs the same preflight as test-env.sh, which the tests use: it validates
# the launcher's lock, live owner, readiness record and hosts-file block,
# requires each local service name to resolve only to the launcher's dedicated
# address (never to loopback), probes that address, and exports the endpoints.
# Unlike test-env.sh it keeps the caller's WARP_SERVICE, WARP_BLOCK,
# WARP_VERSION and WARP_DOMAIN. It does leave test-env.sh's WARP_VAULT_HOME and
# WARP_CONFIG_HOME exported, so the process reads the vault and config that the
# preflight selected: an explicit value first, then WARP_HOME's, a sibling
# checkout's, and finally the checked-in testdata. The preflight checks that the
# vault's pg.yml and redis.yml name the launcher's endpoints; nothing proves the
# launcher started from that vault, and credentials from another one fail to
# authenticate. In managed-local mode WARP_TEST_ENV_LAUNCHER_VAULT_HOME stays
# exported too, which only a test binary reads (env_test_vault.go). A failure
# leaves the attestation unset and returns non-zero; a process started anyway
# then refuses every dial.
#
# Start ./local/run-local.sh in its own terminal first and wait for
# 'Local environment is up'. Requires bash 3.2 or newer.

local_env_attest() {
    local local_env_source="${BASH_SOURCE[0]}"
    local local_env_dir
    local local_env_server_dir
    local local_env_status
    local local_env_name
    local local_env_identity_names=(WARP_SERVICE WARP_BLOCK WARP_VERSION WARP_DOMAIN)
    local local_env_identity_set=()
    local local_env_identity_values=()
    local local_env_index

    if [[ "$local_env_source" == */* ]]; then
        local_env_dir="${local_env_source%/*}"
    else
        local_env_dir="."
    fi
    local_env_server_dir="$(cd -- "$local_env_dir/.." >/dev/null 2>&1 && pwd)" || {
        printf 'local environment: cannot resolve the server directory\n' >&2
        return 1
    }

    # test-env.sh exports the test identity; the caller's own is restored below.
    for local_env_name in "${local_env_identity_names[@]}"; do
        if [[ -n "${!local_env_name+set}" ]]; then
            local_env_identity_set+=(1)
            local_env_identity_values+=("${!local_env_name}")
        else
            local_env_identity_set+=(0)
            local_env_identity_values+=("")
        fi
    done

    # A failed preflight returns non-zero with the attestation already unset.
    local_env_status=0
    source "$local_env_server_dir/test-env.sh" || local_env_status=$?

    for local_env_index in "${!local_env_identity_names[@]}"; do
        local_env_name="${local_env_identity_names[$local_env_index]}"
        if [[ "${local_env_identity_set[$local_env_index]}" == 1 ]]; then
            export "$local_env_name=${local_env_identity_values[$local_env_index]}"
        else
            unset "$local_env_name"
        fi
    done
    if [[ "$local_env_status" -ne 0 ]]; then
        printf 'local environment: no launcher attestation; this WARP_ENV=local process may not dial PostgreSQL or Redis.\n' >&2
        printf "local environment: start ./local/run-local.sh, wait for 'Local environment is up', then try again.\n" >&2
    fi
    return "$local_env_status"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    printf 'local-env.sh must be sourced: source ./local/local-env.sh\n' >&2
    exit 2
fi
local_env_attest
