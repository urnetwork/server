// URL deadline draining and failure scope preserve owned turns and accepted data.
package work

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

// Reject only the selected turn's final acknowledgment, not its sibling.
type urlProbeRejectedPublication struct {
	egressProbeIngest
	health bool
}

func (self *urlProbeRejectedPublication) SubmitEgressHealth(ctx context.Context, id string, result *egresshealth.Result) error {
	if self.health && id == "synthetic-rejected" {
		return errors.New("synthetic health acknowledgment failure")
	}
	return self.egressProbeIngest.SubmitEgressHealth(ctx, id, result)
}

func (self *urlProbeRejectedPublication) ReportAttempt(ctx context.Context, id, failure string) error {
	if !self.health && id == "synthetic-rejected" {
		return errors.New("synthetic attempt acknowledgment failure")
	}
	return self.egressProbeIngest.ReportAttempt(ctx, id, failure)
}

// Crossing the admission cutoff stops refill, not completion. The owner must
// join its remaining tail and return immediately, without sleeping to deadline.
func TestUrlProbeDeadlineJoinsTailWithoutWaitingForOwnerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pass, args, _ := testUrlProbePass()
		args.UrlProbe.Limit, args.UrlProbe.Concurrency = 2, 2
		window := 4 * time.Second
		reserve := providerUrlProbeRunBudget(args) + 3*providerEgressControlPlaneTimeout
		startedAt := time.Now()
		claimed := 0
		pass.fullDue = func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
			if time.Since(startedAt) > window {
				t.Error("claimed another turn after its run/publication reserve was lost")
				return nil, nil
			}
			due := testDueProviders("synthetic-fast")
			if claimed == 0 {
				due = append(due, ingest.DueProvider{ClientId: "synthetic-tail"})
			}
			if len(due) > limit {
				t.Error("synthetic due fixture exceeded free worker slots")
			}
			claimed += len(due)
			return due, nil
		}
		tail := make(chan struct{})
		pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			id := providers[0].ClientId
			if id == "synthetic-tail" {
				<-tail
			} else {
				time.Sleep(time.Second)
			}
			if err := options.HealthResults.SubmitEgressHealth(ctx, id, testHealthRun(1, 1)); err != nil {
				return prober.Summary{}, err
			}
			if err := options.Attempts.ReportAttempt(ctx, id, ""); err != nil {
				return prober.Summary{}, err
			}
			return prober.Summary{Attempted: 1, Submitted: 1}, nil
		}
		ctx, cancel := context.WithTimeout(t.Context(), reserve+window)
		defer cancel()
		var result *ProviderEgressProbeResult
		var runErr error
		done := make(chan struct{})
		go func() { defer close(done); result, runErr = pass.run(ctx, args) }()
		time.Sleep(window + time.Second)
		synctest.Wait()
		select {
		case <-done:
			t.Error("owner returned before its admitted tail finished")
		default:
		}
		beforeRelease, beforeClaims := time.Now(), claimed
		close(tail)
		<-done
		if runErr != nil || result == nil || result.Attempted != claimed || result.Submitted != claimed ||
			claimed != beforeClaims || ctx.Err() != nil || !time.Now().Equal(beforeRelease) {
			t.Fatalf("deadline drain delayed completion or changed accepted work: result=%+v claims=%d error=%v", result, claimed, runErr)
		}
	})
}

// Publication failure stops future claims but still joins and publishes an
// independently successful sibling. Attempt rejection cannot revoke its health.
func testUrlProbePublicationStopsRefill(t *testing.T, health bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		pass, args, inner := testUrlProbePass()
		args.UrlProbe.Limit, args.UrlProbe.Concurrency = 2, 2
		pass.fullSink = testFullBatchSink(&urlProbeRejectedPublication{egressProbeIngest: inner, health: health})
		claims := 0
		pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			claims++
			if claims > 1 {
				return nil, nil
			}
			return testDueProviders("synthetic-rejected", "synthetic-tail"), nil
		}
		tail := make(chan struct{})
		pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			id := providers[0].ClientId
			if id == "synthetic-tail" {
				<-tail
			}
			if err := options.HealthResults.SubmitEgressHealth(ctx, id, testHealthRun(1, 1)); err != nil {
				return prober.Summary{}, err
			}
			if err := options.Attempts.ReportAttempt(ctx, id, ""); err != nil {
				return prober.Summary{}, err
			}
			return prober.Summary{Attempted: 1, Submitted: 1}, nil
		}
		var result *ProviderEgressProbeResult
		var runErr error
		done := make(chan struct{})
		go func() { defer close(done); result, runErr = pass.run(t.Context(), args) }()
		synctest.Wait()
		select {
		case <-done:
			t.Error("publication failure abandoned its admitted sibling")
		default:
		}
		if claims != 1 {
			t.Error("publication failure admitted another turn")
		}
		close(tail)
		<-done
		failed := 0
		if health {
			failed = 1
		}
		if runErr == nil || result == nil || claims != 1 || result.Attempted != 2 ||
			result.Submitted != 2-failed || result.Failed != failed || inner.health["synthetic-tail"] == nil ||
			(inner.health["synthetic-rejected"] == nil) != health {
			t.Fatalf("publication failure changed accepted data or refill scope: health=%t result=%+v claims=%d error=%v", health, result, claims, runErr)
		}
	})
}

func TestUrlProbeHealthPublicationFailureStopsRefillAndPreservesSibling(t *testing.T) {
	testUrlProbePublicationStopsRefill(t, true)
}

func TestUrlProbeAttemptPublicationFailureStopsRefillWithoutRevokingHealth(t *testing.T) {
	testUrlProbePublicationStopsRefill(t, false)
}

// A local tunnel-open failure does not become a shared task failure, and a
// measured negative website result remains an acknowledged denominator.
func TestUrlProbeProviderFailureRefillsAndPreservesMeasuredError(t *testing.T) {
	pass, args, inner := testUrlProbePass()
	args.UrlProbe.Limit, args.UrlProbe.Concurrency = 1, 1
	claimed := 0
	pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
		claimed++
		switch claimed {
		case 1:
			return testDueProviders("synthetic-open-failure"), nil
		case 2:
			return testDueProviders("synthetic-measured-error"), nil
		default:
			return nil, nil
		}
	}
	pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
		owner := &prober.Prober{
			Open: func(_ context.Context, id string) (*http.Client, func() error, error) {
				if id == "synthetic-open-failure" {
					return nil, nil, errors.New("synthetic provider open failure")
				}
				return &http.Client{}, func() error { return nil }, nil
			},
			Health: func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
				return testHealthRun(1, 0), nil
			},
			HealthResults: options.HealthResults,
			Attempts:      options.Attempts,
		}
		return (&prober.Scheduler{Prober: owner, Concurrency: 1}).Run(ctx, providers), nil
	}
	result, err := pass.run(t.Context(), args)
	measured := inner.health["synthetic-measured-error"]
	if err != nil || result.Attempted != 2 || result.Submitted != 1 || result.Failed != 1 || result.UrlDue != 2 ||
		measured == nil || measured.Total != 1 || measured.OkCount != 0 || inner.health["synthetic-open-failure"] != nil {
		t.Fatalf("provider-local failure stopped refill or rejected a measured error: result=%+v error=%v", result, err)
	}
}
