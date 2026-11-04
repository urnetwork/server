// Disposable database regressions exercise the real future job/slot guards.
// Synthetic setup isolates source promotion, already covered by its own suite;
// no execution, baseline, qualification or singleton guard is bypassed.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Public keys are derived from visibly synthetic seeds, never a host identity.
func competitionDistributedSlotTestPolicy(t testing.TB) *CompetitionDistributedEvaluationPolicy {
	t.Helper()
	policy := &CompetitionDistributedEvaluationPolicy{Schema: 1, FromEpoch: 2, CoordinatorId: "synthetic-coordinator",
		EquivalencePolicy: CompetitionHostEquivalencePolicy{ScreenKind: "operational-admission/v1", MaximumRawScoreRelativeDifference: 0.05, ConfidenceZ: 1.96}}
	for index, hostId := range []string{"synthetic-executor-a", "synthetic-executor-b"} {
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(index + 1)}, ed25519.SeedSize))
		transport := "ssh"
		if index == 0 {
			transport = "local"
		}
		policy.Hosts = append(policy.Hosts, CompetitionExecutorHostPolicy{HostId: hostId, SlotId: index + 1, Transport: transport,
			QualificationSha256: strings.Repeat(fmt.Sprintf("%x", index+1), 64), AgentPublicKeyBase64: base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))})
	}
	content, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	policy.ApprovalSha256 = hex.EncodeToString(digest[:])
	if err := validateCompetitionDistributedPolicy(policy); err != nil {
		t.Fatal(err)
	}
	return policy
}

// The owned clock is safe across exact query barriers and concurrent completion.
type competitionDistributedPgFixture struct {
	settings    *Settings
	store       PostgresStore
	round       *roundRecord
	clock       *competitionLeaseRetryClock
	sourceJob   *queuedJob
	preparation *roundBaselinePreparation
	prepared    *preparedRoundBaseline
}

// Source-installation triggers alone are temporarily disabled while inserting
// a synthetic future round. The complete new distributed policy, every job and
// slot guard, and all baseline/qualification guards stay enabled throughout.
func newCompetitionDistributedPgFixture(outer *testing.T, t testing.TB, publish, qualify bool) *competitionDistributedPgFixture {
	t.Helper()
	ctx := context.Background()
	settings := automationApiSettings()
	settings.CompetitionId = "synthetic-distributed-" + server.NewId().String()
	settings.EvaluationPolicy.HardwareId = "synthetic-evaluator"
	settings.EvaluationPolicy.Replicates = 9
	settings.ArtifactRoot = t.TempDir()
	settings.FutureEpochPolicy = g2FuturePolicyForTest(outer)
	settings.FutureEpochPolicy.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(t)
	var err error
	settings, err = settings.ForCompetitionEpoch(2)
	if err != nil {
		t.Fatal(err)
	}
	clock := &competitionLeaseRetryClock{now: server.NowUtc().Truncate(time.Microsecond)}
	settings.SeasonEndsAt = clock.Now().Add(60 * 24 * time.Hour)
	settings.RetainUntil = settings.SeasonEndsAt.Add(30 * 24 * time.Hour)
	settings.roundBaselinePreparation = &roundBaselinePreparationProfile{Settings: defaultRoundBaselinePreparationSettings()}
	settings.automationTargetRoundId = server.NewId()
	fixture := &competitionDistributedPgFixture{settings: settings, clock: clock, store: PostgresStore{now: clock.Now}}
	lease, err := fixture.store.AcquireAutomationLease(ctx, settings, "synthetic-coordinator", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	settings.automationLease = lease
	fixture.round, err = fixture.store.prepareRound(ctx, settings, GenerateRoundArgs{OpensAt: clock.Now(), ClosesAt: clock.Now().Add(7 * 24 * time.Hour), RevealAt: clock.Now().Add(7 * 24 * time.Hour)}, false)
	if err != nil {
		t.Fatal(err)
	}
	fixture.round.Epoch = 2
	settings.automationRoundId = fixture.round.RoundId
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE competition_round DISABLE TRIGGER competition_v2_round_source`))
		server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE competition_round DISABLE TRIGGER competition_v2_round_reserved_identity`))
		round := fixture.round
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_round(round_id,competition_id,epoch_number,staging,workload_commitment,seed_nonce,seed_ciphertext,providers_sha256,providers_path,policy_json,opens_at,closes_at,reveal_at,created_at,canceled)
		 VALUES($1,$2,2,false,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$11,$12,false)`, round.RoundId, round.CompetitionId, round.WorkloadCommitment, round.SeedNonce,
			round.SeedCiphertext, round.ProvidersSha256, round.ProvidersPath, string(round.PolicyJson), round.OpensAt, round.ClosesAt, round.RevealAt, round.CreatedAt))
		server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE competition_round ENABLE TRIGGER competition_v2_round_source`))
		server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE competition_round ENABLE TRIGGER competition_v2_round_reserved_identity`))
	})
	fixture.sourceJob = fixture.addJob(t, server.Id{15: 3}, clock.Now(), "source")
	fixture.preparation, err = fixture.store.ReserveRoundBaseline(ctx, settings, "synthetic-coordinator", testWorkerImageDigest())
	if err != nil || fixture.preparation == nil {
		t.Fatalf("reserve future shared baseline: %v", err)
	}
	fixture.prepared = preparedRoundBaselineTest(t, settings, fixture.preparation)
	if publish {
		if err := fixture.store.PublishPreparedRoundBaseline(ctx, settings, fixture.preparation, fixture.prepared); err != nil {
			t.Fatal(err)
		}
	}
	if qualify {
		for _, host := range settings.DistributedEvaluationPolicy.Hosts {
			fixture.qualifyHost(t, host)
		}
	}
	return fixture
}

// Explicit ids/timestamps test admission order independently of commit order.
func (self *competitionDistributedPgFixture) addJob(t testing.TB, jobId server.Id, submittedAt time.Time, label string) *queuedJob {
	t.Helper()
	patch, patchError := ValidateAndCanonicalizePatch(testPatch("synthetic-"+label), self.settings.PatchPolicy)
	if patchError != nil {
		t.Fatal(patchError)
	}
	server.Tx(context.Background(), func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(context.Background(), `INSERT INTO competition_job(job_id,round_id,patch_bytes,patch_sha256,cache_key,state,submitted_at,available_at,attempt_count,artifact_retain_until,api_image_digest)
		 VALUES($1,$2,$3,$4,$5,'queued',$6,$6,0,$7,$8)`, jobId, self.round.RoundId, patch.Bytes, patch.Sha256, cacheKey(self.round.RoundId, patch.Bytes), submittedAt, self.settings.RetainUntil, testApiImageDigest()))
	})
	job, err := self.store.GetJob(context.Background(), self.settings, jobId, &Principal{Id: "synthetic-operator", Role: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// These are synthetic evidence fixtures, not real host qualification receipts.
// The native identity/append-only guards still verify every frozen linkage.
func (self *competitionDistributedPgFixture) qualifyHost(t testing.TB, host CompetitionExecutorHostPolicy) {
	t.Helper()
	ctx := context.Background()
	now := self.clock.Now()
	jobId, deadline := server.NewId(), now.Add(3*time.Hour)
	receipt := map[string]any{"schema": 1, "round_id": self.round.RoundId.String(), "host_id": host.HostId,
		"baseline_sha256": self.prepared.BaselineSha256, "source_lock_sha256": self.settings.AutomationPolicy.SourceLockSha256,
		"providers_sha256": self.round.ProvidersSha256, "evaluator_image_digest": self.settings.EvaluatorImageDigest,
		"qualification_sha256": host.QualificationSha256, "policy_approval_sha256": self.settings.DistributedEvaluationPolicy.ApprovalSha256,
		"passed": true, "noop": true, "archive_manifest_sha256": strings.Repeat("6", 64),
		"qualification_job_id": jobId.String(), "qualification_started_at": now, "qualification_deadline_at": deadline, "created_at": now}
	content, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(self.store.requireAutomationLeaseInTx(ctx, tx, self.settings, *self.settings.automationLease))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_executor_qualification_run(round_id,host_id,job_id,started_at,deadline_at,api_image_digest,worker_image_digest) VALUES($1,$2,$3,$4,$5,$6,$7)`, self.round.RoundId, host.HostId, jobId, now, deadline, testApiImageDigest(), testWorkerImageDigest()))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_executor_equivalence(round_id,host_id,receipt_json,receipt_sha256,baseline_sha256,created_at) VALUES($1,$2,$3::jsonb,$4,$5,$6)`,
			self.round.RoundId, host.HostId, string(content), hex.EncodeToString(digest[:]), self.prepared.BaselineSha256, now))
	})
}

// Each independent worker receives only its approved physical slot selector.
func (self *competitionDistributedPgFixture) selected(index int) *Settings {
	copy := *self.settings
	host := copy.DistributedEvaluationPolicy.Hosts[index]
	copy.executorHostId, copy.executorSlotId = host.HostId, host.SlotId
	return &copy
}

// A cold future control is published without starting or consuming its source.
func TestCompetitionDistributedBaselinePublicationDoesNotStartCandidate(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, false, false)
		if err := fixture.store.PublishPreparedRoundBaseline(tb.Context(), fixture.settings, fixture.preparation, fixture.prepared); err != nil {
			tb.Fatal(err)
		}
		job, err := fixture.store.GetJob(tb.Context(), fixture.settings, fixture.sourceJob.JobId, &Principal{Id: "synthetic-operator", Role: "operator"})
		if err != nil || job.State != "queued" || job.StartedAt != nil || job.AttemptCount != 0 || job.ExecutionDeadline != nil {
			tb.Fatalf("control publication started candidate: %+v, %v", job, err)
		}
		if claimed, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-worker-a", testWorkerImageDigest()); err != nil || claimed != nil {
			tb.Fatalf("unqualified host set admitted work: %+v, %v", claimed, err)
		}
		fixture.qualifyHost(tb, fixture.settings.DistributedEvaluationPolicy.Hosts[0])
		if claimed, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-worker-a", testWorkerImageDigest()); err != nil || claimed != nil {
			tb.Fatalf("one qualified host bypassed whole-set admission: %+v, %v", claimed, err)
		}
	})
}

// The oldest receipt timestamp, then job id, determines dispatch on both hosts.
func TestCompetitionDistributedClaimsUseOriginalFifoAcrossHosts(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		middle := fixture.addJob(tb, server.Id{15: 2}, fixture.clock.Now(), "middle")
		oldest := fixture.addJob(tb, server.Id{15: 1}, fixture.clock.Now(), "oldest")
		first, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-worker-a", testWorkerImageDigest())
		if err != nil || first == nil || first.JobId != oldest.JobId {
			tb.Fatalf("first FIFO claim=%+v, %v", first, err)
		}
		second, err := fixture.store.Claim(tb.Context(), fixture.selected(1), "synthetic-worker-b", testWorkerImageDigest())
		if err != nil || second == nil || second.JobId != middle.JobId || first.SlotId == second.SlotId {
			tb.Fatalf("second FIFO claim=%+v, %v", second, err)
		}
		if first.AttemptCount != 1 || second.AttemptCount != 1 || first.ExecutionDeadline == nil || !first.ExecutionDeadline.Equal(first.StartedAt.Add(3*time.Hour)) {
			tb.Fatal("dispatch lost its absolute full first-claim budget")
		}
	})
}

// Reconciliation changes only custody; old callbacks cannot borrow a reused id.
func TestCompetitionDistributedRecoveryRetainsAttemptDeadlineAndFencesOldCallbacks(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		settings := fixture.selected(0)
		first, err := fixture.store.Claim(tb.Context(), settings, "synthetic-worker-a", testWorkerImageDigest())
		if err != nil || first == nil {
			tb.Fatal(err)
		}
		oldCtx := withCompetitionJobAuthority(tb.Context(), first)
		fixture.clock.Advance(2 * time.Minute)
		recovered, err := fixture.store.Claim(tb.Context(), settings, "synthetic-worker-a", "sha256:"+strings.Repeat("8", 64))
		if err != nil || recovered == nil || recovered.JobId != first.JobId || recovered.AttemptCount != first.AttemptCount ||
			!recovered.StartedAt.Equal(*first.StartedAt) || !recovered.ExecutionDeadline.Equal(*first.ExecutionDeadline) || recovered.SlotFencingToken <= first.SlotFencingToken || recovered.WorkerImageDigest != first.WorkerImageDigest {
			tb.Fatalf("custody recovery created a fresh execution: %+v, %v", recovered, err)
		}
		if err := fixture.store.Heartbeat(oldCtx, settings, "synthetic-worker-a", first.JobId); !errors.Is(err, ErrLeaseLost) {
			tb.Fatalf("stale callback renewed recovered custody: %v", err)
		}
		if err := fixture.store.HandBack(oldCtx, "synthetic-worker-a", first.JobId, "synthetic_stale_cleanup"); !errors.Is(err, ErrLeaseLost) {
			tb.Fatalf("stale callback withdrew recovered custody: %v", err)
		}
		outcome := EvaluationOutcome{Error: submissionError("candidate_build_failed", "synthetic candidate failure"), distributedArchiveVerified: true}
		if _, err := fixture.store.Complete(oldCtx, settings, "synthetic-worker-a", first.JobId, outcome); !errors.Is(err, ErrLeaseLost) {
			tb.Fatalf("stale callback completed recovered custody: %v", err)
		}
		if err := fixture.store.Heartbeat(withCompetitionJobAuthority(tb.Context(), recovered), settings, "synthetic-worker-a", recovered.JobId); err != nil {
			tb.Fatal(err)
		}
	})
}

// Concurrent completion releases only its own host and requires retained proof.
func TestCompetitionDistributedConcurrentCompletionRequiresCoordinatorArtifacts(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		fixture.addJob(tb, server.Id{15: 4}, fixture.clock.Now(), "second")
		jobs := make([]*queuedJob, 2)
		for index := range 2 {
			var err error
			jobs[index], err = fixture.store.Claim(tb.Context(), fixture.selected(index), fmt.Sprintf("synthetic-worker-%d", index), testWorkerImageDigest())
			if err != nil || jobs[index] == nil {
				tb.Fatal(err)
			}
		}
		unverified := EvaluationOutcome{Error: submissionError("candidate_build_failed", "synthetic failure")}
		if _, err := fixture.store.Complete(withCompetitionJobAuthority(tb.Context(), jobs[0]), fixture.selected(0), "synthetic-worker-0", jobs[0].JobId, unverified); err == nil {
			tb.Fatal("unretained remote outcome became terminal")
		}
		start, done := make(chan struct{}), make(chan error, 2)
		for index := range 2 {
			go func() {
				<-start
				outcome := EvaluationOutcome{Error: submissionError("candidate_build_failed", "synthetic retained failure"), distributedArchiveVerified: true}
				_, err := fixture.store.Complete(withCompetitionJobAuthority(tb.Context(), jobs[index]), fixture.selected(index), fmt.Sprintf("synthetic-worker-%d", index), jobs[index].JobId, outcome)
				done <- err
			}()
		}
		close(start)
		for range 2 {
			if err := <-done; err != nil {
				tb.Fatal(err)
			}
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var running, occupied int
			server.Raise(conn.QueryRow(tb.Context(), `SELECT (SELECT count(*) FROM competition_job WHERE round_id=$1 AND state='running'),
			 (SELECT count(*) FROM competition_execution_slot WHERE round_id=$1 AND job_id IS NOT NULL)`, fixture.round.RoundId).Scan(&running, &occupied))
			if running != 0 || occupied != 0 {
				tb.Fatal("concurrent completion retained or released the wrong authority")
			}
		})
	})
}

// Closing observes the entire round inventory, not whichever host finished
// first. Expired hosts retain the same attempt until terminal reconciliation.
func TestCompetitionDistributedSealWaitsForEveryHostCompletion(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		fixture.addJob(tb, server.Id{15: 4}, fixture.clock.Now(), "second")
		for index := range 2 {
			job, err := fixture.store.Claim(tb.Context(), fixture.selected(index), fmt.Sprintf("synthetic-worker-%d", index), testWorkerImageDigest())
			if err != nil || job == nil {
				tb.Fatal(err)
			}
		}
		fixture.clock.Advance(fixture.round.ClosesAt.Add(time.Second).Sub(fixture.clock.Now()))
		lease, err := fixture.store.AcquireAutomationLease(tb.Context(), fixture.settings, "synthetic-successor-coordinator", 5*time.Minute)
		if err != nil {
			tb.Fatal(err)
		}
		fixture.settings.automationLease = lease
		for index := range 2 {
			if _, err := fixture.store.SealAndElect(tb.Context(), fixture.settings, fixture.round.RoundId); !errors.Is(err, ErrReviewNotReady) {
				tb.Fatalf("round sealed before host %d was reconciled: %v", index, err)
			}
			settings := fixture.selected(index)
			settings.executorReconcileExpiredOnly = true
			job, err := fixture.store.Claim(tb.Context(), settings, fmt.Sprintf("synthetic-recovered-worker-%d", index), testWorkerImageDigest())
			if err != nil || job == nil || job.AttemptCount != 1 {
				tb.Fatalf("offline host reconciliation changed attempt: %+v, %v", job, err)
			}
			outcome := EvaluationOutcome{Error: terminalInfrastructureError("evaluation_time_budget_exhausted", "synthetic original deadline exhausted"), distributedArchiveVerified: true}
			if _, err := fixture.store.Complete(withCompetitionJobAuthority(tb.Context(), job), settings, fmt.Sprintf("synthetic-recovered-worker-%d", index), job.JobId, outcome); err != nil {
				tb.Fatal(err)
			}
		}
		if result, err := fixture.store.SealAndElect(tb.Context(), fixture.settings, fixture.round.RoundId); err != nil || result == nil || result.Seal.RoundId != fixture.round.RoundId {
			tb.Fatalf("fully reconciled round did not seal: %+v, %v", result, err)
		}
	})
}

// New code waits for the legacy owner; an actual legacy Claim SQL transaction
// is independently rolled back if distributed work already occupies a host.
func TestCompetitionDistributedAndLegacySlotsExcludeEachOther(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		legacy, legacyJob := newCompetitionLeasePgFixture(tb, tb.Context(), "distributed-exclusion")
		if job, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-worker-a", testWorkerImageDigest()); err != nil || job != nil {
			tb.Fatalf("future host overlapped legacy owner: %+v, %v", job, err)
		}
		if _, err := legacy.store.Complete(tb.Context(), legacy.settings, "synthetic-lease-worker", legacyJob.JobId,
			EvaluationOutcome{Error: terminalInfrastructureError("evaluation_time_budget_exhausted", "synthetic legacy completion")}); err != nil {
			tb.Fatal(err)
		}
		if job, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-worker-a", testWorkerImageDigest()); err != nil || job == nil {
			tb.Fatalf("released legacy owner prevented future dispatch: %+v, %v", job, err)
		}
		if _, _, err := legacy.store.Enqueue(tb.Context(), legacy.settings, legacyJob.RoundId, legacy.patch(tb, "synthetic-second-legacy"), "synthetic-miner", testApiImageDigest()); err != nil {
			tb.Fatal(err)
		}
		if job, err := legacy.store.Claim(tb.Context(), legacy.settings, "synthetic-lease-worker", testWorkerImageDigest()); err == nil || job != nil {
			tb.Fatalf("legacy Claim bypassed distributed exclusion: %+v, %v", job, err)
		}
		if err := reserveCompetitionQualificationSlot(tb.Context(), "synthetic-qualification", server.NowUtc().Add(time.Minute)); !errors.Is(err, ErrEvaluationRunning) {
			tb.Fatalf("qualification probe overlapped a running distributed candidate: %v", err)
		}
	})
}

// An expired job-less qualification lease is no longer physical authority;
// its healthy successor still receives a complete first-claim budget.
func TestCompetitionDistributedClearsOnlyExpiredIdleLegacyReservation(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		server.Tx(tb.Context(), func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(tb.Context(), `UPDATE competition_worker_slot SET worker_id='synthetic-qualification',job_id=NULL,lease_expires_at=$1 WHERE slot_id=1`, fixture.clock.Now().Add(time.Minute)))
		})
		if job, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-worker-a", testWorkerImageDigest()); err != nil || job != nil {
			tb.Fatalf("active qualification reservation was cleared: %+v, %v", job, err)
		}
		fixture.clock.Advance(2 * time.Minute)
		job, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-worker-a", testWorkerImageDigest())
		if err != nil || job == nil || !job.StartedAt.Equal(fixture.clock.Now()) || !job.ExecutionDeadline.Equal(fixture.clock.Now().Add(3*time.Hour)) {
			tb.Fatalf("expired idle reservation stranded work or shortened its budget: %+v, %v", job, err)
		}
	})
}

// A cold baseline's expired reservation is not proof of completed controls.
// It must remain held for explicit evidenced recovery, never be resampled.
func TestCompetitionDistributedDoesNotClearExpiredUnpublishedPreparation(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, false, false)
		fixture.clock.Advance(4 * time.Hour)
		lease, err := fixture.store.AcquireAutomationLease(tb.Context(), fixture.settings, "synthetic-successor-coordinator", 5*time.Minute)
		if err != nil {
			tb.Fatal(err)
		}
		fixture.settings.automationLease = lease
		if job, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-worker-a", testWorkerImageDigest()); err != nil || job != nil {
			tb.Fatalf("unpublished controls were abandoned for a fresh execution: %+v, %v", job, err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var owner *string
			server.Raise(conn.QueryRow(tb.Context(), `SELECT worker_id FROM competition_worker_slot WHERE slot_id=1`).Scan(&owner))
			if owner == nil || *owner != fixture.preparation.Owner {
				tb.Fatal("expired preparation lost its permanent no-resampling reservation")
			}
		})
	})
}

// The database independently refuses outcome publication without coordinator
// artifact authority, even if an internal program bypasses Store.Complete.
func TestCompetitionDistributedDatabaseRejectsUnretainedOutcome(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		settings := fixture.selected(0)
		job, err := fixture.store.Claim(tb.Context(), settings, "synthetic-worker-a", testWorkerImageDigest())
		if err != nil || job == nil {
			tb.Fatal(err)
		}
		mutationErr := captureDatabaseError(func() {
			server.Tx(tb.Context(), func(tx server.PgTx) {
				server.Raise(fixture.store.requireAutomationLeaseInTx(tb.Context(), tx, settings, *settings.automationLease))
				server.Raise(setCompetitionExecutorAuthorityInTx(tb.Context(), tx, job.ExecutorHostId, job.SlotId, job.SlotFencingToken))
				server.RaisePgResult(tx.Exec(tb.Context(), `SET LOCAL urnetwork.competition_v2_worker='on'`))
				server.RaisePgResult(tx.Exec(tb.Context(), `UPDATE competition_job SET state='failed',completed_at=$2,lease_owner=NULL,lease_expires_at=NULL,
				 eval_error_json='{"kind":"submission","code":"candidate_build_failed","message":"synthetic unretained failure"}'::jsonb WHERE job_id=$1`, job.JobId, fixture.clock.Now()))
			})
		})
		if mutationErr == nil || !strings.Contains(mutationErr.Error(), "retained artifacts") {
			tb.Fatalf("native guard accepted unretained outcome: %v", mutationErr)
		}
	})
}

// A qualification run and its passing evidence are each append-only; no new
// coordinator can reset the clock or relabel the same host's measured result.
func TestCompetitionDistributedQualificationClockAndEvidenceAreImmutable(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		for _, statement := range []string{
			`UPDATE competition_executor_qualification_run SET deadline_at=deadline_at+interval '1 second' WHERE round_id=$1`,
			`DELETE FROM competition_executor_qualification_run WHERE round_id=$1`,
			`UPDATE competition_executor_equivalence SET receipt_json=jsonb_set(receipt_json,'{passed}','false') WHERE round_id=$1`,
			`DELETE FROM competition_executor_equivalence WHERE round_id=$1`,
		} {
			if err := captureDatabaseError(func() {
				server.Tx(tb.Context(), func(tx server.PgTx) { server.RaisePgResult(tx.Exec(tb.Context(), statement, fixture.round.RoundId)) })
			}); err == nil {
				tb.Fatal("qualification owner rewrote immutable run or evidence")
			}
		}
	})
}

// The retained singleton heartbeat is no longer the active executor heartbeat.
// Existing aggregate dashboards must not report a stale healthy future round.
func TestCompetitionDistributedMetricsKeepHostAndAggregateHeartbeatsTruthful(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		for index, host := range fixture.settings.DistributedEvaluationPolicy.Hosts {
			if index > 0 {
				fixture.clock.Advance(10 * time.Second)
			}
			if err := fixture.store.RegisterHost(tb.Context(), fixture.selected(index), HostSelfCheck{HostId: host.HostId}); err != nil {
				tb.Fatal(err)
			}
		}
		fixture.clock.Advance(10 * time.Second)
		if err := refreshCompetitionDistributedMetrics(tb.Context(), fixture.settings, fixture.clock.Now()); err != nil {
			tb.Fatal(err)
		}
		hosts := fixture.settings.DistributedEvaluationPolicy.Hosts
		if testutil.ToFloat64(competitionExecutorHeartbeatAge.WithLabelValues(hosts[0].HostId)) != 20 ||
			testutil.ToFloat64(competitionExecutorHeartbeatAge.WithLabelValues(hosts[1].HostId)) != 10 ||
			testutil.ToFloat64(competitionWorkerHeartbeatAge) != 20 || testutil.ToFloat64(competitionExecutorQualified.WithLabelValues(hosts[0].HostId)) != 1 {
			tb.Fatal("future heartbeat metrics followed the obsolete singleton instead of individual approved executors")
		}
	})
}

// A real held job row delays the first claim; its healthy three hours begin
// after that wait, not when the coordinator first tried to take the slot.
func TestCompetitionDistributedClaimStartsClockAfterJobLockWait(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		ctx, cancel := context.WithTimeout(tb.Context(), 30*time.Second)
		defer cancel()
		release, holderDone := holdCompetitionLeasePgRow(tb, ctx, `SELECT 1 FROM competition_job WHERE job_id=$1 FOR UPDATE`, fixture.sourceJob.JobId)
		admitted, resume := make(chan struct{}), make(chan struct{})
		var once sync.Once
		fixture.store.beforeLeaseRowLockForTest = func(phase string) {
			if phase == "distributed_job" {
				once.Do(func() { close(admitted); <-resume })
			}
		}
		type result struct {
			job *queuedJob
			err error
		}
		done := make(chan result, 1)
		go func() {
			job, err := fixture.store.Claim(ctx, fixture.selected(0), "synthetic-worker-a", testWorkerImageDigest())
			done <- result{job: job, err: err}
		}()
		<-admitted
		fixture.clock.Advance(2 * time.Minute)
		release()
		close(resume)
		if err := <-holderDone; err != nil {
			tb.Fatal(err)
		}
		claimed := <-done
		if claimed.err != nil || claimed.job == nil || !claimed.job.StartedAt.Equal(fixture.clock.Now()) || !claimed.job.ExecutionDeadline.Equal(fixture.clock.Now().Add(3*time.Hour)) {
			tb.Fatalf("healthy claim lost budget during admission: %+v, %v", claimed.job, claimed.err)
		}
	})
}

// The same wait cannot revive a coordinator whose parent fence expired.
func TestCompetitionDistributedClaimRechecksParentAfterJobLockWait(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, true)
		admitted, resume := make(chan struct{}), make(chan struct{})
		var once sync.Once
		fixture.store.beforeLeaseRowLockForTest = func(phase string) {
			if phase == "distributed_job" {
				once.Do(func() { close(admitted); <-resume })
			}
		}
		done := make(chan error, 1)
		go func() {
			_, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-worker-a", testWorkerImageDigest())
			done <- err
		}()
		<-admitted
		fixture.clock.Advance(6 * time.Minute)
		close(resume)
		if err := <-done; !errors.Is(err, ErrLeaseLost) {
			tb.Fatalf("expired parent fence started an execution: %v", err)
		}
	})
}

// A feature-inactive migration still admits the deployed singleton's real
// claim, renewal, hand-back, reclaimed-attempt and terminal SQL lifecycle.
func TestCompetitionDistributedMigrationPreservesLegacyWorkerLifecycle(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture, first := newCompetitionLeasePgFixture(tb, tb.Context(), "distributed-legacy-compatibility")
		originalStart := *first.StartedAt
		if err := fixture.store.Heartbeat(tb.Context(), fixture.settings, "synthetic-lease-worker", first.JobId); err != nil {
			tb.Fatal(err)
		}
		if err := fixture.store.HandBack(tb.Context(), "synthetic-lease-worker", first.JobId, "synthetic_restart"); err != nil {
			tb.Fatal(err)
		}
		second, err := fixture.store.Claim(tb.Context(), fixture.settings, "synthetic-lease-worker", testWorkerImageDigest())
		if err != nil || second == nil || second.AttemptCount != first.AttemptCount+1 || !second.StartedAt.Equal(originalStart) || second.SlotFencingToken != 0 {
			tb.Fatalf("new schema changed legacy recovery: %+v, %v", second, err)
		}
		if _, err := fixture.store.Complete(tb.Context(), fixture.settings, "synthetic-lease-worker", second.JobId,
			EvaluationOutcome{Error: terminalInfrastructureError("evaluation_time_budget_exhausted", "synthetic terminal legacy outcome")}); err != nil {
			tb.Fatal(err)
		}
	})
}

// The typed fence is a value snapshot, including after detached cancellation.
func TestCompetitionDistributedContextCapturesPhysicalAuthority(t *testing.T) {
	settings := &Settings{automationLease: &model.CompetitionSupervisorLease{OwnerId: "synthetic-coordinator", FencingToken: 11}}
	job := &queuedJob{ScoreJobResult: ScoreJobResult{JobId: server.NewId()}, ExecutorHostId: "synthetic-executor", SlotId: 2, SlotFencingToken: 7, executorSettings: settings}
	ctx := withCompetitionJobAuthority(t.Context(), job)
	job.ExecutorHostId, job.SlotId, job.SlotFencingToken = "synthetic-other-executor", 3, 8
	settings.automationLease.FencingToken = 12
	authority := context.WithoutCancel(ctx).Value(competitionJobAuthorityKey{}).(competitionJobAuthority)
	table, slotId := competitionExecutionSlotForContext(ctx)
	if authority.ExecutorHostId != "synthetic-executor" || authority.SlotId != 2 || authority.SlotFencingToken != 7 || table != "competition_execution_slot" || slotId != 2 ||
		authority.ExecutorSettings.automationLease.FencingToken != 11 {
		t.Fatal("detached cleanup borrowed a later physical owner")
	}
	if competitionExecutorSlotId(nil) != 1 || competitionExecutionSlotTable(nil) != "competition_worker_slot" {
		t.Fatal("unconfigured callers lost backwards-compatible singleton authority")
	}
}

// The minimum-work policy is compulsory, not a configurable scoring shortcut.
func TestCompetitionDistributedSlotPolicyRequiresMinimumWorkRound(t *testing.T) {
	settings := automationApiSettings()
	settings.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(t)
	content, err := policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRoundPolicySnapshot(content); err == nil {
		t.Fatal("distributed execution accepted a legacy upper-volume-bound scorer")
	}
	settings.EvaluationPolicy.G2VolumePolicy = model.CompetitionG2VolumePolicyMinimumWork
	content, err = policySnapshot(settings)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRoundPolicySnapshot(content); err != nil {
		t.Fatal(err)
	}
}
