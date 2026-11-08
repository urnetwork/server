//go:build sidecar_pending

package server

import (
	"context"
	"fmt"
	"testing"
)

// The migration groups waiting for their own Gn-out (db_migrations_sidecar.go).
// A Gn-out moves only its own group from here into the migrations slice, in
// order. Every group is additive and compatible with the running release
// (connect/CODESTYLE.md, "Database migrations and deploy order"): it adds
// tables, nullable columns and indexes, and the release that is running when it
// applies neither reads nor writes any of them.

// Panics unless the binary may link a pending group (sidecarPendingAllowed). It
// runs in the migrations slice's initializer, so a release leg built with the
// tag by mistake never gets as far as main.
func sidecarPendingMigrations() []any {
	env, _ := Env()
	if !sidecarPendingAllowed(env, testing.Testing()) {
		panic(fmt.Errorf("the sidecar_pending build tag links unregistered migrations and runs only in local, canary or tests, not WARP_ENV=%q", env))
	}
	return append(sidecarMg0Migrations(), sidecarMg1Migrations()...)
}

// MG0, §7's P0 (§19.7): the owner row that marks a client, and the columns that
// carry an API key's id onto the auth codes it creates.
func sidecarMg0Migrations() []any {
	return []any{
		// Set at mint and deleted with its client (§7.7). The marks admin_refused
		// and strict_exp never change. A client with no row is a legacy client
		// (§7.4). member_id, sponsor_user_id and auth_key_id name rows of tables
		// later groups create, so nothing references them yet.
		// acl_subject_client_id is not here: D-ACL-3 and MG2 put it on
		// network_client.
		newSqlMigration(`
			CREATE TABLE network_client_owner (
				client_id uuid NOT NULL,
				owner_user_id uuid NULL,
				member_id uuid NULL,
				sponsor_user_id uuid NULL,
				auth_key_id uuid NULL,
				tagged bool NOT NULL DEFAULT false,
				ephemeral bool NOT NULL DEFAULT false,
				admin_refused bool NOT NULL DEFAULT false,
				strict_exp bool NOT NULL DEFAULT false,
				acl_enforcing bool NOT NULL DEFAULT false,
				declares_no_egress bool NOT NULL DEFAULT false,

				PRIMARY KEY (client_id)
			)
		`),

		// §7.8: an API key gains scopes, expiry and creator. NULL in each is a
		// legacy key, which keeps its creator's whole authority and never expires.
		// Catalog-only changes, but every API key request reads this table and queues
		// behind ADD COLUMN's lock, so its wait is bounded. A timeout fails
		// `db migrate` with nothing applied, and running it again resumes here.
		newSqlMigration(`
			SET LOCAL lock_timeout = '2s';
			ALTER TABLE account_api_key
				ADD COLUMN scopes jsonb NULL,
				ADD COLUMN expire_time timestamp NULL,
				ADD COLUMN creator_user_id uuid NULL
		`),

		// §7.4: the key a code descends from, NULL for a code a key had no part in.
		// Bounded as above.
		newSqlMigration(`
			SET LOCAL lock_timeout = '2s';
			ALTER TABLE auth_code ADD COLUMN api_key_id uuid NULL
		`),
		// removing a key deletes its codes (§2.8 api-11)
		newRestartableOnlineSqlMigration(
			`DROP INDEX CONCURRENTLY IF EXISTS auth_code_api_key_id`,
			`CREATE INDEX CONCURRENTLY auth_code_api_key_id ON auth_code (api_key_id) WHERE api_key_id IS NOT NULL`,
			`CREATE INDEX auth_code_api_key_id ON auth_code (api_key_id) WHERE api_key_id IS NOT NULL`,
		),

		// the group's marker (SidecarMg0Linked); it changes nothing
		newCodeMigration(sidecarMg0MigrationId, func(ctx context.Context) {}),
	}
}

// MG1 (§19.7): what M1 first reads ungated. §7's auth keys and admin event log
// are here; §5's identity tables, §7's admin_token_use, §18's lease and §20's
// limit join the group in their own tasks' changes, each before the marker.
func sidecarMg1Migrations() []any {
	return []any{
		// §7.8's auth keys (§2.8 api-11): the bearer ur-authkey-<id>-<secret> is
		// stored as the SHA-256 of its secret, never in the clear. tag_ids holds
		// the opaque ids of §7.9's tags, never names. A row goes when its key is
		// revoked, or once it expires (the hourly account expiry), so it lives at
		// most the 90 days a key may.
		newSqlMigration(`
			CREATE TABLE network_auth_key (
				auth_key_id uuid NOT NULL,
				network_id uuid NOT NULL,
				secret_sha256 bytea NOT NULL,
				creator_user_id uuid NOT NULL,
				tag_ids bytea[] NOT NULL DEFAULT '{}',
				create_time timestamp NOT NULL,
				expire_time timestamp NOT NULL,
				max_uses integer NOT NULL,
				use_count integer NOT NULL DEFAULT 0,
				ephemeral bool NOT NULL DEFAULT false,
				pre_approved bool NOT NULL DEFAULT false,

				PRIMARY KEY (auth_key_id)
			)
		`),
		newSqlMigration(`
			CREATE INDEX network_auth_key_network_id ON network_auth_key (network_id, auth_key_id)
		`),
		newSqlMigration(`
			CREATE INDEX network_auth_key_expire_time ON network_auth_key (expire_time)
		`),

		// §7.11's admin event log (§2.8 api-8): one hash chain per network, seq
		// and the chain assigned under a per-network lock, with opaque ids only
		// and no client address. Kept 400 days by the taskworker's sweep.
		newSqlMigration(`
			CREATE TABLE network_admin_event (
				network_id uuid NOT NULL,
				seq bigint NOT NULL,
				event_time timestamp NOT NULL,
				actor_kind varchar(16) NOT NULL,
				actor_id uuid NULL,
				action varchar(64) NOT NULL,
				target_kind varchar(16) NOT NULL,
				target_id uuid NULL,
				result varchar(32) NOT NULL,
				request_id uuid NULL,
				prev_sha256 bytea NOT NULL,
				chain_sha256 bytea NOT NULL,

				PRIMARY KEY (network_id, seq)
			)
		`),
		newSqlMigration(`
			CREATE INDEX network_admin_event_event_time ON network_admin_event (event_time)
		`),

		// the group's marker (SidecarMg1Linked); it changes nothing
		newCodeMigration(sidecarMg1MigrationId, func(ctx context.Context) {}),
	}
}
