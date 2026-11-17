// Destructive partition operations preserve concurrent writes and payment holds.
package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Reads the fixture's actual boundary, including when the suite already cut over.
func testContractPartitionSafetyDay(t testing.TB, ctx context.Context) time.Time {
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
	day := testContractPartitionSafetyDay(t, ctx)
	if attach {
		day = day.AddDate(0, 0, ContractPartitionAheadDays+1)
	}
	contractId := testContractIdAt(day.Add(time.Hour))
	partitionName := ContractPartitionName("transfer_contract", day)
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
					quoteIdentifier(partitionName), contractIdLiteral(ContractIdFloor(day)),
					contractIdLiteral(ContractIdFloor(day.AddDate(0, 0, 1))),
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

// The first daily writer is invisible to an unlocked emptiness check.
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

// One independently retiring day per payment state prevents one hold from
// hiding a missing predicate for another payment.
type testContractPartitionPaymentHold struct {
	name             string
	completed        bool
	canceled         bool
	retentionPending bool
	marker           string
	// the USDC close-out's usdc_payout_closed event
	closed     bool
	protected  bool
	contractId Id
	paymentId  Id
	day        PartitionRange
}

// Includes each external-attempt marker and controls that must remain eligible.
func testContractPartitionPaymentHolds(t testing.TB, ctx context.Context) ([]testContractPartitionPaymentHold, time.Time) {
	cutover := testContractPartitionSafetyDay(t, ctx)
	cases := []testContractPartitionPaymentHold{
		{name: "pending", protected: true},
		{name: "canceled idempotency key", canceled: true, marker: "circle_idempotency_key", protected: true},
		{name: "canceled payment record", canceled: true, marker: "payment_record", protected: true},
		{name: "canceled transaction hash", canceled: true, marker: "tx_hash", protected: true},
		// a former release's retention stamping flag holds nothing since USDC
		// completions ended
		{name: "completed retention pending", completed: true, retentionPending: true},
		{name: "canceled retention pending", canceled: true, retentionPending: true},
		{name: "canceled without attempt", canceled: true},
		{name: "completed external attempt", completed: true, marker: "circle_idempotency_key"},
		// USDC payouts ended (owner decision 2026-11-13): a closed payment is
		// settled, an attempt of it or not; one made pending again is not
		{name: "closed idempotency key", canceled: true, marker: "circle_idempotency_key", closed: true},
		{name: "closed without attempt", canceled: true, closed: true},
		{name: "pending with a closure", closed: true, protected: true},
	}
	// a runway covers the day it is ensured from and ContractPartitionAheadDays
	// past it: the first the cases through day 6, the second the rest
	for _, ensured := range []time.Time{cutover.AddDate(0, 0, 3), cutover.AddDate(0, 0, 6)} {
		if _, _, err := ensureContractDayPartitions(ctx, ensured); err != nil {
			t.Fatal(err)
		}
	}
	for i := range cases {
		c := &cases[i]
		start := cutover.AddDate(0, 0, i-1)
		c.contractId = testContractIdAt(start.Add(time.Hour))
		c.paymentId = NewId()
		c.day = contractPartitionFamily.RangeAt(start)
		if i == 0 {
			c.day = PartitionRange{Upper: cutover, Legacy: true}
		}
		testInsertContractRows(t, ctx, c.contractId, true)
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO account_payment
				(payment_id, payment_plan_id, wallet_id, payout_byte_count, payout_nano_cents,
				 min_sweep_time, completed, canceled, contract_retention_pending, complete_time)
				VALUES ($1,$2,$3,1,1,$4,$5,$6,$7,CASE WHEN $5 THEN $4::timestamp ELSE NULL END)`,
				c.paymentId, NewId(), NewId(), cutover, c.completed, c.canceled, c.retentionPending))
			if c.marker != "" {
				RaisePgResult(tx.Exec(ctx, fmt.Sprintf("UPDATE account_payment SET %s=$2 WHERE payment_id=$1", quoteIdentifier(c.marker)), c.paymentId, NewId().String()))
			}
			if c.closed {
				RaisePgResult(tx.Exec(ctx, `INSERT INTO audit_account_payment (event_id, payment_id, event_type, event_details)
					VALUES (`+UsdcPayoutClosedEventIdSql("$1::uuid")+`, $1, $2, '{}')`, c.paymentId, UsdcPayoutClosedEventType))
			}
			RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow_sweep SET payment_id=$2 WHERE contract_id=$1`, c.contractId, c.paymentId))
		})
	}
	// when the last case's day is ContractRetention (13 weeks) past
	return cases, cutover.AddDate(0, 0, len(cases)-1).Add(ContractRetention)
}

// Retirement deletes no contract and archives none, whatever its payment's
// state (S1): the contract horizon reaper alone judges a payment. Past the
// 91-day horizon, and past the 35-day one, every case keeps all six tables'
// rows and no usage is archived; each day is reported kept. Clearing the
// holds changes nothing until the reaper deletes a contract. The build before
// S1 archived and dropped every contract its own, narrower, payment check
// admitted.
func TestContractPartitionRetirementPreservesPaymentHolds(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cases, now := testContractPartitionPaymentHolds(t, ctx)
		settings := DefaultContractDayRetirementSettings()
		check := func(ctx context.Context, report *ContractPartitionReport) {
			for _, c := range cases {
				for _, table := range ContractPartitionTables {
					if !testContractPartitionScalar[bool](t, ctx, fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM public.%s WHERE contract_id=$1)", quoteIdentifier(table)), c.contractId) {
						t.Errorf("%s: retirement removed its %s row", c.name, table)
					}
				}
				if testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM st_provider_usage_archive WHERE contract_id=$1)`, c.contractId) {
					t.Errorf("%s: retirement archived the contract", c.name)
				}
				if _, kept := report.Kept[contractPartitionFamily.LeafName("transfer_contract", c.day)]; !kept {
					t.Errorf("%s: day not reported kept: blocked %q", c.name, report.Blocked[contractPartitionFamily.LeafName("transfer_contract", c.day)])
				}
			}
			for _, c := range cases {
				for _, leaf := range contractPartitionFamily.LeafNames(c.day) {
					if slices.Contains(report.Dropped, leaf) {
						t.Errorf("%s: retirement dropped %s", c.name, leaf)
					}
				}
			}
		}
		for _, horizonCtx := range []context.Context{ctx, WithContractHorizon(ctx, ContractDayRetentionFloor)} {
			report := &ContractPartitionReport{Blocked: map[string]string{}, Kept: map[string]int64{}}
			retireExpiredContractDays(horizonCtx, now, settings, time.Now().Add(time.Minute), report)
			check(horizonCtx, report)
		}
		Tx(ctx, func(tx PgTx) {
			for _, c := range cases {
				RaisePgResult(tx.Exec(ctx, `UPDATE account_payment SET completed=true, complete_time=$2, contract_retention_pending=false WHERE payment_id=$1`, c.paymentId, now))
			}
		})
		report := &ContractPartitionReport{Blocked: map[string]string{}, Kept: map[string]int64{}}
		retireExpiredContractDays(ctx, now, settings, time.Now().Add(time.Minute), report)
		check(ctx, report)
	})
}

// The drop refuses, under its locks, any day that still holds a tuple, and
// keeps every row: a day with a held contract, or any contract, never drops.
// The build before S1 dropped a range once its own payment check passed.
func TestContractPartitionDropPreservesPaymentHolds(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cases, _ := testContractPartitionPaymentHolds(t, ctx)
		settings := DefaultContractDayRetirementSettings()
		for _, c := range cases {
			leaves, err := readContractDayLeaves(ctx, c.day)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := dropContractDayLeaves(ctx, c.day, leaves, settings); !errors.Is(err, errContractDayNotEmpty) {
				t.Errorf("%s: a day that holds rows was not refused: %v", c.name, err)
			}
			for _, table := range ContractPartitionTables {
				if !testContractPartitionScalar[bool](t, ctx, fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM public.%s WHERE contract_id=$1)", quoteIdentifier(table)), c.contractId) {
					t.Errorf("%s: drop discarded %s", c.name, table)
				}
				if testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass('public.' || $1) IS NULL`, contractPartitionFamily.LeafName(table, c.day)) {
					t.Errorf("%s: drop detached %s", c.name, contractPartitionFamily.LeafName(table, c.day))
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
		day := ContractPartitionDayStart(NowUtc()).AddDate(0, 0, 2)
		var deadline time.Time
		err := MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{
			CutoverDay: day,
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
