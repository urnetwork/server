package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"strings"
	"testing"
	"time"
)

func TestCompetitionReviewHandoffCannotUseOrdinaryReviewer(t *testing.T) {
	_, err := (PostgresStore{}).RecordIntegrityReview(context.Background(), nil, server.NewId(), "synthetic-reviewer", model.CompetitionIntegrityReviewRequest{HandoffId: server.NewId().String()})
	if !errors.Is(err, ErrConflict) {
		t.Fatal("ordinary model reviewer reached handoff storage", err)
	}
}

func TestCompetitionReviewHandoffDurablePendingThenExactOperatorImport(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixtureWithWinnerPolicy(t, true, model.CompetitionRuntimeBlobCapability, model.CompetitionSnowWinnerReviewPolicy, server.NowUtc().Add(-2*time.Hour))
		t.Log("handoff fixture: complete authentic synthetic worker result")
		job := addCompetitionAutomationJob(t, fixture, 80)
		fixture.now = fixture.round.RevealAt.Add(time.Second)
		sealed, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil {
			t.Fatal("seal/elect synthetic candidate", err)
		}
		if sealed.ProvisionalRewardWinner == nil || *sealed.ProvisionalRewardWinner != job.JobId {
			t.Fatal("synthetic election missing")
		}
		fixture.now = server.NowUtc()
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-handoff-owner", time.Minute)
		if err != nil {
			t.Fatal("acquire handoff owner lease", err)
		}
		entry := sealed.Seal.Entries[0]
		decision := sealed.Decisions[len(sealed.Decisions)-1]
		args := model.CompetitionReviewHandoffArgs{RoundId: fixture.round.RoundId, JobId: entry.JobId, RewardRevision: decision.Revision, DecisionSha256: decision.DecisionSha256,
			SealSha256: sealed.Seal.SealSha256, PatchSha256: entry.PatchSha256, BaseSourceLockSha256: entry.BaseSourceLockSha256, ScoreSha256: entry.ScoreSha256, ArtifactManifestSha256: entry.ArtifactManifestSha256, BaselineSha256: sealed.Seal.BaselineSha256,
			AuthorizationSha256: strings.Repeat("a", 64), ProviderSha256: strings.Repeat("b", 64), Protocol: model.CompetitionReviewHandoffProtocol, TimeoutSeconds: 3600}
		plan := model.CompetitionReviewHandoffPlan{PlanSha256: strings.Repeat("c", 64), BundleSha256: strings.Repeat("d", 64), CallIds: []string{"qwen-final", "gemma-final"}}
		planner := func(model.CompetitionReviewHandoff) (model.CompetitionReviewHandoffPlan, error) { return plan, nil }
		owner, err := fixture.store.BeginReviewHandoff(ctx, fixture.settings, *lease, "synthetic-capture", args, planner)
		if err != nil {
			t.Fatal("begin durable handoff", err)
		}
		if owner.Deadline.Sub(owner.StartedAt) != time.Hour {
			t.Fatal("prospective clock did not own its approved duration")
		}
		testReviewHandoffClockBoundary(t, fixture, owner)
		replay, err := fixture.store.BeginReviewHandoff(ctx, fixture.settings, *lease, "synthetic-capture", args, planner)
		if err != nil || replay.HandoffId != owner.HandoffId || !replay.StartedAt.Equal(owner.StartedAt) || !replay.Deadline.Equal(owner.Deadline) {
			t.Fatal("owner replay renewed or lost clock", err)
		}
		changed := args
		changed.TimeoutSeconds++
		if _, err = fixture.store.BeginReviewHandoff(ctx, fixture.settings, *lease, "synthetic-capture", changed, planner); !errors.Is(err, ErrConflict) {
			t.Fatal("owner duration renewed", err)
		}
		before, err := fixture.store.AutomationResult(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || len(before.IntegrityReviews) != 0 || before.RewardRevision != decision.Revision {
			t.Fatal("pending owner changed authority", err)
		}
		proposal := competitionAutomationReviewRequest(sealed, entry, false)
		proposal.HandoffId, proposal.ExpectedDecisionSha256 = owner.HandoffId.String(), decision.DecisionSha256
		proposal.ReviewerModel, proposal.ReviewerReasoning = model.CompetitionSnowWinnerReviewModel, model.CompetitionSnowWinnerReviewReasoning
		proposal.SourceDisposition = "approved"
		findings := proposal.Findings
		proposal.Findings = nil
		for _, member := range model.CompetitionSnowWinnerReviewModels() {
			for _, finding := range findings {
				finding.Model = member
				proposal.Findings = append(proposal.Findings, finding)
			}
		}
		proposalSha, _ := model.CompetitionAutomationDigest(proposal)
		completion := model.CompetitionReviewHandoffCompletion{HandoffId: owner.HandoffId, PlanSha256: plan.PlanSha256, Proposal: &proposal, ProposalSha256: proposalSha,
			Evidence: model.CompetitionReviewHandoffRetention{Key: "synthetic/retained-review", VersionId: "synthetic-exact-version", Sha256: proposal.PrivateEvidenceSha256, RetainUntil: fixture.settings.RetainUntil}}
		if _, err = fixture.store.CompleteReviewHandoff(ctx, fixture.settings, fixture.round.RoundId, "synthetic-capture", completion); !errors.Is(err, ErrConflict) {
			t.Fatal("incomplete model census accepted", err)
		}
		outOfOrder := model.CompetitionReviewHandoffCall{HandoffId: owner.HandoffId, CallId: plan.CallIds[1], Kind: "claim", RequestSha256: strings.Repeat("e", 64)}
		if _, err = fixture.store.RecordReviewHandoffCall(ctx, fixture.settings, fixture.round.RoundId, "synthetic-capture", outOfOrder); !errors.Is(err, ErrConflict) {
			t.Fatal("later planned call bypassed preceding coverage", err)
		}
		for index, id := range plan.CallIds {
			call := model.CompetitionReviewHandoffCall{HandoffId: owner.HandoffId, CallId: id, Kind: "claim", RequestSha256: strings.Repeat("e", 64)}
			if _, err = fixture.store.RecordReviewHandoffCall(ctx, fixture.settings, fixture.round.RoundId, "synthetic-capture", call); err != nil {
				t.Fatal("claim planned call", id, err)
			}
			if _, err = fixture.store.RecordReviewHandoffCall(ctx, fixture.settings, fixture.round.RoundId, "synthetic-capture", call); !errors.Is(err, ErrReviewCallAlreadyClaimed) {
				t.Fatal("duplicate dispatch permitted", err)
			}
			if index == 0 {
				if _, err = fixture.store.RecordReviewHandoffCall(ctx, fixture.settings, fixture.round.RoundId, "synthetic-capture", outOfOrder); !errors.Is(err, ErrConflict) {
					t.Fatal("inflight preceding call allowed a later draw", err)
				}
			}
			call.Kind, call.EvidenceSha256 = "receipt", strings.Repeat("f", 64)
			if _, err = fixture.store.RecordReviewHandoffCall(ctx, fixture.settings, fixture.round.RoundId, "synthetic-capture", call); err != nil {
				t.Fatal("retain original call receipt", id, err)
			}
		}
		completed, err := fixture.store.CompleteReviewHandoff(ctx, fixture.settings, fixture.round.RoundId, "synthetic-capture", completion)
		if err != nil {
			t.Fatal("complete non-authorizing pending proposal", err)
		}
		pending, err := fixture.store.AutomationResult(ctx, fixture.settings, fixture.round.RoundId)
		if err != nil || len(pending.IntegrityReviews) != 0 || pending.RewardRevision != decision.Revision {
			t.Fatal("model completion granted authority", err)
		}
		if _, err = fixture.store.RecordIntegrityReview(ctx, fixture.settings, fixture.round.RoundId, "synthetic-model-reviewer", proposal); !errors.Is(err, ErrConflict) {
			t.Fatal("ordinary reviewer imported handoff", err)
		}
		request := model.CompetitionReviewHandoffImport{HandoffId: owner.HandoffId, RoundId: fixture.round.RoundId, DecisionSha256: decision.DecisionSha256, ProposalSha256: proposalSha, EvidenceSha256: completion.Evidence.Sha256, Disposition: "publish_exact_model_proposal"}
		wrong := request
		wrong.ProposalSha256 = strings.Repeat("1", 64)
		if _, err = fixture.store.ImportReviewHandoff(ctx, fixture.settings, "synthetic-human", wrong); !errors.Is(err, ErrConflict) {
			t.Fatal("changed operator proposal accepted", err)
		}
		result, err := fixture.store.ImportReviewHandoff(ctx, fixture.settings, "synthetic-human", request)
		if err != nil {
			t.Fatal("explicit operator imports exact proposal", err)
		}
		if result.ModelCompletedAt == nil || !result.ModelCompletedAt.Equal(completed.CompletedAt) || result.ActorId != "synthetic-human" || result.CreatedAt.Before(completed.CompletedAt) {
			t.Fatal("operator import falsified model or insertion clock")
		}
		replayed, err := fixture.store.ImportReviewHandoff(ctx, fixture.settings, "synthetic-human", request)
		if err != nil || replayed.ReviewId != result.ReviewId {
			t.Fatal("operator exact replay duplicated review", err)
		}
	})
}

func TestCompetitionReviewHandoffSupervisorNeverResamplesPendingOwner(t *testing.T) {
	fixture := newWinnerClosingFixture()
	checkpoint := &model.CompetitionWinnerReviewCheckpoint{HandoffId: server.NewId().String(), RoundId: fixture.snapshot.Round.RoundId, JobId: *fixture.snapshot.WinnerReview.CurrentDecision.SelectedJobId,
		RewardRevision: fixture.snapshot.WinnerReview.CurrentDecision.Revision, RewardDecisionSha256: fixture.snapshot.WinnerReview.CurrentDecision.DecisionSha256, StartedAt: fixture.now, Deadline: fixture.now.Add(time.Hour)}
	transition := &model.CompetitionSupervisorTransition{TargetEpoch: 2, State: "winner_review_pending", Revision: 1, WinnerReview: checkpoint}
	fixture.snapshot.Transition = transition
	supervisor := CompetitionSupervisor{Driver: fixture}
	_, _, err, handled := supervisor.reconcileWinnerReview(context.Background(), model.CompetitionSupervisorLease{}, &fixture.snapshot, transition, fixture.now)
	if !handled || !errors.Is(err, ErrWinnerReviewPending) || fixture.reviews != 0 || fixture.choices != 0 {
		t.Fatal("pending native handoff invoked an automatic model or source action", err)
	}
	fixture.now = checkpoint.Deadline
	_, _, err, handled = supervisor.reconcileWinnerReview(context.Background(), model.CompetitionSupervisorLease{}, &fixture.snapshot, fixture.snapshot.Transition, fixture.now)
	if !handled || !errors.Is(err, ErrWinnerReviewTimedOut) || fixture.reviews != 0 || *fixture.snapshot.Transition.WinnerReview != *checkpoint {
		t.Fatal("expired native handoff reset or resampled", err)
	}
}

// Only the disposable TestEnv schema gets this clock seam. The transaction
// begins before an explicit barrier; time then advances without sleeping. Both
// the supplied marker and transaction start remain early, so only the guard's
// fresh post-lock clock can reject the late claim for the right reason.
func testReviewHandoffClockBoundary(t testing.TB, fixture *competitionRankingFixture, owner *model.CompetitionReviewHandoff) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clockSql := `CREATE OR REPLACE FUNCTION competition_review_handoff_clock() RETURNS timestamp LANGUAGE sql VOLATILE AS $synthetic_clock$ SELECT COALESCE(NULLIF(current_setting('urnetwork.synthetic_review_time',true),'')::timestamp,timezone('UTC',clock_timestamp())); $synthetic_clock$;`
	server.Db(ctx, func(conn server.PgConn) { server.RaisePgResult(conn.Exec(ctx, clockSql)) })
	defer server.Db(context.Background(), func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(context.Background(), `CREATE OR REPLACE FUNCTION competition_review_handoff_clock() RETURNS timestamp LANGUAGE sql VOLATILE AS $handoff_clock$ SELECT timezone('UTC',clock_timestamp()); $handoff_clock$;`))
	})
	barrier := make(chan struct{})
	resume := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				automationScopeLockInTx(ctx, tx, fixture.settings)
				started, err := reviewHandoffDatabaseTime(ctx, tx)
				server.Raise(err)
				server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_review_handoff_actor','synthetic-capture',true)`))
				call := model.CompetitionReviewHandoffCall{HandoffId: owner.HandoffId, CallId: owner.Plan.CallIds[0], Kind: "claim", RequestSha256: strings.Repeat("e", 64), RecordedAt: started}
				content, err := json.Marshal(call)
				server.Raise(err)
				close(barrier)
				select {
				case <-resume:
				case <-ctx.Done():
					panic(ctx.Err())
				}
				server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.synthetic_review_time',$1,true)`, owner.Deadline.Add(time.Second).Format(time.RFC3339Nano)))
				var oldTransaction, timeNow time.Time
				server.Raise(tx.QueryRow(ctx, `SELECT timezone('UTC',transaction_timestamp()),competition_review_handoff_clock()`).Scan(&oldTransaction, &timeNow))
				if !oldTransaction.Before(owner.Deadline) || !timeNow.After(owner.Deadline) {
					panic(errors.New("synthetic transaction clock barrier is invalid"))
				}
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_review_handoff_call(handoff_id,call_id,kind,call_json,recorded_at) VALUES($1,$2,'claim',$3::jsonb,$4)`, owner.HandoffId, call.CallId, string(content), started))
			}, server.TxReadCommitted)
		})
	}()
	select {
	case <-barrier:
		close(resume)
	case err := <-result:
		t.Fatal("claim clock transaction failed before barrier", err)
	case <-ctx.Done():
		t.Fatal("claim clock barrier unavailable", ctx.Err())
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "expired, completed or changed review cannot claim a call") {
			t.Fatal("post-barrier SQL admission used stale transaction time", err)
		}
	case <-ctx.Done():
		t.Fatal("claim clock barrier did not finish", ctx.Err())
	}
	// A separate native transaction also reads the current database clock,
	// independently of the store's injected application clock.
	constantClock := fmt.Sprintf(`CREATE OR REPLACE FUNCTION competition_review_handoff_clock() RETURNS timestamp LANGUAGE sql VOLATILE AS $synthetic_late$ SELECT '%s'::timestamptz AT TIME ZONE 'UTC'; $synthetic_late$;`, owner.Deadline.Add(time.Second).Format(time.RFC3339Nano))
	server.Db(ctx, func(conn server.PgConn) { server.RaisePgResult(conn.Exec(ctx, constantClock)) })
	_, err := fixture.store.RecordReviewHandoffCall(ctx, fixture.settings, fixture.round.RoundId, "synthetic-capture", model.CompetitionReviewHandoffCall{HandoffId: owner.HandoffId, CallId: owner.Plan.CallIds[0], Kind: "claim", RequestSha256: strings.Repeat("e", 64)})
	if !errors.Is(err, ErrWinnerReviewTimedOut) {
		t.Fatal("native claim ignored the fresh database deadline", err)
	}
}
