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
// move whenever main appends a migration, or a part ahead of it in the pending
// groups grows: a database that applied one is rebuilt after each merge. A
// registered group never changes.
//
// Each group is one function that concatenates its owners' parts in §19.7's
// order and ends with the group's marker, a code migration that changes
// nothing. Each owner adds its tables to its own part, so tracks that add to
// one group merge without touching each other's lines. MG3 and MG4 create
// nothing (§19.7), so they have no function and no marker.
//
// Code that reads a group's tables stays off unless this binary links the
// group, which each group's marker reports. Then the binary's migration head
// includes the group, so without the tables the binary never reports ready
// (router.CheckStartupReadiness). The marker travels with the group, so a Gn-out
// that moves the group with server/local/sidecar-migrations turns its code on
// and needs no other edit.

// The code migrations that end each group and mark it linked.
const (
	sidecarMg0MigrationId = "sidecar_mg0"
	sidecarMg1MigrationId = "sidecar_mg1"
	sidecarMg2MigrationId = "sidecar_mg2"
	sidecarMg5MigrationId = "sidecar_mg5"
	sidecarMg6MigrationId = "sidecar_mg6"
	sidecarMg7MigrationId = "sidecar_mg7"
	sidecarMg8MigrationId = "sidecar_mg8"
	sidecarMg9MigrationId = "sidecar_mg9"
)

// Whether this binary links the group that the code migration id ends, asked once
// and only after the migrations slice is built.
func sidecarGroupLinked(id string) func() bool {
	return sync.OnceValue(func() bool {
		return 0 < CodeMigrationVersion(id)
	})
}

var (
	sidecarMg0Linked = sidecarGroupLinked(sidecarMg0MigrationId)
	sidecarMg1Linked = sidecarGroupLinked(sidecarMg1MigrationId)
	sidecarMg2Linked = sidecarGroupLinked(sidecarMg2MigrationId)
	sidecarMg5Linked = sidecarGroupLinked(sidecarMg5MigrationId)
	sidecarMg6Linked = sidecarGroupLinked(sidecarMg6MigrationId)
	sidecarMg7Linked = sidecarGroupLinked(sidecarMg7MigrationId)
	sidecarMg8Linked = sidecarGroupLinked(sidecarMg8MigrationId)
	sidecarMg9Linked = sidecarGroupLinked(sidecarMg9MigrationId)
)

// Whether this binary links MG0, §7's P0: network_client_owner, and the altered
// account_api_key and auth_code. Nothing may read or write them while it is
// false.
func SidecarMg0Linked() bool {
	return sidecarMg0Linked()
}

// Whether this binary links MG1: §5's identity tables, §7's auth keys, admin
// event log and admin token uses, §18's lease and §20's limit overrides. Nothing
// may read or write its tables while it is false.
func SidecarMg1Linked() bool {
	return sidecarMg1Linked()
}

// Whether this binary links MG2: §8's ACL head and acks and
// network_client.acl_subject_client_id, and §20's quota state, usage, debit
// journal and usage batches.
func SidecarMg2Linked() bool {
	return sidecarMg2Linked()
}

// Whether this binary links MG5: §12's dns tables, §13's tls tables, §16's
// approval_use and §21's seven abuse tables.
func SidecarMg5Linked() bool {
	return sidecarMg5Linked()
}

// Whether this binary links MG6: §7's Team tables, §8's network_acl_share, §17's
// network_bridge_share, and §20's tier, team_credit index and meter reports.
func SidecarMg6Linked() bool {
	return sidecarMg6Linked()
}

// Whether this binary links MG7: §15's mail tables.
func SidecarMg7Linked() bool {
	return sidecarMg7Linked()
}

// Whether this binary links MG8: §18's sidecar_push_token.
func SidecarMg8Linked() bool {
	return sidecarMg8Linked()
}

// Whether this binary links MG9: §14's smtp tables.
func SidecarMg9Linked() bool {
	return sidecarMg9Linked()
}

// Whether a binary that links a pending group may run. A pending group's
// versions are not final, so it must never reach a database that outlives the
// next merge. An env of "" is a binary with no WARP_ENV. Evaluation credential
// overrides refuse in the same way (localEvaluationCredential).
func sidecarPendingAllowed(env string, testBinary bool) bool {
	return testBinary || env == "local" || env == "canary"
}
