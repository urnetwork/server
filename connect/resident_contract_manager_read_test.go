package connect

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func residentReadControl(t *testing.T) (*residentContractManager, context.CancelFunc, server.Id, server.Id) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	manager := newResidentContractManager(ctx, cancel, server.NewId(), &ExchangeSettings{ContractManagerCheckTimeout: time.Hour})
	return manager, cancel, server.NewId(), server.NewId()
}

func awaitResidentRead(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("resident read control did not reach its synchronization boundary")
		}
	}
}

func residentReadWaiters(manager *residentContractManager, sourceId, destinationId server.Id) int {
	manager.stateLock.Lock()
	defer manager.stateLock.Unlock()
	if read := manager.activeReads[model.NewTransferPair(sourceId, destinationId)]; read != nil {
		return read.waiters
	}
	return 0
}

func takeResidentRead(t *testing.T, result <-chan bool) bool {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("resident read caller did not return")
		return false
	}
}

func TestResidentContractReadCoalescesColdPositiveMisses(t *testing.T) {
	manager, _, sourceId, destinationId := residentReadControl(t)
	var calls atomic.Int32
	release := make(chan struct{})
	manager.readContract = func(ctx context.Context, source, destination server.Id) bool {
		if ctx != manager.ctx || source != sourceId || destination != destinationId {
			panic("resident read changed its context or pair")
		}
		calls.Add(1)
		<-release
		return true
	}
	const callers = 16
	results := make(chan bool, callers)
	for range callers {
		go func() { results <- manager.HasActiveContract(sourceId, destinationId) }()
	}
	// On the baseline every caller reaches the controlled database seam. With
	// coalescing, one reaches it and the remaining callers are recorded waiters.
	awaitResidentRead(t, func() bool {
		return calls.Load() == callers || residentReadWaiters(manager, sourceId, destinationId) == callers-1
	})
	close(release)
	for range callers {
		if !takeResidentRead(t, results) {
			t.Fatal("positive shared read became a refusal")
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("concurrent cold reads = %d, want 1", got)
	}
	if !manager.HasActiveContract(sourceId, destinationId) || calls.Load() != 1 {
		t.Fatal("positive result did not retain the existing cache behavior")
	}
	manager.stateLock.Lock()
	defer manager.stateLock.Unlock()
	if len(manager.activeReads) != 0 {
		t.Fatal("completed shared read remained registered")
	}
}

func TestResidentContractReadDoesNotCacheNegative(t *testing.T) {
	manager, _, sourceId, destinationId := residentReadControl(t)
	var calls atomic.Int32
	manager.readContract = func(context.Context, server.Id, server.Id) bool { calls.Add(1); return false }
	if manager.HasActiveContract(sourceId, destinationId) || manager.HasActiveContract(sourceId, destinationId) || calls.Load() != 2 {
		t.Fatal("negative result was cached")
	}
	manager.readContract = func(context.Context, server.Id, server.Id) bool { calls.Add(1); return true }
	if !manager.HasActiveContract(sourceId, destinationId) || calls.Load() != 3 {
		t.Fatal("new contract after a negative result did not become visible")
	}
}

func TestResidentContractReadLateWaiterRechecksOlderNegative(t *testing.T) {
	manager, _, sourceId, destinationId := residentReadControl(t)
	var calls atomic.Int32
	olderSnapshot := make(chan struct{})
	release := make(chan struct{})
	manager.readContract = func(context.Context, server.Id, server.Id) bool {
		if calls.Add(1) == 1 {
			close(olderSnapshot)
			<-release
			return false
		}
		return true
	}
	first := make(chan bool, 1)
	go func() { first <- manager.HasActiveContract(sourceId, destinationId) }()
	<-olderSnapshot
	// A new contract is considered committed after the first lookup's snapshot
	// and before this invocation. It cannot inherit that older negative result.
	second := make(chan bool, 1)
	go func() { second <- manager.HasActiveContract(sourceId, destinationId) }()
	awaitResidentRead(t, func() bool { return calls.Load() == 2 || residentReadWaiters(manager, sourceId, destinationId) == 1 })
	close(release)
	if takeResidentRead(t, first) || !takeResidentRead(t, second) || calls.Load() != 2 {
		t.Fatal("late waiter used a snapshot from before its invocation")
	}
}

func TestResidentContractReadOtherPairsRemainParallel(t *testing.T) {
	manager, _, sourceId, destinationId := residentReadControl(t)
	otherSource := server.NewId()
	started, release := make(chan struct{}), make(chan struct{})
	manager.readContract = func(_ context.Context, source, _ server.Id) bool {
		if source == sourceId {
			close(started)
			<-release
		}
		return true
	}
	first, second := make(chan bool, 1), make(chan bool, 1)
	go func() { first <- manager.HasActiveContract(sourceId, destinationId) }()
	<-started
	go func() { second <- manager.HasActiveContract(otherSource, destinationId) }()
	if !takeResidentRead(t, second) {
		t.Fatal("unrelated pair was refused")
	}
	close(release)
	if !takeResidentRead(t, first) {
		t.Fatal("blocked pair did not complete")
	}
}

func TestResidentContractReadRefreshKeepsPositiveCacheAndSingleRead(t *testing.T) {
	manager, _, sourceId, destinationId := residentReadControl(t)
	pair := model.NewTransferPair(sourceId, destinationId)
	manager.activeContracts[pair] = &activeContractEntry{checkTime: time.Now().Add(-45 * time.Minute)}
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	manager.readContract = func(context.Context, server.Id, server.Id) bool {
		calls.Add(1)
		close(started)
		<-release
		return true
	}
	if !manager.HasActiveContract(sourceId, destinationId) {
		t.Fatal("refresh blocked a valid cached result")
	}
	<-started
	for range 16 {
		if !manager.HasActiveContract(sourceId, destinationId) {
			t.Fatal("refresh invalidated the positive cache")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("positive refresh duplicated the database read")
	}
	manager.stateLock.Lock()
	manager.activeContracts[pair].checkTime = time.Now().Add(-2 * time.Hour)
	manager.stateLock.Unlock()
	result := make(chan bool, 1)
	go func() { result <- manager.HasActiveContract(sourceId, destinationId) }()
	awaitResidentRead(t, func() bool { return calls.Load() > 1 || residentReadWaiters(manager, sourceId, destinationId) == 1 })
	close(release)
	if !takeResidentRead(t, result) || calls.Load() != 1 {
		t.Fatal("expired caller duplicated an in-flight refresh")
	}
}

func TestResidentContractReadCanceledWaiterReturnsAndOwnerJoins(t *testing.T) {
	manager, cancel, sourceId, destinationId := residentReadControl(t)
	started, release := make(chan struct{}), make(chan struct{})
	manager.readContract = func(context.Context, server.Id, server.Id) bool { close(started); <-release; return false }
	first, second := make(chan bool, 1), make(chan bool, 1)
	go func() { first <- manager.HasActiveContract(sourceId, destinationId) }()
	<-started
	go func() { second <- manager.HasActiveContract(sourceId, destinationId) }()
	awaitResidentRead(t, func() bool { return residentReadWaiters(manager, sourceId, destinationId) == 1 })
	cancel()
	if takeResidentRead(t, second) {
		t.Fatal("canceled waiter accepted a contract")
	}
	close(release)
	if takeResidentRead(t, first) {
		t.Fatal("canceled owner accepted a contract")
	}
	manager.stateLock.Lock()
	defer manager.stateLock.Unlock()
	if len(manager.activeReads) != 0 {
		t.Fatal("canceled read retained its flight")
	}
}

func TestResidentContractReadFailureReleasesWaiters(t *testing.T) {
	manager, _, sourceId, destinationId := residentReadControl(t)
	want := errors.New("synthetic resident read failure")
	started, release := make(chan struct{}), make(chan struct{})
	manager.readContract = func(context.Context, server.Id, server.Id) bool { close(started); <-release; panic(want) }
	results := make(chan any, 2)
	call := func() { defer func() { results <- recover() }(); manager.HasActiveContract(sourceId, destinationId) }
	go call()
	<-started
	go call()
	awaitResidentRead(t, func() bool { return residentReadWaiters(manager, sourceId, destinationId) == 1 })
	close(release)
	for range 2 {
		select {
		case got := <-results:
			if got != want {
				t.Fatal("shared read changed its failure")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("failed read retained a waiter")
		}
	}
	manager.readContract = func(context.Context, server.Id, server.Id) bool { return true }
	if !manager.HasActiveContract(sourceId, destinationId) {
		t.Fatal("failed flight prevented a fresh successful read")
	}
}
