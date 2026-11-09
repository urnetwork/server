//go:build !sidecar_pending

package server

import (
	"strings"
	"testing"
)

// Every group stays pending until its own Gn-out. A release build links none of
// them, so no group reports linked and no statement of the migrations slice names
// what a group creates or ends with. A Gn-out that registers a group moves its
// names out of this check with it.
func TestReleaseBuildRegistersNoPendingGroup(t *testing.T) {
	for _, linked := range []func() bool{
		SidecarMg0Linked, SidecarMg1Linked, SidecarMg2Linked, SidecarMg5Linked,
		SidecarMg6Linked, SidecarMg7Linked, SidecarMg8Linked, SidecarMg9Linked,
	} {
		if linked() {
			t.Fatal("a release build links a pending group")
		}
	}
	if 0 < len(sidecarPendingMigrations()) {
		t.Fatal("a release build has pending migrations")
	}
	names := append([]string{
		"network_client_owner", "auth_code_api_key_id", "network_auth_key", "network_admin_event", "sidecar_identity",
		// §20's parts of MG1 and MG2 (QUOTA-M1-02, QUOTA-M2-02)
		"network_sidecar_limit", "network_quota_state", "network_service_usage", "service_debit_journal",
		"service_usage_batch", "protect_pending_service_debit",
	}, sidecarGroupMarkerIds...)
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
		for _, name := range names {
			if strings.Contains(sql, name) {
				t.Fatalf("migration %d names %s", index, name)
			}
		}
	}
}
