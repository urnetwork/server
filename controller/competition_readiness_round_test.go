// Disposable database controls separate frozen grading readiness from intake
// timing. Host facts and retained objects are synthetic, not native evidence.
package controller

import (
	"context"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Reuse one synthetic host row within each disposable database, so an earlier
// fixture's report cannot accidentally satisfy a later round's control.
func registerCompetitionReadinessHost(t testing.TB, ctx context.Context, fixture *competitionRankingFixture, roundId server.Id) HostSelfCheck {
	t.Helper()
	check := passingHostCheck(fixture.settings)
	check.HostId, check.RebaselineRoundId = "synthetic-readiness-host", &roundId
	if err := fixture.store.RegisterHost(ctx, fixture.settings, check); err != nil {
		t.Fatal(err)
	}
	return check
}

// All unrelated physical dependencies must stay identical across a round
// selection failure, not merely remain absent from the assertion.
func requireCompetitionReadinessBaselineOnly(t testing.TB, ctx context.Context, fixture *competitionRankingFixture, settings *Settings, expected map[string]bool, passed bool) {
	t.Helper()
	checks, err := fixture.store.Readiness(ctx, settings)
	if err != nil || checks["host_rebaseline"] != passed {
		t.Fatalf("frozen host baseline readiness=%v want=%t error=%v", checks, passed, err)
	}
	want := maps.Clone(expected)
	want["host_rebaseline"] = passed
	if !maps.Equal(checks, want) {
		t.Fatalf("round selection changed unrelated readiness: got=%v want=%v", checks, want)
	}
}

// Observe this callback's actual database before constructing its fixture;
// the owned connection is released before any subsequent store operation.
func requireCompetitionReadinessFixtureSchema(t testing.TB, ctx context.Context) {
	t.Helper()
	var actualHead int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(max(end_version_number),0) FROM migration_audit WHERE status='success'`).Scan(&actualHead))
	})
	if actualHead != server.MigrationCount() {
		t.Fatalf("readiness fixture schema=%d want=%d", actualHead, server.MigrationCount())
	}
	t.Logf("actual_readiness_fixture_schema_head=%d", actualHead)
}

// This is the runtime-blob v2 API path used while a closed production round
// still grades. Source authentication and the finalization-only hold stay real.
func TestCompetitionReadinessClosedRuntimeRoundKeepsGradingReady(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		requireCompetitionReadinessFixtureSchema(t, ctx)
		fixture := newCompetitionAutomationFixtureWithCapability(t, false, model.CompetitionRuntimeBlobCapability)
		blobs := &competitionRuntimeBlobTestStore{objects: map[string][]byte{}, versions: map[string][]byte{}}
		archive := &blobArtifactArchive{store: blobs}
		fixture.settings.artifactArchive = archive
		intent, err := fixture.store.CurrentSourceActivation(ctx, fixture.settings)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := archive.publishRuntimeManifest(ctx, fixture.settings, intent); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", competitionFinalizationHoldArgs("synthetic-readiness-finalization")); err != nil {
			t.Fatal(err)
		}
		service := newServiceWithImageDigest(fixture.settings, fixture.store, testApiImageDigest(), nil)
		fixture.now = fixture.round.ClosesAt.Add(-time.Microsecond)
		registerCompetitionReadinessHost(t, ctx, fixture, fixture.round.RoundId)
		before, readyErr := service.Ready(ctx)
		if readyErr != nil || !before.Ready || len(before.Checks) != 11 || !allChecks(before.Checks) || before.RuntimeStatus == nil || !before.RuntimeStatus.GradingReady || !before.RuntimeStatus.FinalizationHeld {
			t.Fatalf("open authenticated runtime readiness=%+v error=%v", before, readyErr)
		}
		for _, now := range []time.Time{fixture.round.ClosesAt, fixture.round.ClosesAt.Add(time.Second)} {
			fixture.now = now
			after, readyErr := service.Ready(ctx)
			if readyErr != nil || !after.Ready || !maps.Equal(after.Checks, before.Checks) || after.RuntimeStatus == nil || !after.RuntimeStatus.GradingReady || !after.RuntimeStatus.FinalizationHeld {
				t.Fatalf("closed draining runtime lost readiness: now=%s result=%+v error=%v", now, after, readyErr)
			}
		}
		accepted, status, submitErr := service.SubmitV2(ctx, ScoreArgs{RoundId: fixture.round.RoundId, Patch: testPatch("synthetic-closed-readiness")}, &Principal{Id: "synthetic-readiness-submitter", Role: "submitter"})
		if accepted != nil || status != http.StatusConflict || submitErr == nil || submitErr.Code != "round_not_open" {
			t.Fatalf("grading readiness reopened intake: result=%+v status=%d error=%v", accepted, status, submitErr)
		}
		var jobs int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_job WHERE round_id=$1`, fixture.round.RoundId).Scan(&jobs))
		})
		if jobs != 0 {
			t.Fatal("closed intake created a job or evaluation clock", jobs)
		}
		// A host report cannot replace the existing source-envelope proof.
		blobs.stateLock.Lock()
		blobs.corruptReadback = true
		blobs.stateLock.Unlock()
		if result, readyErr := service.Ready(ctx); readyErr == nil || readyErr.Code != "source_not_ready" || result.Ready || result.Checks["frozen_source_activation"] {
			t.Fatal("host baseline bypassed source authentication", result, readyErr)
		}
	})
}

// Staging uses its explicitly configured season/mode, not another open or
// older production source. Closing that staging intake also preserves grading.
func TestCompetitionReadinessAutomationScopeAndStagingRemainExact(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		requireCompetitionReadinessFixtureSchema(t, ctx)
		fixture := newCompetitionAutomationFixtureWithMode(t, true)
		fixture.now = fixture.round.ClosesAt
		registerCompetitionReadinessHost(t, ctx, fixture, fixture.round.RoundId)
		expected, err := fixture.store.Readiness(ctx, fixture.settings)
		if err != nil || !allChecks(expected) {
			t.Fatal("closed v2 staging round lost its frozen baseline", expected, err)
		}
		for _, change := range []func(*Settings){
			func(settings *Settings) { settings.AutomationPolicy.Environment = "synthetic-other-environment" },
			func(settings *Settings) { settings.AutomationPolicy.SeasonId = "synthetic-other-season" },
			func(settings *Settings) { settings.AutomationPolicy.Staging = false },
			func(settings *Settings) { settings.AutomationPolicy.Enabled = false },
			func(settings *Settings) { settings.CompetitionId = "synthetic-other-competition" },
		} {
			settings := *fixture.settings
			change(&settings)
			requireCompetitionReadinessBaselineOnly(t, ctx, fixture, &settings, expected, false)
		}
		fixture.now = fixture.round.OpensAt.Add(-time.Microsecond)
		registerCompetitionReadinessHost(t, ctx, fixture, fixture.round.RoundId)
		requireCompetitionReadinessBaselineOnly(t, ctx, fixture, fixture.settings, expected, false)
		fixture.now = fixture.round.OpensAt
		requireCompetitionReadinessBaselineOnly(t, ctx, fixture, fixture.settings, expected, true)
		fixture.now = fixture.round.ClosesAt
		registerCompetitionReadinessHost(t, ctx, fixture, fixture.round.RoundId)
		if _, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId); err != nil {
			t.Fatal(err)
		}
		requireCompetitionReadinessBaselineOnly(t, ctx, fixture, fixture.settings, expected, false)
	})
}

// Historical contracts require no source pointer or new role acknowledgement. A legacy
// staging host ceases to be current when production begins, not at close time.
func TestCompetitionReadinessLegacyRoundAndStagingEraRemainReadable(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		requireCompetitionReadinessFixtureSchema(t, ctx)
		for _, staging := range []bool{false, true} {
			fixture := newCompetitionRankingFixture(t, staging)
			fixture.now = fixture.round.ClosesAt
			registerCompetitionReadinessHost(t, ctx, fixture, fixture.round.RoundId)
			expected, err := fixture.store.Readiness(ctx, fixture.settings)
			if err != nil || !allChecks(expected) {
				t.Fatalf("historical contract lost readiness staging=%t checks=%v error=%v", staging, expected, err)
			}
			if !staging {
				continue
			}
			production, err := fixture.store.CreateRound(ctx, fixture.settings, GenerateRoundArgs{OpensAt: fixture.now.Add(time.Hour), ClosesAt: fixture.now.Add(2 * time.Hour), RevealAt: fixture.now.Add(2 * time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			requireCompetitionReadinessBaselineOnly(t, ctx, fixture, fixture.settings, expected, false)
			fixture.now = production.OpensAt
			registerCompetitionReadinessHost(t, ctx, fixture, fixture.round.RoundId)
			requireCompetitionReadinessBaselineOnly(t, ctx, fixture, fixture.settings, expected, false)
			registerCompetitionReadinessHost(t, ctx, fixture, production.RoundId)
			requireCompetitionReadinessBaselineOnly(t, ctx, fixture, fixture.settings, expected, true)
			fixture.now = production.ClosesAt
			registerCompetitionReadinessHost(t, ctx, fixture, production.RoundId)
			state, err := fixture.store.PrepareCandidateReview(ctx, fixture.settings, production.Epoch)
			if err != nil || state == nil || state.Status != "finalized" {
				t.Fatal("empty legacy production fixture did not finalize", state, err)
			}
			requireCompetitionReadinessBaselineOnly(t, ctx, fixture, fixture.settings, expected, false)
		}
	})
}

// The report must still be fresh and match the frozen evaluator class. Moving
// the API's configured class cannot transfer an earlier round's host control.
func TestCompetitionReadinessFrozenHostClassAndFreshnessRemainRequired(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		requireCompetitionReadinessFixtureSchema(t, ctx)
		fixture := newCompetitionAutomationFixture(t)
		fixture.now = fixture.round.ClosesAt
		check := registerCompetitionReadinessHost(t, ctx, fixture, fixture.round.RoundId)
		expected, err := fixture.store.Readiness(ctx, fixture.settings)
		if err != nil || !allChecks(expected) {
			t.Fatal(expected, err)
		}
		for _, change := range []func(*Settings){
			func(settings *Settings) { settings.EvaluatorImageDigest = "sha256:" + strings.Repeat("8", 64) },
			func(settings *Settings) { settings.EvaluationPolicy.HardwareId = "synthetic-other-hardware" },
			func(settings *Settings) { settings.EvaluationPolicy.HostQualificationSha256 = strings.Repeat("7", 64) },
		} {
			settings := *fixture.settings
			change(&settings)
			changed := passingHostCheck(&settings)
			changed.HostId, changed.RebaselineRoundId = check.HostId, &fixture.round.RoundId
			if err := fixture.store.RegisterHost(ctx, &settings, changed); err != nil {
				t.Fatal(err)
			}
			requireCompetitionReadinessBaselineOnly(t, ctx, fixture, &settings, expected, false)
		}
		check = registerCompetitionReadinessHost(t, ctx, fixture, fixture.round.RoundId)
		wrongRoundId := server.NewId()
		check.RebaselineRoundId = &wrongRoundId
		if err := fixture.store.RegisterHost(ctx, fixture.settings, check); err != nil {
			t.Fatal(err)
		}
		requireCompetitionReadinessBaselineOnly(t, ctx, fixture, fixture.settings, expected, false)
		registerCompetitionReadinessHost(t, ctx, fixture, fixture.round.RoundId)
		fixture.now = fixture.now.Add(time.Duration(fixture.settings.HostHeartbeatMaxAgeSeconds) * time.Second)
		requireCompetitionReadinessBaselineOnly(t, ctx, fixture, fixture.settings, expected, true)
		fixture.now = fixture.now.Add(time.Microsecond)
		checks, err := fixture.store.Readiness(ctx, fixture.settings)
		if err != nil || checks["host_rebaseline"] || checks["authoritative_evaluator_host"] || checks["artifact_storage"] {
			t.Fatal("expired host heartbeat was accepted", checks, err)
		}
	})
}

// Each round is inserted through the installed legacy guards. Historical
// writers could leave earlier rounds unfinished; selection must not search
// backward for an older eligible row after the configured latest is rejected.
func TestCompetitionReadinessLatestScheduledAndFinalizedDoNotFallBack(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		requireCompetitionReadinessFixtureSchema(t, ctx)
		fixture := newCompetitionRankingFixture(t, false)
		fixture.now = fixture.round.ClosesAt
		registerCompetitionReadinessHost(t, ctx, fixture, fixture.round.RoundId)
		expected, err := fixture.store.Readiness(ctx, fixture.settings)
		if err != nil || !allChecks(expected) {
			t.Fatal(expected, err)
		}
		latest, err := fixture.store.prepareRound(ctx, fixture.settings, GenerateRoundArgs{OpensAt: fixture.now.Add(time.Hour), ClosesAt: fixture.now.Add(2 * time.Hour), RevealAt: fixture.now.Add(2 * time.Hour)}, false)
		if err != nil {
			t.Fatal(err)
		}
		latest.Epoch = fixture.round.Epoch + 1
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_round(round_id,competition_id,epoch_number,staging,workload_commitment,seed_nonce,seed_ciphertext,providers_sha256,providers_path,policy_json,opens_at,closes_at,reveal_at,created_at,canceled)
			 VALUES($1,$2,$3,false,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12,$13,false)`, latest.RoundId, latest.CompetitionId, latest.Epoch, latest.WorkloadCommitment, latest.SeedNonce, latest.SeedCiphertext, latest.ProvidersSha256, latest.ProvidersPath, string(latest.PolicyJson), latest.OpensAt, latest.ClosesAt, latest.RevealAt, latest.CreatedAt))
		})
		current, err := fixture.store.CurrentRound(ctx, fixture.settings)
		if err != nil || current == nil || current.RoundId != latest.RoundId {
			t.Fatal("fixture did not select its scheduled latest round", current, err)
		}
		requireCompetitionReadinessBaselineOnly(t, ctx, fixture, fixture.settings, expected, false)
		fixture.now = latest.OpensAt
		registerCompetitionReadinessHost(t, ctx, fixture, fixture.round.RoundId)
		requireCompetitionReadinessBaselineOnly(t, ctx, fixture, fixture.settings, expected, false)
		registerCompetitionReadinessHost(t, ctx, fixture, latest.RoundId)
		requireCompetitionReadinessBaselineOnly(t, ctx, fixture, fixture.settings, expected, true)
		fixture.now = latest.ClosesAt
		state, err := fixture.store.PrepareCandidateReview(ctx, fixture.settings, latest.Epoch)
		if err != nil || state == nil || state.Status != "finalized" {
			t.Fatal("latest empty legacy round did not finalize", state, err)
		}
		registerCompetitionReadinessHost(t, ctx, fixture, fixture.round.RoundId)
		requireCompetitionReadinessBaselineOnly(t, ctx, fixture, fixture.settings, expected, false)
	})
}
