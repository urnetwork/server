// Real disposable PostgreSQL exercises original, replacement and public-job
// boundaries with every qualification and recovery guard left enabled.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// An expired original is inserted directly, never rewritten to simulate time.
func executorQualificationRecoveryPgFixture(t *testing.T, tb testing.TB) (*competitionDistributedPgFixture, CompetitionExecutorQualificationRecovery) {
	tb.Helper()
	f := newCompetitionDistributedPgFixture(t, tb, true, false)
	host := f.settings.DistributedEvaluationPolicy.Hosts[0]
	jobId := competitionExecutorQualificationJobId(f.round.RoundId, host.HostId, f.settings.DistributedEvaluationPolicy.ApprovalSha256)
	started := server.NowUtc().Add(-4 * time.Hour).Truncate(time.Microsecond)
	server.Tx(tb.Context(), func(tx server.PgTx) {
		server.Raise(f.store.requireAutomationLeaseInTx(tb.Context(), tx, f.settings, *f.settings.automationLease))
		server.RaisePgResult(tx.Exec(tb.Context(), `INSERT INTO competition_executor_qualification_run(round_id,host_id,job_id,started_at,deadline_at,api_image_digest,worker_image_digest) VALUES($1,$2,$3,$4,$5,$6,$7)`,
			f.round.RoundId, host.HostId, jobId, started, started.Add(3*time.Hour), testApiImageDigest(), testWorkerImageDigest()))
	})
	s := f.settings
	return f, CompetitionExecutorQualificationRecovery{Schema: 1, Kind: "executor-qualification-single-recovery/v1", RoundId: f.round.RoundId, HostId: host.HostId,
		OriginalJobId: jobId, ReplacementJobId: server.NewId(), BaselineSha256: f.prepared.BaselineSha256, QualificationSha256: host.QualificationSha256,
		PolicyApprovalSha256: s.DistributedEvaluationPolicy.ApprovalSha256, SourceLockSha256: s.AutomationPolicy.SourceLockSha256, EvaluatorImageDigest: s.EvaluatorImageDigest,
		ProvidersSha256: f.round.ProvidersSha256, ConfigLocalSha256: s.EvaluationPolicy.ConfigLocalSha256, VaultLocalSha256: s.EvaluationPolicy.VaultLocalSha256,
		OriginalScoreSha256: strings.Repeat("d", 64), EvidenceSha256: strings.Repeat("e", 64)}
}

// Exact operator bytes and live fence traverse the real insert guard.
func insertExecutorQualificationRecoveryPg(ctx context.Context, f *competitionDistributedPgFixture, value CompetitionExecutorQualificationRecovery, fenced bool) error {
	content, digest, err := competitionExecutorQualificationRecoveryBytes(value)
	if err != nil {
		return err
	}
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			if fenced {
				server.Raise(f.store.requireAutomationLeaseInTx(ctx, tx, f.settings, *f.settings.automationLease))
			}
			server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_executor_qualification_recovery',$1,true)`, digest))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_executor_qualification_recovery(round_id,host_id,authorization_sha256,authorization_json,authorization_canonical,job_id,api_image_digest,worker_image_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
				value.RoundId, value.HostId, digest, json.RawMessage(content), content, value.ReplacementJobId, testApiImageDigest(), testWorkerImageDigest()))
		}, server.OptReadWrite())
	})
}

func TestExecutorQualificationRecoveryPgPreservesBothClocksAndRefusesSecondReplacement(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		f, value := executorQualificationRecoveryPgFixture(t, tb)
		if err := insertExecutorQualificationRecoveryPg(tb.Context(), f, value, true); err != nil {
			tb.Fatal(err)
		}
		var started, deadline, originalStarted, originalDeadline time.Time
		server.Db(tb.Context(), func(conn server.PgConn) {
			var jobId, originalJobId server.Id
			server.Raise(conn.QueryRow(tb.Context(), `SELECT job_id,started_at,deadline_at FROM competition_executor_qualification_clock($1,$2)`, value.RoundId, value.HostId).Scan(&jobId, &started, &deadline))
			server.Raise(conn.QueryRow(tb.Context(), `SELECT job_id,started_at,deadline_at FROM competition_executor_qualification_run WHERE round_id=$1 AND host_id=$2`, value.RoundId, value.HostId).Scan(&originalJobId, &originalStarted, &originalDeadline))
			if jobId != value.ReplacementJobId || originalJobId != value.OriginalJobId || !deadline.Equal(started.Add(3*time.Hour)) || !originalDeadline.Equal(originalStarted.Add(3*time.Hour)) || !originalDeadline.Before(started) {
				tb.Fatal("original clock was rewritten or replacement was not fresh")
			}
		})
		changed := value
		changed.ReplacementJobId = server.NewId()
		if err := insertExecutorQualificationRecoveryPg(tb.Context(), f, changed, true); err == nil {
			tb.Fatal("second recovery reserved another clock")
		}
		for _, query := range []string{
			`UPDATE competition_executor_qualification_recovery SET started_at=started_at+interval '1 second' WHERE round_id=$1`,
			`DELETE FROM competition_executor_qualification_recovery WHERE round_id=$1`,
			`UPDATE competition_executor_qualification_run SET started_at=started_at+interval '1 second' WHERE round_id=$1`,
		} {
			if err := captureDatabaseError(func() {
				server.Tx(tb.Context(), func(tx server.PgTx) { server.RaisePgResult(tx.Exec(tb.Context(), query, value.RoundId)) })
			}); err == nil {
				tb.Fatal("immutable admission rewritten", query)
			}
		}
		if err := captureDatabaseError(func() {
			server.Tx(tb.Context(), func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(tb.Context(), `TRUNCATE competition_executor_qualification_recovery`))
			})
		}); err == nil {
			tb.Fatal("recovery evidence could be truncated")
		}
		// A different host retains its original positive admission path.
		f.qualifyHost(tb, f.settings.DistributedEvaluationPolicy.Hosts[1])
		server.Db(tb.Context(), func(conn server.PgConn) {
			var count, startedCustomers int
			var ready bool
			server.Raise(conn.QueryRow(tb.Context(), `SELECT count(*) FROM competition_executor_qualification_recovery WHERE round_id=$1`, value.RoundId).Scan(&count))
			server.Raise(conn.QueryRow(tb.Context(), `SELECT count(*) FROM competition_job WHERE round_id=$1 AND started_at IS NOT NULL`, value.RoundId).Scan(&startedCustomers))
			server.Raise(conn.QueryRow(tb.Context(), `SELECT competition_executor_control_hosts_ready($1)`, value.RoundId).Scan(&ready))
			if count != 1 || startedCustomers != 0 || ready {
				tb.Fatal("replacement started customers or bypassed whole-host admission")
			}
		})
	})
}

func TestExecutorQualificationRecoveryPgKeepsSyntheticJobOutOfPublicQueue(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		f, value := executorQualificationRecoveryPgFixture(t, tb)
		if err := insertExecutorQualificationRecoveryPg(tb.Context(), f, value, true); err != nil {
			tb.Fatal(err)
		}
		patch, patchError := ValidateAndCanonicalizePatch(testPatch("synthetic-recovery-collision"), f.settings.PatchPolicy)
		if patchError != nil {
			tb.Fatal(patchError)
		}
		err := captureDatabaseError(func() {
			server.Tx(tb.Context(), func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(tb.Context(), `INSERT INTO competition_job(job_id,round_id,patch_bytes,patch_sha256,cache_key,state,submitted_at,available_at,attempt_count,artifact_retain_until,api_image_digest) VALUES($1,$2,$3,$4,$5,'queued',$6,$6,0,$7,$8)`,
					value.ReplacementJobId, value.RoundId, patch.Bytes, patch.Sha256, cacheKey(value.RoundId, patch.Bytes), f.clock.Now(), f.settings.RetainUntil, testApiImageDigest()))
			})
		})
		if err == nil || !strings.Contains(err.Error(), "replacement qualification cannot become a public submission") {
			tb.Fatal("synthetic recovery entered the public queue", err)
		}
	})
}

func TestExecutorQualificationRecoveryPgRejectsStaleAndRehashedScopeBeforeClock(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		f, value := executorQualificationRecoveryPgFixture(t, tb)
		if err := insertExecutorQualificationRecoveryPg(tb.Context(), f, value, false); err == nil {
			tb.Fatal("stale operator started a recovery clock")
		}
		for _, mutate := range []func(*CompetitionExecutorQualificationRecovery){
			func(v *CompetitionExecutorQualificationRecovery) { v.BaselineSha256 = strings.Repeat("0", 64) },
			func(v *CompetitionExecutorQualificationRecovery) { v.SourceLockSha256 = strings.Repeat("0", 64) },
			func(v *CompetitionExecutorQualificationRecovery) { v.ProvidersSha256 = strings.Repeat("0", 64) },
			func(v *CompetitionExecutorQualificationRecovery) { v.ConfigLocalSha256 = strings.Repeat("0", 64) },
			func(v *CompetitionExecutorQualificationRecovery) { v.ReplacementJobId = v.OriginalJobId },
			func(v *CompetitionExecutorQualificationRecovery) { v.ReplacementJobId = f.sourceJob.JobId },
			func(v *CompetitionExecutorQualificationRecovery) { v.OriginalScoreSha256 = "" },
		} {
			changed := value
			mutate(&changed)
			if err := insertExecutorQualificationRecoveryPg(tb.Context(), f, changed, true); err == nil {
				tb.Fatal("changed authority started a clock")
			}
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(tb.Context(), `SELECT count(*) FROM competition_executor_qualification_recovery WHERE round_id=$1`, value.RoundId).Scan(&count))
			if count != 0 {
				tb.Fatal("rejected authority left a clock")
			}
		})
	})
}

func TestExecutorQualificationRecoveryPgRejectsOldReceiptAndAcceptsBoundReplacement(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		f, value := executorQualificationRecoveryPgFixture(t, tb)
		if err := insertExecutorQualificationRecoveryPg(tb.Context(), f, value, true); err != nil {
			tb.Fatal(err)
		}
		_, authority, err := competitionExecutorQualificationRecoveryBytes(value)
		if err != nil {
			tb.Fatal(err)
		}
		var started, deadline time.Time
		server.Db(tb.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(tb.Context(), `SELECT started_at,deadline_at FROM competition_executor_qualification_clock($1,$2)`, value.RoundId, value.HostId).Scan(&started, &deadline))
		})
		insert := func(jobId server.Id, recovery string) error {
			receipt := map[string]any{"schema": 1, "round_id": value.RoundId.String(), "host_id": value.HostId, "baseline_sha256": value.BaselineSha256, "source_lock_sha256": value.SourceLockSha256, "providers_sha256": value.ProvidersSha256, "evaluator_image_digest": value.EvaluatorImageDigest, "qualification_sha256": value.QualificationSha256, "policy_approval_sha256": value.PolicyApprovalSha256, "passed": true, "noop": true, "replicates": 9, "archive_manifest_sha256": strings.Repeat("6", 64), "qualification_job_id": jobId.String(), "qualification_started_at": started, "qualification_deadline_at": deadline, "created_at": started, "recovery_sha256": recovery}
			f.addCoverageToReceipt(tb, receipt)
			content, err := json.Marshal(receipt)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(content)
			return captureDatabaseError(func() {
				server.Tx(tb.Context(), func(tx server.PgTx) {
					server.Raise(f.store.requireAutomationLeaseInTx(tb.Context(), tx, f.settings, *f.settings.automationLease))
					server.RaisePgResult(tx.Exec(tb.Context(), `INSERT INTO competition_executor_equivalence(round_id,host_id,receipt_json,receipt_sha256,baseline_sha256,created_at) VALUES($1,$2,$3,$4,$5,$6)`, value.RoundId, value.HostId, json.RawMessage(content), hex.EncodeToString(digest[:]), value.BaselineSha256, started))
				})
			})
		}
		if insert(value.OriginalJobId, authority) == nil {
			tb.Fatal("old job accepted with replacement clock")
		}
		if insert(value.ReplacementJobId, "") == nil {
			tb.Fatal("replacement omitted operator authority")
		}
		if insert(value.ReplacementJobId, strings.Repeat("0", 64)) == nil {
			tb.Fatal("replacement changed operator authority")
		}
		if err := insert(value.ReplacementJobId, authority); err != nil {
			tb.Fatal("valid replacement receipt refused", err)
		}
		// Whole-host readiness must use the selected replacement clock too,
		// not require this new job id to equal the original expired job id.
		f.qualifyHost(tb, f.settings.DistributedEvaluationPolicy.Hosts[1])
		server.Db(tb.Context(), func(conn server.PgConn) {
			var ready bool
			server.Raise(conn.QueryRow(tb.Context(), `SELECT competition_executor_control_hosts_ready($1)`, f.round.RoundId).Scan(&ready))
			if !ready {
				tb.Fatal("passing replacement and original sibling did not admit the whole host set")
			}
		}, server.OptReadOnly())
	})
}
