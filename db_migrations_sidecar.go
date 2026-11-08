package server

import (
	"sync"
)

// The sidecar's migration groups (§19.7, D-OPS-6). A group is pending until its
// own milestone's Gn-out moves it into the migrations slice, and until then it
// lives in db_migrations_sidecar_pending.go under build tag sidecar_pending.
// local and canary set the tag; a release leg never does, so it links main's
// migrations alone. The group is appended in the slice's initializer, because
// an init-time append would escape the catalog's alias of the slice
// (db_migration_catalog.go).
//
// The catalog keys identities by slice position, so a pending group's versions
// move whenever main appends a migration: a database that applied one is
// rebuilt after each merge. A registered group never changes.
//
// Code that reads a group's tables stays off unless this binary links the
// group, which each group's marker reports. Then the binary's migration head
// includes the group, so without the tables the binary never reports ready
// (router.CheckStartupReadiness). The marker travels with the group, so a Gn-out
// that moves the group turns its code on and needs no other edit.

// The code migration that ends MG0 and marks it linked.
const sidecarMg0MigrationId = "sidecar_mg0"

var sidecarMg0Linked = sync.OnceValue(func() bool {
	return 0 < CodeMigrationVersion(sidecarMg0MigrationId)
})

// Whether this binary links MG0, §7's P0: network_client_owner, and the altered
// account_api_key and auth_code. Nothing may read or write them while it is
// false.
func SidecarMg0Linked() bool {
	return sidecarMg0Linked()
}

// The code migration that ends MG1 and marks it linked.
const sidecarMg1MigrationId = "sidecar_mg1"

var sidecarMg1Linked = sync.OnceValue(func() bool {
	return 0 < CodeMigrationVersion(sidecarMg1MigrationId)
})

// Whether this binary links MG1: §7's network_auth_key and network_admin_event,
// and what M1's other tasks add to the group. Nothing may read or write its
// tables while it is false.
func SidecarMg1Linked() bool {
	return sidecarMg1Linked()
}

// Whether a binary that links a pending group may run. A pending group's
// versions are not final, so it must never reach a database that outlives the
// next merge. An env of "" is a binary with no WARP_ENV. Evaluation credential
// overrides refuse in the same way (localEvaluationCredential).
func sidecarPendingAllowed(env string, testBinary bool) bool {
	return testBinary || env == "local" || env == "canary"
}
