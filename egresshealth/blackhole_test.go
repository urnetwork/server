package egresshealth

import (
	"context"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests of the blackhole check: its connectivity-only sample, its any-pass rule,
// the TLS-integrity override, its warm-up and its draw from a pool.

// Builds a connectivity table pointed at one server, plus a
// non-connectivity entry that must never be drawn.
func stubDests(url string, n int) []Destination {
	dests := []Destination{{
		Name: "not-connectivity", Class: ClassCdn, Url: url + "/cdn",
		Expect: ExpectStatus, Status: http.StatusNoContent,
	}}
	for i := 0; i < n; i++ {
		dests = append(dests, Destination{
			Name:  "conn-" + string(rune('a'+i)),
			Class: ClassConnectivity,
			Url:   url + "/conn",
			// the real connectivity entries are 204 probes
			Expect: ExpectStatus, Status: http.StatusNoContent,
		})
	}
	return dests
}

// A provider that carries traffic passes, but the whole small sample is still
// checked so an unrelated first success cannot hide a later TLS integrity
// failure.
func TestBlackholePassesAfterCheckingIntegrityOfWholeSample(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	res := blackhole(context.Background(), srv.Client(), stubDests(srv.URL, 3),
		Options{Rand: rand.New(rand.NewSource(1)), Sleep: noSleep})

	if !res.Ok {
		t.Fatalf("OK = false, want true: every destination answered correctly")
	}
	if res.Failure != "" {
		t.Errorf("Failure = %q, want empty on success", res.Failure)
	}
	if got := requests.Load(); got != BlackholeSampleSize {
		t.Errorf("made %d requests, want %d: every sampled TLS identity must be checked", got, BlackholeSampleSize)
	}
}

// Checking the whole integrity sample must not multiply the per-provider
// deadline by three. All three requests share one tunnel and can run together;
// this barrier proves they are admitted before any one is allowed to finish.
func TestBlackholeChecksIntegritySampleConcurrently(t *testing.T) {
	entered := make(chan struct{}, BlackholeSampleSize)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	done := make(chan *BlackholeResult, 1)
	go func() {
		done <- blackhole(context.Background(), srv.Client(), stubDests(srv.URL, BlackholeSampleSize),
			Options{Rand: rand.New(rand.NewSource(1)), PerRequestTimeout: 2 * time.Second, Sleep: noSleep})
	}()
	for i := 0; i < BlackholeSampleSize; i++ {
		select {
		case <-entered:
		case <-time.After(500 * time.Millisecond):
			close(release)
			t.Fatalf("only %d/%d checks entered before completion; the sample is running sequentially", i, BlackholeSampleSize)
		}
	}
	close(release)
	if res := <-done; !res.Ok {
		t.Fatalf("concurrent healthy sample failed: %+v", res)
	}
}

// A blackhole fails only when every drawn destination fails -- every one of
// its tries.
func TestBlackholeFailsOnlyWhenAllFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// a captive-portal shaped answer: 200 with a body where 204 was required
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("hijacked"))
	}))
	defer srv.Close()

	res := blackhole(context.Background(), srv.Client(), stubDests(srv.URL, 3),
		Options{Rand: rand.New(rand.NewSource(1)), Sleep: noSleep})

	if res.Ok {
		t.Fatalf("OK = true, want false: no destination met its contract")
	}
	if res.Failure != FailureAllDestinationsFailed {
		t.Errorf("Failure = %q, want %q", res.Failure, FailureAllDestinationsFailed)
	}
	if len(res.Results) != BlackholeSampleSize {
		t.Errorf("tried %d destinations, want the full sample of %d before declaring a blackhole",
			len(res.Results), BlackholeSampleSize)
	}
	for _, r := range res.Results {
		if r.Attempts != DefaultLoadAttempts {
			t.Errorf("%s: %d attempt(s), want every one of %d before it counts", r.Name, r.Attempts, DefaultLoadAttempts)
		}
	}
}

// One reachable destination among failures is not a blackhole. A provider
// reaching some destinations is degraded, which is Check's department -- calling
// it dark here would remove working providers on a signal that cannot tell the
// two apart.
func TestBlackholePartialReachabilityIsNotABlackhole(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	res := blackhole(context.Background(), srv.Client(), stubDests(srv.URL, 3),
		Options{Rand: rand.New(rand.NewSource(1)), Sleep: noSleep})

	if !res.Ok {
		t.Errorf("OK = false, want true: the third destination answered, so traffic is getting through")
	}
}

// A successful first destination must not hide a forged TLS certificate on a
// later canary. This is the exact production failure: the old any-success loop
// returned immediately, so a TLS-intercepting provider could be recorded OK
// before the sampled gstatic destination was ever attempted.
func TestBlackholeTlsAuthenticationFailureOverridesSuccess(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer good.Close()
	intercepted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer intercepted.Close()

	dests := []Destination{
		{Name: "good-a", Class: ClassConnectivity, Url: good.URL, Expect: ExpectStatus, Status: http.StatusNoContent},
		{Name: "intercepted", Class: ClassConnectivity, Url: intercepted.URL, Expect: ExpectStatus, Status: http.StatusNoContent},
		{Name: "good-b", Class: ClassConnectivity, Url: good.URL, Expect: ExpectStatus, Status: http.StatusNoContent},
	}
	res := blackhole(context.Background(), http.DefaultClient, dests,
		Options{Rand: rand.New(rand.NewSource(1)), PerRequestTimeout: time.Second, Sleep: noSleep})

	if res.Ok {
		t.Fatal("OK = true, want false: a forged certificate is a hard provider failure")
	}
	if res.Failure != FailureTlsAuthentication {
		t.Fatalf("Failure = %q, want %q", res.Failure, FailureTlsAuthentication)
	}
	if len(res.Results) < 2 {
		t.Fatalf("tried only %d destinations: an ordinary success still ended the integrity check early", len(res.Results))
	}
	for _, r := range res.Results {
		if r.TlsAuthenticationFailure && r.Attempts != 1 {
			t.Errorf("%s: %d attempts after a TLS-authentication failure, want 1: it is terminal", r.Name, r.Attempts)
		}
	}
}

// Only the connectivity class is ever drawn.
func TestBlackholeSampleDrawsConnectivityOnly(t *testing.T) {
	sample := blackholeSample(stubDests("https://blackhole.example", 5), rand.New(rand.NewSource(7)))

	if len(sample) != BlackholeSampleSize {
		t.Fatalf("drew %d, want %d", len(sample), BlackholeSampleSize)
	}
	for _, d := range sample {
		if d.Class != ClassConnectivity {
			t.Errorf("drew %s from class %q, want %q only", d.Name, d.Class, ClassConnectivity)
		}
	}
}

// The real table must actually contain connectivity destinations, or the check
// silently degrades to "no sample, therefore a blackhole" and would condemn the
// entire fleet.
func TestBlackholeRealTableHasConnectivityDestinations(t *testing.T) {
	sample := blackholeSample(Destinations(), rand.New(rand.NewSource(1)))
	if len(sample) == 0 {
		t.Fatal("the real destination table drew no connectivity destinations: " +
			"every provider would be recorded as a blackhole")
	}
	if hosts := BlackholeHosts(); len(hosts) == 0 {
		t.Error("BlackholeHosts() is empty: the confinement self-check would not cover " +
			"the addresses this check dials, so a prober that could reach them directly would record every provider as ok")
	}
}

// The check's own rule, reached the slow way: two loads that never answer and
// one that answers only on its last try. One pass on any attempt of any load
// is traffic getting through.
func TestBlackholeOneLoadPassingOnItsLastAttemptIsOk(t *testing.T) {
	var lateCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/down", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/late", func(w http.ResponseWriter, r *http.Request) {
		if lateCalls.Add(1) < int32(DefaultLoadAttempts) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dests := []Destination{
		{Name: "down-a", Class: ClassConnectivity, Url: srv.URL + "/down", Expect: ExpectStatus, Status: http.StatusNoContent},
		{Name: "down-b", Class: ClassConnectivity, Url: srv.URL + "/down", Expect: ExpectStatus, Status: http.StatusNoContent},
		{Name: "late", Class: ClassConnectivity, Url: srv.URL + "/late", Expect: ExpectStatus, Status: http.StatusNoContent},
	}
	res := blackhole(context.Background(), srv.Client(), dests, Options{Rand: rand.New(rand.NewSource(1)), Sleep: noSleep})

	if !res.Ok || res.Failure != "" {
		t.Fatalf("OK = %t failure = %q, want OK: one load passed on its last attempt", res.Ok, res.Failure)
	}
	for _, r := range res.Results {
		want := DefaultLoadAttempts
		if r.Name == "late" && !r.Ok {
			t.Errorf("late = %+v, want a pass", r)
		}
		if r.Attempts != want {
			t.Errorf("%s: %d attempt(s), want %d", r.Name, r.Attempts, want)
		}
	}
}

// Every load failing every attempt is the one outcome that is
// all_destinations_failed -- and it is a failed check, which the server needs
// several of on one connection before it calls anything dark.
func TestBlackholeEveryLoadFailingEveryAttemptIsAllDestinationsFailed(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	res := blackhole(context.Background(), srv.Client(), stubDests(srv.URL, 5), Options{Rand: rand.New(rand.NewSource(2)), Sleep: noSleep})
	if res.Ok || res.Failure != FailureAllDestinationsFailed {
		t.Fatalf("OK = %t failure = %q, want %q", res.Ok, res.Failure, FailureAllDestinationsFailed)
	}
	if got, want := int(requests.Load()), BlackholeSampleSize*DefaultLoadAttempts; got != want {
		t.Errorf("made %d requests, want %d: every sampled load gets every try", got, want)
	}
	for _, r := range res.Results {
		if r.Ok || r.Attempts != DefaultLoadAttempts || r.LastFailure == "" {
			t.Errorf("%s = %+v, want %d failed attempts", r.Name, r, DefaultLoadAttempts)
		}
	}
}

// The check opens with the warm-up, before any load, and carries its exit
// address -- but the echo alone never makes a provider OK: a provider could
// carry the one well-known operator host and blackhole everything else.
func TestBlackholeWarmsUpFirstAndCarriesTheExitIp(t *testing.T) {
	var stateLock sync.Mutex
	var order []string
	mux := http.NewServeMux()
	mux.HandleFunc(IpEchoPath, func(w http.ResponseWriter, r *http.Request) {
		stateLock.Lock()
		order = append(order, "echo")
		stateLock.Unlock()
		_, _ = w.Write([]byte(`{"info":{"ip":"198.51.100.9"}}`))
	})
	mux.HandleFunc("/conn", func(w http.ResponseWriter, r *http.Request) {
		stateLock.Lock()
		order = append(order, "load")
		stateLock.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res := blackhole(context.Background(), srv.Client(), stubDests(srv.URL, 3), Options{
		Rand:      rand.New(rand.NewSource(3)),
		Sleep:     noSleep,
		IpEchoUrl: srv.URL + IpEchoPath,
	})
	if res.ExitIp != "198.51.100.9" || res.IpEchoErr != "" {
		t.Fatalf("ExitIp = %q (err %q), want the echo's address", res.ExitIp, res.IpEchoErr)
	}
	if res.Ok {
		t.Fatal("OK = true on the echo alone; only a load decides the check")
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	if len(order) == 0 || order[0] != "echo" {
		t.Fatalf("requests arrived as %v, want the warm-up first", order)
	}
	for _, o := range order[1:] {
		if o == "echo" {
			t.Fatalf("the warm-up ran more than once: %v", order)
		}
	}
}

// A check over the server's pool draws its three loads from the pool's
// connectivity class, not from the built-in table.
func TestBlackholeDrawsFromThePool(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	res := Blackhole(context.Background(), srv.Client(), Options{
		Rand:         rand.New(rand.NewSource(4)),
		Sleep:        noSleep,
		Destinations: stubDests(srv.URL, 4),
	})
	if !res.Ok {
		t.Fatalf("a check over a pool whose connectivity entries all answer failed: %+v", res)
	}
	for _, r := range res.Results {
		if !strings.HasPrefix(r.Name, "conn-") {
			t.Errorf("drew %q, which is not the pool's connectivity class", r.Name)
		}
	}
	if got := BlackholeHostsOf(stubDests(srv.URL, 4)); len(got) != 1 {
		t.Errorf("BlackholeHostsOf the pool = %v, want its one connectivity host", got)
	}
}
