package prober

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/operator-proxy/egresshealth"
)

// Tests of the health step of a probe: the same tunnel client, the log line,
// and runs that measured nothing or did not run.

// Redirects the standard logger for the duration of a test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	flags := log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(flags)
	})
	return buf
}

// A provider whose tunnel works, which two CDNs refuse, one of
// whose sites could not be measured when its tunnel died, and which loaded one
// canary that failed. It is shaped like a sampled run, which is what a real
// one is: the tallies are over the destinations this run drew and measured,
// and TableTotal is what they were drawn from.
func healthResult() *egresshealth.Result {
	return &egresshealth.Result{
		Checks: []egresshealth.CheckResult{
			{Name: "cloudflare-doh", Class: egresshealth.ClassDns, Ok: true},
			{Name: "google-doh", Class: egresshealth.ClassDns, Ok: true},
			{Name: "adguard-doh", Class: egresshealth.ClassDns, Ok: true},
			{Name: "google-generate-204", Class: egresshealth.ClassConnectivity, Ok: true},
			{Name: "cloudflare-cp-204", Class: egresshealth.ClassConnectivity, Ok: true},
			{Name: "cloudflare-cdn", Class: egresshealth.ClassCdn, Ok: true},
			{Name: "jsdelivr-fastly-mirror", Class: egresshealth.ClassCdn},
			{Name: "amazon-cloudfront", Class: egresshealth.ClassCdn},
			{Name: "wikipedia", Class: egresshealth.ClassSite, Ok: true},
			{Name: "github", Class: egresshealth.ClassSite, Ok: true},
			{Name: "naver", Class: egresshealth.ClassSite, Ok: true, Attempts: 2},
			{Name: "reddit", Class: egresshealth.ClassSite, NotMeasured: true},
			{Name: "etsy", Class: egresshealth.ClassSite, Canary: true},
		},
		ExitIp:         "203.0.113.7",
		ExitObservedAt: exitObservedAt,
		OkCount:        9,
		Total:          11,
		NotMeasured:    1,
		ByClass: map[egresshealth.Class]egresshealth.ClassSummary{
			egresshealth.ClassDns:          {Ok: 3, Total: 3},
			egresshealth.ClassConnectivity: {Ok: 2, Total: 2},
			egresshealth.ClassCdn:          {Ok: 1, Total: 3},
			egresshealth.ClassSite:         {Ok: 3, Total: 3},
		},
		TableTotal: 139,
	}
}

// The property the whole wiring
// turns on: the check must reuse the client the tunnel already handed back,
// not prompt a second tunnel (double the contract cost, and a different
// session from the one the exit address describes).
func TestEgressHealthRunsOnTheSameTunnelClient(t *testing.T) {
	logs := captureLog(t)
	tunnelClient := &http.Client{}
	opens := 0
	var seen *http.Client

	sub := &stubSubmitter{}
	p := &Prober{
		Open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
			opens++
			return tunnelClient, func() error { return nil }, nil
		},
		Health: func(ctx context.Context, c *http.Client, place egresshealth.Place) (*egresshealth.Result, error) {
			seen = c
			return healthResult(), nil
		},
		Submit: sub,
	}
	if err := p.ProbeOne(context.Background(), provider("provider-1")); err != nil {
		t.Fatalf("ProbeOne err = %v", err)
	}
	if opens != 1 {
		t.Fatalf("tunnels opened = %d, want exactly 1; the health check must not open its own", opens)
	}
	if seen != tunnelClient {
		t.Fatal("the health check ran on a different client than the tunnel's")
	}

	// The failed list is measured, scored failures only; the load the dead
	// tunnel took and the canary are named apart.
	want := "egress-health: provider=provider-1 ok=9/11 dns=3/3 connectivity=2/2 cdn=1/3 site=3/3 table=139 retried=1 not_measured=1 canary=0/1 failed=jsdelivr-fastly-mirror,amazon-cloudfront not-measured=reddit canary-failed=etsy"
	if got := strings.TrimSpace(logs.String()); got != want {
		t.Fatalf("log line =\n%q\nwant\n%q", got, want)
	}
}

// A run whose tunnel could not be
// re-created for any of its loads measured nothing. It is not submitted --
// 0/0 is not evidence -- no location is submitted either, and the probe fails
// as not measured so the attempt backoff brings the provider round again.
func TestNothingMeasuredIsNotSubmitted(t *testing.T) {
	logs := captureLog(t)
	reporter := &stubHealthReporter{}
	sub := &stubSubmitter{}
	unmeasured := &egresshealth.Result{
		Checks:      []egresshealth.CheckResult{{Name: "google", Class: egresshealth.ClassSite, NotMeasured: true}},
		ExitIp:      "203.0.113.7",
		NotMeasured: 1,
		ByClass:     map[egresshealth.Class]egresshealth.ClassSummary{},
	}
	p := &Prober{Open: okOpen, Health: answers(unmeasured), Submit: sub, HealthResults: reporter}
	err := p.ProbeOne(context.Background(), provider("p1"))
	if !errors.Is(err, ErrNotMeasured) {
		t.Fatalf("err = %v, want ErrNotMeasured", err)
	}
	if reporter.calls != 0 || sub.calls != 0 {
		t.Fatalf("health submitted %d, location submitted %d; a run that measured nothing submits nothing", reporter.calls, sub.calls)
	}
	if !strings.Contains(logs.String(), "nothing measured; not submitted") {
		t.Errorf("the log does not say why nothing was submitted:\n%s", logs.String())
	}
}

// A run that measured some loads and not
// others is a measurement of the ones it did, and is submitted as one.
func TestPartlyMeasuredRunIsSubmitted(t *testing.T) {
	captureLog(t)
	reporter := &stubHealthReporter{}
	sub := &stubSubmitter{}
	p := &Prober{Open: okOpen, Health: answers(healthResult()), Submit: sub, HealthResults: reporter}
	if err := p.ProbeOne(context.Background(), provider("p1")); err != nil {
		t.Fatalf("ProbeOne err = %v", err)
	}
	if reporter.calls != 1 || reporter.last.NotMeasured != 1 {
		t.Fatalf("health submitted %d time(s) with %+v", reporter.calls, reporter.last)
	}
	if sub.calls != 1 {
		t.Fatalf("location submitted %d time(s), want once", sub.calls)
	}
}

// A check that did not
// run must not be rendered as ok=0/N, which is the blackhole reading. Framing
// the prober's own fault as the provider's is how a good provider gets a bad
// record.
func TestEgressHealthStructuralFailureIsNotLoggedAsAScore(t *testing.T) {
	logs := captureLog(t)
	p := &Prober{
		Open: okOpen,
		Health: func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
			return nil, egresshealth.ErrNilClient
		},
		Submit: &stubSubmitter{},
	}
	if err := p.ProbeOne(context.Background(), provider("provider-1")); err == nil {
		t.Fatal("a health run that did not happen reported success")
	}
	out := logs.String()
	if !strings.Contains(out, "did not run") {
		t.Fatalf("a structural health failure was not reported as such.\n--- log ---\n%s", out)
	}
	if strings.Contains(out, "ok=0/") {
		t.Fatalf("a health check that never ran was logged as a zero score.\n--- log ---\n%s", out)
	}
}

// Same defect, reached the other way.
// If the probe's context is already done, a run would report 0/N for reasons
// that have nothing to do with the provider.
func TestEgressHealthSkippedWhenNoBudgetLeft(t *testing.T) {
	logs := captureLog(t)
	ran := false
	p := &Prober{
		Open: okOpen,
		Health: func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
			ran = true
			return healthResult(), nil
		},
		Submit: &stubSubmitter{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = p.ProbeOne(ctx, provider("provider-1"))

	if ran {
		t.Fatal("the health check ran on an already-dead context; it would report 0/N and read as a blackhole")
	}
	out := logs.String()
	if !strings.Contains(out, "skipped") {
		t.Fatalf("the skip was not logged.\n--- log ---\n%s", out)
	}
	if strings.Contains(out, "ok=0/") {
		t.Fatalf("a skipped health check was logged as a zero score.\n--- log ---\n%s", out)
	}
}

// Records the failure class of every reported attempt.
type stubAttemptReporter struct {
	stateLock sync.Mutex
	seen      []string
}

// Implements AttemptReporter.
func (self *stubAttemptReporter) ReportAttempt(ctx context.Context, id string, failure string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.seen = append(self.seen, failure)
	return nil
}

// Returns the failure classes reported so far.
func (self *stubAttemptReporter) failures() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]string(nil), self.seen...)
}

// The failure classes
// reported to the server describe the probe; a health submission that failed
// must not be able to rewrite whether the exit was recorded.
func TestHealthSubmissionFailureNeverChangesTheProbeOutcome(t *testing.T) {
	captureLog(t)
	sub := &stubSubmitter{}
	rep := &stubAttemptReporter{}
	p := &Prober{
		Open:          okOpen,
		Health:        answers(healthResult()),
		Submit:        sub,
		Attempts:      rep,
		HealthResults: &stubHealthReporter{err: errors.New("health endpoint exploded")},
	}
	if err := p.ProbeOne(context.Background(), provider("provider-1")); err != nil {
		t.Fatalf("a failing health submission failed the probe: %v", err)
	}
	if sub.calls != 1 {
		t.Fatalf("submit calls = %d, want 1", sub.calls)
	}
	if got := rep.failures(); len(got) != 1 || got[0] != "" {
		t.Fatalf("reported failure classes = %v, want one success (\"\")", got)
	}
}
