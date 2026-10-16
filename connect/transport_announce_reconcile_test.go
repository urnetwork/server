// Pure tests of a handler's connection reconcile over an in-memory store of
// the handler's connected rows. Each pass is driven directly, so the ordering
// of registrations, commits, final disconnects and reads is explicit. The
// database path, with a real failed final disconnect, is in
// transport_announce_reconcile_db_test.go.
package connect

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// One handler's rows as the reconcile reads and retires them.
type reconcileStoreForTest struct {
	stateLock              sync.Mutex
	connectedConnectionIds map[server.Id]bool
	retiredConnectionIds   []server.Id
	// rows whose retire raises, and rows whose retire finds a busy fence
	failingConnectionIds   map[server.Id]bool
	deferringConnectionIds map[server.Id]bool
	// every retire attempt, in order
	retireAttempts []server.Id
	// the disconnect time each retired row was given
	retireDisconnectTimes map[server.Id]time.Time
	// each read and retire is sent here when set, without blocking
	readEvents   chan struct{}
	retireEvents chan server.Id
	// runs once after the next read's snapshot, before the read returns; set
	// only while no pass runs
	afterRead func()
}

func newReconcileStoreForTest() *reconcileStoreForTest {
	return &reconcileStoreForTest{
		connectedConnectionIds: map[server.Id]bool{},
		failingConnectionIds:   map[server.Id]bool{},
		deferringConnectionIds: map[server.Id]bool{},
		retireDisconnectTimes:  map[server.Id]time.Time{},
	}
}

// Makes the row's retire raise, or stops it raising.
func (self *reconcileStoreForTest) failRetire(connectionId server.Id, fail bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.failingConnectionIds[connectionId] = fail
}

// Makes the row's retire find a busy fence, or stops it.
func (self *reconcileStoreForTest) deferRetire(connectionId server.Id, deferRetire bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.deferringConnectionIds[connectionId] = deferRetire
}

// The retire attempts since the last call.
func (self *reconcileStoreForTest) takeAttempts() []server.Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	attempts := self.retireAttempts
	self.retireAttempts = nil
	return attempts
}

// A registration's row commits.
func (self *reconcileStoreForTest) commit(connectionId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.connectedConnectionIds[connectionId] = true
}

// Another owner disconnects the row: the transport's own final disconnect,
// or handler expiry.
func (self *reconcileStoreForTest) disconnect(connectionId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.connectedConnectionIds, connectionId)
}

func (self *reconcileStoreForTest) connected(connectionId server.Id) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.connectedConnectionIds[connectionId]
}

func (self *reconcileStoreForTest) retired() []server.Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.retiredConnectionIds)
}

func (self *reconcileStoreForTest) read(ctx context.Context, handlerId server.Id) []server.Id {
	connectionIds := func() []server.Id {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		connectionIds := []server.Id{}
		for connectionId := range self.connectedConnectionIds {
			connectionIds = append(connectionIds, connectionId)
		}
		return connectionIds
	}()
	if afterRead := self.afterRead; afterRead != nil {
		self.afterRead = nil
		afterRead()
	}
	if self.readEvents != nil {
		select {
		case self.readEvents <- struct{}{}:
		default:
		}
	}
	return connectionIds
}

func (self *reconcileStoreForTest) retire(ctx context.Context, handlerId server.Id, connectionId server.Id, disconnectTime time.Time) (bool, bool) {
	fail, deferRetire := func() (bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.retireAttempts = append(self.retireAttempts, connectionId)
		return self.failingConnectionIds[connectionId], self.deferringConnectionIds[connectionId]
	}()
	if fail {
		panic(errors.New("test: the database refused this retire"))
	}
	if deferRetire {
		return false, true
	}
	retired := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.connectedConnectionIds[connectionId] {
			return false
		}
		delete(self.connectedConnectionIds, connectionId)
		self.retiredConnectionIds = append(self.retiredConnectionIds, connectionId)
		self.retireDisconnectTimes[connectionId] = disconnectTime
		return true
	}()
	if retired && self.retireEvents != nil {
		select {
		case self.retireEvents <- connectionId:
		default:
		}
	}
	return retired, false
}

// A reconcile over store whose passes the test drives.
func newReconcileForTest(store *reconcileStoreForTest, settings *ConnectionReconcileSettings) *handlerConnectionReconcile {
	return newHandlerConnectionReconcileWithStore(
		context.Background(),
		server.NewId(),
		settings,
		store.read,
		store.retire,
	)
}

// Registers a connection the way an announcement does: the row commits while
// the registration is in flight, then the registration holds it.
func registerForTest(reconcile *handlerConnectionReconcile, store *reconcileStoreForTest) server.Id {
	connectionId := server.NewId()
	endConnect := reconcile.beginConnect()
	store.commit(connectionId)
	endConnect(&connectionId)
	return connectionId
}

// Registers count connections whose final disconnects failed, oldest first.
func registerFailedReleasesForTest(reconcile *handlerConnectionReconcile, store *reconcileStoreForTest, count int) []server.Id {
	connectionIds := []server.Id{}
	for i := 0; i < count; i += 1 {
		connectionId := registerForTest(reconcile, store)
		reconcile.release(connectionId, false)
		connectionIds = append(connectionIds, connectionId)
	}
	slices.SortFunc(connectionIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	return connectionIds
}

// The root cause: the transport ended, its single final disconnect failed,
// and the handler stays alive, so handler expiry never reaches the row. Before
// the reconcile nothing else wrote that row, and it stayed connected for the
// life of the process. The reconcile must retire it, on its second sighting.
func TestConnectionReconcileRetiresRowOfFailedFinalDisconnect(t *testing.T) {
	store := newReconcileStoreForTest()
	reconcile := newReconcileForTest(store, DefaultConnectionReconcileSettings())

	connectionId := registerForTest(reconcile, store)
	// the final disconnect raised; the row is still connected
	reconcile.release(connectionId, false)
	if !store.connected(connectionId) {
		t.Fatal("fixture: the failed final disconnect must leave the row connected")
	}

	firstBacklog := reconcile.reconcile()
	firstConnected := store.connected(connectionId)
	secondBacklog := reconcile.reconcile()
	if store.connected(connectionId) {
		t.Fatal("the row of a failed final disconnect is still connected after two passes; it would stay connected while the handler lives")
	}
	if !firstConnected {
		t.Fatal("the reconcile retired a row on its first sighting")
	}
	if !firstBacklog || secondBacklog {
		t.Fatalf("backlog after the first sighting %t and after the retire %t, want true then false", firstBacklog, secondBacklog)
	}
	if retired := store.retired(); len(retired) != 1 || retired[0] != connectionId {
		t.Fatalf("retired %v, want only the failed connection", retired)
	}
	select {
	case <-reconcile.wake:
	default:
		t.Fatal("a failed final disconnect did not ask for an early pass")
	}
}

// Held rows, and rows the transport disconnected itself, are never retired.
func TestConnectionReconcileNeverRetiresHeldConnections(t *testing.T) {
	store := newReconcileStoreForTest()
	reconcile := newReconcileForTest(store, DefaultConnectionReconcileSettings())

	heldA := registerForTest(reconcile, store)
	heldB := registerForTest(reconcile, store)
	ended := registerForTest(reconcile, store)
	store.disconnect(ended)
	reconcile.release(ended, true)
	select {
	case <-reconcile.wake:
		t.Fatal("a written final disconnect asked for an early pass")
	default:
	}

	for pass := 0; pass < 3; pass += 1 {
		if backlog := reconcile.reconcile(); backlog {
			t.Fatalf("pass %d found a backlog among held rows", pass)
		}
	}
	if !store.connected(heldA) || !store.connected(heldB) {
		t.Fatal("the reconcile retired a held connection")
	}
	if retired := store.retired(); len(retired) != 0 {
		t.Fatalf("retired %v, want none", retired)
	}
}

// A registration commits its row before it holds it. While the registration
// is in flight its row is unheld, and must not be retired; a registration
// that raised after its commit leaves the row to the reconcile.
func TestConnectionReconcileWaitsForRegistrationInFlight(t *testing.T) {
	store := newReconcileStoreForTest()
	reconcile := newReconcileForTest(store, DefaultConnectionReconcileSettings())

	inFlight := server.NewId()
	endInFlight := reconcile.beginConnect()
	store.commit(inFlight)
	reconcile.reconcile()
	reconcile.reconcile()
	reconcile.reconcile()
	if !store.connected(inFlight) {
		t.Fatal("the reconcile retired the row of a registration still in flight")
	}
	endInFlight(&inFlight)
	if backlog := reconcile.reconcile(); backlog {
		t.Fatal("a row held after its registration returned is still a backlog")
	}
	if !store.connected(inFlight) {
		t.Fatal("the reconcile retired a row once its registration held it")
	}

	raised := server.NewId()
	endRaised := reconcile.beginConnect()
	store.commit(raised)
	reconcile.reconcile()
	// the registration raised after its commit, before it could hold the row
	endRaised(nil)
	reconcile.reconcile()
	if store.connected(raised) {
		t.Fatal("the row of a registration that raised after its commit is still connected")
	}
	if !store.connected(inFlight) {
		t.Fatal("retiring an orphan retired a held row")
	}
}

// Registrations begin all the time on a busy handler. One that began after a
// stale row's first sighting cannot own that row, and must not hold up its
// retire; its own row waits for it.
func TestConnectionReconcileChurnDoesNotBlockRetire(t *testing.T) {
	store := newReconcileStoreForTest()
	reconcile := newReconcileForTest(store, DefaultConnectionReconcileSettings())

	stale := registerForTest(reconcile, store)
	reconcile.release(stale, false)
	reconcile.reconcile()

	later := server.NewId()
	endLater := reconcile.beginConnect()
	store.commit(later)
	reconcile.reconcile()
	if store.connected(stale) {
		t.Fatal("a registration that began after the first sighting held up the stale row's retire")
	}
	reconcile.reconcile()
	if !store.connected(later) {
		t.Fatal("the reconcile retired the row of a registration still in flight")
	}
	endLater(&later)
	reconcile.reconcile()
	if !store.connected(later) {
		t.Fatal("the reconcile retired a held row")
	}
}

// One pass retires at most the batch count, oldest first, and reports the
// rest as a backlog.
func TestConnectionReconcileRetiresInBoundedBatches(t *testing.T) {
	store := newReconcileStoreForTest()
	settings := DefaultConnectionReconcileSettings()
	settings.RetireBatchCount = 2
	reconcile := newReconcileForTest(store, settings)

	staleConnectionIds := []server.Id{}
	for i := 0; i < 5; i += 1 {
		connectionId := registerForTest(reconcile, store)
		reconcile.release(connectionId, false)
		staleConnectionIds = append(staleConnectionIds, connectionId)
	}
	slices.SortFunc(staleConnectionIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})

	if backlog := reconcile.reconcile(); !backlog || len(store.retired()) != 0 {
		t.Fatalf("first sighting: backlog %t, retired %d", backlog, len(store.retired()))
	}
	for _, c := range []struct {
		retiredCount int
		backlog      bool
	}{
		{retiredCount: 2, backlog: true},
		{retiredCount: 4, backlog: true},
		{retiredCount: 5, backlog: false},
	} {
		backlog := reconcile.reconcile()
		if retired := store.retired(); len(retired) != c.retiredCount || backlog != c.backlog {
			t.Fatalf("retired %d with backlog %t, want %d with backlog %t", len(retired), backlog, c.retiredCount, c.backlog)
		}
	}
	if retired := store.retired(); !slices.Equal(retired, staleConnectionIds) {
		t.Fatalf("retired %v, want oldest first %v", retired, staleConnectionIds)
	}
}

// A row disconnected by another owner after its first sighting is forgotten,
// not retired again.
func TestConnectionReconcileForgetsRowsDisconnectedElsewhere(t *testing.T) {
	store := newReconcileStoreForTest()
	reconcile := newReconcileForTest(store, DefaultConnectionReconcileSettings())

	connectionId := registerForTest(reconcile, store)
	reconcile.release(connectionId, false)
	reconcile.reconcile()
	store.disconnect(connectionId)
	if backlog := reconcile.reconcile(); backlog {
		t.Fatal("a row disconnected elsewhere is still a backlog")
	}
	if retired := store.retired(); len(retired) != 0 {
		t.Fatalf("retired %v, want none", retired)
	}
}

// A failed final disconnect moves the next pass up from the regular interval
// to the backlog interval, and the backlog pass that follows retires the row.
func TestConnectionReconcileFailedReleaseWakesEarlyPass(t *testing.T) {
	store := newReconcileStoreForTest()
	store.readEvents = make(chan struct{}, 8)
	store.retireEvents = make(chan server.Id, 8)
	settings := DefaultConnectionReconcileSettings()
	settings.ReconcileInterval = time.Hour
	settings.ReconcileIntervalJitter = 0
	settings.BacklogInterval = connectionReconcileMinInterval
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reconcile := newHandlerConnectionReconcileWithStore(ctx, server.NewId(), settings, store.read, store.retire)
	done := make(chan struct{})
	go func() {
		defer close(done)
		reconcile.run()
	}()
	defer func() {
		cancel()
		<-done
	}()

	connectionId := registerForTest(reconcile, store)
	reconcile.release(connectionId, false)

	// the guard bounds a hung test; the regular interval is an hour
	guard := time.After(30 * time.Second)
	select {
	case <-store.readEvents:
	case <-guard:
		t.Fatal("a failed final disconnect did not move a pass up")
	}
	select {
	case retired := <-store.retireEvents:
		if retired != connectionId {
			t.Fatalf("retired %s, want %s", retired, connectionId)
		}
	case <-guard:
		t.Fatal("the backlog pass did not retire the row")
	}
}

// Settings left unset, as by a handler settings literal, would spin the loop
// or expire every read at once; they select the defaults.
func TestConnectionReconcileDefaultsUnsetSettings(t *testing.T) {
	store := newReconcileStoreForTest()
	for _, settings := range []*ConnectionReconcileSettings{nil, {}} {
		reconcile := newReconcileForTest(store, settings)
		if *reconcile.settings != *DefaultConnectionReconcileSettings() {
			t.Errorf("unset settings %+v selected %+v, want the defaults", settings, *reconcile.settings)
		}
	}
}

// A zero batch observes unheld rows at the regular interval without retiring
// any.
func TestConnectionReconcileZeroBatchOnlyObserves(t *testing.T) {
	store := newReconcileStoreForTest()
	settings := DefaultConnectionReconcileSettings()
	settings.RetireBatchCount = 0
	reconcile := newReconcileForTest(store, settings)

	connectionId := registerForTest(reconcile, store)
	reconcile.release(connectionId, false)
	reconcile.reconcile()
	if backlog := reconcile.reconcile(); backlog {
		t.Fatal("an observing reconcile asked for backlog passes it cannot act on")
	}
	if !store.connected(connectionId) || len(store.retired()) != 0 {
		t.Fatal("a zero batch retired a row")
	}
	if unheld := len(reconcile.unheldConnectionGenerations); unheld != 1 {
		t.Fatalf("an observing reconcile tracks %d unheld rows, want 1", unheld)
	}
}

// An announcement without a handler owner holds nothing.
func TestConnectionReconcileNilHoldsNothing(t *testing.T) {
	var reconcile *handlerConnectionReconcile
	connectionId := server.NewId()
	reconcile.beginConnect()(&connectionId)
	reconcile.release(connectionId, false)
}

// A row whose retire keeps failing must not block the others. Rows retire
// oldest first, and a raise used to end the pass before the row left the
// map, so the failing row came first on every pass and no other row retired.
func TestConnectionReconcileRetiresAroundARowWhoseRetireKeepsFailing(t *testing.T) {
	store := newReconcileStoreForTest()
	reconcile := newReconcileForTest(store, DefaultConnectionReconcileSettings())
	connectionIds := registerFailedReleasesForTest(reconcile, store, 3)
	failing, others := connectionIds[0], connectionIds[1:]
	store.failRetire(failing, true)

	passCount := 6
	raisedCount := 0
	for pass := 0; pass < passCount; pass += 1 {
		if raisedForTest(func() { reconcile.reconcile() }) != nil {
			raisedCount += 1
		}
	}
	for _, connectionId := range others {
		if store.connected(connectionId) {
			t.Fatalf(
				"a row whose retire keeps failing blocked the other stale rows: %d of %d passes raised, retired %v",
				raisedCount,
				passCount,
				store.retired(),
			)
		}
	}
	if raisedCount != 0 {
		t.Fatalf("%d of %d passes raised; a failed retire must not end its pass", raisedCount, passCount)
	}
	if !store.connected(failing) {
		t.Fatal("the row whose retire fails was retired")
	}
}

// A pass gives up after three consecutive failed retires, without raising,
// and leaves the rest for the next pass.
func TestConnectionReconcileEndsAPassAfterConsecutiveRetireFailures(t *testing.T) {
	store := newReconcileStoreForTest()
	reconcile := newReconcileForTest(store, DefaultConnectionReconcileSettings())
	connectionIds := registerFailedReleasesForTest(reconcile, store, 5)
	for _, connectionId := range connectionIds {
		store.failRetire(connectionId, true)
	}
	reconcile.reconcile()
	store.takeAttempts()

	var backlog bool
	raised := raisedForTest(func() { backlog = reconcile.reconcile() })
	attempts := store.takeAttempts()
	// the pass's bound on consecutive failed retires
	consecutiveFailureBound := 3
	if raised != nil || len(attempts) != consecutiveFailureBound {
		t.Fatalf("a pass of failing retires raised %v after %d attempts, want no raise after %d", raised, len(attempts), consecutiveFailureBound)
	}
	if !backlog {
		t.Fatal("rows left by failed retires are not a backlog")
	}
}

// A row whose retire failed is tried after the rows that have not failed, so
// rows that keep failing cannot use up a pass's consecutive failures.
func TestConnectionReconcileTriesFailedRowsLast(t *testing.T) {
	store := newReconcileStoreForTest()
	reconcile := newReconcileForTest(store, DefaultConnectionReconcileSettings())
	connectionIds := registerFailedReleasesForTest(reconcile, store, 3)
	for _, connectionId := range connectionIds {
		store.failRetire(connectionId, true)
	}
	// every pass may meet a failing retire
	pass := func() {
		raisedForTest(func() { reconcile.reconcile() })
	}
	pass()
	pass()
	store.takeAttempts()

	// newer rows that have never failed
	newer := registerFailedReleasesForTest(reconcile, store, 2)
	pass()
	store.takeAttempts()
	pass()
	attempts := store.takeAttempts()
	if len(attempts) < 2 || !slices.Equal(attempts[:2], newer) {
		t.Fatalf("retire attempts %v, want the rows that never failed %v first", attempts, newer)
	}
	for _, connectionId := range newer {
		if store.connected(connectionId) {
			t.Fatalf("rows that keep failing kept newer row %s from retiring", connectionId)
		}
	}
}

// A retire that finds a busy fence defers the row, which stays for the next
// pass instead of being forgotten until two new sightings.
func TestConnectionReconcileKeepsADeferredRowForTheNextPass(t *testing.T) {
	store := newReconcileStoreForTest()
	reconcile := newReconcileForTest(store, DefaultConnectionReconcileSettings())
	connectionId := registerFailedReleasesForTest(reconcile, store, 1)[0]
	store.deferRetire(connectionId, true)
	reconcile.reconcile()
	if backlog := reconcile.reconcile(); !backlog {
		t.Fatal("a deferred row is not a backlog")
	}
	if !store.connected(connectionId) {
		t.Fatal("fixture: the deferred row must stay connected")
	}

	store.deferRetire(connectionId, false)
	reconcile.reconcile()
	if store.connected(connectionId) {
		t.Fatal("a deferred row was forgotten: the pass after the fence freed did not retire it")
	}
}

// A full busy prefix cannot spend every pass's retirement budget. The first
// pass attempts that prefix; the next must reach the free row beyond it, at
// both a one-row budget and the production batch boundary.
func TestConnectionReconcileRetiresPastAContinuouslyBusyBatch(t *testing.T) {
	for _, batchCount := range []int{1, DefaultConnectionReconcileSettings().RetireBatchCount} {
		store := newReconcileStoreForTest()
		settings := DefaultConnectionReconcileSettings()
		settings.RetireBatchCount = batchCount
		reconcile := newReconcileForTest(store, settings)
		connectionIds := registerFailedReleasesForTest(reconcile, store, batchCount+1)
		for _, connectionId := range connectionIds[:batchCount] {
			store.deferRetire(connectionId, true)
		}
		free := connectionIds[batchCount]
		reconcile.reconcile()
		if attempts := store.takeAttempts(); len(attempts) != 0 {
			t.Fatalf("batch %d: first sighting attempted %v", batchCount, attempts)
		}
		reconcile.reconcile()
		if attempts := store.takeAttempts(); !slices.Equal(attempts, connectionIds[:batchCount]) {
			t.Fatalf("batch %d: first retirement pass attempted %v, want the busy prefix", batchCount, attempts)
		}
		reconcile.reconcile()
		attempts := store.takeAttempts()
		if len(attempts) > batchCount || !slices.Contains(attempts, free) || store.connected(free) {
			t.Fatalf("batch %d: busy prefix starved free row %s; attempts %v", batchCount, free, attempts)
		}
		for _, connectionId := range connectionIds[:batchCount] {
			if !store.connected(connectionId) {
				t.Fatalf("batch %d: a busy row was retired", batchCount)
			}
			store.deferRetire(connectionId, false)
		}
		if backlog := reconcile.reconcile(); backlog {
			t.Fatalf("batch %d: cleared fences left a backlog", batchCount)
		}
		if attempts := store.takeAttempts(); len(attempts) != batchCount || len(store.retired()) != len(connectionIds) {
			t.Fatalf("batch %d: recovery attempted %d and retired %d rows", batchCount, len(attempts), len(store.retired()))
		}
	}
}

// The row of a failed final disconnect is retired with the time its
// transport ended, which the release knows, not the later time of the pass.
func TestConnectionReconcileRetiresWithTheReleaseTime(t *testing.T) {
	store := newReconcileStoreForTest()
	reconcile := newReconcileForTest(store, DefaultConnectionReconcileSettings())
	connectionId := registerForTest(reconcile, store)
	beforeRelease := server.NowUtc()
	reconcile.release(connectionId, false)
	afterRelease := server.NowUtc()
	// the passes run strictly after the release
	for !server.NowUtc().After(afterRelease) {
	}

	reconcile.reconcile()
	reconcile.reconcile()
	if store.connected(connectionId) {
		t.Fatal("fixture: the row must be retired")
	}
	disconnectTime := func() time.Time {
		store.stateLock.Lock()
		defer store.stateLock.Unlock()
		return store.retireDisconnectTimes[connectionId]
	}()
	if disconnectTime.Before(beforeRelease) || disconnectTime.After(afterRelease) {
		t.Fatalf("the row was retired with disconnect time %s, want its release between %s and %s", disconnectTime, beforeRelease, afterRelease)
	}
}

// Ordinary churn leaves rows unheld for a moment: a registration whose row
// committed before the read, a disconnect that commits after it. Those wait
// for the regular pass. Before, any unheld row kept the backlog cadence, so on
// a busy handler the reconcile read every backlog interval, not every
// interval. Rows known to be stale still keep it.
func TestConnectionReconcileKeepsItsIntervalThroughChurn(t *testing.T) {
	store := newReconcileStoreForTest()
	reconcile := newReconcileForTest(store, DefaultConnectionReconcileSettings())
	failed := registerForTest(reconcile, store)
	for pass := 0; pass < 3; pass += 1 {
		// a registration in flight, its row committed, at every pass
		connectionId := server.NewId()
		endConnect := reconcile.beginConnect()
		store.commit(connectionId)
		if backlog := reconcile.reconcile(); backlog {
			t.Fatalf("pass %d: a registration in flight kept the backlog cadence", pass)
		}
		endConnect(&connectionId)
	}

	reconcile.release(failed, false)
	if backlog := reconcile.reconcile(); !backlog {
		t.Fatal("the row of a failed final disconnect did not keep the backlog cadence")
	}
}

// A registration that commits after a pass's read, and whose transport ends
// with a failed final disconnect before that pass takes its lock, is missing
// from that read because the read predates the row, not because the row was
// disconnected. Its release must survive the pass: the row keeps the backlog
// cadence and is retired with its release time.
func TestConnectionReconcileKeepsAReleaseItsReadPredates(t *testing.T) {
	store := newReconcileStoreForTest()
	reconcile := newReconcileForTest(store, DefaultConnectionReconcileSettings())
	var connectionId server.Id
	var beforeRelease, afterRelease time.Time
	store.afterRead = func() {
		connectionId = registerForTest(reconcile, store)
		beforeRelease = server.NowUtc()
		reconcile.release(connectionId, false)
		afterRelease = server.NowUtc()
	}
	reconcile.reconcile()
	if store.afterRead != nil {
		t.Fatal("fixture: the registration must land after the first read")
	}
	// the passes run strictly after the release
	for !server.NowUtc().After(afterRelease) {
	}

	if backlog := reconcile.reconcile(); !backlog {
		t.Fatal("the row of a failed final disconnect lost the backlog cadence: the pass whose read predates the row dropped its release")
	}
	reconcile.reconcile()
	if store.connected(connectionId) {
		t.Fatal("the row of a failed final disconnect is still connected after its second sighting")
	}
	disconnectTime := func() time.Time {
		store.stateLock.Lock()
		defer store.stateLock.Unlock()
		return store.retireDisconnectTimes[connectionId]
	}()
	if disconnectTime.Before(beforeRelease) || disconnectTime.After(afterRelease) {
		t.Fatalf("the row was retired with disconnect time %s, want its release between %s and %s", disconnectTime, beforeRelease, afterRelease)
	}
}
