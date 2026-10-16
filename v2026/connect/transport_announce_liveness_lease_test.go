package connect

// One announcement's part in its client's provider liveness lease
// (transport_announce_liveness_lease.go), with its writes recorded instead of
// sent and its worker driven one step at a time: which reads start the
// worker, that it refreshes whatever the rounds do, which clients write at
// all, when the lease is released, that the end of a connection writes
// nothing, and the process's refreshes that its handler stamp vouches for.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The writes of one connection, in order, and the errors to return.
type livenessLeaseWritesForTest struct {
	writes     []string
	refreshErr error
	releaseErr error
	// the workers the connection asked to start, not started
	workers []func()
}

// A connection whose lease writes go to writes, with a registry of its own and
// a clock at now, whose worker is recorded rather than run: the test drives it
// with step.
func newLivenessLeaseConnectionForTest(writes *livenessLeaseWritesForTest, now *time.Time) *providerLivenessLeaseConnection {
	lease := newProviderLivenessLeaseConnection(server.NewId(), 0, func(run func()) bool {
		writes.workers = append(writes.workers, run)
		return true
	}, nil)
	lease.refreshes = newProviderLivenessRefreshes()
	lease.now = func() time.Time {
		return *now
	}
	lease.refreshLease = func(context.Context, server.Id) error {
		writes.writes = append(writes.writes, "refresh")
		return writes.refreshErr
	}
	lease.releaseLease = func(context.Context, server.Id) error {
		writes.writes = append(writes.writes, "release")
		return writes.releaseErr
	}
	return lease
}

var livenessLeasePublicForTest = map[model.ProvideMode]bool{model.ProvideModePublic: true}

// The connection's entry in its process's refreshes, if any.
func livenessLeaseRefreshTimeForTest(lease *providerLivenessLeaseConnection) (time.Time, bool) {
	lease.refreshes.stateLock.Lock()
	defer lease.refreshes.stateLock.Unlock()
	refreshTime, ok := lease.refreshes.connectionRefreshTimes[lease]
	return refreshTime, ok
}

// The first read that finds the client providing starts the worker once and
// enters the connection in its process's refreshes; later providing reads
// start nothing more. The worker refreshes at every step, without any read:
// a round held for minutes, or reads that keep failing, never stop it.
func TestLivenessLeaseWorkerRefreshesWithoutRounds(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	writes := &livenessLeaseWritesForTest{}
	lease := newLivenessLeaseConnectionForTest(writes, &now)

	lease.afterRead(ctx, livenessLeasePublicForTest)
	lease.afterRead(ctx, livenessLeasePublicForTest)
	if len(writes.workers) != 1 {
		t.Fatalf("providing reads started %d workers, want 1", len(writes.workers))
	}
	if refreshTime, ok := livenessLeaseRefreshTimeForTest(lease); !ok || !refreshTime.Equal(now) {
		t.Fatal("the first providing read did not enter the connection in the process's refreshes")
	}
	for range 4 {
		lease.step(ctx)
	}
	if !slices.Equal(writes.writes, []string{"refresh", "refresh", "refresh", "refresh"}) {
		t.Fatalf("four worker steps with no read wrote %v, want four refreshes", writes.writes)
	}
}

// A refresh that succeeds moves the connection's entry forward; one that fails
// leaves it, so the entry ages while refreshes fail.
func TestLivenessLeaseRefreshMovesTheEntryOnlyOnSuccess(t *testing.T) {
	ctx := context.Background()
	start := time.UnixMilli(1_800_000_000_000)
	now := start
	writes := &livenessLeaseWritesForTest{}
	lease := newLivenessLeaseConnectionForTest(writes, &now)
	lease.afterRead(ctx, livenessLeasePublicForTest)

	now = start.Add(30 * time.Second)
	writes.refreshErr = errors.New("test: redis refused the write")
	lease.step(ctx)
	if refreshTime, _ := livenessLeaseRefreshTimeForTest(lease); !refreshTime.Equal(start) {
		t.Fatal("a failed refresh moved the connection's entry")
	}
	now = start.Add(60 * time.Second)
	writes.refreshErr = nil
	lease.step(ctx)
	if refreshTime, _ := livenessLeaseRefreshTimeForTest(lease); !refreshTime.Equal(now) {
		t.Fatal("a successful refresh did not move the connection's entry")
	}
}

// A client that provides in no discovery mode starts no worker and is never
// entered, whatever its reads and its end.
func TestLivenessLeaseNonProvidingClientNeverWrites(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	for _, provideModes := range []map[model.ProvideMode]bool{
		nil,
		{},
		{model.ProvideModeNone: true},
		{model.ProvideModeFriendsAndFamily: true},
		{model.ProvideModeStream: true},
	} {
		writes := &livenessLeaseWritesForTest{}
		lease := newLivenessLeaseConnectionForTest(writes, &now)
		for range 3 {
			lease.afterRead(ctx, provideModes)
			lease.step(ctx)
		}
		lease.end()
		if len(writes.workers) != 0 || len(writes.writes) != 0 {
			t.Fatalf("modes %v started %d workers and wrote %v, want nothing", provideModes, len(writes.workers), writes.writes)
		}
		if _, ok := lease.refreshes.oldest(); ok {
			t.Fatalf("modes %v entered the connection in the process's refreshes", provideModes)
		}
	}
}

// The pool FindProviders2 draws from holds Network-only providers for their
// own network's callers, so their connections lease them too.
func TestLivenessLeaseLeasesANetworkOnlyProvider(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	writes := &livenessLeaseWritesForTest{}
	lease := newLivenessLeaseConnectionForTest(writes, &now)
	lease.afterRead(ctx, map[model.ProvideMode]bool{model.ProvideModeNetwork: true})
	lease.step(ctx)
	if len(writes.workers) != 1 || !slices.Equal(writes.writes, []string{"refresh"}) {
		t.Fatalf("a Network-only provider started %d workers and wrote %v, want a worker and a refresh", len(writes.workers), writes.writes)
	}
}

// The read that finds the client no longer providing removes its entry and
// has the worker release the lease once; later steps write nothing. A client
// that provides again is entered and refreshed again.
func TestLivenessLeaseReleasesWhenTheClientStopsProviding(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	writes := &livenessLeaseWritesForTest{}
	lease := newLivenessLeaseConnectionForTest(writes, &now)
	lease.afterRead(ctx, livenessLeasePublicForTest)
	lease.step(ctx)
	lease.afterRead(ctx, map[model.ProvideMode]bool{})
	if _, ok := livenessLeaseRefreshTimeForTest(lease); ok {
		t.Fatal("the read that found the client stopping left its entry")
	}
	select {
	case <-lease.wake:
	default:
		t.Fatal("the read that found the client stopping did not wake the worker")
	}
	for range 3 {
		lease.step(ctx)
	}
	if !slices.Equal(writes.writes, []string{"refresh", "release"}) {
		t.Fatalf("the client stopping wrote %v, want one release after the refresh", writes.writes)
	}
	lease.afterRead(ctx, livenessLeasePublicForTest)
	lease.step(ctx)
	if !slices.Equal(writes.writes[2:], []string{"refresh"}) {
		t.Fatalf("the client providing again wrote %v, want a refresh", writes.writes[2:])
	}
	if _, ok := livenessLeaseRefreshTimeForTest(lease); !ok {
		t.Fatal("the client providing again was not entered")
	}
}

// A release that fails is not retried from the worker's own state: only a
// later read that still finds the client not providing requests it again.
func TestLivenessLeaseRetriesAFailedReleaseOnlyOnALaterRead(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	writes := &livenessLeaseWritesForTest{}
	lease := newLivenessLeaseConnectionForTest(writes, &now)
	lease.afterRead(ctx, livenessLeasePublicForTest)
	writes.releaseErr = errors.New("test: redis refused the write")
	lease.afterRead(ctx, map[model.ProvideMode]bool{})
	lease.step(ctx)
	lease.step(ctx)
	if !slices.Equal(writes.writes, []string{"release"}) {
		t.Fatalf("a failed release wrote %v, want it once until a later read", writes.writes)
	}
	writes.releaseErr = nil
	lease.afterRead(ctx, map[model.ProvideMode]bool{})
	lease.step(ctx)
	lease.step(ctx)
	if !slices.Equal(writes.writes, []string{"release", "release"}) {
		t.Fatalf("the later read wrote %v, want the release again, once", writes.writes)
	}
}

// The end writes nothing, a pending release included. The lease is the
// client's: when this connection's last read found the client not providing
// and its release failed, the client may provide again, and another of its
// connections may hold a fresh lease that a release at the end would delete.
// A providing connection's end leaves the lease to lapse within the window for
// the same reason. Either way the process stops vouching for the connection,
// and a refresh still in flight cannot enter it again.
func TestLivenessLeaseEndWritesNothing(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	writes := &livenessLeaseWritesForTest{}
	stale := newLivenessLeaseConnectionForTest(writes, &now)
	stale.afterRead(ctx, livenessLeasePublicForTest)
	stale.step(ctx)
	writes.releaseErr = errors.New("test: redis refused the write")
	stale.afterRead(ctx, map[model.ProvideMode]bool{})
	stale.step(ctx)
	writes.releaseErr = nil
	stale.end()
	stale.step(ctx)
	stale.afterRead(ctx, map[model.ProvideMode]bool{})
	stale.step(ctx)
	if !slices.Equal(writes.writes, []string{"refresh", "release"}) {
		t.Fatalf("the end after a failed release wrote %v, want nothing more", writes.writes)
	}

	providingWrites := &livenessLeaseWritesForTest{}
	providing := newLivenessLeaseConnectionForTest(providingWrites, &now)
	providing.afterRead(ctx, livenessLeasePublicForTest)
	providing.step(ctx)
	providing.end()
	providing.step(ctx)
	if !slices.Equal(providingWrites.writes, []string{"refresh"}) {
		t.Fatalf("the end of a providing connection wrote %v, want nothing after its refresh", providingWrites.writes)
	}
	if _, ok := providing.refreshes.oldest(); ok {
		t.Fatal("an ended connection is still entered in the process's refreshes")
	}
}

// A write that fails because the connection's context ended is the end of the
// connection, not a failed write.
func TestLivenessLeaseDoesNotCountTheWritesOfAnEndedConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	now := time.UnixMilli(1_800_000_000_000)
	writes := &livenessLeaseWritesForTest{}
	lease := newLivenessLeaseConnectionForTest(writes, &now)
	outcomes := []string{}
	lease.writtenForTest = func(outcome string) {
		outcomes = append(outcomes, outcome)
	}
	lease.afterRead(ctx, livenessLeasePublicForTest)
	lease.step(ctx)
	cancel()
	writes.refreshErr = context.Canceled
	failedBefore := testutil.ToFloat64(connectionLivenessLeaseWritesCounter.WithLabelValues(livenessLeaseOutcomeRefreshFailed))
	lease.step(ctx)
	if !slices.Equal(outcomes, []string{livenessLeaseOutcomeRefreshed}) {
		t.Fatalf("observed %v, want the one refresh before the context ended", outcomes)
	}
	if testutil.ToFloat64(connectionLivenessLeaseWritesCounter.WithLabelValues(livenessLeaseOutcomeRefreshFailed)) != failedBefore {
		t.Fatal("a write of an ended connection was counted failed")
	}
}

// The handler stamp vouches for the process's refreshes. A connection entered
// at its first providing read whose refreshes fail, or never run, stops the
// stamps once its last success is more than the withhold age old, and a
// success resumes them; a process with no providing connection stamps.
func TestProviderLivenessWriterWithholdsTheStampWhileARefreshIsStale(t *testing.T) {
	ctx := context.Background()
	handlerId := server.NewId()
	stamps := 0
	stamp := func(_ context.Context, stampHandlerId server.Id) error {
		if stampHandlerId != handlerId {
			t.Fatalf("stamped handler %s, want %s", stampHandlerId, handlerId)
		}
		stamps += 1
		return nil
	}
	start := time.UnixMilli(1_800_000_000_000)
	now := start
	writes := &livenessLeaseWritesForTest{}
	lease := newLivenessLeaseConnectionForTest(writes, &now)
	refreshes := lease.refreshes
	withholdAge := model.ProviderLivenessWriterWithholdAge

	if outcome := stampProviderLivenessWriter(ctx, handlerId, refreshes, start, stamp); outcome != livenessLeaseOutcomeStamped {
		t.Fatalf("a process with no providing connection: %s, want stamped", outcome)
	}
	// entered at its first providing read; its refreshes then fail
	lease.afterRead(ctx, livenessLeasePublicForTest)
	writes.refreshErr = errors.New("test: redis refused the write")
	for _, c := range []struct {
		since   time.Duration
		outcome string
	}{
		{since: 0, outcome: livenessLeaseOutcomeStamped},
		{since: withholdAge, outcome: livenessLeaseOutcomeStamped},
		{since: withholdAge + time.Millisecond, outcome: livenessLeaseOutcomeStampWithheld},
		{since: 10 * withholdAge, outcome: livenessLeaseOutcomeStampWithheld},
	} {
		now = start.Add(c.since)
		lease.step(ctx)
		if outcome := stampProviderLivenessWriter(ctx, handlerId, refreshes, now, stamp); outcome != c.outcome {
			t.Fatalf("%s after the last success: %s, want %s", c.since, outcome, c.outcome)
		}
	}
	writes.refreshErr = nil
	lease.step(ctx)
	if outcome := stampProviderLivenessWriter(ctx, handlerId, refreshes, now, stamp); outcome != livenessLeaseOutcomeStamped {
		t.Fatalf("after a successful refresh: %s, want stamped", outcome)
	}

	// a connection whose worker never runs: no refresh at all
	idleWrites := &livenessLeaseWritesForTest{}
	idle := newLivenessLeaseConnectionForTest(idleWrites, &now)
	idle.refreshes = refreshes
	idle.afterRead(ctx, livenessLeasePublicForTest)
	if outcome := stampProviderLivenessWriter(ctx, handlerId, refreshes, now.Add(withholdAge+time.Millisecond), stamp); outcome != livenessLeaseOutcomeStampWithheld {
		t.Fatalf("a connection that never refreshed: %s, want stamp_withheld", outcome)
	}
	idle.end()
	if outcome := stampProviderLivenessWriter(ctx, handlerId, refreshes, now, stamp); outcome != livenessLeaseOutcomeStamped {
		t.Fatalf("after the idle connection ended: %s, want stamped", outcome)
	}
	if stamps != 5 {
		t.Fatalf("stamped %d times, want the 5 that were not withheld", stamps)
	}
}

// The default cadence fits the contract: a connection that misses one
// refresh does not reach the withhold age.
func TestLivenessLeaseDefaultCadenceFitsTheWithholdAge(t *testing.T) {
	lease := newProviderLivenessLeaseConnection(server.NewId(), 0, func(func()) bool { return false }, nil)
	if lease.refreshInterval != model.ProviderLivenessLeaseRefreshInterval {
		t.Fatalf("the default refresh interval is %s, want %s", lease.refreshInterval, model.ProviderLivenessLeaseRefreshInterval)
	}
	if model.ProviderLivenessWriterWithholdAge <= 2*(lease.refreshInterval+model.ProviderLivenessLeaseWriteTimeout) {
		t.Fatalf("one missed refresh reaches the %s withhold age", model.ProviderLivenessWriterWithholdAge)
	}
}

// The handler is stamped at once, then every interval, a failed stamp counted
// and stamped again, until its context ends.
func TestProviderLivenessWriterStampsUntilItsContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	handlerId := server.NewId()
	stampLock := sync.Mutex{}
	stampCount := 0
	stamped := make(chan int, 16)
	failedBefore := testutil.ToFloat64(connectionLivenessLeaseWritesCounter.WithLabelValues(livenessLeaseOutcomeStampFailed))
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		runProviderLivenessWriter(ctx, handlerId, time.Millisecond, newProviderLivenessRefreshes(), func(stampCtx context.Context, stampHandlerId server.Id) error {
			if stampHandlerId != handlerId {
				t.Errorf("stamped handler %s, want %s", stampHandlerId, handlerId)
			}
			stampLock.Lock()
			stampCount += 1
			count := stampCount
			stampLock.Unlock()
			select {
			case stamped <- count:
			default:
			}
			if count == 2 {
				return errors.New("test: redis refused the write")
			}
			return nil
		})
	}()
	for want := 1; want <= 3; want += 1 {
		select {
		case count := <-stamped:
			if count != want {
				t.Fatalf("stamp %d, want %d", count, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for stamp %d", want)
		}
	}
	cancel()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("the stamp loop did not end with its context")
	}
	if failed := testutil.ToFloat64(connectionLivenessLeaseWritesCounter.WithLabelValues(livenessLeaseOutcomeStampFailed)) - failedBefore; failed != 1 {
		t.Fatalf("counted %v failed stamps, want 1", failed)
	}
}
