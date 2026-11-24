// The cutover's single-table exclusive steps under a workload shaped like
// Main's on 2026-11-23, when the contract tables always had holders: dozens
// of application transactions idle in transaction for 0.5 to 2.6 s on
// several of the tables at once, and readers of 26 to 45 s. A rule that
// requested a lock only while no holder was older than 500 ms, and waited at
// most 200 ms (1cec91a1d), never admitted a step there, so abort and the
// abandonment of a stale prepare could never drop its legacy bound, which
// refuses every contract from the cutover day.
package server

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Workers that each hold transfer_contract and contract_close (in either
// order, as the creation and close paths do), idle in transaction for 0.5 to
// 2.6 s, one transaction after another; and a reader that holds
// transfer_contract for readerHold. Each statement is timed: the statements
// are instant on the test tables, so a statement's time is the stall that a
// lock request imposed on it.
type testContractLifecycleWorkload struct {
	stop    chan struct{}
	running sync.WaitGroup
	mutex   sync.Mutex
	slowest time.Duration
	// statements that waited past PgLockTimeout: on Main they fail with 55P03
	overBound  int
	statements int
	// transactions that a deadlock check ended (40P01)
	deadlocks int
	failure   error
}

// One application path: its tables in the order it first touches them, the
// pause after its first statement, and how long it then stays idle in
// transaction.
type testContractWorkPath struct {
	tables           []string
	gap              time.Duration
	holdMin, holdMax time.Duration
}

func startTestContractLifecycleWorkload(ctx context.Context, workers int, readerHold time.Duration) *testContractLifecycleWorkload {
	paths := []testContractWorkPath{}
	for i := range workers {
		tables := []string{"transfer_contract", "contract_close"}
		if i%2 == 1 {
			tables = []string{"contract_close", "transfer_contract"}
		}
		paths = append(paths, testContractWorkPath{tables: tables, holdMin: 500 * time.Millisecond, holdMax: 2600 * time.Millisecond})
	}
	return startTestContractWorkload(ctx, paths, readerHold)
}

func startTestContractWorkload(ctx context.Context, paths []testContractWorkPath, readerHold time.Duration) *testContractLifecycleWorkload {
	workload := &testContractLifecycleWorkload{stop: make(chan struct{})}
	if 0 < readerHold {
		workload.running.Add(1)
		go func() {
			defer workload.running.Done()
			workload.record(contractPartitionTx(ctx, func(tx PgTx) {
				workload.timed(func() {
					RaisePgResult(tx.Exec(ctx, `SELECT count(*) FROM public.transfer_contract`))
				})
				workload.hold(ctx, readerHold)
			}))
		}()
	}
	for _, path := range paths {
		workload.running.Add(1)
		go func() {
			defer workload.running.Done()
			for workload.hold(ctx, rand.N(100*time.Millisecond)) {
				workload.record(contractPartitionTx(ctx, func(tx PgTx) {
					for i, table := range path.tables {
						workload.timed(func() {
							RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SELECT 1 FROM public.%s LIMIT 1`, quoteIdentifier(table))))
						})
						if i == 0 && 0 < path.gap {
							workload.hold(ctx, path.gap)
						}
					}
					workload.hold(ctx, path.holdMin+rand.N(path.holdMax-path.holdMin))
				}))
			}
		}()
	}
	return workload
}

// Waits d, or less once the workload stops; false once it has stopped.
func (self *testContractLifecycleWorkload) hold(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-self.stop:
		return false
	case <-ctx.Done():
		return false
	}
}

func (self *testContractLifecycleWorkload) timed(statement func()) {
	start := time.Now()
	defer func() {
		elapsed := time.Since(start)
		self.mutex.Lock()
		defer self.mutex.Unlock()
		self.statements += 1
		self.slowest = max(self.slowest, elapsed)
		if PgLockTimeout < elapsed {
			self.overBound += 1
		}
	}()
	statement()
}

// A deadlock check that ends a workload transaction (40P01) is counted, as
// on Main a settlement caught between the swap's locks is: it is not rerun,
// and completes on the next report or deadline.
func (self *testContractLifecycleWorkload) record(err error) {
	if err == nil {
		return
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "40P01" || strings.Contains(err.Error(), "SQLSTATE 40P01") {
		self.deadlocks += 1
		return
	}
	if self.failure == nil {
		self.failure = err
	}
}

// Stops the workload and returns its slowest statement and first failure.
func (self *testContractLifecycleWorkload) end() (time.Duration, error) {
	close(self.stop)
	self.running.Wait()
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.slowest, self.failure
}

// Counts the tries a step logged as busy: deferred by its admission, or a
// lock wait that ran out.
type testContractBusyLog struct {
	t     testing.TB
	mutex sync.Mutex
	busy  int
}

func (self *testContractBusyLog) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	self.mutex.Lock()
	if strings.HasPrefix(line, "not queueing behind") || strings.HasPrefix(line, "lock busy") {
		self.busy += 1
	}
	self.mutex.Unlock()
	self.t.Log(line)
}

// A prepared cutover under the workload, with the default admission and lock
// wait, and tries closer together than the default 20 to 40 s so that the
// test takes about a minute. The workload starts a second before the step, so
// the step's first try finds the 30 s reader young and waits behind it.
func testContractStepUnderLifecycleWorkload(t testing.TB, step func(ctx context.Context, settings *ContractPartitionCutoverSettings, logf func(string, ...any)) error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if IsContractPartitioned(ctx) {
		t.Skip("the cutover starts from plain contract tables")
	}
	cutoverDay := ContractPartitionDayStart(NowUtc()).AddDate(0, 0, 2)
	if err := MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{CutoverDay: cutoverDay, Logf: testContractPartitionLogf(t)}); err != nil {
		t.Fatal(err)
	}

	workload := startTestContractLifecycleWorkload(ctx, 3, 30*time.Second)
	time.Sleep(time.Second)
	settings := DefaultContractPartitionCutoverSettings()
	settings.AbortAttempts = 120
	settings.AbortRetryInterval = 500 * time.Millisecond
	busyLog := &testContractBusyLog{t: t}
	start := time.Now()
	err := step(ctx, settings, busyLog.logf)
	elapsed := time.Since(start)
	slowest, failure := workload.end()
	if failure != nil {
		t.Fatalf("the workload failed: %v", failure)
	}
	if err != nil {
		t.Fatalf("the step did not drop the prepare under the workload in %d tries (%s): %v", busyLog.busy, elapsed.Round(time.Second), err)
	}
	if _, prepared := preparedContractCutoverDay(ctx); prepared {
		t.Fatalf("the step returned, and the cutover is still prepared")
	}
	// a lock wait of at most LockTimeout, and the step's own catalog work
	if limit := settings.LockTimeout + 300*time.Millisecond; limit < slowest {
		t.Fatalf("a workload statement waited %s behind the step; each stall stays under %s", slowest, limit)
	}
	t.Logf("dropped the prepare after %d busy tries in %s; the slowest workload statement took %s", busyLog.busy, elapsed.Round(time.Second), slowest)
}

func TestContractPartitionAbortCompletesUnderLifecycleWorkload(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		testContractStepUnderLifecycleWorkload(t, func(ctx context.Context, settings *ContractPartitionCutoverSettings, logf func(string, ...any)) error {
			return MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{Abort: true, Logf: logf, CutoverSettings: settings})
		})
	})
}

func TestContractPartitionAbandonmentCompletesUnderLifecycleWorkload(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		testContractStepUnderLifecycleWorkload(t, func(ctx context.Context, settings *ContractPartitionCutoverSettings, logf func(string, ...any)) error {
			cutoverDay, prepared := preparedContractCutoverDay(ctx)
			if !prepared {
				return fmt.Errorf("nothing is prepared")
			}
			// the maintenance task's deadline is its run's budget
			if !abandonStaleContractCutover(ctx, cutoverDay, time.Now().Add(3*time.Minute), settings, logf) {
				return fmt.Errorf("the abandonment left the prepare in place")
			}
			return nil
		})
	})
}

// The swap and rollback under a workload of the application's own lock orders
// (CONTRACT-PARTITION-CUTOVER-NOW.md; the paths' first touches mapped from
// model/): creation and close reports take transfer_contract first;
// settlement reads transfer_escrow, then transfer_contract one round trip
// later. Each transaction then stays idle 0.5 to 2.6 s, as on Main on
// 2026-11-23. With the default admission and lock bound, and tries a few
// seconds apart, the swap and then rollback complete, and no workload
// statement waits past PgLockTimeout, at which a main-pool statement fails
// with 55P03.
func TestContractPartitionSwapAndRollbackUnderLifecycleWorkload(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		defer cancel()
		if IsContractPartitioned(ctx) {
			t.Skip("the cutover starts from plain contract tables")
		}
		cutoverDay := ContractPartitionDayStart(NowUtc()).AddDate(0, 0, 2)
		if err := MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{CutoverDay: cutoverDay, Logf: testContractPartitionLogf(t)}); err != nil {
			t.Fatal(err)
		}
		idle := func(tables ...string) testContractWorkPath {
			return testContractWorkPath{tables: tables, holdMin: 500 * time.Millisecond, holdMax: 2600 * time.Millisecond}
		}
		paths := []testContractWorkPath{
			idle("transfer_contract", "transfer_escrow", "contract_extender"),
			idle("transfer_contract", "transfer_escrow", "contract_extender"),
			idle("transfer_contract", "contract_close_report", "contract_close"),
			idle("transfer_contract", "contract_close_report", "contract_close"),
			{tables: []string{"transfer_escrow", "transfer_contract", "contract_close", "transfer_escrow_sweep"}, gap: 5 * time.Millisecond, holdMin: 200 * time.Millisecond, holdMax: time.Second},
		}

		for _, step := range []struct {
			name string
			run  func(logf func(string, ...any)) error
			done func() bool
		}{
			{"the swap", func(logf func(string, ...any)) error {
				settings := DefaultContractPartitionCutoverSettings()
				settings.SwapAttempts = 30
				settings.SwapRetryInterval = 2 * time.Second
				return MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{Finalize: true, Logf: logf, CutoverSettings: settings})
			}, func() bool { return IsContractPartitioned(ctx) }},
			{"rollback", func(logf func(string, ...any)) error {
				settings := DefaultContractPartitionRollbackSettings()
				settings.MaxAttempts = 30
				settings.RetryInterval = 2 * time.Second
				return MigrateContractsToPartitions(ctx, ContractPartitionMigrateOptions{Rollback: true, Logf: logf, RollbackSettings: settings})
			}, func() bool { return !IsContractPartitioned(ctx) }},
		} {
			workload := startTestContractWorkload(ctx, paths, 0)
			time.Sleep(3 * time.Second)
			busyLog := &testContractBusyLog{t: t}
			start := time.Now()
			err := step.run(busyLog.logf)
			elapsed := time.Since(start)
			time.Sleep(time.Second)
			slowest, failure := workload.end()
			if failure != nil {
				t.Fatalf("%s: the workload failed: %v", step.name, failure)
			}
			if err != nil {
				t.Fatalf("%s did not complete under the workload in %d busy tries (%s): %v", step.name, busyLog.busy, elapsed.Round(time.Second), err)
			}
			if !step.done() {
				t.Fatalf("%s returned, and the layout did not change", step.name)
			}
			workload.mutex.Lock()
			overBound, statements, deadlocks := workload.overBound, workload.statements, workload.deadlocks
			workload.mutex.Unlock()
			if 0 < overBound {
				t.Fatalf("%s: %d of %d workload statements waited past PgLockTimeout (%s; the slowest %s): on Main they would fail with 55P03", step.name, overBound, statements, PgLockTimeout, slowest)
			}
			t.Logf("%s: completed after %d busy tries in %s; the slowest of %d workload statements took %s; %d workload transactions ended by a deadlock check", step.name, busyLog.busy, elapsed.Round(time.Second), statements, slowest, deadlocks)
		}
	})
}
