//go:build sidecar_pending

package server

import (
	"context"
	"fmt"
	"slices"
	"testing"
)

// The migration groups waiting for their own Gn-out (db_migrations_sidecar.go).
// A Gn-out moves only its own group from here into the migrations slice, in
// order, with server/local/sidecar-migrations promote. Every group is additive
// and compatible with the running release (connect/CODESTYLE.md, "Database
// migrations and deploy order"): it adds tables, nullable columns and indexes,
// and the release that is running when it applies neither reads nor writes any
// of them. TestSidecarGroupsAreAdditive holds each group to that.
//
// Each group concatenates its owners' parts in §19.7's order, then its marker.
// An owner adds its migrations to its own part, inside that part's literal, so
// two tracks adding to one group touch different lines. A part's entries are
// moved verbatim at promotion, so a part holds only migration values and calls
// that the migrations slice can make too: no helper declared in this file.

// Panics unless the binary may link a pending group (sidecarPendingAllowed). It
// runs in the migrations slice's initializer, so a release leg built with the
// tag by mistake never gets as far as main.
func sidecarPendingMigrations() []any {
	env, _ := Env()
	if !sidecarPendingAllowed(env, testing.Testing()) {
		panic(fmt.Errorf("the sidecar_pending build tag links unregistered migrations and runs only in local, canary or tests, not WARP_ENV=%q", env))
	}
	return slices.Concat(
		sidecarMg0Migrations(),
		sidecarMg1Migrations(),
		sidecarMg2Migrations(),
		sidecarMg5Migrations(),
		sidecarMg6Migrations(),
		sidecarMg7Migrations(),
		sidecarMg8Migrations(),
		sidecarMg9Migrations(),
	)
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

// MG1 (§19.7): what M1 first reads ungated.
func sidecarMg1Migrations() []any {
	return slices.Concat(
		sidecarMg1CoreMigrations(),
		sidecarMg1TeamMigrations(),
		sidecarMg1LeaseMigrations(),
		sidecarMg1QuotaMigrations(),
		[]any{
			// the group's marker (SidecarMg1Linked); it changes nothing
			newCodeMigration(sidecarMg1MigrationId, func(ctx context.Context) {}),
		},
	)
}

// MG1's §5 part: sidecar_identity and sidecar_identity_binding (CORE-M1-06).
func sidecarMg1CoreMigrations() []any {
	return []any{}
}

// MG1's §7 part: auth keys, the admin event log and admin_token_use (TEAM's
// tasks).
func sidecarMg1TeamMigrations() []any {
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
	}
}

// MG1's §18 part: network_client_lease (TEAM-M1-10).
func sidecarMg1LeaseMigrations() []any {
	return []any{}
}

// MG1's §20 part: network_sidecar_limit (QUOTA's tasks).
func sidecarMg1QuotaMigrations() []any {
	return []any{}
}

// MG2 (§19.7): what M2 first reads ungated.
func sidecarMg2Migrations() []any {
	return slices.Concat(
		sidecarMg2AclMigrations(),
		sidecarMg2QuotaMigrations(),
		[]any{
			// the group's marker (SidecarMg2Linked); it changes nothing
			newCodeMigration(sidecarMg2MigrationId, func(ctx context.Context) {}),
		},
	)
}

// MG2's §8 part: network_acl_head, network_acl_ack and
// network_client.acl_subject_client_id, which turns NOT NULL only through a
// validated NOT VALID check (ACL's tasks).
func sidecarMg2AclMigrations() []any {
	return []any{}
}

// MG2's §20 part: network_quota_state, network_service_usage,
// service_debit_journal and service_usage_batch (QUOTA's tasks).
func sidecarMg2QuotaMigrations() []any {
	return []any{}
}

// MG5 (§19.7): what M5 first reads ungated. MG3 and MG4 create nothing.
func sidecarMg5Migrations() []any {
	return slices.Concat(
		sidecarMg5DnsMigrations(),
		sidecarMg5TlsMigrations(),
		sidecarMg5ApprovalMigrations(),
		sidecarMg5AbuseMigrations(),
		[]any{
			// the group's marker (SidecarMg5Linked); it changes nothing
			newCodeMigration(sidecarMg5MigrationId, func(ctx context.Context) {}),
		},
	)
}

// MG5's §12 part: dns_* and network_dns_* (DNS's tasks).
func sidecarMg5DnsMigrations() []any {
	return []any{}
}

// MG5's §13 part: network_tls_* and tls_connection_*, the hourly partitioned
// record among them (TLS-M5-12).
func sidecarMg5TlsMigrations() []any {
	return []any{}
}

// MG5's §16 part: approval_use, §12.8's DNS gate (MCP's tasks).
func sidecarMg5ApprovalMigrations() []any {
	return []any{}
}

// MG5's §21 part: the seven abuse tables (ABUSE-M5-03).
func sidecarMg5AbuseMigrations() []any {
	return []any{}
}

// MG6 (§19.7): what M6 first reads ungated.
func sidecarMg6Migrations() []any {
	return slices.Concat(
		sidecarMg6TeamMigrations(),
		sidecarMg6AclMigrations(),
		sidecarMg6BridgeMigrations(),
		sidecarMg6QuotaMigrations(),
		[]any{
			// the group's marker (SidecarMg6Linked); it changes nothing
			newCodeMigration(sidecarMg6MigrationId, func(ctx context.Context) {}),
		},
	)
}

// MG6's §7 part: the Team tables (TEAM's tasks).
func sidecarMg6TeamMigrations() []any {
	return []any{}
}

// MG6's §8 part: network_acl_share (ACL's tasks).
func sidecarMg6AclMigrations() []any {
	return []any{}
}

// MG6's §17 part: network_bridge_share (BRIDGE's tasks).
func sidecarMg6BridgeMigrations() []any {
	return []any{}
}

// MG6's §20 part: transfer_balance.tier, transfer_balance's team_credit index,
// built concurrently, and stripe_meter_report (QUOTA's tasks).
func sidecarMg6QuotaMigrations() []any {
	return []any{}
}

// MG7 (§19.7): what M7 first reads ungated.
func sidecarMg7Migrations() []any {
	return slices.Concat(
		sidecarMg7MailMigrations(),
		[]any{
			// the group's marker (SidecarMg7Linked); it changes nothing
			newCodeMigration(sidecarMg7MigrationId, func(ctx context.Context) {}),
		},
	)
}

// MG7's §15 part: mail_network, mail_host_key, mail_direct_mx and the hourly
// partitioned mail_connection_record (MAIL-M7-21).
func sidecarMg7MailMigrations() []any {
	return []any{}
}

// MG8 (§19.7): what M8 first reads ungated.
func sidecarMg8Migrations() []any {
	return slices.Concat(
		sidecarMg8PushMigrations(),
		[]any{
			// the group's marker (SidecarMg8Linked); it changes nothing
			newCodeMigration(sidecarMg8MigrationId, func(ctx context.Context) {}),
		},
	)
}

// MG8's §18 part: sidecar_push_token (SDK's tasks).
func sidecarMg8PushMigrations() []any {
	return []any{}
}

// MG9 (§19.7): what M9 first reads ungated.
func sidecarMg9Migrations() []any {
	return slices.Concat(
		sidecarMg9SmtpMigrations(),
		[]any{
			// the group's marker (SidecarMg9Linked); it changes nothing
			newCodeMigration(sidecarMg9MigrationId, func(ctx context.Context) {}),
		},
	)
}

// MG9's §14 part: smtp_*, smtp_send partitioned daily (SMTP-M9-17).
func sidecarMg9SmtpMigrations() []any {
	return []any{}
}
