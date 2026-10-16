package connect

// One announcement's part in its client's provider liveness lease
// (transport_announce_liveness_lease.go), with its writes recorded instead of
// sent: when a round refreshes, which clients write at all, when the lease is
// released, and that the end of a providing connection leaves the lease to the
// client's other connections. And the handler stamp loop.

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
}

// A connection whose lease writes go to writes.
func newLivenessLeaseConnectionForTest(writes *livenessLeaseWritesForTest) *providerLivenessLeaseConnection {
	lease := newProviderLivenessLeaseConnection(server.NewId(), nil)
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

// While the client provides, every round refreshes before its read; the first
// read that finds it providing refreshes at once.
func TestLivenessLeaseRefreshesEveryRoundBeforeItsRead(t *testing.T) {
	ctx := context.Background()
	writes := &livenessLeaseWritesForTest{}
	lease := newLivenessLeaseConnectionForTest(writes)

	// the first round: nothing known before its read
	lease.beforeRead(ctx)
	lease.afterRead(ctx, livenessLeasePublicForTest)
	if !slices.Equal(writes.writes, []string{"refresh"}) {
		t.Fatalf("the first read that found the client providing wrote %v, want one refresh", writes.writes)
	}
	for round := 2; round <= 4; round += 1 {
		lease.beforeRead(ctx)
		if len(writes.writes) != round {
			t.Fatalf("round %d wrote %v before its read, want a refresh", round, writes.writes)
		}
		lease.afterRead(ctx, livenessLeasePublicForTest)
		if len(writes.writes) != round {
			t.Fatalf("round %d wrote %v after its read, want nothing more", round, writes.writes)
		}
	}
}

// A round whose read fails takes no provide modes, and still refreshes the
// lease of a client the last read found providing, as do the rounds after it.
// A refresh that fails is written again next round.
func TestLivenessLeaseRefreshesThroughFailedReads(t *testing.T) {
	ctx := context.Background()
	writes := &livenessLeaseWritesForTest{}
	lease := newLivenessLeaseConnectionForTest(writes)
	lease.beforeRead(ctx)
	lease.afterRead(ctx, livenessLeasePublicForTest)

	// three rounds whose reads fail: no afterRead
	for range 3 {
		lease.beforeRead(ctx)
	}
	if !slices.Equal(writes.writes, []string{"refresh", "refresh", "refresh", "refresh"}) {
		t.Fatalf("rounds whose reads failed wrote %v, want a refresh each", writes.writes)
	}

	writes.refreshErr = errors.New("test: redis refused the write")
	lease.beforeRead(ctx)
	writes.refreshErr = nil
	lease.beforeRead(ctx)
	if len(writes.writes) != 6 {
		t.Fatalf("a failed refresh was not written again next round: %v", writes.writes)
	}
}

// A client that provides in no discovery mode never writes: not before a read,
// not after one, not at the end.
func TestLivenessLeaseNonProvidingClientNeverWrites(t *testing.T) {
	ctx := context.Background()
	for _, provideModes := range []map[model.ProvideMode]bool{
		nil,
		{},
		{model.ProvideModeNone: true},
		{model.ProvideModeFriendsAndFamily: true},
		{model.ProvideModeStream: true},
	} {
		writes := &livenessLeaseWritesForTest{}
		lease := newLivenessLeaseConnectionForTest(writes)
		for range 3 {
			lease.beforeRead(ctx)
			lease.afterRead(ctx, provideModes)
		}
		lease.beforeRead(ctx)
		lease.end(ctx)
		if len(writes.writes) != 0 {
			t.Fatalf("modes %v wrote %v, want nothing", provideModes, writes.writes)
		}
	}
}

// The pool FindProviders2 draws from holds Network-only providers for their
// own network's callers, so their connections lease them too.
func TestLivenessLeaseLeasesANetworkOnlyProvider(t *testing.T) {
	ctx := context.Background()
	writes := &livenessLeaseWritesForTest{}
	lease := newLivenessLeaseConnectionForTest(writes)
	lease.beforeRead(ctx)
	lease.afterRead(ctx, map[model.ProvideMode]bool{model.ProvideModeNetwork: true})
	lease.beforeRead(ctx)
	if !slices.Equal(writes.writes, []string{"refresh", "refresh"}) {
		t.Fatalf("a Network-only provider wrote %v, want a refresh each round", writes.writes)
	}
}

// The read that finds the client no longer providing releases the lease once,
// and the rounds after it write nothing; a client that provides again is
// refreshed at once.
func TestLivenessLeaseReleasesWhenTheClientStopsProviding(t *testing.T) {
	ctx := context.Background()
	writes := &livenessLeaseWritesForTest{}
	lease := newLivenessLeaseConnectionForTest(writes)
	lease.beforeRead(ctx)
	lease.afterRead(ctx, livenessLeasePublicForTest)
	lease.beforeRead(ctx)
	lease.afterRead(ctx, map[model.ProvideMode]bool{})
	if !slices.Equal(writes.writes, []string{"refresh", "refresh", "release"}) {
		t.Fatalf("the client stopping wrote %v, want the round's refresh and then a release", writes.writes)
	}
	for range 2 {
		lease.beforeRead(ctx)
		lease.afterRead(ctx, map[model.ProvideMode]bool{})
	}
	lease.end(ctx)
	if len(writes.writes) != 3 {
		t.Fatalf("a released, non-providing connection wrote again: %v", writes.writes)
	}
	lease.beforeRead(ctx)
	lease.afterRead(ctx, livenessLeasePublicForTest)
	if !slices.Equal(writes.writes[3:], []string{"refresh"}) {
		t.Fatalf("the client providing again wrote %v, want a refresh", writes.writes[3:])
	}
}

// A release that fails is written again by the next round that still finds
// the client not providing, and at the end.
func TestLivenessLeaseRetriesAFailedRelease(t *testing.T) {
	ctx := context.Background()
	writes := &livenessLeaseWritesForTest{}
	lease := newLivenessLeaseConnectionForTest(writes)
	lease.beforeRead(ctx)
	lease.afterRead(ctx, livenessLeasePublicForTest)
	writes.releaseErr = errors.New("test: redis refused the write")
	lease.beforeRead(ctx)
	lease.afterRead(ctx, map[model.ProvideMode]bool{})
	lease.beforeRead(ctx)
	lease.afterRead(ctx, map[model.ProvideMode]bool{})
	if !slices.Equal(writes.writes, []string{"refresh", "refresh", "release", "release"}) {
		t.Fatalf("a failed release wrote %v, want it written again next round", writes.writes)
	}
	lease.end(ctx)
	if !slices.Equal(writes.writes[4:], []string{"release"}) {
		t.Fatalf("the end after a failed release wrote %v, want it written again", writes.writes[4:])
	}
	writes.releaseErr = nil
	lease.end(ctx)
	lease.end(ctx)
	if !slices.Equal(writes.writes[5:], []string{"release"}) {
		t.Fatalf("the ends after a failed release wrote %v, want one more release and then nothing", writes.writes[5:])
	}
}

// The end of a providing connection writes nothing. The lease is the
// client's: another connection of the client may hold it, or the connection
// the client reconnects with may need it until its first refresh, and a
// release here would drop the client from every draw until then. The lease
// lapses within the window instead.
func TestLivenessLeaseEndOfAProvidingConnectionLeavesTheLease(t *testing.T) {
	ctx := context.Background()
	writes := &livenessLeaseWritesForTest{}
	lease := newLivenessLeaseConnectionForTest(writes)
	lease.beforeRead(ctx)
	lease.afterRead(ctx, livenessLeasePublicForTest)
	lease.beforeRead(ctx)
	lease.end(ctx)
	if !slices.Equal(writes.writes, []string{"refresh", "refresh"}) {
		t.Fatalf("the end of a providing connection wrote %v, want nothing after its refreshes", writes.writes)
	}
}

// A write that fails because the connection's context ended is the end of the
// connection, not a failed write.
func TestLivenessLeaseDoesNotCountTheWritesOfAnEndedConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	writes := &livenessLeaseWritesForTest{}
	lease := newLivenessLeaseConnectionForTest(writes)
	outcomes := []string{}
	lease.writtenForTest = func(outcome string) {
		outcomes = append(outcomes, outcome)
	}
	lease.beforeRead(ctx)
	lease.afterRead(ctx, livenessLeasePublicForTest)
	cancel()
	writes.refreshErr = context.Canceled
	failedBefore := testutil.ToFloat64(connectionLivenessLeaseWritesCounter.WithLabelValues(livenessLeaseOutcomeRefreshFailed))
	lease.beforeRead(ctx)
	if !slices.Equal(outcomes, []string{livenessLeaseOutcomeRefreshed}) {
		t.Fatalf("observed %v, want the one refresh before the context ended", outcomes)
	}
	if testutil.ToFloat64(connectionLivenessLeaseWritesCounter.WithLabelValues(livenessLeaseOutcomeRefreshFailed)) != failedBefore {
		t.Fatal("a write of an ended connection was counted failed")
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
		runProviderLivenessWriter(ctx, handlerId, time.Millisecond, func(stampCtx context.Context, stampHandlerId server.Id) error {
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
