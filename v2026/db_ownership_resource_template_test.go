// The protected deployment-resource shape uses a scalar authority template.
package server

import "testing"

// Keep this independent of the list control: the old parser must reject this
// exact supported shape even when every raw YAML field already is a string.
func TestPgOwnershipResourceResolvesScalarAuthorityTemplate(t *testing.T) {
	t.Setenv("SYNTHETIC_OWNERSHIP_AUTHORITY", "synthetic-pg.example:5432")
	defer Vault.PushSimpleResource(MaintenancePgVaultResourceName, []byte(
		"authority: '{{ env:SYNTHETIC_OWNERSHIP_AUTHORITY }}'\ndb: synthetic\nuser: synthetic\n"))()
	want := pgOwnershipResource{host: "synthetic-pg.example", port: 5432, database: "synthetic", user: "synthetic"}
	if got := requirePgOwnershipResource(); got != want {
		t.Fatal("scalar authority template differs from the direct pool's resolved endpoint")
	}
}
