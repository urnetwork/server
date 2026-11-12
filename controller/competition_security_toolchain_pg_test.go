// Existing append-only events durably fence the one approved compiler decision;
// real FIFO claims prove that rejected changes consume no submission clocks.
package controller

import (
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Reuse the full future-round fixture, including publication, lease and native
// admission guards. Only visibly synthetic execution identities are replaced.
func securityToolchainPgFixture(t *testing.T, tb testing.TB) (*competitionDistributedPgFixture, *roundBaselineRecord) {
	tb.Helper()
	f := newCompetitionDistributedPgFixture(t, tb, true, true)
	b, err := f.store.distributedRoundBaseline(tb.Context(), f.settings, f.round.RoundId)
	if err != nil || b == nil {
		tb.Fatal("missing synthetic retained baseline", err)
	}
	v := &competitionSecurityToolchainException{Schema: 1, Kind: "epoch2-security-toolchain-exception/v1", RoundId: f.round.RoundId, Epoch: 2,
		BaselineSha256: b.Sha256, SourceLockSha256: f.settings.AutomationPolicy.SourceLockSha256,
		PreviousEvaluatorImageDigest: f.settings.EvaluatorImageDigest, EvaluatorImageDigest: "sha256:" + strings.Repeat("8", 64),
		PreviousInstrumentSha256: f.settings.EvaluationPolicy.SimulatorSha256, InstrumentSha256: strings.Repeat("9", 64),
		PreviousToolchain: "go1.27.3", Toolchain: "go1.27.4", EvidenceSha256: strings.Repeat("a", 64)}
	for _, host := range f.settings.DistributedEvaluationPolicy.Hosts {
		v.Hosts = append(v.Hosts, competitionSecurityToolchainHost{HostId: host.HostId, PreviousQualificationSha256: host.QualificationSha256, QualificationSha256: strings.Repeat("0", 64)})
	}
	f.settings.securityToolchainException = v
	return f, b
}

// Registration is idempotent, starts no clock, and leaves the same historical
// baseline available to two independently claimed FIFO submissions.
func TestCompetitionSecurityToolchainPgRecordsOnceAndPreservesFifo(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		f, b := securityToolchainPgFixture(t, tb)
		for range 2 {
			if err := f.store.recordCompetitionSecurityToolchainException(tb.Context(), f.settings, f.round, b); err != nil {
				tb.Fatal(err)
			}
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var decisions, started int
			server.Raise(conn.QueryRow(tb.Context(), `SELECT count(*) FROM competition_job_event WHERE job_id=$1 AND event_type='security_toolchain_exception'`, b.SourceJobId).Scan(&decisions))
			server.Raise(conn.QueryRow(tb.Context(), `SELECT count(*) FROM competition_job WHERE round_id=$1 AND started_at IS NOT NULL`, f.round.RoundId).Scan(&started))
			if decisions != 1 || started != 0 {
				tb.Fatal("registration duplicated authority or started a customer clock", decisions, started)
			}
		})
		oldest := f.addJob(tb, server.Id{15: 1}, f.clock.Now(), "hotfix-oldest")
		middle := f.addJob(tb, server.Id{15: 2}, f.clock.Now(), "hotfix-middle")
		for index, expected := range []server.Id{oldest.JobId, middle.JobId} {
			job, err := f.store.Claim(tb.Context(), f.selected(index), "synthetic-hotfix-worker", testWorkerImageDigest())
			if err != nil || job == nil || job.JobId != expected || job.RoundBaseline == nil || job.RoundBaseline.Sha256 != b.Sha256 || job.EvaluatorImageDigest != f.settings.EvaluatorImageDigest {
				tb.Fatal("hotfix changed FIFO, historical image or shared control", index, err)
			}
		}
		if err := f.store.recordCompetitionSecurityToolchainException(tb.Context(), f.settings, f.round, b); err != nil {
			tb.Fatal("identical restart could not reuse decision after execution started", err)
		}
	})
}

// A restart cannot omit or replace its durable exception before a claim. The
// failed claim rolls back and leaves every original evaluation clock unstarted.
func TestCompetitionSecurityToolchainPgRejectsMissingOrChangedRestartAuthority(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		f, b := securityToolchainPgFixture(t, tb)
		if job, err := f.store.Claim(tb.Context(), f.selected(0), "synthetic-hotfix-worker", testWorkerImageDigest()); err == nil || job != nil {
			tb.Fatal("unregistered exception claimed a job", err)
		}
		if err := f.store.recordCompetitionSecurityToolchainException(tb.Context(), f.settings, f.round, b); err != nil {
			tb.Fatal(err)
		}
		for _, change := range []string{"omit", "replace"} {
			settings := *f.selected(0)
			if change == "omit" {
				settings.securityToolchainException = nil
			} else {
				value := *settings.securityToolchainException
				value.EvidenceSha256 = strings.Repeat("c", 64)
				settings.securityToolchainException = &value
			}
			if job, err := f.store.Claim(tb.Context(), &settings, "synthetic-hotfix-worker", testWorkerImageDigest()); err == nil || job != nil {
				tb.Fatal("changed restart authority claimed work", change, err)
			}
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var started int
			server.Raise(conn.QueryRow(tb.Context(), `SELECT count(*) FROM competition_job WHERE round_id=$1 AND started_at IS NOT NULL`, f.round.RoundId).Scan(&started))
			if started != 0 {
				tb.Fatal("rejected hotfix consumed a submission clock")
			}
		})
	})
}

// A newly requested exception cannot rewrite the toolchain for a round that
// already started ordinary execution, even at a currently idle host boundary.
func TestCompetitionSecurityToolchainPgRejectsFirstRegistrationAfterExecutionStart(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		f, b := securityToolchainPgFixture(t, tb)
		value := f.settings.securityToolchainException
		f.settings.securityToolchainException = nil
		job, err := f.store.Claim(tb.Context(), f.selected(0), "synthetic-original-worker", testWorkerImageDigest())
		if err != nil || job == nil {
			tb.Fatal("ordinary counterfactual did not start", err)
		}
		f.settings.securityToolchainException = value
		if err := f.store.recordCompetitionSecurityToolchainException(tb.Context(), f.settings, f.round, b); err == nil {
			tb.Fatal("late first exception was accepted")
		}
	})
}
