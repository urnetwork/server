// Real isolated PostgreSQL tests retain every new amendment/clock/receipt guard.
package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Only source-installation fixtures use the established fixture's narrow setup;
// operational replacement always traverses the real insert guard and lease.
func executorControlPgValue(fixture *competitionDistributedPgFixture) CompetitionExecutorControlAmendment {
	settings := fixture.settings
	value := CompetitionExecutorControlAmendment{Schema: 1, Kind: "executor-control-correctness/v1", RoundId: fixture.round.RoundId,
		Environment: settings.AutomationPolicy.Environment, CompetitionId: settings.CompetitionId, SeasonId: settings.AutomationPolicy.SeasonId,
		PolicyApprovalSha256: settings.DistributedEvaluationPolicy.ApprovalSha256, SourceLockSha256: settings.AutomationPolicy.SourceLockSha256,
		EvaluatorImageDigest: settings.EvaluatorImageDigest, ConfigLocalSha256: settings.EvaluationPolicy.ConfigLocalSha256, VaultLocalSha256: settings.EvaluationPolicy.VaultLocalSha256,
		EvidenceSha256: strings.Repeat("b", 64)}
	for _, host := range settings.DistributedEvaluationPolicy.Hosts {
		value.Hosts = append(value.Hosts, CompetitionExecutorControlHostAmendment{HostId: host.HostId, PreviousQualificationSha256: host.QualificationSha256,
			QualificationSha256: strings.Repeat("c", 64), PreviousAgentSha256: strings.Repeat("d", 64), AgentSha256: strings.Repeat("e", 64), PreviousBuilderSha256: strings.Repeat("f", 64), BuilderSha256: strings.Repeat("0", 64)})
	}
	return value
}

// Each failure is a separate rolled-back transaction, not a savepoint or a
// disabled trigger; the original queued job remains an unstarted FIFO member.
func insertExecutorControlPgValue(ctx context.Context, fixture *competitionDistributedPgFixture, value CompetitionExecutorControlAmendment) error {
	content, digest, err := competitionExecutorControlAmendmentBytes(value)
	if err != nil {
		return err
	}
	return captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			automationScopeLockInTx(ctx, tx, fixture.settings)
			server.Raise(fixture.store.requireAutomationLeaseInTx(ctx, tx, fixture.settings, *fixture.settings.automationLease))
			server.RaisePgResult(tx.Exec(ctx, `SELECT set_config('urnetwork.competition_executor_control_amendment',$1,true)`, digest))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO competition_executor_control_amendment(round_id,amendment_sha256,amendment_json,amendment_canonical,created_at) VALUES($1,$2,$3,$4,$5)`, value.RoundId, digest, json.RawMessage(content), content, fixture.clock.Now()))
		}, server.OptReadWrite())
	})
}

func TestExecutorControlAmendmentPgPreservesFrozenRoundAndQueuedClock(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, false, false)
		value := executorControlPgValue(fixture)
		var original string
		server.Db(tb.Context(), func(conn server.PgConn) {
			server.Raise(conn.QueryRow(tb.Context(), `SELECT policy_json::text FROM competition_round WHERE round_id=$1`, fixture.round.RoundId).Scan(&original))
		})
		if err := insertExecutorControlPgValue(tb.Context(), fixture, value); err != nil {
			tb.Fatal(err)
		}
		if err := fixture.store.loadExecutorControlAmendment(tb.Context(), fixture.settings, fixture.round); err != nil {
			tb.Fatal(err)
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var actual, fingerprint, state string
			var attempts int
			var started *time.Time
			server.Raise(conn.QueryRow(tb.Context(), `SELECT policy_json::text,competition_executor_control_host(round_id,$2)->>'qualification_sha256' FROM competition_round WHERE round_id=$1`, fixture.round.RoundId, value.Hosts[0].HostId).Scan(&actual, &fingerprint))
			server.Raise(conn.QueryRow(tb.Context(), `SELECT state,attempt_count,started_at FROM competition_job WHERE job_id=$1`, fixture.sourceJob.JobId).Scan(&state, &attempts, &started))
			if original != actual || fingerprint != value.Hosts[0].QualificationSha256 || state != "queued" || attempts != 0 || started != nil {
				tb.Fatal("control replacement changed frozen round or started a customer clock")
			}
		})
		for _, query := range []string{`UPDATE competition_executor_control_amendment SET created_at=created_at+interval '1 second' WHERE round_id=$1`, `DELETE FROM competition_executor_control_amendment WHERE round_id=$1`} {
			err := captureDatabaseError(func() {
				server.Tx(tb.Context(), func(tx server.PgTx) { server.RaisePgResult(tx.Exec(tb.Context(), query, fixture.round.RoundId)) })
			})
			if err == nil {
				tb.Fatal("operational amendment was rewritten or deleted")
			}
		}
	})
}

// An old process cannot spend a new admission clock after the atomic amendment.
// The approved process still receives precisely one immutable three-hour clock.
func TestExecutorControlAmendmentPgFencesStaleAdmissionBeforeClock(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, false)
		value := executorControlPgValue(fixture)
		if err := insertExecutorControlPgValue(tb.Context(), fixture, value); err != nil {
			tb.Fatal(err)
		}
		_, digest, err := competitionExecutorControlAmendmentBytes(value)
		if err != nil {
			tb.Fatal(err)
		}
		insert := func(pin string) error {
			return captureDatabaseError(func() {
				server.Tx(tb.Context(), func(tx server.PgTx) {
					automationScopeLockInTx(tb.Context(), tx, fixture.settings)
					server.Raise(fixture.store.requireAutomationLeaseInTx(tb.Context(), tx, fixture.settings, *fixture.settings.automationLease))
					server.RaisePgResult(tx.Exec(tb.Context(), `SELECT set_config('urnetwork.competition_executor_control_amendment',$1,true)`, pin))
					server.RaisePgResult(tx.Exec(tb.Context(), `INSERT INTO competition_executor_qualification_run(round_id,host_id,job_id,started_at,deadline_at,api_image_digest,worker_image_digest) VALUES($1,$2,$3,$4,$5,$6,$6)`, fixture.round.RoundId, value.Hosts[0].HostId, server.NewId(), fixture.clock.Now(), fixture.clock.Now().Add(3*time.Hour), testWorkerImageDigest()))
				}, server.OptReadWrite())
			})
		}
		if err := insert(""); err == nil {
			tb.Fatal("stale coordinator started a replacement-host clock")
		}
		if err := insert(digest); err != nil {
			tb.Fatal("approved replacement could not start its first admission clock", err)
		}
		if err := insertExecutorControlPgValue(tb.Context(), fixture, value); err == nil {
			tb.Fatal("amendment reset an already-started host qualification")
		}
	})
}

// Both corrected executors must publish fresh, amendment-bound evidence before
// the first customer's clock starts; the existing original FIFO still decides.
func TestExecutorControlAmendmentPgRequiresBothCorrectedHostsBeforeClaim(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		fixture := newCompetitionDistributedPgFixture(t, tb, true, false)
		value := executorControlPgValue(fixture)
		if err := insertExecutorControlPgValue(tb.Context(), fixture, value); err != nil {
			tb.Fatal(err)
		}
		if err := fixture.store.loadExecutorControlAmendment(tb.Context(), fixture.settings, fixture.round); err != nil {
			tb.Fatal(err)
		}
		fixture.qualifyHost(tb, fixture.settings.DistributedEvaluationPolicy.Hosts[0])
		job, err := fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-worker-a", testWorkerImageDigest())
		if err != nil || job != nil {
			tb.Fatal("one corrected executor bypassed whole-set admission", err)
		}
		fixture.qualifyHost(tb, fixture.settings.DistributedEvaluationPolicy.Hosts[1])
		job, err = fixture.store.Claim(tb.Context(), fixture.selected(0), "synthetic-worker-a", testWorkerImageDigest())
		if err != nil || job == nil || job.JobId != fixture.sourceJob.JobId || job.StartedAt == nil || job.AttemptCount != 1 || job.ExecutionDeadline == nil || !job.ExecutionDeadline.Equal(job.StartedAt.Add(3*time.Hour)) {
			tb.Fatal("fresh corrected-host admission lost FIFO or the full customer budget", err)
		}
	})
}
