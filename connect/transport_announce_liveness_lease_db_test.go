// The provider liveness lease writes of live announcements, on the database
// and Redis of the test env: the process's writer extends a reconnecting
// provider's lease while its new connection's reads fail, keeps refreshing
// while a round is held and through a failed provide read, writes nothing for
// a client that does not provide, releases a client that stops providing,
// leaves the lease to the other of two connections when one ends, and never
// holds the deadline-bound pool against an admission call.
package connect

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// One announcement of a client with a short sync interval, written by writer.
// Each lease write's outcome for the connection is sent on writes. With
// holdRounds, each recorded stats round is sent on rounds and the run
// goroutine then waits for release, as in the sync tests, so the test decides
// what the next round meets.
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

// A process writer that writes every refreshInterval, running until the test
// ends.
func startLivenessWriterForTest(t testing.TB, refreshInterval time.Duration) *providerLivenessLeaseWriter {
	writer := newProviderLivenessLeaseWriter(refreshInterval)
	writer.jitter = func() time.Duration {
		return 0
	}
	writerCtx, writerCancel := context.WithCancel(context.Background())
	t.Cleanup(writerCancel)
	writer.start(writerCtx)
	return writer
}

// Announces one connection of the client at once.
func startLivenessAnnounceForTest(ctx context.Context, networkId server.Id, clientId server.Id, writer *providerLivenessLeaseWriter, holdRounds bool) *livenessAnnounceForTest {
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
	settings.livenessLeaseWriterForTest = writer
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

// Makes every read of provide_key_change fail from now, counted in the same
// sequence as failNextProvideKeyChangeReadForTest.
func failEveryProvideKeyChangeReadForTest(ctx context.Context) {
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `
			ALTER TABLE provide_key_change RENAME TO test_provide_key_change;
			CREATE SEQUENCE test_provide_key_change_reads;
			CREATE FUNCTION test_provide_key_change_rows() RETURNS SETOF test_provide_key_change
			LANGUAGE plpgsql AS $rows$
			BEGIN
				PERFORM nextval('test_provide_key_change_reads');
				RAISE EXCEPTION 'test: the database refused this read';
			END
			$rows$;
			CREATE VIEW provide_key_change AS SELECT * FROM test_provide_key_change_rows();
		`))
	})
}

// A provider reconnects: its last connection's lease is in the shard, and the
// new connection's provide reads keep failing, so no read says the client
// provides. The process vouches for the new connection from its registration
// and extends the lease it finds, so the provider stays leased however long
// the reads fail. Before this change nothing was written until a read
// succeeded, and two failed first reads let the lease lapse while the process
// went on stamping.
func TestConnectionAnnounceExtendsAReconnectingProvidersLeaseThroughFailedReads(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		networkId, clientId := createLivenessClientForTest(ctx, model.ProvideModePublic)
		// the lease of the connection the provider reconnects from
		if err := model.RefreshProviderLivenessLease(ctx, clientId); err != nil {
			t.Fatal(err)
		}
		lastLease, _ := model.Testing_GetProviderLivenessLease(ctx, clientId)
		failEveryProvideKeyChangeReadForTest(ctx)
		writer := startLivenessWriterForTest(t, 50*time.Millisecond)
		fixture := startLivenessAnnounceForTest(ctx, networkId, clientId, writer, false)
		defer fixture.close()
		fixture.awaitRegistered(t, testCtx)
		for range 3 {
			fixture.awaitOutcome(t, testCtx, livenessLeaseOutcomeExtended)
		}
		if count := provideKeyChangeReadCountForTest(ctx); count < 1 {
			t.Fatalf("provide_key_change was read %d times, want failed reads", count)
		}
		extendedLease, ok := model.Testing_GetProviderLivenessLease(ctx, clientId)
		if !ok || !lastLease.Before(extendedLease) {
			t.Fatalf("the lease is at %s, want extended past %s while every read fails", extendedLease, lastLease)
		}
		if _, vouched := writer.oldest(); !vouched {
			t.Fatal("the process does not vouch for the connection whose reads fail")
		}
	})
}

// The round goroutine is held inside its first round, as a database
// acquisition without a deadline or a failed read's retries and backoff hold
// it, for minutes. The writer keeps refreshing the lease all the while.
// Refreshed by the rounds before 5028d4389's successor, the lease stopped with
// them.
func TestConnectionAnnounceRefreshesLeaseWhileItsRoundIsHeld(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		networkId, clientId := createLivenessClientForTest(ctx, model.ProvideModePublic)
		writer := startLivenessWriterForTest(t, 20*time.Millisecond)
		fixture := startLivenessAnnounceForTest(ctx, networkId, clientId, writer, true)
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

// A round whose provide read fails changes nothing of the lease: the writer
// refreshes it through the failed round and after it.
func TestConnectionAnnounceRefreshesLeaseThroughFailedSyncRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		networkId, clientId := createLivenessClientForTest(ctx, model.ProvideModePublic)
		writer := startLivenessWriterForTest(t, 20*time.Millisecond)
		fixture := startLivenessAnnounceForTest(ctx, networkId, clientId, writer, true)
		defer fixture.close()
		fixture.awaitRegistered(t, testCtx)
		fixture.awaitRound(t, testCtx)
		failNextProvideKeyChangeReadForTest(ctx)
		fixture.release <- struct{}{}

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

// A client without a provide mode for discovery is never scored, round after
// round, and gets no lease.
func TestConnectionAnnounceNonProvidingClientWritesNoLease(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		networkId, clientId := createLivenessClientForTest(ctx)
		// counts the rounds' reads; the first fails
		failNextProvideKeyChangeReadForTest(ctx)
		writer := startLivenessWriterForTest(t, 20*time.Millisecond)
		fixture := startLivenessAnnounceForTest(ctx, networkId, clientId, writer, false)
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
		for _, outcome := range fixture.takeWrites() {
			t.Fatalf("a non-providing client's connection wrote %s", outcome)
		}
		if _, ok := model.Testing_GetProviderLivenessLease(ctx, clientId); ok {
			t.Fatal("a non-providing client has a lease")
		}
	})
}

// The read that finds the client no longer providing has the writer release
// its lease.
func TestConnectionAnnounceReleasesLeaseWhenProvideStops(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		networkId, clientId := createLivenessClientForTest(ctx, model.ProvideModePublic)
		writer := startLivenessWriterForTest(t, time.Hour)
		fixture := startLivenessAnnounceForTest(ctx, networkId, clientId, writer, true)
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

// Two connections of one client hold its lease. One ends: nothing is written
// for it, and the lease stays where the last write left it. A release at the
// end would have dropped the client from every draw until the next write.
func TestConnectionAnnounceLeaseOutlivesOneOfTwoConnections(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testCtx, testCancel := context.WithTimeout(ctx, 60*time.Second)
		defer testCancel()
		networkId, clientId := createLivenessClientForTest(ctx, model.ProvideModePublic)
		writer := startLivenessWriterForTest(t, time.Hour)
		ending := startLivenessAnnounceForTest(ctx, networkId, clientId, writer, false)
		defer ending.close()
		staying := startLivenessAnnounceForTest(ctx, networkId, clientId, writer, false)
		defer staying.close()
		ending.awaitRegistered(t, testCtx)
		staying.awaitRegistered(t, testCtx)
		ending.awaitOutcome(t, testCtx, livenessLeaseOutcomeRefreshed)
		staying.awaitOutcome(t, testCtx, livenessLeaseOutcomeRefreshed)
		// the writer wakes for each connection's first providing read; let the
		// second write land before the snapshot
		for {
			before, ok := model.Testing_GetProviderLivenessLease(ctx, clientId)
			if !ok {
				t.Fatal("the providing client has no lease")
			}
			time.Sleep(100 * time.Millisecond)
			again, _ := model.Testing_GetProviderLivenessLease(ctx, clientId)
			if again.Equal(before) {
				break
			}
		}
		before, _ := model.Testing_GetProviderLivenessLease(ctx, clientId)

		ending.close()
		for _, outcome := range ending.takeWrites() {
			if outcome != livenessLeaseOutcomeRefreshed {
				t.Fatalf("the ending connection wrote %s", outcome)
			}
		}
		after, ok := model.Testing_GetProviderLivenessLease(ctx, clientId)
		if !ok {
			t.Fatal("one connection's end removed the lease the other connection holds")
		}
		if !after.Equal(before) {
			t.Fatalf("the lease moved from %s to %s with no write due", before, after)
		}
	})
}

// Wedges every connection write of a command on a lease shard key until the
// write's deadline, as a wedged node holds it, with the pool connection taken
// all the while, and counts how many it holds at once.
type livenessLeaseSaturationHook struct {
	stateLock sync.Mutex
	held      int
	maxHeld   int
}

func (self *livenessLeaseSaturationHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network string, addr string) (net.Conn, error) {
		conn, err := next(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &livenessLeaseWedgedConn{Conn: conn, hook: self}, nil
	}
}

func (self *livenessLeaseSaturationHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

func (self *livenessLeaseSaturationHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (self *livenessLeaseSaturationHook) heldNow() (held int, maxHeld int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.held, self.maxHeld
}

// A pool connection whose writes of lease commands wait out their deadline.
type livenessLeaseWedgedConn struct {
	net.Conn
	hook          *livenessLeaseSaturationHook
	writeDeadline time.Time
}

func (self *livenessLeaseWedgedConn) SetDeadline(deadline time.Time) error {
	self.writeDeadline = deadline
	return self.Conn.SetDeadline(deadline)
}

func (self *livenessLeaseWedgedConn) SetWriteDeadline(deadline time.Time) error {
	self.writeDeadline = deadline
	return self.Conn.SetWriteDeadline(deadline)
}

func (self *livenessLeaseWedgedConn) Write(b []byte) (int, error) {
	if !bytes.Contains(b, []byte("{provider_live:")) {
		return self.Conn.Write(b)
	}
	func() {
		self.hook.stateLock.Lock()
		defer self.hook.stateLock.Unlock()
		self.hook.held += 1
		self.hook.maxHeld = max(self.hook.maxHeld, self.hook.held)
	}()
	defer func() {
		self.hook.stateLock.Lock()
		defer self.hook.stateLock.Unlock()
		self.hook.held -= 1
	}()
	wait := 2 * time.Second
	if !self.writeDeadline.IsZero() {
		wait = time.Until(self.writeDeadline)
	}
	time.Sleep(wait)
	return 0, os.ErrDeadlineExceeded
}

// Every lease write wedged as on a stuck node, holding its pool connection,
// with 256 providing connections of the process across every shard, while
// admission and debit calls run on the same deadline-bound pool: the lease
// path holds one of the pool's connections at a time, and every admission
// call completes within its own deadline. Writes issued per connection, as
// before, took the pool's 8 connections per node and starved them.
func TestLivenessLeaseSaturatedWritesNeverBlockAdmission(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		hook := &livenessLeaseSaturationHook{}
		// added before the pool dials, so every connection it dials is wedged
		hookCtx, hookCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := server.RedisWithDeadline(hookCtx, func(r server.RedisClient) error {
			r.AddHook(hook)
			return nil
		}, server.OptRedisNoPreflight()); err != nil {
			t.Fatal(err)
		}
		hookCancel()

		writer := newProviderLivenessLeaseWriter(time.Hour)
		writer.startOnce.Do(func() {})
		for range 256 {
			connection := writer.begin(server.NewId(), nil)
			connection.afterRead(livenessLeasePublicForTest)
		}
		writeCtx, writeCancel := context.WithCancel(ctx)
		written := make(chan struct{})
		go func() {
			defer close(written)
			writer.write(writeCtx, true)
		}()
		defer func() {
			writeCancel()
			<-written
		}()
		// the lease writes are wedged
		for {
			if held, _ := hook.heldNow(); 0 < held {
				break
			}
			time.Sleep(time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond)

		errs := make(chan error, 16)
		admissionGroup := sync.WaitGroup{}
		for i := range 16 {
			admissionGroup.Add(1)
			go func() {
				defer admissionGroup.Done()
				// an admission budget well inside the lease write's bound
				admissionCtx, admissionCancel := context.WithTimeout(ctx, 300*time.Millisecond)
				defer admissionCancel()
				errs <- server.RedisWithDeadline(admissionCtx, func(r server.RedisClient) error {
					err := r.Get(admissionCtx, fmt.Sprintf("{test-admission:%d}", i)).Err()
					if err == redis.Nil {
						return nil
					}
					return err
				})
			}()
		}
		admissionGroup.Wait()
		close(errs)
		failed := 0
		var lastErr error
		for err := range errs {
			if err != nil {
				failed += 1
				lastErr = err
			}
		}
		_, maxHeld := hook.heldNow()
		if failed != 0 || maxHeld != 1 {
			t.Fatalf("with the lease writes wedged, %d of 16 admission calls failed (%v), and the lease path held %d pool connections at once; want none failed and one held", failed, lastErr, maxHeld)
		}
	})
}
