package egresshealth

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Tests of the retry machinery: the delay's distribution and cap, spaced
// attempts, terminal TLS failures, loads interleaving under the slots, and
// budgets.

// The spacing skipped: the attempts still happen, in order, but
// the minutes between them do not. It still honours the context, as the
// Options.Sleep contract requires.
func noSleep(ctx context.Context, d time.Duration) error {
	return ctx.Err()
}

// An injected clock whose Sleep advances it instead of waiting,
// and records every wait a run asked for. It is shared by everything that
// reads it, so it is only meaningful for one retry chain at a time; tests with
// several loads use noSleep and the wall clock instead.
type fakeClock struct {
	stateLock sync.Mutex
	now       time.Time
	slept     []time.Duration
}

// Returns a clock stopped at a fixed instant.
func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)}
}

// The clock's current time, as Options.Now.
func (self *fakeClock) Now() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

// Advances the clock by d and records the wait, as Options.Sleep; it honours
// the context.
func (self *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(d)
	self.slept = append(self.slept, d)
	return nil
}

// Returns every wait asked for so far, in order.
func (self *fakeClock) sleeps() []time.Duration {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]time.Duration(nil), self.slept...)
}

// Like fastOptions, on the fake clock, with the default spacing, so
// the waits a chain asks for are the production ones.
func clockedOptions(clock *fakeClock) Options {
	return Options{
		PerRequestTimeout: 200 * time.Millisecond,
		Concurrency:       3,
		Rand:              rand.New(rand.NewSource(7)),
		Now:               clock.Now,
		Sleep:             clock.Sleep,
	}
}

// Holds the spacing to its definition over many
// draws: exponential with the configured mean, capped at three times it. The
// cap trims the tail, so the realised mean is mean * (1 - e^-3) and e^-3 of
// draws sit exactly on the cap. Both are asserted, because a spacing that
// drifted towards fixed intervals -- the scanner shape -- or lost its cap would
// pass a looser test.
func TestRetryDelayMeanAndCap(t *testing.T) {
	const draws = 200000
	mean := DefaultLoadRetryMeanInterval
	limit := retryDelayCapFactor * mean
	r := rand.New(rand.NewSource(1))

	var sum float64
	atCap, belowSecond := 0, 0
	minimum, maximum := time.Duration(math.MaxInt64), time.Duration(0)
	for i := 0; i < draws; i++ {
		d := retryDelay(r.ExpFloat64(), mean)
		sum += float64(d)
		if d == limit {
			atCap++
		}
		if d < time.Second {
			belowSecond++
		}
		minimum = min(minimum, d)
		maximum = max(maximum, d)
	}

	gotMean := time.Duration(sum / draws)
	wantMean := time.Duration(float64(mean) * (1 - math.Exp(-retryDelayCapFactor)))
	if diff := math.Abs(float64(gotMean-wantMean)) / float64(wantMean); 0.01 < diff {
		t.Errorf("mean delay = %s over %d draws, want %s (mean x (1 - e^-3)) within 1%%", gotMean, draws, wantMean)
	}
	if limit < maximum {
		t.Errorf("a draw of %s exceeded the cap %s", maximum, limit)
	}
	if maximum != limit {
		t.Errorf("no draw reached the cap %s in %d; the cap is not being applied where the tail is", limit, draws)
	}
	if gotShare, wantShare := float64(atCap)/draws, math.Exp(-retryDelayCapFactor); 0.005 < math.Abs(gotShare-wantShare) {
		t.Errorf("%.4f of draws sat on the cap, want e^-3 = %.4f", gotShare, wantShare)
	}
	if minimum < 0 {
		t.Errorf("a draw came back negative: %s", minimum)
	}
	// Exponential, not fixed: the spacing must actually vary, with the short
	// end as rare as the distribution says.
	if belowSecond == 0 || draws/100 < belowSecond {
		t.Errorf("%d of %d draws were under a second; want a handful (about 0.3%%)", belowSecond, draws)
	}
	t.Logf("mean %s (want %s), max %s, %.2f%% at the cap", gotMean, wantMean, maximum, 100*float64(atCap)/draws)

	// The edges: a huge draw must come back as the cap, not overflow into a
	// negative duration; a zero mean or draw is no wait at all.
	if got := retryDelay(1e300, mean); got != limit {
		t.Errorf("retryDelay(1e300) = %s, want the cap %s", got, limit)
	}
	if got := retryDelay(math.Inf(1), mean); got != limit {
		t.Errorf("retryDelay(+Inf) = %s, want the cap %s", got, limit)
	}
	if got := retryDelay(0.5, 0); got != 0 {
		t.Errorf("retryDelay with a zero mean = %s, want 0", got)
	}
	if got := retryDelay(0, mean); got != 0 {
		t.Errorf("retryDelay(0) = %s, want 0", got)
	}
}

// The rule the whole change turns
// on: a load passes if any attempt passed. It also proves the attempts are
// spaced -- each retry happened after its drawn wait on the run's clock, not
// back to back -- and that the result keeps both the count and why the
// retries were needed.
func TestLoadThatFailsTwiceThenPassesPasses(t *testing.T) {
	clock := newFakeClock()
	var stateLock sync.Mutex
	var at []time.Time
	dests := stubDestinations(t, []spec{{name: "flaky", class: ClassSite, h: func(w http.ResponseWriter, r *http.Request) {
		stateLock.Lock()
		at = append(at, clock.Now())
		n := len(at)
		stateLock.Unlock()
		if n < 3 {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("User-agent: *"))
	}}})

	res, err := check(context.Background(), http.DefaultClient, dests, clockedOptions(clock))
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	c := res.Checks[0]
	if !c.Ok {
		t.Fatalf("a load that failed twice and then passed was recorded as failed: %+v", c)
	}
	if c.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", c.Attempts)
	}
	if c.Err != "" {
		t.Errorf("Err = %q on a passing load; Err is the outcome, and the outcome was a pass", c.Err)
	}
	if !strings.Contains(c.LastFailure, "429") {
		t.Errorf("LastFailure = %q, want the second attempt's 429", c.LastFailure)
	}
	if res.OkCount != 1 || res.Total != 1 || res.Retried() != 1 {
		t.Errorf("OkCount/Total = %d/%d retried=%d, want 1/1 retried=1", res.OkCount, res.Total, res.Retried())
	}
	if got := res.Summary(); !strings.Contains(got, "retried=1") {
		t.Errorf("Summary = %q, want the retry visible", got)
	}

	sleeps := clock.sleeps()
	if len(sleeps) != 2 {
		t.Fatalf("the chain slept %d time(s), want once before each retry: %v", len(sleeps), sleeps)
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	for i, d := range sleeps {
		if d <= 0 || retryDelayCapFactor*DefaultLoadRetryMeanInterval < d {
			t.Errorf("wait %d = %s, want a positive draw no longer than the cap", i, d)
		}
		if gap := at[i+1].Sub(at[i]); gap != d {
			t.Errorf("attempt %d came %s after attempt %d, want the drawn wait %s", i+2, gap, i+1, d)
		}
	}
}

// The other half of the rule. A load
// that never passes is tried exactly LoadAttempts times and then counts as
// failed, with its last failure as the error.
func TestLoadFailsOnlyWhenEveryAttemptFailed(t *testing.T) {
	var requests atomic.Int32
	dests := stubDestinations(t, []spec{{name: "down", class: ClassCdn, h: func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}}})
	for _, attempts := range []int{1, 2, 3, 5} {
		requests.Store(0)
		clock := newFakeClock()
		opts := clockedOptions(clock)
		opts.LoadAttempts = attempts
		res, err := check(context.Background(), http.DefaultClient, dests, opts)
		if err != nil {
			t.Fatalf("check err = %v", err)
		}
		c := res.Checks[0]
		if c.Ok || c.Attempts != attempts || int(requests.Load()) != attempts {
			t.Errorf("LoadAttempts %d: ok=%t attempts=%d requests=%d, want a failure after exactly %d", attempts, c.Ok, c.Attempts, requests.Load(), attempts)
		}
		if c.LastFailure != c.Err || !strings.Contains(c.Err, "503") {
			t.Errorf("LoadAttempts %d: err=%q last_failure=%q, want both the last 503", attempts, c.Err, c.LastFailure)
		}
		if got := len(clock.sleeps()); got != attempts-1 {
			t.Errorf("LoadAttempts %d: %d wait(s), want %d", attempts, got, attempts-1)
		}
	}
}

// A forged certificate is a hard
// integrity failure whatever a retry does, so the chain ends at the first one
// -- no wait, no second handshake for the interceptor to answer.
func TestTlsAuthenticationFailureStopsRetrying(t *testing.T) {
	var handshakes atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			handshakes.Add(1)
		}
	}
	srv.StartTLS()
	defer srv.Close()

	clock := newFakeClock()
	dests := []Destination{{Name: "intercepted", Class: ClassConnectivity, Url: srv.URL, Expect: ExpectStatus, Status: http.StatusNoContent}}
	res, err := check(context.Background(), http.DefaultClient, dests, clockedOptions(clock))
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	c := res.Checks[0]
	if c.Ok || !c.TlsAuthenticationFailure {
		t.Fatalf("result = %+v, want a TLS-authentication failure", c)
	}
	if c.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1: a TLS-authentication failure is terminal", c.Attempts)
	}
	if got := clock.sleeps(); len(got) != 0 {
		t.Errorf("the chain waited %v after a TLS-authentication failure; it must not retry at all", got)
	}
	if got := handshakes.Load(); got != 1 {
		t.Errorf("%d connection(s) reached the intercepting server, want 1", got)
	}
	if !res.TlsAuthenticationFailure {
		t.Error("the run-level TLS-authentication bit was not set")
	}
}

// A load waiting out its spacing must not
// hold a concurrency slot, or a site that keeps failing would keep the rest of
// the sample queued behind its ten-minute chain. With one slot, a second load
// has to complete while the first is still waiting to retry.
func TestLoadsInterleaveUnderConcurrency(t *testing.T) {
	var flakyCalls atomic.Int32
	steadyServed := make(chan struct{})
	dests := stubDestinations(t, []spec{
		{name: "flaky", class: ClassSite, h: func(w http.ResponseWriter, r *http.Request) {
			if flakyCalls.Add(1) == 1 {
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("ok"))
		}},
		{name: "steady", class: ClassSite, h: func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("ok"))
			close(steadyServed)
		}},
	})

	sleeping := make(chan struct{})
	release := make(chan struct{})
	opts := fastOptions()
	opts.Concurrency = 1
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		close(sleeping)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r := newRun(staticPath{client: http.DefaultClient}, opts, 1, time.Minute, rand.New(rand.NewSource(1)), &exitRecord{})

	flakyDone := make(chan CheckResult, 1)
	go func() { flakyDone <- r.load(context.Background(), dests[0]) }()
	select {
	case <-sleeping:
	case <-time.After(5 * time.Second):
		t.Fatal("the flaky load never reached its retry wait")
	}

	steadyDone := make(chan CheckResult, 1)
	go func() { steadyDone <- r.load(context.Background(), dests[1]) }()
	select {
	case <-steadyServed:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("a second load could not run while the first waited to retry: the waiting load is holding the only slot")
	}
	if steady := <-steadyDone; !steady.Ok || steady.Attempts != 1 {
		t.Errorf("steady = %+v, want one clean attempt", steady)
	}

	close(release)
	if flaky := <-flakyDone; !flaky.Ok || flaky.Attempts != 2 {
		t.Errorf("flaky = %+v, want a pass on its second attempt", flaky)
	}
}

// A caller that sets a budget shorter than
// the spacing can produce must not lose attempts to it. The wait is shortened
// to fit, leaving one per-request timeout for the attempt, so the load still
// gets every try inside the budget.
func TestRetryFitsInsideAnExplicitBudget(t *testing.T) {
	var requests atomic.Int32
	dests := stubDestinations(t, []spec{{name: "down", class: ClassSite, h: func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "no", http.StatusServiceUnavailable)
	}}})
	clock := newFakeClock()
	opts := clockedOptions(clock)
	opts.Budget = 2 * time.Minute
	// A mean far longer than the budget, so every unclamped draw would overrun.
	opts.LoadRetryMeanInterval = time.Hour

	res, err := check(context.Background(), http.DefaultClient, dests, opts)
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	if got := res.Checks[0].Attempts; got != DefaultLoadAttempts || int(requests.Load()) != DefaultLoadAttempts {
		t.Fatalf("attempts = %d (requests %d), want all %d inside the budget", got, requests.Load(), DefaultLoadAttempts)
	}
	var total time.Duration
	for _, d := range clock.sleeps() {
		total += d
	}
	if opts.Budget-opts.PerRequestTimeout < total {
		t.Errorf("the waits sum to %s, past the %s budget less one attempt", total, opts.Budget)
	}
}

// The caller's context ending
// mid-run leaves loads that failed on the dead context and loads that never
// retried. That is the prober's deadline, not the provider's traffic, and it
// must come back as ErrInterrupted rather than as a run full of failures.
func TestCheckInterruptedByTheCallerIsNotAResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dests := stubDestinations(t, []spec{
		{name: "down", class: ClassSite, h: func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no", http.StatusServiceUnavailable)
		}},
		{name: "up", class: ClassSite, h: okBody("ok")},
	})
	opts := fastOptions()
	// The pass is cancelled while the failing load waits to retry.
	opts.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	res, err := check(ctx, http.DefaultClient, dests, opts)
	if !errors.Is(err, ErrInterrupted) || res != nil {
		t.Fatalf("check = (%+v, %v), want (nil, ErrInterrupted)", res, err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to carry the cause", err)
	}
}

// The run's own budget ending is part of the
// measurement -- a provider that swallows everything is bounded by it -- and
// its outcome is a result, not ErrInterrupted.
func TestRunBudgetEndingIsAResult(t *testing.T) {
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	dests := stubDestinations(t, []spec{{name: "swallows", class: ClassSite, h: hangs(done)}})
	res, err := check(context.Background(), http.DefaultClient, dests, Options{
		PerRequestTimeout: 10 * time.Second,
		Budget:            200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("a run cut off by its own budget returned an error: %v", err)
	}
	if res.OkCount != 0 || res.Checks[0].Ok {
		t.Fatalf("result = %+v, want the swallowing load failed", res.Checks[0])
	}
}
