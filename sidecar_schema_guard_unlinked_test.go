//go:build !sidecar_pending

package server

// Without sidecar_pending no pending group is linked, so the schema guard has no group SQL to read.
func sidecarSchemaGuardMigrations() []any {
	return nil
}
