// Historical announced-source fixtures can honestly contain an expired full
// execution budget. ASAP intake fixtures instead retain their real SQL opening.
package controller

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// No source, intake, job, baseline or executor trigger is disabled. An older
// announced round is activated through its genuine two-role source authority;
// it is not relabeled as an ASAP intake grant created in the past.
func newCompetitionPreparedAnnouncedCompletionFixtureAt(t *testing.T, tb testing.TB, historicalNow time.Time) *competitionDistributedPgFixture {
	tb.Helper()
	intake, round := newCompetitionAnnouncedAdmissionFixtureAt(t, tb, historicalNow)
	return finishCompetitionPreparedCompletionFixture(tb, intake, round, true, true)
}

// The returned candidates are genuinely admitted, but no control is published
// yet. Negative controls can therefore exercise the real publication guard.
func newCompetitionAnnouncedAdmissionFixtureAt(t *testing.T, tb testing.TB, historicalNow time.Time) (*competitionIntakeFirstDatabaseFixture, *roundRecord) {
	tb.Helper()
	ctx := tb.Context()
	if historicalNow.IsZero() {
		tb.Fatal("historical announced fixture requires an explicit historical clock")
	}
	intake := newCompetitionIntakeFirstDatabaseFixtureAt(t, ctx, historicalNow, func(settings *Settings) {
		settings.FutureEpochPolicy.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(tb)
	})
	intake.handoffAndPublish(tb, ctx)
	intake.acknowledgeApi(tb, ctx)
	worker := competitionPreparationTestReceipt(intake.intent, "worker", "synthetic-announced-runtime", testWorkerImageDigest())
	if err := intake.store.RecordSourceReadiness(ctx, intake.settings, worker); err != nil {
		tb.Fatal(err)
	}
	active, err := intake.store.ActivateSource(ctx, intake.settings, intake.lease, 2, 1)
	if err != nil {
		tb.Fatal(err)
	}
	frozen, err := sourceSettingsFromIntent(intake.settings, active)
	if err != nil {
		tb.Fatal(err)
	}
	frozen.automationLease, frozen.automationTargetRoundId = &intake.lease, active.TargetRoundId
	round, err := intake.store.CreateRound(ctx, frozen, GenerateRoundArgs{OpensAt: intake.now, ClosesAt: intake.now.Add(7 * 24 * time.Hour), RevealAt: intake.now.Add(7 * 24 * time.Hour)})
	if err != nil {
		tb.Fatal("genuine historical announced source could not open its round", err)
	}
	frozen, err = settingsForCompetitionApiRound(ctx, frozen, intake.store, round)
	if err != nil || frozen.intakeFirstActivation != nil {
		tb.Fatal("historical announced source acquired fabricated ASAP authority", err)
	}
	intake.settings = frozen
	for _, name := range []string{"synthetic-announced-source", "synthetic-announced-next"} {
		patch, patchErr := ValidateAndCanonicalizePatch(testPatch(name), frozen.PatchPolicy)
		if patchErr != nil {
			tb.Fatal(patchErr)
		}
		// This is the actual durable admission boundary. The API's independent
		// pre-baseline containment preflight is covered by the ASAP fixture.
		job, hit, err := intake.store.Enqueue(ctx, frozen, round.RoundId, patch, "synthetic-submitter", testApiImageDigest())
		if err != nil || hit || job == nil || job.StartedAt != nil || job.ExecutionDeadline != nil || job.AttemptCount != 0 {
			tb.Fatal("genuine announced admission did not retain its clockless identity", err)
		}
	}
	return intake, round
}

// The original failure setup needs an honest already-expired SQL deadline,
// while operator handoff, supervisor lease and host publication remain current.
func TestCompetitionDistributedRecoveryPgHistoricalFixtureKeepsLiveAuthority(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedRecoveryPgFixture(t, tb)
		ctx := tb.Context()
		var databaseNow, handoffAt, leaseUntil, completedAt time.Time
		var grants, disabledGuards int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT timezone('UTC',clock_timestamp()),
			 (SELECT created_at FROM competition_source_operator_handoff WHERE competition_id=$1 ORDER BY revision DESC LIMIT 1),
			 (SELECT lease_expires_at FROM competition_supervisor_lease WHERE competition_id=$1),
			 (SELECT completed_at FROM competition_job WHERE job_id=$3),
			 (SELECT count(*) FROM competition_source_intake_activation WHERE target_round_id=$2),
			 (SELECT count(*) FROM pg_trigger WHERE tgrelid IN ('competition_round'::regclass,'competition_job'::regclass,
			 'competition_round_baseline'::regclass,'competition_execution_slot'::regclass) AND NOT tgisinternal AND tgenabled<>'O')`, fixture.settings.CompetitionId, fixture.round.RoundId, fixture.original.JobId).
				Scan(&databaseNow, &handoffAt, &leaseUntil, &completedAt, &grants, &disabledGuards))
		})
		if fixture.original.StartedAt == nil || fixture.original.ExecutionDeadline == nil || completedAt.IsZero() ||
			fixture.original.StartedAt.Before(fixture.round.OpensAt) || fixture.original.StartedAt.Before(fixture.original.SubmittedAt) ||
			!fixture.original.ExecutionDeadline.Equal(fixture.original.StartedAt.Add(3*time.Hour)) ||
			completedAt.Before(*fixture.original.ExecutionDeadline) || databaseNow.Before(completedAt) ||
			!fixture.original.ExecutionDeadline.Before(handoffAt) || !databaseNow.Before(leaseUntil) || grants != 0 || disabledGuards != 0 {
			tb.Fatal("historical fixture conflated expired execution history with current authority")
		}
		limits := fixture.settings.roundBaselinePreparation.Settings
		if limits.TotalTimeout != 3*time.Hour || limits.CleanupTimeout != 2*time.Minute ||
			!fixture.preparation.Deadline.Before(databaseNow) ||
			!databaseNow.Before(fixture.preparation.Deadline.Add(limits.CleanupTimeout)) {
			tb.Fatal("synthetic retained-output publication did not use its unchanged cleanup grace")
		}
	})
}

// The cleanup grace permits only retained control publication. It neither
// enlarges the evaluator budget nor revives an already expired cleanup owner.
func TestCompetitionDistributedRecoveryPgHistoricalPublicationRejectsExpiredCleanupGrace(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		var databaseNow time.Time
		server.Db(ctx, func(conn server.PgConn) {
			var err error
			databaseNow, err = competitionIntakeFirstDatabaseTime(ctx, conn)
			server.Raise(err)
		})
		intake, round := newCompetitionAnnouncedAdmissionFixtureAt(t, tb, databaseNow.Add(-3*time.Hour-3*time.Minute))
		settings := intake.settings
		settings.automationLease, settings.automationRoundId = &intake.lease, round.RoundId
		settings.roundBaselinePreparation = &roundBaselinePreparationProfile{Settings: defaultRoundBaselinePreparationSettings()}
		preparation, err := intake.store.ReserveRoundBaseline(ctx, settings, "synthetic-expired-control-owner", testWorkerImageDigest())
		if err != nil || preparation == nil || !preparation.Deadline.Add(settings.roundBaselinePreparation.Settings.CleanupTimeout).Before(databaseNow) {
			tb.Fatal("expired cleanup control did not own its original bounded reservation", err)
		}
		prepared := preparedRoundBaselineTest(tb, settings, preparation)
		err = intake.store.PublishPreparedRoundBaseline(ctx, settings, preparation, prepared)
		var postgres *pgconn.PgError
		if !errors.As(err, &postgres) || postgres.Code != "P0001" || !strings.Contains(postgres.Message, "baseline source does not belong to its round attempt") {
			tb.Fatal("expired cleanup lease bypassed the original SQL baseline publication guard", err)
		}
		var baselines, startedJobs, publications int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT
			 (SELECT count(*) FROM competition_round_baseline WHERE round_id=$1),
			 (SELECT count(*) FROM competition_job WHERE round_id=$1 AND (started_at IS NOT NULL OR attempt_count<>0)),
			 (SELECT count(*) FROM competition_job_event e JOIN competition_job j USING(job_id) WHERE j.round_id=$1 AND e.event_type='round_baseline_preparation_published')`, round.RoundId).
				Scan(&baselines, &startedJobs, &publications))
		})
		if baselines != 0 || startedJobs != 0 || publications != 0 {
			tb.Fatal("rejected cleanup publication changed baseline, candidate clock or audit")
		}
	})
}

// The genuine ASAP path still derives the same immutable intake grant through
// both API and worker loaders, then claims/completes against its real opening.
// This checks the ASAP-specific boundaries, not an artificially expired run.
func TestCompetitionDistributedRecoveryPgCurrentIntakeBranchesKeepRealOpening(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		var before time.Time
		server.Db(ctx, func(conn server.PgConn) {
			var err error
			before, err = competitionIntakeFirstDatabaseTime(ctx, conn)
			server.Raise(err)
		})
		fixture := newCompetitionPreparedIntakeCompletionFixture(t, tb, true)
		if fixture.round.OpensAt.Before(before) || fixture.settings.intakeFirstActivation == nil ||
			fixture.round.OpensAt.Before(fixture.settings.intakeFirstActivation.CreatedAt) ||
			fixture.round.ClosesAt.Sub(fixture.round.OpensAt) != 7*24*time.Hour {
			tb.Fatal("ASAP fixture backdated or omitted its actual SQL opening/grant")
		}
		// Match RunDistributedCoordinator's real worker child, not its separate
		// synthetic baseline producer profile with intentionally empty Config.
		childSettings := *fixture.selected(0)
		childSettings.roundBaselinePreparation, childSettings.distributedPreparationOnly = nil, false
		for _, loader := range []struct {
			name string
			load func() (*Settings, error)
		}{
			{name: "api-startup-base", load: func() (*Settings, error) {
				return settingsForCompetitionApiRound(ctx, fixture.startupSettings, fixture.store, fixture.round)
			}},
			{name: "worker-startup-base", load: func() (*Settings, error) {
				return settingsForAutomationRound(ctx, fixture.startupSettings, fixture.store, fixture.round)
			}},
			{name: "worker-derived-coordinator-child", load: func() (*Settings, error) {
				return settingsForAutomationRound(ctx, &childSettings, fixture.store, fixture.round)
			}},
		} {
			loaded, err := loader.load()
			if err != nil || loaded == nil || loaded.intakeFirstActivation == nil || *loaded.intakeFirstActivation != *fixture.settings.intakeFirstActivation ||
				!storedPolicyMatches(loaded, fixture.round.PolicyJson) {
				tb.Fatal("ASAP source derivation lost its exact purpose authority", loader.name, err)
			}
		}
		if err := requireConfiguredCompetitionRuntime(ctx, &childSettings, fixture.store); err != nil {
			tb.Fatal("ASAP derived child failed the actual worker startup runtime boundary", err)
		}
		selected := &childSettings
		job, err := fixture.store.Claim(ctx, selected, "synthetic-current-intake-worker", testWorkerImageDigest())
		if err != nil || job == nil || job.StartedAt == nil || job.StartedAt.Before(fixture.round.OpensAt) || job.ExecutionDeadline == nil ||
			!job.ExecutionDeadline.Equal(job.StartedAt.Add(3*time.Hour)) {
			tb.Fatal("ASAP grading claim did not retain its genuine opening and full budget", err)
		}
		prior := readCompetitionPreparedCompletionSnapshot(ctx, job, true)
		authorityCtx := withCompetitionJobAuthority(ctx, job)
		if err := fixture.store.Heartbeat(authorityCtx, selected, "synthetic-current-intake-worker", job.JobId); err != nil {
			tb.Fatal(err)
		}
		outcome := competitionPreparedIntakeCompletionOutcome(tb, selected, job)
		if retry, err := fixture.store.Complete(authorityCtx, selected, "synthetic-current-intake-worker", job.JobId, outcome); err != nil || retry {
			tb.Fatal("ASAP completion changed its shared baseline publication contract", retry, err)
		}
		after := readCompetitionPreparedCompletionSnapshot(ctx, job, true)
		if string(prior.baselineJson) != string(after.baselineJson) {
			tb.Fatal("ASAP completion republished the immutable prepared baseline")
		}
	})
}
