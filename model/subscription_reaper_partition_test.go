// The contract partition cutover drops the cascade from transfer_contract to
// contract_close_report, and its weekly partitions prune only by contract id.
// The reaper deletes every dependent row itself, by its contracts' ids.
package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

// Gives each contract every kind of dependent row the reaper deletes: two
// closes, an escrow, a sweep, an extender party and two close reports.
func seedReaperDependentTestRows(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, contractIds []server.Id) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		for _, contractId := range contractIds {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count)
				VALUES($1,'source',0),($1,'destination',0)`, contractId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
				VALUES($1,$2,0)`, contractId, f.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
				(contract_id,balance_id,network_id,payout_byte_count,payout_net_revenue_nano_cents)
				VALUES($1,$2,$3,0,0)`, contractId, f.balanceId, f.destinationNetworkId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_extender(contract_id,extender_id,party,client_id,network_id)
				VALUES($1,$2,'source',$3,$4)`, contractId, server.NewId(), f.sourceId, f.sourceNetworkId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close_report
				(contract_id,party,report_id,used_transfer_byte_count,checkpoint)
				VALUES($1,'source',$2,0,true),($1,'destination',$3,0,false)`, contractId, server.NewId(), server.NewId()))
		}
	}, server.TxReadCommitted, server.OptNoRetry())
}

// Row counts of one contract: contract, close, escrow, sweep, extender party
// and close report.
func countReaperDependentTestRows(t testing.TB, ctx context.Context, contractId server.Id) [6]int {
	t.Helper()
	var counts [6]int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM transfer_contract WHERE contract_id=$1),
			(SELECT count(*) FROM contract_close WHERE contract_id=$1),
			(SELECT count(*) FROM transfer_escrow WHERE contract_id=$1),
			(SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=$1),
			(SELECT count(*) FROM contract_extender WHERE contract_id=$1),
			(SELECT count(*) FROM contract_close_report WHERE contract_id=$1)`, contractId).Scan(
			&counts[0], &counts[1], &counts[2], &counts[3], &counts[4], &counts[5]))
	})
	return counts
}

// Two due contracts and one that is not due, each with every dependent row.
// Returns the due ids, the other id and the reap clock.
func newReaperDependentTestContracts(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture) (dueIds []server.Id, retainedId server.Id, now time.Time) {
	t.Helper()
	contractIds := make([]server.Id, 3)
	for i := range contractIds {
		var err error
		contractIds[i], err = CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 0)
		server.Raise(err)
	}
	seedReaperDependentTestRows(t, ctx, f, contractIds)
	now = server.NowUtc()
	old := now.Add(-400 * 24 * time.Hour)
	dueIds, retainedId = contractIds[:2], contractIds[2]
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,close_time=$2
			WHERE contract_id=ANY($1)`, contractIds, old))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET reap_time=$2 WHERE contract_id=ANY($1)`, dueIds, old))
	}, server.TxReadCommitted, server.OptNoRetry())
	for _, id := range contractIds {
		if counts := countReaperDependentTestRows(t, ctx, id); counts != [6]int{1, 2, 1, 1, 1, 2} {
			t.Fatalf("fixture contract lacks its dependent rows: %v", counts)
		}
	}
	return
}

// With the cascade dropped first, as the cutover will, the reaper must still
// leave no close report behind a reaped contract. Every other dependent row
// goes with its contract as before, and a contract that is not due keeps all
// of its rows.
func TestRemoveDueContractsDeleteCloseReportsWithoutCascade(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		dueIds, retainedId, now := newReaperDependentTestContracts(t, ctx, f)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE contract_close_report DROP CONSTRAINT contract_close_report_contract_id_fkey`))
		}, server.TxReadCommitted, server.OptNoRetry())
		server.Db(ctx, func(conn server.PgConn) {
			var foreignKeys int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
				WHERE conrelid='contract_close_report'::regclass AND contype='f'`).Scan(&foreignKeys))
			if foreignKeys != 0 {
				t.Fatal("contract_close_report kept a foreign key that could cascade", foreignKeys)
			}
		})
		// One contract per batch, so each due contract takes its own pass.
		removeDueContractBatches(ctx, now, now.Add(-StragglerContractExpiration), 1)
		for _, id := range dueIds {
			if counts := countReaperDependentTestRows(t, ctx, id); counts != [6]int{} {
				t.Fatalf("reaped contract kept rows (contract, close, escrow, sweep, extender, report): %v", counts)
			}
		}
		if counts := countReaperDependentTestRows(t, ctx, retainedId); counts != [6]int{1, 2, 1, 1, 1, 2} {
			t.Fatalf("contract that was not due lost rows: %v", counts)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var orphanReports int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM contract_close_report AS report
				WHERE NOT EXISTS (SELECT 1 FROM transfer_contract WHERE contract_id=report.contract_id)`).Scan(&orphanReports))
			if orphanReports != 0 {
				t.Fatal("reaped contracts left close reports behind", orphanReports)
			}
		})
	})
}

// The tables the reaper only deletes from are partitioned by contract_id here,
// as the cutover will leave them, and a transaction holds every partition below
// the reaped contracts exclusively. Sessions default to generic plans, which
// cannot prune by an array parameter. Each reaper write must still prune to the
// partitions holding its contracts and finish without waiting on that holder.
func TestRemoveDueContractDeletesPruneToTheirPartitions(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		var reaperDone chan any
		defer func() {
			cancel()
			if reaperDone != nil {
				<-reaperDone
			}
		}()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		dueIds, retainedId, now := newReaperDependentTestContracts(t, ctx, f)
		// An older contract below the partition bound, with its own rows.
		var pastId server.Id
		var bound string
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(tx.QueryRow(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,
				 payer_network_id,transfer_byte_count)
				VALUES(contract_id_floor(now() - interval '30 days'),$1,$2,$3,$4,$1,0)
				RETURNING contract_id, contract_id_floor(now() - interval '1 day')::text`,
				f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId).Scan(&pastId, &bound))
		}, server.TxReadCommitted, server.OptNoRetry())
		seedReaperDependentTestRows(t, ctx, f, []server.Id{pastId})
		partitionedTables := []string{"contract_close_report", "contract_close", "contract_extender"}
		pastPartitions := make([]string, len(partitionedTables))
		server.Tx(ctx, func(tx server.PgTx) {
			for i, table := range partitionedTables {
				pastPartitions[i] = table + "_past"
				server.RaisePgResult(tx.Exec(ctx, strings.NewReplacer("{{table}}", table, "{{bound}}", bound).Replace(`
					CREATE TABLE {{table}}_partitioned (LIKE {{table}} INCLUDING ALL) PARTITION BY RANGE (contract_id);
					CREATE TABLE {{table}}_past PARTITION OF {{table}}_partitioned
						FOR VALUES FROM (MINVALUE) TO ('{{bound}}');
					CREATE TABLE {{table}}_current PARTITION OF {{table}}_partitioned
						FOR VALUES FROM ('{{bound}}') TO (MAXVALUE);
					INSERT INTO {{table}}_partitioned SELECT * FROM {{table}};
					DROP TABLE {{table}};
					ALTER TABLE {{table}}_partitioned RENAME TO {{table}};
				`)))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		// Every new session defaults to generic plans until the test resets it.
		setDatabasePlanCacheMode := func(setting string) {
			server.Db(context.WithoutCancel(ctx), func(conn server.PgConn) {
				var database string
				server.Raise(conn.QueryRow(context.WithoutCancel(ctx), `SELECT current_database()`).Scan(&database))
				server.RaisePgResult(conn.Exec(context.WithoutCancel(ctx), `ALTER DATABASE `+pgx.Identifier{database}.Sanitize()+` `+setting))
			})
			server.PgReset()
		}
		setDatabasePlanCacheMode(`SET plan_cache_mode = force_generic_plan`)
		defer setDatabasePlanCacheMode(`RESET plan_cache_mode`)
		holder := acquireContractLifecycleTestConnection(t, ctx)
		defer holder.Release()
		held, err := holder.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		server.Raise(err)
		defer held.Rollback(context.WithoutCancel(ctx))
		var planCacheMode string
		server.Raise(held.QueryRow(ctx, `SHOW plan_cache_mode`).Scan(&planCacheMode))
		if planCacheMode != "force_generic_plan" {
			t.Fatal("new sessions did not default to generic plans", planCacheMode)
		}
		server.RaisePgResult(held.Exec(ctx, `LOCK TABLE `+strings.Join(pastPartitions, ", ")+` IN ACCESS EXCLUSIVE MODE`))
		reaperDone = make(chan any, 1)
		go func() {
			reaperDone <- server.HandleError(func() {
				removeDueContractBatches(ctx, now, now.Add(-StragglerContractExpiration), 1)
			})
		}()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for reaperDone != nil {
			select {
			case reaperErr := <-reaperDone:
				reaperDone = nil
				if reaperErr != nil {
					t.Fatal("reaper failed", reaperErr)
				}
			case <-tick.C:
				var waiting bool
				server.Raise(held.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
					WHERE NOT granted AND relation::regclass::text = ANY($1::text[]))`, pastPartitions).Scan(&waiting))
				if waiting {
					t.Fatal("a reaper write waited on a partition that holds none of its contracts")
				}
			case <-ctx.Done():
				t.Fatal("reaper did not finish")
			}
		}
		server.Raise(held.Rollback(ctx))
		for _, id := range dueIds {
			if counts := countReaperDependentTestRows(t, ctx, id); counts != [6]int{} {
				t.Fatalf("reaped contract kept rows (contract, close, escrow, sweep, extender, report): %v", counts)
			}
		}
		for _, id := range []server.Id{retainedId, pastId} {
			if counts := countReaperDependentTestRows(t, ctx, id); counts != [6]int{1, 2, 1, 1, 1, 2} {
				t.Fatalf("contract that was not due lost rows: %v", counts)
			}
		}
	})
}
