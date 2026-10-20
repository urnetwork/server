// Disposable database regressions bind worker claims to the exact frozen
// runtime capability and keep ordinary seal polling out of panic recovery.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Creates a queued synthetic row only in the caller's private TestEnv. Neither
// admission, real artifacts nor evaluator processes are involved in this proof.
func queueCompetitionAutomationPollJob(t testing.TB, fixture *competitionRankingFixture) server.Id {
	t.Helper()
	jobId := server.NewId()
	patch := []byte("synthetic-worker-poll-patch-" + jobId.String())
	digest := sha256.Sum256(patch)
	server.Tx(context.Background(), func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(context.Background(), `INSERT INTO competition_job
		(job_id,round_id,patch_bytes,patch_sha256,cache_key,state,submitted_at,available_at,artifact_retain_until,api_image_digest)
		VALUES($1,$2,$3,$4,$4,'queued',$5,$5,$6,$7)`, jobId, fixture.round.RoundId, patch, hex.EncodeToString(digest[:]), fixture.now, fixture.settings.RetainUntil, testApiImageDigest()))
	})
	return jobId
}

// A new pinned control worker may report its actual per-job runtime identity
// without changing the original immutable source-readiness acknowledgment.
func testCompetitionAutomationClaimCapability(t *testing.T, capability string) {
	t.Helper()
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixtureWithCapability(t, true, capability)
		fixture.store.dequeueCompetitionSignal = func(context.Context, *Settings) (*server.Id, error) { return nil, nil }
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-claim-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		fixture.settings.automationLease, fixture.settings.automationRoundId = lease, fixture.round.RoundId
		jobId := queueCompetitionAutomationPollJob(t, fixture)
		actualImage := "sha256:" + strings.Repeat("e", 64)
		job, err := fixture.store.Claim(ctx, fixture.settings, "synthetic-claim-worker", actualImage)
		if err != nil || job == nil || job.JobId != jobId || job.State != "running" || job.AttemptCount != 1 || job.WorkerImageDigest != actualImage {
			t.Fatalf("%s did not claim its exact queued job: %+v %v", capability, job, err)
		}
		if competitionRoundApiCapability(&job.Round) != capability {
			t.Fatal("claim lost the immutable runtime capability")
		}
		if err := fixture.store.Heartbeat(ctx, fixture.settings, "synthetic-claim-worker", jobId); err != nil {
			t.Fatalf("claimed capability could not renew its exact lease: %v", err)
		}
		receipts, err := fixture.store.sourcePreparationReceipts(ctx, fixture.settings, 1)
		if err != nil || len(receipts) != 2 {
			t.Fatalf("readiness evidence changed: %v", err)
		}
		for _, receipt := range receipts {
			if receipt.RuntimeImageDigest != testWorkerImageDigest() {
				t.Fatal("claim rewrote an immutable readiness receipt")
			}
		}
		if err := fixture.store.HandBack(ctx, "synthetic-claim-worker", jobId, "synthetic-test-complete"); err != nil {
			t.Fatal(err)
		}
	})
}

// Historical filesystem-enabled v2 rounds retain their exact supported selector.
func TestCompetitionAutomationClaimFilesystemCapability(t *testing.T) {
	testCompetitionAutomationClaimCapability(t, model.CompetitionAutomationCapability)
}

// Blob-backed rounds must not be silently skipped by a filesystem-only constant.
func TestCompetitionAutomationClaimRuntimeBlobCapability(t *testing.T) {
	testCompetitionAutomationClaimCapability(t, model.CompetitionRuntimeBlobCapability)
}

// Supporting both transports is not permission to claim a round under another
// configured capability, scope or unsupported future protocol.
func TestCompetitionAutomationClaimRejectsDifferentCapabilityAndScope(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixtureWithCapability(t, true, model.CompetitionRuntimeBlobCapability)
		dequeues := 0
		fixture.store.dequeueCompetitionSignal = func(context.Context, *Settings) (*server.Id, error) { dequeues++; return nil, nil }
		lease, err := fixture.store.AcquireAutomationLease(ctx, fixture.settings, "synthetic-claim-supervisor", 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		fixture.settings.automationLease, fixture.settings.automationRoundId = lease, fixture.round.RoundId
		jobId := queueCompetitionAutomationPollJob(t, fixture)
		for _, capability := range []string{"", "synthetic-unknown-capability"} {
			settings := *fixture.settings
			settings.AutomationPolicy.RequiredApiCapability = capability
			if job, err := fixture.store.Claim(ctx, &settings, "synthetic-claim-worker", testWorkerImageDigest()); job != nil || !errors.Is(err, ErrUnsupportedDecisionMode) || dequeues != 0 {
				t.Fatalf("unsupported capability reached queue or database authority: job=%+v error=%v", job, err)
			}
		}
		settings := *fixture.settings
		settings.AutomationPolicy.RequiredApiCapability = model.CompetitionAutomationCapability
		if job, err := fixture.store.Claim(ctx, &settings, "synthetic-claim-worker", testWorkerImageDigest()); err != nil || job != nil {
			t.Fatalf("filesystem configuration claimed a blob-required job: %+v %v", job, err)
		}
		for _, change := range []func(*Settings){
			func(value *Settings) { value.AutomationPolicy.SeasonId = "synthetic-foreign-season" },
			func(value *Settings) { value.CompetitionId = "synthetic-foreign-competition" },
			func(value *Settings) { value.AutomationPolicy.Environment = "synthetic-foreign-environment" },
		} {
			settings := *fixture.settings
			change(&settings)
			if job, err := fixture.store.Claim(ctx, &settings, "synthetic-claim-worker", testWorkerImageDigest()); job != nil || !errors.Is(err, ErrLeaseLost) {
				t.Fatalf("foreign scope gained claim authority: %+v %v", job, err)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var state string
			var attempts int
			server.Raise(conn.QueryRow(ctx, `SELECT state,attempt_count FROM competition_job WHERE job_id=$1`, jobId).Scan(&state, &attempts))
			if state != "queued" || attempts != 0 {
				t.Fatalf("refused selector mutated the job: %s attempts=%d", state, attempts)
			}
		})
	})
}

// Observes actual transaction boundaries without intercepting global logging.
// An expected polling result must commit its read-only transaction, not unwind
// through the panic/rollback path that emits an unexpected-error stack.
type competitionSealPollTracer struct {
	stateLock sync.Mutex
	commits   int
	rollbacks int
}

// Only transaction verbs are retained; statements and arguments are discarded.
func (self *competitionSealPollTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch strings.ToLower(strings.TrimSpace(data.SQL)) {
	case "commit":
		self.commits++
	case "rollback":
		self.rollbacks++
	}
	return ctx
}

// Completion adds no state; the transaction verb already identifies its path.
func (self *competitionSealPollTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

// Repeated open and draining states preserve their sentinel and all durable
// evidence without the panic path responsible for unbounded stderr stacks.
func TestCompetitionAutomationSealPollingIsExpectedControlFlow(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newCompetitionAutomationFixtureWithMode(t, true)
		jobId := queueCompetitionAutomationPollJob(t, fixture)
		trace := &competitionSealPollTracer{}
		scope, err := server.NewTestPgQueryScope(ctx, trace)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := scope.Close(); err != nil {
				t.Error(err)
			}
		}()
		for _, now := range []time.Time{fixture.now, fixture.round.ClosesAt} {
			fixture.now = now
			for range 3 {
				result, err := fixture.store.SealAndElect(ctx, fixture.settings, fixture.round.RoundId)
				if result != nil || !errors.Is(err, ErrReviewNotReady) {
					t.Fatalf("open or draining round changed its refusal: %+v %v", result, err)
				}
			}
		}
		trace.stateLock.Lock()
		commits, rollbacks := trace.commits, trace.rollbacks
		trace.stateLock.Unlock()
		if commits != 6 || rollbacks != 0 {
			t.Fatalf("expected polling used panic recovery: commits=%d rollbacks=%d", commits, rollbacks)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var state string
			var seals int
			server.Raise(conn.QueryRow(ctx, `SELECT state FROM competition_job WHERE job_id=$1`, jobId).Scan(&state))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM competition_evaluation_seal WHERE round_id=$1`, fixture.round.RoundId).Scan(&seals))
			if state != "queued" || seals != 0 {
				t.Fatalf("not-ready polling changed durable inventory: state=%s seals=%d", state, seals)
			}
		})
	})
}
