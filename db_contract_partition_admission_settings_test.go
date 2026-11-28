// The admission's classification of each lock, and the bounds of the cutover
// settings (db_contract_partition_admission.go).
package server

import (
	"context"
	"testing"
	"time"
)

// The admission's judgement of each lock in a step's scope. A SHARE UPDATE
// EXCLUSIVE request (validate) is blocked only by a lock whose mode conflicts
// with it.
func TestContractScopeLockBlocks(t *testing.T) {
	maxAge := 5 * time.Second
	for _, test := range []struct {
		name      string
		lock      contractScopeLock
		exclusive bool
		validate  bool
	}{
		{
			name:      "an autovacuum that just started",
			lock:      contractScopeLock{pid: 10, backendType: "autovacuum worker", mode: "ShareUpdateExclusiveLock", granted: true, transactionAge: 100 * time.Millisecond},
			exclusive: true,
			validate:  true,
		},
		{
			name:      "an anti-wraparound autovacuum hours in",
			lock:      contractScopeLock{pid: 11, backendType: "autovacuum worker", mode: "ShareUpdateExclusiveLock", granted: true, transactionAge: 3 * time.Hour},
			exclusive: true,
			validate:  true,
		},
		{
			name:      "a prepared transaction's write",
			lock:      contractScopeLock{pid: 0, mode: "RowExclusiveLock", granted: true, transactionAge: -1},
			exclusive: true,
		},
		{
			name:      "a prepared transaction's share lock",
			lock:      contractScopeLock{pid: 0, mode: "ShareLock", granted: true, transactionAge: -1},
			exclusive: true,
			validate:  true,
		},
		{
			// a parallel worker that started or exited between the read's
			// pg_locks and pg_stat_activity: the analyze of the day's new
			// transfer_contract leaf at 2026-11-28 00:01Z
			name:      "a read of a session this role cannot see",
			lock:      contractScopeLock{pid: 12, mode: "AccessShareLock", granted: true, transactionAge: -1},
			exclusive: true,
		},
		{
			name:      "a write of a session this role cannot see",
			lock:      contractScopeLock{pid: 19, mode: "RowExclusiveLock", granted: true, transactionAge: -1},
			exclusive: true,
		},
		{
			name:      "a vacuum or analyze of a session this role cannot see",
			lock:      contractScopeLock{pid: 20, mode: "ShareUpdateExclusiveLock", granted: true, transactionAge: -1},
			exclusive: true,
			validate:  true,
		},
		{
			name:      "a DDL wait of a session this role cannot see",
			lock:      contractScopeLock{pid: 21, mode: "AccessExclusiveLock", granted: false, transactionAge: -1},
			exclusive: true,
			validate:  true,
		},
		{
			name: "a short write",
			lock: contractScopeLock{pid: 13, backendType: "client backend", mode: "RowExclusiveLock", granted: true, transactionAge: 200 * time.Millisecond},
		},
		{
			name:      "a long write",
			lock:      contractScopeLock{pid: 14, backendType: "client backend", mode: "RowExclusiveLock", granted: true, transactionAge: 30 * time.Second},
			exclusive: true,
		},
		{
			name:      "a long read",
			lock:      contractScopeLock{pid: 15, backendType: "client backend", mode: "AccessShareLock", granted: true, transactionAge: 30 * time.Second},
			exclusive: true,
		},
		{
			name:      "a long index build",
			lock:      contractScopeLock{pid: 16, backendType: "client backend", mode: "ShareUpdateExclusiveLock", granted: true, transactionAge: 30 * time.Minute},
			exclusive: true,
			validate:  true,
		},
		{
			name:      "a long DDL wait",
			lock:      contractScopeLock{pid: 17, backendType: "client backend", mode: "AccessExclusiveLock", granted: false, transactionAge: 10 * time.Second},
			exclusive: true,
			validate:  true,
		},
		{
			name: "an index build that just started",
			lock: contractScopeLock{pid: 18, backendType: "client backend", mode: "ShareUpdateExclusiveLock", granted: true, transactionAge: time.Second},
		},
	} {
		if blocks := contractScopeLockBlocks(contractLockAccessExclusive, test.lock, maxAge); blocks != test.exclusive {
			t.Errorf("%s: blocks an exclusive request %t, want %t", test.name, blocks, test.exclusive)
		}
		if blocks := contractScopeLockBlocks(contractLockShareUpdateExclusive, test.lock, maxAge); blocks != test.validate {
			t.Errorf("%s: blocks a validation %t, want %t", test.name, blocks, test.validate)
		}
	}
}

// Settings that would admit unbounded or negative waits are refused before
// anything runs.
func TestContractPartitionCutoverSettingsValidate(t *testing.T) {
	if err := DefaultContractPartitionCutoverSettings().validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ContractPartitionCutoverSettings){
		"transaction age":       func(s *ContractPartitionCutoverSettings) { s.LockSettings.MaxTransactionAge = 0 },
		"lock timeout":          func(s *ContractPartitionCutoverSettings) { s.LockTimeout = 0 },
		"swap transaction age":  func(s *ContractPartitionCutoverSettings) { s.SwapLockSettings.MaxTransactionAge = 0 },
		"swap lock timeout":     func(s *ContractPartitionCutoverSettings) { s.SwapLockTimeout = 0 },
		"validate lock timeout": func(s *ContractPartitionCutoverSettings) { s.ValidateLockTimeout = 0 },
		"step attempts":         func(s *ContractPartitionCutoverSettings) { s.StepAttempts = 0 },
		"swap attempts":         func(s *ContractPartitionCutoverSettings) { s.SwapAttempts = 0 },
		"abort attempts":        func(s *ContractPartitionCutoverSettings) { s.AbortAttempts = 0 },
		"retry interval":        func(s *ContractPartitionCutoverSettings) { s.SwapRetryInterval = -time.Second },
	} {
		settings := DefaultContractPartitionCutoverSettings()
		change(settings)
		if settings.validate() == nil {
			t.Errorf("%s: invalid settings accepted", name)
		}
		if err := MigrateContractsToPartitions(context.Background(), ContractPartitionMigrateOptions{Finalize: true, CutoverSettings: settings}); err == nil {
			t.Errorf("%s: finalize accepted invalid settings", name)
		}
	}
}

// Single-table exclusive steps (the legacy bound's add, abort, the
// abandonment) wait out the application's transactions but no long reader,
// and space their tries; on Main's workload of 2026-11-23 a rule of 500 ms
// and 200 ms (1cec91a1d) never admitted them, and a wait of 1 s three times
// within seconds (9d6650a97) backed PgBouncer up past 500 waiting clients.
// The swap and rollback, which lock the six tables in one transaction, are a
// bounded blip: their LOCK TABLE statement waits long enough to drain the
// application's transactions and short enough that no queued statement fails.
func TestContractExclusiveStepsWaitOutTransactionsButNotLongReaders(t *testing.T) {
	const deadlockTimeout = time.Second
	cutover := DefaultContractPartitionCutoverSettings()
	rollback := DefaultContractPartitionRollbackSettings()

	// single-table steps
	if cutover.LockTimeout < 500*time.Millisecond || 900*time.Millisecond < cutover.LockTimeout || deadlockTimeout <= cutover.LockTimeout || PgLockTimeout/2 < cutover.LockTimeout {
		t.Errorf("a single-table step waits up to %s for its lock; want 500 to 900 ms, under deadlock_timeout (%s) and PgLockTimeout/2 (%s)", cutover.LockTimeout, deadlockTimeout, PgLockTimeout/2)
	}
	if age := cutover.LockSettings.MaxTransactionAge; age < 2*time.Second || 3*time.Second < age {
		t.Errorf("a single-table step is admitted behind transactions up to %s old; want 2 to 3 s: older than the application's (up to 2.6 s), younger than its long readers", age)
	}
	for name, interval := range map[string]time.Duration{"a step": cutover.StepRetryInterval, "abort": cutover.AbortRetryInterval} {
		if interval < 20*time.Second || 30*time.Second < interval {
			t.Errorf("%s retries %s apart before jitter; want 20 to 30 s, so each stall is alone", name, interval)
		}
	}
	for name, attempts := range map[string]int{"a step": cutover.StepAttempts, "abort": cutover.AbortAttempts} {
		if attempts < 10 || 30 < attempts {
			t.Errorf("%s tries %d times a run; want 10 to 30, a run of minutes", name, attempts)
		}
	}

	// the swap and rollback: a bounded blip, a few times
	rollbackSql := contractRollbackLockSql([]string{"public.transfer_contract"}, rollback)
	for name, wait := range map[string]time.Duration{"the swap": cutover.SwapLockTimeout, "rollback": rollback.LockTimeout, "rollback's statements": testContractLockTimeoutOf(t, rollbackSql[0])} {
		if wait < time.Second || PgLockTimeout-400*time.Millisecond < wait {
			t.Errorf("%s waits up to %s for its locks; want 1 s to %s: long enough to drain the application's transactions, short enough that no queued statement reaches PgLockTimeout", name, wait, PgLockTimeout-400*time.Millisecond)
		}
	}
	for name, age := range map[string]time.Duration{"the swap": cutover.SwapLockSettings.MaxTransactionAge, "rollback": rollback.LockSettings.MaxTransactionAge} {
		if age < 2*time.Second || 3*time.Second < age {
			t.Errorf("%s is admitted behind transactions up to %s old; want 2 to 3 s", name, age)
		}
	}
	for name, attempts := range map[string]int{"the swap": cutover.SwapAttempts, "rollback": rollback.MaxAttempts} {
		if attempts < 3 || 10 < attempts {
			t.Errorf("%s tries %d times a run; want 3 to 10 blips at most", name, attempts)
		}
	}
	for name, interval := range map[string]time.Duration{"the swap": cutover.SwapRetryInterval, "rollback": rollback.RetryInterval} {
		if interval < 2*time.Minute {
			t.Errorf("%s retries %s apart before jitter; at least 2 minutes, so each blip is alone", name, interval)
		}
	}
	if err := cutover.validate(); err != nil {
		t.Fatal(err)
	}
}

// Retries wait 1 to 2 intervals at random, so that they do not fall into step
// with periodic work on the tables.
func TestContractRetryWaitIsJittered(t *testing.T) {
	waits := map[time.Duration]bool{}
	for range 1000 {
		wait := contractRetryWait(20 * time.Second)
		if wait < 20*time.Second || 40*time.Second <= wait {
			t.Fatalf("a retry after a 20 s interval waits %s; want 20 to 40 s", wait)
		}
		waits[wait] = true
	}
	if len(waits) < 100 {
		t.Fatalf("retries after a 20 s interval waited only %d distinct times in 1000", len(waits))
	}
	if wait := contractRetryWait(0); wait != 0 {
		t.Fatalf("a zero interval waits %s", wait)
	}
}
