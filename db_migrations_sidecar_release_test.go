//go:build !sidecar_pending

package server

import (
	"strings"
	"testing"
)

// MG0 stays pending until its own Gn-out. A release build links none of it, so
// no statement of the migrations slice names what the group creates. A Gn-out
// that registers the group moves this check with it.
func TestReleaseBuildRegistersNoPendingGroup(t *testing.T) {
	if SidecarMg0Linked() {
		t.Fatal("a release build links MG0")
	}
	if 0 < len(sidecarPendingMigrations()) {
		t.Fatal("a release build has pending migrations")
	}
	for index, migration := range migrations {
		sql := ""
		switch value := migration.(type) {
		case *SqlMigration:
			sql = value.sql
		case *OnlineSqlMigration:
			sql = value.sql + value.auditSql
		case *CodeMigration:
			sql = value.id
		}
		for _, name := range []string{"network_client_owner", "sidecar_mg0", "auth_code_api_key_id"} {
			if strings.Contains(sql, name) {
				t.Fatalf("migration %d names %s", index, name)
			}
		}
	}
}
