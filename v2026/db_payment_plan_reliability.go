package server

// payment_plan_reliability_window accumulates the network reliability scores
// of one payout window in committed block chunks, before the plan transaction
// reads them. Partials are keyed by client, so the final fold joins client
// locations once, exactly like the single statement it replaces.
const paymentPlanReliabilitySchemaSql = `
CREATE TABLE payment_plan_reliability_window (
    min_block_number bigint NOT NULL,
    max_block_number bigint NOT NULL,
    block_shift bigint NOT NULL,
    effective_block_count bigint NOT NULL,
    degraded_block_numbers bigint[] NOT NULL,
    next_block_number bigint NOT NULL,
    create_time timestamp NOT NULL,
    complete_time timestamp NULL,
    PRIMARY KEY (min_block_number, max_block_number)
);

CREATE TABLE payment_plan_reliability_partial (
    min_block_number bigint NOT NULL,
    max_block_number bigint NOT NULL,
    network_id uuid NOT NULL,
    client_id uuid NOT NULL,
    valid_block_count bigint NOT NULL,
    reliability_sum numeric NOT NULL,
    PRIMARY KEY (min_block_number, max_block_number, network_id, client_id),
    FOREIGN KEY (min_block_number, max_block_number)
        REFERENCES payment_plan_reliability_window (min_block_number, max_block_number) ON DELETE CASCADE
);
`
