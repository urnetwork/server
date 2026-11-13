package proxy

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

type testUniqueExitClock struct {
	mutex sync.Mutex
	now   time.Time
}

func (self *testUniqueExitClock) Now() time.Time {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	return self.now
}

func (self *testUniqueExitClock) Advance(d time.Duration) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.now = self.now.Add(d)
}

// The share spends up to its burst at once, refills at its rate, and never
// holds more than the burst. A zero rate is unlimited.
func TestUniqueExitShareHoldsTheRateAndTheBurst(t *testing.T) {
	clock := &testUniqueExitClock{now: time.Unix(1_762_380_000, 0)}
	share := newUniqueExitShare(clock.Now)

	connect.AssertEqual(t, share.allow(1<<30), true)

	share.setRate(1_000_000)
	connect.AssertEqual(t, share.allow(250_000), true)
	connect.AssertEqual(t, share.allow(1), false)

	clock.Advance(100 * time.Millisecond)
	connect.AssertEqual(t, share.allow(100_000), true)
	connect.AssertEqual(t, share.allow(1), false)

	// a long idle refills only to the burst
	clock.Advance(time.Hour)
	connect.AssertEqual(t, share.allow(250_000), true)
	connect.AssertEqual(t, share.allow(1), false)
}

type testSleeps struct {
	mutex sync.Mutex
	total time.Duration
	clock *testUniqueExitClock
}

func (self *testSleeps) sleep(ctx context.Context, d time.Duration) error {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.total += d
	self.clock.Advance(d)
	return nil
}

type testCountingConn struct {
	net.Conn
	written int
}

func (self *testCountingConn) Write(b []byte) (int, error) {
	self.written += len(b)
	return len(b), nil
}

// A paced connection writes everything, and waits the share's time for what
// is over the burst: 1 MB at 1 MB/s with a 250 kB burst waits 0.75 s.
func TestUniqueExitShareConnPacesWrites(t *testing.T) {
	clock := &testUniqueExitClock{now: time.Unix(1_762_380_000, 0)}
	sleeps := &testSleeps{clock: clock}
	share := newUniqueExitShare(clock.Now)
	share.sleep = sleeps.sleep
	share.setRate(1_000_000)

	inner := &testCountingConn{}
	conn := &uniqueExitShareConn{Conn: inner, ctx: context.Background(), share: share}
	n, err := conn.Write(make([]byte, 1_000_000))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, n, 1_000_000)
	connect.AssertEqual(t, inner.written, 1_000_000)
	if sleeps.total < 740*time.Millisecond || 760*time.Millisecond < sleeps.total {
		t.Fatalf("paced 1 MB at 1 MB/s with a 250 kB burst in %s, want 0.75 s", sleeps.total)
	}
}

type testCallCountingConn struct {
	net.Conn
	calls   int
	written int
}

func (self *testCallCountingConn) Write(b []byte) (int, error) {
	self.calls += 1
	self.written += len(b)
	return len(b), nil
}

// A share without a rate (the device's loop has not read its exit yet, or the
// provider has no throughput estimate) is unlimited: a connection writes in
// one call and never waits. Before the fix a share that had never been given a
// rate had a burst of zero, so every write went out one byte per call.
func TestUniqueExitShareWithoutARateWritesAtOnce(t *testing.T) {
	for _, rated := range []bool{false, true} {
		clock := &testUniqueExitClock{now: time.Unix(1_762_380_000, 0)}
		sleeps := &testSleeps{clock: clock}
		share := newUniqueExitShare(clock.Now)
		share.sleep = sleeps.sleep
		if rated {
			// no estimate
			share.setRate(0)
		}
		inner := &testCallCountingConn{}
		conn := &uniqueExitShareConn{Conn: inner, ctx: context.Background(), share: share}
		n, err := conn.Write(make([]byte, 1_000_000))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, n, 1_000_000)
		connect.AssertEqual(t, inner.written, 1_000_000)
		connect.AssertEqual(t, inner.calls, 1)
		connect.AssertEqual(t, sleeps.total, time.Duration(0))
	}
}

type testUniqueExitSource struct {
	mutex     sync.Mutex
	exit      *model.ProxyExitForDevice
	confirms  [][]byte
	failings  []string
	confirmed []server.Id
}

func (self *testUniqueExitSource) GetProxyExitForDevice(ctx context.Context, proxyClientId server.Id) *model.ProxyExitForDevice {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	if self.exit == nil {
		return nil
	}
	exit := *self.exit
	return &exit
}

func (self *testUniqueExitSource) ConfirmProxyExit(ctx context.Context, proxyClientId server.Id, assignmentId server.Id, observedKey []byte, now time.Time) string {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.confirms = append(self.confirms, observedKey)
	self.confirmed = append(self.confirmed, assignmentId)
	return model.ProxyExitConfirmMatch
}

func (self *testUniqueExitSource) ReportProxyExitFailing(ctx context.Context, proxyClientId server.Id, assignmentId server.Id, reason string, now time.Time) {
	self.mutex.Lock()
	defer self.mutex.Unlock()
	self.failings = append(self.failings, reason)
}

type testUniqueExitDevice struct {
	loop      *proxyUniqueExit
	source    *testUniqueExitSource
	clock     *testUniqueExitClock
	locations []*sdk.ConnectLocation
	echoIp    netip.Addr
	echoErr   error
}

func newTestUniqueExitDevice(initialProvider *server.Id, exit *model.ProxyExitForDevice) *testUniqueExitDevice {
	device := &testUniqueExitDevice{
		source: &testUniqueExitSource{exit: exit},
		clock:  &testUniqueExitClock{now: time.Unix(1_762_380_000, 0)},
		echoIp: netip.MustParseAddr("203.0.113.44"),
	}
	device.loop = &proxyUniqueExit{
		clientId: server.NewId(),
		settings: model.DefaultUniqueExitSettings(),
		source:   device.source,
		setLocation: func(location *sdk.ConnectLocation) {
			device.locations = append(device.locations, location)
		},
		echo: func(ctx context.Context) (netip.Addr, error) {
			return device.echoIp, device.echoErr
		},
		share:             newUniqueExitShare(device.clock.Now),
		now:               device.clock.Now,
		initialProviderId: initialProvider,
	}
	return device
}

// A device created pinned to its exit is not repinned on its first step: a
// repin would only rebuild its one window client. A replacement repins it to
// the new provider, once, and it confirms the new exit at once.
func TestUniqueExitDeviceRepinsOnlyWhenTheProviderChanges(t *testing.T) {
	defer model.Testing_SetUniqueExitKeySecret(bytes.Repeat([]byte{9}, 32))()
	first := server.NewId()
	device := newTestUniqueExitDevice(&first, &model.ProxyExitForDevice{
		AssignmentId:            server.NewId(),
		ProviderClientId:        first,
		State:                   "pending",
		EstimatedBytesPerSecond: 2_000_000,
	})
	ctx := context.Background()

	device.loop.step(ctx)
	connect.AssertEqual(t, len(device.locations), 0)
	connect.AssertEqual(t, len(device.source.confirms), 1)
	connect.AssertEqual(t, bytes.Equal(device.source.confirms[0], model.UniqueExitEgressKey(device.echoIp)), true)
	// half the provider's estimate
	connect.AssertEqual(t, device.loop.share.bytesPerSecond, float64(1_000_000))

	// within the confirm interval nothing more happens
	device.clock.Advance(time.Minute)
	device.loop.step(ctx)
	connect.AssertEqual(t, len(device.source.confirms), 1)

	second := server.NewId()
	secondAssignment := server.NewId()
	device.source.exit = &model.ProxyExitForDevice{
		AssignmentId:     secondAssignment,
		ProviderClientId: second,
		State:            "pending",
	}
	device.loop.step(ctx)
	connect.AssertEqual(t, len(device.locations), 1)
	connect.AssertEqual(t, server.RequireIdFromBytes(device.locations[0].ConnectLocationId.ClientId.Bytes()), second)
	connect.AssertEqual(t, len(device.source.confirms), 2)
	connect.AssertEqual(t, device.source.confirmed[1], secondAssignment)
	// no estimate, no share
	connect.AssertEqual(t, device.loop.share.allow(1<<30), true)

	device.loop.step(ctx)
	connect.AssertEqual(t, len(device.locations), 1)

	// after the confirm interval, the exit is confirmed again
	device.clock.Advance(device.loop.settings.ConfirmInterval)
	device.loop.step(ctx)
	connect.AssertEqual(t, len(device.source.confirms), 3)
}

// A device that cannot reach its exit reports it unreachable after the
// configured number of failed confirms, not before, and a later success
// confirms as usual. An exit being replaced is not confirmed.
func TestUniqueExitDeviceReportsAnUnreachableExit(t *testing.T) {
	defer model.Testing_SetUniqueExitKeySecret(bytes.Repeat([]byte{9}, 32))()
	provider := server.NewId()
	device := newTestUniqueExitDevice(&provider, &model.ProxyExitForDevice{
		AssignmentId:     server.NewId(),
		ProviderClientId: provider,
		State:            "active",
	})
	ctx := context.Background()
	device.echoErr = errors.New("no route")
	for i := 0; i < device.loop.settings.UnreachableConfirmFailures-1; i += 1 {
		device.loop.step(ctx)
	}
	connect.AssertEqual(t, len(device.source.failings), 0)
	device.loop.step(ctx)
	connect.AssertEqual(t, device.source.failings, []string{model.ProxyExitReasonUnreachable})
	// still failing: reported again only after another threshold's worth
	for i := 0; i < device.loop.settings.UnreachableConfirmFailures-1; i += 1 {
		device.loop.step(ctx)
	}
	connect.AssertEqual(t, len(device.source.failings), 1)
	device.loop.step(ctx)
	connect.AssertEqual(t, len(device.source.failings), 2)

	device.echoErr = nil
	device.loop.step(ctx)
	connect.AssertEqual(t, len(device.source.confirms), 1)

	device.source.exit.State = "replacing"
	device.clock.Advance(device.loop.settings.ConfirmInterval)
	device.loop.step(ctx)
	connect.AssertEqual(t, len(device.source.confirms), 1)
}

// WireGuard packets over the share are dropped, counted, and the device's
// other packets are untouched; a device without unique exit polices nothing.
func TestUniqueExitDevicePolicesWireGuardPackets(t *testing.T) {
	plain := &ProxyDevice{}
	connect.AssertEqual(t, plain.policeUniqueExitPacket("client_to_destination", 1<<30), true)

	clock := &testUniqueExitClock{now: time.Unix(1_762_380_000, 0)}
	share := newUniqueExitShare(clock.Now)
	share.setRate(1_000_000)
	pinned := &ProxyDevice{uniqueExitShare: share}
	connect.AssertEqual(t, pinned.policeUniqueExitPacket("client_to_destination", 200_000), true)
	connect.AssertEqual(t, pinned.policeUniqueExitPacket("destination_to_client", 200_000), false)
	clock.Advance(200 * time.Millisecond)
	connect.AssertEqual(t, pinned.policeUniqueExitPacket("destination_to_client", 200_000), true)
}

// A unique-exit proxy's pinned location reaches the data plane as one
// provider spec naming its exit, which the multi-client generator runs as a
// fixed window of exactly one client: no spare windows to other providers,
// whose exits would not be the proxy's unique one. A normal hosted proxy's
// best-available location stays an auto window. The spec is the one the sdk
// derives from a location (sdk/device_local_preferences.go,
// locationProviderSpecs).
func TestUniqueExitPinnedLocationIsAFixedWindowOfOne(t *testing.T) {
	provider := server.NewId()
	location := model.UniqueExitPinnedLocation(provider, "de")
	locationSpec := func(location *sdk.ConnectLocation) *connect.ProviderSpec {
		spec := &connect.ProviderSpec{BestAvailable: location.ConnectLocationId.BestAvailable}
		if id := location.ConnectLocationId.LocationId; id != nil {
			connectId := connect.Id(id.Bytes())
			spec.LocationId = &connectId
		}
		if id := location.ConnectLocationId.LocationGroupId; id != nil {
			connectId := connect.Id(id.Bytes())
			spec.LocationGroupId = &connectId
		}
		if id := location.ConnectLocationId.ClientId; id != nil {
			connectId := connect.Id(id.Bytes())
			spec.ClientId = &connectId
		}
		return spec
	}
	fixedSize := func(spec *connect.ProviderSpec) (int, bool) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		generator := connect.NewApiMultiClientGenerator(
			ctx,
			[]*connect.ProviderSpec{spec},
			connect.NewClientStrategyWithDefaults(ctx),
			nil,
			"https://api.unique-exit.invalid",
			"test-jwt",
			"wss://platform.unique-exit.invalid",
			"unique exit test",
			"test",
			"test",
			nil,
			connect.DefaultClientSettings,
			connect.DefaultApiMultiClientGeneratorSettings(),
		)
		return generator.FixedDestinationSize()
	}

	pinned := locationSpec(location)
	connect.AssertEqual(t, pinned.LocationId == nil && pinned.LocationGroupId == nil && !pinned.BestAvailable, true)
	connect.AssertEqual(t, server.Id(*pinned.ClientId), provider)
	size, fixed := fixedSize(pinned)
	connect.AssertEqual(t, size, 1)
	connect.AssertEqual(t, fixed, true)

	size, fixed = fixedSize(&connect.ProviderSpec{BestAvailable: true})
	connect.AssertEqual(t, size, 0)
	connect.AssertEqual(t, fixed, false)
}
