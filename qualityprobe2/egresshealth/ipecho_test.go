package egresshealth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests of the warm-up: parsing the /ip answer, its place before every load,
// its failures and its own timeout.

// The shape the operator's GET /my-ip-info serves.
const echoAnswer = `{"info":{"ip":"203.0.113.7","location":{"coordinates":{"lat":51.5967,"lon":-0.1593},"city":"London","region":"England","country":{"code":"gb","name":"United Kingdom"},"continent":{"code":"eu","name":"Europe"},"timezone":"Europe/London"}},"connected_to_network":true}`

// The exit address is read from info.ip and nowhere else, in
// canonical form, and anything that does not carry one is refused -- a portal
// or a misrouted request answers 200 with a body too, and a wrong address is
// worse than none, since the server would place the provider there.
func TestParseIpEcho(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
	}{
		{body: echoAnswer, want: "203.0.113.7"},
		{body: `{"info":{"ip":"203.0.113.7"}}`, want: "203.0.113.7"},
		{body: `{"info":{"ip":" 2001:DB8::1 "},"connected_to_network":false}`, want: "2001:db8::1"},
		{body: `{"info":{"ip":"198.51.100.20","location":null},"extra":{"ignored":true}}`, want: "198.51.100.20"},
	} {
		got, err := parseIpEcho([]byte(tc.body))
		if err != nil || got != tc.want {
			t.Errorf("parseIpEcho(%s) = (%q, %v), want %q", tc.body, got, err, tc.want)
		}
	}

	for _, body := range []string{
		"",
		"<html><body>Please sign in</body></html>",
		`{"ip":"203.0.113.7"}`,
		`{"info":{}}`,
		`{"info":{"ip":""}}`,
		`{"info":{"ip":"not-an-address"}}`,
		`{"info":{"ip":"203.0.113.7:443"}}`,
		`{"info":"203.0.113.7"}`,
		`{"info":{"ip":"203.0.113.7"`,
	} {
		if got, err := parseIpEcho([]byte(body)); err == nil {
			t.Errorf("parseIpEcho(%q) = %q, want an error: it carries no parseable info.ip", body, got)
		}
	}
}

// Serves the /ip echo and one load, and records the order requests
// arrive in and the headers the echo was asked with.
type echoServer struct {
	stateLock sync.Mutex
	order     []string
	echoHead  http.Header
	*httptest.Server
}

// Starts an echo server whose /ip answer is echo, closed when the test ends.
func newEchoServer(t *testing.T, echo http.HandlerFunc) *echoServer {
	t.Helper()
	s := &echoServer{}
	mux := http.NewServeMux()
	mux.HandleFunc(IpEchoPath, func(w http.ResponseWriter, r *http.Request) {
		s.stateLock.Lock()
		s.order = append(s.order, "echo")
		s.echoHead = r.Header.Clone()
		s.stateLock.Unlock()
		echo(w, r)
	})
	mux.HandleFunc("/load", func(w http.ResponseWriter, r *http.Request) {
		s.stateLock.Lock()
		s.order = append(s.order, "load")
		s.stateLock.Unlock()
		_, _ = w.Write([]byte("User-agent: *"))
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// Returns the order requests arrived in so far.
func (self *echoServer) requests() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.order...)
}

// The echo is the first fetch of
// the run, before any scored load starts its clock, it is not itself scored,
// and its answer is the result's exit address, stamped on the run's clock.
func TestWarmUpPrecedesEveryLoadAndCarriesTheExit(t *testing.T) {
	srv := newEchoServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(echoAnswer))
	})
	clock := newFakeClock()
	opts := clockedOptions(clock)
	opts.IpEchoUrl = srv.URL + IpEchoPath
	dests := []Destination{
		{Name: "site-a", Class: ClassSite, Url: srv.URL + "/load"},
		{Name: "site-b", Class: ClassSite, Url: srv.URL + "/load"},
		{Name: "site-c", Class: ClassSite, Url: srv.URL + "/load"},
	}

	res, err := check(context.Background(), http.DefaultClient, dests, opts)
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	if res.ExitIp != "203.0.113.7" || res.IpEchoErr != "" {
		t.Fatalf("ExitIp = %q (err %q), want 203.0.113.7", res.ExitIp, res.IpEchoErr)
	}
	if !res.ExitObservedAt.Equal(clock.Now()) {
		t.Errorf("ExitObservedAt = %s, want the run's clock %s", res.ExitObservedAt, clock.Now())
	}
	got := srv.requests()
	if len(got) != 4 || got[0] != "echo" {
		t.Fatalf("requests arrived as %v, want the echo first and then the three loads", got)
	}
	if res.Total != 3 || res.OkCount != 3 {
		t.Errorf("OkCount/Total = %d/%d, want 3/3: the warm-up is never scored", res.OkCount, res.Total)
	}
	for _, c := range res.Checks {
		if strings.Contains(c.Name, "echo") {
			t.Errorf("the warm-up appears among the checks as %q", c.Name)
		}
	}
	// The echo carries the profile like every request, asking for json.
	srv.stateLock.Lock()
	head := srv.echoHead
	srv.stateLock.Unlock()
	if head.Get("User-Agent") != DefaultRequestProfile().UserAgent || head.Get("Accept") != "application/json" {
		t.Errorf("the echo was asked with User-Agent %q and Accept %q, want the profile's agent and json", head.Get("User-Agent"), head.Get("Accept"))
	}
	if _, present := head["Range"]; present {
		t.Error("the echo carried a Range header")
	}
}

// An echo that does not answer with an
// address costs the run its exit address and nothing else. The loads still
// run and are scored, and the result says why there is no exit.
func TestMalformedEchoLeavesTheRunScored(t *testing.T) {
	for name, echo := range map[string]http.HandlerFunc{
		"a portal page": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("<html><body>Please sign in</body></html>"))
		},
		"json with no address": func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"info":{"ip":""}}`))
		},
		"a 404": func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		},
		"a 502 carrying an address": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(echoAnswer))
		},
	} {
		srv := newEchoServer(t, echo)
		opts := fastOptions()
		opts.IpEchoUrl = srv.URL + IpEchoPath
		dests := []Destination{{Name: "site-a", Class: ClassSite, Url: srv.URL + "/load"}}

		res, err := check(context.Background(), http.DefaultClient, dests, opts)
		if err != nil {
			t.Fatalf("%s: a malformed echo stopped the run: %v", name, err)
		}
		if res.ExitIp != "" {
			t.Errorf("%s: ExitIp = %q from a malformed echo; a wrong address is worse than none", name, res.ExitIp)
		}
		if res.IpEchoErr == "" {
			t.Errorf("%s: IpEchoErr is empty; the result must say why there is no exit address", name)
		}
		if res.OkCount != 1 || res.Total != 1 {
			t.Errorf("%s: OkCount/Total = %d/%d, want the load still scored 1/1", name, res.OkCount, res.Total)
		}
		if res.TlsAuthenticationFailure {
			t.Errorf("%s: an ordinary echo failure was promoted to a TLS-authentication failure", name)
		}
		if !strings.Contains(res.Summary(), "exit=unobserved") {
			t.Errorf("%s: Summary = %q, want exit=unobserved", name, res.Summary())
		}
	}
}

// The operator's own api host
// presenting an identity that does not authenticate is interception of the
// one host whose answer places the provider, and it is the same hard signal a
// forged destination is.
func TestEchoTlsFailureIsATlsAuthenticationFailure(t *testing.T) {
	intercepted := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(echoAnswer))
	}))
	defer intercepted.Close()
	good := newEchoServer(t, func(w http.ResponseWriter, r *http.Request) {})

	opts := fastOptions()
	opts.IpEchoUrl = intercepted.URL + IpEchoPath
	res, err := check(context.Background(), http.DefaultClient, []Destination{{Name: "site-a", Class: ClassSite, Url: good.URL + "/load"}}, opts)
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	if res.ExitIp != "" || !res.TlsAuthenticationFailure {
		t.Fatalf("ExitIp = %q tls=%t, want no address and the hard TLS signal", res.ExitIp, res.TlsAuthenticationFailure)
	}
	if !strings.Contains(res.Summary(), "tls_authentication_failure=true") {
		t.Errorf("Summary = %q, want the TLS failure visible", res.Summary())
	}
}

// The echo is bounded by IpEchoTimeout, not by a load's per-request timeout
// -- it is the one fetch sized for the cold start. The deadline the echo
// request carries is read at the client's transport, so the bound is observed
// directly rather than inferred from a handler that sleeps past one timeout
// and not the other: an echo that answers carries its own, longer deadline,
// and one that never answers ends at that deadline and at no other.
func TestWarmUpHasItsOwnTimeout(t *testing.T) {
	var stateLock sync.Mutex
	var echoRemaining []time.Duration
	client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == IpEchoPath {
			remaining := time.Duration(-1)
			if deadline, ok := r.Context().Deadline(); ok {
				remaining = time.Until(deadline)
			}
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				echoRemaining = append(echoRemaining, remaining)
			}()
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	lastEchoRemaining := func() time.Duration {
		stateLock.Lock()
		defer stateLock.Unlock()
		if len(echoRemaining) == 0 {
			t.Fatal("the echo was never requested")
		}
		return echoRemaining[len(echoRemaining)-1]
	}

	answers := newEchoServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(echoAnswer))
	})
	opts := fastOptions() // 200ms per load attempt, a 5s run budget
	opts.IpEchoUrl = answers.URL + IpEchoPath
	opts.IpEchoTimeout = 2 * time.Second
	res, err := check(context.Background(), client, []Destination{{Name: "site-a", Class: ClassSite, Url: answers.URL + "/load"}}, opts)
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	if res.ExitIp != "203.0.113.7" {
		t.Fatalf("ExitIp = %q (err %q), want the echo's address", res.ExitIp, res.IpEchoErr)
	}
	if remaining := lastEchoRemaining(); remaining <= opts.PerRequestTimeout || opts.IpEchoTimeout < remaining {
		t.Fatalf("the echo was sent with %s left, want its own timeout (%s), longer than a load's %s", remaining, opts.IpEchoTimeout, opts.PerRequestTimeout)
	}

	// An echo that never answers is cut off by its own timeout: the handler
	// returns only once the client has given up.
	neverAnswers := newEchoServer(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	opts.IpEchoUrl = neverAnswers.URL + IpEchoPath
	opts.IpEchoTimeout = 50 * time.Millisecond
	res, err = check(context.Background(), client, []Destination{{Name: "site-a", Class: ClassSite, Url: neverAnswers.URL + "/load"}}, opts)
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	if res.ExitIp != "" || res.IpEchoErr == "" {
		t.Fatalf("ExitIp = %q (err %q), want the echo cut off", res.ExitIp, res.IpEchoErr)
	}
	if remaining := lastEchoRemaining(); remaining < 0 || opts.IpEchoTimeout < remaining {
		t.Fatalf("the echo was sent with %s left, want at most its own %s", remaining, opts.IpEchoTimeout)
	}
	if res.OkCount != 1 || res.Total != 1 {
		t.Errorf("OkCount/Total = %d/%d, want the load still scored after the echo was cut off", res.OkCount, res.Total)
	}
}
