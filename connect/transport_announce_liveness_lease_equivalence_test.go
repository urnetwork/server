package connect

// The liveness lease writer (transport_announce_liveness_lease.go) against the
// algorithm it replaced, which walked every connection of the process for each
// write and again for each shard write's result. Both are driven with the same
// seeded operations: registrations, reads that find the client providing, only
// on its network, or not providing, ends, woken and full writes, failed shard
// writes, extensions the shard holds or not, passes cut short by their
// deadline, and registrations, reads and ends during a shard write. After each
// operation both must have written the same shards with the same clients, and
// every connection must hold the same state, vouch time, hold, pending release
// and outcomes. A synctest run checks that the writer's loop writes the same
// records at the same instants through churn, a drain and many shards.

import (
	"context"
	"encoding/binary"
	"errors"
	"maps"
	mathrand "math/rand/v2"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// One connection of the full walk.
type livenessLeaseFullWalkConnectionForTest struct {
	writer         *livenessLeaseFullWalkWriterForTest
	clientId       server.Id
	state          int
	writeTime      time.Time
	held           bool
	releasePending bool
	dirty          bool
	ended          bool
	outcomes       []string
}

// The writer's algorithm as it was before it kept its connections indexed:
// every write walks every connection for the providing clients and the dirty
// connections, and every shard write's result walks every connection again.
// Single goroutine.
type livenessLeaseFullWalkWriterForTest struct {
	now              func() time.Time
	writeShard       func(ctx context.Context, shard int, scoredClientIds []server.Id, extendedClientIds []server.Id, removedClientIds []server.Id) ([]bool, error)
	observe          func(outcome string, count int)
	connections      map[*livenessLeaseFullWalkConnectionForTest]bool
	releaseClientIds map[server.Id]bool
}

func newLivenessLeaseFullWalkWriterForTest(now func() time.Time) *livenessLeaseFullWalkWriterForTest {
	return &livenessLeaseFullWalkWriterForTest{
		now:              now,
		observe:          func(string, int) {},
		connections:      map[*livenessLeaseFullWalkConnectionForTest]bool{},
		releaseClientIds: map[server.Id]bool{},
	}
}

// Returns the connection, and that the writer wakes.
func (self *livenessLeaseFullWalkWriterForTest) begin(clientId server.Id) (*livenessLeaseFullWalkConnectionForTest, bool) {
	connection := &livenessLeaseFullWalkConnectionForTest{
		writer:    self,
		clientId:  clientId,
		state:     livenessLeasePending,
		writeTime: self.now(),
		dirty:     true,
	}
	self.connections[connection] = true
	return connection, true
}

// Returns whether the writer wakes.
func (self *livenessLeaseFullWalkConnectionForTest) afterRead(provideModes map[model.ProvideMode]bool) bool {
	providing := providesForDiscovery(provideModes)
	writer := self.writer
	if self.ended {
		return false
	}
	if providing {
		self.releasePending = false
		if self.state != livenessLeaseProviding {
			if self.writeTime.IsZero() {
				self.writeTime = writer.now()
			}
			self.state = livenessLeaseProviding
			self.dirty = true
			return true
		}
		return false
	}
	wake := false
	if self.state == livenessLeaseProviding || self.releasePending || (self.state == livenessLeasePending && self.held) {
		writer.releaseClientIds[self.clientId] = true
		self.releasePending = false
		wake = true
	}
	self.state = livenessLeaseNotProviding
	self.writeTime = time.Time{}
	return wake
}

func (self *livenessLeaseFullWalkConnectionForTest) end() {
	self.ended = true
	delete(self.writer.connections, self)
}

func (self *livenessLeaseFullWalkWriterForTest) write(ctx context.Context, all bool) {
	shardWrites := map[int]*providerLivenessLeaseShardWrite{}
	shardWrite := func(clientId server.Id) *providerLivenessLeaseShardWrite {
		shard := model.ProviderLivenessLeaseShard(clientId)
		if shardWrites[shard] == nil {
			shardWrites[shard] = &providerLivenessLeaseShardWrite{}
		}
		return shardWrites[shard]
	}
	providingClientIds := map[server.Id]bool{}
	for connection := range self.connections {
		if connection.state == livenessLeaseProviding {
			providingClientIds[connection.clientId] = true
		}
	}
	scoredClientIds := map[server.Id]bool{}
	extendedClientIds := map[server.Id]bool{}
	for connection := range self.connections {
		if !all && !connection.dirty {
			continue
		}
		connection.dirty = false
		clientId := connection.clientId
		switch {
		case providingClientIds[clientId] && (connection.state == livenessLeaseProviding || connection.state == livenessLeasePending):
			if !scoredClientIds[clientId] {
				scoredClientIds[clientId] = true
				shardWrite(clientId).scoredClientIds = append(shardWrite(clientId).scoredClientIds, clientId)
			}
		case connection.state == livenessLeasePending:
			if !extendedClientIds[clientId] {
				extendedClientIds[clientId] = true
				shardWrite(clientId).extendedClientIds = append(shardWrite(clientId).extendedClientIds, clientId)
			}
		}
	}
	for clientId := range self.releaseClientIds {
		if !providingClientIds[clientId] {
			shardWrite(clientId).removedClientIds = append(shardWrite(clientId).removedClientIds, clientId)
		}
	}
	self.releaseClientIds = map[server.Id]bool{}

	shards := []int{}
	for shard := range shardWrites {
		shards = append(shards, shard)
	}
	slices.Sort(shards)
	for _, shard := range shards {
		if ctx.Err() != nil {
			return
		}
		write := shardWrites[shard]
		start := self.now()
		held, err := self.writeShard(ctx, shard, write.scoredClientIds, write.extendedClientIds, write.removedClientIds)
		if err != nil && ctx.Err() != nil {
			return
		}
		self.written(start, write, held, err)
	}
}

func (self *livenessLeaseFullWalkWriterForTest) written(start time.Time, write *providerLivenessLeaseShardWrite, held []bool, err error) {
	scoredClientIds := map[server.Id]bool{}
	for _, clientId := range write.scoredClientIds {
		scoredClientIds[clientId] = true
	}
	heldClientIds := map[server.Id]bool{}
	unheldClientIds := map[server.Id]bool{}
	for i, clientId := range write.extendedClientIds {
		if err == nil && i < len(held) {
			if held[i] {
				heldClientIds[clientId] = true
			} else {
				unheldClientIds[clientId] = true
			}
		}
	}
	removedClientIds := map[server.Id]bool{}
	for _, clientId := range write.removedClientIds {
		removedClientIds[clientId] = true
	}
	for connection := range self.connections {
		clientId := connection.clientId
		outcome := ""
		switch {
		case scoredClientIds[clientId] && connection.state != livenessLeaseNotProviding:
			if err == nil {
				connection.writeTime = start
				outcome = livenessLeaseOutcomeRefreshed
			} else {
				outcome = livenessLeaseOutcomeRefreshFailed
			}
		case heldClientIds[clientId] && connection.state == livenessLeasePending:
			connection.writeTime = start
			connection.held = true
			outcome = livenessLeaseOutcomeExtended
		case unheldClientIds[clientId] && connection.state == livenessLeasePending:
			connection.writeTime = time.Time{}
			connection.held = false
		case removedClientIds[clientId] && connection.state == livenessLeaseNotProviding:
			if err == nil {
				outcome = livenessLeaseOutcomeReleased
			} else {
				connection.releasePending = true
				outcome = livenessLeaseOutcomeReleaseFailed
			}
		}
		if outcome != "" {
			connection.outcomes = append(connection.outcomes, outcome)
		}
	}
	if err == nil {
		self.observe(livenessLeaseOutcomeRefreshed, len(write.scoredClientIds))
		self.observe(livenessLeaseOutcomeExtended, len(heldClientIds))
		self.observe(livenessLeaseOutcomeReleased, len(write.removedClientIds))
	} else {
		self.observe(livenessLeaseOutcomeRefreshFailed, len(write.scoredClientIds)+len(write.extendedClientIds))
		self.observe(livenessLeaseOutcomeReleaseFailed, len(write.removedClientIds))
	}
}

// One shard write as the equivalence tests compare it: the shard and its
// clients as sorted sets, and when it started.
type livenessLeaseEquivalenceWriteForTest struct {
	at       time.Duration
	shard    int
	scored   []string
	extended []string
	removed  []string
}

func livenessLeaseEquivalenceWriteOf(at time.Duration, shard int, scored []server.Id, extended []server.Id, removed []server.Id) livenessLeaseEquivalenceWriteForTest {
	sorted := func(clientIds []server.Id) []string {
		s := []string{}
		for _, clientId := range clientIds {
			s = append(s, clientId.String())
		}
		slices.Sort(s)
		return s
	}
	return livenessLeaseEquivalenceWriteForTest{
		at:       at,
		shard:    shard,
		scored:   sorted(scored),
		extended: sorted(extended),
		removed:  sorted(removed),
	}
}

func (self livenessLeaseEquivalenceWriteForTest) equal(other livenessLeaseEquivalenceWriteForTest) bool {
	return self.at == other.at && self.shard == other.shard &&
		slices.Equal(self.scored, other.scored) &&
		slices.Equal(self.extended, other.extended) &&
		slices.Equal(self.removed, other.removed)
}

// One connection in both writers, with the writer's outcomes.
type livenessLeaseEquivalencePairForTest struct {
	indexed  *providerLivenessLeaseConnection
	fullWalk *livenessLeaseFullWalkConnectionForTest
	outcomes *[]string
}

// What an equivalence run exercises.
type livenessLeaseEquivalenceSettingsForTest struct {
	seed        int64
	steps       int
	clientCount int
	// a shard write fails, of every 100
	failPercent int
	// an operation runs during a shard write, of every 100
	duringPercent int
	// a write's pass ends at a shard write, of every 100
	abortPercent int
	// the run ends every connection at the halfway step
	drain bool
}

// The seeded clients: their ids come from the seed, so their shards do too.
func livenessLeaseClientIdsForTest(rng *mathrand.Rand, count int) []server.Id {
	clientIds := []server.Id{}
	for range count {
		idBytes := make([]byte, 16)
		binary.LittleEndian.PutUint64(idBytes[0:8], rng.Uint64())
		binary.LittleEndian.PutUint64(idBytes[8:16], rng.Uint64())
		clientIds = append(clientIds, server.RequireIdFromBytes(idBytes))
	}
	return clientIds
}

// Checks that the writer's index matches its connections: each client's
// connections and count of providing connections, and that each dirty
// connection is listed once.
func checkLivenessLeaseIndexForTest(t *testing.T, writer *providerLivenessLeaseWriter) {
	t.Helper()
	writer.stateLock.Lock()
	defer writer.stateLock.Unlock()
	clientIdConnectionCounts := map[server.Id]int{}
	clientIdProvidingCounts := map[server.Id]int{}
	for connection := range writer.connections {
		if connection.ended {
			t.Fatal("an ended connection is still the writer's")
		}
		if !writer.clientIdConnections[connection.clientId][connection] {
			t.Fatal("a connection is missing from its client's connections")
		}
		clientIdConnectionCounts[connection.clientId] += 1
		if connection.state == livenessLeaseProviding {
			clientIdProvidingCounts[connection.clientId] += 1
		}
	}
	for clientId, clientConnections := range writer.clientIdConnections {
		if len(clientConnections) != clientIdConnectionCounts[clientId] {
			t.Fatalf("a client has %d indexed connections, want %d", len(clientConnections), clientIdConnectionCounts[clientId])
		}
	}
	if !maps.Equal(writer.clientIdProvidingCounts, clientIdProvidingCounts) {
		t.Fatalf("providing counts %v, want %v", writer.clientIdProvidingCounts, clientIdProvidingCounts)
	}
	listed := map[*providerLivenessLeaseConnection]bool{}
	for _, connection := range writer.dirtyConnections {
		if listed[connection] {
			t.Fatal("a dirty connection is listed twice")
		}
		listed[connection] = true
	}
	for connection := range writer.connections {
		if connection.dirty && !listed[connection] {
			t.Fatal("a dirty connection is not listed")
		}
	}
}

// Drives the writer and the full walk with the same seeded operations, and
// fails at the first difference.
func checkLivenessLeaseEquivalenceForTest(t *testing.T, settings livenessLeaseEquivalenceSettingsForTest) {
	t.Helper()
	rng := mathrand.New(mathrand.NewPCG(uint64(settings.seed), 0))
	clientIds := livenessLeaseClientIdsForTest(rng, settings.clientCount)
	start := time.UnixMilli(1_800_000_000_000)
	now := start
	clock := func() time.Time {
		return now
	}

	indexed := newProviderLivenessLeaseWriter(model.ProviderLivenessLeaseRefreshInterval)
	// never started by begin: the test calls write
	indexed.startOnce.Do(func() {})
	indexed.now = clock
	indexedObserved := map[string]int{}
	indexed.observe = func(outcome string, count int) {
		indexedObserved[outcome] += count
	}
	fullWalk := newLivenessLeaseFullWalkWriterForTest(clock)
	fullWalkObserved := map[string]int{}
	fullWalk.observe = func(outcome string, count int) {
		fullWalkObserved[outcome] += count
	}

	pairs := []*livenessLeaseEquivalencePairForTest{}
	live := func() []*livenessLeaseEquivalencePairForTest {
		l := []*livenessLeaseEquivalencePairForTest{}
		for _, pair := range pairs {
			if !pair.fullWalk.ended {
				l = append(l, pair)
			}
		}
		return l
	}
	readModes := []map[model.ProvideMode]bool{
		{model.ProvideModePublic: true},
		{model.ProvideModeNetwork: true},
		{},
	}

	// one operation on both writers, chosen before either runs it. A
	// registration made in a shard write is paired once both have made it.
	type operationForTest struct {
		kind     string
		clientId server.Id
		pair     *livenessLeaseEquivalencePairForTest
		mode     map[model.ProvideMode]bool
	}
	chooseOperation := func() operationForTest {
		l := live()
		r := rng.IntN(100)
		switch {
		case r < 30 || len(l) == 0:
			return operationForTest{kind: "begin", clientId: clientIds[rng.IntN(len(clientIds))]}
		case r < 75:
			return operationForTest{kind: "read", pair: l[rng.IntN(len(l))], mode: readModes[rng.IntN(len(readModes))]}
		default:
			return operationForTest{kind: "end", pair: l[rng.IntN(len(l))]}
		}
	}
	// operations on an ended connection do nothing in either writer
	// a registration takes its outcomes from the start, since a shard write it
	// is made in can give it one
	type pendingForTest struct {
		connection *providerLivenessLeaseConnection
		outcomes   *[]string
	}
	pendingIndexeds := []pendingForTest{}
	pendingFullWalks := []*livenessLeaseFullWalkConnectionForTest{}
	runIndexed := func(operation operationForTest) {
		switch operation.kind {
		case "begin":
			outcomes := &[]string{}
			connection := indexed.begin(operation.clientId, func(outcome string) {
				*outcomes = append(*outcomes, outcome)
			})
			pendingIndexeds = append(pendingIndexeds, pendingForTest{connection: connection, outcomes: outcomes})
		case "read":
			operation.pair.indexed.afterRead(operation.mode)
		case "end":
			operation.pair.indexed.end()
		}
	}
	runFullWalk := func(operation operationForTest) {
		switch operation.kind {
		case "begin":
			connection, _ := fullWalk.begin(operation.clientId)
			pendingFullWalks = append(pendingFullWalks, connection)
		case "read":
			operation.pair.fullWalk.afterRead(operation.mode)
		case "end":
			operation.pair.fullWalk.end()
		}
	}
	// pairs the registrations both writers made, in order
	pairPending := func() {
		if len(pendingIndexeds) != len(pendingFullWalks) {
			t.Fatalf("seed %d: the writer registered %d connections, the full walk %d", settings.seed, len(pendingIndexeds), len(pendingFullWalks))
		}
		for i := range pendingIndexeds {
			pairs = append(pairs, &livenessLeaseEquivalencePairForTest{
				indexed:  pendingIndexeds[i].connection,
				fullWalk: pendingFullWalks[i],
				outcomes: pendingIndexeds[i].outcomes,
			})
		}
		pendingIndexeds = pendingIndexeds[:0]
		pendingFullWalks = pendingFullWalks[:0]
	}

	for step := range settings.steps {
		now = now.Add(time.Duration(rng.IntN(3000)) * time.Millisecond)
		if settings.drain && step == settings.steps/2 {
			for _, pair := range live() {
				pair.indexed.end()
				pair.fullWalk.end()
			}
		}
		if r := rng.IntN(100); r < 70 {
			operation := chooseOperation()
			runIndexed(operation)
			runFullWalk(operation)
			pairPending()
		} else {
			all := r < 76
			// what each shard write answers and does, by its place in the write
			held := map[server.Id]bool{}
			for _, clientId := range clientIds {
				held[clientId] = rng.IntN(2) == 0
			}
			failed := map[int]bool{}
			during := map[int]operationForTest{}
			aborted := -1
			for ordinal := range 32 {
				if rng.IntN(100) < settings.failPercent {
					failed[ordinal] = true
				}
				if rng.IntN(100) < settings.duringPercent {
					during[ordinal] = chooseOperation()
				}
			}
			if rng.IntN(100) < settings.abortPercent {
				aborted = rng.IntN(4)
			}
			var indexedWrites, fullWalkWrites []livenessLeaseEquivalenceWriteForTest
			shardWriteFor := func(writes *[]livenessLeaseEquivalenceWriteForTest, cancel context.CancelFunc, runOperation func(operationForTest)) func(context.Context, int, []server.Id, []server.Id, []server.Id) ([]bool, error) {
				ordinal := 0
				return func(ctx context.Context, shard int, scored []server.Id, extended []server.Id, removed []server.Id) ([]bool, error) {
					o := ordinal
					ordinal += 1
					*writes = append(*writes, livenessLeaseEquivalenceWriteOf(now.Sub(start), shard, scored, extended, removed))
					if operation, ok := during[o]; ok {
						runOperation(operation)
					}
					if o == aborted {
						cancel()
						return nil, context.DeadlineExceeded
					}
					if failed[o] {
						return nil, errors.New("test: redis refused the write")
					}
					answers := make([]bool, len(extended))
					for i, clientId := range extended {
						answers[i] = held[clientId]
					}
					return answers, nil
				}
			}
			indexedCtx, indexedCancel := context.WithCancel(context.Background())
			indexed.writeShard = shardWriteFor(&indexedWrites, indexedCancel, runIndexed)
			// the full walk's own operations during its shard writes find the
			// pairs as the writer's left them, so both run on the same pairs
			fullWalkCtx, fullWalkCancel := context.WithCancel(context.Background())
			fullWalk.writeShard = shardWriteFor(&fullWalkWrites, fullWalkCancel, runFullWalk)
			indexed.write(indexedCtx, all)
			fullWalk.write(fullWalkCtx, all)
			indexedCancel()
			fullWalkCancel()
			pairPending()
			if len(indexedWrites) != len(fullWalkWrites) {
				t.Fatalf("seed %d step %d: the writer wrote %d shards, the full walk %d", settings.seed, step, len(indexedWrites), len(fullWalkWrites))
			}
			for i := range indexedWrites {
				if !indexedWrites[i].equal(fullWalkWrites[i]) {
					t.Fatalf("seed %d step %d: shard write %d is %+v, the full walk's %+v", settings.seed, step, i, indexedWrites[i], fullWalkWrites[i])
				}
			}
		}
		checkLivenessLeaseIndexForTest(t, indexed)
		for i, pair := range pairs {
			a, b := pair.indexed, pair.fullWalk
			if a.state != b.state || !a.writeTime.Equal(b.writeTime) || a.held != b.held || a.releasePending != b.releasePending || a.dirty != b.dirty || a.ended != b.ended {
				t.Fatalf("seed %d step %d: connection %d is state %d write %s held %t release %t dirty %t ended %t, the full walk's state %d write %s held %t release %t dirty %t ended %t",
					settings.seed, step, i, a.state, a.writeTime, a.held, a.releasePending, a.dirty, a.ended, b.state, b.writeTime, b.held, b.releasePending, b.dirty, b.ended)
			}
			if !slices.Equal(*pair.outcomes, b.outcomes) {
				t.Fatalf("seed %d step %d: connection %d took %v, the full walk's %v", settings.seed, step, i, *pair.outcomes, b.outcomes)
			}
		}
		releases := func(releaseClientIds map[server.Id]bool) string {
			s := []string{}
			for clientId := range releaseClientIds {
				s = append(s, clientId.String())
			}
			slices.Sort(s)
			return strings.Join(s, ",")
		}
		if releases(indexed.releaseClientIds) != releases(fullWalk.releaseClientIds) {
			t.Fatalf("seed %d step %d: requested releases differ", settings.seed, step)
		}
		for _, outcome := range []string{livenessLeaseOutcomeRefreshed, livenessLeaseOutcomeRefreshFailed, livenessLeaseOutcomeExtended, livenessLeaseOutcomeReleased, livenessLeaseOutcomeReleaseFailed} {
			if indexedObserved[outcome] != fullWalkObserved[outcome] {
				t.Fatalf("seed %d step %d: observed %d %s, the full walk %d", settings.seed, step, indexedObserved[outcome], outcome, fullWalkObserved[outcome])
			}
		}
	}
}

// Registrations, reads and ends between writes, on a few clients with several
// connections each.
func TestLivenessLeaseWriterMatchesTheFullWalk(t *testing.T) {
	for seed := int64(1); seed <= 300; seed += 1 {
		checkLivenessLeaseEquivalenceForTest(t, livenessLeaseEquivalenceSettingsForTest{
			seed:        seed,
			steps:       300,
			clientCount: 12,
			failPercent: 10,
		})
	}
}

// Churn during shard writes: registrations, reads and ends between a shard
// write's snapshot and its result.
func TestLivenessLeaseWriterMatchesTheFullWalkThroughChurnInAWrite(t *testing.T) {
	for seed := int64(1); seed <= 300; seed += 1 {
		checkLivenessLeaseEquivalenceForTest(t, livenessLeaseEquivalenceSettingsForTest{
			seed:          seed,
			steps:         300,
			clientCount:   16,
			failPercent:   10,
			duringPercent: 30,
		})
	}
}

// Passes cut short by their deadline, and every connection ending halfway: the
// writes that remain are the requested releases.
func TestLivenessLeaseWriterMatchesTheFullWalkThroughADrainAndCutPasses(t *testing.T) {
	for seed := int64(1); seed <= 300; seed += 1 {
		checkLivenessLeaseEquivalenceForTest(t, livenessLeaseEquivalenceSettingsForTest{
			seed:          seed,
			steps:         300,
			clientCount:   16,
			failPercent:   10,
			duringPercent: 10,
			abortPercent:  20,
			drain:         true,
		})
	}
}

// Many clients across every shard.
func TestLivenessLeaseWriterMatchesTheFullWalkAcrossTheShards(t *testing.T) {
	for seed := int64(1); seed <= 40; seed += 1 {
		checkLivenessLeaseEquivalenceForTest(t, livenessLeaseEquivalenceSettingsForTest{
			seed:          seed,
			steps:         2000,
			clientCount:   400,
			failPercent:   5,
			duringPercent: 10,
			abortPercent:  5,
		})
	}
}

// The writer's own loop under virtual time writes the same records at the
// same instants as the full walk with the loop's cadence: a woken write at each
// registration and each read that wakes, and a full pass every refresh
// interval, through churn, a drain and clients across the shards.
func TestLivenessLeaseWriterLoopWritesLikeTheFullWalk(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		interval := 25 * time.Second
		rng := mathrand.New(mathrand.NewPCG(7, 0))
		clientIds := livenessLeaseClientIdsForTest(rng, 120)
		start := time.Now()

		writer := newProviderLivenessLeaseWriter(interval)
		// started below, not by begin
		writer.startOnce.Do(func() {})
		writer.jitter = func() time.Duration {
			return 0
		}
		writer.observe = func(string, int) {}
		loopWrites := []livenessLeaseEquivalenceWriteForTest{}
		writer.writeShard = func(ctx context.Context, shard int, scored []server.Id, extended []server.Id, removed []server.Id) ([]bool, error) {
			loopWrites = append(loopWrites, livenessLeaseEquivalenceWriteOf(time.Since(start), shard, scored, extended, removed))
			answers := make([]bool, len(extended))
			for i, clientId := range extended {
				answers[i] = clientId[0]%2 == 0
			}
			return answers, nil
		}

		fullWalkNow := start
		fullWalk := newLivenessLeaseFullWalkWriterForTest(func() time.Time {
			return fullWalkNow
		})
		fullWalkWrites := []livenessLeaseEquivalenceWriteForTest{}
		fullWalk.writeShard = func(ctx context.Context, shard int, scored []server.Id, extended []server.Id, removed []server.Id) ([]bool, error) {
			fullWalkWrites = append(fullWalkWrites, livenessLeaseEquivalenceWriteOf(fullWalkNow.Sub(start), shard, scored, extended, removed))
			answers := make([]bool, len(extended))
			for i, clientId := range extended {
				answers[i] = clientId[0]%2 == 0
			}
			return answers, nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		runDone := make(chan struct{})
		go func() {
			defer close(runDone)
			writer.run(ctx)
		}()

		type pairForTest struct {
			indexed  *providerLivenessLeaseConnection
			fullWalk *livenessLeaseFullWalkConnectionForTest
		}
		pairs := []*pairForTest{}
		nextFullPass := interval
		// moves both clocks to at, the full walk running each full pass due by
		// then at its instant, as the loop's timer does
		advance := func(at time.Duration) {
			for nextFullPass <= at {
				time.Sleep(nextFullPass - time.Since(start))
				synctest.Wait()
				fullWalkNow = start.Add(nextFullPass)
				fullWalk.write(context.Background(), true)
				nextFullPass += interval
			}
			time.Sleep(at - time.Since(start))
			fullWalkNow = start.Add(at)
		}
		modes := []map[model.ProvideMode]bool{
			{model.ProvideModePublic: true},
			{model.ProvideModeNetwork: true},
			{},
		}
		at := time.Duration(0)
		for step := range 900 {
			// off the full passes' instants, so a wake and a full pass never tie
			at += time.Duration(100+rng.IntN(1500)) * time.Millisecond
			if at%interval == 0 {
				at += time.Millisecond
			}
			advance(at)
			wake := false
			if step == 600 {
				// the drain: every connection ends
				for _, pair := range pairs {
					pair.indexed.end()
					pair.fullWalk.end()
				}
			} else if r := rng.IntN(100); r < 35 || len(pairs) == 0 {
				clientId := clientIds[rng.IntN(len(clientIds))]
				fullWalkConnection, fullWalkWakes := fullWalk.begin(clientId)
				pairs = append(pairs, &pairForTest{
					indexed:  writer.begin(clientId, nil),
					fullWalk: fullWalkConnection,
				})
				wake = fullWalkWakes
			} else if r < 85 {
				pair := pairs[rng.IntN(len(pairs))]
				mode := modes[rng.IntN(len(modes))]
				pair.indexed.afterRead(mode)
				wake = pair.fullWalk.afterRead(mode)
			} else {
				pair := pairs[rng.IntN(len(pairs))]
				pair.indexed.end()
				pair.fullWalk.end()
			}
			synctest.Wait()
			if wake {
				fullWalk.write(context.Background(), false)
			}
			for i, pair := range pairs {
				a, b := pair.indexed, pair.fullWalk
				a.writer.stateLock.Lock()
				same := a.state == b.state && a.writeTime.Equal(b.writeTime) && a.held == b.held && a.releasePending == b.releasePending && a.dirty == b.dirty && a.ended == b.ended
				a.writer.stateLock.Unlock()
				if !same {
					t.Fatalf("step %d: connection %d differs from the full walk's", step, i)
				}
			}
		}
		advance(at + 3*interval)
		synctest.Wait()
		cancel()
		<-runDone

		if len(loopWrites) != len(fullWalkWrites) {
			t.Fatalf("the loop wrote %d shards, the full walk %d", len(loopWrites), len(fullWalkWrites))
		}
		for i := range loopWrites {
			if !loopWrites[i].equal(fullWalkWrites[i]) {
				t.Fatalf("shard write %d is %+v, the full walk's %+v", i, loopWrites[i], fullWalkWrites[i])
			}
		}
		if len(loopWrites) < 100 {
			t.Fatalf("only %d shard writes: the run did not exercise the loop", len(loopWrites))
		}
	})
}
