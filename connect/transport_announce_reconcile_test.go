// Pure tests of a handler's connection reconcile over an in-memory store of
// the handler's connected rows. Each pass is driven directly, so the ordering
// of registrations, commits, final disconnects and reads is explicit. The
// database path, with a real failed final disconnect, is in
// transport_announce_reconcile_db_test.go.
package connect

import (
	"context"
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
	// each read and retire is sent here when set, without blocking
	readEvents   chan struct{}
	retireEvents chan server.Id
}

func newReconcileStoreForTest() *reconcileStoreForTest {
	return &reconcileStoreForTest{
		connectedConnectionIds: map[server.Id]bool{},
	}
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
	if self.readEvents != nil {
		select {
		case self.readEvents <- struct{}{}:
		default:
		}
	}
	return connectionIds
}

func (self *reconcileStoreForTest) retire(ctx context.Context, handlerId server.Id, connectionId server.Id) bool {
	retired := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.connectedConnectionIds[connectionId] {
			return false
		}
		delete(self.connectedConnectionIds, connectionId)
		self.retiredConnectionIds = append(self.retiredConnectionIds, connectionId)
		return true
	}()
	if retired && self.retireEvents != nil {
		select {
		case self.retireEvents <- connectionId:
		default:
		}
	}
	return retired
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
