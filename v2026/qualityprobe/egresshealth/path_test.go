package egresshealth

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests of a run over a Path whose tunnel dies: re-creation, single-flight
// re-opening, the new tunnel's warm-up, the re-creation cap and loads left
// not measured.

// Stands in for providertunnel's loss cause.
var errTunnelGone = errors.New("the tunnel's path to the provider is gone")

// A Path over plain http clients, standing in for a provider
// tunnel the test can kill. Each generation's client tags its requests with
// X-Tunnel: <generation> and refuses to send anything once that generation is
// lost, which is what a dead tunnel does; Reopen starts a new generation,
// unless the test has said the provider will not come back.
type fakePath struct {
	stateLock  sync.Mutex
	generation int
	signal     context.Context
	lose       context.CancelCauseFunc
	reopens    atomic.Int32
	failReopen atomic.Bool
}

// Returns a path on its first generation.
func newFakePath() *fakePath {
	p := &fakePath{}
	p.nextWithLock()
	return p
}

// Starts a new generation. Called with stateLock held, or before the path is
// shared.
func (self *fakePath) nextWithLock() {
	self.generation++
	self.signal, self.lose = context.WithCancelCause(context.Background())
}

// Implements Path: a client of the current generation and its loss signal.
func (self *fakePath) Current() (*http.Client, context.Context) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return &http.Client{Transport: generationTransport{generation: self.generation, signal: self.signal}}, self.signal
}

// Implements Path: starts a new generation, unless the provider will not come
// back.
func (self *fakePath) Reopen(ctx context.Context) error {
	self.reopens.Add(1)
	if self.failReopen.Load() {
		return errors.New("the provider is not taking contracts")
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.nextWithLock()
	return nil
}

// Loses the current generation's tunnel.
func (self *fakePath) kill() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lose(errTunnelGone)
}

// The transport of one generation's client: it tags every request with its
// generation and refuses to send once that generation is lost.
type generationTransport struct {
	generation int
	signal     context.Context
}

// Implements http.RoundTripper.
func (self generationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if self.signal.Err() != nil {
		return nil, fmt.Errorf("dial through a dead tunnel: %w", context.Cause(self.signal))
	}
	r = r.Clone(r.Context())
	r.Header.Set("X-Tunnel", strconv.Itoa(self.generation))
	return http.DefaultTransport.RoundTrip(r)
}

// Answers every path OK and records which tunnel generation each
// request came through; the second request it sees kills its tunnel, which is
// "a tunnel that dies after the first load".
type tunnelServer struct {
	stateLock sync.Mutex
	byPath    map[string][]string
	order     []string
	requests  atomic.Int32
	*httptest.Server
}

// Starts a tunnel server that kills path's tunnel under the second load it
// sees, closed when the test ends.
func newTunnelServer(t *testing.T, path *fakePath) *tunnelServer {
	t.Helper()
	s := &tunnelServer{byPath: map[string][]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.stateLock.Lock()
		s.byPath[r.URL.Path] = append(s.byPath[r.URL.Path], r.Header.Get("X-Tunnel"))
		s.order = append(s.order, r.URL.Path+"@"+r.Header.Get("X-Tunnel"))
		s.stateLock.Unlock()
		if r.URL.Path == IpEchoPath {
			_, _ = w.Write([]byte(`{"info":{"ip":"203.0.113.7"}}`))
			return
		}
		if s.requests.Add(1) == 2 {
			path.kill()
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(s.Close)
	return s
}

// Returns the tunnel generations the requests for path came through, in order.
func (self *tunnelServer) tunnels(path string) []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.byPath[path]...)
}

// Like fastOptions, for a run over path: a per-request timeout long
// enough that only the tunnel dying can end a hanging attempt, and the derived
// budget, since fastOptions' short explicit one would leave no room for the
// retries these tests are about.
func pathOptions(path Path, concurrency int) Options {
	opts := fastOptions()
	opts.Budget = 0
	opts.Concurrency = concurrency
	opts.PerRequestTimeout = 5 * time.Second
	opts.Path = path
	return opts
}

// Returns three site destinations on the server at url.
func threeSites(url string) []Destination {
	return []Destination{
		{Name: "site-a", Class: ClassSite, Url: url + "/a"},
		{Name: "site-b", Class: ClassSite, Url: url + "/b"},
		{Name: "site-c", Class: ClassSite, Url: url + "/c"},
	}
}

// The tunnel dies after the
// first load. The run does not fail what it had not reached: the next attempt
// of every pending load re-opens the tunnel -- once, for all of them -- and
// the remaining loads complete on the new one, with nothing left unmeasured.
func TestTunnelThatDiesIsRecreatedAndTheRunCompletes(t *testing.T) {
	path := newFakePath()
	srv := newTunnelServer(t, path)
	opts := pathOptions(path, 1)

	res, err := check(context.Background(), nil, threeSites(srv.URL), opts)
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	if res.OkCount != 3 || res.Total != 3 || res.NotMeasured != 0 {
		t.Fatalf("ok/total = %d/%d not_measured = %d (%s), want 3/3: every load completes on the re-created tunnel", res.OkCount, res.Total, res.NotMeasured, res.Summary())
	}
	if got := path.reopens.Load(); got != 1 {
		t.Errorf("the tunnel was re-opened %d time(s), want once for every load that needed it", got)
	}
	onFirst, onSecond := 0, 0
	for _, name := range []string{"/a", "/b", "/c"} {
		tunnels := srv.tunnels(name)
		switch last := tunnels[len(tunnels)-1]; last {
		case "1":
			onFirst++
		case "2":
			onSecond++
		default:
			t.Errorf("%s last went through tunnel %q", name, last)
		}
	}
	if onFirst != 1 || onSecond != 2 {
		t.Errorf("%d load(s) finished on the first tunnel and %d on the re-created one, want 1 and 2", onFirst, onSecond)
	}
	for _, c := range res.Checks {
		if 2 < c.Attempts {
			t.Errorf("%s took %d attempts; losing the tunnel should cost at most the attempt it was lost under", c.Name, c.Attempts)
		}
	}
}

// When the provider
// does not come back, the loads it took with it are not measured -- never
// failed -- and the run still carries what it did measure.
func TestTunnelThatCannotBeReopenedLeavesTheRestNotMeasured(t *testing.T) {
	path := newFakePath()
	path.failReopen.Store(true)
	srv := newTunnelServer(t, path)
	opts := pathOptions(path, 1)

	res, err := check(context.Background(), nil, threeSites(srv.URL), opts)
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	if res.OkCount != 1 || res.Total != 1 {
		t.Fatalf("ok/total = %d/%d, want the first load's 1/1 carried", res.OkCount, res.Total)
	}
	if res.NotMeasured != 2 || len(res.NotMeasuredNames()) != 2 {
		t.Fatalf("not_measured = %d (%v), want the two loads the tunnel took with it", res.NotMeasured, res.NotMeasuredNames())
	}
	if names := res.FailedNames(); len(names) != 0 {
		t.Errorf("FailedNames = %v; a load whose tunnel could not be re-created is not a failed site", names)
	}
	for _, c := range res.Checks {
		if c.NotMeasured && c.Attempts != DefaultLoadAttempts {
			t.Errorf("%s: %d attempt(s), want every one tried before it counts as not measured", c.Name, c.Attempts)
		}
	}
	if got := res.Summary(); got != "ok=1/1 site=1/1 not_measured=2" {
		t.Errorf("Summary = %q", got)
	}
}

// A re-created tunnel is as cold
// as the first, so it gets the warm-up before any load uses it -- the cold
// start is the warm-up's to pay, not a scored load's.
func TestReopenedTunnelIsWarmedUpBeforeItsLoads(t *testing.T) {
	path := newFakePath()
	srv := newTunnelServer(t, path)
	opts := pathOptions(path, 1)
	opts.IpEchoUrl = srv.URL + IpEchoPath

	res, err := check(context.Background(), nil, threeSites(srv.URL), opts)
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	if res.ExitIp != "203.0.113.7" {
		t.Errorf("ExitIp = %q", res.ExitIp)
	}
	srv.stateLock.Lock()
	order := append([]string(nil), srv.order...)
	srv.stateLock.Unlock()
	firstOnSecond := -1
	for i, entry := range order {
		if entry[len(entry)-1] == '2' {
			firstOnSecond = i
			break
		}
	}
	if firstOnSecond < 0 || order[firstOnSecond] != IpEchoPath+"@2" {
		t.Fatalf("requests arrived as %v; the first through the re-created tunnel must be its warm-up", order)
	}
}

// Every pending load notices a dead tunnel at once;
// they must share one re-open -- one new device, one contract -- rather than
// open a tunnel each.
func TestReopenIsSingleFlight(t *testing.T) {
	path := newFakePath()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Tunnel") == "1" {
			if requests.Add(1) == 10 {
				path.kill()
			}
			<-r.Context().Done()
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	var dests []Destination
	for i := 0; i < 10; i++ {
		dests = append(dests, Destination{Name: fmt.Sprintf("site-%d", i), Class: ClassSite, Url: fmt.Sprintf("%s/%d", srv.URL, i)})
	}
	opts := pathOptions(path, 10)
	res, err := check(context.Background(), nil, dests, opts)
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	if res.OkCount != 10 {
		t.Fatalf("Summary = %s, want every load passed on the re-created tunnel", res.Summary())
	}
	if got := path.reopens.Load(); got != 1 {
		t.Fatalf("ten loads losing one tunnel re-opened it %d times, want once", got)
	}
}

// A load whose last attempt was
// cut off by the tunnel dying under it had no tunnel to finish on, and is not
// measured -- a load is only failed when its last attempt failed on a tunnel
// that was there.
func TestLastAttemptOnADyingTunnelIsNotMeasured(t *testing.T) {
	path := newFakePath()
	path.failReopen.Store(true)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) < int32(DefaultLoadAttempts) {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		path.kill()
		<-r.Context().Done()
	}))
	defer srv.Close()

	opts := pathOptions(path, 3)
	res, err := check(context.Background(), nil, []Destination{{Name: "site-a", Class: ClassSite, Url: srv.URL}}, opts)
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	c := res.Checks[0]
	if !c.NotMeasured || c.Ok || c.Attempts != DefaultLoadAttempts {
		t.Fatalf("result = %+v, want not measured after its last attempt lost the tunnel", c)
	}
	if res.Total != 0 || res.NotMeasured != 1 {
		t.Errorf("ok/total = %d/%d not_measured = %d", res.OkCount, res.Total, res.NotMeasured)
	}
}

// A blackhole check whose
// tunnel is gone and cannot be re-created measured nothing. It is
// FailureNotMeasured -- rescheduled by the server, counted against no one --
// never all_destinations_failed, the first step towards dark.
func TestCheckWhoseTunnelNeverComesBackIsNotMeasured(t *testing.T) {
	path := newFakePath()
	path.failReopen.Store(true)
	path.kill()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	res := blackhole(context.Background(), nil, stubDests(srv.URL, 3), Options{
		Rand:      rand.New(rand.NewSource(1)),
		Sleep:     noSleep,
		Path:      path,
		IpEchoUrl: srv.URL + IpEchoPath,
	})
	if res.Ok || res.Failure != FailureNotMeasured {
		t.Fatalf("OK = %t Failure = %q, want %q: a check whose tunnel never came back is not measured, not dark", res.Ok, res.Failure, FailureNotMeasured)
	}
	if res.NotMeasured != BlackholeSampleSize {
		t.Errorf("NotMeasured = %d, want %d", res.NotMeasured, BlackholeSampleSize)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("%d request(s) reached a destination through a tunnel that was gone", got)
	}
	if got := path.reopens.Load(); got < 1 || int32(DefaultTunnelRecreateAttempts) < got {
		t.Errorf("re-open was tried %d time(s), want at least once and at most the run's %d", got, DefaultTunnelRecreateAttempts)
	}
}

// Every re-creation is a new proxy device
// and a new contract, so a run re-creates its tunnel at most
// TunnelRecreateAttempts times. A provider whose every tunnel dies at once
// gets two; the third is never asked for, and the load is not measured.
func TestThirdRecreationIsNotAttempted(t *testing.T) {
	path := newFakePath()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// every tunnel dies under its first request
		path.kill()
		<-r.Context().Done()
	}))
	defer srv.Close()

	opts := pathOptions(path, 1)
	opts.LoadAttempts = 5
	res, err := check(context.Background(), nil, []Destination{{Name: "site-a", Class: ClassSite, Url: srv.URL}}, opts)
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	if got := path.reopens.Load(); got != int32(DefaultTunnelRecreateAttempts) {
		t.Fatalf("the tunnel was re-created %d time(s), want exactly %d: the third must not be attempted", got, DefaultTunnelRecreateAttempts)
	}
	c := res.Checks[0]
	if !c.NotMeasured || c.Ok || c.Attempts != 5 {
		t.Fatalf("result = %+v, want not measured after all five attempts", c)
	}
	if c.Err != "no tunnel: "+errRecreateLimit.Error() {
		t.Errorf("Err = %q, want the re-creation limit named", c.Err)
	}

	// The cap is a setting.
	path = newFakePath()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path.kill()
		<-r.Context().Done()
	}))
	defer srv2.Close()
	opts = pathOptions(path, 1)
	opts.LoadAttempts = 5
	opts.TunnelRecreateAttempts = 3
	if _, err := check(context.Background(), nil, []Destination{{Name: "site-a", Class: ClassSite, Url: srv2.URL}}, opts); err != nil {
		t.Fatalf("check err = %v", err)
	}
	if got := path.reopens.Load(); got != 3 {
		t.Errorf("with TunnelRecreateAttempts 3 the tunnel was re-created %d time(s)", got)
	}
}

// The tunnel under a blackhole check
// dies and comes back; the check's loads carry on through the new one, and a
// load that passes there makes the check OK.
func TestCheckCarriedAcrossAReconnectIsOk(t *testing.T) {
	path := newFakePath()
	path.kill()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	res := blackhole(context.Background(), nil, stubDests(srv.URL, 3), Options{
		Rand:  rand.New(rand.NewSource(1)),
		Sleep: noSleep,
		Path:  path,
	})
	if !res.Ok || res.Failure != "" || res.NotMeasured != 0 {
		t.Fatalf("OK = %t Failure = %q not_measured = %d, want OK on the re-created tunnel", res.Ok, res.Failure, res.NotMeasured)
	}
	if got := path.reopens.Load(); got != 1 {
		t.Errorf("re-opened %d time(s), want once", got)
	}
}
