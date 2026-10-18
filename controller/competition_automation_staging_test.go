// Disposable state and controlled clocks prove staging automation cannot consume legacy authority.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Real publishers keep config on main while all eleven source repositories use the isolated staging branch.
func TestCompetitionAutomationStagingPublicationBranchesMatchRealProducer(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixtureWithMode(t, true)
		fixture.now = fixture.round.RevealAt
		result, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-stage-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		intent := competitionAutomationSourceIntent(t, fixture, result, 2)
		if len(intent.Commits) != 11 || intent.GitRefs["config"].Branch != "main" {
			t.Fatal("fixture does not match the real publication branch contract")
		}
		for _, repository := range []string{"config", "server", "connect", "sdk", "proxy", "glog", "goidenticons", "userwireguard", "sn", "operator-proxy", "warp", "gvisor"} {
			wrong := intent
			wrong.GitRefs = maps.Clone(intent.GitRefs)
			ref := wrong.GitRefs[repository]
			if repository == "config" {
				ref.Branch = "sim-latency-staging"
			} else {
				if ref.Branch != "sim-latency-staging" {
					t.Fatalf("source fixture %s is not staging-isolated", repository)
				}
				ref.Branch = "sim-latency"
			}
			wrong.GitRefs[repository] = ref
			if _, err := fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, wrong); err == nil || !strings.Contains(err.Error(), "publication branch") {
				t.Fatalf("Go accepted the wrong %s branch: %v", repository, err)
			}
			content, err := json.Marshal(wrong)
			if err != nil {
				t.Fatal(err)
			}
			databaseErr := captureDatabaseError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.Raise(fixture.store.requireAutomationLeaseInTx(ctx, tx, fixture.settings, *lease))
					server.RaisePgResult(tx.Exec(ctx, `SELECT competition_v2_source_authorized($1::jsonb,$2,$3,$4)`, string(content), intent.Environment, intent.CompetitionId, intent.SeasonId))
				})
			})
			if databaseErr == nil || !strings.Contains(databaseErr.Error(), "source mode") {
				t.Fatalf("SQL accepted the wrong %s branch: %v", repository, databaseErr)
			}
		}
		stored, err := fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, intent)
		if err != nil || stored == nil || stored.GitRefs["config"].Branch != "main" {
			t.Fatalf("real-shaped source intent was rejected: %+v, %v", stored, err)
		}
	})
}

// Disabled automation cannot strand a live v2 stage, but sealing still permits the legacy production transition.
func TestCompetitionAutomationStagingBlocksLegacyProductionUntilSealed(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixtureWithMode(t, true)
		legacySettings := *fixture.settings
		legacySettings.AutomationPolicy = model.CompetitionAutomationPolicy{}
		legacySettings.SeasonPolicy = validSettings().SeasonPolicy
		legacySettings.EvaluationPolicy.TakeoverMargin = .12
		if err := legacySettings.Validate(); err != nil {
			t.Fatal(err)
		}
		args := GenerateRoundArgs{OpensAt: fixture.round.RevealAt, ClosesAt: fixture.round.RevealAt.Add(7 * 24 * time.Hour), RevealAt: fixture.round.RevealAt.Add(7 * 24 * time.Hour)}
		if _, err := fixture.store.CreateRound(ctx, &legacySettings, args); !errors.Is(err, ErrPreviousEpochOpen) {
			t.Fatalf("legacy production stranded active v2 staging: %v", err)
		}
		blocked, err := fixture.store.prepareRound(ctx, &legacySettings, args, false)
		if err != nil {
			t.Fatal(err)
		}
		databaseErr := captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_round(round_id,competition_id,epoch_number,staging,workload_commitment,seed_nonce,seed_ciphertext,providers_sha256,providers_path,policy_json,opens_at,closes_at,reveal_at,created_at,canceled)
			 VALUES($1,$2,1,false,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$11,$12,false)`, blocked.RoundId, blocked.CompetitionId, blocked.WorkloadCommitment, blocked.SeedNonce, blocked.SeedCiphertext, blocked.ProvidersSha256, blocked.ProvidersPath, string(blocked.PolicyJson), blocked.OpensAt, blocked.ClosesAt, blocked.RevealAt, blocked.CreatedAt))
			})
		})
		if databaseErr == nil || !strings.Contains(databaseErr.Error(), "active execution mode") {
			t.Fatalf("legacy SQL bypassed active v2 staging: %v", databaseErr)
		}
		unchanged, err := fixture.store.GetRound(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || unchanged.Canceled || unchanged.AutomationSealedAt != nil || unchanged.FinalizedAt != nil {
			t.Fatalf("active staging changed after rejected production: %+v, %v", unchanged, err)
		}
		fixture.now = fixture.round.RevealAt
		if _, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId); err != nil {
			t.Fatal(err)
		}
		production, err := fixture.store.CreateRound(ctx, &legacySettings, args)
		if err != nil || production == nil || production.Staging || production.Epoch != 1 {
			t.Fatalf("sealed staging blocked the legitimate production transition: %+v, %v", production, err)
		}
		unchanged, err = fixture.store.GetRound(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || unchanged.Canceled || unchanged.AutomationSealedAt == nil || unchanged.FinalizedAt != nil {
			t.Fatalf("production transition changed the sealed staging history: %+v, %v", unchanged, err)
		}
	})
}

// A legacy ninth staging epoch remains byte-identical while v2 seals and opens its own second epoch.
func TestCompetitionAutomationStagingElectsAndCreatesSuccessorWithoutLegacyMutation(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixtureWithMode(t, true)
		legacySettings := *fixture.settings
		legacySettings.AutomationPolicy = model.CompetitionAutomationPolicy{}
		legacy, err := fixture.store.prepareRound(ctx, &legacySettings, GenerateRoundArgs{OpensAt: fixture.now.Add(-time.Hour), ClosesAt: fixture.now.Add(-time.Minute), RevealAt: fixture.now.Add(-time.Minute)}, true)
		if err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_round(round_id,competition_id,epoch_number,staging,workload_commitment,seed_nonce,seed_ciphertext,providers_sha256,providers_path,policy_json,opens_at,closes_at,reveal_at,created_at,canceled,finalized_at)
		 VALUES($1,$2,9,true,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$11,$12,false,$10)`, legacy.RoundId, legacy.CompetitionId, legacy.WorkloadCommitment, legacy.SeedNonce, legacy.SeedCiphertext, legacy.ProvidersSha256, legacy.ProvidersPath, string(legacy.PolicyJson), legacy.OpensAt, legacy.ClosesAt, legacy.RevealAt, legacy.CreatedAt))
		})
		job := addCompetitionAutomationJob(t, fixture, 110)
		fixture.now = fixture.round.RevealAt
		result, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || result == nil || !result.Staging || !result.Seal.Staging || !result.Decisions[0].Staging || result.ProvisionalRewardWinner == nil || *result.ProvisionalRewardWinner != job.JobId || result.Decisions[0].SettlementMode != "shadow" {
			t.Fatalf("staging did not elect its safe non-significant entry: %+v, %v", result, err)
		}
		if result.Seal.Entries[0].Score.Significance.StatisticallySignificant {
			t.Fatal("fixture failed to exercise staging's significance exemption")
		}
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-stage-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		intent := competitionAutomationSourceIntent(t, fixture, result, 2)
		prepared, err := fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, intent)
		if err != nil {
			t.Fatal(err)
		}
		acknowledgeCompetitionAutomationSource(t, fixture, prepared, "api", "worker")
		if _, err = fixture.store.ActivateSource(ctx, fixture.settings, *lease, 2, 1); err != nil {
			t.Fatal(err)
		}
		if _, err = fixture.store.CreateAutomationRound(ctx, fixture.settings, *lease, 2, fixture.now); !errors.Is(err, ErrReviewNotReady) {
			t.Fatalf("staging skipped its frozen preparation interval: %v", err)
		}
		fixture.now = prepared.PreparedAt.Add(time.Minute)
		next, err := fixture.store.CreateAutomationRound(ctx, fixture.settings, *lease, 2, fixture.now)
		if err != nil || next == nil || next.RoundId != prepared.TargetRoundId || !next.Staging || next.Epoch != 2 || next.ClosesAt.Sub(next.OpensAt) != 10*time.Minute {
			t.Fatalf("staging successor = %+v, %v", next, err)
		}
		replayed, err := fixture.store.CreateAutomationRound(ctx, fixture.settings, *lease, 2, fixture.now)
		if err != nil || replayed.RoundId != next.RoundId {
			t.Fatalf("successor replay changed identity: %+v, %v", replayed, err)
		}
		current, err := fixture.store.CurrentStagingRound(ctx, fixture.settings)
		if err != nil || current == nil || current.RoundId != next.RoundId {
			t.Fatalf("legacy epoch nine replaced v2 discovery: %+v, %v", current, err)
		}
		historical, err := fixture.store.GetRound(ctx, &legacySettings, legacy.RoundId)
		if err != nil || historical.Canceled || historical.FinalizedAt == nil || !historical.FinalizedAt.Equal(legacy.ClosesAt) || historical.Epoch != 9 || roundUsesAutomation(historical) || !storedPolicyMatches(&legacySettings, historical.PolicyJson) {
			t.Fatalf("legacy staging history was mutated: %+v, %v", historical, err)
		}
		production := *fixture.settings
		production.AutomationPolicy.Staging = false
		production.AutomationPolicy.StagingRoundWindowSeconds, production.AutomationPolicy.StagingPreparationSeconds = 0, 0
		if _, err := fixture.store.AutomationResult(ctx, &production, fixture.round.RoundId); !errors.Is(err, ErrConflict) {
			t.Fatalf("production read staging evidence: %v", err)
		}
		page, err := fixture.store.RewardEvents(ctx, &production, 0, 10)
		if err != nil || len(page.Events) != 0 {
			t.Fatalf("production feed leaked staging decisions: %+v, %v", page, err)
		}
		if _, err := fixture.store.CurrentSourceActivation(ctx, &production); !errors.Is(err, ErrConflict) {
			t.Fatalf("production accepted staging activation pointer: %v", err)
		}
	})
}

// SQL independently rejects cross-mode source and round writers even with a live lease.
func TestCompetitionAutomationStagingDatabaseRejectsModeAndFeeBypasses(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixtureWithMode(t, true)
		fixture.now = fixture.round.RevealAt
		result, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-stage-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		intent := competitionAutomationSourceIntent(t, fixture, result, 2)
		wrong := intent
		wrong.Staging = false
		if _, err := fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, wrong); !errors.Is(err, ErrConflict) {
			t.Fatalf("Go accepted a cross-mode source: %v", err)
		}
		content, err := json.Marshal(wrong)
		if err != nil {
			t.Fatal(err)
		}
		databaseErr := captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(fixture.store.requireAutomationLeaseInTx(ctx, tx, fixture.settings, *lease))
				server.RaisePgResult(tx.Exec(ctx, `SELECT competition_v2_source_authorized($1::jsonb,$2,$3,$4)`, string(content), intent.Environment, intent.CompetitionId, intent.SeasonId))
			})
		})
		if databaseErr == nil || !strings.Contains(databaseErr.Error(), "source mode") {
			t.Fatalf("SQL accepted cross-mode source: %v", databaseErr)
		}
		stored, err := fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, intent)
		if err != nil {
			t.Fatal(err)
		}
		acknowledgeCompetitionAutomationSource(t, fixture, stored, "api", "worker")
		if _, err = fixture.store.ActivateSource(ctx, fixture.settings, *lease, 2, 1); err != nil {
			t.Fatal(err)
		}
		fixture.now = stored.PreparedAt.Add(time.Minute)
		derived, err := sourceSettingsFromIntent(fixture.settings, stored)
		if err != nil {
			t.Fatal(err)
		}
		derived.automationTargetRoundId = stored.TargetRoundId
		derived.SeasonPolicy.SubmissionFeeUsd = 20
		invalid, err := fixture.store.prepareRound(ctx, derived, GenerateRoundArgs{OpensAt: fixture.now, ClosesAt: fixture.now.Add(10 * time.Minute), RevealAt: fixture.now.Add(10 * time.Minute)}, true)
		if err != nil {
			t.Fatal(err)
		}
		databaseErr = captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(fixture.store.requireAutomationLeaseInTx(ctx, tx, fixture.settings, *lease))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_round(round_id,competition_id,epoch_number,staging,workload_commitment,seed_nonce,seed_ciphertext,providers_sha256,providers_path,policy_json,opens_at,closes_at,reveal_at,created_at,canceled)
			 VALUES($1,$2,2,true,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$11,$12,false)`, invalid.RoundId, invalid.CompetitionId, invalid.WorkloadCommitment, invalid.SeedNonce, invalid.SeedCiphertext, invalid.ProvidersSha256, invalid.ProvidersPath, string(invalid.PolicyJson), invalid.OpensAt, invalid.ClosesAt, invalid.RevealAt, invalid.CreatedAt))
			})
		})
		if databaseErr == nil || !strings.Contains(databaseErr.Error(), "zero-fee shadow") {
			t.Fatalf("SQL accepted fee-bearing staging: %v", databaseErr)
		}
	})
}

// Request decoding carries the explicit mode while the execution boundary checks configured authority.
func TestCompetitionSupervisorWorkerAcceptsExplicitStagingButRejectsModeSubstitution(t *testing.T) {
	request := CompetitionSupervisorWorkerRequest{Operation: "run_one_epoch", Round: RoundResult{RoundId: server.NewId(), Epoch: 1, Staging: true, DecisionMode: model.CompetitionDecisionModeV2},
		Lease: model.CompetitionSupervisorLease{Environment: "synthetic", CompetitionId: "synthetic-competition", SeasonId: "synthetic-season", OwnerId: "synthetic-owner", FencingToken: 1}}
	content, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCompetitionSupervisorWorkerRequest(bytes.NewReader(content))
	if err != nil || !decoded.Round.Staging {
		t.Fatalf("explicit staging worker rejected: %+v, %v", decoded, err)
	}
	settings := &Settings{CompetitionId: request.Lease.CompetitionId, AutomationPolicy: model.CompetitionAutomationPolicy{Enabled: true, Environment: request.Lease.Environment, SeasonId: request.Lease.SeasonId}}
	if err := RunCompetitionSupervisorWorker(context.Background(), settings, request, "synthetic-worker"); !errors.Is(err, ErrAutomationDisabled) {
		t.Fatalf("mode mismatch reached runtime authority: %v", err)
	}
}

// The fake owns phase state only; the database test above exercises real source and successor commits.
type stagingSupervisorFixture struct {
	*competitionSupervisorFixture
}

// A stage choice carries the incumbent unchanged, with a prospectively frozen zero-second preparation.
func (self *stagingSupervisorFixture) ChooseSource(ctx context.Context, lease model.CompetitionSupervisorLease, snapshot *CompetitionSupervisorSnapshot, transition model.CompetitionSupervisorTransition) (*model.CompetitionSourceActivation, error) {
	intent, err := self.competitionSupervisorFixture.ChooseSource(ctx, lease, snapshot, transition)
	intent.Staging, intent.StagingRoundWindowSeconds, intent.Threshold = true, 600, 0
	intent.Generation, intent.ExpectedGeneration = int64(transition.TargetEpoch), int64(transition.TargetEpoch-1)
	intent.Commits, intent.SourceLockSha256 = map[string]string{"server": strings.Repeat("a", 40)}, strings.Repeat("b", 64)
	if transition.TargetEpoch == 1 {
		intent.Kind = "baseline"
	}
	return intent, err
}

// The state-machine fixture preserves the stage window supplied by the immutable intent.
func (self *stagingSupervisorFixture) CreateRound(ctx context.Context, lease model.CompetitionSupervisorLease, intent *model.CompetitionSourceActivation, opensAt time.Time) (*RoundResult, error) {
	round, err := self.competitionSupervisorFixture.CreateRound(ctx, lease, intent, opensAt)
	round.Staging, round.ClosesAt = true, opensAt.Add(time.Duration(intent.StagingRoundWindowSeconds)*time.Second)
	return round, err
}

// Six complete fake-clock generations require no review service and can never create a seventh.
func TestCompetitionAutomationStagingSupervisorCompletesSixRoundSeason(t *testing.T) {
	fixture := &stagingSupervisorFixture{competitionSupervisorFixture: newCompetitionSupervisorFixture()}
	fixture.snapshot = CompetitionSupervisorSnapshot{}
	step := func(want string) {
		t.Helper()
		supervisor := &CompetitionSupervisor{Enabled: true, OwnerId: "synthetic-stage-supervisor", Driver: fixture, Now: func() time.Time { return fixture.now }}
		if state, err := supervisor.Reconcile(context.Background()); err != nil || state != want {
			t.Fatalf("state=%s want=%s error=%v", state, want, err)
		}
	}
	for epoch := 1; epoch <= 6; epoch++ {
		for _, phase := range []string{"source_choice_pending", "source_publication_pending", "preparation_pending", "source_active", "round_created"} {
			step(phase)
		}
		round, source := fixture.snapshot.Round, fixture.snapshot.CurrentSource
		if round.Epoch != epoch || !round.Staging || round.ClosesAt.Sub(round.OpensAt) != 10*time.Minute || source.Threshold != 0 || source.SourceGeneration != 0 || source.Commits["server"] != strings.Repeat("a", 40) || source.ReviewId != nil {
			t.Fatalf("stage changed source or frozen policy at epoch %d: %+v %+v", epoch, round, source)
		}
		fixture.now = round.ClosesAt
		fixture.snapshot.Seal = &model.CompetitionEvaluationSeal{Staging: true, RoundId: round.RoundId, Epoch: epoch, SealedAt: fixture.now, SourceChoiceAt: fixture.now}
		fixture.snapshot.Transition, fixture.snapshot.Intent = nil, nil
	}
	step("season_complete")
	if fixture.choices != 6 || fixture.creations != 6 || fixture.activations != 6 {
		t.Fatalf("stage cap or automatic generation failed: %+v", fixture)
	}
}

// A newly configured season cannot strand the preceding season's still-open stage queue.
func TestCompetitionAutomationStagingNewSeasonCannotBypassActiveRound(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixtureWithMode(t, true)
		original, err := fixture.store.CurrentSourceActivation(ctx, fixture.settings)
		if err != nil {
			t.Fatal(err)
		}
		settings := *fixture.settings
		settings.AutomationPolicy.SeasonId = "synthetic-other-stage-season"
		intent := *original
		intent.SeasonId, intent.TargetRoundId = settings.AutomationPolicy.SeasonId, server.NewId()
		intent.PreparedAt, intent.OpensAt = fixture.now.Add(-time.Minute), fixture.now
		lease, err := fixture.store.AcquireAutomationLease(ctx, &settings, "synthetic-other-stage-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := fixture.store.RecordSourceIntent(ctx, &settings, *lease, intent)
		if err != nil {
			t.Fatal(err)
		}
		other := &competitionRankingFixture{settings: &settings, store: fixture.store, now: fixture.now}
		acknowledgeCompetitionAutomationSource(t, other, stored, "api", "worker")
		if _, err := fixture.store.ActivateSource(ctx, &settings, *lease, 1, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.CreateAutomationRound(ctx, &settings, *lease, 1, fixture.now); !errors.Is(err, ErrPreviousEpochOpen) {
			t.Fatalf("another season stranded the active staging queue: %v", err)
		}
		current, err := fixture.store.CurrentAutomationRound(ctx, fixture.settings)
		if err != nil || current.RoundId != fixture.round.RoundId || current.Canceled || current.AutomationSealedAt != nil {
			t.Fatalf("preceding season changed: %+v, %v", current, err)
		}
	})
}

// A legacy epoch retains the same global active slot; v2 cannot replace or reinterpret it.
func TestCompetitionAutomationStagingCannotBypassActiveLegacyRound(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixtureWithMode(t, true)
		fixture.now = fixture.round.RevealAt
		result, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-stage-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := fixture.store.RecordSourceIntent(ctx, fixture.settings, *lease, competitionAutomationSourceIntent(t, fixture, result, 2))
		if err != nil {
			t.Fatal(err)
		}
		acknowledgeCompetitionAutomationSource(t, fixture, stored, "api", "worker")
		if _, err := fixture.store.ActivateSource(ctx, fixture.settings, *lease, 2, 1); err != nil {
			t.Fatal(err)
		}
		fixture.now = stored.PreparedAt.Add(time.Minute)
		legacySettings := *fixture.settings
		legacySettings.AutomationPolicy = model.CompetitionAutomationPolicy{}
		legacy, err := fixture.store.CreateStagingRound(ctx, &legacySettings, GenerateRoundArgs{OpensAt: fixture.now, ClosesAt: fixture.now.Add(time.Hour), RevealAt: fixture.now.Add(time.Hour)}, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.store.CreateAutomationRound(ctx, fixture.settings, *lease, 2, fixture.now); !errors.Is(err, ErrPreviousEpochOpen) {
			t.Fatalf("v2 stranded active legacy staging: %v", err)
		}
		derived, err := sourceSettingsFromIntent(fixture.settings, stored)
		if err != nil {
			t.Fatal(err)
		}
		derived.automationTargetRoundId = stored.TargetRoundId
		blocked, err := fixture.store.prepareRound(ctx, derived, GenerateRoundArgs{OpensAt: fixture.now, ClosesAt: fixture.now.Add(10 * time.Minute), RevealAt: fixture.now.Add(10 * time.Minute)}, true)
		if err != nil {
			t.Fatal(err)
		}
		databaseErr := captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(fixture.store.requireAutomationLeaseInTx(ctx, tx, fixture.settings, *lease))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_round(round_id,competition_id,epoch_number,staging,workload_commitment,seed_nonce,seed_ciphertext,providers_sha256,providers_path,policy_json,opens_at,closes_at,reveal_at,created_at,canceled)
			 VALUES($1,$2,2,true,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$11,$12,false)`, blocked.RoundId, blocked.CompetitionId, blocked.WorkloadCommitment, blocked.SeedNonce, blocked.SeedCiphertext, blocked.ProvidersSha256, blocked.ProvidersPath, string(blocked.PolicyJson), blocked.OpensAt, blocked.ClosesAt, blocked.RevealAt, blocked.CreatedAt))
			})
		})
		if databaseErr == nil || !strings.Contains(databaseErr.Error(), "competition_round_one_active_staging") {
			t.Fatalf("SQL bypassed global staging ownership: %v", databaseErr)
		}
		unchanged, err := fixture.store.GetRound(ctx, &legacySettings, legacy.RoundId)
		if err != nil || unchanged.Canceled || unchanged.FinalizedAt != nil || !unchanged.ClosesAt.Equal(legacy.ClosesAt) {
			t.Fatalf("legacy staging was altered: %+v, %v", unchanged, err)
		}
	})
}

// An older writer cannot shorten a v2 staging window through the legacy early-close marker.
func TestCompetitionAutomationStagingDatabaseRejectsLegacyEarlyClose(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixtureWithMode(t, true)
		databaseErr := captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_round SET admission_closed_at=$2 WHERE round_id=$1`, fixture.round.RoundId, fixture.now))
			})
		})
		if databaseErr == nil || !strings.Contains(databaseErr.Error(), "frozen full window") {
			t.Fatalf("legacy SQL shortened automated staging admission: %v", databaseErr)
		}
		legacySettings := *fixture.settings
		legacySettings.AutomationPolicy = model.CompetitionAutomationPolicy{}
		if _, err := fixture.store.CloseStagingRound(ctx, &legacySettings); !errors.Is(err, ErrNotFound) {
			t.Fatalf("legacy close discovered the v2 staging round: %v", err)
		}
		unchanged, err := fixture.store.GetRound(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || unchanged.AdmissionClosedAt != nil || !unchanged.ClosesAt.Equal(fixture.round.ClosesAt) {
			t.Fatalf("frozen staging admission changed: %+v, %v", unchanged, err)
		}
	})
}
