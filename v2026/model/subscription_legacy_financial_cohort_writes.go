// Already-owned cohort rows share statement execution without merging their
// reports, usage evidence, payout rounding, durable identities or replay guards.
package model

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// The owner already holds these logical rows. Fresh statement-local tuple
// addresses bound the target scan; logical keys and exact results retain custody.
const legacyFinancialCohortOutcomeSql = `WITH bounded_owned AS MATERIALIZED (
    SELECT requested.*,picked.row_tid
    FROM unnest($1::uuid[],$2::text[],$3::text[],$4::boolean[])
        AS requested(contract_id,outcome,provider_usage,clear_dispute)
    CROSS JOIN LATERAL (
        SELECT point_contract.ctid AS row_tid FROM transfer_contract AS point_contract
        WHERE point_contract.contract_id=requested.contract_id LIMIT 1 OFFSET 0
    ) AS picked
)
    UPDATE transfer_contract SET outcome=owned.outcome,
    close_time=clock_timestamp() AT TIME ZONE 'UTC',provider_usage=owned.provider_usage::jsonb,
    dispute=CASE WHEN owned.clear_dispute THEN false ELSE transfer_contract.dispute END
    FROM bounded_owned AS owned
    WHERE transfer_contract.contract_id=owned.contract_id
        AND transfer_contract.ctid=ANY(ARRAY(SELECT row_tid FROM bounded_owned))
        AND CASE WHEN transfer_contract.outcome IS NULL THEN true ELSE false END
    RETURNING transfer_contract.contract_id,transfer_contract.close_time`

// Every returned identity must match exactly one admitted owner. The two
// statement callbacks finish before metadata, debit, cache or output writes.
func queueLegacyFinancialCohortOutcomes(batch *pgx.Batch, contracts []*legacyFinancialCohortContract) error {
	ids := make([]server.Id, len(contracts))
	outcomes := make([]string, len(contracts))
	usages := make([]*string, len(contracts))
	clearDisputes := make([]bool, len(contracts))
	byId := map[server.Id]*legacyFinancialCohortContract{}
	for index, contract := range contracts {
		if byId[contract.contractId] != nil {
			return fmt.Errorf("cohort repeats an outcome owner")
		}
		byId[contract.contractId] = contract
		ids[index], outcomes[index], clearDisputes[index] = contract.contractId, string(contract.outcome), contract.clearDispute
		if contract.usage != nil {
			raw, err := json.Marshal(contract.usage)
			if err != nil {
				return err
			}
			value := string(raw)
			usages[index] = &value
		}
	}
	batch.Queue(`DELETE FROM legacy_settlement_intent WHERE contract_id=ANY($1)`, ids).Exec(func(tag pgconn.CommandTag) error {
		if tag.RowsAffected() != int64(len(ids)) {
			return fmt.Errorf("cohort lost intent ownership")
		}
		return nil
	})
	batch.Queue(legacyFinancialCohortOutcomeSql, ids, outcomes, usages, clearDisputes).Query(func(rows pgx.Rows) error {
		seen := map[server.Id]bool{}
		for rows.Next() {
			var id server.Id
			var closedAt time.Time
			if err := rows.Scan(&id, &closedAt); err != nil {
				return err
			}
			contract := byId[id]
			if contract == nil || seen[id] {
				return fmt.Errorf("cohort returned an unexpected outcome owner")
			}
			seen[id] = true
			contract.closedAt = closedAt
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(seen) != len(contracts) {
			return fmt.Errorf("cohort lost an outcome owner")
		}
		return nil
	})
	return nil
}

const legacyFinancialCohortMetadataSql = `WITH bounded_owned AS MATERIALIZED (
    SELECT requested.*,picked.row_tid
    FROM unnest($1::uuid[],$2::uuid[],$3::bigint[]) AS requested(contract_id,balance_id,byte_count)
    CROSS JOIN LATERAL (
        SELECT point_escrow.ctid AS row_tid FROM transfer_escrow AS point_escrow
        WHERE point_escrow.contract_id=requested.contract_id
            AND point_escrow.balance_id=requested.balance_id LIMIT 1 OFFSET 0
    ) AS picked
)
    UPDATE transfer_escrow AS escrow
    SET settled=true,settle_time=$4,payout_byte_count=owned.byte_count
    FROM bounded_owned AS owned
    WHERE escrow.contract_id=owned.contract_id AND escrow.balance_id=owned.balance_id
        AND escrow.ctid=ANY(ARRAY(SELECT row_tid FROM bounded_owned))`

// A single statement advances each distinct affected grant revision once. It
// follows the completed outcome statement, whose trigger saw unsettled rows.
func queueLegacyFinancialCohortMetadata(batch *pgx.Batch, contracts []*legacyFinancialCohortContract) {
	var ids, balances []server.Id
	var byteCounts []ByteCount
	for _, contract := range contracts {
		balanceIds := make([]server.Id, 0, len(contract.sweepPayouts))
		for id := range contract.sweepPayouts {
			balanceIds = append(balanceIds, id)
		}
		slices.SortFunc(balanceIds, server.Id.Cmp)
		for _, id := range balanceIds {
			ids = append(ids, contract.contractId)
			balances = append(balances, id)
			byteCounts = append(byteCounts, contract.sweepPayouts[id].payoutByteCount)
		}
	}
	batch.Queue(legacyFinancialCohortMetadataSql, ids, balances, byteCounts, server.NowUtc()).Exec(func(tag pgconn.CommandTag) error {
		if tag.RowsAffected() != int64(len(ids)) {
			return fmt.Errorf("cohort lost exact escrow metadata ownership")
		}
		return nil
	})
}

// Per-contract allocations were already rounded independently. Preserve their
// first insertion clock on conflict, including exact provider attribution JSON.
const legacyFinancialCohortSweepSql = `INSERT INTO transfer_escrow_sweep
    (contract_id,balance_id,network_id,payout_byte_count,payout_net_revenue_nano_cents,destination_id,provider_payouts,sweep_time)
    SELECT owned.contract_id,owned.balance_id,owned.network_id,owned.byte_count,owned.revenue,
        owned.destination_id,owned.provider_payouts::jsonb,now() AT TIME ZONE 'UTC'
    FROM unnest($1::uuid[],$2::uuid[],$3::uuid[],$4::bigint[],$5::bigint[],$6::uuid[],$7::text[])
        AS owned(contract_id,balance_id,network_id,byte_count,revenue,destination_id,provider_payouts)
    ON CONFLICT (contract_id,balance_id,network_id) DO UPDATE SET
        payout_byte_count=EXCLUDED.payout_byte_count,
        payout_net_revenue_nano_cents=EXCLUDED.payout_net_revenue_nano_cents,
        destination_id=EXCLUDED.destination_id,provider_payouts=EXCLUDED.provider_payouts`

func queueLegacyFinancialCohortPayouts(batch *pgx.Batch, contracts []*legacyFinancialCohortContract) error {
	var ids, balances, networks, destinations []server.Id
	var byteCounts []ByteCount
	var revenues []NanoCents
	var providers []*string
	for _, contract := range contracts {
		keys := make([]participantSweepKey, 0, len(contract.participantPayouts))
		for key := range contract.participantPayouts {
			keys = append(keys, key)
		}
		slices.SortFunc(keys, func(a, b participantSweepKey) int {
			if cmp := a.balanceId.Cmp(b.balanceId); cmp != 0 {
				return cmp
			}
			return a.networkId.Cmp(b.networkId)
		})
		for _, key := range keys {
			payout := contract.participantPayouts[key]
			var providerJson *string
			if payout.providerPayouts != nil {
				raw, err := json.Marshal(payout.providerPayouts)
				if err != nil {
					return err
				}
				value := string(raw)
				providerJson = &value
			}
			ids = append(ids, contract.contractId)
			balances = append(balances, key.balanceId)
			networks = append(networks, key.networkId)
			destinations = append(destinations, payout.destinationId)
			byteCounts = append(byteCounts, payout.payoutByteCount)
			revenues = append(revenues, payout.payout)
			providers = append(providers, providerJson)
		}
	}
	if len(ids) > 0 {
		batch.Queue(legacyFinancialCohortSweepSql, ids, balances, networks, byteCounts, revenues, destinations, providers)
	}
	return nil
}
