// Real intake/source/host authority composes with retained admission bytes at
// the public Info and Ready boundaries, including another CPU's roundoff.
package controller

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The source and round are installed through their real guarded lifecycle;
// only execution/retention producers are visibly synthetic test fixtures.
func TestCompetitionDistributedProbabilityPgPublicProjectionAuthenticatesRoundedException(t *testing.T) {
	t.Setenv(runtimeImageDigestEnvironment, testWorkerImageDigest())
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		fixture := newCompetitionIntakeFirstDatabaseFixture(t, ctx, func(settings *Settings) {
			settings.FutureEpochPolicy.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(tb)
		})
		fixture.handoffAndPublish(tb, ctx)
		fixture.acknowledgeApi(tb, ctx)
		if _, err := fixture.store.ActivateSourceIntake(ctx, fixture.settings, fixture.lease, 2); err != nil {
			tb.Fatal(err)
		}
		opened, err := fixture.store.CreateIntakeFirstRound(ctx, fixture.settings, fixture.lease, 2)
		if err != nil {
			tb.Fatal(err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			fixture.now, err = competitionIntakeFirstDatabaseTime(ctx, conn)
			server.Raise(err)
		})
		service := newServiceWithImageDigest(fixture.settings, fixture.store, testApiImageDigest(), nil)
		accepted, code, evalError := service.SubmitV2(ctx, ScoreArgs{RoundId: opened.RoundId, Patch: testPatch("synthetic-probability-public-projection")}, &Principal{Id: "synthetic-submitter", Role: "submitter"})
		if evalError != nil || code != 202 || accepted == nil {
			tb.Fatal("genuine intake could not retain its source admission", code, evalError)
		}
		worker := competitionPreparationTestReceipt(fixture.intent, "worker", "synthetic-probability-worker", testWorkerImageDigest())
		if err := fixture.store.RecordSourceReadiness(ctx, fixture.settings, worker); err != nil {
			tb.Fatal(err)
		}
		if _, err := fixture.store.ActivateSource(ctx, fixture.settings, fixture.lease, 2, 1); err != nil {
			tb.Fatal(err)
		}
		round, err := fixture.store.GetRound(ctx, fixture.settings, opened.RoundId)
		if err != nil {
			tb.Fatal(err)
		}
		frozen, err := settingsForCompetitionApiRound(ctx, fixture.settings, fixture.store, round)
		if err != nil {
			tb.Fatal(err)
		}
		frozen.automationLease, frozen.automationRoundId = &fixture.lease, round.RoundId
		frozen.roundBaselinePreparation = &roundBaselinePreparationProfile{Settings: defaultRoundBaselinePreparationSettings()}
		preparation, err := fixture.store.ReserveRoundBaseline(ctx, frozen, "synthetic-probability-coordinator", testWorkerImageDigest())
		if err != nil || preparation == nil {
			tb.Fatal("genuine queued source did not reserve its control", err)
		}
		prepared := preparedRoundBaselineTest(tb, frozen, preparation)
		if err := fixture.store.PublishPreparedRoundBaseline(ctx, frozen, preparation, prepared); err != nil {
			tb.Fatal(err)
		}
		baseline, err := fixture.store.distributedRoundBaseline(ctx, frozen, round.RoundId)
		if err != nil || baseline == nil {
			tb.Fatal("published control was not available", err)
		}
		var control roundBaselineManifest
		if err := json.Unmarshal(baseline.Json, &control); err != nil {
			tb.Fatal(err)
		}
		var exceptedRetention *retainedArtifact
		for index, host := range frozen.DistributedEvaluationPolicy.Hosts {
			jobId := competitionExecutorQualificationJobId(round.RoundId, host.HostId, frozen.DistributedEvaluationPolicy.ApprovalSha256)
			run, err := fixture.store.reserveDistributedExecutorQualification(ctx, frozen, round.RoundId, host.HostId, jobId)
			if err != nil {
				tb.Fatal(err)
			}
			receipt := &competitionExecutorEquivalenceReceipt{Schema: 1, Kind: "competition-executor-noop-admission/v1", RoundId: round.RoundId.String(), HostId: host.HostId,
				BaselineSha256: baseline.Sha256, SourceLockSha256: frozen.AutomationPolicy.SourceLockSha256, ProvidersSha256: round.ProvidersSha256,
				EvaluatorImageDigest: frozen.EvaluatorImageDigest, QualificationSha256: host.QualificationSha256, ConfigLocalSha256: frozen.EvaluationPolicy.ConfigLocalSha256, VaultLocalSha256: frozen.EvaluationPolicy.VaultLocalSha256,
				PolicyApprovalSha256: frozen.DistributedEvaluationPolicy.ApprovalSha256, QualificationJobId: run.JobId.String(), QualificationStartedAt: run.StartedAt, QualificationDeadlineAt: run.DeadlineAt,
				Noop: true, Passed: index != 0, Replicates: 9, ScreenKind: "operational-admission/v1", MaximumRawScoreRelativeDifference: 0.05, ConfidenceLevel: 0.95,
				StatisticalMethod: "two-sided-welch-t-run-level", ArchiveManifestSha256: strings.Repeat("a", 64), CreatedAt: run.StartedAt, WorkCoveragePolicy: frozen.EvaluationPolicy.WorkCoveragePolicy.Clone()}
			for _, name := range competitionNoopMetricNames(frozen) {
				values, withinSe := []float64{}, []float64{}
				for _, replicate := range control.Replicates {
					var value float64
					switch name {
					case "raw_score":
						value = replicate.RawScore
					case "request_count":
						value = float64(replicate.RequestCount)
					case "received_bytes":
						value = float64(replicate.ReceivedBytes)
					case "page_coverage":
						value = replicate.WorkCoverage.PageCoverage
					case "byte_coverage":
						value = replicate.WorkCoverage.ByteCoverage
					default:
						value = replicate.LiveMetrics[name].Value
						withinSe = append(withinSe, replicate.LiveMetrics[name].BlockSe)
					}
					values = append(values, value)
				}
				candidate := append([]float64(nil), values...)
				for sampleIndex := range candidate {
					if name == "raw_score" {
						candidate[sampleIndex] += 0.01
					} else if index == 0 && name == "throughput_p50_bytes_per_s" {
						candidate[sampleIndex] *= 0.5
					}
				}
				metric, err := compareCompetitionHostMetric(name, values, candidate)
				if err != nil {
					tb.Fatal(err)
				}
				metric.BaselineWithinRunSe, metric.CandidateWithinRunSe = withinSe, append([]float64(nil), withinSe...)
				if name == "raw_score" {
					if metric.SignificantShift || metric.TwoSidedPValue <= 0 || metric.TwoSidedPValue >= 1 {
						tb.Fatal("synthetic raw-score diagnostic did not remain passing")
					}
					receipt.RawScoreRelativeDifference = math.Abs(metric.Difference) / metric.BaselineMean
					if index == 0 {
						for range 4 {
							metric.TwoSidedPValue = math.Nextafter(metric.TwoSidedPValue, 1)
						}
					}
				}
				receipt.Metrics = append(receipt.Metrics, *metric)
			}
			document, err := competitionExecutorReceiptBytes(receipt)
			if err != nil {
				tb.Fatal(err)
			}
			qualification := *preparation.SourceJob
			qualification.JobId, qualification.AttemptCount = run.JobId, 1
			qualification.StartedAt, qualification.ExecutionDeadline = &run.StartedAt, &run.DeadlineAt
			receipt.Retention, err = fixture.archive.ArchiveExecutorQualification(ctx, frozen, &qualification, document)
			if err != nil {
				tb.Fatal(err)
			}
			if index == 0 {
				exceptedRetention = receipt.Retention
				approval := CompetitionExecutorAdmissionException{Schema: 1, Kind: "executor-retained-admission-exception/v1", RoundId: round.RoundId, HostId: host.HostId, JobId: run.JobId,
					BaselineSha256: baseline.Sha256, ReceiptDocumentSha256: receipt.Retention.Sha256, ExceptedMetric: "throughput_p50_bytes_per_s", EvidenceSha256: strings.Repeat("b", 64)}
				content, err := json.Marshal(receipt)
				if err != nil {
					tb.Fatal(err)
				}
				var record map[string]any
				if err := json.Unmarshal(content, &record); err != nil {
					tb.Fatal(err)
				}
				// Model a receipt authored on another CPU: real SQL guards admit
				// its exact canonical document before this API reauthenticates it.
				if err := insertAdmissionExceptionPg(tb, &competitionDistributedPgFixture{settings: frozen, store: fixture.store}, approval, record, true); err != nil {
					tb.Fatal(err)
				}
			} else if err := fixture.store.recordDistributedExecutorQualification(ctx, frozen, round, baseline, host, receipt); err != nil {
				tb.Fatal(err)
			}
			selected := *frozen
			selected.executorHostId, selected.executorSlotId = host.HostId, host.SlotId
			check := passingHostCheck(&selected)
			check.HostId, check.QualificationSha256 = host.HostId, host.QualificationSha256
			check.RebaselinePassed, check.RebaselineRoundId = true, &round.RoundId
			if err := fixture.store.RegisterHost(ctx, &selected, check); err != nil {
				tb.Fatal(err)
			}
		}
		info, evalError := service.Info(ctx)
		if evalError != nil || info == nil || info.RuntimeStatus == nil || !info.RuntimeStatus.IntakeReady || !info.RuntimeStatus.GradingReady || info.RuntimeStatus.PublicationHeld {
			tb.Fatal("rounded retained exception broke the composed public projection", info, evalError)
		}
		ready, evalError := service.Ready(ctx)
		if evalError != nil || !ready.Ready || !allChecks(ready.Checks) || ready.RuntimeStatus == nil || !ready.RuntimeStatus.GradingReady {
			tb.Fatal("rounded retained exception broke real readiness", ready, evalError)
		}
		// This is the exact object version, not the separately retained source
		// envelope. Corruption must remain fatal after the roundoff fix.
		blobs := fixture.archive.store.(*competitionRuntimeBlobTestStore)
		blobs.stateLock.Lock()
		blobs.versions[exceptedRetention.Key+"\x00"+exceptedRetention.VersionId] = []byte("synthetic-corrupt-retained-qualification")
		blobs.stateLock.Unlock()
		if info, evalError := service.Info(ctx); evalError == nil || info != nil {
			tb.Fatal("public discovery waived retained qualification authentication")
		}
		if ready, evalError := service.Ready(ctx); evalError == nil || ready.Ready || ready.RuntimeStatus != nil {
			tb.Fatal("public readiness waived retained qualification authentication")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var started int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_job WHERE round_id=$1 AND started_at IS NOT NULL`, round.RoundId).Scan(&started))
			if started != 0 {
				tb.Fatal("readiness authentication started a candidate clock", started)
			}
		})
	})
}
