// The admission's classification of each lock, and the bounds of the cutover
// settings (db_contract_partition_admission.go).
package server

import (
	"context"
	"testing"
	"time"
)

// The admission's judgement of each lock in a step's scope.
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
			name:      "a prepared transaction",
			lock:      contractScopeLock{pid: 0, mode: "RowExclusiveLock", granted: true, transactionAge: -1},
			exclusive: true,
			validate:  true,
		},
		{
			name:      "a session this role cannot see",
			lock:      contractScopeLock{pid: 12, mode: "AccessShareLock", granted: true, transactionAge: -1},
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
