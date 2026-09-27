package prober

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// Tests of attempt reporting: every outcome reported, the failure classes, and
// the bounded, per-pass error-log gates.

// One reported attempt.
type attempt struct {
	id      string
	failure string
}

// An AttemptReporter that records every attempt and answers err.
type stubReporter struct {
	stateLock sync.Mutex
	attempts  []attempt
	err       error
}

// Implements AttemptReporter.
func (self *stubReporter) ReportAttempt(ctx context.Context, id string, probeFailure string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.attempts = append(self.attempts, attempt{id: id, failure: probeFailure})
	return self.err
}

// Returns the attempts reported so far.
func (self *stubReporter) snapshot() []attempt {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]attempt(nil), self.attempts...)
}

// A TunnelOpener that always opens a tunnel whose close succeeds.
func okOpen(ctx context.Context, id string) (*http.Client, func() error, error) {
	return &http.Client{}, func() error { return nil }, nil
}

// Every attempt is reported, success
// included: the rule has to be unconditional or there is a path through the
// prober that forgets.
func TestProbeOneReportsAttemptOnSuccess(t *testing.T) {
	rep := &stubReporter{}
	p := &Prober{
		Open:     okOpen,
		Health:   answers(exitResult()),
		Submit:   &stubSubmitter{},
		Attempts: rep,
	}
	if err := p.ProbeOne(context.Background(), provider("p1")); err != nil {
		t.Fatalf("ProbeOne err = %v", err)
	}
	got := rep.snapshot()
	if len(got) != 1 {
		t.Fatalf("attempts = %v, want exactly one", got)
	}
	if got[0].id != "p1" {
		t.Errorf("attempt client id = %q, want p1", got[0].id)
	}
	if got[0].failure != "" {
		t.Errorf("probe_failure = %q, want empty on success", got[0].failure)
	}
}

// The starvation fix's
// prober-side half. A provider that always fails to probe never gets a
// location row, so the server's due query hands it back on every poll forever
// and no healthy provider is ever refreshed -- silently, because the endpoint
// keeps returning a full plausible batch. The server can only defer such a
// provider if the prober tells it the attempt happened, so every failure stage
// must report.
func TestProbeOneReportsAttemptOnEveryFailureStage(t *testing.T) {
	noExit := exitResult()
	noExit.ExitIp, noExit.IpEchoErr = "", "the /ip echo answered status 502"
	unmeasured := &egresshealth.Result{
		Checks:      []egresshealth.CheckResult{{Name: "google", Class: egresshealth.ClassSite, NotMeasured: true}},
		ExitIp:      "203.0.113.7",
		NotMeasured: 1,
		ByClass:     map[egresshealth.Class]egresshealth.ClassSummary{},
	}

	cases := []struct {
		name        string
		prober      func(rep *stubReporter) *Prober
		wantFailure string
	}{
		{
			name: "tunnel",
			prober: func(rep *stubReporter) *Prober {
				return &Prober{
					Open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
						return nil, nil, errors.New("no contract")
					},
					Health:   answers(exitResult()),
					Submit:   &stubSubmitter{},
					Attempts: rep,
				}
			},
			wantFailure: FailureTunnel,
		},
		{
			name: "health did not run",
			prober: func(rep *stubReporter) *Prober {
				return &Prober{
					Open: okOpen,
					Health: func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
						return nil, egresshealth.ErrNoDestinations
					},
					Submit:   &stubSubmitter{},
					Attempts: rep,
				}
			},
			wantFailure: FailureHealthNotRun,
		},
		{
			name: "nothing measured",
			prober: func(rep *stubReporter) *Prober {
				return &Prober{Open: okOpen, Health: answers(unmeasured), Submit: &stubSubmitter{}, Attempts: rep}
			},
			wantFailure: FailureNotMeasured,
		},
		{
			name: "no exit address",
			prober: func(rep *stubReporter) *Prober {
				return &Prober{Open: okOpen, Health: answers(noExit), Submit: &stubSubmitter{}, Attempts: rep}
			},
			wantFailure: FailureNoExitIp,
		},
		{
			name: "submit",
			prober: func(rep *stubReporter) *Prober {
				return &Prober{
					Open:     okOpen,
					Health:   answers(exitResult()),
					Submit:   &stubSubmitter{err: errors.New("status 500")},
					Attempts: rep,
				}
			},
			wantFailure: FailureSubmit,
		},
	}

	for _, tc := range cases {
		rep := &stubReporter{}
		if err := tc.prober(rep).ProbeOne(context.Background(), provider("p1")); err == nil {
			t.Fatalf("%s: ProbeOne returned nil on a failing probe", tc.name)
		}
		got := rep.snapshot()
		if len(got) != 1 {
			t.Fatalf("%s: attempts = %v, want exactly one; an unreported attempt leaves this provider at the head of the due queue forever", tc.name, got)
		}
		if got[0].failure != tc.wantFailure {
			t.Fatalf("%s: probe_failure = %q, want %q", tc.name, got[0].failure, tc.wantFailure)
		}
	}
}

// The server rejects a class longer than
// 64 chars with a 400, and a rejected report is a lost report.
func TestFailureClassesFitTheServerColumn(t *testing.T) {
	for _, class := range []string{FailureTunnel, FailureHealthNotRun, FailureNotMeasured, FailureNoExitIp, FailureSubmit} {
		if class == "" {
			t.Error("a failure class is empty; empty means success on the wire")
		}
		if 64 < len(class) {
			t.Errorf("failure class %q is longer than the server's varchar(64)", class)
		}
	}
}

// A broken attempt endpoint
// must not turn a good probe into a failure -- the location was submitted and
// is already recorded server-side.
func TestProbeOneDoesNotFailTheProbeWhenReportingFails(t *testing.T) {
	rep := &stubReporter{err: errors.New("attempt endpoint down")}
	p := &Prober{
		Open:     okOpen,
		Health:   answers(exitResult()),
		Submit:   &stubSubmitter{},
		Attempts: rep,
	}
	if err := p.ProbeOne(context.Background(), provider("p1")); err != nil {
		t.Fatalf("ProbeOne err = %v; a failed attempt report must not fail the probe itself", err)
	}
}

// Keeps Attempts optional so a caller
// that has no reporter (a test, a one-shot manual probe) is not broken by it.
func TestProbeOneWithoutAReporterStillProbes(t *testing.T) {
	sub := &stubSubmitter{}
	p := &Prober{
		Open:   okOpen,
		Health: answers(exitResult()),
		Submit: sub,
	}
	if err := p.ProbeOne(context.Background(), provider("p1")); err != nil {
		t.Fatalf("ProbeOne err = %v", err)
	}
	if sub.calls != 1 {
		t.Fatalf("submit calls = %d, want 1", sub.calls)
	}
}

// Fails with a different message every call, the way a real
// server does when its 5xx body carries a request id or a timestamp.
type varyingReporter struct{ n atomic.Int64 }

// Implements AttemptReporter.
func (self *varyingReporter) ReportAttempt(ctx context.Context, id string, probeFailure string) error {
	return fmt.Errorf("ingest: rejected: status 503: request-id %d", self.n.Add(1))
}

// The dedup map keyed on the full
// error text is the whole mechanism keeping a broken ingest endpoint from
// logging once per provider per pass -- and it inverts when the message
// varies. ErrRejected embeds up to 4096 bytes of server response body, and a
// body carrying a request id or timestamp makes every message distinct: the
// "logged once" gate then logs every line (the flood it exists to prevent)
// while the map grows one entry per probe, forever, in a process designed to
// run for months.
func TestReportAttemptErrorLoggingIsBounded(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	p := &Prober{Open: okOpen, Attempts: &varyingReporter{}}
	maxLoggedDistinctErrors := p.maxLoggedDistinctErrors()
	const probes = 200
	for i := 0; i < probes; i++ {
		p.reportAttempt(context.Background(), fmt.Sprintf("provider-%d", i), FailureTunnel)
	}

	lines := strings.Count(buf.String(), "could not report a probe attempt")
	if maxLoggedDistinctErrors < lines {
		t.Errorf("logged %d attempt-error lines across %d probes, want at most %d: a varying error message must not defeat the dedup gate",
			lines, probes, maxLoggedDistinctErrors)
	}
	p.attemptErr.stateLock.Lock()
	size := len(p.attemptErr.seen)
	p.attemptErr.stateLock.Unlock()
	if maxLoggedDistinctErrors < size {
		t.Errorf("dedup map holds %d entries after %d probes, want at most %d: it must not grow without bound in a long-running process",
			size, probes, maxLoggedDistinctErrors)
	}
}

// The same property for the health
// submitter, which has its own map for the same reason.
func TestHealthSubmitErrorLoggingIsBounded(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	p := &Prober{Open: okOpen}
	maxLoggedDistinctErrors := p.maxLoggedDistinctErrors()
	for i := 0; i < 200; i++ {
		p.logHealthErrOnce(fmt.Errorf("ingest: rejected: status 503: request-id %d", i), "prober: could not submit an egress-health result")
	}

	p.healthErr.stateLock.Lock()
	size := len(p.healthErr.seen)
	p.healthErr.stateLock.Unlock()
	if maxLoggedDistinctErrors < size {
		t.Errorf("health dedup map holds %d entries, want at most %d", size, maxLoggedDistinctErrors)
	}
}

// The scheduler dedups on
// the error text and keeps only MaxLoggedDistinctErrors distinct messages. A
// fleet-wide identical tunnel failure -- a wrong -platform-url, a revoked jwt,
// the doc comment's own examples -- produced a distinct message per provider
// when the id was wrapped into it, so all ten detail slots filled with copies
// of one failure mode and a genuinely different eleventh error was suppressed.
// The id is already in the log line's provider= field.
func TestTunnelFailureErrorDoesNotEmbedTheProviderId(t *testing.T) {
	p := &Prober{
		Open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
			return nil, nil, errors.New("dial platform: connection refused")
		},
		Submit: &stubSubmitter{},
	}
	first := p.ProbeOne(context.Background(), provider("provider-aaa"))
	second := p.ProbeOne(context.Background(), provider("provider-bbb"))
	if first == nil || second == nil {
		t.Fatal("both probes must fail")
	}
	if first.Error() != second.Error() {
		t.Errorf("two providers failing the same way produced different error text:\n  %s\n  %s\nthe scheduler dedups on this text, so per-provider variation exhausts its detail budget on one failure mode",
			first, second)
	}
}

// The cap on distinct messages is only safe
// because every pass starts with a clean gate. These gates live on the
// Prober, which lives for the whole process, so a permanent cap would let ten
// transient errors (a burst of 503s carrying request ids) silence a later
// fault that breaks every provider -- a rotated operator secret answering 401
// -- and that silence is the exact failure the logging exists to prevent.
func TestErrorLogGatesReArmEachPass(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	p := &Prober{Open: okOpen, Attempts: &varyingReporter{}}
	maxLoggedDistinctErrors := p.maxLoggedDistinctErrors()
	// Pass one burns the whole budget on transient, all-distinct errors.
	for i := 0; i < 50; i++ {
		p.reportAttempt(context.Background(), fmt.Sprintf("provider-%d", i), FailureTunnel)
	}
	if got := strings.Count(buf.String(), "could not report a probe attempt"); got != maxLoggedDistinctErrors {
		t.Fatalf("pass one logged %d lines, want %d", got, maxLoggedDistinctErrors)
	}

	buf.Reset()
	p.ResetErrorLogging()
	if !strings.Contains(buf.String(), "suppressed") {
		t.Errorf("no suppression notice after a pass that withheld 40 errors: %s", buf.String())
	}

	// Pass two: a new, stable fault affecting every provider must still be
	// reported.
	buf.Reset()
	stable := &stubReporter{err: errors.New("ingest: the server rejected the operator secret")}
	p.Attempts = stable
	for i := 0; i < 5; i++ {
		p.reportAttempt(context.Background(), fmt.Sprintf("provider-%d", i), FailureTunnel)
	}
	if got := strings.Count(buf.String(), "could not report a probe attempt"); got != 1 {
		t.Fatalf("pass two logged %d lines for a new fleet-wide fault, want exactly 1: %s", got, buf.String())
	}
}
