// Real PostgreSQL keeps replacement, operator, append-only and whole-host
// boundaries enabled for a synthetic failed admission and separate approval.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The database tests scope and authority; pure tests recompute all statistics.
func admissionExceptionPgFixture(t *testing.T, tb testing.TB) (*competitionDistributedPgFixture, CompetitionExecutorAdmissionException, map[string]any) {
	tb.Helper()
	f, recovery := executorQualificationRecoveryPgFixture(t, tb)
	if err := insertExecutorQualificationRecoveryPg(tb.Context(), f, recovery, true); err != nil {
		tb.Fatal(err)
	}
	_, recoverySha, err := competitionExecutorQualificationRecoveryBytes(recovery)
	if err != nil {
		tb.Fatal(err)
	}
	var started, deadline time.Time
	server.Db(tb.Context(), func(conn server.PgConn) {
		server.Raise(conn.QueryRow(tb.Context(), `SELECT started_at,deadline_at FROM competition_executor_qualification_clock($1,$2)`, recovery.RoundId, recovery.HostId).Scan(&started, &deadline))
	})
	r := map[string]any{"schema": 1, "kind": "competition-executor-noop-admission/v1", "round_id": recovery.RoundId.String(), "host_id": recovery.HostId, "qualification_job_id": recovery.ReplacementJobId.String(), "qualification_started_at": started, "qualification_deadline_at": deadline, "created_at": started,
		"baseline_sha256": recovery.BaselineSha256, "source_lock_sha256": recovery.SourceLockSha256, "providers_sha256": recovery.ProvidersSha256, "evaluator_image_digest": recovery.EvaluatorImageDigest, "qualification_sha256": recovery.QualificationSha256, "policy_approval_sha256": recovery.PolicyApprovalSha256, "config_local_sha256": recovery.ConfigLocalSha256, "vault_local_sha256": recovery.VaultLocalSha256, "recovery_sha256": recoverySha,
		"passed": false, "noop": true, "replicates": 9, "statistical_equivalence_proven": false, "screen_kind": "operational-admission/v1", "statistical_method": "two-sided-welch-t-run-level", "confidence_level": 0.95, "maximum_raw_score_relative_difference": 0.05, "raw_score_relative_difference": 0.01, "archive_manifest_sha256": strings.Repeat("a", 64),
		"retention": map[string]any{"sha256": strings.Repeat("b", 64), "mode": "COMPLIANCE", "retain_until": f.settings.RetainUntil, "version_id": "synthetic-retained-version", "key": "synthetic/qualification", "bytes": 100}}
	f.addCoverageToReceipt(tb, r)
	metrics := r["metrics"].([]competitionHostMetricScreen)
	for i := range metrics {
		if metrics[i].Metric == "throughput_p50_bytes_per_s" {
			metrics[i].SignificantShift = true
		}
	}
	r["metrics"] = metrics
	v := CompetitionExecutorAdmissionException{Schema: 1, Kind: "executor-retained-admission-exception/v1", RoundId: recovery.RoundId, HostId: recovery.HostId, JobId: recovery.ReplacementJobId, BaselineSha256: recovery.BaselineSha256, ReceiptDocumentSha256: strings.Repeat("b", 64), ExceptedMetric: "throughput_p50_bytes_per_s", EvidenceSha256: strings.Repeat("c", 64)}
	return f, v, r
}

// Rehashed wrong scopes still exercise the database's independent guard.
func insertAdmissionExceptionPg(tb testing.TB, f *competitionDistributedPgFixture, v CompetitionExecutorAdmissionException, r map[string]any, fenced bool) error {
	tb.Helper()
	approval, err := json.Marshal(v)
	if err != nil {
		return err
	}
	a := sha256.Sum256(approval)
	content, err := json.Marshal(r)
	if err != nil {
		return err
	}
	d := sha256.Sum256(content)
	return captureDatabaseError(func() {
		server.Tx(tb.Context(), func(tx server.PgTx) {
			if fenced {
				server.Raise(f.store.requireAutomationLeaseInTx(tb.Context(), tx, f.settings, *f.settings.automationLease))
			}
			server.RaisePgResult(tx.Exec(tb.Context(), `SELECT set_config('urnetwork.competition_executor_admission_exception',$1,true)`, hex.EncodeToString(a[:])))
			server.RaisePgResult(tx.Exec(tb.Context(), `INSERT INTO competition_executor_admission_exception(round_id,host_id,approval_sha256,approval_json,approval_canonical,receipt_json,receipt_canonical,receipt_sha256,baseline_sha256,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, v.RoundId, v.HostId, hex.EncodeToString(a[:]), json.RawMessage(approval), approval, json.RawMessage(content), content, hex.EncodeToString(d[:]), v.BaselineSha256, r["created_at"]))
		}, server.OptReadWrite())
	})
}

func TestExecutorAdmissionExceptionPgPreservesFailureAndAdmitsBothHosts(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		f, v, r := admissionExceptionPgFixture(t, tb)
		if err := insertAdmissionExceptionPg(tb, f, v, r, true); err != nil {
			tb.Fatal(err)
		}
		ready := func() bool {
			var result bool
			server.Db(tb.Context(), func(conn server.PgConn) {
				server.Raise(conn.QueryRow(tb.Context(), `SELECT competition_executor_control_hosts_ready($1)`, v.RoundId).Scan(&result))
			})
			return result
		}
		if ready() {
			tb.Fatal("exception on one host bypassed sibling qualification")
		}
		f.qualifyHost(tb, f.settings.DistributedEvaluationPolicy.Hosts[1])
		if !ready() {
			tb.Fatal("exact exception and passing sibling did not admit both hosts")
		}
		server.Db(tb.Context(), func(conn server.PgConn) {
			var passed bool
			var started int
			server.Raise(conn.QueryRow(tb.Context(), `SELECT (receipt_json->>'passed')::boolean FROM competition_executor_admission_exception WHERE round_id=$1 AND host_id=$2`, v.RoundId, v.HostId).Scan(&passed))
			server.Raise(conn.QueryRow(tb.Context(), `SELECT count(*) FROM competition_job WHERE round_id=$1 AND started_at IS NOT NULL`, v.RoundId).Scan(&started))
			if passed || started != 0 {
				tb.Fatal("exception relabeled failure or started a customer clock")
			}
		})
		for _, query := range []string{`UPDATE competition_executor_admission_exception SET approval_sha256=approval_sha256 WHERE round_id=$1`, `DELETE FROM competition_executor_admission_exception WHERE round_id=$1`} {
			if captureDatabaseError(func() {
				server.Tx(tb.Context(), func(tx server.PgTx) { server.RaisePgResult(tx.Exec(tb.Context(), query, v.RoundId)) })
			}) == nil {
				tb.Fatal("exception was not append-only")
			}
		}
		if captureDatabaseError(func() {
			server.Tx(tb.Context(), func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(tb.Context(), `TRUNCATE competition_executor_admission_exception`))
			})
		}) == nil {
			tb.Fatal("exception could be truncated")
		}
	})
}

func TestExecutorAdmissionExceptionPgRejectsRehashedScopeAndStaleFence(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(tb testing.TB) {
		f, v, r := admissionExceptionPgFixture(t, tb)
		if insertAdmissionExceptionPg(tb, f, v, r, false) == nil {
			tb.Fatal("unfenced approval accepted")
		}
		for _, change := range []string{"job", "baseline", "metric", "document", "passed", "other-shift", "missing-retention", "wrong-source", "raw-bound"} {
			value := v
			copy := map[string]any{}
			for k, item := range r {
				copy[k] = item
			}
			switch change {
			case "job":
				value.JobId = server.NewId()
			case "baseline":
				value.BaselineSha256 = strings.Repeat("0", 64)
			case "metric":
				value.ExceptedMetric = "ttfb_p95_ms"
			case "document":
				value.ReceiptDocumentSha256 = strings.Repeat("0", 64)
			case "passed":
				copy["passed"] = true
			case "other-shift":
				metrics := append([]competitionHostMetricScreen(nil), r["metrics"].([]competitionHostMetricScreen)...)
				metrics[0].SignificantShift = true
				copy["metrics"] = metrics
			case "missing-retention":
				copy["retention"] = map[string]any{}
			case "wrong-source":
				copy["source_lock_sha256"] = strings.Repeat("0", 64)
			case "raw-bound":
				copy["raw_score_relative_difference"] = 0.06
			}
			if insertAdmissionExceptionPg(tb, f, value, copy, true) == nil {
				tb.Fatal("rehashed approval changed scope", change)
			}
		}
	})
}
