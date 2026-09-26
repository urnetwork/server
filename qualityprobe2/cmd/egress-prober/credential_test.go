package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/operator-proxy/ingest"
)

// ---------------------------------------------------------------------------
// fetchByJwtIfEmpty: the precedence and the wait.
// ---------------------------------------------------------------------------

type credentialResult struct {
	cred *ingest.ProberCredential
	err  error
}

// stubCredentialFetcher hands back one result per call, repeating the last one
// forever, and counts the calls. The count is the assertion that matters in
// most of these tests: whether the server was asked at all.
type stubCredentialFetcher struct {
	results []credentialResult
	calls   int
}

func (s *stubCredentialFetcher) ProberCredential(ctx context.Context) (*ingest.ProberCredential, error) {
	s.calls++
	if len(s.results) == 0 {
		return nil, errors.New("stub: no result configured")
	}
	r := s.results[0]
	if 1 < len(s.results) {
		s.results = s.results[1:]
	}
	return r.cred, r.err
}

func okCredential(jwt string) credentialResult {
	return credentialResult{cred: &ingest.ProberCredential{ByClientJwt: jwt, ClientId: "019f8835-158d-6fd8-e9dd-fd0e4c6d6792"}}
}

// TestFetchByJwtIfEmptyLeavesAnExplicitJwtAlone is the regression test for the
// existing deployment, which supplies UR_PROBER_BY_JWT today.
//
// The assertion is on the CALL COUNT, not only on the returned value. A
// fetch-then-prefer-the-explicit-one implementation would leave the right
// string in place and still be broken: against a server whose bootstrap task
// has not run, it would sit in the 404 wait forever for a prober that never
// needed a credential at all -- turning a working deployment into one that
// never starts. Asserting the value alone would not catch that.
func TestFetchByJwtIfEmptyLeavesAnExplicitJwtAlone(t *testing.T) {
	f := &stubCredentialFetcher{results: []credentialResult{okCredential("fetched-jwt")}}
	byJwt := "explicitly-supplied-jwt"

	if err := fetchByJwtIfEmpty(context.Background(), &byJwt, f, time.Millisecond, 2*time.Millisecond); err != nil {
		t.Fatalf("fetchByJwtIfEmpty err = %v", err)
	}
	if byJwt != "explicitly-supplied-jwt" {
		t.Errorf("byJwt = %q, want the explicitly supplied jwt to survive untouched", byJwt)
	}
	if f.calls != 0 {
		t.Fatalf("the server was asked for a credential %d time(s) even though a jwt was supplied; startup would then depend on an endpoint this deployment does not need", f.calls)
	}
}

func TestFetchByJwtIfEmptyFetchesWhenEmpty(t *testing.T) {
	f := &stubCredentialFetcher{results: []credentialResult{okCredential("fetched-jwt")}}
	byJwt := ""

	if err := fetchByJwtIfEmpty(context.Background(), &byJwt, f, time.Millisecond, 2*time.Millisecond); err != nil {
		t.Fatalf("fetchByJwtIfEmpty err = %v", err)
	}
	if byJwt != "fetched-jwt" {
		t.Errorf("byJwt = %q, want the jwt the server handed over", byJwt)
	}
	if f.calls != 1 {
		t.Errorf("calls = %d, want exactly 1", f.calls)
	}
}

// TestFetchByJwtIfEmptyKeepsPollingUntilTheCredentialExists: the bootstrap task
// runs immediately and then every 6h, so a prober started alongside a fresh deployment arrives
// before its credential does. That must be a wait, not an exit.
func TestFetchByJwtIfEmptyKeepsPollingUntilTheCredentialExists(t *testing.T) {
	f := &stubCredentialFetcher{results: []credentialResult{
		{err: ingest.ErrCredentialNotReady},
		{err: ingest.ErrCredentialNotReady},
		okCredential("fetched-after-the-wait"),
	}}
	byJwt := ""

	if err := fetchByJwtIfEmpty(context.Background(), &byJwt, f, time.Millisecond, 2*time.Millisecond); err != nil {
		t.Fatalf("fetchByJwtIfEmpty err = %v; a 404 means \"not yet\", and exiting on it would crash-loop a prober started before the bootstrap task", err)
	}
	if byJwt != "fetched-after-the-wait" {
		t.Errorf("byJwt = %q, want the credential that appeared on the third ask", byJwt)
	}
	if f.calls != 3 {
		t.Errorf("calls = %d, want 3 (two 404s then the credential)", f.calls)
	}
}

// TestFetchByJwtIfEmptyStopsOnUnauthorized: a wrong operator secret is a
// broken deployment, and it will be just as wrong on the thousandth ask. It
// must surface once, immediately -- the posture blackhole.go already takes on
// the same error.
func TestFetchByJwtIfEmptyStopsOnUnauthorized(t *testing.T) {
	f := &stubCredentialFetcher{results: []credentialResult{{err: ingest.ErrUnauthorized}}}
	byJwt := ""

	err := fetchByJwtIfEmpty(context.Background(), &byJwt, f, time.Millisecond, 2*time.Millisecond)
	if err == nil {
		t.Fatal("fetchByJwtIfEmpty returned nil on a 401; a rejected operator secret must be loud")
	}
	if !errors.Is(err, ingest.ErrUnauthorized) {
		t.Errorf("err = %v, want it to wrap ingest.ErrUnauthorized", err)
	}
	if !strings.Contains(err.Error(), "-operator-secret") {
		t.Errorf("err = %v, want it to name -operator-secret so the operator knows what to fix", err)
	}
	if f.calls != 1 {
		t.Errorf("calls = %d, want exactly 1: a rejected secret must never be retried", f.calls)
	}
	if byJwt != "" {
		t.Errorf("byJwt = %q, want it left empty", byJwt)
	}
}

// TestFetchByJwtIfEmptyRetriesEverythingElse: a 500 or an unreachable api is
// transient. The prober may well come up before the server does.
func TestFetchByJwtIfEmptyRetriesEverythingElse(t *testing.T) {
	f := &stubCredentialFetcher{results: []credentialResult{
		{err: ingest.ErrCredentialUnavailable},
		okCredential("fetched-after-the-blip"),
	}}
	byJwt := ""

	if err := fetchByJwtIfEmpty(context.Background(), &byJwt, f, time.Millisecond, 2*time.Millisecond); err != nil {
		t.Fatalf("fetchByJwtIfEmpty err = %v; a transient failure must be retried", err)
	}
	if byJwt != "fetched-after-the-blip" {
		t.Errorf("byJwt = %q, want the credential fetched after the retry", byJwt)
	}
	if f.calls != 2 {
		t.Errorf("calls = %d, want 2", f.calls)
	}
}

// TestFetchByJwtIfEmptyStopsWhenTheContextEnds: the wait is unbounded in time,
// so ctx is the only thing that ends it. A loop watching only its timer would
// swallow SIGTERM for the length of a backoff and, worse, could never be shut
// down while the server keeps answering 404 -- the same
// interrupt-during-a-wait shape this codebase already fixed once.
func TestFetchByJwtIfEmptyStopsWhenTheContextEnds(t *testing.T) {
	f := &stubCredentialFetcher{results: []credentialResult{{err: ingest.ErrCredentialNotReady}}}
	byJwt := ""

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := fetchByJwtIfEmpty(ctx, &byJwt, f, time.Hour, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled so an interrupted wait is not reported as a broken deployment", err)
	}
	if 1 < f.calls {
		t.Errorf("calls = %d, want the loop to stop at the first cancellation check", f.calls)
	}
}

// TestNextBackoffDoublesAndCaps pins the schedule without sleeping through it.
// The cap is the "bounded" half of bounded backoff: without it a prober that
// waited through a full 6h bootstrap cycle would end up asking once a day.
func TestNextBackoffDoublesAndCaps(t *testing.T) {
	max := 5 * time.Minute
	got := []time.Duration{}
	cur := 30 * time.Second
	for i := 0; i < 6; i++ {
		got = append(got, cur)
		cur = nextBackoff(cur, max)
	}
	want := []time.Duration{
		30 * time.Second,
		time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		5 * time.Minute, // capped, not 8m
		5 * time.Minute,
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backoff schedule = %v, want %v", got, want)
		}
	}
	if n := nextBackoff(10*time.Minute, max); n != max {
		t.Errorf("nextBackoff(10m, 5m) = %s, want the cap %s", n, max)
	}
}

// The constants the binary actually runs with have to satisfy the same
// property the loop's own guard assumes: a positive interval that never
// exceeds the cap. A zero here would be a busy loop against the server.
func TestCredentialPollConstantsAreSane(t *testing.T) {
	if credentialPollInitial <= 0 {
		t.Errorf("credentialPollInitial = %s, must be positive or the poll becomes a busy loop", credentialPollInitial)
	}
	if credentialPollMax < credentialPollInitial {
		t.Errorf("credentialPollMax %s < credentialPollInitial %s", credentialPollMax, credentialPollInitial)
	}
}

// ---------------------------------------------------------------------------
// Startup, end to end: the fetched jwt goes through the same checks.
// ---------------------------------------------------------------------------

// credentialStub serves the prober-credential endpoint and counts what was
// asked for. Everything else 404s, which for the pin endpoint means startup
// stops there -- the marker these tests use for "the prober got past the
// credential stage".
type credentialStub struct {
	mu              sync.Mutex
	credentialCalls int
	pinCalls        int
	status          int
	body            string
}

func (s *credentialStub) counts() (credential int, pin int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credentialCalls, s.pinCalls
}

func (s *credentialStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		switch r.URL.Path {
		case "/network/prober-credential":
			s.credentialCalls++
		case "/network/geolocation-source-pins":
			s.pinCalls++
		}
		s.mu.Unlock()

		if r.URL.Path != "/network/prober-credential" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(s.body))
	}))
}

// runProberWithoutJwt runs the built binary with NO UR_PROBER_BY_JWT.
//
// It strips the variable from the inherited environment rather than merely not
// setting it: a developer or CI runner with UR_PROBER_BY_JWT exported would
// otherwise silently supply the very thing these tests exist to prove is
// fetched, and every one of them would pass without exercising anything.
func runProberWithoutJwt(t *testing.T, args ...string) (string, int) {
	t.Helper()
	// Insurance, not a deadline the tests rely on: every stub below answers
	// immediately, so a run that hangs means the fetch was reached when it
	// should not have been. Killing it turns that into a failure rather than a
	// 20-minute package timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, buildProber(t), args...)
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "UR_PROBER_BY_JWT=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "UR_OPERATOR_SECRET="+testOperatorSecret)

	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exitErr):
		return string(out), exitErr.ExitCode()
	default:
		t.Fatalf("running the prober: %s", err)
		return "", -1
	}
}

// TestProberStartsWithNoJwtAndFetchesOne is the end-to-end proof of the
// feature: with UR_PROBER_BY_JWT absent the process no longer refuses to
// start, it asks the server instead, and the credential it gets back carries
// it into the rest of startup.
//
// "Carries it into the rest of startup" is asserted as the pin fetch having
// been reached, because that is the next thing a running prober does. Without
// it the test would pass against an implementation that fetched the jwt and
// then dropped it.
func TestProberStartsWithNoJwtAndFetchesOne(t *testing.T) {
	jwt := testByJwt(t)
	stub := &credentialStub{
		status: http.StatusOK,
		body:   `{"by_client_jwt":"` + jwt + `","client_id":"019f8835-158d-6fd8-e9dd-fd0e4c6d6792"}`,
	}
	srv := stub.server(t)
	defer srv.Close()

	out, code := runProberWithoutJwt(t,
		"-api-url", srv.URL,
		"-platform-url", "ws://127.0.0.1:1",
		"-interval", "0",
		"-skip-confinement-check",
		"-skip-bandwidth",
	)

	if strings.Contains(out, "missing required flag") {
		t.Fatalf("the prober still refuses to start without -by-jwt; the whole point is that an empty one is fetched.\n--- output ---\n%s", out)
	}
	credentialCalls, pinCalls := stub.counts()
	if credentialCalls != 1 {
		t.Errorf("the credential endpoint was called %d time(s), want exactly 1.\n--- output ---\n%s", credentialCalls, out)
	}
	if pinCalls == 0 {
		t.Errorf("startup never reached the pin fetch, so the fetched jwt was not actually carried forward.\n--- output ---\n%s", out)
	}
	if !strings.Contains(out, "019f8835-158d-6fd8-e9dd-fd0e4c6d6792") {
		t.Errorf("the prober did not report which client id it got; an operator has to be able to match it against the server's record.\n--- output ---\n%s", out)
	}
	// The jwt is a credential and this output is journald's. The client id is
	// logged instead, on purpose.
	if strings.Contains(out, jwt) {
		t.Errorf("the fetched jwt was printed verbatim.\n--- output ---\n%s", out)
	}
	// The stub has no pin endpoint, so startup stops there -- the existing
	// fail-closed behaviour, unchanged by this feature.
	if code == 0 {
		t.Errorf("exited 0 with no pin set.\n--- output ---\n%s", out)
	}
	assertNoSecrets(t, "the credential fetch", out)
}

// TestProberRunsAFetchedJwtThroughTheStartupCheck is requirement 4: a
// credential the server hands over is not trusted because the server handed it
// over. It goes through parseByJwtClientId exactly as a hand-placed one does,
// so a token this process cannot use stops it here, loudly, instead of
// becoming an outage that looks like a healthy prober doing nothing.
//
// The assertion that the pin endpoint was never reached is what proves the
// check runs BEFORE the rest of startup rather than somewhere after it.
func TestProberRunsAFetchedJwtThroughTheStartupCheck(t *testing.T) {
	stub := &credentialStub{
		status: http.StatusOK,
		body:   `{"by_client_jwt":"this-is-not-a-jwt","client_id":"019f8835-158d-6fd8-e9dd-fd0e4c6d6792"}`,
	}
	srv := stub.server(t)
	defer srv.Close()

	out, code := runProberWithoutJwt(t,
		"-api-url", srv.URL,
		"-platform-url", "ws://127.0.0.1:1",
		"-interval", "0",
		"-skip-confinement-check",
		"-skip-bandwidth",
	)

	if code == 0 {
		t.Errorf("exited 0 with a fetched jwt it cannot parse; a credential the server serves but this process cannot use must fail loudly.\n--- output ---\n%s", out)
	}
	if !strings.Contains(out, "parse by-jwt client id") {
		t.Errorf("the fetched jwt did not go through the same startup credential check a supplied one does.\n--- output ---\n%s", out)
	}
	credentialCalls, pinCalls := stub.counts()
	if credentialCalls != 1 {
		t.Errorf("the credential endpoint was called %d time(s), want exactly 1", credentialCalls)
	}
	if pinCalls != 0 {
		t.Errorf("startup continued to the pin fetch after being handed an unusable jwt; the check must gate everything that follows it.\n--- output ---\n%s", out)
	}
	assertNoSecrets(t, "the unusable fetched credential", out)
}

// TestProberDoesNotFetchWhenAJwtIsSupplied is the deployment-safety test at the
// process level: with UR_PROBER_BY_JWT set, exactly as the running deployment
// sets it today, the new endpoint is never contacted. A prober configured the
// old way must not acquire a new dependency on a server endpoint that may not
// be deployed, or on a bootstrap task that may not have run.
func TestProberDoesNotFetchWhenAJwtIsSupplied(t *testing.T) {
	stub := &credentialStub{
		status: http.StatusOK,
		body:   `{"by_client_jwt":"` + testByJwt(t) + `","client_id":"019f8835-158d-6fd8-e9dd-fd0e4c6d6792"}`,
	}
	srv := stub.server(t)
	defer srv.Close()

	out, code := runProberWithJwt(t, testByJwt(t),
		"-api-url", srv.URL,
		"-platform-url", "ws://127.0.0.1:1",
		"-interval", "0",
		"-skip-confinement-check",
		"-skip-bandwidth",
	)

	credentialCalls, pinCalls := stub.counts()
	if credentialCalls != 0 {
		t.Errorf("the prober asked the server for a credential %d time(s) despite being given one; startup would then depend on an endpoint this deployment does not need.\n--- output ---\n%s", credentialCalls, out)
	}
	if strings.Contains(out, "asking the server for the prober credential") {
		t.Errorf("the prober announced a credential fetch even though a jwt was supplied.\n--- output ---\n%s", out)
	}
	// It must still get on with the run it always did: straight to the pin
	// fetch, which this stub refuses, so it stops there as before.
	if pinCalls == 0 {
		t.Errorf("startup never reached the pin fetch with a supplied jwt; the existing deployment path is broken.\n--- output ---\n%s", out)
	}
	if code == 0 {
		t.Errorf("exited 0 with no pin set.\n--- output ---\n%s", out)
	}
}

func TestCheckCredentialAcceptsOK(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := checkCredential(context.Background(), srv.Client(), srv.URL, "the-jwt"); err != nil {
		t.Fatalf("checkCredential: %s, want nil", err)
	}
	// Without the header the endpoint would answer 401 for a reason that has
	// nothing to do with the credential, and the check would condemn a working
	// token.
	if want := "Bearer the-jwt"; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
}

func TestCheckCredentialRejects401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := checkCredential(context.Background(), srv.Client(), srv.URL, "stale-jwt")
	if !errors.Is(err, errCredentialRejected) {
		t.Fatalf("checkCredential = %v, want errCredentialRejected", err)
	}
	// A rejection that also satisfied errCredentialUnverified would be
	// downgraded to a warning by main's switch, which is the whole failure this
	// change exists to prevent.
	if errors.Is(err, errCredentialUnverified) {
		t.Errorf("a rejection also matched errCredentialUnverified; main would let the prober start")
	}
}

// A server that predates the endpoint answers 404. That says nothing about the
// credential, so it must not stop the prober -- the same posture ingest takes
// when the due endpoint is missing.

func TestCheckCredentialTreats404AsUnverified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	err := checkCredential(context.Background(), srv.Client(), srv.URL, "fine-jwt")
	if !errors.Is(err, errCredentialUnverified) {
		t.Fatalf("checkCredential = %v, want errCredentialUnverified", err)
	}
	if errors.Is(err, errCredentialRejected) {
		t.Errorf("404 was reported as a rejected credential; an old server would stop the prober")
	}
}

func TestCheckCredentialTreatsTransportErrorAsUnverified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	err := checkCredential(context.Background(), http.DefaultClient, url, "fine-jwt")
	if !errors.Is(err, errCredentialUnverified) {
		t.Fatalf("checkCredential = %v, want errCredentialUnverified", err)
	}
	if errors.Is(err, errCredentialRejected) {
		t.Errorf("an unreachable server was reported as a rejected credential")
	}
}
