// The provider liveness lease writes of a live announcement, on the database
// and Redis of the test env: its worker refreshes the lease while its round is
// held and through a failed provide read, a client that does not provide never
// writes, a client that stops providing is released, and one connection of two
// ending leaves the lease to the other.
package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// One announcement of a client with a short sync interval and the given lease
// refresh interval. Each lease write's outcome is sent on writes. With
// holdRounds, each recorded stats round is sent on rounds and the run goroutine
// then waits for release, as in the sync tests, so the test decides what the
// next round meets.
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
func startLivenessAnnounceForTest(ctx context.Context, networkId server.Id, clientId server.Id, refreshInterval time.Duration, holdRounds bool) *livenessAnnounceForTest {
	transportCtx, transportCancel := context.WithCancel(ctx)
	fixture := &livenessAnnounceForTest{
		transportCtx:    transportCtx,
		transportCancel: transportCancel,
		registered:      make(chan server.Id, 1),
		writes:          make(chan string, 4096),
		rounds:          make(chan model.ClientReliabilityStats),
		release:         make(chan struct{}),
	}
	settings := DefaultConnectionAnnounceSettings()
	settings.SyncConnectionTimeout = 50 * time.Millisecond
	settings.LocationRetryTimeout = 0
	settings.livenessLeaseRefreshIntervalForTest = refreshInterval
	settings.verifySettingsForTest = func() (*model.VerifySettings, bool) {
		return nil, false
	}
	settings.connectionRegisteredForTest = func(announce *ConnectionAnnounce) {
		fixture.registered <- *announce.ConnectionId()
	}
	settings.livenessLeaseWrittenForTest = func(outcome string) {
		select {
		case fixture.writes <- outcome:
		default:
		}
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

// Waits for the next write of the outcome, skipping others.
func (self *livenessAnnounceForTest) awaitOutcome(t testing.TB, testCtx context.Context, want string) {
	t.Helper()
	for {
		select {
		case outcome := <-self.writes:
			if outcome == want {
				return
			}
		case <-self.announce.done:
			t.Fatalf("the announcement ended before writing %s", want)
		case <-testCtx.Done():
			t.Fatalf("timed out waiting for a lease write: %s", want)
		}
	}
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

// The round goroutine is held inside its first round, as a database
// acquisition without a deadline or a failed read's retries and backoff hold
// it, for minutes. The connection's worker keeps refreshing the lease all the
// while. Refreshed by the rounds before this change, the lease stopped with
// them: a round held past the window left a live provider leaseless.
func TestConnectionAnnounceRefreshesLeaseWhileItsRoundIsHeld(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		networkId, clientId := createLivenessClientForTest(ctx, model.ProvideModePublic)
		fixture := startLivenessAnnounceForTest(ctx, networkId, clientId, 20*time.Millisecond, true)
		defer fixture.close()
		fixture.awaitRegistered(t, testCtx)
		fixture.awaitRound(t, testCtx)
		// held from here on
		fixture.takeWrites()
		for range 3 {
			fixture.awaitOutcome(t, testCtx, livenessLeaseOutcomeRefreshed)
		}
		if _, ok := model.Testing_GetProviderLivenessLease(ctx, clientId); !ok {
			t.Fatal("the providing client has no lease")
		}
		fixture.release <- struct{}{}
	})
}

// A round whose provide read fails changes nothing of the lease: the worker
// refreshes it through the failed round and after it.
func TestConnectionAnnounceRefreshesLeaseThroughFailedSyncRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		networkId, clientId := createLivenessClientForTest(ctx, model.ProvideModePublic)
		fixture := startLivenessAnnounceForTest(ctx, networkId, clientId, 20*time.Millisecond, true)
		defer fixture.close()
		fixture.awaitRegistered(t, testCtx)
		fixture.awaitRound(t, testCtx)
		failNextProvideKeyChangeReadForTest(ctx)
		fixture.release <- struct{}{}

		// the failed read, then refreshes
		for provideKeyChangeReadCountForTest(ctx) < 1 {
			select {
			case <-fixture.announce.done:
				t.Fatal("the announcement ended")
			case <-testCtx.Done():
				t.Fatal("timed out waiting for the failed read")
			case <-time.After(10 * time.Millisecond):
			}
		}
		fixture.takeWrites()
		for range 2 {
			fixture.awaitOutcome(t, testCtx, livenessLeaseOutcomeRefreshed)
		}
		// the round after the failed one reads and records its stats
		fixture.awaitRound(t, testCtx)
		if count := provideKeyChangeReadCountForTest(ctx); count < 2 {
			t.Fatalf("provide_key_change was read %d times, want the failed read and a later one", count)
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
		fixture := startLivenessAnnounceForTest(ctx, networkId, clientId, 20*time.Millisecond, false)
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

// The read that finds the client no longer providing has the worker release
// its lease.
func TestConnectionAnnounceReleasesLeaseWhenProvideStops(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		networkId, clientId := createLivenessClientForTest(ctx, model.ProvideModePublic)
		fixture := startLivenessAnnounceForTest(ctx, networkId, clientId, time.Hour, true)
		defer fixture.close()
		fixture.awaitRegistered(t, testCtx)
		fixture.awaitRound(t, testCtx)
		fixture.awaitOutcome(t, testCtx, livenessLeaseOutcomeRefreshed)
		if _, ok := model.Testing_GetProviderLivenessLease(ctx, clientId); !ok {
			t.Fatal("the providing client has no lease")
		}

		model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{})
		fixture.release <- struct{}{}
		// the change is counted, so the round records stats
		fixture.awaitRound(t, testCtx)
		fixture.awaitOutcome(t, testCtx, livenessLeaseOutcomeReleased)
		if _, ok := model.Testing_GetProviderLivenessLease(ctx, clientId); ok {
			t.Fatal("the lease of a client that stopped providing remains")
		}
		fixture.release <- struct{}{}
	})
}

// Two connections of one client hold its lease. One ends: it writes nothing,
// and the lease stays, at the latest refresh of either; the other, refreshing
// only once an hour, cannot have written it since. A release at the end would
// have dropped the client from every draw until the other's next refresh.
func TestConnectionAnnounceLeaseOutlivesOneOfTwoConnections(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		networkId, clientId := createLivenessClientForTest(ctx, model.ProvideModePublic)
		ending := startLivenessAnnounceForTest(ctx, networkId, clientId, time.Hour, false)
		defer ending.close()
		staying := startLivenessAnnounceForTest(ctx, networkId, clientId, time.Hour, false)
		defer staying.close()
		ending.awaitRegistered(t, testCtx)
		staying.awaitRegistered(t, testCtx)
		ending.awaitOutcome(t, testCtx, livenessLeaseOutcomeRefreshed)
		staying.awaitOutcome(t, testCtx, livenessLeaseOutcomeRefreshed)
		before, ok := model.Testing_GetProviderLivenessLease(ctx, clientId)
		if !ok {
			t.Fatal("the providing client has no lease")
		}

		ending.close()
		for _, outcome := range ending.takeWrites() {
			t.Fatalf("the ending connection wrote %s", outcome)
		}
		after, ok := model.Testing_GetProviderLivenessLease(ctx, clientId)
		if !ok {
			t.Fatal("one connection's end removed the lease the other connection holds")
		}
		if !after.Equal(before) {
			t.Fatalf("the lease moved from %s to %s with no refresh", before, after)
		}
	})
}
