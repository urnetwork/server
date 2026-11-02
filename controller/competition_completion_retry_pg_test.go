// Disposable-PostgreSQL controls prove completion rollback, fresh authority and
// exact finished-outcome persistence. None of these tests use a live service.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Entire synthetic rows expose partial metadata, authority or budget changes.
type competitionCompletionPgSnapshot struct {
	jobJson, slotJson []byte
	events, baselines int
	terminalEvents    int
}

// Nullable subqueries also represent a deliberately missing job or slot row.
func readCompetitionCompletionPgSnapshot(ctx context.Context, job *queuedJob) (snapshot competitionCompletionPgSnapshot) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
		 (SELECT to_jsonb(j) FROM competition_job j WHERE j.job_id=$1),
		 (SELECT to_jsonb(s) FROM competition_worker_slot s WHERE s.slot_id=1),
		 (SELECT count(*) FROM competition_job_event WHERE job_id=$1),
		 (SELECT count(*) FROM competition_round_baseline WHERE round_id=$2),
		 (SELECT count(*) FROM competition_job_event WHERE job_id=$1 AND event_type IN ('failed','succeeded','infrastructure_retry'))`,
			job.JobId, job.RoundId).Scan(&snapshot.jobJson, &snapshot.slotJson, &snapshot.events, &snapshot.baselines, &snapshot.terminalEvents))
	})
	return snapshot
}

// Suppressing either job transition or slot release must roll back even an
// already inserted shared baseline, event and prior update in that transaction.
func TestCompetitionCompletePgRollsBackSuppressedUpdates(t *testing.T) {
	for _, check := range []struct {
		name, table string
		retry       bool
	}{
		{name: "terminal-job", table: "competition_job"},
		{name: "terminal-slot", table: "competition_worker_slot"},
		{name: "retry-job", table: "competition_job", retry: true},
		{name: "retry-slot", table: "competition_worker_slot", retry: true},
	} {
		testEnv := server.DefaultTestEnv()
		testEnv.RerunCount = 0
		testEnv.Run(t, func(t testing.TB) {
			ctx := context.Background()
			fixture, job := newCompetitionLeasePgFixture(t, ctx, "completion-"+check.name)
			outcome := EvaluationOutcome{Score: competitionRankingScore(90, 110)}
			if check.retry {
				outcome = EvaluationOutcome{Error: infrastructureError("synthetic_completion_failure", "synthetic retained evaluator failure")}
			}
			outcome = testSharedBaselineOutcome(t, fixture.settings, job, outcome)
			before := readCompetitionCompletionPgSnapshot(ctx, job)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_suppress_completion() RETURNS trigger AS $body$
				 BEGIN RETURN NULL; END $body$ LANGUAGE plpgsql;`))
				// The relation names are fixed test cases, never runtime input.
				server.RaisePgResult(tx.Exec(ctx, `CREATE TRIGGER synthetic_suppress_completion BEFORE UPDATE ON `+check.table+`
				 FOR EACH ROW EXECUTE FUNCTION synthetic_suppress_completion();`))
			})
			fixture.clock = fixture.clock.Add(time.Second)
			if retry, err := fixture.store.Complete(ctx, fixture.settings, "synthetic-lease-worker", job.JobId, outcome); !errors.Is(err, ErrLeaseLost) || retry {
				t.Fatalf("%s suppressed update did not roll back with lost authority: retry=%t, %v", check.name, retry, err)
			}
			if after := readCompetitionCompletionPgSnapshot(ctx, job); !reflect.DeepEqual(after, before) {
				t.Fatalf("%s committed partial completion, baseline, event or release", check.name)
			}
		})
	}
}

// Every current authority component must exist, match and remain unexpired.
func TestCompetitionCompletePgRejectsInvalidAuthority(t *testing.T) {
	for _, check := range []struct {
		name, query string
	}{
		{name: "missing-slot", query: `DELETE FROM competition_worker_slot WHERE slot_id=1`},
		{name: "missing-job"},
		{name: "wrong-slot-owner", query: `UPDATE competition_worker_slot SET worker_id='synthetic-other-worker' WHERE slot_id=1`},
		{name: "wrong-job-owner", query: `UPDATE competition_job SET lease_owner='synthetic-other-worker' WHERE job_id=$1`},
		{name: "null-slot-job", query: `UPDATE competition_worker_slot SET job_id=NULL WHERE slot_id=1`},
		{name: "null-slot-expiry", query: `UPDATE competition_worker_slot SET lease_expires_at=NULL WHERE slot_id=1`},
		{name: "null-job-expiry", query: `UPDATE competition_job SET lease_expires_at=NULL WHERE job_id=$1`},
		{name: "expired-slot", query: `UPDATE competition_worker_slot SET lease_expires_at=$1 WHERE slot_id=1`},
		{name: "expired-job", query: `UPDATE competition_job SET lease_expires_at=$2 WHERE job_id=$1`},
	} {
		testEnv := server.DefaultTestEnv()
		testEnv.RerunCount = 0
		testEnv.Run(t, func(t testing.TB) {
			ctx := context.Background()
			fixture, job := newCompetitionLeasePgFixture(t, ctx, "complete-"+check.name)
			completeJobId := job.JobId
			if check.name == "missing-job" {
				completeJobId = server.NewId()
			} else {
				server.Db(ctx, func(conn server.PgConn) {
					arguments := []any{}
					switch check.name {
					case "wrong-job-owner", "null-job-expiry":
						arguments = []any{job.JobId}
					case "expired-slot":
						arguments = []any{fixture.clock}
					case "expired-job":
						arguments = []any{job.JobId, fixture.clock}
					}
					server.RaisePgResult(conn.Exec(ctx, check.query, arguments...))
				}, server.OptReadWrite())
			}
			before := readCompetitionCompletionPgSnapshot(ctx, job)
			outcome := EvaluationOutcome{Error: terminalInfrastructureError("synthetic_terminal_failure", "synthetic completion metadata")}
			if retry, err := fixture.store.Complete(ctx, fixture.settings, "synthetic-lease-worker", completeJobId, outcome); !errors.Is(err, ErrLeaseLost) || retry {
				t.Fatalf("%s accepted invalid authority: retry=%t, %v", check.name, retry, err)
			}
			if after := readCompetitionCompletionPgSnapshot(ctx, job); !reflect.DeepEqual(after, before) {
				t.Fatalf("%s changed a job, slot, baseline, event or execution budget", check.name)
			}
		})
	}
}

// The explicit boundary precedes SQL against a genuinely held authority row;
// it proves post-boundary clock sampling, not an observed pg_stat_activity wait.
func TestCompetitionCompletePgSamplesClockAfterAuthorityLocks(t *testing.T) {
	for _, phase := range []string{"completion_slot", "completion_job"} {
		testEnv := server.DefaultTestEnv()
		testEnv.RerunCount = 0
		testEnv.Run(t, func(t testing.TB) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			fixture, job := newCompetitionLeasePgFixture(t, ctx, phase)
			clock := &competitionLeaseRetryClock{now: fixture.clock}
			fixture.store.now = clock.Now
			query, arguments := `SELECT 1 FROM competition_worker_slot WHERE slot_id=1 FOR UPDATE`, []any{}
			if phase == "completion_job" {
				query, arguments = `SELECT 1 FROM competition_job WHERE job_id=$1 FOR UPDATE`, []any{job.JobId}
			}
			releaseRow, holderDone := holdCompetitionLeasePgRow(t, ctx, query, arguments...)
			admitted, resume := make(chan struct{}), make(chan struct{})
			var resumeOnce sync.Once
			resumeCompletion := func() { resumeOnce.Do(func() { close(resume) }) }
			t.Cleanup(resumeCompletion)
			fixture.store.beforeLeaseRowLockForTest = func(actual string) {
				if actual == phase {
					close(admitted)
					select {
					case <-resume:
					case <-ctx.Done():
						panic(ctx.Err())
					}
				}
			}
			before := readCompetitionCompletionPgSnapshot(ctx, job)
			done := make(chan error, 1)
			go func() {
				_, err := fixture.store.Complete(ctx, fixture.settings, "synthetic-lease-worker", job.JobId,
					EvaluationOutcome{Error: terminalInfrastructureError("synthetic_terminal_failure", "synthetic completion metadata")})
				done <- err
			}()
			select {
			case <-admitted:
			case <-ctx.Done():
				t.Fatal("completion did not admit its held authority boundary")
			}
			clock.Advance(time.Duration(fixture.settings.WorkerLeaseSeconds) * time.Second)
			releaseRow()
			if err := <-holderDone; err != nil {
				t.Fatal(err)
			}
			resumeCompletion()
			if err := <-done; !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("completion revived authority across held %s lock: %v", phase, err)
			}
			if after := readCompetitionCompletionPgSnapshot(ctx, job); !reflect.DeepEqual(after, before) {
				t.Fatal("expired held authority committed completion metadata")
			}
		})
	}
}

// The public decorator observes the real first transaction error, never injects it.
type competitionCompletionPgStore struct {
	PostgresStore
	calls int
	first func(error)
}

// Each return follows the entire actual database operation and its rollback.
func (self *competitionCompletionPgStore) Complete(ctx context.Context, settings *Settings, workerId string, jobId server.Id, outcome EvaluationOutcome) (bool, error) {
	self.calls++
	retry, err := self.PostgresStore.Complete(ctx, settings, workerId, jobId, outcome)
	if self.calls == 1 && self.first != nil {
		self.first(err)
	}
	return retry, err
}

// A real 55P03 rolls back before the wait releases the holder; the same scored
// outcome then commits once, with no new evaluator, claim or execution lifetime.
func TestCompetitionCompletePgLockTimeoutRetriesSameOutcome(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		fixture, job := newCompetitionLeasePgFixture(t, ctx, "complete-real-lock")
		before := readCompetitionCompletionPgSnapshot(ctx, job)
		outcome := testSharedBaselineOutcome(t, fixture.settings, job, EvaluationOutcome{Score: competitionRankingScore(90, 110)})
		clock := &competitionLeaseRetryClock{now: fixture.clock}
		fixture.store.now = clock.Now
		releaseRow, holderDone := holdCompetitionLeasePgRow(t, ctx, `SELECT 1 FROM competition_job WHERE job_id=$1 FOR UPDATE`, job.JobId)
		store := &competitionCompletionPgStore{PostgresStore: fixture.store, first: func(err error) {
			if !server.IsLockTimeout(err) {
				t.Fatalf("controlled held completion did not return PostgreSQL55P03: %v", err)
			}
			if rolledBack := readCompetitionCompletionPgSnapshot(ctx, job); !reflect.DeepEqual(rolledBack, before) {
				t.Fatal("first lock-timeout transaction did not roll back completely")
			}
		}}
		evaluator := &fakeEvaluator{}
		worker, err := newWorkerWithImageDigest(fixture.settings, store, evaluator, "synthetic-lease-worker", testWorkerImageDigest())
		if err != nil {
			t.Fatal(err)
		}
		worker.heartbeatSettings.CompletionRetry.Timeout = time.Minute
		worker.heartbeatSettings.CompletionRetry.Now = clock.Now
		worker.heartbeatSettings.CompletionRetry.Wait = func(_ context.Context, delay time.Duration) error {
			releaseRow()
			if err := <-holderDone; err != nil {
				return err
			}
			clock.Advance(delay)
			return nil
		}
		if retry, err := worker.completeWithRetry(ctx, job, *job.LeaseExpiresAt, outcome); err != nil || retry || store.calls != 2 || evaluator.calls != 0 {
			t.Fatalf("rolled-back completion did not persist the same outcome exactly once: calls=%d retry=%t, %v", store.calls, retry, err)
		}
		after := readCompetitionCompletionPgSnapshot(ctx, job)
		afterLease := readCompetitionLeasePgSnapshot(ctx, job.JobId)
		var stored struct {
			State            string          `json:"state"`
			Score            json.RawMessage `json:"score_json"`
			ArtifactManifest json.RawMessage `json:"artifact_manifest_json"`
			LeaseOwner       *string         `json:"lease_owner"`
		}
		if err := json.Unmarshal(after.jobJson, &stored); err != nil {
			t.Fatal(err)
		}
		wantedScore, err := json.Marshal(outcome.Score)
		if err != nil {
			t.Fatal(err)
		}
		var actualScore, expectedScore, actualManifest, expectedManifest any
		if json.Unmarshal(stored.Score, &actualScore) != nil || json.Unmarshal(wantedScore, &expectedScore) != nil ||
			json.Unmarshal(stored.ArtifactManifest, &actualManifest) != nil || json.Unmarshal(outcome.ArtifactManifest, &expectedManifest) != nil ||
			!reflect.DeepEqual(actualScore, expectedScore) || !reflect.DeepEqual(actualManifest, expectedManifest) ||
			stored.State != "succeeded" || stored.LeaseOwner != nil || afterLease.started == nil || !afterLease.started.Equal(*job.StartedAt) || afterLease.attempts != job.AttemptCount ||
			after.baselines != 1 || after.terminalEvents != 1 {
			t.Fatal("durable completion lost its score, archive, baseline, single terminal event or original claim budget")
		}
		var slot struct {
			Worker *string    `json:"worker_id"`
			Job    *server.Id `json:"job_id"`
			Expiry *time.Time `json:"lease_expires_at"`
		}
		if err := json.Unmarshal(after.slotJson, &slot); err != nil || slot.Worker != nil || slot.Job != nil || slot.Expiry != nil {
			t.Fatalf("committed completion did not atomically release its slot: %v", err)
		}
	})
}

// Genuine automation admission binds a synthetic worker to the current fence.
func newCompetitionSupervisedCompletionPgFixture(t testing.TB, ctx context.Context) (*competitionRankingFixture, *queuedJob, *competitionLeaseRetryClock) {
	t.Helper()
	fixture := newCompetitionAutomationFixture(t)
	clock := &competitionLeaseRetryClock{now: fixture.now}
	fixture.store.now = clock.Now
	lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-completion-supervisor", time.Minute)
	if err != nil {
		t.Fatalf("acquire synthetic completion supervisor: %v", err)
	}
	fixture.settings.automationLease, fixture.settings.automationRoundId = lease, fixture.round.RoundId
	fixture.settings.WorkerLeaseSeconds = 1800
	patch, patchError := ValidateAndCanonicalizePatch(testPatch("synthetic-completion-fence"), fixture.settings.PatchPolicy)
	if patchError != nil {
		t.Fatalf("canonicalize synthetic supervised completion patch: %v", patchError)
	}
	if _, _, err := fixture.store.Enqueue(ctx, fixture.settings, fixture.round.RoundId, patch, "synthetic-completion-submitter", testApiImageDigest()); err != nil {
		t.Fatalf("enqueue synthetic supervised completion: %v", err)
	}
	job, err := fixture.store.Claim(ctx, fixture.settings, "synthetic-completion-worker", testWorkerImageDigest())
	if err != nil || job == nil {
		t.Fatalf("claim synthetic supervised completion: missing_job=%t error=%v", job == nil, err)
	}
	return fixture, job, clock
}

// A live worker lease cannot substitute for an expired supervisor fence sampled
// after the admitted job-lock boundary, even when its request once was valid.
func TestCompetitionCompletePgChecksSupervisorAfterAuthorityWait(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		fixture, job, clock := newCompetitionSupervisedCompletionPgFixture(t, ctx)
		before := readCompetitionCompletionPgSnapshot(ctx, job)
		releaseRow, holderDone := holdCompetitionLeasePgRow(t, ctx, `SELECT 1 FROM competition_job WHERE job_id=$1 FOR UPDATE`, job.JobId)
		admitted, resume := make(chan struct{}), make(chan struct{})
		var resumeOnce sync.Once
		resumeCompletion := func() { resumeOnce.Do(func() { close(resume) }) }
		t.Cleanup(resumeCompletion)
		fixture.store.beforeLeaseRowLockForTest = func(phase string) {
			if phase == "completion_job" {
				close(admitted)
				select {
				case <-resume:
				case <-ctx.Done():
					panic(ctx.Err())
				}
			}
		}
		done := make(chan error, 1)
		go func() {
			_, err := fixture.store.Complete(ctx, fixture.settings, "synthetic-completion-worker", job.JobId,
				EvaluationOutcome{Error: terminalInfrastructureError("synthetic_terminal_failure", "synthetic completion metadata")})
			done <- err
		}()
		select {
		case <-admitted:
		case <-ctx.Done():
			t.Fatal("supervised completion did not admit its authority boundary")
		}
		clock.Advance(time.Minute)
		releaseRow()
		if err := <-holderDone; err != nil {
			t.Fatal(err)
		}
		resumeCompletion()
		if err := <-done; !errors.Is(err, ErrLeaseLost) {
			t.Fatalf("completion used a supervisor fence that expired during its authority wait: %v", err)
		}
		if after := readCompetitionCompletionPgSnapshot(ctx, job); !reflect.DeepEqual(after, before) {
			t.Fatal("expired supervisor committed completion metadata or released worker authority")
		}
	})
}

// Time consumed at the completed baseline boundary must invalidate the whole
// transaction, including its provisional baseline and round-baseline event.
func TestCompetitionCompletePgRechecksAuthorityAfterBaseline(t *testing.T) {
	for _, supervisorExpiry := range []bool{false, true} {
		testEnv := server.DefaultTestEnv()
		testEnv.RerunCount = 0
		testEnv.Run(t, func(t testing.TB) {
			ctx := context.Background()
			var store PostgresStore
			var settings *Settings
			var job *queuedJob
			var clock *competitionLeaseRetryClock
			workerId := "synthetic-lease-worker"
			advance := time.Duration(0)
			if supervisorExpiry {
				fixture, claimed, ownedClock := newCompetitionSupervisedCompletionPgFixture(t, ctx)
				store, settings, job, clock = fixture.store, fixture.settings, claimed, ownedClock
				workerId, advance = "synthetic-completion-worker", time.Minute
			} else {
				fixture, claimed := newCompetitionLeasePgFixture(t, ctx, "complete-baseline-expiry")
				clock = &competitionLeaseRetryClock{now: fixture.clock}
				store, settings, job = fixture.store, fixture.settings, claimed
				store.now = clock.Now
				advance = time.Duration(settings.WorkerLeaseSeconds) * time.Second
			}
			before := readCompetitionCompletionPgSnapshot(ctx, job)
			crossedBaseline := false
			store.afterCompletionBaselineForTest = func() { crossedBaseline = true; clock.Advance(advance) }
			outcome := testSharedBaselineOutcome(t, settings, job, EvaluationOutcome{Error: submissionError("synthetic_terminal_failure", "synthetic retained completion")})
			if retry, err := store.Complete(ctx, settings, workerId, job.JobId, outcome); !errors.Is(err, ErrLeaseLost) || retry || !crossedBaseline {
				t.Fatalf("supervisor=%t: completion retained authority across baseline work: retry=%t, %v", supervisorExpiry, retry, err)
			}
			if after := readCompetitionCompletionPgSnapshot(ctx, job); !reflect.DeepEqual(after, before) {
				t.Fatal("expiry after baseline work committed a baseline, event, job transition or slot release")
			}
		})
	}
}

// A final live authority check also supplies the completion timestamp and retry
// decision, so a baseline wait cannot manufacture a backdated executable retry.
func TestCompetitionCompletePgUsesFreshCompletionTimeAfterBaseline(t *testing.T) {
	testEnv := server.DefaultTestEnv()
	testEnv.RerunCount = 0
	testEnv.Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSubmissionArchiveFixture(t, ctx, "complete-final-clock")
		fixture.settings.EvaluationPolicy.ScoreTimeoutSeconds = 30
		fixture.settings.WorkerLeaseSeconds = 600
		round := fixture.openStagingRound(t, ctx)
		if _, _, err := fixture.store.Enqueue(ctx, fixture.settings, round.RoundId, fixture.patch(t, "complete-final-clock"), "synthetic-completion-submitter", testApiImageDigest()); err != nil {
			t.Fatal(err)
		}
		job, err := fixture.store.Claim(ctx, fixture.settings, "synthetic-lease-worker", testWorkerImageDigest())
		if err != nil || job == nil {
			t.Fatalf("claim synthetic completion clock: %v", err)
		}
		clock := &competitionLeaseRetryClock{now: fixture.clock}
		fixture.store.now = clock.Now
		fixture.store.afterCompletionBaselineForTest = func() { clock.Advance(30 * time.Second) }
		outcome := testSharedBaselineOutcome(t, fixture.settings, job, EvaluationOutcome{Error: infrastructureError("synthetic_retryable_failure", "synthetic retained completion")})
		if retry, err := fixture.store.Complete(ctx, fixture.settings, "synthetic-lease-worker", job.JobId, outcome); err != nil || retry {
			t.Fatalf("baseline time granted a backdated retry: retry=%t, %v", retry, err)
		}
		var state, code string
		var completedAt, startedAt time.Time
		var attempts, retryEvents int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT state,completed_at,started_at,attempt_count,eval_error_json->>'code',
			 (SELECT count(*) FROM competition_job_event WHERE job_id=$1 AND event_type='infrastructure_retry')
			 FROM competition_job WHERE job_id=$1`, job.JobId).Scan(&state, &completedAt, &startedAt, &attempts, &code, &retryEvents))
		})
		if state != "failed" || code != "evaluation_time_budget_exhausted" || !completedAt.Equal(clock.Now()) ||
			!startedAt.Equal(*job.StartedAt) || attempts != job.AttemptCount || retryEvents != 0 {
			t.Fatal("completion used pre-baseline time, requeued work or changed the original execution budget")
		}
	})
}
