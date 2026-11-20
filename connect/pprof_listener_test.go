package connect

// The loopback pprof listener (pprof_listener.go): what it binds, serves and
// refuses, its port gauge, its kill switch, its bounds, and its life inside
// connect's start, as run.go wires it.

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime/pprof"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
)

// A listener publishing in a registry of its own, closed at the test's end.
func testPprofListener(t *testing.T, settings *pprofListenerSettings) (*pprofListener, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	listener, err := newPprofListener(settings, registry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(listener.Close)
	return listener, registry
}

// A request of path on the loopback port, on a connection of its own: the
// status and the body. A transport failure is status 0.
func testPprofRequest(method string, port int, path string) (int, http.Header, []byte) {
	client := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		// a hang guard: no request here waits on time
		Timeout: 60 * time.Second,
	}
	request, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	if err != nil {
		return 0, nil, nil
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, nil
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, nil, nil
	}
	return response.StatusCode, response.Header, body
}

// A GET of path on the listener: the status and the body.
func testPprofGet(listener *pprofListener, path string) (int, []byte) {
	status, _, body := testPprofRequest(http.MethodGet, listener.address.Port, path)
	return status, body
}

// The protobuf of a gzipped profile, as runtime/pprof writes it.
func testPprofProto(body []byte) ([]byte, bool) {
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	decoded, err := io.ReadAll(reader)
	return decoded, err == nil && 0 < len(decoded)
}

// The port gauge's value in the registry, or 0 without one.
func testPprofPort(t *testing.T, gatherer prometheus.Gatherer) int {
	t.Helper()
	families, err := gatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "urnetwork_connect_pprof_port" {
			if len(family.Metric) != 1 || 0 < len(family.Metric[0].Label) {
				t.Fatalf("the port gauge has series %v, want one unlabeled", family.Metric)
			}
			return int(family.Metric[0].GetGauge().GetValue())
		}
	}
	return 0
}

// Whether a connection to address is accepted.
func testPprofAccepts(network string, address string) bool {
	conn, err := net.DialTimeout(network, address, 5*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// The listener binds 127.0.0.1 on a port the kernel picks, and the port
// answers on no other address of the host.
func TestPprofListenerBindsTheLoopbackOnly(t *testing.T) {
	listener, _ := testPprofListener(t, defaultPprofListenerSettings())
	if !listener.address.IP.Equal(net.IPv4(127, 0, 0, 1)) || listener.address.Port == 0 {
		t.Fatalf("bound %s, want 127.0.0.1 on a picked port", listener.address)
	}
	port := strconv.Itoa(listener.address.Port)
	if !testPprofAccepts("tcp4", net.JoinHostPort("127.0.0.1", port)) {
		t.Fatal("the loopback port refuses connections")
	}
	if testPprofAccepts("tcp6", net.JoinHostPort("::1", port)) {
		t.Error("the port answers on ::1")
	}
	interfaceAddresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, interfaceAddress := range interfaceAddresses {
		ipNet, ok := interfaceAddress.(*net.IPNet)
		// a link-local address needs a zone to dial
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() {
			continue
		}
		if testPprofAccepts("tcp", net.JoinHostPort(ipNet.IP.String(), port)) {
			t.Errorf("the port answers on the host's address %s", ipNet.IP)
		}
	}
}

// The CPU profile and the four snapshots answer with their profiles, and a
// snapshot with debug=1 answers in text.
func TestPprofListenerServesTheChosenProfiles(t *testing.T) {
	settings := defaultPprofListenerSettings()
	settings.cpuProfileWaitForTest = func(context.Context, time.Duration) time.Duration {
		// a real profile, stopped at once
		return 0
	}
	listener, _ := testPprofListener(t, settings)
	for _, c := range []struct {
		path       string
		sampleType string
	}{
		{path: "/debug/pprof/profile", sampleType: "cpu"},
		{path: "/debug/pprof/profile?seconds=1", sampleType: "cpu"},
		{path: "/debug/pprof/heap", sampleType: "inuse_space"},
		{path: "/debug/pprof/heap?debug=0", sampleType: "inuse_space"},
		{path: "/debug/pprof/allocs", sampleType: "alloc_space"},
		{path: "/debug/pprof/mutex", sampleType: "contentions"},
		{path: "/debug/pprof/block", sampleType: "contentions"},
	} {
		status, header, body := testPprofRequest(http.MethodGet, listener.address.Port, c.path)
		if status != http.StatusOK || header.Get("Content-Type") != "application/octet-stream" {
			t.Errorf("GET %s answered %d %q, want 200 with a profile", c.path, status, header.Get("Content-Type"))
			continue
		}
		decoded, ok := testPprofProto(body)
		if !ok || !bytes.Contains(decoded, []byte(c.sampleType)) {
			t.Errorf("GET %s answered no profile of %s", c.path, c.sampleType)
		}
	}
	for _, path := range []string{"/debug/pprof/heap?debug=1", "/debug/pprof/allocs?debug=1"} {
		status, body := testPprofGet(listener, path)
		if status != http.StatusOK || !bytes.HasPrefix(body, []byte("heap profile:")) {
			t.Errorf("GET %s answered %d, want 200 with a text heap profile", path, status)
		}
	}
}

// goroutine, trace, cmdline, symbol, threadcreate and the index are 404, as
// is every other path; a served path answers only GET.
func TestPprofListenerServesNothingElse(t *testing.T) {
	listener, _ := testPprofListener(t, defaultPprofListenerSettings())
	for _, path := range []string{
		"/debug/pprof/goroutine",
		"/debug/pprof/goroutine?debug=2",
		"/debug/pprof/trace",
		"/debug/pprof/trace?seconds=1",
		"/debug/pprof/cmdline",
		"/debug/pprof/symbol",
		"/debug/pprof/threadcreate",
		"/debug/pprof/",
		"/debug/pprof",
		"/debug/pprof/heap/",
		"/debug/pprof/HEAP",
		"/debug/pprof/x/../goroutine",
		"/",
		"/metrics",
	} {
		if status, _ := testPprofGet(listener, path); status != http.StatusNotFound {
			t.Errorf("GET %s answered %d, want 404", path, status)
		}
	}
	for _, method := range []string{http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete} {
		for _, path := range []string{"/debug/pprof/profile", "/debug/pprof/heap"} {
			if status, _, _ := testPprofRequest(method, listener.address.Port, path); status != http.StatusMethodNotAllowed {
				t.Errorf("%s %s answered %d, want 405", method, path, status)
			}
		}
		if status, _, _ := testPprofRequest(method, listener.address.Port, "/debug/pprof/goroutine"); status != http.StatusNotFound {
			t.Errorf("%s /debug/pprof/goroutine answered %d, want 404", method, status)
		}
	}
}

// A snapshot takes only debug, 0 or 1: no delta (seconds), no forced GC.
func TestPprofListenerRefusesUnservedSnapshotParameters(t *testing.T) {
	listener, _ := testPprofListener(t, defaultPprofListenerSettings())
	for _, path := range []string{
		"/debug/pprof/heap?seconds=30",
		"/debug/pprof/allocs?seconds=30",
		"/debug/pprof/mutex?seconds=30",
		"/debug/pprof/heap?gc=1",
		"/debug/pprof/heap?debug=2",
		"/debug/pprof/heap?debug=",
		"/debug/pprof/heap?debug=1&debug=1",
		"/debug/pprof/block?debug=1&seconds=30",
	} {
		if status, _ := testPprofGet(listener, path); status != http.StatusBadRequest {
			t.Errorf("GET %s answered %d, want 400", path, status)
		}
	}
}

// A CPU profile lasts its seconds, the default without them, and never more
// than the cap; a malformed seconds is refused before any profile starts.
func TestPprofListenerCapsCpuProfileSeconds(t *testing.T) {
	settings := defaultPprofListenerSettings()
	waits := make(chan time.Duration, 1)
	settings.cpuProfileWaitForTest = func(_ context.Context, duration time.Duration) time.Duration {
		waits <- duration
		return 0
	}
	listener, _ := testPprofListener(t, settings)
	for _, c := range []struct {
		query    string
		duration time.Duration
	}{
		{query: "", duration: 30 * time.Second},
		{query: "?seconds=1", duration: 1 * time.Second},
		{query: "?seconds=30", duration: 30 * time.Second},
		{query: "?seconds=60", duration: 60 * time.Second},
		{query: "?seconds=61", duration: 60 * time.Second},
		{query: "?seconds=3600", duration: 60 * time.Second},
		{query: "?seconds=9223372036854775807", duration: 60 * time.Second},
	} {
		if status, _ := testPprofGet(listener, "/debug/pprof/profile"+c.query); status != http.StatusOK {
			t.Errorf("GET /debug/pprof/profile%s answered %d, want 200", c.query, status)
			continue
		}
		select {
		case duration := <-waits:
			if duration != c.duration {
				t.Errorf("GET /debug/pprof/profile%s profiled %s, want %s", c.query, duration, c.duration)
			}
		default:
			t.Errorf("GET /debug/pprof/profile%s answered without profiling", c.query)
		}
	}
	for _, query := range []string{
		"?seconds=0",
		"?seconds=-1",
		"?seconds=1.5",
		"?seconds=30s",
		"?seconds=",
		"?seconds=9223372036854775808",
		"?seconds=30&seconds=30",
		"?debug=1",
		"?gc=1",
	} {
		if status, _ := testPprofGet(listener, "/debug/pprof/profile"+query); status != http.StatusBadRequest {
			t.Errorf("GET /debug/pprof/profile%s answered %d, want 400", query, status)
		}
		select {
		case duration := <-waits:
			t.Errorf("GET /debug/pprof/profile%s profiled %s", query, duration)
		default:
		}
	}

	// the cap is the setting's, a fraction of a second included
	capped := defaultPprofListenerSettings()
	capped.MaxCpuProfileDuration = 1500 * time.Millisecond
	for seconds, duration := range map[int64]time.Duration{
		1: 1 * time.Second,
		2: 1500 * time.Millisecond,
	} {
		if got := capped.cpuProfileDuration(seconds); got != duration {
			t.Errorf("%d seconds under a 1.5 s cap profiles %s, want %s", seconds, got, duration)
		}
	}
}

// The port gauge carries the bound port, unlabeled, while the listener
// serves, and is withdrawn with the port when it closes.
func TestPprofListenerPublishesItsPortUntilClosed(t *testing.T) {
	registry := prometheus.NewRegistry()
	listener, err := newPprofListener(defaultPprofListenerSettings(), registry)
	if err != nil {
		t.Fatal(err)
	}
	port := listener.address.Port
	if published := testPprofPort(t, registry); published != port {
		t.Fatalf("the gauge carries %d, the listener bound %d", published, port)
	}
	// a second listener can't publish beside the first
	if second, err := newPprofListener(defaultPprofListenerSettings(), registry); err == nil {
		second.Close()
		t.Fatal("a second listener published its port in the same registry")
	}

	listener.Close()
	listener.Close()
	if published := testPprofPort(t, registry); published != 0 {
		t.Fatalf("the gauge carries %d after the listener closed", published)
	}
	if testPprofAccepts("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))) {
		t.Fatal("the port accepts after the listener closed")
	}
	var absent *pprofListener
	absent.Close()
}

// The port gauge pushes under the stats push's grouping
// (server.StatsPushReservedLabelNames), as connect pushes it.
func TestPprofListenerPortPushesUnderStatsPushGrouping(t *testing.T) {
	_, registry := testPprofListener(t, defaultPprofListenerSettings())
	pushServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer pushServer.Close()

	// the job is the push client's own label; every other reserved name groups
	pusher := push.New(pushServer.URL, "connect").Gatherer(registry)
	for _, labelName := range server.StatsPushReservedLabelNames() {
		if labelName != "job" {
			pusher = pusher.Grouping(labelName, "test")
		}
	}
	if err := pusher.Push(); err != nil {
		t.Fatal(err)
	}
}

// CONNECT_PPROF unset or true serves; 0, false, or any other value binds and
// publishes nothing.
func TestPprofListenerKillSwitch(t *testing.T) {
	for value, enabled := range map[string]bool{
		"":      true,
		"1":     true,
		"true":  true,
		" 1 ":   true,
		"0":     false,
		"false": false,
		"off":   false,
		"no":    false,
		"2":     false,
	} {
		if pprofListenerEnabled(value) != enabled {
			t.Errorf("CONNECT_PPROF=%q enabled %t, want %t", value, !enabled, enabled)
		}
	}

	t.Setenv(pprofListenerEnvName, "0")
	registry := prometheus.NewRegistry()
	listener, err := newPprofListenerFromEnv(defaultPprofListenerSettings(), registry)
	if listener != nil || err != nil {
		listener.Close()
		t.Fatalf("CONNECT_PPROF=0 started %v, %v", listener, err)
	}
	if published := testPprofPort(t, registry); published != 0 {
		t.Fatalf("CONNECT_PPROF=0 published port %d", published)
	}

	t.Setenv(pprofListenerEnvName, "")
	listener, err = newPprofListenerFromEnv(defaultPprofListenerSettings(), registry)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if published := testPprofPort(t, registry); published == 0 || published != listener.address.Port {
		t.Fatalf("unset CONNECT_PPROF published port %d", published)
	}
}

// Past MaxConcurrentRequestCount a request is refused at once, and served
// again once a place is free.
func TestPprofListenerRefusesRequestsBeyondItsCapacity(t *testing.T) {
	settings := defaultPprofListenerSettings()
	settings.MaxConcurrentRequestCount = 1
	started := make(chan struct{})
	release := make(chan struct{})
	settings.cpuProfileWaitForTest = func(context.Context, time.Duration) time.Duration {
		close(started)
		<-release
		return 0
	}
	listener, _ := testPprofListener(t, settings)

	cpuStatuses := make(chan int, 1)
	go func() {
		status, _ := testPprofGet(listener, "/debug/pprof/profile")
		cpuStatuses <- status
	}()
	<-started
	if status, _ := testPprofGet(listener, "/debug/pprof/heap"); status != http.StatusServiceUnavailable {
		t.Errorf("a snapshot during the one place's CPU profile answered %d, want 503", status)
	}
	close(release)
	if status := <-cpuStatuses; status != http.StatusOK {
		t.Errorf("the CPU profile answered %d, want 200", status)
	}
	if status, _ := testPprofGet(listener, "/debug/pprof/heap"); status != http.StatusOK {
		t.Errorf("a snapshot after the CPU profile answered %d, want 200", status)
	}
}

// The process runs one CPU profile at a time: a second is refused with 409
// while a snapshot is still served.
func TestPprofListenerRefusesASecondCpuProfile(t *testing.T) {
	settings := defaultPprofListenerSettings()
	settings.MaxConcurrentRequestCount = 3
	started := make(chan struct{})
	release := make(chan struct{})
	settings.cpuProfileWaitForTest = func(context.Context, time.Duration) time.Duration {
		close(started)
		<-release
		return 0
	}
	listener, _ := testPprofListener(t, settings)

	cpuStatuses := make(chan int, 1)
	go func() {
		status, _ := testPprofGet(listener, "/debug/pprof/profile")
		cpuStatuses <- status
	}()
	<-started
	if status, _ := testPprofGet(listener, "/debug/pprof/profile"); status != http.StatusConflict {
		t.Errorf("a second CPU profile answered %d, want 409", status)
	}
	if status, _ := testPprofGet(listener, "/debug/pprof/allocs"); status != http.StatusOK {
		t.Errorf("a snapshot during the CPU profile answered %d, want 200", status)
	}
	close(release)
	if status := <-cpuStatuses; status != http.StatusOK {
		t.Errorf("the first CPU profile answered %d, want 200", status)
	}
}

// Close ends a CPU profile in flight at once rather than waiting out its
// seconds, waits for it, and leaves the process's profiler free.
func TestPprofListenerCloseEndsACpuProfileInFlight(t *testing.T) {
	settings := defaultPprofListenerSettings()
	started := make(chan time.Duration, 1)
	settings.cpuProfileWaitForTest = func(_ context.Context, duration time.Duration) time.Duration {
		started <- duration
		// the real wait, which only the listener's close can end
		return duration
	}
	listener, err := newPprofListener(settings, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}

	cpuDone := make(chan struct{})
	go func() {
		defer close(cpuDone)
		testPprofGet(listener, "/debug/pprof/profile?seconds=60")
	}()
	if duration := <-started; duration != 60*time.Second {
		t.Fatalf("the CPU profile waits %s, want 60s", duration)
	}
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		listener.Close()
	}()
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		// a hang guard: Close cancels the wait rather than outlasting it
		t.Fatal("Close waited out the CPU profile")
	}
	<-cpuDone
	if err := pprof.StartCPUProfile(io.Discard); err != nil {
		t.Fatalf("the profiler is still busy after Close: %s", err)
	}
	pprof.StopCPUProfile()
}

// A CPU profile whose wait panics, which net/http recovers, still stops the
// process's profiler and gives back its place, so the next one is served.
func TestPprofListenerStopsTheProfilerWhenAProfilePanics(t *testing.T) {
	settings := defaultPprofListenerSettings()
	settings.MaxConcurrentRequestCount = 1
	var waitCount atomic.Int32
	settings.cpuProfileWaitForTest = func(context.Context, time.Duration) time.Duration {
		if waitCount.Add(1) == 1 {
			panic("the test's panic during a CPU profile")
		}
		return 0
	}
	listener, _ := testPprofListener(t, settings)

	if status, _ := testPprofGet(listener, "/debug/pprof/profile"); status == http.StatusOK {
		t.Fatal("the panicking CPU profile answered 200")
	}
	if status, _ := testPprofGet(listener, "/debug/pprof/profile"); status != http.StatusOK {
		t.Fatalf("the CPU profile after a panic answered %d, want 200", status)
	}
	if waitCount.Load() != 2 {
		t.Fatalf("%d CPU profiles waited, want 2", waitCount.Load())
	}
}

// Settings that would leave a wait unbounded or serve nothing are refused,
// before anything binds.
func TestPprofListenerRefusesInvalidSettings(t *testing.T) {
	for _, change := range []func(*pprofListenerSettings){
		func(settings *pprofListenerSettings) { settings.DefaultCpuProfileDuration = 0 },
		func(settings *pprofListenerSettings) { settings.MaxCpuProfileDuration = -time.Second },
		func(settings *pprofListenerSettings) { settings.MaxConcurrentRequestCount = 0 },
		func(settings *pprofListenerSettings) { settings.ReadHeaderTimeout = 0 },
		func(settings *pprofListenerSettings) { settings.WriteTimeoutMargin = 0 },
		func(settings *pprofListenerSettings) { settings.IdleTimeout = 0 },
	} {
		settings := defaultPprofListenerSettings()
		change(settings)
		registry := prometheus.NewRegistry()
		if listener, err := newPprofListener(settings, registry); err == nil {
			listener.Close()
			t.Errorf("settings %+v started a listener", settings)
		}
		if published := testPprofPort(t, registry); published != 0 {
			t.Errorf("refused settings published port %d", published)
		}
	}
}

// connect's start serves the listener from before readiness to after its
// last listener returns, publishing its port in the default registry the
// stats push gathers. A start that is not ready runs without a database.
func TestRunServesLoopbackPprofUntilItReturns(t *testing.T) {
	setTestFrontRunEnv(t)
	t.Setenv(pprofListenerEnvName, "")
	port := 0
	err := runWithDependencies(
		t.Context(),
		RunOptions{Port: 8081, WarpStatus: &router.WarpStatusState{}, PrivateHeapProfileTarget: "disabled", LoopbackPprof: true},
		func(context.Context) error {
			port = testPprofPort(t, prometheus.DefaultGatherer)
			return fmt.Errorf("the test's readiness failure")
		},
		func(context.Context) func() {
			t.Error("a start that is not ready published a metrics cohort")
			return func() {}
		},
		func(context.Context, string, http.Handler, bool, server.HttpServerOptions) error {
			if published := testPprofPort(t, prometheus.DefaultGatherer); published == 0 || published != port {
				t.Errorf("the listener's port %d while serving, %d at readiness", published, port)
			}
			status, _, body := testPprofRequest(http.MethodGet, port, "/debug/pprof/heap")
			if _, ok := testPprofProto(body); status != http.StatusOK || !ok {
				t.Errorf("the heap profile answered %d while serving", status)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if port == 0 {
		t.Fatal("no pprof port was published before readiness")
	}
	if published := testPprofPort(t, prometheus.DefaultGatherer); published != 0 {
		t.Fatalf("the pprof port %d is still published after the start returned", published)
	}
	if testPprofAccepts("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))) {
		t.Fatal("the pprof port accepts after the start returned")
	}
}

// Without RunOptions.LoopbackPprof, or with the kill switch, connect's start
// binds no pprof listener and still serves.
func TestRunWithoutLoopbackPprofServesNone(t *testing.T) {
	setTestFrontRunEnv(t)
	for _, c := range []struct {
		loopbackPprof bool
		killSwitch    string
	}{
		{loopbackPprof: false, killSwitch: ""},
		{loopbackPprof: true, killSwitch: "0"},
	} {
		t.Setenv(pprofListenerEnvName, c.killSwitch)
		served := false
		err := runWithDependencies(
			t.Context(),
			RunOptions{Port: 8081, WarpStatus: &router.WarpStatusState{}, PrivateHeapProfileTarget: "disabled", LoopbackPprof: c.loopbackPprof},
			func(context.Context) error {
				return fmt.Errorf("the test's readiness failure")
			},
			func(context.Context) func() {
				return func() {}
			},
			func(context.Context, string, http.Handler, bool, server.HttpServerOptions) error {
				served = true
				if published := testPprofPort(t, prometheus.DefaultGatherer); published != 0 {
					t.Errorf("LoopbackPprof %t with CONNECT_PPROF=%q published port %d", c.loopbackPprof, c.killSwitch, published)
				}
				return nil
			},
		)
		if err != nil || !served {
			t.Fatalf("LoopbackPprof %t with CONNECT_PPROF=%q: served %t, %v", c.loopbackPprof, c.killSwitch, served, err)
		}
	}
}
