package prober

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// Tests of one probe's flow: the exit submitted, the provider's place, and the
// early exits.

// A Submitter shared by prober_test.go (single-goroutine callers) and
// schedule_test.go (concurrent callers via Scheduler.Run), so its fields must
// be safe for concurrent access.
type stubSubmitter struct {
	stateLock sync.Mutex
	calls     int
	lastIp    string
	lastAt    time.Time
	err       error
}

// Implements Submitter.
func (self *stubSubmitter) Submit(ctx context.Context, id string, exitIp string, observedAt time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls++
	self.lastIp, self.lastAt = exitIp, observedAt
	return self.err
}

// When the stub runs' warm-ups answered.
var exitObservedAt = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

// The smallest run a probe can succeed on: one measured load and
// the exit address its warm-up saw.
func exitResult() *egresshealth.Result {
	return &egresshealth.Result{
		Checks:         []egresshealth.CheckResult{{Name: "google", Class: egresshealth.ClassSite, Ok: true, Attempts: 1}},
		ExitIp:         "203.0.113.7",
		ExitObservedAt: exitObservedAt,
		OkCount:        1,
		Total:          1,
		ByClass:        map[egresshealth.Class]egresshealth.ClassSummary{egresshealth.ClassSite: {Ok: 1, Total: 1}},
	}
}

// A Health checker that returns res for every provider.
func answers(res *egresshealth.Result) EgressHealthChecker {
	return func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
		return res, nil
	}
}

// A provider with no place.
func provider(id string) Provider {
	return Provider{ClientId: id}
}

// A probe whose health run measured and whose warm-up saw an exit submits
// that exit, at the warm-up's time, and closes the tunnel.
func TestProbeOneHappyPathSubmitsTheExitAndCloses(t *testing.T) {
	closed := false
	sub := &stubSubmitter{}
	p := &Prober{
		Open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
			return &http.Client{}, func() error { closed = true; return nil }, nil
		},
		Health: answers(exitResult()),
		Submit: sub,
	}
	if err := p.ProbeOne(context.Background(), provider("provider-1")); err != nil {
		t.Fatalf("ProbeOne err = %v", err)
	}
	if sub.calls != 1 || sub.lastIp != "203.0.113.7" || !sub.lastAt.Equal(exitObservedAt) {
		t.Fatalf("submitted %d time(s) exit %q at %s, want once, the warm-up's address and time", sub.calls, sub.lastIp, sub.lastAt)
	}
	if !closed {
		t.Fatal("the tunnel must be closed after the probe")
	}
}

// The place the due list carries
// is what the health run draws its sample against.
func TestProbeOnePassesTheProvidersPlaceToTheRun(t *testing.T) {
	var got egresshealth.Place
	p := &Prober{
		Open: okOpen,
		Health: func(ctx context.Context, c *http.Client, place egresshealth.Place) (*egresshealth.Result, error) {
			got = place
			return exitResult(), nil
		},
		Submit: &stubSubmitter{},
	}
	want := egresshealth.Place{Country: "de", Region: "Bavaria"}
	if err := p.ProbeOne(context.Background(), Provider{ClientId: "p1", Place: want}); err != nil {
		t.Fatalf("ProbeOne err = %v", err)
	}
	if got != want {
		t.Fatalf("the run was drawn for %+v, want %+v", got, want)
	}
}

// A warm-up that never got
// an answer leaves nothing to place the provider by. The probe fails with
// no_exit_ip and submits no location.
func TestProbeOneWithoutAnExitDoesNotSubmitALocation(t *testing.T) {
	res := exitResult()
	res.ExitIp, res.IpEchoErr = "", "the /ip echo answered status 502"
	sub := &stubSubmitter{}
	p := &Prober{Open: okOpen, Health: answers(res), Submit: sub}
	err := p.ProbeOne(context.Background(), provider("p1"))
	if err == nil {
		t.Fatal("a probe with no exit address succeeded")
	}
	if sub.calls != 0 {
		t.Fatal("a location was submitted with no exit address")
	}
}

// A tunnel that will not open
// must short-circuit the probe.
func TestProbeOneTunnelFailureSkipsHealthAndSubmit(t *testing.T) {
	sub := &stubSubmitter{}
	p := &Prober{
		Open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
			return nil, nil, errors.New("no route to provider")
		},
		Health: func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
			t.Fatal("Health must not run when the tunnel fails")
			return nil, nil
		},
		Submit: sub,
	}
	if err := p.ProbeOne(context.Background(), provider("provider-1")); err == nil {
		t.Fatal("expected a tunnel error")
	}
	if sub.calls != 0 {
		t.Fatal("must not submit when the tunnel fails")
	}
}

// The exit address comes from the
// health run's warm-up, so a prober without one has nothing to probe with.
func TestProbeOneWithoutAHealthCheckerFails(t *testing.T) {
	sub := &stubSubmitter{}
	p := &Prober{Open: okOpen, Submit: sub}
	if err := p.ProbeOne(context.Background(), provider("p1")); err == nil {
		t.Fatal("a prober with no health checker reported success")
	}
	if sub.calls != 0 {
		t.Fatal("a location was submitted without a health run")
	}
}
