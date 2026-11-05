// Window parsing is exercised in PostgreSQL itself; Go validation alone cannot
// establish the direct-SQL guard's date, omission and malformed-input behavior.
package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Raw SQL needs only the new helper, not a migrated production-shaped schema.
func TestCompetitionReevaluationPostCloseSqlWindowRejectsUnboundAuthority(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		Db(t.Context(), func(conn PgConn) {
			RaisePgResult(conn.Exec(t.Context(), competitionReevaluationPostCloseWindowSql, pgx.QueryExecModeSimpleProtocol))
			closeAt := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
			granted := closeAt.Add(time.Minute)
			format := func(at time.Time) string { return at.Format(time.RFC3339Nano) }
			for _, c := range []struct {
				name    string
				mutate  func(map[string]any)
				allowed bool
			}{
				{name: "bound", allowed: true},
				{name: "omitted", mutate: func(a map[string]any) { delete(a, "post_close_approval") }},
				{name: "null", mutate: func(a map[string]any) { a["post_close_approval"] = nil }},
				{name: "scalar", mutate: func(a map[string]any) { a["post_close_approval"] = "synthetic-invalid-approval" }},
				{name: "array", mutate: func(a map[string]any) { a["post_close_approval"] = []any{} }},
				{name: "wrong-close", mutate: func(a map[string]any) {
					a["post_close_approval"].(map[string]any)["closes_at"] = format(closeAt.Add(time.Second))
				}},
				{name: "at-expiry", mutate: func(a map[string]any) { a["expires_at"] = format(granted) }},
				{name: "missing-evidence", mutate: func(a map[string]any) { delete(a["post_close_approval"].(map[string]any), "evidence_sha256") }},
				{name: "unknown-field", mutate: func(a map[string]any) { a["post_close_approval"].(map[string]any)["waive"] = true }},
				{name: "bad-date", mutate: func(a map[string]any) {
					a["post_close_approval"].(map[string]any)["expires_at"] = "synthetic-invalid-date"
				}},
				{name: "outside-approval", mutate: func(a map[string]any) { a["expires_at"] = format(closeAt.Add(3 * time.Hour)) }},
			} {
				authorization := map[string]any{"authorized_at": format(granted), "expires_at": format(closeAt.Add(time.Hour)),
					"post_close_approval": map[string]any{"schema": 1, "closes_at": format(closeAt), "expires_at": format(closeAt.Add(2 * time.Hour)), "evidence_sha256": strings.Repeat("a", 64)}}
				if c.mutate != nil {
					c.mutate(authorization)
				}
				content, err := json.Marshal(authorization)
				if err != nil {
					t.Fatal(err)
				}
				var allowed bool
				Raise(conn.QueryRow(t.Context(), `SELECT competition_job_reevaluation_post_close_allows($1::jsonb,$2::timestamp,$3::timestamp)`, string(content), closeAt, granted).Scan(&allowed))
				if allowed != c.allowed {
					t.Fatalf("%s allowed=%t want=%t", c.name, allowed, c.allowed)
				}
			}
		}, OptReadWrite(), OptNoRetry())
	})
}
