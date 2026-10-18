package connect

// The process's provider liveness lease writer (transport_announce_liveness_lease.go),
// with its shard writes recorded instead of sent and driven one write at a
// time: what a pending, providing and non-providing connection has written,
// when a lease is released, that the end of a connection writes nothing, that
// the writes are one at a time, and the vouching that the handler stamp
// stands for.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// One recorded shard write.
type livenessLeaseShardWriteForTest struct {
	shard    int
	scored   []server.Id
	extended []server.Id
	removed  []server.Id
}

// A writer whose shard writes are recorded, answering each extension with held,
// on a clock at *now, never started: the test drives it with write.
type livenessLeaseWriterForTest struct {
	writer *providerLivenessLeaseWriter
	writes []livenessLeaseShardWriteForTest
	held   map[server.Id]bool
	err    error
	// runs during each shard write
	during func()
}

func newLivenessLeaseWriterForTest(now *time.Time) *livenessLeaseWriterForTest {
	recorder := &livenessLeaseWriterForTest{
		held: map[server.Id]bool{},
	}
	writer := newProviderLivenessLeaseWriter(model.ProviderLivenessLeaseRefreshInterval)
	// never started by begin
	writer.startOnce.Do(func() {})
	writer.now = func() time.Time {
		return *now
	}
	writer.observe = func(string, int) {}
	writer.writeShard = func(ctx context.Context, shard int, scored []server.Id, extended []server.Id, removed []server.Id) ([]bool, error) {
		recorder.writes = append(recorder.writes, livenessLeaseShardWriteForTest{
			shard:    shard,
			scored:   slices.Clone(scored),
			extended: slices.Clone(extended),
			removed:  slices.Clone(removed),
		})
		if recorder.during != nil {
			recorder.during()
		}
		if recorder.err != nil {
			return nil, recorder.err
		}
		held := make([]bool, len(extended))
		for i, clientId := range extended {
			held[i] = recorder.held[clientId]
		}
		return held, nil
	}
	recorder.writer = writer
	return recorder
}

// What the recorded writes did, in order: "score", "extend" and "remove", one
// per client.
func (self *livenessLeaseWriterForTest) take() []string {
	operations := []string{}
	for _, write := range self.writes {
		for range write.scored {
			operations = append(operations, "score")
		}
		for range write.extended {
			operations = append(operations, "extend")
		}
		for range write.removed {
			operations = append(operations, "remove")
		}
	}
	self.writes = nil
	return operations
}

// The connection's vouch time, zero while the process does not vouch for it.
func livenessLeaseWriteTimeForTest(connection *providerLivenessLeaseConnection) time.Time {
	connection.writer.stateLock.Lock()
	defer connection.writer.stateLock.Unlock()
	return connection.writeTime
}

var livenessLeasePublicForTest = map[model.ProvideMode]bool{model.ProvideModePublic: true}

// An unheld pending connection stops vouching until its first providing read.
// Failed score writes must then withhold coverage, while a successful write
// restores it; a still-pending consumer cannot withhold the stamp.
func TestLivenessLeaseUnheldPendingProviderRestoresTheStampSafeguard(t *testing.T) {
	for _, mode := range []model.ProvideMode{model.ProvideModePublic, model.ProvideModeNetwork} {
		ctx := context.Background()
		start := time.UnixMilli(1_800_000_000_000)
		now := start
		recorder := newLivenessLeaseWriterForTest(&now)
		connection := recorder.writer.begin(server.NewId(), nil)
		recorder.writer.write(ctx, true)
		if writeTime := livenessLeaseWriteTimeForTest(connection); !writeTime.IsZero() {
			t.Fatalf("%d: the unheld pending connection is still vouched for", mode)
		}
		stampCount := 0
		stamp := func(context.Context, server.Id) error {
			stampCount += 1
			return nil
		}
		now = start.Add(2 * model.ProviderLivenessWriterWithholdAge)
		if outcome := stampProviderLivenessWriter(ctx, server.Id{}, recorder.writer, now, stamp); outcome != livenessLeaseOutcomeStamped || stampCount != 1 {
			t.Fatalf("%d: an unheld pending consumer withheld coverage: %s, %d stamps", mode, outcome, stampCount)
		}
		connection.afterRead(map[model.ProvideMode]bool{mode: true})
		providingAt := now
		recorder.err = errors.New("test: score writes refused")
		for range 3 {
			recorder.writer.write(ctx, true)
			now = now.Add(model.ProviderLivenessWriterWithholdAge / 2)
			connection.afterRead(map[model.ProvideMode]bool{mode: true})
		}
		if outcome := stampProviderLivenessWriter(ctx, server.Id{}, recorder.writer, now, stamp); outcome != livenessLeaseOutcomeStampWithheld || stampCount != 1 {
			t.Fatalf("%d: failed first scores kept coverage healthy: %s, %d stamps", mode, outcome, stampCount)
		}
		if writeTime := livenessLeaseWriteTimeForTest(connection); !writeTime.Equal(providingAt) {
			t.Fatalf("%d: providing read vouched from %s, want %s", mode, writeTime, providingAt)
		}
		recorder.err = nil
		recorder.writer.write(ctx, true)
		if outcome := stampProviderLivenessWriter(ctx, server.Id{}, recorder.writer, now, stamp); outcome != livenessLeaseOutcomeStamped || stampCount != 2 {
			t.Fatalf("%d: a successful score did not restore coverage: %s, %d stamps", mode, outcome, stampCount)
		}
	}
}

// A pending connection with a retained lease keeps its original vouch age:
// the first providing read must not grant extra time to a stale writer.
func TestLivenessLeaseFirstProvidingReadKeepsAnExistingVouch(t *testing.T) {
	ctx := context.Background()
	start := time.UnixMilli(1_800_000_000_000)
	now := start
	recorder := newLivenessLeaseWriterForTest(&now)
	connection := recorder.writer.begin(server.NewId(), nil)
	recorder.held[connection.clientId] = true
	recorder.writer.write(ctx, true)
	now = start.Add(model.ProviderLivenessWriterWithholdAge + time.Millisecond)
	connection.afterRead(livenessLeasePublicForTest)
	outcome := stampProviderLivenessWriter(ctx, server.Id{}, recorder.writer, now, func(context.Context, server.Id) error {
		t.Fatal("a stale held lease still stamped coverage")
		return nil
	})
	if outcome != livenessLeaseOutcomeStampWithheld {
		t.Fatalf("first providing read reset a retained vouch: %s", outcome)
	}
}

// A connection registers when its rounds start, before any read: the process
// vouches for it from then, and the writer extends its client's lease, which
// keeps a reconnecting provider's lease whatever its first reads do. A held
// lease moves the vouch time to the write's start; an unheld one ends the
// vouching, and nothing is created for a client that may not provide.
func TestLivenessLeaseWriterExtendsAPendingConnectionsLease(t *testing.T) {
	ctx := context.Background()
	start := time.UnixMilli(1_800_000_000_000)
	now := start
	recorder := newLivenessLeaseWriterForTest(&now)
	reconnecting := recorder.writer.begin(server.NewId(), nil)
	unknown := recorder.writer.begin(server.NewId(), nil)
	recorder.held[reconnecting.clientId] = true
	if oldestTime, ok := recorder.writer.oldest(); !ok || !oldestTime.Equal(start) {
		t.Fatal("the process does not vouch for connections pending their first read")
	}

	now = start.Add(30 * time.Second)
	recorder.writer.write(ctx, true)
	if operations := recorder.take(); !slices.Equal(operations, []string{"extend", "extend"}) {
		t.Fatalf("pending connections wrote %v, want an extension each", operations)
	}
	if writeTime := livenessLeaseWriteTimeForTest(reconnecting); !writeTime.Equal(now) {
		t.Fatalf("a held extension vouched from %s, want the write's start %s", writeTime, now)
	}
	if writeTime := livenessLeaseWriteTimeForTest(unknown); !writeTime.IsZero() {
		t.Fatal("the process still vouches for a pending connection whose client the shard holds no lease for")
	}
	for range 3 {
		now = now.Add(30 * time.Second)
		recorder.writer.write(ctx, true)
	}
	if operations := recorder.take(); slices.Contains(operations, "score") || slices.Contains(operations, "remove") {
		t.Fatalf("pending connections whose reads keep failing wrote %v, want extensions only", operations)
	}
	if writeTime := livenessLeaseWriteTimeForTest(reconnecting); !writeTime.Equal(now) {
		t.Fatal("the reconnecting provider's lease was not extended every write")
	}
}

// The first read that finds the client providing has it scored at the next
// write, which a woken writer runs at once for it alone, and every write after.
// A write's success is the time it started, not the time it returned.
func TestLivenessLeaseWriterScoresAProvidingConnectionFromTheWriteStart(t *testing.T) {
	ctx := context.Background()
	start := time.UnixMilli(1_800_000_000_000)
	now := start
	recorder := newLivenessLeaseWriterForTest(&now)
	recorder.writer.begin(server.NewId(), nil)
	recorder.writer.write(ctx, false)
	recorder.take()

	connection := recorder.writer.begin(server.NewId(), nil)
	connection.afterRead(livenessLeasePublicForTest)
	writeStart := start.Add(10 * time.Second)
	now = writeStart
	recorder.during = func() {
		// the write returns well after it started
		now = writeStart.Add(model.ProviderLivenessLeaseWriteTimeout)
	}
	recorder.writer.write(ctx, false)
	recorder.during = nil
	if operations := recorder.take(); !slices.Equal(operations, []string{"score"}) {
		t.Fatalf("the woken write wrote %v, want the new providing client alone", operations)
	}
	if writeTime := livenessLeaseWriteTimeForTest(connection); !writeTime.Equal(writeStart) {
		t.Fatalf("the providing connection is vouched from %s, want the write's start %s", writeTime, writeStart)
	}
	recorder.writer.write(ctx, true)
	operations := recorder.take()
	slices.Sort(operations)
	if !slices.Equal(operations, []string{"extend", "score"}) {
		t.Fatalf("the next full write wrote %v, want the score and the other's extension", operations)
	}
}

// A client that provides in no discovery mode is never scored or released,
// and the process stops vouching for it at its first read.
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
		recorder := newLivenessLeaseWriterForTest(&now)
		connection := recorder.writer.begin(server.NewId(), nil)
		// one write while pending, which extends nothing the shard holds
		recorder.writer.write(ctx, true)
		for range 3 {
			connection.afterRead(provideModes)
			recorder.writer.write(ctx, true)
		}
		connection.end()
		recorder.writer.write(ctx, true)
		if operations := recorder.take(); !slices.Equal(operations, []string{"extend"}) {
			t.Fatalf("modes %v wrote %v, want the one pending extension", provideModes, operations)
		}
		if _, ok := recorder.writer.oldest(); ok {
			t.Fatalf("modes %v: the process still vouches for the connection", provideModes)
		}
	}
}

// The pool FindProviders2 draws from holds Network-only providers for their
// own network's callers, so their connections lease them too.
func TestLivenessLeaseLeasesANetworkOnlyProvider(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	recorder := newLivenessLeaseWriterForTest(&now)
	connection := recorder.writer.begin(server.NewId(), nil)
	connection.afterRead(map[model.ProvideMode]bool{model.ProvideModeNetwork: true})
	recorder.writer.write(ctx, true)
	if operations := recorder.take(); !slices.Equal(operations, []string{"score"}) {
		t.Fatalf("a Network-only provider wrote %v, want a score", operations)
	}
}

// The read that finds the client no longer providing has the next write
// release the lease once; later writes write nothing for it. A client that
// provides again is scored again, vouched for from that read.
func TestLivenessLeaseReleasesWhenTheClientStopsProviding(t *testing.T) {
	ctx := context.Background()
	start := time.UnixMilli(1_800_000_000_000)
	now := start
	recorder := newLivenessLeaseWriterForTest(&now)
	connection := recorder.writer.begin(server.NewId(), nil)
	connection.afterRead(livenessLeasePublicForTest)
	recorder.writer.write(ctx, true)
	connection.afterRead(map[model.ProvideMode]bool{})
	if _, ok := recorder.writer.oldest(); ok {
		t.Fatal("the read that found the client stopping left it vouched for")
	}
	for range 3 {
		recorder.writer.write(ctx, true)
	}
	if operations := recorder.take(); !slices.Equal(operations, []string{"score", "remove"}) {
		t.Fatalf("the client stopping wrote %v, want one release after the score", operations)
	}
	now = start.Add(time.Minute)
	connection.afterRead(livenessLeasePublicForTest)
	if writeTime := livenessLeaseWriteTimeForTest(connection); !writeTime.Equal(now) {
		t.Fatal("the client providing again is not vouched for from that read")
	}
	recorder.writer.write(ctx, false)
	if operations := recorder.take(); !slices.Equal(operations, []string{"score"}) {
		t.Fatalf("the client providing again wrote %v, want a score", operations)
	}
}

// A release that fails is not retried from the writer's own state: only a
// later read that still finds the client not providing requests it again.
func TestLivenessLeaseRetriesAFailedReleaseOnlyOnALaterRead(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	recorder := newLivenessLeaseWriterForTest(&now)
	connection := recorder.writer.begin(server.NewId(), nil)
	connection.afterRead(livenessLeasePublicForTest)
	recorder.writer.write(ctx, true)
	recorder.take()
	connection.afterRead(map[model.ProvideMode]bool{})
	recorder.err = errors.New("test: redis refused the write")
	recorder.writer.write(ctx, true)
	recorder.err = nil
	recorder.writer.write(ctx, true)
	if operations := recorder.take(); !slices.Equal(operations, []string{"remove"}) {
		t.Fatalf("a failed release wrote %v, want it once until a later read", operations)
	}
	connection.afterRead(map[model.ProvideMode]bool{})
	recorder.writer.write(ctx, true)
	recorder.writer.write(ctx, true)
	if operations := recorder.take(); !slices.Equal(operations, []string{"remove"}) {
		t.Fatalf("the later read wrote %v, want the release again, once", operations)
	}
}

// The end writes nothing, a pending release included. The lease is the
// client's: when this connection's last read found the client not providing
// and its release failed, the client may provide again, and another of its
// connections may hold a fresh lease that a release at the end would delete.
// A providing connection's end leaves the lease to lapse within the window for
// the same reason. Either way the process stops vouching for the connection.
func TestLivenessLeaseEndWritesNothing(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	recorder := newLivenessLeaseWriterForTest(&now)
	stale := recorder.writer.begin(server.NewId(), nil)
	stale.afterRead(livenessLeasePublicForTest)
	recorder.writer.write(ctx, true)
	stale.afterRead(map[model.ProvideMode]bool{})
	recorder.err = errors.New("test: redis refused the write")
	recorder.writer.write(ctx, true)
	recorder.err = nil
	stale.end()
	stale.afterRead(map[model.ProvideMode]bool{})
	recorder.writer.write(ctx, true)
	if operations := recorder.take(); !slices.Equal(operations, []string{"score", "remove"}) {
		t.Fatalf("the end after a failed release wrote %v, want nothing more", operations)
	}

	providing := recorder.writer.begin(server.NewId(), nil)
	providing.afterRead(livenessLeasePublicForTest)
	recorder.writer.write(ctx, true)
	providing.end()
	recorder.writer.write(ctx, true)
	if operations := recorder.take(); !slices.Equal(operations, []string{"score"}) {
		t.Fatalf("the end of a providing connection wrote %v, want nothing after its score", operations)
	}
	if _, ok := recorder.writer.oldest(); ok {
		t.Fatal("an ended connection is still vouched for")
	}
}

// A read of one connection that finds its client stopped is not released
// while another connection of the process still has the client providing: its
// read may be older, but deleting a fresh lease is worse.
func TestLivenessLeaseKeepsALeaseAnotherConnectionProvides(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	recorder := newLivenessLeaseWriterForTest(&now)
	clientId := server.NewId()
	first := recorder.writer.begin(clientId, nil)
	second := recorder.writer.begin(clientId, nil)
	first.afterRead(livenessLeasePublicForTest)
	second.afterRead(livenessLeasePublicForTest)
	recorder.writer.write(ctx, true)
	first.afterRead(map[model.ProvideMode]bool{})
	recorder.writer.write(ctx, true)
	if operations := recorder.take(); !slices.Equal(operations, []string{"score", "score"}) {
		t.Fatalf("one connection of two finding its client stopped wrote %v, want the client scored, not removed", operations)
	}
}

// The writes are one shard script at a time, one per shard, whatever the
// number of connections: the lease path holds at most one connection of the
// deadline-bound pool it shares with admission and debit.
func TestLivenessLeaseWriterWritesOneShardAtATime(t *testing.T) {
	ctx := context.Background()
	now := time.UnixMilli(1_800_000_000_000)
	recorder := newLivenessLeaseWriterForTest(&now)
	inFlightLock := sync.Mutex{}
	inFlight := 0
	maxInFlight := 0
	recorder.during = func() {
		inFlightLock.Lock()
		inFlight += 1
		maxInFlight = max(maxInFlight, inFlight)
		inFlightLock.Unlock()
		time.Sleep(time.Millisecond)
		inFlightLock.Lock()
		inFlight -= 1
		inFlightLock.Unlock()
	}
	for range 256 {
		connection := recorder.writer.begin(server.NewId(), nil)
		connection.afterRead(livenessLeasePublicForTest)
	}
	recorder.writer.write(ctx, true)
	shards := map[int]bool{}
	for _, write := range recorder.writes {
		if shards[write.shard] {
			t.Fatalf("shard %d was written twice in one write", write.shard)
		}
		shards[write.shard] = true
	}
	if maxInFlight != 1 || 32 < len(recorder.writes) {
		t.Fatalf("256 providing connections wrote %d shard scripts, %d at once, want at most 32, one at a time", len(recorder.writes), maxInFlight)
	}
}

// The handler stamp vouches for the process's writes. A connection whose
// writes fail, or that is pending a first read no write can prove, stops the
// stamps once it is vouched for more than the withhold age ago, and a
// successful write resumes them; a process with no connection stamps.
func TestProviderLivenessWriterWithholdsTheStampWhileAWriteIsStale(t *testing.T) {
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
	recorder := newLivenessLeaseWriterForTest(&now)
	writer := recorder.writer
	withholdAge := model.ProviderLivenessWriterWithholdAge

	if outcome := stampProviderLivenessWriter(ctx, handlerId, writer, start, stamp); outcome != livenessLeaseOutcomeStamped {
		t.Fatalf("a process with no connection: %s, want stamped", outcome)
	}
	connection := writer.begin(server.NewId(), nil)
	connection.afterRead(livenessLeasePublicForTest)
	recorder.err = errors.New("test: redis refused the write")
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
		writer.write(ctx, true)
		if outcome := stampProviderLivenessWriter(ctx, handlerId, writer, now, stamp); outcome != c.outcome {
			t.Fatalf("%s after the registration: %s, want %s", c.since, outcome, c.outcome)
		}
	}
	recorder.err = nil
	writer.write(ctx, true)
	if outcome := stampProviderLivenessWriter(ctx, handlerId, writer, now, stamp); outcome != livenessLeaseOutcomeStamped {
		t.Fatalf("after a successful write: %s, want stamped", outcome)
	}

	// pending, registered, before any write could say what it holds
	pending := writer.begin(server.NewId(), nil)
	if outcome := stampProviderLivenessWriter(ctx, handlerId, writer, now.Add(withholdAge+time.Millisecond), stamp); outcome != livenessLeaseOutcomeStampWithheld {
		t.Fatalf("a connection pending past the withhold age with no write: %s, want stamp_withheld", outcome)
	}
	pending.end()
	if outcome := stampProviderLivenessWriter(ctx, handlerId, writer, now, stamp); outcome != livenessLeaseOutcomeStamped {
		t.Fatalf("after the pending connection ended: %s, want stamped", outcome)
	}
	if stamps != 5 {
		t.Fatalf("stamped %d times, want the 5 that were not withheld", stamps)
	}
}

// Every announcement registers, consumers included. A consumer connection
// whose provide reads keep failing stays pending for as long as it is
// connected; once a write finds no lease for its client the process no longer
// vouches for it, and goes on stamping. Before, it was vouched for from its
// registration for good, and one stuck consumer withheld the stamp, which
// turned enforcement off fleet-wide.
func TestProviderLivenessWriterKeepsStampingThroughAStuckConsumer(t *testing.T) {
	ctx := context.Background()
	start := time.UnixMilli(1_800_000_000_000)
	now := start
	recorder := newLivenessLeaseWriterForTest(&now)
	writer := recorder.writer
	stamp := func(context.Context, server.Id) error {
		return nil
	}
	providing := writer.begin(server.NewId(), nil)
	providing.afterRead(livenessLeasePublicForTest)
	consumer := writer.begin(server.NewId(), nil)
	for since := time.Duration(0); since <= 10*model.ProviderLivenessWriterWithholdAge; since += model.ProviderLivenessLeaseRefreshInterval {
		now = start.Add(since)
		writer.write(ctx, true)
		if outcome := stampProviderLivenessWriter(ctx, server.NewId(), writer, now, stamp); outcome != livenessLeaseOutcomeStamped {
			t.Fatalf("%s into a consumer's failing reads: %s, want stamped", since, outcome)
		}
	}
	if writeTime := livenessLeaseWriteTimeForTest(consumer); !writeTime.IsZero() {
		t.Fatal("the process vouches for a stuck consumer connection")
	}
}

// A pass that raises is logged and ends that pass only: the writer's next
// pass writes as planned.
func TestLivenessLeaseWriterPassThatRaisesDoesNotStopLaterPasses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := newProviderLivenessLeaseWriter(time.Hour)
	writer.observe = func(string, int) {}
	calls := make(chan int, 16)
	callCount := 0
	callLock := sync.Mutex{}
	writer.writeShard = func(ctx context.Context, shard int, scored []server.Id, extended []server.Id, removed []server.Id) ([]bool, error) {
		callLock.Lock()
		callCount += 1
		count := callCount
		callLock.Unlock()
		calls <- count
		if count == 1 {
			panic("test: a pass that raises")
		}
		return make([]bool, len(extended)), nil
	}
	writer.start(ctx)
	connection := writer.begin(server.NewId(), nil)
	for want := 1; want <= 2; want += 1 {
		select {
		case count := <-calls:
			if count != want {
				t.Fatalf("shard write %d, want %d", count, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for shard write %d: a raised pass stopped the writer", want)
		}
		// the next pass, woken by the client's first providing read
		connection.afterRead(livenessLeasePublicForTest)
	}
}

// The default cadence fits the contract: a connection that misses one write
// does not reach the withhold age.
func TestLivenessLeaseDefaultCadenceFitsTheWithholdAge(t *testing.T) {
	writer := newProviderLivenessLeaseWriter(model.ProviderLivenessLeaseRefreshInterval)
	oneMissedWrite := 2*(writer.refreshInterval+model.ProviderLivenessLeaseRefreshJitter) + model.ProviderLivenessLeasePassTimeout
	if model.ProviderLivenessWriterWithholdAge <= oneMissedWrite {
		t.Fatalf("one missed write reaches the %s withhold age", model.ProviderLivenessWriterWithholdAge)
	}
	for range 100 {
		if jitter := writer.jitter(); jitter < 0 || model.ProviderLivenessLeaseRefreshJitter < jitter {
			t.Fatalf("jitter %s outside [0, %s]", jitter, model.ProviderLivenessLeaseRefreshJitter)
		}
	}
}

// The handler is stamped at once, then every interval, a failed stamp
// observed and stamped again, until its context ends.
func TestProviderLivenessWriterStampsUntilItsContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	handlerId := server.NewId()
	now := time.Now()
	recorder := newLivenessLeaseWriterForTest(&now)
	stampLock := sync.Mutex{}
	stampCount := 0
	stamped := make(chan int, 16)
	outcomes := map[string]int{}
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		runProviderLivenessWriter(ctx, handlerId, time.Millisecond, recorder.writer, func(stampCtx context.Context, stampHandlerId server.Id) error {
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
		}, func(outcome string, count int) {
			stampLock.Lock()
			defer stampLock.Unlock()
			outcomes[outcome] += count
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
	stampLock.Lock()
	defer stampLock.Unlock()
	if outcomes[livenessLeaseOutcomeStampFailed] != 1 || outcomes[livenessLeaseOutcomeStamped] < 2 {
		t.Fatalf("observed %v, want one failed stamp and the others stamped", outcomes)
	}
}
