// Close-page report reads stay keyed to the selected raw contract page.
package model

// The raw page retains its cursor and pending-intent semantics. Missing
// deadlines expire 60 minutes after creation, regardless of recent checkpoints.
// Each report is unique by (contract_id, party). LATERAL LIMIT 1 prevents the
// planner from scanning retained global report history for a bounded page;
// absent reports still produce the same nullable LEFT JOIN fields.
const forceCloseOpenContractPageSql = `
                WITH bounded AS MATERIALIZED (
                    SELECT contract_id,source_id,destination_id,dispute,create_time,usage_unverified,expiration_time
                    FROM transfer_contract
                    WHERE open AND (create_time,contract_id)>($5,$6) AND create_time <= $7
                    ORDER BY create_time,contract_id LIMIT $4
                )
                SELECT t.contract_id,t.source_id,t.destination_id,t.dispute,
                    source_contract_close.close_time,source_contract_close.used_transfer_byte_count,source_contract_close.checkpoint,
                    destination_contract_close.close_time,destination_contract_close.used_transfer_byte_count,destination_contract_close.checkpoint,
                    t.create_time,
                    COALESCE(t.expiration_time <= statement_timestamp() AT TIME ZONE 'UTC',false) OR
                    NOT COALESCE((SELECT true FROM legacy_settlement_intent pending WHERE pending.contract_id=t.contract_id),false)
                    AND (t.usage_unverified OR COALESCE(t.expiration_time, t.create_time + interval '60 minutes') <= statement_timestamp() AT TIME ZONE 'UTC' OR (t.create_time <= $3
                        AND NOT EXISTS(SELECT 1 FROM contract_close recent_close WHERE recent_close.contract_id=t.contract_id AND recent_close.close_time > $3))),
                    CASE WHEN COALESCE(t.expiration_time, t.create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC'
                        THEN COALESCE(t.expiration_time, t.create_time + interval '60 minutes') END AS next_expiration_time
                FROM bounded t
                LEFT JOIN LATERAL (SELECT close_time,used_transfer_byte_count,checkpoint FROM contract_close
                    WHERE contract_id=t.contract_id AND party=$1 LIMIT 1) source_contract_close ON true
                LEFT JOIN LATERAL (SELECT close_time,used_transfer_byte_count,checkpoint FROM contract_close
                    WHERE contract_id=t.contract_id AND party=$2 LIMIT 1) destination_contract_close ON true
                ORDER BY t.create_time,t.contract_id
			`

// The disputed page already probes report recency by contract. Keep its exact
// selection separate from the open-page source/destination report projection.
const forceCloseDisputedContractPageSql = `
                WITH bounded AS MATERIALIZED (
                    SELECT contract_id,source_id,destination_id,create_time,usage_unverified,expiration_time
                    FROM transfer_contract
                    WHERE dispute AND outcome IS NULL AND (create_time,contract_id)>($3,$4) AND create_time <= $5
                    ORDER BY create_time,contract_id LIMIT $2
                )
                SELECT t.contract_id,t.source_id,t.destination_id,t.create_time,
                    COALESCE(t.expiration_time <= statement_timestamp() AT TIME ZONE 'UTC',false) OR
                    NOT COALESCE((SELECT true FROM legacy_settlement_intent pending WHERE pending.contract_id=t.contract_id),false)
                    AND (t.usage_unverified OR COALESCE(t.expiration_time, t.create_time + interval '60 minutes') <= statement_timestamp() AT TIME ZONE 'UTC' OR (t.create_time <= $1
                        AND NOT EXISTS(SELECT 1 FROM contract_close recent_close WHERE recent_close.contract_id=t.contract_id AND recent_close.close_time > $1))),
                    CASE WHEN COALESCE(t.expiration_time, t.create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC'
                        THEN COALESCE(t.expiration_time, t.create_time + interval '60 minutes') END AS next_expiration_time
                FROM bounded t ORDER BY t.create_time,t.contract_id
			`
