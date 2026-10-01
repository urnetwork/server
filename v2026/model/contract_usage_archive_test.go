// Real billing retention must preserve completed provider usage independently
// of the mutable contract, payment, close-report and membership tables.
package model

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Complete the real local payment lifecycle and reap its contracts. The epoch
// must retain exactly the same usage alongside an unreaped neighboring row.
func TestStContractUsageArchiveSurvivesRealPaidContractReaper(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		start := server.NowUtc()
		_, _, _, _, paidContractIds := testingSettledPayoutContracts(ctx, t)
		addStContractUsageSnapshotTestRow(t, ctx, start, &contractUsageSnapshot{Version: 1, ByteCount: 73,
			Providers: []contractProviderUsage{{ClientId: server.NewId(), NetworkId: server.NewId(), ByteCount: 73}}})
		before, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
		if err != nil || len(before) < 2 {
			t.Fatalf("real paid fixture has no independent usage: %+v, %v", before, err)
		}
		beforeWire, err := json.Marshal(before)
		server.Raise(err)
		plan, err := PlanPayments(ctx)
		server.Raise(err)
		if len(plan.NetworkPayments) == 0 {
			t.Fatal("paid fixture did not create a payment plan")
		}
		for _, payment := range plan.NetworkPayments {
			SetPaymentRecord(ctx, payment.PaymentId, "usdc", NanoCentsToUsd(payment.Payout), "")
			CompletePayment(ctx, payment.PaymentId, "", "synthetic-settlement")
		}
		RemoveCompletedContracts(ctx, server.NowUtc().Add(time.Hour))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET reap_time=$2 WHERE contract_id=ANY($1::uuid[])`, paidContractIds, start.Add(-time.Hour)))
		})
		RemoveCompletedContracts(ctx, server.NowUtc().Add(time.Hour))
		for _, contractId := range paidContractIds {
			contractCount, closeCount, escrowCount, sweepCount := testingCountContractRows(ctx, contractId)
			if contractCount != 0 || closeCount != 0 || escrowCount != 0 || sweepCount != 0 {
				t.Fatalf("real billing reaper left its source rows: %d/%d/%d/%d", contractCount, closeCount, escrowCount, sweepCount)
			}
		}
		after, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		afterWire, err := json.Marshal(after)
		server.Raise(err)
		if !bytes.Equal(beforeWire, afterWire) {
			t.Fatalf("billing retention changed completed usage: before=%s after=%s", beforeWire, afterWire)
		}
		RemoveCompletedContracts(ctx, server.NowUtc().Add(time.Hour))
		replay, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour))
		server.Raise(err)
		replayWire, err := json.Marshal(replay)
		server.Raise(err)
		if !bytes.Equal(beforeWire, replayWire) {
			t.Fatalf("replayed retention duplicated or lost usage: %s", replayWire)
		}
	})
}

// Both an interrupted transaction and a refused archive insert retain the
// live proof. Only one successful commit moves its exact bytes to the archive.
func TestStContractUsageArchiveCaptureRollbackAndRetry(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		closedAt := server.NowUtc()
		id := addStContractUsageSnapshotTestRow(t, ctx, closedAt, &contractUsageSnapshot{Version: 1, ByteCount: 121,
			Providers: []contractProviderUsage{{ClientId: server.NewId(), NetworkId: server.NewId(), ByteCount: 121}}})
		server.Db(ctx, func(conn server.PgConn) {
			var original []byte
			server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, id).Scan(&original))
			tx, err := conn.Begin(ctx)
			server.Raise(err)
			defer tx.Rollback(ctx)
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, id))
			var inside []byte
			server.Raise(tx.QueryRow(ctx, `SELECT provider_usage FROM st_provider_usage_archive WHERE contract_id=$1`, id).Scan(&inside))
			if !bytes.Equal(original, inside) {
				t.Fatalf("capture changed original proof: %s -> %s", original, inside)
			}
			visible, err := GetStEpochProviderUsage(ctx, closedAt, closedAt.Add(time.Hour))
			if err != nil || len(visible) != 1 || visible[0].PayoutByteCount != 121 {
				t.Fatalf("uncommitted deletion exposed a gap or duplicate to a concurrent reader: %+v, %v", visible, err)
			}
			server.Raise(tx.Rollback(ctx))
			assertCensus := func(live, archived int) {
				t.Helper()
				var actualLive, actualArchived int
				server.Raise(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM transfer_contract WHERE contract_id=$1),
					(SELECT count(*) FROM st_provider_usage_archive WHERE contract_id=$1)`, id).Scan(&actualLive, &actualArchived))
				if actualLive != live || actualArchived != archived {
					t.Fatalf("usage capture census live/archive=%d/%d, want %d/%d", actualLive, actualArchived, live, archived)
				}
			}
			assertCensus(1, 0)
			server.RaisePgResult(conn.Exec(ctx, `CREATE FUNCTION synthetic_archive_failure() RETURNS trigger LANGUAGE plpgsql AS
				'BEGIN RAISE EXCEPTION ''synthetic archive unavailable''; END';
				CREATE TRIGGER synthetic_archive_failure BEFORE INSERT ON st_provider_usage_archive
				FOR EACH ROW EXECUTE FUNCTION synthetic_archive_failure()`))
			if _, err := conn.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, id); err == nil {
				t.Fatal("capture failure allowed deletion of the live proof")
			}
			assertCensus(1, 0)
			server.RaisePgResult(conn.Exec(ctx, `DROP TRIGGER synthetic_archive_failure ON st_provider_usage_archive;
				DROP FUNCTION synthetic_archive_failure()`))
			server.RaisePgResult(conn.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, id))
			assertCensus(0, 1)
			var retained []byte
			var retainedTime time.Time
			var outcome string
			server.Raise(conn.QueryRow(ctx, `SELECT provider_usage,close_time,outcome FROM st_provider_usage_archive WHERE contract_id=$1`, id).Scan(&retained, &retainedTime, &outcome))
			if !bytes.Equal(original, retained) || !closedAt.Equal(retainedTime) || outcome != "settled" {
				t.Fatalf("committed archive changed usage or its epoch owner: %s, %v, %s", retained, retainedTime, outcome)
			}
		})
	})
}

// A reused identity must fail the whole epoch, never count twice or replace
// the first proof. Both row mutations and bulk deletion are refused.
func TestStContractUsageArchiveRejectsRewriteAndDuplicateIdentity(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		closedAt := server.NowUtc()
		id := addStContractUsageSnapshotTestRow(t, ctx, closedAt, &contractUsageSnapshot{Version: 1, ByteCount: 121,
			Providers: []contractProviderUsage{{ClientId: server.NewId(), NetworkId: server.NewId(), ByteCount: 121}}})
		server.Db(ctx, func(conn server.PgConn) {
			if _, err := conn.Exec(ctx, `INSERT INTO st_provider_usage_archive SELECT contract_id,outcome,close_time,jsonb_set(provider_usage,'{byte_count}','999') FROM transfer_contract WHERE contract_id=$1`, id); err == nil {
				t.Fatal("archive accepted a different source proof")
			}
			server.RaisePgResult(conn.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, id))
			for _, mutation := range []string{
				`UPDATE st_provider_usage_archive SET provider_usage=NULL`,
				`UPDATE st_provider_usage_archive SET close_time=close_time+interval '1 hour'`,
				`DELETE FROM st_provider_usage_archive`,
				`TRUNCATE st_provider_usage_archive`,
				`TRUNCATE transfer_contract`,
			} {
				if _, err := conn.Exec(ctx, mutation); err == nil {
					t.Fatalf("archive permitted history rewrite: %s", mutation)
				}
			}
			// A single statement reader detects both copies under one snapshot;
			// even a buggy producer reusing an old identity cannot double credit it.
			server.RaisePgResult(conn.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_id,source_network_id,destination_id,destination_network_id,transfer_byte_count,usage_origin_is_source,outcome,close_time,provider_usage)
				SELECT contract_id,$2,$3,$4,$5,1000000,true,outcome,close_time,provider_usage FROM st_provider_usage_archive WHERE contract_id=$1`,
				id, server.NewId(), server.NewId(), server.NewId(), server.NewId()))
			if _, err := conn.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, id); err == nil {
				t.Fatal("duplicate identity replaced archived proof during cleanup")
			}
		})
		if usage, err := GetStEpochProviderUsage(ctx, closedAt, closedAt.Add(time.Hour)); err == nil || usage != nil {
			t.Fatalf("live and archived copies received duplicate credit: %+v, %v", usage, err)
		}
	})
}

// Migration does not forgive missing historical proof. Cancellations remain
// excluded, and the same half-open boundary applies to archived positive usage.
func TestStContractUsageArchiveRetainsHistoricalNullAndExactWindow(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrationsUpTo(ctx, 744)
		start := time.Unix(1_700_000_000, 0).UTC()
		addStContractUsageSnapshotTestRow(t, ctx, start, nil)
		addStContractUsageSnapshotTestOutcome(t, ctx, start.Add(time.Hour), nil, "canceled")
		addStContractUsageSnapshotTestRow(t, ctx, start.Add(time.Hour), &contractUsageSnapshot{Version: 1, ByteCount: 121,
			Providers: []contractProviderUsage{{ClientId: server.NewId(), NetworkId: server.NewId(), ByteCount: 121}}})
		server.ApplyDbMigrations(ctx)
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `DELETE FROM transfer_contract`))
			var missing, total int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FILTER (WHERE provider_usage IS NULL),count(*) FROM st_provider_usage_archive`).Scan(&missing, &total))
			if missing != 1 || total != 2 {
				t.Fatalf("archive relabeled historical missing or canceled work: %d/%d", missing, total)
			}
		})
		if usage, err := GetStEpochProviderUsage(ctx, start, start.Add(time.Hour)); err == nil || usage != nil {
			t.Fatalf("archived missing proof was silently omitted: %+v, %v", usage, err)
		}
		usage, err := GetStEpochProviderUsage(ctx, start.Add(time.Hour), start.Add(2*time.Hour))
		if err != nil || len(usage) != 1 || usage[0].PayoutByteCount != 121 {
			t.Fatalf("archive changed half-open or cancellation accounting: %+v, %v", usage, err)
		}
	})
}

// There is no evidence from which to rebuild rows deleted before this append.
// The migration must not manufacture coverage for that historical interval.
func TestStContractUsageArchiveDoesNotBackfillAlreadyDeletedHistory(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.ApplyDbMigrations = false
	testEnv.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.ApplyDbMigrationsUpTo(ctx, 745)
		start := time.Unix(1_700_000_000, 0).UTC()
		id := addStContractUsageSnapshotTestRow(t, ctx, start, &contractUsageSnapshot{Version: 1, ByteCount: 121,
			Providers: []contractProviderUsage{{ClientId: server.NewId(), NetworkId: server.NewId(), ByteCount: 121}}})
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `DELETE FROM transfer_contract WHERE contract_id=$1`, id))
		})
		server.ApplyDbMigrations(ctx)
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM st_provider_usage_archive`).Scan(&count))
			if count != 0 {
				t.Fatal("archive migration invented already deleted history")
			}
		})
	})
}
