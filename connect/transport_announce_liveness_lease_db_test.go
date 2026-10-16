// The provider liveness lease writes of a live announcement's sync rounds, on
// the database and Redis of the test env: a round whose provide read fails
// still refreshes, a client that does not provide never writes, a client that
// stops providing is released, and one connection of two ending leaves the
// lease to the other.
package connect

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// One announcement of a client with a short sync interval. Each lease write's
// outcome is sent on writes. With holdRounds, each recorded stats round is sent
// on rounds and the run goroutine then waits for release, as in the sync
// tests, so the test decides what the next round meets.
type livenessAnnounceForTest struct {
	transportCtx    context.Context
	transportCancel context.CancelFunc
	registered      chan server.Id
	writes          chan string
	rounds          chan model.ClientReliabilityStats
	release         chan struct{}
	announce        *ConnectionAnnounce
}

// Creates a device for a new client and sets its provide keys.
func createLivenessClientForTest(ctx context.Context, provideModes ...model.ProvideMode) (networkId server.Id, clientId server.Id) {
	networkId = server.NewId()
	clientId = server.NewId()
	model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
	secretKeys := map[model.ProvideMode][]byte{}
	for _, provideMode := range provideModes {
		secretKeys[provideMode] = make([]byte, 32)
	}
	model.SetProvide(ctx, clientId, secretKeys)
	return
}

// Announces one connection of the client at once.
func startLivenessAnnounceForTest(ctx context.Context, networkId server.Id, clientId server.Id, holdRounds bool) *livenessAnnounceForTest {
	transportCtx, transportCancel := context.WithCancel(ctx)
	fixture := &livenessAnnounceForTest{
		transportCtx:    transportCtx,
		transportCancel: transportCancel,
		registered:      make(chan server.Id, 1),
		writes:          make(chan string, 1024),
		rounds:          make(chan model.ClientReliabilityStats),
		release:         make(chan struct{}),
	}
	settings := DefaultConnectionAnnounceSettings()
	settings.SyncConnectionTimeout = 50 * time.Millisecond
	settings.LocationRetryTimeout = 0
	settings.verifySettingsForTest = func() (*model.VerifySettings, bool) {
		return nil, false
	}
	settings.connectionRegisteredForTest = func(announce *ConnectionAnnounce) {
		fixture.registered <- *announce.ConnectionId()
	}
	settings.livenessLeaseWrittenForTest = func(outcome string) {
		fixture.writes <- outcome
	}
	if holdRounds {
		settings.reliabilityStatsRecordedForTest = func(_ server.Id, stats model.ClientReliabilityStats) {
			select {
			case fixture.rounds <- stats:
			case <-transportCtx.Done():
				return
			}
			select {
			case <-fixture.release:
			case <-transportCtx.Done():
			}
		}
	}
	fixture.announce = NewConnectionAnnounceWithIpFamily(
		controller.Testing_WithConnectionLocationWriter(
			transportCtx,
			func(context.Context, server.Id, string) error {
				return nil
			},
		),
		transportCancel,
		networkId,
		clientId,
		locationAnnounceClientAddress,
		0,
		"",
		server.NewId(),
		0,
		&TestConfig{},
		settings,
	)
	return fixture
}

func (self *livenessAnnounceForTest) awaitRegistered(t testing.TB, testCtx context.Context) {
	t.Helper()
	select {
	case <-self.registered:
	case <-self.announce.done:
		t.Fatal("the announcement ended before registering its connection")
	case <-testCtx.Done():
		t.Fatal("timed out waiting for the announcement to register")
	}
}

func (self *livenessAnnounceForTest) awaitRound(t testing.TB, testCtx context.Context) {
	t.Helper()
	select {
	case <-self.rounds:
	case <-self.announce.done:
		t.Fatal("the announcement ended before recording a sync round")
	case <-testCtx.Done():
		t.Fatal("timed out waiting for a sync round")
	}
}

// The next write, waiting for it.
func (self *livenessAnnounceForTest) awaitWrite(t testing.TB, testCtx context.Context) string {
	t.Helper()
	select {
	case outcome := <-self.writes:
		return outcome
	case <-self.announce.done:
		t.Fatal("the announcement ended before writing its lease")
	case <-testCtx.Done():
		t.Fatal("timed out waiting for a lease write")
	}
	panic("unreachable")
}

// The writes so far, without waiting.
func (self *livenessAnnounceForTest) takeWrites() []string {
	outcomes := []string{}
	for {
		select {
		case outcome := <-self.writes:
			outcomes = append(outcomes, outcome)
		default:
			return outcomes
		}
	}
}

// Ends the transport and joins the announcement, whatever round it holds.
func (self *livenessAnnounceForTest) close() {
	self.transportCancel()
	self.announce.CloseAndWait()
}

// The round after a held one meets a failed provide read, and still refreshes
// the lease, before the read; so does the round after it. Before this change
// no round wrote a lease; refreshed only after a successful read, the failed
// round would have written nothing.
func TestConnectionAnnounceRefreshesLeaseThroughFailedSyncRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		networkId, clientId := createLivenessClientForTest(ctx, model.ProvideModePublic)
		fixture := startLivenessAnnounceForTest(ctx, networkId, clientId, true)
		defer fixture.close()
		fixture.awaitRegistered(t, testCtx)

		fixture.awaitRound(t, testCtx)
		if writes := fixture.takeWrites(); !slices.Equal(writes, []string{livenessLeaseOutcomeRefreshed}) {
			t.Fatalf("the first round wrote %v, want one refresh once it read the client providing", writes)
		}
		failNextProvideKeyChangeReadForTest(ctx)
		fixture.release <- struct{}{}

		// the failed round records no stats; the round after it does
		fixture.awaitRound(t, testCtx)
		if count := provideKeyChangeReadCountForTest(ctx); count != 2 {
			t.Fatalf("provide_key_change was read %d times, want the failed read and the next round's", count)
		}
		if writes := fixture.takeWrites(); !slices.Equal(writes, []string{livenessLeaseOutcomeRefreshed, livenessLeaseOutcomeRefreshed}) {
			t.Fatalf("the failed round and the next wrote %v, want a refresh each, before each read", writes)
		}
		if _, ok := model.Testing_GetProviderLivenessLease(ctx, clientId); !ok {
			t.Fatal("the providing client has no lease")
		}
		fixture.release <- struct{}{}
	})
}

// A client without a provide mode for discovery never writes a lease, round
// after round.
func TestConnectionAnnounceNonProvidingClientWritesNoLease(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		networkId, clientId := createLivenessClientForTest(ctx)
		// counts the rounds' reads; the first fails, which writes nothing either
		failNextProvideKeyChangeReadForTest(ctx)
		fixture := startLivenessAnnounceForTest(ctx, networkId, clientId, false)
		defer fixture.close()
		fixture.awaitRegistered(t, testCtx)
		for provideKeyChangeReadCountForTest(ctx) < 4 {
			select {
			case <-fixture.announce.done:
				t.Fatal("the announcement ended")
			case <-testCtx.Done():
				t.Fatal("timed out waiting for three successful rounds")
			case <-time.After(10 * time.Millisecond):
			}
		}
		if writes := fixture.takeWrites(); len(writes) != 0 {
			t.Fatalf("a non-providing client wrote %v", writes)
		}
		if _, ok := model.Testing_GetProviderLivenessLease(ctx, clientId); ok {
			t.Fatal("a non-providing client has a lease")
		}
	})
}

// The read that finds the client no longer providing releases its lease.
func TestConnectionAnnounceReleasesLeaseWhenProvideStops(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		networkId, clientId := createLivenessClientForTest(ctx, model.ProvideModePublic)
		fixture := startLivenessAnnounceForTest(ctx, networkId, clientId, true)
		defer fixture.close()
		fixture.awaitRegistered(t, testCtx)
		fixture.awaitRound(t, testCtx)
		if _, ok := model.Testing_GetProviderLivenessLease(ctx, clientId); !ok {
			t.Fatal("the providing client has no lease")
		}
		fixture.takeWrites()

		model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{})
		fixture.release <- struct{}{}
		// the change is counted, so the round records stats
		fixture.awaitRound(t, testCtx)
		if writes := fixture.takeWrites(); !slices.Equal(writes, []string{livenessLeaseOutcomeRefreshed, livenessLeaseOutcomeReleased}) {
			t.Fatalf("the round that read the client stopping wrote %v, want its refresh, then the release", writes)
		}
		if _, ok := model.Testing_GetProviderLivenessLease(ctx, clientId); ok {
			t.Fatal("the lease of a client that stopped providing remains")
		}
		fixture.release <- struct{}{}
	})
}

// Two connections of one client hold its lease. One ends: it writes nothing,
// and the lease stays, scored by the latest refresh of either, while the other
// is held between rounds and cannot refresh it. The other's next round
// refreshes it again. A release at the end would have dropped the client from
// every draw until then.
func TestConnectionAnnounceLeaseOutlivesOneOfTwoConnections(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		networkId, clientId := createLivenessClientForTest(ctx, model.ProvideModePublic)
		ending := startLivenessAnnounceForTest(ctx, networkId, clientId, false)
		defer ending.close()
		staying := startLivenessAnnounceForTest(ctx, networkId, clientId, true)
		defer staying.close()
		ending.awaitRegistered(t, testCtx)
		staying.awaitRegistered(t, testCtx)

		if outcome := ending.awaitWrite(t, testCtx); outcome != livenessLeaseOutcomeRefreshed {
			t.Fatalf("the ending connection wrote %s, want a refresh", outcome)
		}
		// the staying connection's round, held: it refreshed, and waits
		staying.awaitRound(t, testCtx)
		if writes := staying.takeWrites(); !slices.Equal(writes, []string{livenessLeaseOutcomeRefreshed}) {
			t.Fatalf("the staying connection wrote %v, want a refresh", writes)
		}

		ending.close()
		for _, outcome := range ending.takeWrites() {
			if outcome != livenessLeaseOutcomeRefreshed {
				t.Fatalf("the ending connection wrote %s", outcome)
			}
		}
		endedTime, ok := model.Testing_GetProviderLivenessLease(ctx, clientId)
		if !ok {
			t.Fatal("one connection's end removed the lease the other connection holds")
		}

		staying.release <- struct{}{}
		if outcome := staying.awaitWrite(t, testCtx); outcome != livenessLeaseOutcomeRefreshed {
			t.Fatalf("the staying connection wrote %s, want a refresh", outcome)
		}
		refreshTime, ok := model.Testing_GetProviderLivenessLease(ctx, clientId)
		if !ok || refreshTime.Before(endedTime) {
			t.Fatalf("the staying connection's refresh scored the lease %s, want at or after %s", refreshTime, endedTime)
		}
		staying.awaitRound(t, testCtx)
		staying.release <- struct{}{}
	})
}
