package work

// The uptime pass's webrtc pool (ExtenderProbeSettings): the addresses
// accepted on the webrtc carrier alone are probed on a pool sized to their
// number, beside the pool of every other address, so a pass in which every
// probe waits out its address budget still ends inside the cadence. The pass
// runs against the churn tests' in-memory store (extender_probe_churn_test.go)
// on the bubble's clock, so a probe that waits out its 15 s budget does so at
// once and to the instant, with no database and no network.

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// The webrtc pool's size and the worst case it gives, at the defaults: the
// fewest workers that end a pass whose every probe waits out its 15 s budget
// within four minutes, never fewer than the other pool's 16 and never more
// than 256. Past what 256 workers fit, the worst case grows instead.
func TestExtenderProbeWebRtcConcurrencySizing(t *testing.T) {
	settings := DefaultExtenderProbeSettings()
	for _, c := range []struct {
		webRtcCount     int
		concurrency     int
		worstCaseRounds int
	}{
		{webRtcCount: 0, concurrency: 16, worstCaseRounds: 0},
		{webRtcCount: 1, concurrency: 16, worstCaseRounds: 1},
		{webRtcCount: 200, concurrency: 16, worstCaseRounds: 13},
		{webRtcCount: 256, concurrency: 16, worstCaseRounds: 16},
		{webRtcCount: 257, concurrency: 17, worstCaseRounds: 16},
		{webRtcCount: 1000, concurrency: 63, worstCaseRounds: 16},
		{webRtcCount: 4000, concurrency: 250, worstCaseRounds: 16},
		{webRtcCount: 4096, concurrency: 256, worstCaseRounds: 16},
		{webRtcCount: 4097, concurrency: 256, worstCaseRounds: 17},
		{webRtcCount: 10000, concurrency: 256, worstCaseRounds: 40},
	} {
		concurrency := settings.webRtcConcurrency(c.webRtcCount, ExtenderProbeAddressTimeout)
		worstCase := extenderProbeWorstCaseDuration(c.webRtcCount, concurrency, ExtenderProbeAddressTimeout)
		if concurrency != c.concurrency || worstCase != time.Duration(c.worstCaseRounds)*ExtenderProbeAddressTimeout {
			t.Errorf("%d addresses: %d at once taking %s at worst, want %d taking %d rounds",
				c.webRtcCount, concurrency, worstCase, c.concurrency, c.worstCaseRounds)
		}
	}

	// settings that probe one at a time, and a pass time below one budget
	narrow := &ExtenderProbeSettings{
		Concurrency:          0,
		WebRtcPassTimeout:    time.Second,
		MaxWebRtcConcurrency: 0,
	}
	if concurrency := narrow.webRtcConcurrency(100, ExtenderProbeAddressTimeout); concurrency != 1 {
		t.Errorf("non-positive bounds gave %d at once, want one", concurrency)
	}
	short := DefaultExtenderProbeSettings()
	short.WebRtcPassTimeout = time.Second
	if concurrency := short.webRtcConcurrency(100, ExtenderProbeAddressTimeout); concurrency != 100 {
		t.Errorf("a pass time below one address budget gave %d at once for 100 addresses, want 100", concurrency)
	}
}

// The activation and the uptime probe agree on the cap: at the activation's
// cap (controller ExtenderActivateSettings.MaxWebRtcOnlyExtenders), with one
// address on each family, the most an extender holds, the webrtc pool fits a
// pass whose every probe waits out its budget within its pass time, inside
// the cadence and the pass's probe window, without reaching past its bound.
// A cap raised past what the pool's bound fits fails here.
func TestExtenderProbeWebRtcPoolCoversTheActivationCap(t *testing.T) {
	settings := DefaultExtenderProbeSettings()
	maxWebRtcOnlyExtenders := controller.DefaultExtenderActivateSettings().MaxWebRtcOnlyExtenders
	addressCount := 2 * maxWebRtcOnlyExtenders
	concurrency := settings.webRtcConcurrency(addressCount, ExtenderProbeAddressTimeout)
	worstCase := extenderProbeWorstCaseDuration(addressCount, concurrency, ExtenderProbeAddressTimeout)
	if settings.WebRtcPassTimeout < worstCase || settings.MaxWebRtcConcurrency < concurrency {
		t.Fatalf("at the cap of %d extenders, %d addresses on %d workers take %s at worst, past the pass time %s",
			maxWebRtcOnlyExtenders, addressCount, concurrency, worstCase, settings.WebRtcPassTimeout)
	}
	if ExtenderProbeTimeout <= settings.WebRtcPassTimeout || ExtenderProbeMaxTime-ExtenderProbeRecordReserve < settings.WebRtcPassTimeout {
		t.Fatalf("the webrtc pass time %s is not inside the cadence %s and the probe window %s",
			settings.WebRtcPassTimeout, ExtenderProbeTimeout, ExtenderProbeMaxTime-ExtenderProbeRecordReserve)
	}
	connect.AssertEqual(t, maxWebRtcOnlyExtenders, 2000)
	connect.AssertEqual(t, concurrency, 250)
}

// Adds one active extender of its own network to the store, with an address
// on each family given, both listing carriers, at documentation addresses.
func addTestWebRtcPoolExtender(store *testChurnStore, index int, carriers []string, ipVersions ...int) {
	store.stateLock.Lock()
	defer store.stateLock.Unlock()
	extender := &testChurnExtender{
		extenderId: server.NewId(),
		networkId:  server.NewId(),
		publicKey:  []byte(fmt.Sprintf("extender-public-key-pool-%06d", index)),
		active:     true,
		addresses:  map[int]*testChurnAddress{},
		carriers:   carriers,
	}
	documentationPrefixes := [][3]byte{{192, 0, 2}, {198, 51, 100}, {203, 0, 113}}
	for _, ipVersion := range ipVersions {
		prefix := documentationPrefixes[index%len(documentationPrefixes)]
		ip := netip.AddrFrom4([4]byte{prefix[0], prefix[1], prefix[2], byte(1 + index%250)})
		if ipVersion == 6 {
			ip = netip.MustParseAddr(fmt.Sprintf("2001:db8:7::%x", index))
		}
		extender.addresses[ipVersion] = &testChurnAddress{
			ipVersion:    ipVersion,
			ip:           ip,
			activateTime: testExtenderChurnStart,
			active:       true,
		}
	}
	store.order = append(store.order, extender.extenderId)
	store.extenders[extender.extenderId] = extender
}

// The worst-case pass at 100, 500 and 2,000 extenders on webrtc alone, each
// with an address on both families, beside 200 tcp addresses: every probe
// waits out its 15 s budget. The pass probes every address once and ends
// inside the five-minute cadence, the webrtc pool at the size that fits its
// four minutes -- 16, 63 and 250 at once -- and the tcp pool at its 16 at the
// same time, so the pass takes the longer of the two, not their sum. Before,
// every address shared 16 workers: at 100 extenders such a pass took six
// minutes and a quarter, at 2,000 it stopped starting probes at the eight-minute
// probe window with most addresses unprobed.
func TestExtenderProbeWorstCasePassFitsTheCadence(t *testing.T) {
	const tcpExtenderCount = 100
	tcpAddressCount := 2 * tcpExtenderCount
	settings := DefaultExtenderProbeSettings()
	for _, c := range []struct {
		webRtcExtenderCount int
		webRtcConcurrency   int
	}{
		{webRtcExtenderCount: 100, webRtcConcurrency: 16},
		{webRtcExtenderCount: 500, webRtcConcurrency: 63},
		{webRtcExtenderCount: 2000, webRtcConcurrency: 250},
	} {
		webRtcAddressCount := 2 * c.webRtcExtenderCount
		world := newTestChurnWorld(t)
		for i := range tcpExtenderCount {
			addTestWebRtcPoolExtender(world.store, i, []string{connect.ExtenderCarrierTcp}, 4, 6)
		}
		for i := range c.webRtcExtenderCount {
			addTestWebRtcPoolExtender(world.store, tcpExtenderCount+i, []string{connect.ExtenderCarrierWebRtc, connect.ExtenderCarrierQuic}, 4, 6)
		}

		// the real dispatch over the address's carriers, with each path
		// waiting out the address's budget
		world.restoreProbeDispatch(t)
		stateLock := sync.Mutex{}
		probed := map[string]int{}
		inFlight := map[string]int{}
		maxInFlight := map[string]int{}
		maxTotalInFlight := 0
		waitOut := func(ctx context.Context, path string) error {
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				probed[path] += 1
				inFlight[path] += 1
				maxInFlight[path] = max(maxInFlight[path], inFlight[path])
				maxTotalInFlight = max(maxTotalInFlight, inFlight[connect.ExtenderCarrierTcp]+inFlight[connect.ExtenderCarrierWebRtc])
			}()
			<-ctx.Done()
			stateLock.Lock()
			defer stateLock.Unlock()
			inFlight[path] -= 1
			return ctx.Err()
		}
		stubTestUptimePaths(t,
			func(ctx context.Context, target *model.NetworkExtenderProbeTarget, serverName string, destinationHost string) error {
				return waitOut(ctx, connect.ExtenderCarrierTcp)
			},
			func(ctx context.Context, target *model.NetworkExtenderProbeTarget, destinationHost string) error {
				return waitOut(ctx, connect.ExtenderCarrierWebRtc)
			},
		)

		var result *ExtenderProbeResult
		var elapsed time.Duration
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			result = world.pass()
			elapsed = time.Since(start)
		})

		webRtcWorstCase := extenderProbeWorstCaseDuration(webRtcAddressCount, c.webRtcConcurrency, ExtenderProbeAddressTimeout)
		tcpWorstCase := extenderProbeWorstCaseDuration(tcpAddressCount, settings.Concurrency, ExtenderProbeAddressTimeout)
		if ExtenderProbeTimeout <= elapsed || settings.WebRtcPassTimeout < elapsed || elapsed != max(webRtcWorstCase, tcpWorstCase) {
			t.Errorf("%d extenders on webrtc alone: the worst-case pass took %s, want %s, inside the %s cadence",
				c.webRtcExtenderCount, elapsed, max(webRtcWorstCase, tcpWorstCase), ExtenderProbeTimeout)
		}
		if probed[connect.ExtenderCarrierWebRtc] != webRtcAddressCount || probed[connect.ExtenderCarrierTcp] != tcpAddressCount {
			t.Errorf("%d extenders on webrtc alone: probed %d webrtc and %d tcp addresses, want %d and %d",
				c.webRtcExtenderCount, probed[connect.ExtenderCarrierWebRtc], probed[connect.ExtenderCarrierTcp], webRtcAddressCount, tcpAddressCount)
		}
		if maxInFlight[connect.ExtenderCarrierWebRtc] != c.webRtcConcurrency || maxInFlight[connect.ExtenderCarrierTcp] != settings.Concurrency ||
			maxTotalInFlight != c.webRtcConcurrency+settings.Concurrency {
			t.Errorf("%d extenders on webrtc alone: at most %d webrtc and %d tcp probes at once, %d together, want %d and %d at the same time",
				c.webRtcExtenderCount, maxInFlight[connect.ExtenderCarrierWebRtc], maxInFlight[connect.ExtenderCarrierTcp], maxTotalInFlight,
				c.webRtcConcurrency, settings.Concurrency)
		}
		if result.Probed != webRtcAddressCount+tcpAddressCount || result.WebRtcOnlyProbed != webRtcAddressCount || result.WebRtcConcurrency != c.webRtcConcurrency {
			t.Errorf("%d extenders on webrtc alone: result %+v", c.webRtcExtenderCount, result)
		}
	}
}

// Undoes the churn world's prober for the length of the test, so a pass
// dispatches each address over its carriers (probeExtenderAddress) to the
// path probes.
func (self *testChurnWorld) restoreProbeDispatch(t testing.TB) {
	t.Helper()
	previous := probeExtenderAddress
	probeExtenderAddress = defaultProbeExtenderAddress
	t.Cleanup(func() {
		probeExtenderAddress = previous
	})
}

// Replaces the two path probes for the length of the test.
func stubTestUptimePaths(
	t testing.TB,
	tcp func(ctx context.Context, target *model.NetworkExtenderProbeTarget, serverName string, destinationHost string) error,
	webRtc func(ctx context.Context, target *model.NetworkExtenderProbeTarget, destinationHost string) error,
) {
	t.Helper()
	previousTcp := probeExtenderTcpAddress
	previousWebRtc := probeExtenderWebRtcAddress
	probeExtenderTcpAddress = tcp
	probeExtenderWebRtcAddress = webRtc
	t.Cleanup(func() {
		probeExtenderTcpAddress = previousTcp
		probeExtenderWebRtcAddress = previousWebRtc
	})
}

// The production dispatch, captured before any test replaces it.
var defaultProbeExtenderAddress = probeExtenderAddress
