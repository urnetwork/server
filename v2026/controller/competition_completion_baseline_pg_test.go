// Prepared intake controls are consumed by real fenced completions, never
// republished by the candidate. Every lifecycle and baseline guard stays on.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// Real source handoff, protected admissions, runtime quorum and separate
// reserved publication precede synthetic no-op receipts. No trigger is disabled.
func newCompetitionPreparedIntakeCompletionFixture(t *testing.T, tb testing.TB, distributed bool) *competitionDistributedPgFixture {
	tb.Helper()
	ctx := tb.Context()
	intake := newCompetitionIntakeFirstDatabaseFixture(t, ctx, func(settings *Settings) {
		if distributed {
			settings.FutureEpochPolicy.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(tb)
		}
	})
	intake.handoffAndPublish(tb, ctx)
	intake.acknowledgeApi(tb, ctx)
	if _, err := intake.store.ActivateSourceIntake(ctx, intake.settings, intake.lease, 2); err != nil {
		tb.Fatal(err)
	}
	opened, err := intake.store.CreateIntakeFirstRound(ctx, intake.settings, intake.lease, 2)
	if err != nil {
		tb.Fatal(err)
	}
	server.Db(ctx, func(conn server.PgConn) {
		intake.now, err = competitionIntakeFirstDatabaseTime(ctx, conn)
		server.Raise(err)
	})
	service := newServiceWithImageDigest(intake.settings, intake.store, testApiImageDigest(), nil)
	for _, name := range []string{"synthetic-prepared-source", "synthetic-prepared-next"} {
		accepted, code, evalError := service.SubmitV2(ctx, ScoreArgs{RoundId: opened.RoundId, Patch: testPatch(name)}, &Principal{Id: "synthetic-submitter", Role: "submitter"})
		if evalError != nil || code != 202 || accepted == nil {
			tb.Fatal("genuine intake did not admit its clockless candidate", code, evalError)
		}
	}
	worker := competitionPreparationTestReceipt(intake.intent, "worker", "synthetic-prepared-runtime", testWorkerImageDigest())
	if err := intake.store.RecordSourceReadiness(ctx, intake.settings, worker); err != nil {
		tb.Fatal(err)
	}
	if _, err := intake.store.ActivateSource(ctx, intake.settings, intake.lease, 2, 1); err != nil {
		tb.Fatal(err)
	}
	round, err := intake.store.GetRound(ctx, intake.settings, opened.RoundId)
	if err != nil {
		tb.Fatal(err)
	}
	return finishCompetitionPreparedCompletionFixture(tb, intake, round, distributed, false)
}

// Announced and ASAP fixtures share only the guarded baseline/host bootstrap.
// Their distinct source activation, opening and admission paths stay explicit.
func finishCompetitionPreparedCompletionFixture(tb testing.TB, intake *competitionIntakeFirstDatabaseFixture, round *roundRecord, distributed, historical bool) *competitionDistributedPgFixture {
	tb.Helper()
	ctx := tb.Context()
	frozen, err := settingsForCompetitionApiRound(ctx, intake.settings, intake.store, round)
	if err != nil {
		tb.Fatal(err)
	}
	frozen.automationLease, frozen.automationRoundId = &intake.lease, round.RoundId
	frozen.roundBaselinePreparation = &roundBaselinePreparationProfile{
		Config: roundBaselinePreparationConfig{Schema: 1, Kind: "sim-latency-round-baseline-preparation-config",
			G2VolumePolicy: frozen.EvaluationPolicy.G2VolumePolicy, WorkCoveragePolicy: frozen.EvaluationPolicy.WorkCoveragePolicy.Clone()},
		Settings: defaultRoundBaselinePreparationSettings(),
	}
	preparation, err := intake.store.ReserveRoundBaseline(ctx, frozen, "synthetic-prepared-coordinator", testWorkerImageDigest())
	if err != nil || preparation == nil {
		tb.Fatal("genuine reserved queued source did not own baseline preparation", err)
	}
	prepared := preparedRoundBaselineTest(tb, frozen, preparation)
	if err := intake.store.PublishPreparedRoundBaseline(ctx, frozen, preparation, prepared); err != nil {
		tb.Fatal("genuine reserved queued source could not publish its control", err)
	}
	clock := &competitionLeaseRetryClock{now: intake.now}
	fixture := &competitionDistributedPgFixture{settings: frozen, startupSettings: intake.settings, store: intake.store, clock: clock, round: round,
		sourceJob: preparation.SourceJob, preparation: preparation, prepared: prepared}
	fixture.store.now = clock.Now
	if distributed {
		// SQL independently bounds actual qualification publication. Synthetic
		// original candidate history must not backdate that live SQL authority.
		originalNow := clock.Now()
		if historical {
			var currentNow time.Time
			server.Db(ctx, func(conn server.PgConn) {
				currentNow, err = competitionIntakeFirstDatabaseTime(ctx, conn)
				server.Raise(err)
			})
			clock.Advance(currentNow.Sub(clock.Now()))
		}
		for _, host := range frozen.DistributedEvaluationPolicy.Hosts {
			fixture.qualifyHost(tb, host)
		}
		clock.Advance(originalNow.Sub(clock.Now()))
	} else {
		check := passingHostCheck(frozen)
		check.RebaselinePassed, check.RebaselineRoundId = true, &round.RoundId
		if err := fixture.store.RegisterHost(ctx, frozen, check); err != nil {
			tb.Fatal(err)
		}
	}
	return fixture
}

// The score retains every required future-policy gate and the claimed shared digest.
func competitionPreparedIntakeCompletionOutcome(t testing.TB, settings *Settings, job *queuedJob) EvaluationOutcome {
	t.Helper()
	outcome := testSharedBaselineOutcome(t, settings, job, EvaluationOutcome{Score: competitionRankingScore(90, 110), distributedArchiveVerified: true})
	outcome.Score.Diagnostics["g2_volume_policy"] = string(settings.EvaluationPolicy.G2VolumePolicy)
	outcome.Score.Diagnostics["scorer_version"] = settings.scorerVersion()
	outcome.Score.Diagnostics["work_coverage_policy"] = settings.EvaluationPolicy.WorkCoveragePolicy
	outcome.Score.Gates = map[string]Gate{}
	for _, name := range []string{"G1_success", "G2_volume", "G3_path_integrity", "G4_matchmaking", "G5_stability", "G6_resources", "G7_work_coverage"} {
		outcome.Score.Gates[name] = Gate{Passed: true, Details: map[string]any{}}
	}
	outcome.Score.Gates["G7_work_coverage"] = Gate{Passed: true, Details: map[string]any{"policy": settings.EvaluationPolicy.WorkCoveragePolicy}}
	return outcome
}

// Entire retained baseline metadata and both current authorities are observed together.
type competitionPreparedCompletionSnapshot struct {
	jobJson, slotJson, baselineJson []byte
	events                          int
}

// This read owns no resource across the actual completion invocation.
func readCompetitionPreparedCompletionSnapshot(ctx context.Context, job *queuedJob, distributed bool) (snapshot competitionPreparedCompletionSnapshot) {
	slotTable := "competition_worker_slot"
	if distributed {
		slotTable = "competition_execution_slot"
	}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT (SELECT to_jsonb(j) FROM competition_job j WHERE j.job_id=$1),
		 (SELECT to_jsonb(s) FROM `+slotTable+` s WHERE s.slot_id=$3),
		 (SELECT to_jsonb(b) FROM competition_round_baseline b WHERE b.round_id=$2),
		 (SELECT count(*) FROM competition_job_event WHERE job_id=$1)`, job.JobId, job.RoundId, max(1, job.SlotId)).
			Scan(&snapshot.jobJson, &snapshot.slotJson, &snapshot.baselineJson, &snapshot.events))
	})
	return snapshot
}

// PostgreSQL's real before-insert guard also rejects ON CONFLICT no-ops. This
// reproduces the old statement while proving the fix does not relax the guard.
func requirePreparedBaselineInsertRejection(t testing.TB, fixture *competitionDistributedPgFixture, settings *Settings, job *queuedJob, outcome EvaluationOutcome) {
	t.Helper()
	ctx := withCompetitionJobAuthority(t.Context(), job)
	manifest := sha256.Sum256(outcome.ArtifactManifest)
	err := captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(fixture.store.requireAutomationLeaseInTx(ctx, tx, settings, *settings.automationLease))
			server.Raise(requireCompetitionJobAuthorityInTx(ctx, tx, job.JobId, job.EvaluationCycle, job.AttemptCount))
			server.Raise(requireCompetitionExecutorAuthorityInTx(ctx, tx, settings, job.JobId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_round_baseline
			 (round_id,baseline_json,baseline_sha256,source_job_id,source_attempt,source_artifact_manifest_sha256,
			 base_sha,providers_sha256,evaluator_image_digest,scorer_version,hardware_id,host_qualification_sha256,created_at)
			 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT(round_id) DO NOTHING`,
				job.RoundId, outcome.RoundBaselineJson, outcome.RoundBaselineSha256, job.JobId, job.AttemptCount, hex.EncodeToString(manifest[:]),
				settings.BaseSha, job.Round.ProvidersSha256, settings.EvaluatorImageDigest, settings.scorerVersion(),
				settings.EvaluationPolicy.HardwareId, settings.EvaluationPolicy.HostQualificationSha256, fixture.clock.Now()))
		})
	})
	var postgres *pgconn.PgError
	if !errors.As(err, &postgres) || postgres.Code != "P0001" || !strings.Contains(postgres.Message, "intake baseline requires its genuine reserved queued source") {
		t.Fatal("old insert shape did not encounter the genuine reserved-source guard", err)
	}
}

// Both hosts consume one genuine prepared row; candidate completion cannot
// republish it, duplicate its event, reset clocks or replay stale custody.
func TestCompetitionCompletePgPreparedIntakeBaselineReusedAcrossHostsAndReplay(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionPreparedIntakeCompletionFixture(t, tb, true)
		for index := range fixture.settings.DistributedEvaluationPolicy.Hosts {
			settings := fixture.selected(index)
			worker := "synthetic-prepared-completion-worker"
			job, err := fixture.store.Claim(tb.Context(), settings, worker, testWorkerImageDigest())
			if err != nil || job == nil || job.RoundBaseline == nil {
				tb.Fatal("qualified host did not claim the genuine intake candidate", err)
			}
			outcome := competitionPreparedIntakeCompletionOutcome(tb, settings, job)
			before := readCompetitionPreparedCompletionSnapshot(tb.Context(), job, true)
			requirePreparedBaselineInsertRejection(tb, fixture, settings, job, outcome)
			if after := readCompetitionPreparedCompletionSnapshot(tb.Context(), job, true); !reflect.DeepEqual(before, after) {
				tb.Fatal("rejected old insert shape changed custody or retained control")
			}
			authority := withCompetitionJobAuthority(tb.Context(), job)
			if retry, err := fixture.store.Complete(authority, settings, worker, job.JobId, outcome); err != nil || retry {
				tb.Fatal("existing prepared baseline was republished instead of authenticated", retry, err)
			}
			after := readCompetitionPreparedCompletionSnapshot(tb.Context(), job, true)
			if !bytes.Equal(before.baselineJson, after.baselineJson) {
				tb.Fatal("candidate replaced retained baseline bytes or source provenance")
			}
			saved, err := fixture.store.GetJob(tb.Context(), settings, job.JobId, &Principal{Id: "synthetic-operator", Role: "operator"})
			if err != nil || saved.State != "succeeded" || saved.Score == nil || saved.AttemptCount != job.AttemptCount ||
				saved.StartedAt == nil || !saved.StartedAt.Equal(*job.StartedAt) || saved.ExecutionDeadline == nil || !saved.ExecutionDeadline.Equal(*job.ExecutionDeadline) || saved.SlotFencingToken != job.SlotFencingToken {
				tb.Fatal("completion changed its original execution identity or clock", err)
			}
			if retry, err := fixture.store.Complete(authority, settings, worker, job.JobId, outcome); !errors.Is(err, ErrLeaseLost) || retry {
				tb.Fatal("stale completion replay acquired terminal custody", retry, err)
			}
			if replayed := readCompetitionPreparedCompletionSnapshot(tb.Context(), job, true); !reflect.DeepEqual(after, replayed) {
				tb.Fatal("completion replay mutated terminal data or retained baseline")
			}
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var baselines, frozen, terminal int
			server.Raise(conn.QueryRow(tb.Context(), `SELECT
			 (SELECT count(*) FROM competition_round_baseline WHERE round_id=$1),
			 (SELECT count(*) FROM competition_job_event e JOIN competition_job j USING(job_id) WHERE j.round_id=$1 AND e.event_type='round_baseline_frozen'),
			 (SELECT count(*) FROM competition_job_event e JOIN competition_job j USING(job_id) WHERE j.round_id=$1 AND e.event_type='succeeded')`, fixture.round.RoundId).Scan(&baselines, &frozen, &terminal))
			if baselines != 1 || frozen != 0 || terminal != 2 {
				tb.Fatal("prepared completion duplicated publication or lost a terminal result", baselines, frozen, terminal)
			}
		})
	})
}

// The same prepared intake rule applies to the serial worker, not only epoch-two slots.
func TestCompetitionCompletePgPreparedIntakeBaselineReusedBySerialWorker(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionPreparedIntakeCompletionFixture(t, tb, false)
		job, err := fixture.store.Claim(tb.Context(), fixture.settings, "synthetic-serial-prepared-worker", testWorkerImageDigest())
		if err != nil || job == nil || job.RoundBaseline == nil {
			tb.Fatal("serial intake worker did not claim its genuine prepared control", err)
		}
		before := readCompetitionPreparedCompletionSnapshot(tb.Context(), job, false)
		outcome := competitionPreparedIntakeCompletionOutcome(tb, fixture.settings, job)
		if retry, err := fixture.store.Complete(withCompetitionJobAuthority(tb.Context(), job), fixture.settings, "synthetic-serial-prepared-worker", job.JobId, outcome); err != nil || retry {
			tb.Fatal("serial completion tried to republish a prepared intake control", retry, err)
		}
		if after := readCompetitionPreparedCompletionSnapshot(tb.Context(), job, false); !bytes.Equal(before.baselineJson, after.baselineJson) {
			tb.Fatal("serial completion replaced baseline provenance")
		}
	})
}

// Shared-control reuse is not permission to accept changed bytes or lose the
// same-run fencing/deadline when an expired coordinator's custody is reclaimed.
func TestCompetitionCompletePgPreparedIntakeRejectsConflictAndPreservesRecovery(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionPreparedIntakeCompletionFixture(t, tb, true)
		settings := fixture.selected(0)
		job, err := fixture.store.Claim(tb.Context(), settings, "synthetic-before-recovery", testWorkerImageDigest())
		if err != nil || job == nil {
			tb.Fatal(err)
		}
		outcome := competitionPreparedIntakeCompletionOutcome(tb, settings, job)
		conflicting := outcome
		conflicting.RoundBaselineJson = append(append([]byte(nil), outcome.RoundBaselineJson...), '\n')
		digest := sha256.Sum256(conflicting.RoundBaselineJson)
		conflicting.RoundBaselineSha256 = hex.EncodeToString(digest[:])
		before := readCompetitionPreparedCompletionSnapshot(tb.Context(), job, true)
		if retry, err := fixture.store.Complete(withCompetitionJobAuthority(tb.Context(), job), settings, "synthetic-before-recovery", job.JobId, conflicting); err == nil || retry {
			tb.Fatal("changed shared-control bytes were accepted", retry, err)
		}
		if after := readCompetitionPreparedCompletionSnapshot(tb.Context(), job, true); !reflect.DeepEqual(before, after) {
			tb.Fatal("conflicting control committed partial completion")
		}
		if err := fixture.store.HandBack(withCompetitionJobAuthority(tb.Context(), job), "synthetic-before-recovery", job.JobId, "synthetic coordinator replacement"); err != nil {
			tb.Fatal(err)
		}
		recovered, err := fixture.store.Claim(tb.Context(), settings, "synthetic-after-recovery", testWorkerImageDigest())
		if err != nil || recovered == nil || recovered.JobId != job.JobId || recovered.AttemptCount != job.AttemptCount || recovered.SlotFencingToken <= job.SlotFencingToken ||
			!recovered.StartedAt.Equal(*job.StartedAt) || !recovered.ExecutionDeadline.Equal(*job.ExecutionDeadline) {
			tb.Fatal("reconciliation relaunched or reset the original execution", err)
		}
		if retry, err := fixture.store.Complete(withCompetitionJobAuthority(tb.Context(), recovered), settings, "synthetic-after-recovery", recovered.JobId, outcome); err != nil || retry {
			tb.Fatal("reconciled execution could not reuse its unchanged prepared control", retry, err)
		}
		if after := readCompetitionPreparedCompletionSnapshot(tb.Context(), recovered, true); !bytes.Equal(before.baselineJson, after.baselineJson) {
			tb.Fatal("reconciliation changed the original prepared-control provenance")
		}
	})
}
