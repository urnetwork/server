package server

import (
	"strings"
	"testing"
)

func TestCompetitionSilleStorageRecoveryPreservesAuditGuards(t *testing.T) {
	old := CompetitionOperatorRequestedPatchRetryBodiesSql().Audit
	next := CompetitionSilleStorageRecoveryAuditBodySql()
	const oldGate = "  OR COALESCE(authority->>'reason','') NOT IN ('retention_budget_exhausted','closed_legacy_prelaunch')"
	before, after, ok := strings.Cut(old, oldGate)
	if !ok || !strings.HasPrefix(next, before) || !strings.HasSuffix(next, after) {
		t.Fatal("storage recovery modified checks outside the narrow reason gate")
	}
	addition := strings.TrimSuffix(strings.TrimPrefix(next, before), after)
	for _, guard := range []string{
		"authority->>'reason' IS DISTINCT FROM 'sille_storage_interruption_20261117'",
		"NEW.job_id::text IS DISTINCT FROM '01a1e738-6c64-357a-8d4f-143e4b3364d3'",
		"NEW.round_id::text IS DISTINCT FROM '01a1c110-585b-ce38-4f29-ca4860a1c058'",
		"jsonb_typeof(NEW.authorization_json->'post_close_approval') IS DISTINCT FROM 'object'",
		"NEW.original_terminal_json->>'attempt_count' IS DISTINCT FROM '1'",
		"authority->>'original_host_id' IS DISTINCT FROM 'sille'",
		"authority->>'closure_evidence_sha256' IS DISTINCT FROM '3fc5cbab1e3e2ad6c7c85929f94d67ba599be415c5521ceb9e54bb8c59ef15e0'",
		"native->>'state' IS DISTINCT FROM 'expired'",
		"native->>'exit_code' IS DISTINCT FROM '1'",
		"native->>'error_code' IS DISTINCT FROM 'original_deadline_elapsed'",
	} {
		if !strings.Contains(addition, guard) {
			t.Errorf("storage recovery lost guard %q", guard)
		}
	}
}

func TestCompetitionSilleStorageRecoveryPostgresExactReason(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		start := "  OR (COALESCE(authority->>'reason','') NOT IN "
		end := "\n  OR jsonb_typeof(authority->'idle_slots')"
		_, gate, ok := strings.Cut(CompetitionSilleStorageRecoveryAuditBodySql(), start)
		gate, _, endFound := strings.Cut(gate, end)
		if !ok || !endFound {
			t.Fatal("cannot isolate the production reason predicate")
		}
		sql := "SELECT NOT (" + strings.TrimPrefix(start, "  OR ") + gate + `) FROM
			jsonb_to_record($1::jsonb) AS new(environment text,competition_id text,season_id text,round_id uuid,job_id uuid,
			  authorization_json jsonb,original_terminal_json jsonb,receipt_json jsonb),
			(SELECT $2::jsonb AS authority,$3::jsonb AS native) AS proof`
		for _, test := range []struct {
			name, field, value string
			want               bool
		}{
			{name: "exact expiry", want: true},
			{name: "different job", field: "job_id", value: "00000000-0000-0000-0000-000000000001"},
			{name: "different round", field: "round_id", value: "00000000-0000-0000-0000-000000000001"},
			{name: "different host", field: "original_host_id", value: "goofe"},
			{name: "missing closure", field: "closure_evidence_sha256"},
			{name: "not expired", field: "state", value: "failed"},
			{name: "unknown reason", field: "reason", value: "unknown"},
			{name: "legacy retention unchanged", field: "reason", value: "retention_budget_exhausted", want: true},
			{name: "legacy prelaunch unchanged", field: "reason", value: "closed_legacy_prelaunch", want: true},
		} {
			row := map[string]any{
				"environment": "main", "competition_id": "sim-latency-season-1", "season_id": "sim-latency-production-20261028-r2",
				"round_id": "01a1c110-585b-ce38-4f29-ca4860a1c058", "job_id": "01a1e738-6c64-357a-8d4f-143e4b3364d3",
				"authorization_json":     map[string]any{"post_close_approval": map[string]any{}},
				"original_terminal_json": map[string]any{"attempt_count": 1},
				"receipt_json":           map[string]any{"distributed": map[string]any{"reason": "sille_storage_interruption_20261117"}},
			}
			authority := map[string]any{
				"reason": "sille_storage_interruption_20261117", "original_host_id": "sille",
				"original_run_id":         "35e95bdc8f4db995113463b989323b19be5f5f4681b375ca7d8f731a35eece9b",
				"closure_evidence_sha256": "3fc5cbab1e3e2ad6c7c85929f94d67ba599be415c5521ceb9e54bb8c59ef15e0",
			}
			native := map[string]any{"state": "expired", "exit_code": 1, "error_code": "original_deadline_elapsed"}
			for _, input := range []map[string]any{row, authority, native} {
				if _, exists := input[test.field]; exists {
					input[test.field] = test.value
				}
			}
			var actual bool
			Db(t.Context(), func(conn PgConn) {
				Raise(conn.QueryRow(t.Context(), sql, row, authority, native).Scan(&actual))
			}, OptReadWrite(), OptNoRetry())
			if actual != test.want {
				t.Errorf("%s: reason predicate accepted=%t, want %t", test.name, actual, test.want)
			}
		}
	})
}
