// Settlement readers share immutable per-client allocation validation. Account
// payments stay network aggregates; no current membership can rewrite a share.
package model

// Appends to a caller's payout_sweeps CTE, already scoped by network and time
// as appropriate. Every returned provider row carries full-sweep validity;
// consumers must reject any invalid row before selecting individual clients.
// Legacy attribution requires immutable non-stream contract endpoints, and
// modern allocations must conserve both totals over the entire sweep key.
// The unique contract lookup stays inside a guarded lateral limit so modern
// rows never probe contract history or pull it into a network-wide hash join.
const contractProviderPayoutRowsSql = `
    , allocated_sweeps AS (
        SELECT sweep.*,
            CASE WHEN sweep.provider_payouts IS NOT NULL THEN sweep.provider_payouts
            WHEN legacy.provider_count = 1 AND legacy.client_id = sweep.destination_id
            THEN jsonb_build_array(jsonb_build_object(
                'client_id', sweep.destination_id,
                'payout_byte_count', sweep.payout_byte_count,
                'payout_nano_cents', sweep.payout_net_revenue_nano_cents
            )) ELSE '[]'::jsonb END AS allocations
        FROM payout_sweeps sweep
        LEFT JOIN LATERAL (
            SELECT * FROM transfer_contract
            WHERE sweep.provider_payouts IS NULL AND contract_id = sweep.contract_id
                AND stream_id IS NULL
            LIMIT 1
        ) contract ON true
        LEFT JOIN LATERAL (
            SELECT
                CASE WHEN contract.payer_network_id IS NULL OR
                    contract.source_network_id = contract.destination_network_id
                    THEN contract.companion_contract_id IS NULL
                    ELSE contract.payer_network_id = contract.source_network_id
                END AS origin_is_source,
                COALESCE(contract.payer_network_id,
                    CASE WHEN contract.companion_contract_id IS NULL
                        THEN contract.source_network_id ELSE contract.destination_network_id END
                ) AS origin_network_id
        ) direction ON sweep.provider_payouts IS NULL
        LEFT JOIN LATERAL (
            SELECT count(*) AS provider_count, MIN(provider.client_id::text)::uuid AS client_id
            FROM (
                SELECT CASE WHEN direction.origin_is_source
                    THEN contract.destination_id ELSE contract.source_id END AS client_id
                WHERE sweep.network_id = CASE WHEN direction.origin_is_source
                    THEN contract.destination_network_id ELSE contract.source_network_id END
                    AND contract.source_id <> contract.destination_id
            ) provider
            WHERE sweep.network_id <> direction.origin_network_id
                AND (contract.payer_network_id IS NULL OR contract.payer_network_id
                    IN (contract.source_network_id, contract.destination_network_id))
        ) legacy ON sweep.provider_payouts IS NULL
    ), provider_amounts AS (
        SELECT sweep.contract_id, sweep.balance_id, sweep.network_id, sweep.sweep_time,
            sweep.payout_byte_count AS network_bytes,
            sweep.payout_net_revenue_nano_cents AS network_revenue,
            provider.client_id, provider.payout_byte_count, provider.payout_nano_cents,
            SUM(provider.payout_byte_count) OVER network_sweep AS allocated_bytes,
            SUM(provider.payout_nano_cents) OVER network_sweep AS allocated_revenue,
            count(*) OVER (PARTITION BY sweep.contract_id, sweep.balance_id,
                sweep.network_id, provider.client_id) AS client_count
        FROM allocated_sweeps sweep
        LEFT JOIN LATERAL jsonb_to_recordset(sweep.allocations) AS provider(
            client_id uuid, payout_byte_count bigint, payout_nano_cents bigint
        ) ON true
        WINDOW network_sweep AS (PARTITION BY sweep.contract_id, sweep.balance_id, sweep.network_id)
    ), provider_rows AS (
        SELECT client_id, network_id, sweep_time, payout_byte_count, payout_nano_cents,
            COALESCE(client_id <> '00000000-0000-0000-0000-000000000000'::uuid
                AND payout_byte_count >= 0 AND payout_nano_cents >= 0 AND client_count = 1
                AND allocated_bytes = network_bytes AND allocated_revenue = network_revenue,
                false) AS valid
        FROM provider_amounts
    )
`
