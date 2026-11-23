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
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"
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
	failure error
}

func startTestContractLifecycleWorkload(ctx context.Context, workers int, readerHold time.Duration) *testContractLifecycleWorkload {
	workload := &testContractLifecycleWorkload{stop: make(chan struct{})}
	orders := [][]string{{"transfer_contract", "contract_close"}, {"contract_close", "transfer_contract"}}
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
	for i := range workers {
		order := orders[i%len(orders)]
		workload.running.Add(1)
		go func() {
			defer workload.running.Done()
			for workload.hold(ctx, rand.N(100*time.Millisecond)) {
				workload.record(contractPartitionTx(ctx, func(tx PgTx) {
					for _, table := range order {
						workload.timed(func() {
							RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SELECT 1 FROM public.%s LIMIT 1`, quoteIdentifier(table))))
						})
					}
					workload.hold(ctx, 500*time.Millisecond+rand.N(2100*time.Millisecond))
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
	statement()
	elapsed := time.Since(start)
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.slowest = max(self.slowest, elapsed)
}

func (self *testContractLifecycleWorkload) record(err error) {
	if err == nil {
		return
	}
	self.mutex.Lock()
	defer self.mutex.Unlock()
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
