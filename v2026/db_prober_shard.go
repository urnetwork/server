package server

// Ownership outlives the disposable account. Closed rows fence replayed task
// epochs and retain the identity of the balance's net-escrow tombstone.
const proberShardSchemaSql = `
CREATE TABLE prober_shard_run (
    task_id uuid NOT NULL,
    epoch uuid NOT NULL,
    shard_index int NOT NULL CHECK (shard_index >= 0 AND shard_index < 256),
    shard_count int NOT NULL CHECK (shard_count > shard_index AND shard_count <= 256),
    network_id uuid NOT NULL UNIQUE,
    user_id uuid NOT NULL UNIQUE,
    client_id uuid NOT NULL UNIQUE,
    device_id uuid NOT NULL UNIQUE,
    balance_id uuid NOT NULL UNIQUE,
    state text NOT NULL CHECK (state IN ('active', 'draining', 'deleted', 'closed')),
    retired_client_ids uuid[] NOT NULL DEFAULT '{}',
    create_time timestamp NOT NULL,
    deadline timestamp NOT NULL,
    next_cleanup_time timestamp NOT NULL,
    close_time timestamp,
    PRIMARY KEY (task_id, epoch),
    CHECK ((state IN ('deleted', 'closed')) = (close_time IS NOT NULL))
);
CREATE UNIQUE INDEX prober_shard_run_active_slot ON prober_shard_run(shard_index) WHERE state = 'active';
CREATE INDEX prober_shard_run_cleanup ON prober_shard_run(next_cleanup_time) WHERE state <> 'closed';
`
