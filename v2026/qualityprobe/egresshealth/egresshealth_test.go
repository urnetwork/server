package egresshealth

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests of a health run: the success contracts, sampling, the byte and time
// budgets, the production table's invariants and the summary line.

// Keeps every test off the wall clock: no test here talks to a real
// network, and the only thing a long timeout would buy is a slow suite. Loads
// keep the production attempt count, so every test below exercises the retries
// it would in the field, but the spacing between attempts is skipped (noSleep):
// a run whose failing loads each wait minutes would otherwise take minutes.
func fastOptions() Options {
	return Options{
		PerRequestTimeout: 200 * time.Millisecond,
		Budget:            5 * time.Second,
		Concurrency:       3,
		Sleep:             noSleep,
	}
}

// Describes what a stub destination does.
type handler func(w http.ResponseWriter, r *http.Request)

// A healthy destination: a non-error status and a real body.
func okBody(body string) handler {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}
}

// The blackhole signature at the http layer: a perfectly good
// status line and not one byte behind it.
func emptyBody200() handler {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}
}

// A healthy connectivity check: an empty body is the correct
// answer, and it is only distinguishable from the blackhole above by the status
// the destination declared it expects.
func status204() handler {
	return emptyStatus(http.StatusNoContent)
}

// Any status line with nothing behind it: the shape of every
// generate_204 endpoint and of the redirects this table declares rather than
// chases.
func emptyStatus(code int) handler {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}
}

// Models a content provider refusing the request -- the
// datacenter-IP-rejection case. Bytes flow in both directions; the answer is no.
func statusWithBody(code int, body string) handler {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}
}

// Never answers until the test tears down, modelling a provider that
// swallows the request.
func hangs(done <-chan struct{}) handler {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}
}

// One stub destination: a name, a class, a handler, and the parts of
// the success contract a test wants to exercise. The contract fields are keyed
// and optional, so the common case stays a three-field literal.
type spec struct {
	name   string
	class  Class
	h      handler
	expect Expect
	status int
	verify BodyCheck
}

// Stands up one httptest server multiplexing every named
// destination and returns the table pointing at it. Order is preserved so
// assertions can index by position.
func stubDestinations(t *testing.T, specs []spec) []Destination {
	t.Helper()
	mux := http.NewServeMux()
	dests := make([]Destination, 0, len(specs))
	for _, s := range specs {
		path := "/" + s.name
		mux.HandleFunc(path, s.h)
		dests = append(dests, Destination{
			Name:   s.name,
			Class:  s.class,
			Url:    path,
			Expect: s.expect,
			Status: s.status,
			Verify: s.verify,
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	for i := range dests {
		dests[i].Url = srv.URL + dests[i].Url
	}
	return dests
}

// A healthy table -- every scored class, three deep, all working -- passes
// whole, each load on its first attempt. The connectivity entries
// deliberately mix both success contracts: two 204s where an empty body is
// correct, and one small body, which is exactly the shape of the production
// class.
func TestCheckAllHealthy(t *testing.T) {
	dests := stubDestinations(t, []spec{
		{name: "dns-a", class: ClassDns, h: okBody(`{"Status":0}`)},
		{name: "dns-b", class: ClassDns, h: okBody(`{"Status":0}`)},
		{name: "dns-c", class: ClassDns, h: okBody(`{"Status":0}`)},
		{name: "conn-a", class: ClassConnectivity, h: status204(), expect: ExpectStatus, status: http.StatusNoContent},
		{name: "conn-b", class: ClassConnectivity, h: status204(), expect: ExpectStatus, status: http.StatusNoContent},
		{name: "conn-c", class: ClassConnectivity, h: okBody("success\n")},
		{name: "cdn-a", class: ClassCdn, h: okBody("/* css */")},
		{name: "cdn-b", class: ClassCdn, h: okBody("/* css */")},
		{name: "cdn-c", class: ClassCdn, h: okBody("/* css */")},
		{name: "site-a", class: ClassSite, h: okBody("User-agent: *")},
		{name: "site-b", class: ClassSite, h: okBody("User-agent: *")},
		{name: "site-c", class: ClassSite, h: okBody("User-agent: *")},
	})
	res, err := check(context.Background(), http.DefaultClient, dests, fastOptions())
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	if res.OkCount != res.Total || res.Total != len(dests) {
		t.Fatalf("OkCount/Total = %d/%d, want %d/%d", res.OkCount, res.Total, len(dests), len(dests))
	}
	byName := map[string]Destination{}
	for _, d := range dests {
		byName[d.Name] = d
	}
	for _, c := range res.Checks {
		if !c.Ok {
			t.Errorf("%s not OK: status=%d bytes=%d err=%q", c.Name, c.StatusCode, c.ByteCount, c.Err)
		}
		// Only an ExpectBody destination owes us bytes. Asserting it for the
		// whole table would make the 204 entries -- where an empty body is the
		// correct answer -- unrepresentable in a healthy table.
		if byName[c.Name].Expect == ExpectBody && c.ByteCount == 0 {
			t.Errorf("%s reported OK with zero bytes", c.Name)
		}
		if c.Err != "" {
			t.Errorf("%s is OK but carries Err %q", c.Name, c.Err)
		}
		// A load that passes first time is fetched once: the retries are for
		// failures, and a healthy provider must not be asked twice.
		if c.Attempts != 1 || c.LastFailure != "" {
			t.Errorf("%s: attempts=%d last_failure=%q, want one clean attempt", c.Name, c.Attempts, c.LastFailure)
		}
	}
	for _, class := range Classes {
		s := res.ByClass[class]
		if s.Ok != 3 || s.Total != 3 {
			t.Errorf("ByClass[%s] = %d/%d, want 3/3", class, s.Ok, s.Total)
		}
	}
	if got, want := res.Summary(), "ok=12/12 dns=3/3 connectivity=3/3 cdn=3/3 site=3/3"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
}

// Reproduces the production
// failure where a provider returned a self-signed leaf for a real HTTPS
// destination. A string-only error is not enough: the operator has to carry a
// machine-readable, fail-closed signal to the server so one forged certificate
// cannot be diluted into an otherwise healthy percentage.
func TestFetchClassifiesTlsAuthenticationFailure(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	result := fetch(context.Background(), http.DefaultClient, Destination{
		Name:   "intercepted",
		Class:  ClassConnectivity,
		Url:    srv.URL,
		Expect: ExpectStatus,
		Status: http.StatusNoContent,
	}, time.Second, DefaultRequestProfile(), time.Now)

	if result.Ok {
		t.Fatal("a self-signed TLS endpoint passed")
	}
	if !result.TlsAuthenticationFailure {
		t.Fatalf("TlsAuthenticationFailure = false, want true for %q", result.Err)
	}
}

// An ordinary application or reachability failure must not be promoted into
// the hard TLS-authenticity signal. Otherwise one refused endpoint could evict
// a working provider instead of merely lowering its sampled health score.
func TestFetchDoesNotClassifyHttpFailureAsTlsAuthenticationFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	result := fetch(context.Background(), srv.Client(), Destination{
		Name:   "ordinary-failure",
		Class:  ClassConnectivity,
		Url:    srv.URL,
		Expect: ExpectStatus,
		Status: http.StatusNoContent,
	}, time.Second, DefaultRequestProfile(), time.Now)

	if result.Ok {
		t.Fatal("the wrong status unexpectedly passed")
	}
	if result.TlsAuthenticationFailure {
		t.Fatalf("TlsAuthenticationFailure = true for ordinary HTTP failure %q", result.Err)
	}
}

// The run-level bit is what crosses the ingest boundary. A single forged
// certificate must survive aggregation even though all of the ordinary score
// math still describes the rest of the sample.
func TestCheckAggregatesTlsAuthenticationFailure(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer good.Close()
	intercepted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer intercepted.Close()

	dests := []Destination{
		{Name: "good", Class: ClassConnectivity, Url: good.URL, Expect: ExpectStatus, Status: http.StatusNoContent},
		{Name: "intercepted", Class: ClassConnectivity, Url: intercepted.URL, Expect: ExpectStatus, Status: http.StatusNoContent},
	}
	res, err := check(context.Background(), http.DefaultClient, dests, fastOptions())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !res.TlsAuthenticationFailure {
		t.Fatalf("TlsAuthenticationFailure = false; checks = %+v", res.Checks)
	}
	if got := res.Summary(); !strings.Contains(got, "tls_authentication_failure=true") {
		t.Fatalf("Summary = %q, want the hard TLS failure visible in logs", got)
	}
}

// The case this package exists for, and it asserts
// both halves of the requirement in one place: a blackhole is a successful run
// reporting 0/12 (err == nil), while a run that could not happen at all is an
// error and no Result. If those two collapsed into each other, "this provider
// delivers nothing" would be indistinguishable from "the prober was
// misconfigured", and the whole signal would be unusable.
func TestCheckTotalBlackhole(t *testing.T) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })

	dests := stubDestinations(t, []spec{
		// Half swallow the request entirely, half return a status line with no
		// body -- the two shapes a blackholing provider produces. Note the
		// connectivity entries: a bare 200 where the destination declared 204 is
		// the synthesized-status-line case, and it must fail.
		{name: "dns-a", class: ClassDns, h: hangs(done)},
		{name: "dns-b", class: ClassDns, h: emptyBody200()},
		{name: "dns-c", class: ClassDns, h: hangs(done)},
		{name: "conn-a", class: ClassConnectivity, h: emptyBody200(), expect: ExpectStatus, status: http.StatusNoContent},
		{name: "conn-b", class: ClassConnectivity, h: hangs(done), expect: ExpectStatus, status: http.StatusNoContent},
		{name: "conn-c", class: ClassConnectivity, h: emptyBody200()},
		{name: "cdn-a", class: ClassCdn, h: emptyBody200()},
		{name: "cdn-b", class: ClassCdn, h: hangs(done)},
		{name: "cdn-c", class: ClassCdn, h: emptyBody200()},
		{name: "site-a", class: ClassSite, h: hangs(done)},
		{name: "site-b", class: ClassSite, h: emptyBody200()},
		{name: "site-c", class: ClassSite, h: hangs(done)},
	})

	res, err := check(context.Background(), http.DefaultClient, dests, fastOptions())
	if err != nil {
		t.Fatalf("a total blackhole must be a RESULT, not an error: %v", err)
	}
	if res.OkCount != 0 {
		t.Fatalf("OkCount = %d, want 0 for a total blackhole (%+v)", res.OkCount, res.Checks)
	}
	if res.Total != 12 {
		t.Fatalf("Total = %d, want the 12 scored destinations", res.Total)
	}
	if len(res.Checks) != len(dests) {
		t.Fatalf("len(Checks) = %d, want %d; every destination must be attempted", len(res.Checks), len(dests))
	}
	for _, class := range Classes {
		if s := res.ByClass[class]; s.Ok != 0 || s.Total != 3 {
			t.Errorf("ByClass[%s] = %d/%d, want 0/3; a wholly failing class must still be reported", class, s.Ok, s.Total)
		}
	}
	if got, want := res.Summary(), "ok=0/12 dns=0/3 connectivity=0/3 cdn=0/3 site=0/3"; got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
	// A blackhole fails every load on every try: the zero above is after the
	// retries, not instead of them.
	for _, c := range res.Checks {
		if c.Attempts != DefaultLoadAttempts {
			t.Errorf("%s: %d attempt(s), want all %d before it counts as failed", c.Name, c.Attempts, DefaultLoadAttempts)
		}
		if c.LastFailure == "" || c.LastFailure != c.Err {
			t.Errorf("%s: last_failure = %q, err = %q; a failed load's last failure is its error", c.Name, c.LastFailure, c.Err)
		}
	}

	// ...and the structural failures, which must not look like a blackhole: a
	// nil client, an empty table, and no budget left.
	if res, err := check(context.Background(), nil, dests, fastOptions()); !errors.Is(err, ErrNilClient) || res != nil {
		t.Errorf("check(nil client) = (%v, %v), want (nil, ErrNilClient)", res, err)
	}
	if res, err := check(context.Background(), http.DefaultClient, nil, fastOptions()); !errors.Is(err, ErrNoDestinations) || res != nil {
		t.Errorf("check(no destinations) = (%v, %v), want (nil, ErrNoDestinations)", res, err)
	}
	// A run started on an already-dead context returns 0/9 for reasons that
	// have nothing to do with the provider. Reporting that as a blackhole
	// would frame the prober's own exhausted deadline as a provider fault.
	deadCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if res, err := check(deadCtx, http.DefaultClient, dests, fastOptions()); !errors.Is(err, ErrNoBudget) || res != nil {
		t.Errorf("check(dead ctx) = (%v, %v), want (nil, ErrNoBudget)", res, err)
	}
}

// The datacenter-IP case and the main reason the
// class dimension exists: DNS resolves fine, every CDN refuses. A flat
// "ok=6/9" would be indistinguishable from six random flakes.
func TestCheckSelectiveFailure(t *testing.T) {
	dests := stubDestinations(t, []spec{
		{name: "dns-a", class: ClassDns, h: okBody(`{"Status":0}`)},
		{name: "dns-b", class: ClassDns, h: okBody(`{"Status":0}`)},
		{name: "dns-c", class: ClassDns, h: okBody(`{"Status":0}`)},
		{name: "conn-a", class: ClassConnectivity, h: status204(), expect: ExpectStatus, status: http.StatusNoContent},
		{name: "conn-b", class: ClassConnectivity, h: status204(), expect: ExpectStatus, status: http.StatusNoContent},
		{name: "conn-c", class: ClassConnectivity, h: okBody("success\n")},
		{name: "cdn-a", class: ClassCdn, h: statusWithBody(http.StatusForbidden, "error 1015: rate limited")},
		{name: "cdn-b", class: ClassCdn, h: statusWithBody(http.StatusServiceUnavailable, "denied: datacenter range")},
		{name: "cdn-c", class: ClassCdn, h: statusWithBody(http.StatusForbidden, "access denied")},
		{name: "site-a", class: ClassSite, h: okBody("User-agent: *")},
		{name: "site-b", class: ClassSite, h: okBody("User-agent: *")},
		{name: "site-c", class: ClassSite, h: okBody("User-agent: *")},
	})

	res, err := check(context.Background(), http.DefaultClient, dests, fastOptions())
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	if got, want := res.Summary(), "ok=9/12 dns=3/3 connectivity=3/3 cdn=0/3 site=3/3"; got != want {
		t.Fatalf("Summary = %q, want %q", got, want)
	}
	if s := res.ByClass[ClassCdn]; s.Ok != 0 || s.Total != 3 {
		t.Fatalf("ByClass[cdn] = %d/%d, want 0/3", s.Ok, s.Total)
	}
	if s := res.ByClass[ClassDns]; s.Ok != 3 {
		t.Fatalf("ByClass[dns].OK = %d, want 3", s.Ok)
	}

	// The refusals must be recorded as refusals -- a status and a body -- not
	// as silence. This is what tells an operator "the tunnel carried bytes and
	// the CDN said no" rather than "nothing came back", and it is the whole
	// diagnostic difference between this case and a blackhole.
	for _, c := range res.Checks {
		if c.Class != ClassCdn {
			continue
		}
		if c.StatusCode == 0 {
			t.Errorf("%s: StatusCode = 0; a refusal must record the status it was refused with", c.Name)
		}
		if c.ByteCount == 0 {
			t.Errorf("%s: ByteCount = 0 on a refusal that carried a body; without this, a refusal is indistinguishable from a blackhole", c.Name)
		}
		if !strings.Contains(c.Err, "status") {
			t.Errorf("%s: Err = %q, want it to name the status", c.Name, c.Err)
		}
	}
	if names := res.FailedNames(); strings.Join(names, ",") != "cdn-a,cdn-b,cdn-c" {
		t.Errorf("FailedNames = %v, want the three cdn destinations in table order", names)
	}
}

// Asserted on its own because it is the rule the
// whole package turns on. A provider that terminates connections locally, or
// whose upstream returns a stub, produces exactly this: a clean 200 and zero
// bytes. If that counted as success, the failure being hunted would pass the
// hunt.
func TestEmptyBodyIs200Failure(t *testing.T) {
	cases := []struct {
		name string
		h    handler
	}{
		{name: "explicit 200 with no body", h: emptyBody200()},
		{name: "200 with a zero-length write", h: func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write(nil)
		}},
		{name: "204 no content", h: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}},
	}
	for _, tc := range cases {
		dests := stubDestinations(t, []spec{{name: "d", class: ClassDns, h: tc.h}})
		res, err := check(context.Background(), http.DefaultClient, dests, fastOptions())
		if err != nil {
			t.Fatalf("%s: check err = %v", tc.name, err)
		}
		c := res.Checks[0]
		if c.Ok {
			t.Fatalf("%s: a %d with an empty body counted as SUCCESS; that is the blackhole signature", tc.name, c.StatusCode)
		}
		if c.ByteCount != 0 {
			t.Fatalf("%s: ByteCount = %d, want 0", tc.name, c.ByteCount)
		}
		if c.StatusCode == 0 {
			t.Fatalf("%s: StatusCode = 0; the status line did arrive and must be recorded", tc.name)
		}
		if !strings.Contains(c.Err, "empty body") && !strings.Contains(c.Err, "status") {
			t.Fatalf("%s: Err = %q, want it to say why", tc.name, c.Err)
		}
		if res.OkCount != 0 {
			t.Fatalf("%s: OkCount = %d, want 0", tc.name, res.OkCount)
		}
	}
}

// A destination that streams far more than the cap must
// not cause an oversized read. Without the cap a hostile (or merely
// misconfigured) destination could spend the provider's entire byte budget --
// the budget this whole package is documented to stay inside.
func TestBodyCapHonoured(t *testing.T) {
	const streamed = 64 * MaxBodyBytes

	dests := stubDestinations(t, []spec{{name: "flood", class: ClassCdn, h: func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 4096)
		for i := range chunk {
			chunk[i] = 'x'
		}
		for written := 0; written < streamed; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}}})

	res, err := check(context.Background(), http.DefaultClient, dests, fastOptions())
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	c := res.Checks[0]
	if MaxBodyBytes < c.ByteCount {
		t.Fatalf("read %d bytes from a destination streaming %d; the cap is %d and an uncapped read spends the provider's byte budget", c.ByteCount, streamed, MaxBodyBytes)
	}
	if c.ByteCount != MaxBodyBytes {
		t.Fatalf("ByteCount = %d, want exactly the cap %d (the destination had far more to give)", c.ByteCount, MaxBodyBytes)
	}
	if !c.Ok {
		t.Fatalf("a large healthy response must still be OK: %+v", c)
	}
}

// The pattern is the value, so a failure in
// the middle of the table must not stop the destinations after it from being
// attempted.
func TestOneFailureDoesNotAbortTheRun(t *testing.T) {
	dests := stubDestinations(t, []spec{
		{name: "first", class: ClassDns, h: okBody("ok")},
		{name: "broken", class: ClassCdn, h: statusWithBody(http.StatusInternalServerError, "boom")},
		{name: "last", class: ClassSite, h: okBody("ok")},
	})
	res, err := check(context.Background(), http.DefaultClient, dests, fastOptions())
	if err != nil {
		t.Fatalf("one failing destination must not fail the run: %v", err)
	}
	if len(res.Checks) != 3 {
		t.Fatalf("len(Checks) = %d, want 3; every destination must be recorded", len(res.Checks))
	}
	if !res.Checks[2].Ok {
		t.Fatalf("the destination after the failing one was not attempted or not recorded: %+v", res.Checks[2])
	}
	if got, want := res.Summary(), "ok=2/3 dns=1/1 cdn=0/1 site=1/1"; got != want {
		t.Fatalf("Summary = %q, want %q", got, want)
	}
}

// Every request is the browser profile plus the
// destination's own headers over it (Cloudflare's DoH JSON form answers 400
// without its Accept header), a plain GET, and never a Range header -- even
// when a destination, which may now arrive from the server as data, asks for
// one.
func TestRequestShape(t *testing.T) {
	var stateLock sync.Mutex
	var got http.Header
	var gotMethod string
	dests := stubDestinations(t, []spec{{name: "d", class: ClassDns, h: func(w http.ResponseWriter, r *http.Request) {
		stateLock.Lock()
		gotMethod, got = r.Method, r.Header.Clone()
		stateLock.Unlock()
		_, _ = w.Write([]byte("ok"))
	}}})
	dests[0].Headers = map[string]string{"Accept": acceptDnsJson, "Range": "bytes=0-1023", "Accept-Encoding": "br"}

	opts := fastOptions()
	opts.LoadAttempts = 1
	if _, err := check(context.Background(), http.DefaultClient, dests, opts); err != nil {
		t.Fatalf("check err = %v", err)
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if got.Get("Accept") != acceptDnsJson {
		t.Errorf("Accept = %q, want the destination's %q over the profile's", got.Get("Accept"), acceptDnsJson)
	}
	if _, present := got["Range"]; present {
		t.Errorf("Range = %q was sent; a byte-range request is one of the things bot managers refuse", got.Get("Range"))
	}
	// The transport's own gzip, which it decodes transparently -- not the br
	// the destination named, which would have handed the capped read and the
	// body check compressed bytes.
	if got.Get("Accept-Encoding") != "gzip" {
		t.Errorf("Accept-Encoding = %q, want the transport's own gzip", got.Get("Accept-Encoding"))
	}
	profile := DefaultRequestProfile()
	if got.Get("User-Agent") != profile.UserAgent {
		t.Errorf("User-Agent = %q, want the profile's %q", got.Get("User-Agent"), profile.UserAgent)
	}
	for _, name := range []string{"Accept-Language", "Upgrade-Insecure-Requests", "Sec-Fetch-Dest", "Sec-Fetch-Mode", "Sec-Fetch-Site", "Sec-Fetch-User"} {
		if got.Get(name) != profile.Headers[name] {
			t.Errorf("%s = %q, want the profile's %q", name, got.Get(name), profile.Headers[name])
		}
	}
}

// The run must not open the whole table at once over
// a cold tunnel.
func TestConcurrencyIsBounded(t *testing.T) {
	const limit = 2
	var stateLock sync.Mutex
	inFlight, peak := 0, 0
	specs := make([]spec, 0, 9)
	for i := 0; i < 9; i++ {
		specs = append(specs, spec{name: fmt.Sprintf("d%d", i), class: ClassDns, h: func(w http.ResponseWriter, r *http.Request) {
			stateLock.Lock()
			inFlight++
			if peak < inFlight {
				peak = inFlight
			}
			stateLock.Unlock()
			time.Sleep(10 * time.Millisecond)
			stateLock.Lock()
			inFlight--
			stateLock.Unlock()
			_, _ = w.Write([]byte("ok"))
		}})
	}
	dests := stubDestinations(t, specs)

	opts := fastOptions()
	opts.Concurrency = limit
	opts.PerRequestTimeout = 2 * time.Second
	if _, err := check(context.Background(), http.DefaultClient, dests, opts); err != nil {
		t.Fatalf("check err = %v", err)
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	if limit < peak {
		t.Fatalf("peak in-flight requests = %d, want at most Concurrency = %d", peak, limit)
	}
}

// A provider that swallows everything must not hold the
// pass open for concurrency-batched multiples of the per-request timeout.
func TestBudgetBoundsTheRun(t *testing.T) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	specs := make([]spec, 0, 9)
	for i := 0; i < 9; i++ {
		specs = append(specs, spec{name: fmt.Sprintf("d%d", i), class: ClassDns, h: hangs(done)})
	}
	dests := stubDestinations(t, specs)

	opts := Options{PerRequestTimeout: 10 * time.Second, Budget: 300 * time.Millisecond, Concurrency: 1}
	start := time.Now()
	res, err := check(context.Background(), http.DefaultClient, dests, opts)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	if res.OkCount != 0 {
		t.Fatalf("OkCount = %d, want 0", res.OkCount)
	}
	if 5*time.Second < elapsed {
		t.Fatalf("the run took %s with a 300ms budget; the budget does not bound it", elapsed)
	}
}

// The zero Options, and negative settings, fall back to the documented
// defaults rather than disabling a bound.
func TestOptionsDefaults(t *testing.T) {
	var zero Options
	if zero.perRequestTimeout() != DefaultPerRequestTimeout {
		t.Errorf("perRequestTimeout = %s, want the default", zero.perRequestTimeout())
	}
	if zero.budget(50) != zero.RunBudget(50) {
		t.Errorf("budget = %s, want the derived RunBudget %s", zero.budget(50), zero.RunBudget(50))
	}
	if zero.concurrency() != DefaultConcurrency {
		t.Errorf("concurrency = %d, want the default", zero.concurrency())
	}
	if zero.loadAttempts() != DefaultLoadAttempts || DefaultLoadAttempts != 3 {
		t.Errorf("loadAttempts = %d (default %d), want 3", zero.loadAttempts(), DefaultLoadAttempts)
	}
	if zero.loadRetryMeanInterval() != DefaultLoadRetryMeanInterval || DefaultLoadRetryMeanInterval != 5*time.Minute {
		t.Errorf("loadRetryMeanInterval = %s (default %s), want 5m", zero.loadRetryMeanInterval(), DefaultLoadRetryMeanInterval)
	}
	if zero.ipEchoTimeout() != DefaultIpEchoTimeout {
		t.Errorf("ipEchoTimeout = %s, want the default", zero.ipEchoTimeout())
	}
	if zero.tunnelRecreateAttempts() != DefaultTunnelRecreateAttempts || DefaultTunnelRecreateAttempts != 2 {
		t.Errorf("tunnelRecreateAttempts = %d (default %d), want 2", zero.tunnelRecreateAttempts(), DefaultTunnelRecreateAttempts)
	}
	if got := zero.profile(); got.UserAgent != DefaultRequestProfile().UserAgent {
		t.Errorf("profile user agent = %q, want the default profile's", got.UserAgent)
	}
	if len(zero.table()) != len(destinations) {
		t.Errorf("table has %d destinations, want the built-in %d", len(zero.table()), len(destinations))
	}
	neg := Options{PerRequestTimeout: -1, Budget: -1, Concurrency: -1, LoadAttempts: -1, LoadRetryMeanInterval: -1, IpEchoTimeout: -1}
	if neg.perRequestTimeout() != DefaultPerRequestTimeout || neg.budget(50) != zero.RunBudget(50) || neg.concurrency() != DefaultConcurrency ||
		neg.loadAttempts() != DefaultLoadAttempts || neg.loadRetryMeanInterval() != DefaultLoadRetryMeanInterval || neg.ipEchoTimeout() != DefaultIpEchoTimeout {
		t.Error("a negative option must fall back to the default, not disable the bound")
	}
}

// Guards the production table's shape: names are what the
// log line and any future storage key on, so they must be unique and non-empty,
// and every destination must belong to a declared class.
func TestDestinationsTable(t *testing.T) {
	if len(destinations) < 8 {
		t.Fatalf("len(destinations) = %d; the table is meant to span several classes and operators", len(destinations))
	}
	// Every class must be in Classes or it would sort after the declared ones in
	// every summary -- and a pool carrying it would be refused.
	declared := map[Class]bool{}
	for _, c := range Classes {
		declared[c] = true
	}
	seen := map[string]bool{}
	perClass := map[Class]int{}
	for i, d := range destinations {
		if d.Name == "" {
			t.Fatalf("destinations[%d] has no name", i)
		}
		if seen[d.Name] {
			t.Fatalf("destinations[%d] repeats the name %q", i, d.Name)
		}
		seen[d.Name] = true
		if !declared[d.Class] {
			t.Fatalf("destination %q has class %q, which is not in Classes -- it would sort after the declared ones in every summary", d.Name, d.Class)
		}
		perClass[d.Class]++
	}
	for _, c := range Classes {
		if perClass[c] < 2 {
			t.Errorf("class %q has %d destination(s); a class with fewer than two cannot distinguish a class-wide fault from one flaky endpoint", c, perClass[c])
		}
	}
}

// Encodes a constraint that is invisible in
// the table itself: cmd/egress-prober's confinement self-check dials one fixed
// port (443). A destination on any other port would be silently uncovered by
// that check while the check kept reporting a pass -- which is exactly why the
// Quad9 DoH JSON endpoint (port 5053) was rejected for this table.
func TestEveryDestinationIsHttpsOn443(t *testing.T) {
	for _, d := range destinations {
		u, err := url.Parse(d.Url)
		if err != nil {
			t.Fatalf("destination %q has an unparseable URL %q: %s", d.Name, d.Url, err)
		}
		if u.Scheme != "https" {
			t.Errorf("destination %q is %q, not https; a plaintext destination could be forged by the provider on the path", d.Name, u.Scheme)
		}
		if p := u.Port(); p != "" && p != "443" {
			t.Errorf("destination %q is on port %q; the confinement self-check only dials 443, so this destination would not be covered by it", d.Name, p)
		}
	}
}

// The point of the table is
// that a provider which whitelists one vendor cannot pass. Two destinations in
// the same class sharing a host would be one destination wearing two names.
func TestDestinationsSpreadAcrossOperatorsWithinAClass(t *testing.T) {
	perClass := map[Class]map[string]string{}
	for _, d := range destinations {
		u, err := url.Parse(d.Url)
		if err != nil {
			t.Fatalf("destination %q: %s", d.Name, err)
		}
		if perClass[d.Class] == nil {
			perClass[d.Class] = map[string]string{}
		}
		if other, dup := perClass[d.Class][u.Hostname()]; dup {
			t.Errorf("destinations %q and %q are both class %q on host %q; one blocked host would fail both", other, d.Name, d.Class, u.Hostname())
		}
		perClass[d.Class][u.Hostname()] = d.Name
	}
}

// The anti-drift test. The
// operator translates this list into -confinement-address entries: if a
// destination is added to the table and its host does not come out here, the
// confinement check silently stops covering a real endpoint while still
// reporting success.
func TestDestinationHostsCoversEveryDestination(t *testing.T) {
	hosts := DestinationHosts()
	if len(hosts) == 0 {
		t.Fatal("DestinationHosts is empty; the confinement check would have nothing to test")
	}
	for _, d := range destinations {
		u, err := url.Parse(d.Url)
		if err != nil {
			t.Fatalf("destination %q has an unparseable URL %q: %s", d.Name, d.Url, err)
		}
		found := false
		for _, h := range hosts {
			if h == u.Hostname() {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("destination %q (%s) has no host in DestinationHosts %v; the confinement check would not cover it", d.Name, d.Url, hosts)
		}
	}

	seen := map[string]bool{}
	for _, h := range hosts {
		if h == "" {
			t.Fatalf("DestinationHosts contains an empty host: %v", hosts)
		}
		if seen[h] {
			t.Fatalf("DestinationHosts contains %q twice: %v", h, hosts)
		}
		seen[h] = true
		// Each entry must be a bare, dialable host: no scheme, no port, no path.
		if strings.ContainsAny(h, "/:") {
			t.Fatalf("DestinationHosts entry %q is not a bare host", h)
		}
		if u, err := url.Parse("https://" + h); err != nil || u.Hostname() != h {
			t.Fatalf("DestinationHosts entry %q does not parse back to itself", h)
		}
	}

	// One entry per distinct host, no more and no fewer.
	distinct := map[string]bool{}
	for _, d := range destinations {
		u, _ := url.Parse(d.Url)
		distinct[u.Hostname()] = true
	}
	if len(hosts) != len(distinct) {
		t.Fatalf("DestinationHosts has %d entries for %d distinct hosts", len(hosts), len(distinct))
	}
}

// The summary is read from logs across passes, so
// it must not reorder itself between runs (ByClass is a map, and iterating it
// directly would).
func TestSummaryIsDeterministic(t *testing.T) {
	res := &Result{
		OkCount: 4,
		Total:   9,
		ByClass: map[Class]ClassSummary{
			ClassSite: {Ok: 1, Total: 3},
			ClassDns:  {Ok: 3, Total: 3},
			ClassCdn:  {Ok: 0, Total: 3},
		},
	}
	want := "ok=4/9 dns=3/3 cdn=0/3 site=1/3"
	for i := 0; i < 50; i++ {
		if got := res.Summary(); got != want {
			t.Fatalf("Summary = %q, want %q (iteration %d)", got, want, i)
		}
	}
}

// A class added to the table but not to
// Classes must still be reported, sorted, rather than disappearing from the
// line.
func TestSummaryIncludesAnUndeclaredClass(t *testing.T) {
	res := &Result{
		OkCount: 1,
		Total:   2,
		ByClass: map[Class]ClassSummary{
			ClassDns:     {Ok: 1, Total: 1},
			Class("zzz"): {Ok: 0, Total: 1},
		},
	}
	if got, want := res.Summary(), "ok=1/2 dns=1/1 zzz=0/1"; got != want {
		t.Fatalf("Summary = %q, want %q", got, want)
	}
}

// A nil result renders as an empty tally and lists nothing.
func TestSummaryOfNilResult(t *testing.T) {
	var res *Result
	if got := res.Summary(); got != "ok=0/0" {
		t.Fatalf("Summary of a nil Result = %q", got)
	}
	if names := res.FailedNames(); names != nil {
		t.Fatalf("FailedNames of a nil Result = %v", names)
	}
}

// The trust property, asserted rather
// than assumed: the package must make every request through the *http.Client it
// was handed. If it ever built its own transport, the confined prober's
// "no path out except the tunnel" guarantee would be silently broken -- a
// request could then succeed without a working provider.
func TestCheckUsesTheInjectedClientOnly(t *testing.T) {
	var calls int64
	var stateLock sync.Mutex
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		stateLock.Lock()
		calls++
		stateLock.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       http.NoBody,
			Header:     http.Header{},
			Request:    r,
		}, nil
	})
	client := &http.Client{Transport: rt}

	// URLs that resolve to nothing: if anything reached the network instead of
	// the injected transport, this would fail rather than silently pass.
	dests := []Destination{
		{Name: "a", Class: ClassDns, Url: "https://egresshealth.invalid/a"},
		{Name: "b", Class: ClassCdn, Url: "https://egresshealth.invalid/b"},
	}
	res, err := check(context.Background(), client, dests, fastOptions())
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	// Every load fails (below), so each is tried every time -- and every try
	// must go through the supplied client.
	if want := int64(DefaultLoadAttempts * len(dests)); calls != want {
		t.Fatalf("injected transport saw %d requests, want %d; some request did not go through the supplied client", calls, want)
	}
	// http.NoBody is a 200 with zero bytes: the empty-body rule applies.
	if res.OkCount != 0 {
		t.Fatalf("OkCount = %d, want 0 (every response was a bodiless 200)", res.OkCount)
	}
}

// A function used as an http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

// Implements http.RoundTripper.
func (self roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return self(r) }

// Check must run a sample of the production
// table -- not an empty one, not a stub one, and not the whole thing. It cannot
// reach the real hosts here (no network is used), but the shape of the result
// proves which table it drew from and that it drew rather than took.
func TestCheckProductionTableIsWiredUp(t *testing.T) {
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("no network in tests")
	})
	res, err := Check(context.Background(), &http.Client{Transport: rt}, fastOptions())
	if err != nil {
		t.Fatalf("Check err = %v", err)
	}

	// Total counts every destination of the sample: every class is scored.
	scoredSample := SamplePerRun()
	if res.Total != scoredSample {
		t.Fatalf("Total = %d, want the %d destinations of one sample", res.Total, scoredSample)
	}
	if len(res.Checks) != SamplePerRun() {
		t.Fatalf("len(Checks) = %d, want SamplePerRun() = %d; every sampled destination must be ATTEMPTED and recorded", len(res.Checks), SamplePerRun())
	}
	if len(destinations) <= len(res.Checks) {
		t.Fatalf("Check ran %d of the %d destinations; it must SAMPLE the table, not fetch it -- the whole table costs 128 KiB per provider per run", len(res.Checks), len(destinations))
	}

	// Every name that came back must be a real production destination: a sample
	// of a stub table would be small too.
	inTable := map[string]bool{}
	for _, d := range destinations {
		inTable[d.Name] = true
	}
	for _, c := range res.Checks {
		if !inTable[c.Name] {
			t.Fatalf("Check ran %q, which is not in the production table", c.Name)
		}
	}
	if res.TableTotal != len(destinations) {
		t.Fatalf("TableTotal = %d, want the full table %d; the log line has to say what the sample was drawn from", res.TableTotal, len(destinations))
	}
	if want := fmt.Sprintf("table=%d", len(destinations)); !strings.Contains(res.Summary(), want) {
		t.Errorf("Summary = %q, want it to carry %s, or dns=6/6 reads as a six-entry class", res.Summary(), want)
	}

	// Sorted class tallies must still cover every destination sampled.
	total := 0
	for _, s := range res.ByClass {
		total += s.Total
	}
	if total != scoredSample {
		t.Fatalf("ByClass totals sum to %d, want %d", total, scoredSample)
	}
	if _, present := res.ByClass[Class("reputation")]; present {
		t.Fatal("ByClass carries a reputation entry; the class is dissolved into site, and the server rejects it inside class_results")
	}
}

// Keeps Classes (the render order) and the table from
// drifting apart in the other direction: a class declared but never used just
// makes the summary lie about what was checked.
func TestClassesCoversTheTable(t *testing.T) {
	used := map[Class]bool{}
	for _, d := range destinations {
		used[d.Class] = true
	}
	var unused []string
	for _, c := range Classes {
		if !used[c] {
			unused = append(unused, string(c))
		}
	}
	sort.Strings(unused)
	if len(unused) != 0 {
		t.Fatalf("Classes declares %v, which no destination uses", unused)
	}
}

// The production table must not be reachable for
// mutation from outside. A caller that changed an entry -- or the Headers map
// inside one -- would change what every subsequent probe measures, invisibly
// from the table's own source.
func TestDestinationsReturnsACopy(t *testing.T) {
	got := Destinations()
	if len(got) != len(destinations) {
		t.Fatalf("Destinations() has %d entries, want %d", len(got), len(destinations))
	}
	for i := range got {
		if got[i].Name != destinations[i].Name || got[i].Url != destinations[i].Url || got[i].Class != destinations[i].Class {
			t.Fatalf("Destinations()[%d] = %+v, want %+v", i, got[i], destinations[i])
		}
	}

	got[0].Url = "https://mutated.example/"
	got[0].Name = "mutated"
	for k := range got[0].Headers {
		got[0].Headers[k] = "mutated"
	}
	if destinations[0].Url == "https://mutated.example/" || destinations[0].Name == "mutated" {
		t.Fatal("mutating the returned slice changed the production table")
	}
	for k, v := range destinations[0].Headers {
		if v == "mutated" {
			t.Fatalf("mutating a returned entry's Headers changed the production table (%s)", k)
		}
	}
}

// The new half of the success contract.
// Every generate_204 connectivity endpoint answers 204 with zero bytes -- that
// is the correct answer, and 21 of 143 endpoints measured from a real
// datacenter host behave this way. Under the ExpectBody rule they would all be
// scored as failures.
//
// Read this together with TestEmptyBodyIs200Failure: the same handler, an empty
// 204, is a success here and a failure there. The only difference is what the
// destination declared, which is the whole point of Expect.
func TestExpectStatusAcceptsAnEmptyBody(t *testing.T) {
	cases := []struct {
		name   string
		class  Class
		status int
	}{
		// Every generate_204 connectivity endpoint, and the redirects the table
		// declares rather than chases: 13 site entries answer 3xx with no body
		// at all, and the ExpectBody rule scores every one of them as a
		// blackhole.
		{name: "204 no content", class: ClassConnectivity, status: http.StatusNoContent},
		{name: "302 found, the declared redirect", class: ClassSite, status: http.StatusFound},
		{name: "301 moved permanently", class: ClassCdn, status: http.StatusMovedPermanently},
		{name: "307 temporary redirect", class: ClassSite, status: http.StatusTemporaryRedirect},
		{name: "202 accepted", class: ClassSite, status: http.StatusAccepted},
	}
	for _, tc := range cases {
		dests := stubDestinations(t, []spec{
			{name: "d", class: tc.class, h: emptyStatus(tc.status), expect: ExpectStatus, status: tc.status},
		})
		res, err := check(context.Background(), http.DefaultClient, dests, fastOptions())
		if err != nil {
			t.Fatalf("%s: check err = %v", tc.name, err)
		}
		c := res.Checks[0]
		if !c.Ok {
			t.Fatalf("%s: a %d with an empty body FAILED an ExpectStatus %d destination (%+v); the endpoints that answer this way would all read as blackholes", tc.name, tc.status, tc.status, c)
		}
		if c.StatusCode != tc.status {
			t.Errorf("%s: StatusCode = %d, want %d", tc.name, c.StatusCode, tc.status)
		}
		if c.ByteCount != 0 {
			t.Errorf("%s: ByteCount = %d, want 0", tc.name, c.ByteCount)
		}
		if c.Err != "" {
			t.Errorf("%s: Err = %q, want empty", tc.name, c.Err)
		}
		if res.OkCount != 1 || res.Total != 1 {
			t.Fatalf("%s: OkCount/Total = %d/%d, want 1/1", tc.name, res.OkCount, res.Total)
		}
	}
}

// The total-blackhole case run
// through the sampling path rather than a hand-built table, because that is
// what production runs. Every destination the draw lands on returns a bodiless
// 200 -- what a provider that terminates connections itself produces -- and
// every scored class must come back 0/n whichever destinations were drawn.
func TestCheckSamplesABlackholedProviderToZero(t *testing.T) {
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
	})
	opts := fastOptions()
	opts.Rand = rand.New(rand.NewSource(42))
	res, err := Check(context.Background(), &http.Client{Transport: rt}, opts)
	if err != nil {
		t.Fatalf("a total blackhole must be a RESULT, not an error: %v", err)
	}
	// The one contract that accepts a bare 2xx by declaration: ExpectReachable,
	// the geo-redirecting sites (hulu, microsoft), which pass any 2xx or 3xx
	// with or without a body. At 26 sites a run, about half of all runs draw
	// one, so the count below is exact rather than zero.
	byName := map[string]Destination{}
	for _, d := range destinations {
		byName[d.Name] = d
	}
	reachable := 0
	for _, c := range res.Checks {
		if byName[c.Name].Expect == ExpectReachable {
			reachable++
			if !c.Ok {
				t.Errorf("%s: ExpectReachable accepts a bare 200 by declaration, and failed it: %+v", c.Name, c)
			}
		} else if c.Ok {
			t.Errorf("%s passed a bodiless 200, the blackhole signature", c.Name)
		}
	}
	if res.OkCount != reachable {
		t.Fatalf("OkCount = %d, want only the %d ExpectReachable draw(s) (%s)", res.OkCount, reachable, res.Summary())
	}
	if res.Total != SamplePerRun() {
		t.Fatalf("Total = %d; the whole sample is what must read zero, and it must not be empty", res.Total)
	}
	for _, c := range Classes {
		s := res.ByClass[c]
		if s.Total == 0 {
			t.Errorf("class %q is absent from a sampled run; every class must be drawn from on every run or a whole fault mode goes unwatched", c)
		}
		if c != ClassSite && s.Ok != 0 {
			t.Errorf("ByClass[%s] = %d/%d; a bodiless 200 is the blackhole signature and must not pass", c, s.Ok, s.Total)
		}
	}
	if len(res.FailedNames()) != res.Total-reachable {
		t.Errorf("FailedNames listed %d of %d failures; a sampled run's failure list is the only record of what it asked for", len(res.FailedNames()), res.Total-reachable)
	}
}

// Keeps ExpectStatus from becoming a hole in
// the blackhole rule. A provider that terminates connections itself and
// synthesizes a bare status line produces a 200 with no body; if ExpectStatus
// accepted "any 2xx", every connectivity destination would pass for exactly the
// provider this package exists to catch. The match is exact, which makes these
// entries stricter than ExpectBody, not looser.
func TestExpectStatusIsExactNotAny2xx(t *testing.T) {
	cases := []struct {
		name string
		h    handler
	}{
		{name: "bare 200, no body -- the synthesized status line", h: emptyBody200()},
		{name: "200 with a body -- a portal answering instead", h: okBody("<html>sign in</html>")},
		{name: "206, still not the declared status", h: statusWithBody(http.StatusPartialContent, "partial")},
	}
	for _, tc := range cases {
		dests := stubDestinations(t, []spec{
			{name: "c204", class: ClassConnectivity, h: tc.h, expect: ExpectStatus, status: http.StatusNoContent},
		})
		res, err := check(context.Background(), http.DefaultClient, dests, fastOptions())
		if err != nil {
			t.Fatalf("%s: check err = %v", tc.name, err)
		}
		c := res.Checks[0]
		if c.Ok {
			t.Fatalf("%s: status %d passed an ExpectStatus 204 destination; ExpectStatus must be an exact match, or a synthesized status line passes the check", tc.name, c.StatusCode)
		}
		if !strings.Contains(c.Err, "want exactly") {
			t.Errorf("%s: Err = %q, want it to say the status was not the declared one", tc.name, c.Err)
		}
	}
}

// An entry that declares ExpectStatus and
// forgets the status must not silently accept anything that comes back.
// TestDestinationsTable keeps the production table free of this, and this keeps
// the runtime honest if one ever slips through.
func TestExpectStatusWithoutAStatusFails(t *testing.T) {
	dests := stubDestinations(t, []spec{{name: "broken", class: ClassConnectivity, h: okBody("anything"), expect: ExpectStatus}})
	res, err := check(context.Background(), http.DefaultClient, dests, fastOptions())
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	if res.Checks[0].Ok {
		t.Fatal("a destination declaring ExpectStatus with no Status accepted the response; a misconfigured entry must fail, not pass everything")
	}
}

// The captive-portal case. A 200
// with a body is not proof that a name was resolved: a portal, a transparent
// proxy or an interception box all return exactly that. Only an answer section
// proves resolution, which is why every DoH entry carries Verify.
func TestVerifyRejectsAnInterceptedDnsAnswer(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "a captive portal login page", body: "<html><body>Please sign in to continue</body></html>"},
		{name: "valid json that is not a dns answer", body: `{"result":"ok","message":"welcome"}`},
		{name: "a dns document with no answer section", body: `{"Status":3,"Question":[{"name":"example.com.","type":1}]}`},
		{name: "an answer section with empty data", body: `{"Status":0,"Answer":[{"name":"example.com.","type":1,"data":""}]}`},
	}
	for _, tc := range cases {
		dests := stubDestinations(t, []spec{
			{name: "doh", class: ClassDns, h: okBody(tc.body), verify: BodyCheck{Kind: BodyCheckDnsJson}},
		})
		res, err := check(context.Background(), http.DefaultClient, dests, fastOptions())
		if err != nil {
			t.Fatalf("%s: check err = %v", tc.name, err)
		}
		c := res.Checks[0]
		if c.Ok {
			t.Fatalf("%s: a 200 carrying %q passed a DoH destination; a status and some bytes are not proof a name was resolved", tc.name, tc.body)
		}
		if c.StatusCode != http.StatusOK || c.ByteCount == 0 {
			t.Errorf("%s: status=%d bytes=%d; the response DID arrive and must be recorded as such, so this is distinguishable from a blackhole", tc.name, c.StatusCode, c.ByteCount)
		}
		if !strings.Contains(c.Err, "did not verify") {
			t.Errorf("%s: Err = %q, want it to name the verification failure", tc.name, c.Err)
		}
		if res.OkCount != 0 {
			t.Errorf("%s: OkCount = %d, want 0", tc.name, res.OkCount)
		}
	}
}

// The other side of the same
// rule, and it is not hypothetical: the seven operators in the table disagree
// about the document around the answer section. dns.alidns.com returns Question
// as an object where the others return an array, and dns.adguard-dns.com omits
// Status entirely. A validator that decoded either field would reject a working
// resolver as a captive portal -- for AliDNS on every single pass.
//
// The bodies below are the shapes captured from the live endpoints on
// 2026-07-31, trimmed.
func TestVerifyDnsJsonAcceptsEveryOperatorsShape(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "google/cloudflare/nextdns/dns.sb/doh.pub: Question is an array", body: `{"Status":0,"TC":false,"RD":true,"RA":true,"Question":[{"name":"example.com.","type":1}],"Answer":[{"name":"example.com.","type":1,"TTL":300,"data":"192.0.2.10"}]}`},
		{name: "alidns: Question is an OBJECT", body: `{"Status":0,"TC":false,"RD":true,"RA":true,"Question":{"name":"example.com","type":1},"Answer":[{"name":"example.com.","TTL":127,"type":1,"data":"192.0.2.11"}]}`},
		{name: "adguard: no Status field at all", body: `{"Question":[{"name":"example.com.","type":1}],"Answer":[{"name":"example.com.","data":"192.0.2.10","TTL":101,"type":1,"class":1}]}`},
		{name: "dns.sb: extra per-record fields", body: `{"Status":0,"Answer":[{"name":"example.com.","type":1,"TTL":21,"Expires":"Fri, 31 Jul 2026 08:57:28 UTC","data":"192.0.2.10"}]}`},
	}
	for _, tc := range cases {
		if err := verifyDnsJson([]byte(tc.body)); err != nil {
			t.Fatalf("%s: verifyDnsJson rejected a real, working answer: %s\nbody: %s", tc.name, err, tc.body)
		}
	}
}

// The built-in detectportal.firefox.com entry serves "success\n" -- eight
// bytes for a seven-character word. An equality check would fail for every
// provider forever, which is noise dressed as signal.
func TestVerifyContainsToleratesTrailingWhitespace(t *testing.T) {
	if err := verifyContains([]byte("success\n"), "success"); err != nil {
		t.Fatalf("verifyContains rejected the real 8-byte body %q: %s", "success\n", err)
	}
	if err := verifyContains([]byte("<HTML><HEAD><TITLE>Success</TITLE></HEAD><BODY>Success</BODY></HTML>"), "Success"); err != nil {
		t.Fatalf("verifyContains rejected captive.apple.com's real body: %s", err)
	}
	if err := verifyContains([]byte("<html>Please sign in</html>"), "success"); err == nil {
		t.Fatal("verifyContains accepted a captive portal page")
	}
}

// The echo endpoints answer with an address and a newline
// (checkip.amazonaws.com) and may answer in v6 (icanhazip did, from the
// measuring host). A portal answers with html.
func TestVerifyIpText(t *testing.T) {
	for _, body := range []string{"203.0.113.25", "203.0.113.25\n", "2001:db8:0:2600::a\n"} {
		if err := verifyIpText([]byte(body)); err != nil {
			t.Errorf("verifyIpText(%q) = %s, want nil", body, err)
		}
	}
	for _, body := range []string{"", "<html>Please sign in</html>", "not-an-ip"} {
		if err := verifyIpText([]byte(body)); err == nil {
			t.Errorf("verifyIpText(%q) accepted a body that is not an address", body)
		}
	}
}

// The eight destinations of the dissolved
// reputation class, and the contract each keeps (GEOMAP §11.3).
var formerReputationSites = map[string]Expect{
	"akamai":         ExpectBody,
	"ecosia":         ExpectBody,
	"reddit":         ExpectBody,
	"etsy":           ExpectBody,
	"stack-overflow": ExpectStatus,
	"reuters":        ExpectBody,
	"canva":          ExpectBody,
	"epic-games":     ExpectBody,
}

// The reputation class is
// dissolved into site with its contracts unchanged -- stack-overflow keeps its
// declared 302, the rest their body rule -- and nothing in the table is filed
// under the old class name, which the server rejects inside class_results.
func TestFormerReputationSitesLoadAsSiteEntries(t *testing.T) {
	byName := map[string]Destination{}
	for _, d := range destinations {
		byName[d.Name] = d
		if d.Class == Class("reputation") {
			t.Errorf("destination %q is still filed under the dissolved reputation class", d.Name)
		}
	}
	for name, expect := range formerReputationSites {
		d, ok := byName[name]
		if !ok {
			t.Errorf("former reputation site %q is gone from the table; it was to become a site entry, not be dropped", name)
			continue
		}
		if d.Class != ClassSite {
			t.Errorf("%q is class %q, want %q", name, d.Class, ClassSite)
		}
		if d.Expect != expect {
			t.Errorf("%q expects %s, want its existing %s", name, d.Expect, expect)
		}
		if name == "stack-overflow" && d.Status != http.StatusFound {
			t.Errorf("stack-overflow declares %d, want its measured 302", d.Status)
		}
	}
	for _, c := range Classes {
		if c == Class("reputation") {
			t.Fatal("Classes still declares reputation")
		}
	}
}

// The rule reversed on purpose. Those
// sites refusing an exit used to be kept out of ok/total as a fact about a
// vendor's ip feed; now a refusal is a failed site load like any other, and a
// site a user cannot reach counts against the exit.
func TestFormerReputationSitesAreScored(t *testing.T) {
	dests := stubDestinations(t, []spec{
		{name: "dns-a", class: ClassDns, h: okBody(`{"Status":0}`)},
		{name: "conn-a", class: ClassConnectivity, h: status204(), expect: ExpectStatus, status: http.StatusNoContent},
		{name: "cdn-a", class: ClassCdn, h: okBody("/* css */")},
		{name: "site-a", class: ClassSite, h: okBody("User-agent: *")},
		{name: "akamai", class: ClassSite, h: statusWithBody(http.StatusForbidden, "Access Denied")},
		{name: "reuters", class: ClassSite, h: statusWithBody(http.StatusUnauthorized, "unauthorized")},
	})
	res, err := check(context.Background(), http.DefaultClient, dests, fastOptions())
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	if res.OkCount != 4 || res.Total != 6 {
		t.Fatalf("OkCount/Total = %d/%d, want 4/6: the two refusals are failed site loads", res.OkCount, res.Total)
	}
	if got, want := res.Summary(), "ok=4/6 dns=1/1 connectivity=1/1 cdn=1/1 site=1/3"; got != want {
		t.Fatalf("Summary = %q, want %q", got, want)
	}
	if got, want := strings.Join(res.FailedNames(), ","), "akamai,reuters"; got != want {
		t.Errorf("FailedNames = %q, want %q", got, want)
	}
	for _, c := range res.Checks {
		if c.Ok {
			continue
		}
		// Refusals are recorded as refusals -- a status and a body -- and only
		// after every try.
		if c.StatusCode == 0 || c.ByteCount == 0 {
			t.Errorf("%s: status=%d bytes=%d; a refusal must be recorded as a refusal, not as silence", c.Name, c.StatusCode, c.ByteCount)
		}
		if c.Attempts != DefaultLoadAttempts {
			t.Errorf("%s: %d attempt(s), want %d", c.Name, c.Attempts, DefaultLoadAttempts)
		}
	}
}

// The arithmetic the package documents,
// asserted rather than asserted-in-prose. It is the arithmetic of the sample:
// the full table would cost 128 KiB per attempt, which is the reason a run
// samples at all, so the assertion has to be about what a run actually spends.
// A sample size raised, or a destination added to a class with a cap above its
// neighbours', would quietly undo that.
//
// The per-class figure takes the largest cap in the class, not an assumed
// uniform one: the draw can land on any subset, so the worst case is
// sampleCount x max(MaxBytes). Assuming uniformity would make this assertion
// silently wrong the first time one entry is capped differently.
func TestWorstCaseBytesPerRunFitsTheBudget(t *testing.T) {
	// One attempt of every sampled load, and the ceiling the package promises
	// not to exceed for it. A retry reads the same cap again, so a whole run is
	// bounded by DefaultLoadAttempts times this plus the warm-up.
	const perRound = 45056 // 44 KiB

	for _, d := range destinations {
		if d.MaxBytes <= 0 {
			t.Errorf("destination %q sets no MaxBytes; it would take the %d-byte default and the budget below stops being arithmetic", d.Name, MaxBodyBytes)
		}
		if MaxBodyBytes < d.MaxBytes {
			t.Errorf("destination %q has MaxBytes = %d, above the package ceiling %d; no single entry may raise the documented budget", d.Name, d.MaxBytes, MaxBodyBytes)
		}
	}

	var worst, wholeTable int64
	for _, c := range tableClasses(destinations) {
		var largest, classTotal int64
		pool := 0
		for _, d := range destinations {
			if d.Class != c {
				continue
			}
			pool++
			classTotal += d.maxBytes()
			if largest < d.maxBytes() {
				largest = d.maxBytes()
			}
		}
		n := int64(sampleCount(destinations, c, sampleSizes))
		worst += n * largest
		wholeTable += classTotal
		t.Logf("  %-12s %2d of %3d x %4d = %6d bytes", c, n, pool, largest, n*largest)
	}
	whole := int64(DefaultLoadAttempts)*worst + maxIpEchoBytes
	t.Logf("worst case per attempt round: %d bytes (%.2f KiB), budget %d; whole run with retries and the warm-up %d (%.2f KiB); the whole table would be %d (%.2f KiB) a round",
		worst, float64(worst)/1024, perRound, whole, float64(whole)/1024, wholeTable, float64(wholeTable)/1024)

	if perRound < worst {
		t.Fatalf("worst-case body bytes per attempt round = %d, above the documented budget %d", worst, perRound)
	}
	if wholeTable <= perRound {
		t.Fatalf("the whole table costs %d bytes a round, within the %d budget; sampling is then buying nothing and the table should simply be run", wholeTable, perRound)
	}
	if limit := int64(DefaultLoadAttempts)*perRound + maxIpEchoBytes; limit < whole {
		t.Fatalf("a whole run can read %d bytes, above %d", whole, limit)
	}
}

// The other bound on a run,
// and the one that used to bind first. A run no longer has to fit in one probe
// timeout -- it spans minutes by design -- but its budget still has to fit the
// longest schedule the spacing can produce, or the last attempt of an unlucky
// chain is cut off and a load fails for the prober's arithmetic; and it has to
// stay bounded, or a swallowing provider holds its tunnel open for nothing.
func TestRunBudgetFitsTheScheduleAndStaysBounded(t *testing.T) {
	opts := Options{IpEchoUrl: "https://api.example/my-ip-info"}
	loads := SamplePerRun()
	budget := opts.RunBudget(loads)

	rounds := (loads + DefaultConcurrency - 1) / DefaultConcurrency
	longestChain := time.Duration(DefaultLoadAttempts)*DefaultPerRequestTimeout +
		time.Duration(DefaultLoadAttempts-1)*retryDelayCapFactor*DefaultLoadRetryMeanInterval
	if budget < DefaultIpEchoTimeout+longestChain {
		t.Fatalf("RunBudget(%d) = %s, shorter than the warm-up plus the longest single chain %s", loads, budget, DefaultIpEchoTimeout+longestChain)
	}
	if want := DefaultIpEchoTimeout + time.Duration(DefaultLoadAttempts*rounds)*DefaultPerRequestTimeout +
		time.Duration(DefaultLoadAttempts-1)*retryDelayCapFactor*DefaultLoadRetryMeanInterval; budget != want {
		t.Fatalf("RunBudget(%d) = %s, want %s", loads, budget, want)
	}
	if 40*time.Minute < budget {
		t.Fatalf("RunBudget(%d) = %s; a run's worst case should stay near the ~35 minutes the defaults imply", loads, budget)
	}
	t.Logf("RunBudget(%d loads) = %s at the defaults", loads, budget)

	// An explicit budget wins, and the settings it is derived from move it.
	if got := (Options{Budget: time.Minute}).budget(loads); got != time.Minute {
		t.Errorf("an explicit Budget was not used: %s", got)
	}
	if (Options{}).RunBudget(loads) <= (Options{LoadAttempts: 1}).RunBudget(loads) {
		t.Error("RunBudget does not shrink when retries are disabled")
	}
	if (Options{}).RunBudget(loads) <= (Options{LoadRetryMeanInterval: time.Minute}).RunBudget(loads) {
		t.Error("RunBudget does not follow the retry spacing")
	}
	if opts.RunBudget(loads) <= (Options{}).RunBudget(loads) {
		t.Error("RunBudget does not include the warm-up when an echo is configured")
	}
}

// The one-in-ten rule and the egress
// index read runs of at least 50 scored loads (GEOMAP §10.5, the server's
// MinScoredLoads). A run below that is a run the server does not rank on, so
// the sizes are asserted against it rather than merely recorded.
func TestSampleSizesGiveFiftyScoredLoads(t *testing.T) {
	const minScoredLoads = 50
	if got := SamplePerRun(); got < minScoredLoads {
		t.Fatalf("a run samples %d loads, below the %d the server's rule and index read", got, minScoredLoads)
	}
	want := map[Class]int{ClassDns: 6, ClassConnectivity: 8, ClassCdn: 10, ClassSite: 26}
	for c, n := range want {
		if got := sampleCount(destinations, c, sampleSizes); got != n {
			t.Errorf("class %q samples %d, want %d", c, got, n)
		}
	}
}

// A class with no declared size is
// probed whole, which is the safe direction at runtime but is not a state the
// production table should ever be in -- site alone would put 93 requests on
// every provider.
func TestSampleSizesAreDeclaredForEveryClass(t *testing.T) {
	for _, c := range tableClasses(destinations) {
		n, declared := sampleSizes[c]
		if !declared {
			t.Errorf("class %q has no sample size; it would be probed whole on every run", c)
			continue
		}
		// Three is the floor: at two, one flaky endpoint is half the class and
		// "cdn=1/2" says nothing about whether the class is failing.
		if n < 3 {
			t.Errorf("class %q samples %d; below 3 a class verdict cannot separate one flaky endpoint from a class-wide fault", c, n)
		}
		pool := 0
		for _, d := range destinations {
			if d.Class == c {
				pool++
			}
		}
		if pool <= n {
			t.Errorf("class %q samples %d from a pool of %d; the sample must be drawn from more than it takes, or it is a fixed table wearing a sample's name", c, n, pool)
		}
	}
	for c := range sampleSizes {
		found := false
		for _, tc := range tableClasses(destinations) {
			if tc == c {
				found = true
			}
		}
		if !found {
			t.Errorf("sampleSizes declares class %q, which no destination uses", c)
		}
	}
}

// The first half of the sampling
// contract: a run asks for exactly as many destinations of each class as the
// package says it will, they are really from that class, and none is drawn
// twice.
func TestSampleDrawsTheConfiguredCountPerClass(t *testing.T) {
	got := sampleDestinations(destinations, sampleSizes, rand.New(rand.NewSource(1)))

	perClass := map[Class]int{}
	seen := map[string]bool{}
	for _, d := range got {
		perClass[d.Class]++
		if seen[d.Name] {
			t.Errorf("%q was drawn twice in one sample", d.Name)
		}
		seen[d.Name] = true
	}
	for _, c := range tableClasses(destinations) {
		if want := sampleCount(destinations, c, sampleSizes); perClass[c] != want {
			t.Errorf("sample drew %d of class %q, want %d", perClass[c], c, want)
		}
	}
	if len(got) != SamplePerRun() {
		t.Errorf("sample is %d destinations, SamplePerRun() says %d", len(got), SamplePerRun())
	}

	// Table order, so the log line and FailedNames stay diffable between runs
	// whatever the draw was.
	pos := map[string]int{}
	for i, d := range destinations {
		pos[d.Name] = i
	}
	for i := 1; i < len(got); i++ {
		if pos[got[i].Name] <= pos[got[i-1].Name] {
			t.Fatalf("sample is not in table order: %q (%d) before %q (%d)", got[i-1].Name, pos[got[i-1].Name], got[i].Name, pos[got[i].Name])
		}
	}
}

// The draw must be a function of the
// generator it is handed and nothing else. Without this a test that asserts on
// a sample is asserting on the weather -- and the property is real, not just
// convenient: it is what proves the class ordering inside sampleDestinations
// does not come from map iteration, which would vary run to run under an
// identical seed.
//
// Each run gets a freshly seeded generator, because a *rand.Rand is stateful:
// handing the same one to two runs is a different experiment (and would fail).
func TestSampleIsReproducibleForASeed(t *testing.T) {
	first := names(sampleDestinations(destinations, sampleSizes, rand.New(rand.NewSource(7))))
	for i := 0; i < 20; i++ {
		again := names(sampleDestinations(destinations, sampleSizes, rand.New(rand.NewSource(7))))
		if again != first {
			t.Fatalf("seed 7 drew a different sample on iteration %d:\n first: %s\n again: %s", i, first, again)
		}
	}

	// ...and through Check, which is the path production takes.
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("no network in tests")
	})
	client := &http.Client{Transport: rt}
	run := func(seed int64) string {
		opts := fastOptions()
		opts.Rand = rand.New(rand.NewSource(seed))
		res, err := Check(context.Background(), client, opts)
		if err != nil {
			t.Fatalf("Check err = %v", err)
		}
		var out []string
		for _, c := range res.Checks {
			out = append(out, c.Name)
		}
		return strings.Join(out, ",")
	}
	if a, b := run(11), run(11); a != b {
		t.Fatalf("Check with an identical seed drew different samples:\n %s\n %s", a, b)
	}
}

// The other half, and it is what "sampling"
// actually means: a fixed table, or a fixed rotation offset, would pass every
// other test in this file. It is asserted on the whole sample rather than one
// class on purpose -- dns draws 6 of 7, which is only 7 possible subsets, so
// two arbitrary seeds collide on that class often enough to flake.
//
// The property is not cosmetic. A provider that knows which destinations it
// will be asked for can whitelist them and blackhole everything else; the
// per-run draw is what takes that away, and it is only taken away if the draw
// really varies.
func TestSampleDiffersBetweenSeeds(t *testing.T) {
	first := names(sampleDestinations(destinations, sampleSizes, rand.New(rand.NewSource(1))))
	differed := 0
	for seed := int64(2); seed <= 11; seed++ {
		if names(sampleDestinations(destinations, sampleSizes, rand.New(rand.NewSource(seed)))) != first {
			differed++
		}
	}
	if differed < 9 {
		t.Fatalf("only %d of 10 other seeds drew a different sample; the draw is not random, and a provider that can predict the destinations can whitelist them", differed)
	}

	// The widest class on its own, where a collision is vanishingly unlikely
	// (26 of 100), so this stays a statement about sampling rather than about
	// the classes happening to differ somewhere.
	site := func(seed int64) string {
		var out []string
		for _, d := range sampleDestinations(destinations, sampleSizes, rand.New(rand.NewSource(seed))) {
			if d.Class == ClassSite {
				out = append(out, d.Name)
			}
		}
		return strings.Join(out, ",")
	}
	if site(1) == site(2) {
		t.Fatalf("two seeds drew the identical 26-of-100 site sample (%s); that is not a draw", site(1))
	}
}

// A class smaller than its declared
// size, or with no declared size at all, is probed whole rather than silently
// skipped. The cost of the safe direction is visible bytes; the cost of the
// other one is a class nobody notices is gone.
func TestSampleTakesAWholeClassItCannotFill(t *testing.T) {
	small := []Destination{
		{Name: "a", Class: ClassDns}, {Name: "b", Class: ClassDns},
		{Name: "c", Class: Class("undeclared")}, {Name: "d", Class: Class("undeclared")},
	}
	got := sampleDestinations(small, map[Class]int{ClassDns: 5}, rand.New(rand.NewSource(3)))
	if len(got) != len(small) {
		t.Fatalf("sample took %d of %d; a class it cannot fill, and a class with no declared size, must both be taken whole", len(got), len(small))
	}
}

// Renders a sample for comparison, in order.
func names(dests []Destination) string {
	var out []string
	for _, d := range dests {
		out = append(out, d.Name)
	}
	return strings.Join(out, ",")
}

// Guards the table against the two
// ways an entry can be self-contradictory: an ExpectStatus with no status (it
// would accept nothing and fail every pass), and a status declared on an
// ExpectBody entry (it would be silently ignored, so the author's intent would
// not be what runs).
func TestSuccessContractsAreDeclaredCoherently(t *testing.T) {
	for _, d := range destinations {
		switch d.Expect {
		case ExpectStatus:
			if d.Status < 200 || 400 <= d.Status {
				t.Errorf("destination %q declares ExpectStatus with Status = %d; it must name the non-error status it actually answers", d.Name, d.Status)
			}
		case ExpectReachable:
			// Any 2xx/3xx, so a declared Status would be ignored -- same trap as
			// ExpectBody. Confined to ClassSite on purpose: it is the weakest
			// contract, and letting dns/cdn/connectivity take it would put a
			// hole in the classes where a body is the actual evidence.
			if d.Status != 0 {
				t.Errorf("destination %q is ExpectReachable but declares Status = %d, which is ignored; drop it", d.Name, d.Status)
			}
			if d.Class != ClassSite {
				t.Errorf("destination %q is ExpectReachable in class %q; that contract is confined to %q", d.Name, d.Class, ClassSite)
			}
		case ExpectBody:
			if d.Status != 0 {
				t.Errorf("destination %q is ExpectBody but declares Status = %d, which is ignored; either drop it or declare ExpectStatus", d.Name, d.Status)
			}
		default:
			t.Errorf("destination %q has an unknown Expect %d", d.Name, d.Expect)
		}
	}
}

// A 200 with bytes from a DoH
// endpoint is not evidence of resolution, and this is the class where that
// distinction matters most -- name resolution is the shared precondition for
// every other destination in the table.
func TestEveryDnsDestinationVerifiesItsAnswer(t *testing.T) {
	for _, d := range destinations {
		if d.Class != ClassDns {
			continue
		}
		if d.Verify.Kind != BodyCheckDnsJson {
			t.Errorf("dns destination %q has body check %q; a captive portal returning 200 with a body would pass it", d.Name, d.Verify.Kind)
			continue
		}
		if err := d.Verify.check([]byte(`{"Status":0,"Answer":[{"name":"example.com.","type":1,"data":"192.0.2.1"}]}`)); err != nil {
			t.Errorf("dns destination %q rejects a well-formed answer: %s", d.Name, err)
		}
		if err := d.Verify.check([]byte(`<html>sign in</html>`)); err == nil {
			t.Errorf("dns destination %q accepts a portal page", d.Name)
		}
	}
	// Every DoH url must ask a question. A url without one answers 400 (or an
	// empty answer) for every provider forever.
	for _, d := range destinations {
		if d.Class == ClassDns && !strings.Contains(d.Url, "name=") {
			t.Errorf("dns destination %q carries no name= query: %s", d.Name, d.Url)
		}
	}
}

// This class is the cheapest useful
// signal in the table, and it is only cheap if it stays that way.
func TestConnectivityClassIsCheapAndBroad(t *testing.T) {
	var n, status204Count int
	for _, d := range destinations {
		if d.Class != ClassConnectivity {
			continue
		}
		n++
		if d.Expect == ExpectStatus {
			status204Count++
		}
		if 512 < d.MaxBytes {
			t.Errorf("connectivity destination %q caps at %d bytes; this class answers in tens of bytes and a large cap gives that up", d.Name, d.MaxBytes)
		}
		if d.Expect == ExpectBody && d.Verify.Kind == "" {
			t.Errorf("connectivity destination %q reads a body but does not verify it; these endpoints exist to detect captive portals, which answer 200 with a page", d.Name)
		}
	}
	if n < 5 {
		t.Errorf("the connectivity class has %d destination(s), want a broad spread of operators", n)
	}
	if status204Count == 0 {
		t.Error("no connectivity destination uses ExpectStatus; the generate_204 endpoints are the reason the contract exists")
	}
}

// AllDestinations is an operator diagnostic, so the property that matters is
// that it really does run the whole table -- a run that silently still sampled
// would report a partial picture as if it were complete. The requests fail
// here (no network in tests); only how many were attempted matters.
func TestAllDestinationsRunsEveryDestination(t *testing.T) {
	rt := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("no network in tests")
	})

	perRequest := 50 * time.Millisecond
	concurrency := AllConcurrency
	budget := time.Duration(RoundsForAllDestinations()) * perRequest
	res, err := Check(context.Background(), &http.Client{Transport: rt}, Options{
		AllDestinations:   true,
		Budget:            budget,
		Concurrency:       concurrency,
		PerRequestTimeout: perRequest,
		Sleep:             noSleep,
	})
	if err != nil {
		t.Fatalf("Check err = %v", err)
	}
	if len(res.Checks) != len(destinations) {
		t.Errorf("ran %d destinations, want the whole table of %d", len(res.Checks), len(destinations))
	}
	if len(res.Checks) <= SamplePerRun() {
		t.Errorf("AllDestinations ran %d, no more than a sample (%d) -- it is still sampling", len(res.Checks), SamplePerRun())
	}
}

// ExpectReachable must accept the geo-redirect it exists for, and must still
// reject a refusal -- otherwise it would be a hole in the blackhole rule rather
// than a contract for sites whose status depends on the exit country.
func TestExpectReachableAcceptsRedirectsButNotRefusals(t *testing.T) {
	d := Destination{Name: "x", Class: ClassSite, Expect: ExpectReachable}
	for _, tc := range []struct {
		status int
		ok     bool
		why    string
	}{
		{status: 200, ok: true, why: "2xx with body"},
		{status: 204, ok: true, why: "2xx empty"},
		{status: 302, ok: true, why: "the measured microsoft case: locale redirect, empty body"},
		{status: 301, ok: true, why: "permanent redirect"},
		{status: 403, ok: false, why: "a refusal must stay a failure"},
		{status: 404, ok: false, why: "not found is a failure"},
		{status: 500, ok: false, why: "server error is a failure"},
	} {
		err := d.judge(tc.status, nil)
		if tc.ok && err != nil {
			t.Errorf("status %d (%s): got error %v, want success", tc.status, tc.why, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("status %d (%s): got success, want failure", tc.status, tc.why)
		}
	}
}
