package prober

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe"
)

type testingUrlCompletionReporter struct {
	completions []qualityprobe.UrlProbeCompletion
	legacy      int
	err         error
	closed      *bool
	closedFirst bool
}

func (self *testingUrlCompletionReporter) ReportAttempt(context.Context, string, string) error {
	self.legacy++
	return nil
}

func (self *testingUrlCompletionReporter) ReportUrlProbeCompletion(_ context.Context, completion qualityprobe.UrlProbeCompletion) error {
	self.completions = append(self.completions, completion)
	self.closedFirst = self.closed == nil || *self.closed
	return self.err
}

// Every entered probe turn finishes once, including setup/health failures and
// cancellation. Only the legacy/manual path may emit an identity-free attempt.
func TestUrlCompletedProbeReportsEveryTerminalPath(t *testing.T) {
	for _, scenario := range []string{"healthy", "tunnel_failed", "health_not_run", "canceled"} {
		closed := false
		reporter := &testingUrlCompletionReporter{closed: &closed}
		probe := &Prober{HealthResults: &stubHealthReporter{}, Health: answers(exitResult()),
			Attempts: reporter, Submit: &stubSubmitter{},
			Open: func(context.Context, string) (*http.Client, func() error, error) {
				if scenario == "tunnel_failed" {
					closed = true // no tunnel was constructed
					return nil, nil, errors.New("synthetic setup failure")
				}
				return &http.Client{}, func() error { closed = true; return nil }, nil
			},
		}
		ctx, cancel := context.WithCancel(context.Background())
		if scenario == "health_not_run" {
			probe.Health = nil
		}
		if scenario == "canceled" {
			cancel()
		}
		startedAt := time.Now()
		err := probe.ProbeOne(ctx, Provider{ClientId: "synthetic-provider", ClaimOrdinal: 19})
		cancel()
		if (err == nil) != (scenario == "healthy") || len(reporter.completions) != 1 || reporter.legacy != 0 || !reporter.closedFirst {
			t.Fatalf("terminal %s lost/duplicated completion or reported before close: reports=%+v legacy=%d closed=%t error=%v",
				scenario, reporter.completions, reporter.legacy, reporter.closedFirst, err)
		}
		completion := reporter.completions[0]
		if completion.ClientId != "synthetic-provider" || completion.ClaimOrdinal != 19 || !completion.AllowPacing || completion.CompletedAt.Before(startedAt) ||
			(completion.ProbeFailure == "") != (scenario == "healthy") {
			t.Fatalf("terminal completion changed its owning identity/outcome: %+v", completion)
		}
	}
}

// A claimed probe cannot silently downgrade through an old adapter. Conversely
// failure to publish its completion does not acquire negative health evidence.
func TestUrlCompletedProbeRequiresCapableAcknowledgedReporter(t *testing.T) {
	probe := &Prober{HealthResults: &stubHealthReporter{}, Open: okOpen, Health: answers(exitResult()),
		Submit: &stubSubmitter{}, Attempts: &stubAttempts{}}
	if err := probe.ProbeOne(context.Background(), Provider{ClientId: "synthetic-provider", ClaimOrdinal: 7}); !errors.Is(err, qualityprobe.ErrUrlProbeCompletionUnsupported) {
		t.Fatalf("claimed turn silently used legacy attempt: %v", err)
	}
	publicationError := errors.New("synthetic acknowledgement unavailable")
	reporter := &testingUrlCompletionReporter{err: publicationError}
	probe.Attempts = reporter
	if err := probe.ProbeOne(context.Background(), Provider{ClientId: "synthetic-provider", ClaimOrdinal: 8}); !errors.Is(err, publicationError) ||
		len(reporter.completions) != 1 || reporter.completions[0].ProbeFailure != "" {
		t.Fatalf("receipt delivery failure rewrote healthy measurement: reports=%+v error=%v", reporter.completions, err)
	}
	probe.Attempts = &stubAttempts{}
	if err := probe.ProbeOne(context.Background(), Provider{ClientId: "synthetic-provider"}); err != nil {
		t.Fatalf("manual/legacy probe lost its compatibility path: %v", err)
	}
}
