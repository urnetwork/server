package edgeplacement

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A model store whose reads the test sets, and a clock the test moves.
type testPlacementStore struct {
	stateLock      sync.Mutex
	loadRecords    []*model.ConnectMoveLoad
	nodeTime       time.Time
	controlFields  map[string]string
	edgeInventory  *server.ServicesLbInventory
	loadErr        error
	controlErr     error
	inventoryErr   error
	controlReads   int
	inventoryReads int
}

func (self *testPlacementStore) loads(ctx context.Context) ([]*model.ConnectMoveLoad, time.Time, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.loadErr != nil {
		return nil, time.Time{}, self.loadErr
	}
	return slices.Clone(self.loadRecords), self.nodeTime, nil
}

func (self *testPlacementStore) control(ctx context.Context, ceiling model.ConnectMoveControlCeiling) (*model.ConnectMoveControl, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.controlReads += 1
	if self.controlErr != nil {
		return nil, self.controlErr
	}
	return model.NarrowConnectMoveControl(self.controlFields, ceiling), nil
}

func (self *testPlacementStore) inventory() (*server.ServicesLbInventory, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.inventoryReads += 1
	if self.inventoryErr != nil {
		return nil, self.inventoryErr
	}
	return self.edgeInventory, nil
}

type testPlacementClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func (self *testPlacementClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *testPlacementClock) advance(duration time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(duration)
}

// Only test-edge-1 has headroom below the band, so every placement goes there.
func newTestPlacementStore(nodeTime time.Time) *testPlacementStore {
	return &testPlacementStore{
		loadRecords: []*model.ConnectMoveLoad{
			testPlacementLoad("test-edge-1", 10000, 100, 0.5, nodeTime),
			testPlacementLoad("test-edge-3", 160000, 100, 0.5, nodeTime),
			testPlacementLoad("test-edge-4", 180000, 100, 0.5, nodeTime),
		},
		nodeTime:      nodeTime,
		controlFields: map[string]string{},
		edgeInventory: testPlacementInventory(),
	}
}

// A placement over the store with one pass of its loop done.
func newTestEdgePlacement(t *testing.T, store *testPlacementStore, clock *testPlacementClock) *EdgePlacement {
	t.Helper()
	placement := newEdgePlacementWithStore(context.Background(), DefaultEdgePlacementSettings(), store, &testPlacementRandom{
		floats: slices.Repeat([]float64{0.5}, 100),
		ints:   slices.Repeat([]int{0}, 200),
	}, clock.Now)
	t.Cleanup(placement.Close)
	placement.tick()
	return placement
}

var testEdge1Addresses = []netip.Addr{
	netip.MustParseAddr("192.0.2.1"),
	netip.MustParseAddr("2001:db8::1"),
}

// The placement answers for the platform host of the inventory's domain,
// whatever its case or trailing dot, and for no other host.
func TestEdgePlacementAnswersOnlyForThePlatformHost(t *testing.T) {
	clock := &testPlacementClock{now: time.Now()}
	placement := newTestEdgePlacement(t, newTestPlacementStore(clock.Now()), clock)
	for _, host := range []string{"connect.space.example", "CONNECT.Space.Example", "connect.space.example."} {
		if got := placement.PlatformPlacementTargets(host); !slices.Equal(got, testEdge1Addresses) {
			t.Errorf("%s: targets = %v, want test-edge-1's addresses", host, got)
		}
	}
	for _, host := range []string{"api.space.example", "connect.other.example", "space.example", ""} {
		if got := placement.PlatformPlacementTargets(host); 0 < len(got) {
			t.Errorf("%s: targets = %v, want none", host, got)
		}
	}
}

// No answer before the first read, after the snapshot outlives ModelMaxAge,
// or while the control's placement is off.
func TestEdgePlacementPlacesOnlyOnAFreshModel(t *testing.T) {
	clock := &testPlacementClock{now: time.Now()}
	store := newTestPlacementStore(clock.Now())
	settings := DefaultEdgePlacementSettings()
	unread := newEdgePlacementWithStore(context.Background(), settings, store, &testPlacementRandom{}, clock.Now)
	defer unread.Close()
	if got := unread.PlatformPlacementTargets("connect.space.example"); 0 < len(got) {
		t.Fatalf("targets before the first read = %v", got)
	}

	placement := newTestEdgePlacement(t, store, clock)
	if got := placement.PlatformPlacementTargets("connect.space.example"); !slices.Equal(got, testEdge1Addresses) {
		t.Fatalf("targets after the first read = %v", got)
	}
	clock.advance(settings.ModelMaxAge + time.Second)
	if got := placement.PlatformPlacementTargets("connect.space.example"); 0 < len(got) {
		t.Fatalf("targets past ModelMaxAge = %v", got)
	}

	store.controlFields = map[string]string{model.ConnectMoveControlFieldPlacement: "off"}
	off := newTestEdgePlacement(t, store, clock)
	if got := off.PlatformPlacementTargets("connect.space.example"); 0 < len(got) {
		t.Fatalf("targets with the control's placement off = %v", got)
	}
	// the operator turns it back on: the next read places again
	store.controlFields = map[string]string{}
	off.tick()
	if got := off.PlatformPlacementTargets("connect.space.example"); !slices.Equal(got, testEdge1Addresses) {
		t.Fatalf("targets once the control's placement is cleared = %v", got)
	}

	disabledSettings := DefaultEdgePlacementSettings()
	disabledSettings.EnablePlacement = false
	store.controlFields = map[string]string{model.ConnectMoveControlFieldPlacement: "on"}
	disabled := newEdgePlacementWithStore(context.Background(), disabledSettings, store, &testPlacementRandom{}, clock.Now)
	defer disabled.Close()
	disabled.tick()
	if got := disabled.PlatformPlacementTargets("connect.space.example"); 0 < len(got) {
		t.Fatalf("the control turned placement on past the code switch: %v", got)
	}
}

// A failed read keeps the last snapshot, which still answers until it ages.
func TestEdgePlacementKeepsTheLastModelWhenAReadFails(t *testing.T) {
	clock := &testPlacementClock{now: time.Now()}
	store := newTestPlacementStore(clock.Now())
	placement := newTestEdgePlacement(t, store, clock)

	store.loadErr = errors.New("test: load table unavailable")
	clock.advance(time.Minute)
	placement.refresh(store.edgeInventory)
	if got := placement.PlatformPlacementTargets("connect.space.example"); !slices.Equal(got, testEdge1Addresses) {
		t.Fatalf("targets after a failed read = %v, want the last snapshot's", got)
	}
	clock.advance(placement.settings.ModelMaxAge)
	if got := placement.PlatformPlacementTargets("connect.space.example"); 0 < len(got) {
		t.Fatalf("targets once the last snapshot aged = %v", got)
	}
}

// An evacuating host takes no arrivals: the control's evacuateHosts reaches
// the placement.
func TestEdgePlacementNeverPlacesOnAnEvacuatingHost(t *testing.T) {
	clock := &testPlacementClock{now: time.Now()}
	store := newTestPlacementStore(clock.Now())
	store.loadRecords = append(store.loadRecords, testPlacementLoad("test-edge-2", 10000, 100, 0.5, clock.Now()))
	store.controlFields = map[string]string{model.ConnectMoveControlFieldEvacuateHosts: "test-edge-1"}
	placement := newTestEdgePlacement(t, store, clock)
	want := []netip.Addr{netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("2001:db8::2")}
	for i := 0; i < 10; i += 1 {
		if got := placement.PlatformPlacementTargets("connect.space.example"); !slices.Equal(got, want) {
			t.Fatalf("targets with test-edge-1 evacuating = %v, want test-edge-2's", got)
		}
	}
}

// A failed dial holds its addresses for every client of the process until
// FailHold passes: the placement offers the other family meanwhile, then
// nothing once both are held, then the host again.
func TestEdgePlacementHoldsAFailedAddress(t *testing.T) {
	clock := &testPlacementClock{now: time.Now()}
	placement := newTestEdgePlacement(t, newTestPlacementStore(clock.Now()), clock)

	placement.PlatformPlacementFailed("connect.space.example", []netip.Addr{netip.MustParseAddr("192.0.2.1")})
	if got := placement.PlatformPlacementTargets("connect.space.example"); !slices.Equal(got, []netip.Addr{netip.MustParseAddr("2001:db8::1")}) {
		t.Fatalf("targets with the ipv4 held = %v, want the ipv6 only", got)
	}
	// with both held, test-edge-1 takes no arrivals; the other hosts have no
	// headroom, so capacity decides among them
	placement.PlatformPlacementFailed("connect.space.example", []netip.Addr{netip.MustParseAddr("2001:db8::1")})
	others := []netip.Addr{netip.MustParseAddr("192.0.2.3"), netip.MustParseAddr("192.0.2.4")}
	if got := placement.PlatformPlacementTargets("connect.space.example"); len(got) != 1 || !slices.Contains(others, got[0]) {
		t.Fatalf("targets with test-edge-1 held = %v, want one of %v", got, others)
	}
	clock.advance(placement.settings.FailHold)
	if got := placement.PlatformPlacementTargets("connect.space.example"); !slices.Equal(got, testEdge1Addresses) {
		t.Fatalf("targets after the hold = %v, want test-edge-1's again", got)
	}
}

// The loop reads nothing before an inventory loads, reloads it only every
// InventoryRefreshInterval, and keeps the last good one while a reload fails.
func TestEdgePlacementReloadsTheInventoryOnItsInterval(t *testing.T) {
	clock := &testPlacementClock{now: time.Now()}
	store := newTestPlacementStore(clock.Now())
	store.inventoryErr = errors.New("test: vault unavailable")
	settings := DefaultEdgePlacementSettings()
	placement := newEdgePlacementWithStore(context.Background(), settings, store, &testPlacementRandom{
		floats: slices.Repeat([]float64{0.5}, 10),
		ints:   slices.Repeat([]int{0}, 20),
	}, clock.Now)
	defer placement.Close()
	placement.tick()
	if got := placement.PlatformPlacementTargets("connect.space.example"); 0 < len(got) {
		t.Fatalf("targets without an inventory = %v", got)
	}
	if store.controlReads != 0 {
		t.Fatalf("the load model was read %d times without an inventory", store.controlReads)
	}

	store.inventoryErr = nil
	placement.tick()
	if got := placement.PlatformPlacementTargets("connect.space.example"); !slices.Equal(got, testEdge1Addresses) {
		t.Fatalf("targets once the inventory loads = %v", got)
	}
	inventoryReads := store.inventoryReads

	// within the interval the model is read but the inventory is not
	clock.advance(settings.InventoryRefreshInterval - time.Second)
	placement.tick()
	if store.inventoryReads != inventoryReads {
		t.Fatalf("the inventory was reloaded within its interval")
	}

	// a failed reload keeps the last good inventory, and the next pass retries
	clock.advance(time.Second)
	store.inventoryErr = errors.New("test: vault unavailable")
	placement.tick()
	if store.inventoryReads != inventoryReads+1 {
		t.Fatalf("inventory reads = %d, want a reload once due", store.inventoryReads)
	}
	if got := placement.PlatformPlacementTargets("connect.space.example"); !slices.Equal(got, testEdge1Addresses) {
		t.Fatalf("targets after a failed reload = %v, want the last inventory's", got)
	}
	placement.tick()
	if store.inventoryReads != inventoryReads+2 {
		t.Fatalf("inventory reads = %d, want a retry on the next pass", store.inventoryReads)
	}
}
