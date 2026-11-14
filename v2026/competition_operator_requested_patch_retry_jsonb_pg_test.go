// Actual PostgreSQL operator resolution must subtract fields from the projected
// object, never cast the bare projection key to jsonb as a subtraction operand.
package server

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The old expression deterministically raises 22P02. The exact production
// expression accepts only declared fields and still detects an unknown key.
func TestCompetitionOperatorPatchRetryPgJsonbFieldFilterUsesParenthesizedProjection(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		known := `{"patch_retry":{"schema":1,"patch_sha256":"synthetic-patch","applicability_evidence_sha256":"synthetic-evidence"}}`
		oldExpression := `$1::jsonb->'patch_retry'-ARRAY['schema','patch_sha256','applicability_evidence_sha256']<>'{}'::jsonb`
		Db(ctx, func(conn PgConn) {
			var unknown bool
			err := conn.QueryRow(ctx, "SELECT "+oldExpression, known).Scan(&unknown)
			var databaseError *pgconn.PgError
			if !errors.As(err, &databaseError) || databaseError.Code != "22P02" {
				tb.Fatal("pre-fix projection/subtraction precedence did not reproduce PostgreSQL invalid-json error", err)
			}
		})
		marker := "ARRAY['schema','patch_sha256','applicability_evidence_sha256']"
		expression, matches := "", 0
		for _, line := range strings.Split(CompetitionOperatorRequestedPatchRetryBodiesSql().AuditValidation, "\n") {
			if strings.Contains(line, marker) {
				expression = strings.TrimPrefix(strings.TrimSpace(line), "OR ")
				matches++
			}
		}
		if matches != 1 || !strings.HasPrefix(expression, "(authority->'patch_retry') - ARRAY") {
			tb.Fatal("production patch retry audit lost exact parenthesized field filter")
		}
		expression = strings.ReplaceAll(expression, "authority", "$1::jsonb")
		for _, testCase := range []struct {
			name        string
			content     string
			wantUnknown bool
		}{
			{name: "declared-fields", content: known, wantUnknown: false},
			{name: "unknown-field", content: `{"patch_retry":{"schema":1,"patch_sha256":"synthetic-patch","applicability_evidence_sha256":"synthetic-evidence","synthetic_unknown":true}}`, wantUnknown: true},
		} {
			Db(ctx, func(conn PgConn) {
				var unknown bool
				if err := conn.QueryRow(ctx, "SELECT "+expression, testCase.content).Scan(&unknown); err != nil || unknown != testCase.wantUnknown {
					tb.Fatal("actual production field filter returned wrong verdict", testCase.name, unknown, err)
				}
			})
		}
	})
}
