// The database boundary protects settlement from older writers and preserves
// the exact usage and epoch attribution already admitted by a newer writer.
package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Seed independent synthetic contract identities without mutable membership.
func insertContractUsageGuardTestRow(ctx context.Context, conn PgConn, contractId Id, outcome *string, closedAt *time.Time, snapshot []byte) error {
	_, err := conn.Exec(ctx, `INSERT INTO transfer_contract
		(contract_id,source_id,source_network_id,destination_id,destination_network_id,
		 transfer_byte_count,usage_origin_is_source,outcome,close_time,provider_usage)
		VALUES ($1,$2,$3,$4,$5,200,true,$6,$7,$8)`, contractId, NewId(), NewId(), NewId(), NewId(), outcome, closedAt, snapshot)
	return err
}

// A valid snapshot is intentionally independent of the production encoder.
func contractUsageGuardTestSnapshot(t testing.TB) []byte {
	t.Helper()
	wire, err := json.Marshal(map[string]any{"version": 1, "byte_count": 120,
		"providers": []any{map[string]any{"client_id": NewId().String(), "network_id": NewId().String(), "byte_count": 120}}})
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

// An old writer must not commit a newly terminal credit outcome without the
// snapshot; no historical or current-direction exception permits missing proof.
func TestContractUsageGuardRejectsMixedVersionSettlement(t *testing.T) {
	testEnv := DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		Db(ctx, func(conn PgConn) {
			for _, outcome := range []string{"settled", "dispute_resolved_to_source", "dispute_resolved_to_destination"} {
				for _, direction := range []any{nil, true, false} {
					contractId := NewId()
					Raise(insertContractUsageGuardTestRow(ctx, conn, contractId, nil, nil, nil))
					RaisePgResult(conn.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=$2 WHERE contract_id=$1`, contractId, direction))
					if _, err := conn.Exec(ctx, `UPDATE transfer_contract SET outcome=$2,close_time=$3 WHERE contract_id=$1`, contractId, outcome, NowUtc()); err == nil {
						t.Errorf("older writer finalized %s with direction %v and no usage snapshot", outcome, direction)
					}
				}
			}
		})
	})
}

// Direct terminal inserts cannot bypass the update boundary. A canceled row
// remains non-credit history and requires no invented usage or close reports.
func TestContractUsageGuardRejectsMissingTerminalInsert(t *testing.T) {
	testEnv := DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		Db(ctx, func(conn PgConn) {
			outcome, closedAt := "settled", NowUtc()
			if err := insertContractUsageGuardTestRow(ctx, conn, NewId(), &outcome, &closedAt, nil); err == nil {
				t.Error("direct insert admitted missing terminal usage")
			}
			outcome = "canceled"
			Raise(insertContractUsageGuardTestRow(ctx, conn, NewId(), &outcome, &closedAt, nil))
		})
	})
}

// Refusing a legacy terminal write rolls back reports in the same transaction;
// a later compatible writer can publish the outcome and proof atomically.
func TestContractUsageGuardRollbackAndCompatibleRetry(t *testing.T) {
	testEnv := DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		Db(ctx, func(conn PgConn) {
			contractId := NewId()
			Raise(insertContractUsageGuardTestRow(ctx, conn, contractId, nil, nil, nil))
			tx, err := conn.Begin(ctx)
			Raise(err)
			defer tx.Rollback(ctx)
			RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close (contract_id,party,used_transfer_byte_count,checkpoint) VALUES ($1,'source',120,false)`, contractId))
			if _, err := tx.Exec(ctx, `UPDATE transfer_contract SET outcome='settled',close_time=$2 WHERE contract_id=$1`, contractId, NowUtc()); err == nil {
				t.Fatal("legacy terminal write did not abort its transaction")
			}
			if err := tx.Commit(ctx); err == nil {
				t.Fatal("refused settlement still committed")
			}
			var reports int
			var outcome *string
			var snapshot []byte
			Raise(conn.QueryRow(ctx, `SELECT outcome,provider_usage,(SELECT count(*) FROM contract_close WHERE contract_id=$1) FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(&outcome, &snapshot, &reports))
			if outcome != nil || len(snapshot) != 0 || reports != 0 {
				t.Fatalf("failed transaction leaked outcome, usage or reports: %v, %s, %d", outcome, snapshot, reports)
			}
			RaisePgResult(conn.Exec(ctx, `UPDATE transfer_contract SET outcome='settled',close_time=$2,provider_usage=$3 WHERE contract_id=$1`, contractId, NowUtc(), contractUsageGuardTestSnapshot(t)))
		})
	})
}

// Retry is idempotent, while changing the snapshot, its contract, its terminal
// outcome or its close time would alter credit or move it to another epoch.
func TestContractUsageGuardRejectsFinalizedReattribution(t *testing.T) {
	testEnv := DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		Db(ctx, func(conn PgConn) {
			outcome, closedAt := "settled", NowUtc()
			snapshot := contractUsageGuardTestSnapshot(t)
			for _, mutation := range []string{
				`provider_usage = NULL`,
				`provider_usage = jsonb_set(provider_usage, '{byte_count}', '121')`,
				`close_time = close_time + interval '1 second'`,
				`outcome = 'canceled'`,
				`outcome = NULL`,
				`contract_id = '00000000-0000-0000-0000-000000000001'`,
			} {
				contractId := NewId()
				Raise(insertContractUsageGuardTestRow(ctx, conn, contractId, &outcome, &closedAt, snapshot))
				RaisePgResult(conn.Exec(ctx, `UPDATE transfer_contract SET provider_usage=$2,outcome=$3,close_time=$4 WHERE contract_id=$1`, contractId, snapshot, outcome, closedAt))
				if _, err := conn.Exec(ctx, `UPDATE transfer_contract SET `+mutation+` WHERE contract_id=$1`, contractId); err == nil {
					t.Errorf("finalized usage permitted reattribution: %s", mutation)
				}
			}
		})
	})
}

// Expiry publishes the original proof before billing settles. That durable
// snapshot must survive later billing edits, not only terminal contract edits.
func TestContractUsageGuardPreservesPreparedExpirySnapshot(t *testing.T) {
	testEnv := DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		Db(ctx, func(conn PgConn) {
			contractId := NewId()
			snapshot := contractUsageGuardTestSnapshot(t)
			Raise(insertContractUsageGuardTestRow(ctx, conn, contractId, nil, nil, snapshot))
			if _, err := conn.Exec(ctx, `UPDATE transfer_contract SET provider_usage=NULL WHERE contract_id=$1`, contractId); err == nil {
				t.Error("an open contract lost its durable expiry proof")
				return
			}
			RaisePgResult(conn.Exec(ctx, `UPDATE transfer_contract SET outcome='settled',close_time=$2 WHERE contract_id=$1`, contractId, NowUtc()))
		})
	})
}

// The append cannot backfill historical credit or rewrite terminal failures.
// The existing explicit repair remains a row-bound, zero-credit debt record.
func TestContractUsageGuardPreservesLegacyNullWithoutBackfill(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		ApplyDbMigrationsUpTo(ctx, 744)
		contractId, outcome, closedAt := NewId(), "settled", time.Unix(1_700_000_000, 0).UTC()
		Db(ctx, func(conn PgConn) {
			Raise(insertContractUsageGuardTestRow(ctx, conn, contractId, &outcome, &closedAt, nil))
		})
		ApplyDbMigrations(ctx)
		Db(ctx, func(conn PgConn) {
			var missing bool
			Raise(conn.QueryRow(ctx, `SELECT provider_usage IS NULL FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(&missing))
			if !missing {
				t.Fatal("migration rewrote historical missing usage")
			}
			if _, err := conn.Exec(ctx, `UPDATE transfer_contract SET provider_usage=$2 WHERE contract_id=$1`, contractId, contractUsageGuardTestSnapshot(t)); err == nil {
				t.Error("historical missing usage was backfilled with positive credit")
				return
			}
			wire, err := json.Marshal(map[string]any{"version": 1, "byte_count": 0, "providers": []any{}, "excluded_reason": "legacy_usage_unavailable",
				"legacy_exclusion": map[string]any{"repair_manifest_sha256": "sha256:" + strings.Repeat("ab", 32), "contract_id": contractId.String(),
					"epoch": 17, "closed_at": closedAt.Format(time.RFC3339Nano), "retained_report_minimum": 120, "final_acceptance": false}})
			Raise(err)
			for _, mutate := range []string{
				`jsonb_set($2::jsonb, '{legacy_exclusion,contract_id}', '"00000000-0000-0000-0000-000000000001"')`,
				`jsonb_set($2::jsonb, '{legacy_exclusion,closed_at}', '"2000-01-01T00:00:00Z"')`,
				`jsonb_set($2::jsonb, '{legacy_exclusion,final_acceptance}', 'true')`,
				`jsonb_set($2::jsonb, '{legacy_exclusion,retained_report_minimum}', '-1')`,
				`jsonb_set($2::jsonb, '{legacy_exclusion,epoch}', '0')`,
				`jsonb_set($2::jsonb, '{byte_count}', '1')`,
			} {
				if _, err := conn.Exec(ctx, `UPDATE transfer_contract SET provider_usage=`+mutate+` WHERE contract_id=$1`, contractId, wire); err == nil {
					t.Fatalf("historical debt accepted altered custody: %s", mutate)
				}
			}
			RaisePgResult(conn.Exec(ctx, `UPDATE transfer_contract SET provider_usage=$2 WHERE contract_id=$1`, contractId, wire))
			if _, err := conn.Exec(ctx, `UPDATE transfer_contract SET provider_usage=$2 WHERE contract_id=$1`, contractId, contractUsageGuardTestSnapshot(t)); err == nil {
				t.Error("explicit zero-credit debt was converted into credit")
			}
		})
	})
}
