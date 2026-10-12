package server

// client_reliability_running_job stages one lookback's checkpointed running
// window step (model/network_client_reliability_running_stage.go): the applied
// window it advances from, the window it moves to, the degraded blocks of each
// range, and a block cursor over the entering (or whole) and leaving ranges.
// client_reliability_running_partial holds the exact per-client partial sums
// of the committed block chunks until the step is applied. Partials are
// updated in place once per chunk, so a lower fillfactor keeps those updates
// on their page.
const reliabilityRunningStageSchemaSql = `
CREATE TABLE client_reliability_running_job (
    lookback_index int NOT NULL,
    recompute bool NOT NULL,
    prev_exists bool NOT NULL,
    prev_min_block_number bigint NOT NULL,
    prev_max_block_number bigint NOT NULL,
    prev_last_recompute_block bigint NOT NULL,
    prev_degraded_classification_version smallint NOT NULL,
    prev_degraded_classification_write_token uuid NULL,
    prev_observation_version smallint NOT NULL,
    prev_observation_write_token uuid NULL,
    min_block_number bigint NOT NULL,
    max_block_number bigint NOT NULL,
    last_recompute_block bigint NOT NULL,
    enter_min_block_number bigint NOT NULL,
    enter_max_block_number bigint NOT NULL,
    enter_next_block_number bigint NOT NULL,
    enter_degraded_block_numbers bigint[] NOT NULL,
    leave_min_block_number bigint NOT NULL,
    leave_max_block_number bigint NOT NULL,
    leave_next_block_number bigint NOT NULL,
    leave_degraded_block_numbers bigint[] NOT NULL,
    create_time timestamp NOT NULL,
    PRIMARY KEY (lookback_index)
);

CREATE TABLE client_reliability_running_partial (
    lookback_index int NOT NULL,
    leaving bool NOT NULL,
    client_id uuid NOT NULL,
    network_id uuid NOT NULL,
    valid_row_count bigint NOT NULL,
    reliability_sum numeric NOT NULL,
    observed_row_count bigint NOT NULL,
    PRIMARY KEY (lookback_index, leaving, client_id, network_id),
    FOREIGN KEY (lookback_index)
        REFERENCES client_reliability_running_job (lookback_index) ON DELETE CASCADE
) WITH (fillfactor = 50);
`
