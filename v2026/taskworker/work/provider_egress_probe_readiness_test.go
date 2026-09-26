// Synthetic credit transitions exercise the real pass publication boundaries.
package work

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// A permanently unfunded prober must not turn a selected provider into a
// persisted dark result or start the independent full lane.
func TestProviderEgressProbeReadinessUnfundedPassDoesNotMeasureProviders(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	args.Blackhole.Concurrency = 1
	tunnels, submissions := 0, 0
	pass := &providerEgressProbePass{
		readiness: &providerEgressProbeReadiness{
			minimum:   1,
			available: func(context.Context) (model.ByteCount, error) { return 0, nil },
		},
		blackholeDue: func(context.Context, int) ([]ingest.DueProvider, error) {
			return testDueProviders("provider.example"), nil
		},
		fullDue: func(context.Context, int) ([]ingest.DueProvider, error) {
			return testDueProviders("provider.example"), nil
		},
		loadPins: func(context.Context) (map[string][]string, error) {
			return map[string][]string{"geo.example": {"synthetic-pin"}}, nil
		},
		runBlackhole: func(context.Context, []prober.Provider, fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			tunnels++
			return fleetprobe.BlackholeSummary{Checks: []ingest.BlackholeCheck{{ClientId: "provider.example"}}, Dark: 1}, nil
		},
		runFull: func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
			tunnels++
			return prober.Summary{Attempted: 1, Failed: 1}, nil
		},
		submitBlackholeChecks: func(context.Context, []ingest.BlackholeCheck) error { submissions++; return nil },
	}
	_, err := pass.run(context.Background(), args)
	if tunnels != 0 || submissions != 0 {
		t.Fatalf("unfunded prober measured providers: tunnels=%d published_batches=%d", tunnels, submissions)
	}
	if !errors.Is(err, errProviderEgressProbeUnfunded) {
		t.Fatalf("unfunded pass error = %v, want explicit shared funding failure", err)
	}
}

// The runner can consume the last credit after the admission check. Its
// synthetic all-dark result must not replace a provider's retained verdict.
func TestProviderEgressProbeReadinessDepletionBeforeBlackholePublication(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	available := model.ByteCount(1)
	submissions := 0
	pass := &providerEgressProbePass{
		readiness: &providerEgressProbeReadiness{
			minimum:   1,
			available: func(context.Context) (model.ByteCount, error) { return available, nil },
		},
		runBlackhole: func(context.Context, []prober.Provider, fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			available = 0
			return fleetprobe.BlackholeSummary{Checks: []ingest.BlackholeCheck{{ClientId: "provider.example"}}, Dark: 1}, nil
		},
		submitBlackholeChecks: func(context.Context, []ingest.BlackholeCheck) error { submissions++; return nil },
	}
	summary, _, err := pass.runBlackholeBatch(context.Background(), args, nil, nil, 1, fleetprobe.ProvidersFromClientIds([]string{"provider.example"}))
	if submissions != 0 || len(summary.Checks) != 0 || summary.Dark != 0 {
		t.Fatalf("credit depletion published or counted a dark provider: submissions=%d checks=%d dark=%d", submissions, len(summary.Checks), summary.Dark)
	}
	if !errors.Is(err, errProviderEgressProbeUnfunded) {
		t.Fatalf("depleted batch error = %v, want explicit shared funding failure", err)
	}
}

// Full failures affect both health and attempt backoff, so neither publisher
// may blame a provider after the shared credit observation fails.
func TestProviderEgressProbeReadinessUnfundedFullFailureIsNotPublished(t *testing.T) {
	inner := newFakeEgressProbeIngest()
	reporter := &providerEgressProbeReadinessReporter{
		egressProbeIngest: inner,
		readiness: &providerEgressProbeReadiness{
			minimum:   1,
			available: func(context.Context) (model.ByteCount, error) { return 0, nil },
		},
	}
	attemptErr := reporter.ReportAttempt(context.Background(), "provider.example", prober.FailureNoExitIp)
	healthErr := reporter.SubmitEgressHealth(context.Background(), "provider.example", &egresshealth.Result{Total: 2})
	if len(inner.attempts) != 0 || len(inner.health) != 0 {
		t.Fatalf("unfunded full probe published provider failure: attempts=%d health=%d", len(inner.attempts), len(inner.health))
	}
	if !errors.Is(attemptErr, errProviderEgressProbeUnfunded) || !errors.Is(healthErr, errProviderEgressProbeUnfunded) {
		t.Fatalf("full publisher did not expose shared funding failure: attempt=%v health=%v", attemptErr, healthErr)
	}
}

// A failed credit read has the same publication boundary as exhausted credit,
// while preserving the distinct unknown cause for diagnosis and retry policy.
func TestProviderEgressProbeReadinessUnknownFullFailureIsNotPublished(t *testing.T) {
	inner := newFakeEgressProbeIngest()
	reporter := &providerEgressProbeReadinessReporter{
		egressProbeIngest: inner,
		readiness: &providerEgressProbeReadiness{
			minimum: 1,
			available: func(context.Context) (model.ByteCount, error) {
				return 0, errors.New("synthetic credit read unavailable")
			},
		},
	}
	attemptErr := reporter.ReportAttempt(context.Background(), "provider.example", prober.FailureNoExitIp)
	healthErr := reporter.SubmitEgressHealth(context.Background(), "provider.example", &egresshealth.Result{Total: 2})
	if len(inner.attempts) != 0 || len(inner.health) != 0 {
		t.Fatal("unknown shared credit published a negative provider measurement")
	}
	if !errors.Is(attemptErr, errProviderEgressProbeFundingUnknown) || !errors.Is(healthErr, errProviderEgressProbeFundingUnknown) ||
		errors.Is(attemptErr, errProviderEgressProbeUnfunded) || errors.Is(healthErr, errProviderEgressProbeUnfunded) {
		t.Fatalf("unknown credit gained a financial verdict: attempt=%v health=%v", attemptErr, healthErr)
	}
}

// Ordinary provider failure remains publishable while the shared account can
// fund its initial contract; timeout text alone never activates this guard.
func TestProviderEgressProbeReadinessFundedDarkMeasurementStillPublishes(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	inner := newFakeEgressProbeIngest()
	pass := &providerEgressProbePass{
		readiness: &providerEgressProbeReadiness{
			minimum:   1,
			available: func(context.Context) (model.ByteCount, error) { return 1, nil },
		},
		runBlackhole: func(context.Context, []prober.Provider, fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			return fleetprobe.BlackholeSummary{Checks: []ingest.BlackholeCheck{{ClientId: "provider.example", Failure: egresshealth.FailureAllDestinationsFailed}}, Dark: 1}, nil
		},
		submitBlackholeChecks: inner.SubmitBlackholeChecks,
	}
	summary, _, err := pass.runBlackholeBatch(context.Background(), args, nil, nil, 1, fleetprobe.ProvidersFromClientIds([]string{"provider.example"}))
	if err != nil || len(inner.blackhole) != 1 || summary.Dark != 1 {
		t.Fatalf("funded provider failure was suppressed: published=%d dark=%d error=%v", len(inner.blackhole), summary.Dark, err)
	}
	reporter := &providerEgressProbeReadinessReporter{egressProbeIngest: inner, readiness: pass.readiness}
	if err := reporter.ReportAttempt(context.Background(), "provider.example", prober.FailureNoExitIp); err != nil {
		t.Fatal(err)
	}
	if err := reporter.SubmitEgressHealth(context.Background(), "provider.example", &egresshealth.Result{Total: 2}); err != nil {
		t.Fatal(err)
	}
	if len(inner.attempts) != 1 || len(inner.health) != 1 {
		t.Fatal("funded full failure did not retain its ordinary health and backoff observations")
	}
}

// A shared funding outage cannot invalidate observed traffic or a verified
// TLS integrity failure. Those independent facts still advance provider state.
func TestProviderEgressProbeReadinessRetainsPositiveAndTlsEvidence(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	available := model.ByteCount(1)
	inner := newFakeEgressProbeIngest()
	pass := &providerEgressProbePass{
		readiness: &providerEgressProbeReadiness{
			minimum:   1,
			available: func(context.Context) (model.ByteCount, error) { return available, nil },
		},
		runBlackhole: func(context.Context, []prober.Provider, fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			available = 0
			return fleetprobe.BlackholeSummary{Checks: []ingest.BlackholeCheck{
				{ClientId: "healthy.example", Ok: true},
				{ClientId: "tls.example", Failure: egresshealth.FailureTlsAuthentication},
				{ClientId: "unknown.example", Failure: egresshealth.FailureAllDestinationsFailed},
			}, Dark: 2}, nil
		},
		submitBlackholeChecks: inner.SubmitBlackholeChecks,
	}
	summary, _, err := pass.runBlackholeBatch(context.Background(), args, nil, nil, 1, fleetprobe.ProvidersFromClientIds([]string{"healthy.example", "tls.example", "unknown.example"}))
	if !errors.Is(err, errProviderEgressProbeUnfunded) || len(inner.blackhole) != 2 || summary.Dark != 1 ||
		inner.blackhole[0].ClientId != "healthy.example" || inner.blackhole[1].ClientId != "tls.example" {
		t.Fatalf("independent blackhole evidence changed: published=%+v summary=%+v error=%v", inner.blackhole, summary, err)
	}
	reporter := &providerEgressProbeReadinessReporter{egressProbeIngest: inner, readiness: pass.readiness}
	if err := reporter.Submit(context.Background(), "healthy.example", "203.0.113.7", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := reporter.ReportAttempt(context.Background(), "healthy.example", ""); err != nil {
		t.Fatal(err)
	}
	for _, result := range []*egresshealth.Result{{Total: 2, OkCount: 2}, {Total: 2, TlsAuthenticationFailure: true}, nil} {
		if err := reporter.SubmitEgressHealth(context.Background(), "healthy.example", result); err != nil {
			t.Fatal(err)
		}
	}
	if len(inner.submitted) != 1 || len(inner.attempts) != 1 || inner.healthCalls != 3 {
		t.Fatal("shared funding failure suppressed successful location/attempt, positive health, TLS integrity, or nil forwarding")
	}
}

// The full prober treats report errors as nonfatal. Its owning durable pass
// must still retain a latched shared failure after that runner returns.
func TestProviderEgressProbeReadinessFullRunnerCannotSwallowFundingFailure(t *testing.T) {
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	available := model.ByteCount(1)
	readiness := &providerEgressProbeReadiness{
		minimum:   1,
		available: func(context.Context) (model.ByteCount, error) { return available, nil },
	}
	inner := newFakeEgressProbeIngest()
	reporter := &providerEgressProbeReadinessReporter{egressProbeIngest: inner, readiness: readiness}
	pass := &providerEgressProbePass{
		readiness: readiness,
		runFull: func(ctx context.Context, _ []prober.Provider, _ fleetprobe.FullOptions) (prober.Summary, error) {
			available = 0
			_ = reporter.ReportAttempt(ctx, "provider.example", prober.FailureNoExitIp)
			return prober.Summary{Attempted: 1, Failed: 1}, nil
		},
	}
	outcome := pass.runFullBatch(context.Background(), args, nil, nil, testDueProviders("provider.example"))
	if !errors.Is(outcome.err, errProviderEgressProbeUnfunded) || outcome.summary.Attempted != 1 || len(inner.attempts) != 0 {
		t.Fatalf("full runner lost shared failure or published provider backoff: outcome=%+v attempts=%d", outcome, len(inner.attempts))
	}
}

// Unknown datastore state cannot become funded or overwrite provider history;
// only an independent new pass can observe later recovery.
func TestProviderEgressProbeReadinessUnknownAndRecoveryRemainDistinct(t *testing.T) {
	reads := 0
	readiness := &providerEgressProbeReadiness{
		minimum: 1,
		available: func(context.Context) (model.ByteCount, error) {
			reads++
			return 0, errors.New("synthetic unavailable datastore")
		},
	}
	if err := readiness.check(context.Background()); !errors.Is(err, errProviderEgressProbeFundingUnknown) || errors.Is(err, errProviderEgressProbeUnfunded) {
		t.Fatalf("unknown readiness was assigned a funding verdict: %v", err)
	}
	readiness.available = func(context.Context) (model.ByteCount, error) { reads++; return 1, nil }
	if err := readiness.check(context.Background()); !errors.Is(err, errProviderEgressProbeFundingUnknown) || reads != 1 {
		t.Fatalf("same-pass recovery validated earlier unknown measurements: error=%v reads=%d", err, reads)
	}
	recovered := &providerEgressProbeReadiness{minimum: 1, available: readiness.available}
	if err := recovered.check(context.Background()); err != nil || reads != 2 {
		t.Fatalf("new pass could not observe funding recovery: error=%v reads=%d", err, reads)
	}
}

// Equality meets admission; cancellation never becomes an account failure.
func TestProviderEgressProbeReadinessMinimumAndCancellation(t *testing.T) {
	for _, available := range []model.ByteCount{-1, 0, 1023, 1024, 1025} {
		readiness := &providerEgressProbeReadiness{
			minimum:   1024,
			available: func(context.Context) (model.ByteCount, error) { return available, nil },
		}
		err := readiness.check(context.Background())
		if errors.Is(err, errProviderEgressProbeUnfunded) != (available < 1024) {
			t.Errorf("synthetic available=%d minimum boundary error=%v", available, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	readiness := &providerEgressProbeReadiness{available: func(context.Context) (model.ByteCount, error) {
		t.Fatal("canceled readiness called the datastore")
		return 0, nil
	}}
	if err := readiness.check(ctx); !errors.Is(err, context.Canceled) || readiness.err() != nil {
		t.Fatalf("cancellation became an account failure: %v", err)
	}
}

// A joined failure must not use the funding cadence to shorten an unrelated
// transport, unknown-readiness or cancellation backoff.
func TestProviderEgressProbeReadinessRetryClassifiesTheCompleteError(t *testing.T) {
	for _, err := range []error{errProviderEgressProbeUnfunded, fmt.Errorf("batch: %w", errProviderEgressProbeUnfunded), errors.Join(errProviderEgressProbeUnfunded, errProviderEgressProbeUnfunded)} {
		if !providerEgressProbeUnfundedOnly(err) {
			t.Errorf("funding-only failure lost retry classification: %v", err)
		}
	}
	for _, err := range []error{nil, errProviderEgressProbeFundingUnknown, context.Canceled, errors.New("synthetic transport failure"), errors.Join(errProviderEgressProbeUnfunded, errProviderEgressProbeFundingUnknown), errors.Join(errProviderEgressProbeUnfunded, context.Canceled)} {
		if providerEgressProbeUnfundedOnly(err) {
			t.Errorf("unrelated or incomplete failure borrowed funding retry: %v", err)
		}
	}
}

// Drive the real task evaluator at an accumulated error count that would
// otherwise produce hour-scale backoff. Assert stored timestamps, never sleeps.
func TestProviderEgressProbeReadinessUnfundedTaskKeepsIdleRetryCadence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		settings := testProviderEgressProbeSettings(1)
		withProviderEgressProbeSettings(t, settings)
		previous := executeProviderEgressProbe
		executeProviderEgressProbe = func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
			return nil, errProviderEgressProbeUnfunded
		}
		t.Cleanup(func() { executeProviderEgressProbe = previous })
		oldBase, oldCap := task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout
		task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout = 2*time.Second, time.Hour
		t.Cleanup(func() { task.RescheduleTimeout, task.RescheduleBackoffMaxTimeout = oldBase, oldCap })
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		defer clientSession.Cancel()
		args := providerEgressProbeArgs(settings, 0)
		server.Tx(ctx, func(tx server.PgTx) { scheduleProviderEgressProbeAt(clientSession, tx, args, server.NowUtc()) })
		target := task.NewTaskTargetWithPost(ProviderEgressProbe, ProviderEgressProbePost)
		var taskId server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT task_id FROM pending_task WHERE function_name=$1`, target.TargetFunctionName()).Scan(&taskId))
		})
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET run_at=$2,release_time=$2,reschedule_error_count=16 WHERE task_id=$1`, taskId, time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)))
		})
		worker := task.NewTaskWorkerWithDefaults(ctx)
		worker.AddTargets(target)
		finished, rescheduled, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 0 || len(posts) != 0 || len(rescheduled) != 1 || rescheduled[0] != taskId {
			t.Fatal("unfunded task was completed or replaced instead of retained")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var runAt, releaseAt time.Time
			var errorCount int
			var storedError string
			server.Raise(conn.QueryRow(ctx, `SELECT run_at,release_time,reschedule_error_count,reschedule_error FROM pending_task WHERE task_id=$1`, taskId).Scan(&runAt, &releaseAt, &errorCount, &storedError))
			if runAt.Sub(releaseAt) != time.Duration(args.IdleDelaySeconds)*time.Second || errorCount != 17 || !strings.Contains(storedError, errProviderEgressProbeUnfunded.Error()) {
				t.Fatalf("funding retry lost idle cadence or visible failure: delay=%s error_count=%d", runAt.Sub(releaseAt), errorCount)
			}
		})
		if len(task.GetFinishedTasks(ctx, taskId)) != 0 {
			t.Fatal("unfunded task was falsely recorded as complete")
		}
	})
}
