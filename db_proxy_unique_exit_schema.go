package server

// The schema of unique-exit hosted proxies (HOSTED-PROXY-UNIQUE-EXIT.md §4,
// model/proxy_unique_exit_model.go). Every statement is additive.

// The egress key is a keyed hash of the address the operator's /ip echo saw
// through the provider's tunnel: the exact IPv4 address, or the IPv6 /64. The
// address itself is still never stored. The stable count is how many
// consecutive probes saw the same key, so a proxy-miner slot behind a rotating
// upstream (a new key every probe) never reaches the two the allocator wants
// from a slot-like provider.
//
// While ADD COLUMN waits for its ACCESS EXCLUSIVE lock, new readers of
// provider_egress_location (find-providers, the reliability reads) queue behind
// it, so the lock timeout bounds that wait: a busy table makes the migration
// give up, and a rerun applies it.
const proxyUniqueExitEgressKeyColumnsSql = `
    SET LOCAL lock_timeout = '2s';
    ALTER TABLE provider_egress_location
        ADD COLUMN IF NOT EXISTS egress_key bytea NULL,
        ADD COLUMN IF NOT EXISTS egress_key_observed_at timestamp NULL,
        ADD COLUMN IF NOT EXISTS egress_key_stable_count integer NOT NULL DEFAULT 0
`

// One row per unique-exit proxy: what the customer asked for (the replace mode
// and the location constraint as provider specs) and the state they see.
const proxyUniqueExitTableSql = `
    CREATE TABLE IF NOT EXISTS proxy_unique_exit (
        proxy_client_id uuid NOT NULL PRIMARY KEY,
        network_id      uuid NOT NULL,
        replace_mode    varchar(8) NOT NULL,
        specs_json      text NOT NULL,
        state           varchar(16) NOT NULL,
        state_reason    varchar(32) NOT NULL DEFAULT '',
        create_time     timestamp NOT NULL,
        update_time     timestamp NOT NULL
    )
`

const proxyUniqueExitNetworkIndexSql = `
    CREATE INDEX IF NOT EXISTS proxy_unique_exit_network_id
        ON proxy_unique_exit (network_id)
`

// One row per exit a unique-exit proxy is pinned to. The assignment id is the
// customer's opaque exit_id. Released rows are the audit trail for an abuse
// report about an exit, kept for UniqueExitSettings.ReleasedRetention.
const proxyExitAssignmentTableSql = `
    CREATE TABLE IF NOT EXISTS proxy_exit_assignment (
        assignment_id              uuid NOT NULL PRIMARY KEY,
        network_id                 uuid NOT NULL,
        proxy_client_id            uuid NOT NULL,
        provider_client_id         uuid NOT NULL,
        egress_key                 bytea NOT NULL,
        state                      varchar(16) NOT NULL,
        location_id                uuid NULL,
        country_code               varchar(2) NOT NULL DEFAULT '',
        estimated_bytes_per_second bigint NOT NULL DEFAULT 0,
        assigned_at                timestamp NOT NULL,
        confirmed_at               timestamp NULL,
        failing_since              timestamp NULL,
        failing_reason             varchar(32) NOT NULL DEFAULT '',
        released_at                timestamp NULL,
        release_reason             varchar(32) NOT NULL DEFAULT ''
    )
`

// Postgres, not the application, guarantees that no two live (pending or
// active) assignments of one network share an egress key.
const proxyExitAssignmentNetworkEgressKeyLiveIndexSql = `
    CREATE UNIQUE INDEX IF NOT EXISTS proxy_exit_assignment_network_egress_key_live
        ON proxy_exit_assignment (network_id, egress_key)
        WHERE state IN ('pending', 'active')
`

// A proxy holds at most one assignment that is not released.
const proxyExitAssignmentProxyLiveIndexSql = `
    CREATE UNIQUE INDEX IF NOT EXISTS proxy_exit_assignment_proxy_client_id_live
        ON proxy_exit_assignment (proxy_client_id)
        WHERE state IN ('pending', 'active', 'replacing')
`

// The pins-per-provider count across networks.
const proxyExitAssignmentProviderLiveIndexSql = `
    CREATE INDEX IF NOT EXISTS proxy_exit_assignment_provider_client_id_live
        ON proxy_exit_assignment (provider_client_id)
        WHERE state IN ('pending', 'active')
`

// The sweeper's walk of every assignment that is not released.
const proxyExitAssignmentAssignedAtLiveIndexSql = `
    CREATE INDEX IF NOT EXISTS proxy_exit_assignment_assigned_at_live
        ON proxy_exit_assignment (assigned_at)
        WHERE state IN ('pending', 'active', 'replacing')
`

// The per-proxy replacement cap and its 24 h provider exclusion.
const proxyExitAssignmentProxyReleasedIndexSql = `
    CREATE INDEX IF NOT EXISTS proxy_exit_assignment_proxy_client_id_released
        ON proxy_exit_assignment (proxy_client_id, released_at)
        WHERE state = 'released'
`

// The per-network replacement cap.
const proxyExitAssignmentNetworkReleasedIndexSql = `
    CREATE INDEX IF NOT EXISTS proxy_exit_assignment_network_id_released
        ON proxy_exit_assignment (network_id, released_at)
        WHERE state = 'released'
`

// The retention delete.
const proxyExitAssignmentReleasedAtIndexSql = `
    CREATE INDEX IF NOT EXISTS proxy_exit_assignment_released_at
        ON proxy_exit_assignment (released_at)
        WHERE state = 'released'
`
