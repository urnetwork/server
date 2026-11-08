package controller

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"strings"
	"sync"
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
		testReviewHandoffSqlRejectsPartialProposal(t, fixture, completion)
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

// These bypass the native typed validator deliberately. Every mutation has a
// complete timely call census, so the SQL proposal predicate itself must refuse
// absent/null critical fields. Even an unexpected acceptance rolls back.
func testReviewHandoffSqlRejectsPartialProposal(t testing.TB, fixture *competitionRankingFixture, completion model.CompetitionReviewHandoffCompletion) {
	t.Helper()
	ctx := context.Background()
	rollback := errors.New("synthetic proposal accepted; roll back mutation")
	paths := [][]string{
		{"reward_disposition"}, {"source_disposition"}, {"reviewer_model"}, {"reviewer_reasoning"}, {"findings"}, {"reason_codes"},
		{"findings", "0", "model"}, {"findings", "0", "criterion"}, {"findings", "0", "disposition"},
		{"findings", "0", "reason_code"}, {"findings", "0", "explanation"}, {"findings", "0", "evidence_sha256"},
	}
	for _, path := range paths {
		for _, omit := range []bool{false, true} {
			var inserted bool
			err := captureDatabaseError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					automationScopeLockInTx(ctx, tx, fixture.settings)
					now, err := reviewHandoffDatabaseTime(ctx, tx)
					server.Raise(err)
					server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_review_handoff_actor','synthetic-capture',true)`))
					completion.CompletedAt = now
					content, err := json.Marshal(completion)
					server.Raise(err)
					fullPath := append([]string{"proposal"}, path...)
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_review_handoff_completion(handoff_id,completion_json,completed_at)
					 VALUES($1,CASE WHEN $4 THEN $2::jsonb #- $3::text[] ELSE jsonb_set($2::jsonb,$3::text[],'null'::jsonb) END,$5)`,
						completion.HandoffId, string(content), fullPath, omit, now))
					inserted = true
					panic(rollback)
				}, server.TxReadCommitted)
			})
			if inserted || err == nil || !strings.Contains(err.Error(), "only complete timely exact Qwen/Gemma evidence can form a pending proposal") {
				t.Fatal("direct SQL partial proposal was not refused by its authority guard", strings.Join(path, "."), "omitted", omit, err)
			}
		}
	}
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

// Only the disposable TestEnv schema gets this non-MVCC clock seam. A positive
// pg_blocking_pids edge proves the claim is waiting on the round row before the
// clock advances. Both its marker and transaction start remain early; only the
// guard's fresh post-lock clock can reject it. No elapsed sleep proves expiry.
func testReviewHandoffClockBoundary(t testing.TB, fixture *competitionRankingFixture, owner *model.CompetitionReviewHandoff) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	pop := server.Config.PushSimpleResource(server.MaintenancePgConfigResourceName, []byte("min_connections: 0\nmax_connections: 2\n"))
	server.PgReset()
	defer func() { pop(); server.PgReset() }()
	clockSql := `CREATE SEQUENCE synthetic_review_handoff_clock;
	 CREATE OR REPLACE FUNCTION competition_review_handoff_clock() RETURNS timestamp LANGUAGE sql VOLATILE AS $synthetic_clock$
	 SELECT CASE WHEN is_called THEN timestamp 'epoch' + last_value * interval '1 microsecond' ELSE timezone('UTC',clock_timestamp()) END FROM synthetic_review_handoff_clock;
	 $synthetic_clock$;`
	server.Db(ctx, func(conn server.PgConn) { server.RaisePgResult(conn.Exec(ctx, clockSql)) })
	defer server.Db(context.Background(), func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(context.Background(), `CREATE OR REPLACE FUNCTION competition_review_handoff_clock() RETURNS timestamp LANGUAGE sql VOLATILE AS $handoff_clock$ SELECT timezone('UTC',clock_timestamp()); $handoff_clock$; DROP SEQUENCE synthetic_review_handoff_clock;`))
	})
	held, waiting := make(chan int32, 1), make(chan int32, 1)
	release := make(chan struct{})
	holderResult, holderJoined := make(chan error, 1), make(chan struct{})
	var releaseOnce sync.Once
	unlock := func() { releaseOnce.Do(func() { close(release) }) }
	go func() {
		defer close(holderJoined)
		holderResult <- captureDatabaseError(func() {
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				var pid int32
				server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid() FROM competition_round WHERE round_id=$1 FOR UPDATE`, owner.Args.RoundId).Scan(&pid))
				held <- pid
				select {
				case <-release:
				case <-ctx.Done():
					panic(ctx.Err())
				}
			}, server.TxReadCommitted, server.OptNoRetry())
		})
	}()
	defer func() { unlock(); cancel(); <-holderJoined }()
	var holderPid int32
	select {
	case holderPid = <-held:
	case err := <-holderResult:
		t.Fatal("claim clock round holder failed", err)
	case <-ctx.Done():
		t.Fatal("claim clock round holder unavailable", ctx.Err())
	}
	result := make(chan error, 1)
	waiterJoined := make(chan struct{})
	go func() {
		defer close(waiterJoined)
		result <- captureDatabaseError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				started, err := reviewHandoffDatabaseTime(ctx, tx)
				server.Raise(err)
				server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_review_handoff_actor','synthetic-capture',true)`))
				var pid int32
				var transactionStarted time.Time
				server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid(),timezone('UTC',transaction_timestamp())`).Scan(&pid, &transactionStarted))
				if !started.Before(owner.Deadline) || !transactionStarted.Before(owner.Deadline) {
					panic(errors.New("synthetic transaction clock barrier is invalid"))
				}
				call := model.CompetitionReviewHandoffCall{HandoffId: owner.HandoffId, CallId: owner.Plan.CallIds[0], Kind: "claim", RequestSha256: strings.Repeat("e", 64), RecordedAt: started}
				content, err := json.Marshal(call)
				server.Raise(err)
				waiting <- pid
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_review_handoff_call(handoff_id,call_id,kind,call_json,recorded_at) VALUES($1,$2,'claim',$3::jsonb,$4)`, owner.HandoffId, call.CallId, string(content), started))
			}, server.TxReadCommitted, server.OptNoRetry())
		})
	}()
	defer func() { unlock(); cancel(); <-waiterJoined }()
	var waiterPid int32
	select {
	case waiterPid = <-waiting:
	case err := <-result:
		t.Fatal("claim clock transaction failed before barrier", err)
	case <-ctx.Done():
		t.Fatal("claim clock barrier unavailable", ctx.Err())
	}
	if err := testReviewHandoffAwaitRowWait(ctx, waiterPid, holderPid); err != nil {
		t.Fatal("claim did not reach the actual round-row lock barrier", err)
	}
	server.MaintenanceDb(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `SELECT setval('synthetic_review_handoff_clock',$1,true)`, owner.Deadline.Add(time.Second).UnixMicro()))
		var observed time.Time
		server.Raise(conn.QueryRow(ctx, `SELECT competition_review_handoff_clock()`).Scan(&observed))
		if !observed.After(owner.Deadline) {
			t.Fatal("synthetic database clock did not advance while claim was blocked")
		}
	})
	unlock()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "expired, completed or changed review cannot claim a call") {
			t.Fatal("post-lock SQL admission used stale transaction time", err)
		}
	case <-ctx.Done():
		t.Fatal("claim clock barrier did not finish", ctx.Err())
	}
	if err := <-holderResult; err != nil {
		t.Fatal("claim clock round holder failed on release", err)
	}
	// A separate native transaction also reads the current database clock,
	// independently of the store's injected application clock.
	_, err := fixture.store.RecordReviewHandoffCall(ctx, fixture.settings, fixture.round.RoundId, "synthetic-capture", model.CompetitionReviewHandoffCall{HandoffId: owner.HandoffId, CallId: owner.Plan.CallIds[0], Kind: "claim", RequestSha256: strings.Repeat("e", 64)})
	if !errors.Is(err, ErrWinnerReviewTimedOut) {
		t.Fatal("native claim ignored the fresh database deadline", err)
	}
}

func testReviewHandoffAwaitRowWait(ctx context.Context, waiterPid, holderPid int32) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err := captureDatabaseError(func() {
			server.MaintenanceDb(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND wait_event IN ('transactionid','tuple') AND $2::integer=ANY(pg_blocking_pids(pid)))`, waiterPid, holderPid).Scan(&blocked))
			})
		})
		if err != nil || blocked {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
