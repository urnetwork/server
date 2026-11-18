// The setup-time metrics (setup_metrics.go): the open kind of a device and of
// a CONNECT, the wait for the window's first exit, the first dial and the
// time to the destination's first byte, through the production HTTP and
// SOCKS dial callbacks on the synctest clock.
package proxy

import (
	"bufio"
	"context"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	proxylib "github.com/urnetwork/proxy"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A proxy id created at createTime, as server.NewId mints one then. Inside a
// synctest bubble server.NewId keeps the last real millisecond (it never
// steps back), so the tests set the creation time themselves.
func setupTestProxyIdAt(createTime time.Time) server.Id {
	id := server.NewId()
	floor := server.ContractIdFloor(createTime)
	copy(id[0:6], floor[0:6])
	return id
}

// One histogram series' sample count and sum.
type setupTestSamples struct {
	count uint64
	sum   float64
}

func setupTestHistogram(t *testing.T, vec *prometheus.HistogramVec, labels ...string) setupTestSamples {
	t.Helper()
	metric := &dto.Metric{}
	if err := vec.WithLabelValues(labels...).(prometheus.Metric).Write(metric); err != nil {
		t.Fatal(err)
	}
	return setupTestSamples{count: metric.GetHistogram().GetSampleCount(), sum: metric.GetHistogram().GetSampleSum()}
}

func setupTestCounter(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := vec.WithLabelValues(labels...).Write(metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetCounter().GetValue()
}

// The listeners a test window holds.
func setupTestListenerCount(window *lazyOpenTestWindow) int {
	window.stateLock.Lock()
	defer window.stateLock.Unlock()
	return len(window.listeners)
}

// Requires the series to have gained exactly count samples summing to sum
// since before.
func requireSetupTestSamples(t *testing.T, name string, before setupTestSamples, after setupTestSamples, count uint64, sum float64) {
	t.Helper()
	// the sum is a float other tests' observations also added to
	if after.count-before.count != count || math.Abs(after.sum-before.sum-sum) > 1e-6 {
		t.Errorf("%s gained %d samples summing to %v, want %d summing to %v", name, after.count-before.count, after.sum-before.sum, count, sum)
	}
}

// A construction is cold when its proxy was created less than ten minutes
// before it began, a little clock skew allowed; any other is a reopen.
func TestProxyDeviceOpenKindReadsTheProxyAge(t *testing.T) {
	buildStart := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		age  time.Duration
		want string
	}{
		{0, proxyOpenCold},
		{time.Second, proxyOpenCold},
		{9*time.Minute + 59*time.Second, proxyOpenCold},
		{10 * time.Minute, proxyOpenReopen},
		{2 * time.Hour, proxyOpenReopen},
		{30 * 24 * time.Hour, proxyOpenReopen},
		// an api clock ahead of this host's
		{-30 * time.Second, proxyOpenCold},
		{-2 * time.Minute, proxyOpenReopen},
	} {
		proxyId := setupTestProxyIdAt(buildStart.Add(-test.age))
		if got := proxyDeviceOpenKind(proxyId, buildStart); got != test.want {
			t.Errorf("a proxy %s old opens %s, want %s", test.age, got, test.want)
		}
	}
}

// The library carries a CONNECT's whole dial budget as the dial context's
// deadline, so every attempt finds the CONNECT's start one budget before it.
func TestProxyConnectStartIsOneBudgetBeforeTheDialDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		begin := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		time.Sleep(7 * time.Second)
		if got := proxyConnectStart(ctx, 30*time.Second, time.Now()); !got.Equal(begin) {
			t.Errorf("a retry 7 s in started at %s, want %s", got, begin)
		}
		if got := proxyConnectStart(context.Background(), 30*time.Second, time.Now()); !got.Equal(time.Now()) {
			t.Errorf("a dial without a deadline started at %s, want now", got)
		}
		// a deadline further than one budget away never puts the start ahead
		if got := proxyConnectStart(ctx, time.Second, time.Now()); !got.Equal(time.Now()) {
			t.Errorf("a short budget started at %s, want now", got)
		}
	})
}

// A CONNECT that began before its device was built, or as the build ended
// (one instant on a fake clock), waited on the construction and takes its
// kind; one that began after found it warm, as does any CONNECT on a device
// the manager did not build.
func TestProxyConnectFindsItsDeviceWarmOnlyAfterItsBuild(t *testing.T) {
	built := time.Date(2026, 11, 18, 6, 0, 0, 0, time.UTC)
	setup := &proxyDeviceSetup{}
	if got := setup.connectOpenKind(built.Add(-time.Hour)); got != proxyOpenWarm {
		t.Errorf("a device the manager did not build opened %s", got)
	}
	for _, kind := range []string{proxyOpenCold, proxyOpenReopen} {
		setup.open.Store(&proxyDeviceOpen{kind: kind, start: built.Add(-2 * time.Second), built: built})
		for connectStart, want := range map[time.Time]string{
			built.Add(-3 * time.Second):  kind,
			built.Add(-time.Millisecond): kind,
			built:                        kind,
			built.Add(time.Millisecond):  proxyOpenWarm,
			built.Add(time.Minute):       proxyOpenWarm,
		} {
			if got := setup.connectOpenKind(connectStart); got != want {
				t.Errorf("%s device: a CONNECT at %s found it %s, want %s", kind, connectStart.Sub(built), got, want)
			}
		}
	}
}

// The first exit is observed once, from the construction's start, whether it
// was already there when the watch began or came later; a device that closes
// first observes nothing; and the watch leaves no listener behind.
func TestProxyDeviceFirstExitWatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		cold := func() setupTestSamples { return setupTestHistogram(t, proxyDeviceFirstExitSeconds, proxyOpenCold) }

		before := cold()
		later := newLazyOpenTestWindow()
		laterSetup := &proxyDeviceSetup{}
		laterSetup.open.Store(&proxyDeviceOpen{kind: proxyOpenCold, start: start, built: start})
		laterSetup.installed(later)
		time.Sleep(4 * time.Second)
		later.addExit()
		later.addExit()
		requireSetupTestSamples(t, "a later first exit", before, cold(), 1, 4)
		if setupTestListenerCount(later) != 0 {
			t.Error("the watch kept its listener after the first exit")
		}

		before = cold()
		ready := newLazyOpenTestWindow()
		ready.addExit()
		readySetup := &proxyDeviceSetup{}
		readySetup.open.Store(&proxyDeviceOpen{kind: proxyOpenCold, start: start, built: start})
		readySetup.installed(ready)
		requireSetupTestSamples(t, "an exit already there", before, cold(), 1, 4)
		if setupTestListenerCount(ready) != 0 {
			t.Error("the watch kept its listener on a window that already routed")
		}

		before = cold()
		never := newLazyOpenTestWindow()
		neverSetup := &proxyDeviceSetup{}
		neverSetup.open.Store(&proxyDeviceOpen{kind: proxyOpenCold, start: start, built: start})
		neverSetup.installed(never)
		neverSetup.closed()
		never.addExit()
		requireSetupTestSamples(t, "a device closed before its first exit", before, cold(), 0, 0)
		if setupTestListenerCount(never) != 0 {
			t.Error("a closed device's watch kept its listener")
		}

		// a device the manager did not build watches nothing
		unbuilt := newLazyOpenTestWindow()
		(&proxyDeviceSetup{}).installed(unbuilt)
		if setupTestListenerCount(unbuilt) != 0 {
			t.Error("a device without its open watched its window")
		}
	})
}

// Only a device's first dial is observed, with its outcome.
func TestProxyDeviceFirstDialObservedOnce(t *testing.T) {
	ok := func() setupTestSamples {
		return setupTestHistogram(t, proxyDeviceFirstDialSeconds, proxyOpenReopen, "ok")
	}
	failed := func() setupTestSamples {
		return setupTestHistogram(t, proxyDeviceFirstDialSeconds, proxyOpenReopen, "error")
	}
	beforeOk, beforeFailed := ok(), failed()
	setup := &proxyDeviceSetup{}
	setup.open.Store(&proxyDeviceOpen{kind: proxyOpenReopen})
	setup.observeDial(2*time.Second, nil)
	setup.observeDial(3*time.Second, nil)
	setup.observeDial(5*time.Second, io.EOF)
	requireSetupTestSamples(t, "the first dial", beforeOk, ok(), 1, 2)
	requireSetupTestSamples(t, "later dials", beforeFailed, failed(), 0, 0)

	failedSetup := &proxyDeviceSetup{}
	failedSetup.open.Store(&proxyDeviceOpen{kind: proxyOpenReopen})
	failedSetup.observeDial(time.Second, io.EOF)
	requireSetupTestSamples(t, "a failed first dial", beforeFailed, failed(), 1, 1)

	beforeOk = ok()
	(&proxyDeviceSetup{}).observeDial(time.Second, nil)
	requireSetupTestSamples(t, "a device the manager did not build", beforeOk, ok(), 0, 0)
}

// The manager counts each installed device by how it was opened: a proxy
// created a minute ago opens cold, one created yesterday reopens.
func TestProxyDeviceBuildsCountedByOpenKind(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		window := newLazyOpenTestWindow()
		window.addExit()
		newLazyOpenTestDevices(t, manager, window)
		coldBefore := setupTestCounter(t, proxyDeviceBuilds, proxyOpenCold)
		reopenBefore := setupTestCounter(t, proxyDeviceBuilds, proxyOpenReopen)
		for _, age := range []time.Duration{time.Minute, 24 * time.Hour} {
			if _, err := manager.OpenProxyDevice(setupTestProxyIdAt(time.Now().Add(-age))); err != nil {
				t.Fatal(err)
			}
		}
		if got := setupTestCounter(t, proxyDeviceBuilds, proxyOpenCold) - coldBefore; got != 1 {
			t.Errorf("cold builds %v, want 1", got)
		}
		if got := setupTestCounter(t, proxyDeviceBuilds, proxyOpenReopen) - reopenBefore; got != 1 {
			t.Errorf("reopen builds %v, want 1", got)
		}
	})
}

// Sends a CONNECT through the production handler over a pipe and returns the
// client's side once the proxy answered 200, with its reader.
func startSetupTestConnect(t *testing.T, core *proxylib.HttpProxy, proxyId server.Id, handlerDone chan<- struct{}) (net.Conn, *bufio.Reader) {
	t.Helper()
	client, accepted := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	request := httptest.NewRequestWithContext(server.WithoutPostgres(context.Background()), http.MethodConnect, "https://target.example:443", nil)
	request.RemoteAddr = "192.0.2.1:1234"
	request.Header.Set("Proxy-Authorization", httpConnectTestBasic(proxyId))
	go func() {
		defer close(handlerDone)
		core.ServeHTTP(&httpBudgetTestHijacker{ResponseRecorder: httptest.NewRecorder(), connection: accepted}, request)
	}()
	reader := bufio.NewReader(client)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT answered %d", response.StatusCode)
	}
	return client, reader
}

// The destination's end of the devices' last dial.
func setupTestLastDialPipe(t *testing.T, devices *lazyOpenTestDevices) net.Conn {
	t.Helper()
	devices.stateLock.Lock()
	defer devices.stateLock.Unlock()
	if len(devices.dialPipes) == 0 {
		t.Fatal("no dial")
	}
	return devices.dialPipes[len(devices.dialPipes)-1]
}

// Writes one byte from the destination and reads it at the client.
func setupTestFirstByte(t *testing.T, destination net.Conn, client *bufio.Reader) {
	t.Helper()
	written := make(chan error, 1)
	go func() {
		_, err := destination.Write([]byte{7})
		written <- err
	}()
	if b, err := client.ReadByte(); err != nil || b != 7 {
		t.Fatalf("the client read %v, %v", b, err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}

// A first CONNECT to a new proxy builds its device, waits 4 s for the
// window's first exit and dials it, and the destination answers a second
// later: the route wait, the first exit, the first dial and the setup to the
// first byte are observed as cold. A second CONNECT finds the device warm.
func TestProxyHttpConnectSetupMetrics(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		proxyId := setupTestProxyIdAt(time.Now())
		seedHttpConnectTestLock(manager, proxyId, true)
		window := newLazyOpenTestWindow()
		devices := newLazyOpenTestDevices(t, manager, window)
		core := newHttpConnectTestProxy(manager)

		setupCold := setupTestHistogram(t, proxyConnectSetupSeconds, "http", proxyOpenCold)
		setupWarm := setupTestHistogram(t, proxyConnectSetupSeconds, "http", proxyOpenWarm)
		waitCold := setupTestHistogram(t, proxyConnectRouteWaitSeconds, proxyOpenCold, "routed")
		waitWarm := setupTestHistogram(t, proxyConnectRouteWaitSeconds, proxyOpenWarm, "routed")
		firstExit := setupTestHistogram(t, proxyDeviceFirstExitSeconds, proxyOpenCold)
		firstDial := setupTestHistogram(t, proxyDeviceFirstDialSeconds, proxyOpenCold, "ok")

		go func() {
			time.Sleep(4 * time.Second)
			window.addExit()
		}()
		handlerDone := make(chan struct{})
		client, reader := startSetupTestConnect(t, core, proxyId, handlerDone)
		time.Sleep(time.Second)
		setupTestFirstByte(t, setupTestLastDialPipe(t, devices), reader)
		// later bytes observe nothing more
		setupTestFirstByte(t, setupTestLastDialPipe(t, devices), reader)

		requireSetupTestSamples(t, "the cold route wait", waitCold, setupTestHistogram(t, proxyConnectRouteWaitSeconds, proxyOpenCold, "routed"), 1, 4)
		requireSetupTestSamples(t, "the first exit", firstExit, setupTestHistogram(t, proxyDeviceFirstExitSeconds, proxyOpenCold), 1, 4)
		requireSetupTestSamples(t, "the first dial", firstDial, setupTestHistogram(t, proxyDeviceFirstDialSeconds, proxyOpenCold, "ok"), 1, 0)
		requireSetupTestSamples(t, "the cold setup", setupCold, setupTestHistogram(t, proxyConnectSetupSeconds, "http", proxyOpenCold), 1, 5)

		_ = client.Close()
		<-handlerDone

		time.Sleep(10 * time.Second)
		handlerDone = make(chan struct{})
		client, reader = startSetupTestConnect(t, core, proxyId, handlerDone)
		time.Sleep(2 * time.Second)
		setupTestFirstByte(t, setupTestLastDialPipe(t, devices), reader)
		requireSetupTestSamples(t, "the warm route wait", waitWarm, setupTestHistogram(t, proxyConnectRouteWaitSeconds, proxyOpenWarm, "routed"), 1, 0)
		requireSetupTestSamples(t, "the warm setup", setupWarm, setupTestHistogram(t, proxyConnectSetupSeconds, "http", proxyOpenWarm), 1, 2)
		requireSetupTestSamples(t, "a second dial", firstDial, setupTestHistogram(t, proxyDeviceFirstDialSeconds, proxyOpenCold, "ok"), 1, 0)
		_ = client.Close()
		<-handlerDone
	})
}

// A CONNECT whose window never gets an exit observes its whole wait as
// unrouted, and no setup. The device takes a second to build, and the
// library's budget, 20 s here, ends the wait at 19 s. On a fake clock a budget
// equal to the dial callback's own 30 s timeout would expire in the same
// instant, which a real clock never does, and could let the library start a
// second attempt as the budget ends.
func TestProxyHttpConnectUnroutedWait(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		proxyId := setupTestProxyIdAt(time.Now().Add(-time.Hour))
		seedHttpConnectTestLock(manager, proxyId, true)
		devices := newLazyOpenTestDevices(t, manager, newLazyOpenTestWindow())
		manager.proxyDeviceBuilder = func(proxyId server.Id) (*ProxyDevice, error) {
			time.Sleep(time.Second)
			return devices.build(proxyId)
		}
		core := newHttpConnectTestProxy(manager)
		core.Settings().ProxyConnectBudget = 20 * time.Second

		unrouted := setupTestHistogram(t, proxyConnectRouteWaitSeconds, proxyOpenReopen, "unrouted")
		setup := setupTestHistogram(t, proxyConnectSetupSeconds, "http", proxyOpenReopen)
		responseDone, handlerDone := startHttpBudgetTestConnect(t, core, proxyId)
		if code := <-responseDone; code != http.StatusGatewayTimeout {
			t.Fatalf("CONNECT answered %d, want 504", code)
		}
		<-handlerDone
		requireSetupTestSamples(t, "the unrouted wait", unrouted, setupTestHistogram(t, proxyConnectRouteWaitSeconds, proxyOpenReopen, "unrouted"), 1, 19)
		requireSetupTestSamples(t, "the setup", setup, setupTestHistogram(t, proxyConnectSetupSeconds, "http", proxyOpenReopen), 0, 0)
	})
}

// A SOCKS CONNECT's setup counts from its request: here a reopened proxy's
// device takes 3 s to its first exit, and the destination answers 2 s after
// the dial.
func TestProxySocksConnectSetupMetrics(t *testing.T) {
	configureHttpBudgetTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		proxyId := setupTestProxyIdAt(time.Now().Add(-48 * time.Hour))
		window := newLazyOpenTestWindow()
		devices := newLazyOpenTestDevices(t, manager, window)
		core := (&socks5Server{ctx: manager.ctx, settings: DefaultProxySettings(), proxyDeviceManager: manager}).newSocksProxy()
		request := &proxylib.Request{
			AuthContext: &proxylib.AuthContext{Payload: map[string]string{"username": model.SignProxyId(proxyId)}},
			DestAddr:    &proxylib.AddrSpec{IP: net.ParseIP("192.0.2.1"), Port: 443},
		}

		setup := setupTestHistogram(t, proxyConnectSetupSeconds, "socks", proxyOpenReopen)
		wait := setupTestHistogram(t, proxyConnectRouteWaitSeconds, proxyOpenReopen, "routed")
		go func() {
			time.Sleep(3 * time.Second)
			window.addExit()
		}()
		conn, err := core.ConnectDialWithRequest(context.Background(), request, "tcp", "192.0.2.1:443")
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		time.Sleep(2 * time.Second)
		go func() {
			_, _ = setupTestLastDialPipe(t, devices).Write([]byte{7})
		}()
		buffer := make([]byte, 1)
		if n, err := conn.Read(buffer); err != nil || n != 1 {
			t.Fatalf("read %d, %v", n, err)
		}
		requireSetupTestSamples(t, "the socks route wait", wait, setupTestHistogram(t, proxyConnectRouteWaitSeconds, proxyOpenReopen, "routed"), 1, 3)
		requireSetupTestSamples(t, "the socks setup", setup, setupTestHistogram(t, proxyConnectSetupSeconds, "socks", proxyOpenReopen), 1, 5)
	})
}
