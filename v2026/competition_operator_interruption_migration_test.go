// Native PostgreSQL checks the independent additive guard, not a Go mock or
// a guessed heartbeat event. Every identity and retained digest is synthetic.
package server

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A minimal catalog isolates the new predicate and trigger. Controller tests
// independently exercise the public grant against the entire migration chain.
func competitionOperatorInterruptionSqlFixture(t testing.TB, conn PgConn) (Id, Id, map[string]any, map[string]any) {
	t.Helper()
	RaisePgResult(conn.Exec(t.Context(), `CREATE TABLE competition_round(round_id uuid PRIMARY KEY,policy_json jsonb);
 CREATE TABLE competition_job_event(event_id bigint PRIMARY KEY,job_id uuid,event_at timestamp,event_type text,payload_json jsonb,payload_sha256 text);
 CREATE INDEX synthetic_operator_event_order ON competition_job_event(job_id,event_id);
 CREATE TABLE competition_job_reevaluation(job_id uuid,round_id uuid,original_terminal_json jsonb,authorization_json jsonb,receipt_json jsonb);
 `+competitionOperatorInterruptionMigrationSql, pgx.QueryExecModeSimpleProtocol))
	jobId, roundId := NewId(), NewId()
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	completed := started.Add(3*time.Hour + 2*time.Minute)
	terminal := map[string]any{"job_id": jobId.String(), "round_id": roundId.String(), "state": "failed", "evaluation_cycle": 0,
		"attempt_count": 2, "started_at": started, "completed_at": completed, "score_json": nil,
		"lease_owner": nil, "lease_expires_at": nil,
		"eval_error_json": map[string]any{"kind": "infrastructure", "code": "evaluation_time_budget_exhausted", "retriable": false}}
	proof := map[string]any{"schema": 1, "action": "stop_worker", "stopped_at": started.Add(50 * time.Minute), "claim_event_id": 10,
		"claim_event_sha256": strings.Repeat("a", 64), "evidence_sha256": strings.Repeat("b", 64)}
	request := map[string]any{"incident_class": "operator_authorized_interrupted_budget_v1", "job_id": jobId.String(), "round_id": roundId.String(),
		"expected_cycle": 0, "single_use": true, "incident_attempt": 1, "expected_attempt_count": 2, "expected_terminal_snapshot": terminal,
		"incident_event_id": 30, "incident_event_sha256": strings.Repeat("c", 64), "authorized_at": completed.Add(time.Minute),
		"operator_interruption_evidence": proof}
	RaisePgResult(conn.Exec(t.Context(), `INSERT INTO competition_round VALUES($1,'{"evaluation_policy":{"score_timeout_seconds":10800}}')`, roundId))
	RaisePgResult(conn.Exec(t.Context(), `INSERT INTO competition_job_event VALUES
 (10,$1,$2,'claimed','{"attempt":1}',$3),(20,$1,$4,'claimed','{"attempt":2}',$3),(30,$1,$5,'failed','{"attempt":2}',$6)`,
		jobId, started, strings.Repeat("a", 64), started.Add(3*time.Hour+time.Minute), completed, strings.Repeat("c", 64)))
	return jobId, roundId, terminal, request
}

// Unknown or malformed attestations fail closed even when the outer operator
// selected a matching terminal code. No history row is relabelled by the guard.
func TestCompetitionOperatorInterruptionSqlRejectsUnboundEvidence(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		Db(t.Context(), func(conn PgConn) {
			jobId, roundId, terminal, original := competitionOperatorInterruptionSqlFixture(t, conn)
			for _, c := range []struct {
				name    string
				allowed bool
				mutate  func(map[string]any)
			}{
				{name: "exact", allowed: true},
				{name: "missing-proof", mutate: func(a map[string]any) { delete(a, "operator_interruption_evidence") }},
				{name: "null-proof", mutate: func(a map[string]any) { a["operator_interruption_evidence"] = nil }},
				{name: "scalar-proof", mutate: func(a map[string]any) { a["operator_interruption_evidence"] = "synthetic" }},
				{name: "unknown-field", mutate: func(a map[string]any) { a["operator_interruption_evidence"].(map[string]any)["waive"] = true }},
				{name: "string-schema", mutate: func(a map[string]any) { a["operator_interruption_evidence"].(map[string]any)["schema"] = "1" }},
				{name: "wrong-action", mutate: func(a map[string]any) {
					a["operator_interruption_evidence"].(map[string]any)["action"] = "synthetic_product_failure"
				}},
				{name: "missing-stop-evidence", mutate: func(a map[string]any) {
					delete(a["operator_interruption_evidence"].(map[string]any), "evidence_sha256")
				}},
				{name: "wrong-claim-digest", mutate: func(a map[string]any) {
					a["operator_interruption_evidence"].(map[string]any)["claim_event_sha256"] = strings.Repeat("d", 64)
				}},
				{name: "string-claim-id", mutate: func(a map[string]any) { a["operator_interruption_evidence"].(map[string]any)["claim_event_id"] = "10" }},
				{name: "missing-claim", mutate: func(a map[string]any) { a["operator_interruption_evidence"].(map[string]any)["claim_event_id"] = 9 }},
				{name: "overflow-claim", mutate: func(a map[string]any) {
					a["operator_interruption_evidence"].(map[string]any)["claim_event_id"] = json.Number("9999999999999999999")
				}},
				{name: "bad-stop-date", mutate: func(a map[string]any) {
					a["operator_interruption_evidence"].(map[string]any)["stopped_at"] = "2026-99-01T00:50:00Z"
				}},
				{name: "submicrosecond-stop", mutate: func(a map[string]any) {
					a["operator_interruption_evidence"].(map[string]any)["stopped_at"] = "2026-01-01T00:50:00.0000001Z"
				}},
				{name: "before-claim", mutate: func(a map[string]any) {
					a["operator_interruption_evidence"].(map[string]any)["stopped_at"] = "2025-12-31T23:59:59Z"
				}},
				{name: "at-expiry", mutate: func(a map[string]any) {
					a["operator_interruption_evidence"].(map[string]any)["stopped_at"] = "2026-01-01T03:00:00Z"
				}},
				{name: "missing-authorization-time", mutate: func(a map[string]any) { delete(a, "authorized_at") }},
				{name: "null-authorization-time", mutate: func(a map[string]any) { a["authorized_at"] = nil }},
				{name: "numeric-authorization-time", mutate: func(a map[string]any) { a["authorized_at"] = 1 }},
				{name: "infinite-authorization-time", mutate: func(a map[string]any) { a["authorized_at"] = "infinity" }},
				{name: "missing-terminal-attempt", mutate: func(a map[string]any) { delete(a, "expected_attempt_count") }},
				{name: "null-terminal-attempt", mutate: func(a map[string]any) { a["expected_attempt_count"] = nil }},
				{name: "missing-terminal-event", mutate: func(a map[string]any) { delete(a, "incident_event_id") }},
				{name: "null-terminal-event", mutate: func(a map[string]any) { a["incident_event_id"] = nil }},
				{name: "missing-stop-time", mutate: func(a map[string]any) { delete(a["operator_interruption_evidence"].(map[string]any), "stopped_at") }},
				{name: "null-stop-time", mutate: func(a map[string]any) { a["operator_interruption_evidence"].(map[string]any)["stopped_at"] = nil }},
				{name: "terminal-as-interrupted-attempt", mutate: func(a map[string]any) { a["incident_attempt"] = 2 }},
				{name: "different-outer-job", mutate: func(a map[string]any) { a["job_id"] = NewId().String() }},
				{name: "different-terminal", mutate: func(a map[string]any) {
					a["expected_terminal_snapshot"].(map[string]any)["patch_sha256"] = strings.Repeat("e", 64)
				}},
				{name: "invented-archive-proof", mutate: func(a map[string]any) { a["failure_manifest_sha256"] = strings.Repeat("e", 64) }},
				{name: "borrowed-backing-proof", mutate: func(a map[string]any) { a["backing_service_evidence"] = map[string]any{} }},
				{name: "wrong-terminal-event", mutate: func(a map[string]any) { a["incident_event_id"] = 20 }},
				{name: "wrong-terminal-event-digest", mutate: func(a map[string]any) { a["incident_event_sha256"] = strings.Repeat("d", 64) }},
			} {
				content, err := json.Marshal(original)
				if err != nil {
					t.Fatal(err)
				}
				var request map[string]any
				if err := json.Unmarshal(content, &request); err != nil {
					t.Fatal(err)
				}
				if c.mutate != nil {
					c.mutate(request)
				}
				requestBytes, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				terminalBytes, err := json.Marshal(terminal)
				if err != nil {
					t.Fatal(err)
				}
				var allowed bool
				Raise(conn.QueryRow(t.Context(), `SELECT competition_job_operator_interruption_valid($1,$2,$3::jsonb,$4::jsonb)`, jobId, roundId,
					string(terminalBytes), string(requestBytes)).Scan(&allowed))
				if allowed != c.allowed {
					t.Fatalf("%s allowed=%t want=%t", c.name, allowed, c.allowed)
				}
			}
		}, OptReadWrite(), OptNoRetry())
	})
}

// The independent insert trigger preserves the typed failure, refuses borrowed
// evidence and sees intervening real events even if a caller bypasses Go.
func TestCompetitionOperatorInterruptionSqlInsertGuardRequiresExactHistory(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		Db(t.Context(), func(conn PgConn) {
			jobId, roundId, terminal, request := competitionOperatorInterruptionSqlFixture(t, conn)
			terminalBytes, err := json.Marshal(terminal)
			if err != nil {
				t.Fatal(err)
			}
			receipt := map[string]any{"incident_class": "operator_authorized_interrupted_budget_v1", "execution_timeout_seconds": 10800,
				"previous_failure": terminal["eval_error_json"]}
			insert := func() error {
				requestBytes, marshalErr := json.Marshal(request)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				receiptBytes, marshalErr := json.Marshal(receipt)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				_, insertErr := conn.Exec(t.Context(), `INSERT INTO competition_job_reevaluation VALUES($1,$2,$3::jsonb,$4::jsonb,$5::jsonb)`,
					jobId, roundId, string(terminalBytes), string(requestBytes), string(receiptBytes))
				return insertErr
			}
			refuse := func(name string) {
				var pgError *pgconn.PgError
				if err := insert(); !errors.As(err, &pgError) || pgError.Code != "P0001" {
					t.Fatalf("%s did not reach explicit authority refusal: %v", name, err)
				}
			}
			receipt["incident_class"] = "operator_authorized_heartbeat_budget_v1"
			refuse("relabelled receipt")
			receipt["incident_class"] = "operator_authorized_interrupted_budget_v1"
			request["incident_class"] = "operator_authorized_heartbeat_budget_v1"
			refuse("borrowed stop proof")
			request["incident_class"] = "operator_authorized_interrupted_budget_v1"
			RaisePgResult(conn.Exec(t.Context(), `INSERT INTO competition_job_event VALUES(15,$1,TIMESTAMP '2026-01-01 01:00:00','handed_back','{"reason":"worker_shutdown"}',$2)`, jobId, strings.Repeat("f", 64)))
			refuse("hidden hand-back")
			RaisePgResult(conn.Exec(t.Context(), `DELETE FROM competition_job_event WHERE job_id=$1 AND event_id=15`, jobId))
			RaisePgResult(conn.Exec(t.Context(), `UPDATE competition_job_event SET event_at=TIMESTAMP '2026-01-01 02:59:59' WHERE job_id=$1 AND event_id=20`, jobId))
			refuse("unexpired reclaim")
			RaisePgResult(conn.Exec(t.Context(), `UPDATE competition_job_event SET event_at=TIMESTAMP '2026-01-01 03:01:00' WHERE job_id=$1 AND event_id=20`, jobId))
			if err := insert(); err != nil {
				t.Fatalf("exact additive trigger rejected bound sequence: %v", err)
			}
			delete(request, "operator_interruption_evidence")
			request["incident_class"] = "operator_authorized_heartbeat_budget_v1"
			if err := insert(); err != nil {
				t.Fatalf("additive trigger narrowed an older incident class: %v", err)
			}
		}, OptReadWrite(), OptNoRetry())
	})
}

// The standalone predicate cannot rely on an earlier trigger to reject an
// absent terminal custody field, even when the two supplied snapshots agree.
func TestCompetitionOperatorInterruptionSqlMissingTerminalFieldsFailClosed(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		Db(t.Context(), func(conn PgConn) {
			jobId, roundId, original, request := competitionOperatorInterruptionSqlFixture(t, conn)
			for _, field := range []string{"job_id", "round_id", "state", "evaluation_cycle", "attempt_count", "started_at", "completed_at", "score_json", "lease_owner", "lease_expires_at", "eval_error_json"} {
				terminal := map[string]any{}
				for key, value := range original {
					terminal[key] = value
				}
				delete(terminal, field)
				request["expected_terminal_snapshot"] = terminal
				terminalBytes, err := json.Marshal(terminal)
				if err != nil {
					t.Fatal(err)
				}
				requestBytes, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				var allowed bool
				Raise(conn.QueryRow(t.Context(), `SELECT competition_job_operator_interruption_valid($1,$2,$3::jsonb,$4::jsonb)`, jobId, roundId,
					string(terminalBytes), string(requestBytes)).Scan(&allowed))
				if allowed {
					t.Fatalf("missing terminal %s passed the independent predicate", field)
				}
			}
		}, OptReadWrite(), OptNoRetry())
	})
}
