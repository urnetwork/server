//go:build sidecar_pending

package server

import (
	"context"
	"regexp"
	"testing"
)

// §20's parts of the pending groups (QUOTA's tasks), held to CODESTYLE's migration
// rules on top of sidecarGroupFindings: new tables and their indexes only.

// MG1's §20 part: network_sidecar_limit alone, with exactly §20.14's columns.
func TestSidecarMg1QuotaPartIsAdditive(t *testing.T) {
	statement := regexp.MustCompile(`(?is)^\s*CREATE TABLE network_sidecar_limit\b`)
	refused := regexp.MustCompile(`(?i)\b(DROP|ALTER|RENAME|NOT VALID|TRUNCATE|UPDATE|DELETE|REFERENCES|transaction_timeout)\b`)
	migrations := sidecarMg1QuotaMigrations()
	if len(migrations) == 0 {
		t.Fatal("MG1's §20 part is empty")
	}
	for index, migration := range migrations {
		switch value := migration.(type) {
		case *SqlMigration:
			if !statement.MatchString(value.sql) {
				t.Errorf("entry %d is not an addition: %s", index, value.sql)
			}
			if refused.MatchString(value.sql) {
				t.Errorf("entry %d changes what the running release uses: %s", index, value.sql)
			}
		default:
			t.Fatalf("entry %d is a %T", index, migration)
		}
	}

	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		columns := map[string]string{}
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT column_name, data_type
				FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = 'network_sidecar_limit'
			`)
			WithPgResult(result, err, func() {
				for result.Next() {
					var column, dataType string
					Raise(result.Scan(&column, &dataType))
					columns[column] = dataType
				}
			})
		})
		want := map[string]string{
			"network_id":  "uuid",
			"limit_key":   "character varying",
			"limit_value": "bigint",
			"reason":      "character varying",
		}
		if len(columns) != len(want) {
			t.Errorf("columns %v, want %v", columns, want)
		}
		for column, dataType := range want {
			if columns[column] != dataType {
				t.Errorf("%s is %q, want %q", column, columns[column], dataType)
			}
		}
	})
}
