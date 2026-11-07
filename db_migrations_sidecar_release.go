//go:build !sidecar_pending

package server

// A release leg links main's migrations alone.
func sidecarPendingMigrations() []any {
	return nil
}
