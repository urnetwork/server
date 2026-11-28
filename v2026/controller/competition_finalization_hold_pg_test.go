// Disposable PostgreSQL proves prospective custody against unmodified writer
// statements, including snapshots acquired before a competing scope commit.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The same public request shape is used for both physical journals.
func competitionFinalizationHoldArgs(key string) model.CompetitionAutomationHoldArgs {
	return model.CompetitionAutomationHoldArgs{Kind: "finalization", Active: true, IdempotencyKey: key,
		ReasonCode: "OPERATOR_HOLD", PublicExplanation: "Synthetic pending calibration review.", EvidenceSha256: strings.Repeat("a", 64)}
}

// An empty inventory is a valid, fully drained round, not a disabled guard.
// Its payload follows the existing writer's exact hashes and publication rule.
func competitionFinalizationEmptySeal(t testing.TB, fixture *competitionRankingFixture) *model.CompetitionEvaluationSeal {
	t.Helper()
	policy, err := decodeRoundPolicySnapshot(fixture.round.PolicyJson)
	if err != nil {
		t.Fatal(err)
	}
	seal := &model.CompetitionEvaluationSeal{Staging: fixture.round.Staging, RoundId: fixture.round.RoundId,
		Environment: policy.AutomationPolicy.Environment, CompetitionId: fixture.settings.CompetitionId, SeasonId: policy.AutomationPolicy.SeasonId,
		Epoch: fixture.round.Epoch, SealedAt: fixture.round.ClosesAt, PublicationAt: fixture.round.RevealAt,
		SourceChoiceAt: fixture.round.ClosesAt.Add(time.Duration(policy.AutomationPolicy.SourceChoiceDelaySeconds) * time.Second),
		TakeoverMargin: policy.EvaluationPolicy.TakeoverMargin, TerminalCounts: map[string]int{}, Entries: []model.CompetitionSealedEntry{}}
	seal.PolicySha256, err = model.CompetitionAutomationDigest(policy)
	if err != nil {
		t.Fatal(err)
	}
	seal.InventorySha256, err = model.CompetitionAutomationDigest(seal.Entries)
	if err != nil {
		t.Fatal(err)
	}
	seal.RankingSha256 = seal.InventorySha256
	seal.SealSha256, err = model.CompetitionAutomationDigest(seal)
	if err != nil {
		t.Fatal(err)
	}
	return seal
}

// This deliberately has no new application-level hold check. It models the
// pre-migration binary's valid seal/cache/election/outbox statements, leaving
// every published PostgreSQL guard and deferred outbox constraint enabled.
func competitionFinalizationOldWriterInTx(ctx context.Context, tx server.PgTx, fixture *competitionRankingFixture, seal *model.CompetitionEvaluationSeal) error {
	content, err := json.Marshal(seal)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO competition_evaluation_seal(round_id,sealed_at,publication_at,seal_sha256,seal_json)
	 VALUES($1,$2,$3,$4,$5::jsonb)`, seal.RoundId, seal.SealedAt, seal.PublicationAt, seal.SealSha256, string(content)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE competition_round SET automation_sealed_at=$2,automation_publication_at=$3 WHERE round_id=$1`,
		seal.RoundId, seal.SealedAt, seal.PublicationAt); err != nil {
		return err
	}
	decision := automationDecision(seal, fixture.settings.AutomationPolicy, nil, nil, seal.SealedAt)
	return appendAutomationDecisionInTx(ctx, tx, &decision, seal.PublicationAt)
}

// Assertions use PostgreSQL's finite refusal class, not arbitrary SQL failure.
func requireCompetitionFinalizationSqlState(t testing.TB, err error, state string) {
	t.Helper()
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != state {
		t.Fatalf("database authority error=%v, want SQLSTATE %s", err, state)
	}
}

// Existing namespace and revision rules span both journals, even when the
// process toggle is disabled for operator recovery. Scope remains exact.
func TestCompetitionFinalizationHoldPgScopedCASAndLegacyNamespace(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newCompetitionAutomationFixture(t)
		args := competitionFinalizationHoldArgs("synthetic-finalization-hold")
		hold, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args)
		if err != nil || hold == nil || hold.Revision != 1 {
			t.Fatal("initial finalization CAS failed", hold, err)
		}
		for _, kind := range []string{"admission", "evaluation", "source_activation", "reward_delivery"} {
			conflict := args
			conflict.Kind = kind
			if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", conflict); !errors.Is(err, ErrConflict) {
				t.Fatalf("finalization key was reused by %s: %v", kind, err)
			}
		}
		// The old binary only knows its original journal. A direct otherwise
		// valid insert cannot silently reuse a key already owned by the new one.
		oldWriterConflict := captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_automation_hold
				 (hold_id,environment,competition_id,season_id,kind,revision,active,actor_id,idempotency_key,request_sha256,hold_json,created_at)
				 SELECT $2,environment,competition_id,season_id,'evaluation',1,active,actor_id,idempotency_key,request_sha256,hold_json,created_at
				 FROM competition_finalization_hold WHERE hold_id=$1`, hold.HoldId, server.NewId()))
			}, server.OptNoRetry())
		})
		requireCompetitionFinalizationSqlState(t, oldWriterConflict, "P0001")
		legacy := args
		legacy.Kind, legacy.IdempotencyKey = "admission", "synthetic-legacy-key"
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", legacy); err != nil {
			t.Fatal(err)
		}
		conflict := args
		conflict.IdempotencyKey = legacy.IdempotencyKey
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", conflict); !errors.Is(err, ErrConflict) {
			t.Fatal("legacy key was reused in the new physical journal", err)
		}
		stale := args
		stale.IdempotencyKey = "synthetic-stale-finalization-cas"
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", stale); !errors.Is(err, ErrConflict) {
			t.Fatal("stale finalization revision was accepted", err)
		}
		for _, change := range []func(*Settings){
			func(value *Settings) { value.AutomationPolicy.Environment = "synthetic-other-environment" },
			func(value *Settings) { value.CompetitionId = "synthetic-other-competition" },
			func(value *Settings) { value.AutomationPolicy.SeasonId = "synthetic-other-season" },
		} {
			settings := *fixture.settings
			change(&settings)
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(ensureAutomationNotHeldInTx(ctx, tx, &settings, "finalization"))
			})
		}
		fixture.settings.AutomationPolicy.Enabled = false
		replay, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args)
		if err != nil || replay == nil || replay.HoldId != hold.HoldId {
			t.Fatal("disabled recovery lost exact idempotent record", replay, err)
		}
		args.Active, args.ExpectedRevision, args.ReasonCode, args.IdempotencyKey = false, 1, "RECOVERY_APPROVED", "synthetic-finalization-release"
		release, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args)
		if err != nil || release == nil || release.Revision != 2 {
			t.Fatal("explicit finalization recovery failed", release, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			status, err := readCompetitionIntakeHolds(ctx, conn, fixture.settings)
			server.Raise(err)
			if status.Finalization || !status.Admission || status.Evaluation || status.RewardDelivery {
				t.Fatal("finalization release changed an independent capability", status)
			}
		})
	})
}

// A prospective hold is not an evaluation hold: a genuine queued claim,
// renewal and terminal transaction retain their authority and original clock.
func TestCompetitionFinalizationHoldPgClaimHeartbeatAndCompleteContinue(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newCompetitionAutomationFixture(t)
		fixture.store.dequeueCompetitionSignal = func(context.Context, *Settings) (*server.Id, error) { return nil, nil }
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-held-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		fixture.settings.automationLease, fixture.settings.automationRoundId = lease, fixture.round.RoundId
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", competitionFinalizationHoldArgs("synthetic-live-grading-hold")); err != nil {
			t.Fatal(err)
		}
		jobId := queueCompetitionAutomationPollJob(t, fixture)
		job, err := fixture.store.Claim(ctx, fixture.settings, "synthetic-held-worker", testWorkerImageDigest())
		if err != nil || job == nil || job.JobId != jobId || job.StartedAt == nil || job.AttemptCount != 1 {
			t.Fatal("finalization blocked a genuine claim", job, err)
		}
		startedAt := *job.StartedAt
		fixture.now = fixture.now.Add(time.Second)
		if err := fixture.store.Heartbeat(ctx, fixture.settings, "synthetic-held-worker", jobId); err != nil {
			t.Fatal("finalization blocked lease renewal", err)
		}
		fixture.now = fixture.now.Add(time.Second)
		if retry, err := fixture.store.Complete(ctx, fixture.settings, "synthetic-held-worker", jobId,
			EvaluationOutcome{Error: terminalInfrastructureError("synthetic-terminal-evidence", "Synthetic joined terminal outcome.")}); err != nil || retry {
			t.Fatal("finalization blocked exact terminal completion", retry, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var state string
			var attempts int
			var persistedStart time.Time
			server.Raise(conn.QueryRow(ctx, `SELECT state,attempt_count,started_at FROM competition_job WHERE job_id=$1`, jobId).Scan(&state, &attempts, &persistedStart))
			if state != "failed" || attempts != 1 || !persistedStart.Equal(startedAt) {
				t.Fatal("hold reset the admitted evaluation clock or attempt", state, attempts, persistedStart)
			}
		})
		// This second actual Complete also freezes the authenticated shared
		// baseline and successful score, rather than testing only failure writes.
		addCompetitionAutomationJob(t, fixture, 70)
		if result, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId); result != nil || !errors.Is(err, ErrFinalizationHeld) {
			t.Fatal("completed provisional evidence escaped finalization hold", result, err)
		}
		// The new capability must not weaken the pre-existing supervisor fence.
		fixture.now = lease.LeaseExpiresAt
		if _, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-successor-supervisor", 5*time.Minute); err != nil {
			t.Fatal(err)
		}
		if stale, err := fixture.store.Claim(ctx, fixture.settings, "synthetic-stale-worker", testWorkerImageDigest()); stale != nil || !errors.Is(err, ErrLeaseLost) {
			t.Fatal("held finalization granted a stale supervisor execution authority", stale, err)
		}
	})
}

// Old application binaries do not know this capability, so enforcement must
// precede their still-enabled original PostgreSQL guards and constraints.
func TestCompetitionFinalizationHoldPgBlocksOldWriterSealElectionAndTerminal(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newCompetitionAutomationFixture(t)
		seal := competitionFinalizationEmptySeal(t, fixture)
		args := competitionFinalizationHoldArgs("synthetic-oldwriter-hold")
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args); err != nil {
			t.Fatal(err)
		}
		for _, write := range []func(server.PgTx) error{
			func(tx server.PgTx) error { return competitionFinalizationOldWriterInTx(ctx, tx, fixture, seal) },
			func(tx server.PgTx) error {
				decision := automationDecision(seal, fixture.settings.AutomationPolicy, nil, nil, seal.SealedAt)
				return appendAutomationDecisionInTx(ctx, tx, &decision, seal.PublicationAt)
			},
			func(tx server.PgTx) error {
				_, err := tx.Exec(ctx, `UPDATE competition_round SET automation_sealed_at=$2 WHERE round_id=$1`, seal.RoundId, seal.SealedAt)
				return err
			},
			func(tx server.PgTx) error {
				_, err := tx.Exec(ctx, `UPDATE competition_round SET automation_publication_at=$2 WHERE round_id=$1`, seal.RoundId, seal.PublicationAt)
				return err
			},
			func(tx server.PgTx) error {
				_, err := tx.Exec(ctx, `UPDATE competition_round SET finalized_at=$2 WHERE round_id=$1`, seal.RoundId, seal.SealedAt)
				return err
			},
			func(tx server.PgTx) error {
				_, err := tx.Exec(ctx, `UPDATE competition_round SET winner_job_id=$2 WHERE round_id=$1`, seal.RoundId, server.NewId())
				return err
			},
		} {
			err := captureDatabaseError(func() {
				server.Tx(ctx, func(tx server.PgTx) { server.Raise(write(tx)) }, server.OptNoRetry())
			})
			requireCompetitionFinalizationSqlState(t, err, "55000")
		}
		args.Active, args.ExpectedRevision, args.ReasonCode, args.IdempotencyKey = false, 1, "RECOVERY_APPROVED", "synthetic-oldwriter-release"
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args); err != nil {
			t.Fatal(err)
		}
		// The identical legacy statement path must succeed after release; a
		// failure merely caused by malformed fixture DML cannot pass this test.
		server.Tx(ctx, func(tx server.PgTx) { server.Raise(competitionFinalizationOldWriterInTx(ctx, tx, fixture, seal)) })
		fixture.now = fixture.round.RevealAt
		result, err := fixture.store.AutomationResult(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || result == nil || result.Seal.SealSha256 != seal.SealSha256 || len(result.Decisions) != 1 {
			t.Fatal("release did not admit the valid old writer atomically", result, err)
		}
	})
}

// Once a seal commits, its scheduled publication and append-only review
// revisions remain authorized. This capability is not a retrospective embargo.
func TestCompetitionFinalizationHoldPgPreservesScheduledPublicationAndReviews(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newCompetitionAutomationFixture(t)
		addCompetitionAutomationJob(t, fixture, 70)
		addCompetitionAutomationJob(t, fixture, 80)
		fixture.now = fixture.round.ClosesAt
		original, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || original == nil || original.ProvisionalRewardWinner == nil {
			t.Fatal("pre-hold election failed", original, err)
		}
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", competitionFinalizationHoldArgs("synthetic-prospective-history-hold")); err != nil {
			t.Fatal(err)
		}
		replayed, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || replayed == nil || replayed.Seal.SealSha256 != original.Seal.SealSha256 || !replayed.Seal.PublicationAt.Equal(original.Seal.PublicationAt) {
			t.Fatal("active hold changed an already committed seal", replayed, err)
		}
		if _, err := fixture.store.AutomationResult(ctx, fixture.settings, fixture.round.RoundId); !errors.Is(err, ErrAutomationNotPublished) {
			t.Fatal("prospective hold bypassed the original reveal boundary", err)
		}
		fixture.now = fixture.round.RevealAt
		public, err := fixture.store.AutomationResult(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || public == nil || public.Seal.SealSha256 != original.Seal.SealSha256 {
			t.Fatal("hold hid an already authorized scheduled publication", public, err)
		}
		// Disqualifying B first leaves A elected; revoking A then records a
		// genuine no-winner revision. Restoring B appends the replacement.
		secondRequest := competitionAutomationReviewRequest(public, public.Seal.Entries[1], true)
		secondReview, err := fixture.store.RecordIntegrityReview(ctx, fixture.settings, fixture.round.RoundId, "synthetic-reviewer", secondRequest)
		if err != nil || secondReview == nil || secondReview.RewardDecision.Revision != 1 {
			t.Fatal("finalization blocked a nonwinner integrity finding", secondReview, err)
		}
		firstRequest := competitionAutomationReviewRequest(public, public.Seal.Entries[0], true)
		firstReview, err := fixture.store.RecordIntegrityReview(ctx, fixture.settings, fixture.round.RoundId, "synthetic-reviewer", firstRequest)
		if err != nil || firstReview == nil || firstReview.RewardDecision.Revision != 2 || firstReview.RewardDecision.SelectedJobId != nil {
			t.Fatal("finalization blocked the append-only winner revocation", firstReview, err)
		}
		current, err := fixture.store.AutomationResult(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal(err)
		}
		appeal := competitionAutomationReviewRequest(current, current.Seal.Entries[1], false)
		appeal.RewardDisposition, appeal.AppealId, appeal.SupersedesReviewId, appeal.ReasonCodes = "restored", "synthetic-finalization-appeal", &secondReview.ReviewId, []string{"APPEAL_UPHELD"}
		replacement, err := fixture.store.RecordIntegrityReview(ctx, fixture.settings, fixture.round.RoundId, "synthetic-reviewer", appeal)
		if err != nil || replacement == nil || replacement.RewardDecision.Revision != 3 || replacement.RewardDecision.SelectedJobId == nil || *replacement.RewardDecision.SelectedJobId != current.Seal.Entries[1].JobId {
			t.Fatal("finalization blocked an authorized replacement after revocation", replacement, err)
		}
		page, err := fixture.store.RewardEvents(ctx, fixture.settings, 0, 200)
		if err != nil || len(page.Events) != 3 || page.Events[0].Decision.DecisionSha256 != original.Decisions[0].DecisionSha256 || page.Events[1].Decision.SelectedJobId != nil || page.Events[2].Decision.SelectedJobId == nil {
			t.Fatal("hold hid or rewrote historical reward delivery", page, err)
		}
		// Legacy v2 cache writes were never authorized by review. Preserve
		// the existing guard rather than manufacturing a new bypass fixture.
		cacheError := captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE competition_round SET winner_job_id=$2 WHERE round_id=$1`, fixture.round.RoundId, current.Seal.Entries[1].JobId))
			}, server.OptNoRetry())
		})
		if cacheError == nil {
			t.Fatal("finalization relaxed the original legacy winner-cache prohibition")
		}
	})
}

// Successful scope omission and release clear the runtime gauge; an old-season
// finalization journal cannot override the current scope's operational status.
func TestCompetitionFinalizationHoldPgMetricScopeAndRelease(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newCompetitionAutomationFixture(t)
		args := competitionFinalizationHoldArgs("synthetic-metric-finalization")
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args); err != nil {
			t.Fatal(err)
		}
		if err := refreshAutomationOperationalMetrics(ctx, fixture.settings, fixture.now); err != nil {
			t.Fatal(err)
		}
		if got := testutil.ToFloat64(competitionAutomationHoldActive.WithLabelValues("finalization")); got != 1 {
			t.Fatal("current held scope was not reported", got)
		}
		other := *fixture.settings
		other.AutomationPolicy.SeasonId = "synthetic-new-season"
		if err := refreshAutomationOperationalMetrics(ctx, &other, fixture.now); err != nil {
			t.Fatal(err)
		}
		if got := testutil.ToFloat64(competitionAutomationHoldActive.WithLabelValues("finalization")); got != 0 {
			t.Fatal("historical season leaked into current scope", got)
		}
		args.Active, args.ExpectedRevision, args.ReasonCode, args.IdempotencyKey = false, 1, "RECOVERY_APPROVED", "synthetic-metric-release"
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args); err != nil {
			t.Fatal(err)
		}
		if err := refreshAutomationOperationalMetrics(ctx, fixture.settings, fixture.now); err != nil {
			t.Fatal(err)
		}
		if got := testutil.ToFloat64(competitionAutomationHoldActive.WithLabelValues("finalization")); got != 0 {
			t.Fatal("released scope left a stale active gauge", got)
		}
	})
}

// A positive server-side blocking edge is the permission to release; elapsed
// time and a queued client goroutine never substitute for lock ownership.
func competitionFinalizationAwaitScopeWait(ctx context.Context, waiterPid, holderPid int32) error {
	for {
		var blocked bool
		err := captureDatabaseError(func() {
			server.MaintenanceDb(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
				 WHERE pid=$1 AND wait_event_type='Lock' AND wait_event='advisory' AND $2::integer=ANY(pg_blocking_pids(pid)))`, waiterPid, holderPid).Scan(&blocked))
			})
		})
		if err != nil || blocked {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Both commit orders, including a previously absent fence tuple, must force
// a stale RR waiter to retry. Advisory serialization alone fails this proof.
func TestCompetitionFinalizationHoldPgRepeatableReadCommitOrders(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		// Holder plus observer require separate, test-owned maintenance slots.
		pop := server.Config.PushSimpleResource(server.MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 2\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		for _, seedFence := range []bool{false, true} {
			for _, holdCommitsFirst := range []bool{false, true} {
				competitionFinalizationPgCommitOrder(t, seedFence, holdCommitsFirst)
			}
		}
	})
}

// Each participant owns one actual transaction and every exit releases and
// joins both goroutines before the private database lifecycle can end.
func competitionFinalizationPgCommitOrder(t testing.TB, seedFence, holdCommitsFirst bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	fixture := newCompetitionAutomationFixture(t)
	seal := competitionFinalizationEmptySeal(t, fixture)
	args := competitionFinalizationHoldArgs("synthetic-racing-finalization-hold")
	if seedFence {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT competition_finalization_touch_scope($1,$2,$3)`, fixture.settings.AutomationPolicy.Environment, fixture.settings.CompetitionId, fixture.settings.AutomationPolicy.SeasonId))
		})
	}
	writeHold := func(tx server.PgTx) error {
		automationScopeLockInTx(ctx, tx, fixture.settings)
		_, err := recordAutomationHoldInTx(ctx, tx, fixture.settings, "synthetic-scheduler", args, fixture.now)
		return err
	}
	writeSeal := func(tx server.PgTx) error { return competitionFinalizationOldWriterInTx(ctx, tx, fixture, seal) }
	first, second := writeSeal, writeHold
	if holdCommitsFirst {
		first, second = writeHold, writeSeal
	}
	startWaiter, releaseHolder := make(chan struct{}), make(chan struct{})
	var startOnce, releaseOnce sync.Once
	start := func() { startOnce.Do(func() { close(startWaiter) }) }
	release := func() { releaseOnce.Do(func() { close(releaseHolder) }) }
	waiterSnapshot, holderOwned := make(chan int32, 1), make(chan int32, 1)
	waiterResult, holderResult := make(chan error, 1), make(chan error, 1)
	waiterJoined, holderJoined := make(chan struct{}), make(chan struct{})
	holderStarted := false
	go func() {
		defer close(waiterJoined)
		waiterResult <- captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				var pid int32
				var visibleHolds int
				server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid(),(SELECT count(*) FROM competition_finalization_hold WHERE competition_id=$1)`, fixture.settings.CompetitionId).Scan(&pid, &visibleHolds))
				if visibleHolds != 0 {
					panic(errors.New("synthetic stale snapshot was not established before the first hold"))
				}
				waiterSnapshot <- pid
				select {
				case <-startWaiter:
				case <-ctx.Done():
					panic(ctx.Err())
				}
				server.Raise(second(tx))
			}, server.OptNoRetry(), server.TxLockTimeout(20*time.Second))
		})
	}()
	defer func() {
		start()
		release()
		cancel()
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 40*time.Second)
		defer stop()
		joins := []<-chan struct{}{waiterJoined}
		if holderStarted {
			joins = append(joins, holderJoined)
		}
		for _, joined := range joins {
			select {
			case <-joined:
			case <-cleanup.Done():
				t.Error("finalization race database owner did not join cleanup")
			}
		}
	}()
	var waiterPid int32
	select {
	case waiterPid = <-waiterSnapshot:
	case err := <-waiterResult:
		t.Fatal("stale waiter failed before snapshot", err)
	case <-ctx.Done():
		t.Fatal("stale waiter did not acknowledge its snapshot")
	}
	holderStarted = true
	go func() {
		defer close(holderJoined)
		holderResult <- captureDatabaseError(func() {
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.Raise(first(tx))
				var pid int32
				server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
				holderOwned <- pid
				select {
				case <-releaseHolder:
				case <-ctx.Done():
					panic(ctx.Err())
				}
			}, server.OptNoRetry())
		})
	}()
	var holderPid int32
	select {
	case holderPid = <-holderOwned:
	case err := <-holderResult:
		t.Fatal("first writer failed before owning scope", err)
	case <-ctx.Done():
		t.Fatal("first writer did not acknowledge scope custody")
	}
	start()
	if err := competitionFinalizationAwaitScopeWait(ctx, waiterPid, holderPid); err != nil {
		t.Fatal("could not prove the exact finalization scope wait", err)
	}
	release()
	select {
	case err := <-holderResult:
		if err != nil {
			t.Fatal("first scope writer did not commit", err)
		}
	case <-ctx.Done():
		t.Fatal("first writer did not finish")
	}
	var staleErr error
	select {
	case staleErr = <-waiterResult:
	case <-ctx.Done():
		t.Fatal("stale snapshot writer did not finish")
	}
	requireCompetitionFinalizationSqlState(t, staleErr, "40001")
	fixture.now = fixture.round.RevealAt
	if holdCommitsFirst {
		if result, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId); result != nil || !errors.Is(err, ErrFinalizationHeld) {
			t.Fatal("old snapshot escaped the committed hold", result, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var seals, decisions int
			server.Raise(conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM competition_evaluation_seal WHERE round_id=$1),(SELECT count(*) FROM competition_reward_decision WHERE round_id=$1)`, seal.RoundId).Scan(&seals, &decisions))
			if seals != 0 || decisions != 0 {
				t.Fatal("refused stale old writer committed a terminal artifact", seals, decisions)
			}
		})
	} else {
		if _, err := fixture.store.SetAutomationHold(ctx, fixture.settings, "synthetic-scheduler", args); err != nil {
			t.Fatal("fresh hold did not retry against committed seal", err)
		}
		result, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || result == nil || result.Seal.SealSha256 != seal.SealSha256 || len(result.Decisions) != 1 {
			t.Fatal("later hold revoked or duplicated an already committed decision", result, err)
		}
	}
}
