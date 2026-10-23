// Destructive partition operations preserve concurrent writes and payment holds.
package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// Reads the fixture's actual boundary, including when the suite already cut over.
func testContractPartitionSafetyWeek(t testing.TB, ctx context.Context) time.Time {
	Testing_PartitionContracts(ctx)
	partitions, err := listContractPartitions(ctx, "transfer_contract")
	if err != nil {
		t.Fatal(err)
	}
	for _, partition := range partitions {
		if partition.legacy {
			return partition.upper
		}
	}
	t.Fatal("the fixture has no legacy partition")
	return time.Time{}
}

// A writer commits only after PostgreSQL proves rollback is blocked by it.
// Each goroutine owns one connection; cancellation joins both.
func testContractPartitionRollbackWriter(t testing.TB, attach bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	week := testContractPartitionSafetyWeek(t, ctx)
	if attach {
		week = week.Add(time.Duration(ContractPartitionAheadWeeks+1) * contractPartitionWeek)
	}
	contractId := testContractIdAt(week.Add(time.Hour))
	partitionName := ContractPartitionName("transfer_contract", week)
	writerReady := make(chan uint32, 1)
	commitWriter := make(chan struct{})
	writerDone := make(chan error, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		writerDone <- contractPartitionTx(ctx, func(tx PgTx) {
			if attach {
				RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
					"CREATE TABLE public.%s PARTITION OF transfer_contract FOR VALUES FROM (%s) TO (%s)",
					quoteIdentifier(partitionName), contractIdLiteral(ContractIdFloor(week)),
					contractIdLiteral(ContractIdFloor(week.Add(contractPartitionWeek))),
				)))
			}
			networkId := NewId()
			RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id, source_network_id, source_id, destination_network_id,
				 destination_id, transfer_byte_count, payer_network_id)
				VALUES ($1,$2,$3,$4,$5,1024,$2)`, contractId, networkId, NewId(), NewId(), NewId()))
			writerReady <- tx.Conn().PgConn().PID()
			select {
			case <-commitWriter:
			case <-ctx.Done():
				panic(ctx.Err())
			}
		})
	}()
	var writerPid uint32
	select {
	case writerPid = <-writerReady:
	case err := <-writerDone:
		t.Fatalf("writer failed before its barrier: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	rollbackDone := make(chan error, 1)
	settings := DefaultContractPartitionRollbackSettings()
	// Keep the writer admissible and its lock wait observable for the full
	// test deadline, even when the test process is descheduled.
	settings.LockSettings.MaxTransactionAge = time.Minute
	settings.LockTimeout = time.Minute
	settings.StatementTimeout = time.Minute
	workers.Add(1)
	go func() {
		defer workers.Done()
		failure := testContractPartitionRecover(func() {
			Raise(MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{Rollback: true, RollbackSettings: settings}))
		})
		if failure != nil {
			rollbackDone <- fmt.Errorf("%v", failure)
		} else {
			rollbackDone <- nil
		}
	}()
	lockModes := []string{"AccessExclusiveLock"}
	if attach {
		// A concurrent attach also blocks catalog deparsing on the old path.
		lockModes = append(lockModes, "AccessShareLock")
	}
	MaintenanceDb(ctx, func(conn PgConn) {
		for {
			var waiting bool
			Raise(conn.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM pg_locks AS waiting
				WHERE waiting.relation = 'public.transfer_contract'::regclass
					AND waiting.database = (SELECT oid FROM pg_database WHERE datname = current_database())
					AND waiting.mode = ANY($2::text[]) AND NOT waiting.granted
					AND $1::int = ANY(pg_blocking_pids(waiting.pid)))`, writerPid, lockModes).Scan(&waiting))
			if waiting {
				break
			}
			select {
			case err := <-rollbackDone:
				t.Fatalf("rollback ended before its lock barrier: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
	})
	close(commitWriter)
	if err := <-writerDone; err != nil {
		t.Fatalf("writer commit: %v", err)
	}
	err := <-rollbackDone
	if err == nil {
		t.Errorf("rollback must refuse the committed row, got %v", err)
	}
	if !IsContractPartitioned(ctx) {
		t.Error("refused rollback changed the partitioned layout")
	}
	if !testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM transfer_contract WHERE contract_id=$1)`, contractId) {
		t.Error("rollback discarded the committed contract")
	}
}

// The first weekly writer is invisible to an unlocked emptiness check.
func TestContractPartitionRollbackPreservesConcurrentInsert(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		testContractPartitionRollbackWriter(t, false)
	})
}

// An uncommitted leaf attachment must be visible to the rollback safety check.
func TestContractPartitionRollbackRefreshesConcurrentPartition(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		testContractPartitionRollbackWriter(t, true)
	})
}

// One independently retiring week per payment state prevents one hold from
// hiding a missing predicate for another payment.
type testContractPartitionPaymentHold struct {
	name             string
	completed        bool
	canceled         bool
	retentionPending bool
	marker           string
	protected        bool
	contractId       Id
	paymentId        Id
	week             contractWeek
}

// Includes each external-attempt marker and controls that must remain eligible.
func testContractPartitionPaymentHolds(t testing.TB, ctx context.Context) ([]testContractPartitionPaymentHold, time.Time) {
	week := testContractPartitionSafetyWeek(t, ctx)
	cases := []testContractPartitionPaymentHold{
		{name: "pending", protected: true},
		{name: "canceled idempotency key", canceled: true, marker: "circle_idempotency_key", protected: true},
		{name: "canceled payment record", canceled: true, marker: "payment_record", protected: true},
		{name: "canceled transaction hash", canceled: true, marker: "tx_hash", protected: true},
		{name: "completed retention pending", completed: true, retentionPending: true, protected: true},
		{name: "canceled retention pending", canceled: true, retentionPending: true, protected: true},
		{name: "canceled without attempt", canceled: true},
		{name: "completed external attempt", completed: true, marker: "circle_idempotency_key"},
	}
	_, _, err := ensureContractWeekPartitions(ctx, week.Add(3*contractPartitionWeek))
	if err != nil {
		t.Fatal(err)
	}
	for i := range cases {
		c := &cases[i]
		start := week.Add(time.Duration(i-1) * contractPartitionWeek)
		c.contractId = testContractIdAt(start.Add(time.Hour))
		c.paymentId = NewId()
		c.week = contractWeek{
			partition: contractPartition{
				name: ContractPartitionName("transfer_contract", start), weekStart: start,
				upper: start.Add(contractPartitionWeek), legacy: i == 0,
			},
			lower: ContractIdFloor(start), upper: ContractIdFloor(start.Add(contractPartitionWeek)),
		}
		if i == 0 {
			c.week.partition.name = ContractLegacyPartitionName("transfer_contract")
			c.week.lower = Id{}
		}
		testInsertContractRows(t, ctx, c.contractId, true)
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment
				(payment_id, payment_plan_id, wallet_id, payout_byte_count, payout_nano_cents,
				 min_sweep_time, completed, canceled, contract_retention_pending, complete_time)
				VALUES ($1,$2,$3,1,1,$4,$5,$6,$7,CASE WHEN $5 THEN $4::timestamp ELSE NULL END)`,
				c.paymentId, NewId(), NewId(), week, c.completed, c.canceled, c.retentionPending))
			if c.marker != "" {
				RaisePgResult(tx.Exec(ctx, fmt.Sprintf("UPDATE account_payment SET %s=$2 WHERE payment_id=$1", quoteIdentifier(c.marker)), c.paymentId, NewId().String()))
			}
			RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow_sweep SET payment_id=$2 WHERE contract_id=$1`, c.contractId, c.paymentId))
		})
	}
	return cases, week.Add(20 * contractPartitionWeek)
}

// Retirement must preserve all six tables and must not archive held contracts;
// clearing the holds must allow the same cursor to finish retirement.
func TestContractPartitionRetirementPreservesPaymentHolds(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cases, now := testContractPartitionPaymentHolds(t, ctx)
		report := &ContractPartitionReport{Blocked: map[string]string{}}
		cursor := retireExpiredContractWeeks(ctx, now, ContractRetentionCursor{}, time.Now().Add(time.Minute), report)
		for _, c := range cases {
			for _, table := range ContractPartitionTables {
				exists := testContractPartitionScalar[bool](t, ctx, fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM public.%s WHERE contract_id=$1)", quoteIdentifier(table)), c.contractId)
				if exists != c.protected {
					t.Errorf("%s: %s exists=%t, want %t", c.name, table, exists, c.protected)
				}
			}
			archived := testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM st_provider_usage_archive WHERE contract_id=$1)`, c.contractId)
			if archived == c.protected {
				t.Errorf("%s: archived=%t with protected=%t", c.name, archived, c.protected)
			}
			if (report.Blocked[c.week.partition.name] != "") != c.protected {
				t.Errorf("%s: inconsistent blocker %q", c.name, report.Blocked[c.week.partition.name])
			}
		}
		Tx(ctx, func(tx PgTx) {
			for _, c := range cases {
				RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET completed=true, complete_time=$2, contract_retention_pending=false WHERE payment_id=$1`, c.paymentId, now))
			}
		})
		report = &ContractPartitionReport{Blocked: map[string]string{}}
		retireExpiredContractWeeks(ctx, now, cursor, time.Now().Add(time.Minute), report)
		for _, c := range cases {
			if !testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM st_provider_usage_archive WHERE contract_id=$1)`, c.contractId) {
				t.Errorf("%s: cleared hold did not retire", c.name)
			}
		}
	})
}

// The final check after detach independently rejects every protected state and
// rolls all six detach operations back, even if an earlier check admitted work.
func TestContractPartitionDropPreservesPaymentHolds(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cases, _ := testContractPartitionPaymentHolds(t, ctx)
		for _, c := range cases {
			if !c.protected {
				continue
			}
			if _, err := dropContractWeek(ctx, c.week); err == nil {
				t.Errorf("%s: drop bypassed its payment hold", c.name)
			}
			for _, table := range ContractPartitionTables {
				if !testContractPartitionScalar[bool](t, ctx, fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM public.%s WHERE contract_id=$1)", quoteIdentifier(table)), c.contractId) {
					t.Errorf("%s: drop discarded %s", c.name, table)
				}
			}
		}
	})
}

// Maintenance must not abandon preparation before the operator's printed
// deadline. Exercise the task on either side of the deadline the command emits.
func TestContractPartitionPrepareReportsAbandonmentDeadline(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if IsContractPartitioned(ctx) {
			t.Skip("prepare starts from plain tables")
		}
		week := ContractPartitionWeekStart(NowUtc()).Add(2 * contractPartitionWeek)
		var deadline time.Time
		err := MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{
			CutoverWeek: week,
			Logf: func(format string, args ...any) {
				message := fmt.Sprintf(format, args...)
				if value, found := strings.CutPrefix(message, "prepared; run --finalize before "); found {
					var err error
					deadline, err = time.Parse(time.RFC3339, value)
					if err != nil {
						t.Fatalf("invalid reported deadline: %v", err)
					}
				}
			},
		})
		if err != nil || deadline.IsZero() {
			t.Fatalf("prepare did not report its deadline: %v", err)
		}
		report, cursor := MaintainContractPartitions(ctx, deadline.Add(-time.Minute), ContractRetentionCursor{}, time.Minute)
		if report.AbandonedCutover {
			t.Fatal("maintenance abandoned preparation before the advertised deadline")
		}
		report, _ = MaintainContractPartitions(ctx, deadline.Add(time.Minute), cursor, time.Minute)
		if !report.AbandonedCutover {
			t.Fatal("maintenance did not enforce the advertised deadline")
		}
	})
}
