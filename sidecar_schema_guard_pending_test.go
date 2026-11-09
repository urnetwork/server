//go:build sidecar_pending

package server

// The groups the schema guard reads the SQL of: every pending group, which this build links.
func sidecarSchemaGuardMigrations() []any {
	return sidecarPendingMigrations()
}
