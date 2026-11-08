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
	return []any{
		// §5.15's durable identity (§2.8 api-5): one row per sidecar_id, never
		// reaped, deleted only with its network. No foreign key to
		// network_client, whose rows the reaper removes. status is active or
		// revoked, a candidate a client verifies against the config (§5.6).
		newSqlMigration(`
			CREATE TABLE sidecar_identity (
				sidecar_id uuid NOT NULL,
				network_id uuid NOT NULL,
				identity_public_key bytea NOT NULL,
				identity_alg varchar(16) NOT NULL,
				status varchar(16) NOT NULL,
				create_time timestamp NOT NULL,
				revoke_time timestamp NULL,

				PRIMARY KEY (sidecar_id)
			)
		`),
		// a network's deletion finds its identities here
		newSqlMigration(`
			CREATE INDEX sidecar_identity_network_id ON sidecar_identity (network_id, sidecar_id)
		`),
		// §5.15's append-only bindings: one row per client generation, the
		// wrapper's bytes kept verbatim in signatures, since the other columns
		// cannot rebuild them (plan-CORE-binding-wire #1). Never reaped.
		newSqlMigration(`
			CREATE TABLE sidecar_identity_binding (
				sidecar_id uuid NOT NULL,
				generation bigint NOT NULL,
				client_id uuid NOT NULL,
				client_public_key bytea NOT NULL,
				signatures bytea NOT NULL,
				create_time timestamp NOT NULL,

				PRIMARY KEY (sidecar_id, generation)
			)
		`),
		// a client binds once, whatever its sidecar_id (plan-CORE-binding-wire #2)
		newSqlMigration(`
			CREATE UNIQUE INDEX sidecar_identity_binding_client_id ON sidecar_identity_binding (client_id)
		`),
	}
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
	return []any{
		// §8.17's head (§2.8 api-7): each network with a config repo, its newest
		// verified log position n and chain digest A_n, and whether its rows
		// hold a document. api's append path and §19.10's restore raise it and
		// nothing lowers it; it goes with its network. A network with no row
		// costs the contract gate one map miss (R-ACL-8).
		newSqlMigration(`
			CREATE TABLE network_acl_head (
				network_id uuid NOT NULL,
				version bigint NOT NULL,
				chain bytea NOT NULL,
				has_document bool NOT NULL DEFAULT false,

				PRIMARY KEY (network_id)
			)
		`),
		// §8.9's acks (§2.8 api-15): the newest projection version each client
		// applied and when, one row per client however chatty its network,
		// kept 7 days by the taskworker's sweep and in no dump (§19.6).
		newSqlMigration(`
			CREATE TABLE network_acl_ack (
				client_id uuid NOT NULL,
				version bigint NOT NULL,
				ack_time timestamp NOT NULL,

				PRIMARY KEY (client_id)
			)
		`),
		newSqlMigration(`
			CREATE INDEX network_acl_ack_ack_time ON network_acl_ack (ack_time)
		`),
		// D-ACL-3: each client's subject, written once at creation in the
		// insert's own statement. Nullable until a backfill and a validated NOT
		// VALID check (§19.7), and until then NULL means itself for a top-level
		// client, else its source client's subject. Catalog-only, but every
		// client request reads this table and queues behind ADD COLUMN's lock,
		// so its wait is bounded as MG0's are.
		newSqlMigration(`
			SET LOCAL lock_timeout = '2s';
			ALTER TABLE network_client ADD COLUMN acl_subject_client_id uuid NULL
		`),
	}
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
// record among them (TLS-M5-12). Every column is one of §2.8's cert-1, tls-3
// or tls-4 (I-18); states and reasons are varchar, never ENUM. No column holds
// a client address (I-13): a visitor shows only as tls-3's keyed hash. Nothing
// reads these tables unless the binary links MG5 (server.SidecarMg5Linked).
func sidecarMg5TlsMigrations() []any {
	return []any{
		// §13.8's orders and certificates (cert-1), written before an order
		// reaches a CA and kept 30 days past expiry or revocation. An order's
		// time is its cert_id's, which certs mints. ca, account_uri and profile
		// are set when an order reaches a CA; serial and the validity when it
		// issues; the ARI window when the CA gives one. successor_cert_id names
		// the row that took over. chain holds a Team custom domain's registered
		// chain (v1), each certificate's DER, leaf first, and stays NULL on any
		// other row. csr is NULL only on a registered chain no request
		// preceded.
		newSqlMigration(`
			CREATE TABLE network_tls_cert (
				cert_id uuid NOT NULL,
				network_id uuid NOT NULL,
				client_id uuid NOT NULL,
				sidecar_id uuid NOT NULL,
				identifier_set_sha256 bytea NOT NULL,
				spki_sha256 bytea NOT NULL,
				attestation bytea NOT NULL,
				csr bytea NULL,
				ca varchar(32) NULL,
				account_uri varchar(1024) NULL,
				profile varchar(64) NULL,
				serial varchar(64) NULL,
				not_before timestamp NULL,
				not_after timestamp NULL,
				ari_window_start timestamp NULL,
				ari_window_end timestamp NULL,
				state varchar(16) NOT NULL,
				successor_cert_id uuid NULL,
				chain bytea[] NULL,

				PRIMARY KEY (cert_id)
			)
		`),
		// GET /tls/certs: one network's rows
		newSqlMigration(`
			CREATE INDEX network_tls_cert_network_id ON network_tls_cert (network_id, cert_id)
		`),
		// a set's history: §13.8's 5 per 7 days, and the new-set test
		newSqlMigration(`
			CREATE INDEX network_tls_cert_identifier_set ON network_tls_cert (identifier_set_sha256, cert_id)
		`),
		// certs' queue and the rows its chains still owe a step
		newSqlMigration(`
			CREATE INDEX network_tls_cert_active ON network_tls_cert (state, cert_id) WHERE state IN ('queued', 'ordering', 'waiting_dns', 'revoking')
		`),
		// the revocation sweep's removed and revoked sidecars (I-15)
		newSqlMigration(`
			CREATE INDEX network_tls_cert_sidecar_id ON network_tls_cert (sidecar_id, cert_id)
		`),
		// expiry and the 30-day purge
		newSqlMigration(`
			CREATE INDEX network_tls_cert_not_after ON network_tls_cert (not_after) WHERE not_after IS NOT NULL
		`),
		// a replaced certificate, by its serial within its network
		newSqlMigration(`
			CREATE INDEX network_tls_cert_serial ON network_tls_cert (network_id, serial) WHERE serial IS NOT NULL
		`),
		// a CA's budget, rebuilt from its rows when its Redis key is lost
		newSqlMigration(`
			CREATE INDEX network_tls_cert_ca ON network_tls_cert (ca, cert_id) WHERE ca IS NOT NULL
		`),

		// §13.9's kill switch (tls-4): a host of the network, or * for all of
		// them, with its reason and until_time, NULL while its case holds it.
		// The row goes with its keys when the case lifts it or until_time
		// passes.
		newSqlMigration(`
			CREATE TABLE network_tls_suspension (
				network_id uuid NOT NULL,
				host varchar(253) NOT NULL,
				reason varchar(256) NOT NULL,
				until_time timestamp NULL,

				PRIMARY KEY (network_id, host)
			)
		`),
		// the sweep of kills whose until_time passed
		newSqlMigration(`
			CREATE INDEX network_tls_suspension_until_time ON network_tls_suspension (until_time) WHERE until_time IS NOT NULL
		`),

		// tlsedge's connection records (tls-3), in hourly partitions by
		// end_time, which the partition registry creates ahead and retires at
		// 24 h once any §21.9 hold has copied its network's rows
		// (model/tls_connection_model.go). A connection's record is written as
		// it ends, so a connection that lasted days still lands in a partition
		// that exists. host is the published host within the network, @ or a
		// label; visitor_hash is the 16-byte keyed hash of the visitor's /24 or
		// /48 under the day's key, never an address. The primary key makes a
		// batch's retry insert nothing twice, and serves the abuse join by
		// connection_id.
		newSqlMigration(`
			CREATE TABLE tls_connection_record (
				connection_id uuid NOT NULL,
				network_id uuid NOT NULL,
				host varchar(253) NOT NULL,
				start_time timestamp NOT NULL,
				end_time timestamp NOT NULL,
				byte_count bigint NOT NULL,
				visitor_hash bytea NOT NULL,

				PRIMARY KEY (connection_id, end_time)
			) PARTITION BY RANGE (end_time)
		`),
		// one network's rows: the roll-up, and a hold's copy
		newSqlMigration(`
			CREATE INDEX tls_connection_record_network_id ON tls_connection_record (network_id, end_time)
		`),
		// daily counts per network and host (tls-3), kept 13 months: each hour
		// is added in the transaction that retires it
		newSqlMigration(`
			CREATE TABLE tls_connection_day (
				network_id uuid NOT NULL,
				host varchar(253) NOT NULL,
				day date NOT NULL,
				connection_count bigint NOT NULL,
				byte_count bigint NOT NULL,

				PRIMARY KEY (network_id, host, day)
			)
		`),
		// the 13-month retention cursor
		newSqlMigration(`
			CREATE INDEX tls_connection_day_day ON tls_connection_day (day)
		`),
	}
}

// MG5's §16 part: approval_use, §12.8's DNS gate (MCP's tasks).
func sidecarMg5ApprovalMigrations() []any {
	return []any{}
}

// MG5's §21 part: the seven abuse tables (ABUSE-M5-03), all of §2.8 api-14 and
// exactly the seven §21.16 names. Kinds, states and classes are varchar, never
// ENUM, so a new value is a code change and no migration. No column holds a
// client address, a URL path, report text or a reporter's address (I-13), and a
// reviewer, signer or user is an opaque id. Indexes serve lookups by case id
// and join key, one network's own rows, review clocks and retention cursors;
// none serves a read across networks. Nothing reads these tables unless the
// binary links MG5 (server.SidecarMg5Linked).
func sidecarMg5AbuseMigrations() []any {
	return []any{
		// §21.8's cases: one per report target, a later report on the same
		// network, target and category coalescing into the open one. network_id
		// is NULL until a join names a network. barred marks a sealing order or
		// a CSAM case, which tells the network nothing (§21.8). Kept 13 months
		// after the case last closes (plan-ABUSE-small-readings #9).
		newSqlMigration(`
			CREATE TABLE abuse_case (
				case_id uuid NOT NULL,
				network_id uuid NULL,
				category varchar(32) NOT NULL,
				target_kind varchar(16) NOT NULL,
				target varchar(256) NOT NULL,
				reporter_class varchar(32) NOT NULL,
				join_kind varchar(16) NOT NULL,
				join_key varchar(256) NOT NULL,
				state varchar(16) NOT NULL,
				barred bool NOT NULL DEFAULT false,
				review_due_time timestamp NULL,
				create_time timestamp NOT NULL,
				close_time timestamp NULL,

				PRIMARY KEY (case_id)
			)
		`),
		// the abuse role's join-key lookup (§21.10)
		newSqlMigration(`
			CREATE INDEX abuse_case_join ON abuse_case (join_kind, join_key, case_id)
		`),
		// one network's open cases: coalescing a report, and DNS-M5-15's
		// open-case check
		newSqlMigration(`
			CREATE INDEX abuse_case_network_open ON abuse_case (network_id, target, category) WHERE state = 'open'
		`),
		// §21.8's review clocks
		newSqlMigration(`
			CREATE INDEX abuse_case_review_due_time ON abuse_case (review_due_time, case_id) WHERE state = 'open'
		`),
		// the 13-month retention cursor
		newSqlMigration(`
			CREATE INDEX abuse_case_close_time ON abuse_case (close_time, case_id) WHERE close_time IS NOT NULL
		`),

		// §21.4's actions and lifts, each under its caller's action_id, which a
		// restore re-applies idempotently (§21.10). A lift is its own row, kind
		// lift, target the lifted action's id (plan-ABUSE-small-readings #11).
		// network_id is NULL for a platform-wide breaker level
		// (plan-ABUSE-metadata-rows #5). applied_time and mailed_time mark the
		// effect and the off-site record after commit.
		newSqlMigration(`
			CREATE TABLE abuse_action (
				action_id uuid NOT NULL,
				case_id uuid NOT NULL,
				network_id uuid NULL,
				category varchar(32) NOT NULL,
				kind varchar(32) NOT NULL,
				target varchar(256) NOT NULL,
				until_time timestamp NULL,
				by_machine bool NOT NULL,
				review_state varchar(16) NOT NULL,
				reviewer_id uuid NULL,
				review_reason varchar(2048) NULL,
				review_time timestamp NULL,
				applied_time timestamp NULL,
				mailed_time timestamp NULL,
				create_time timestamp NOT NULL,

				PRIMARY KEY (action_id)
			)
		`),
		newSqlMigration(`
			CREATE INDEX abuse_action_case_id ON abuse_action (case_id, action_id)
		`),
		// one network's history, which §21.4's ladders and §21.7's standing read
		newSqlMigration(`
			CREATE INDEX abuse_action_network_id ON abuse_action (network_id, create_time) WHERE network_id IS NOT NULL
		`),
		// the action pass's cursor over rows whose effect or record is owed
		newSqlMigration(`
			CREATE INDEX abuse_action_owed ON abuse_action (create_time, action_id) WHERE applied_time IS NULL OR mailed_time IS NULL
		`),

		// §21.8's appeals: an Owner or Admin files, a second reviewer decides
		// with reasons. Of free text only the appeal's own reason is kept.
		newSqlMigration(`
			CREATE TABLE abuse_appeal (
				appeal_id uuid NOT NULL,
				case_id uuid NOT NULL,
				network_id uuid NOT NULL,
				user_id uuid NOT NULL,
				reason varchar(2048) NOT NULL,
				state varchar(16) NOT NULL,
				reviewer_id uuid NULL,
				decision_reason varchar(2048) NULL,
				create_time timestamp NOT NULL,
				decide_time timestamp NULL,

				PRIMARY KEY (appeal_id)
			)
		`),
		newSqlMigration(`
			CREATE INDEX abuse_appeal_case_id ON abuse_appeal (case_id, appeal_id)
		`),
		// the appeals waiting for a reviewer
		newSqlMigration(`
			CREATE INDEX abuse_appeal_pending ON abuse_appeal (create_time, appeal_id) WHERE decide_time IS NULL
		`),

		// §21.7's standing, a row only while restricted or suspended
		// (plan-ABUSE-small-readings #3), gone once no longer current.
		newSqlMigration(`
			CREATE TABLE network_abuse_state (
				network_id uuid NOT NULL,
				state varchar(16) NOT NULL,
				case_id uuid NOT NULL,
				until_time timestamp NULL,
				update_time timestamp NOT NULL,

				PRIMARY KEY (network_id)
			)
		`),
		newSqlMigration(`
			CREATE INDEX network_abuse_state_until_time ON network_abuse_state (until_time) WHERE until_time IS NOT NULL
		`),

		// §21.9's holds, mirrored to hold/ (plan-ABUSE-hold-format). Part 0 is
		// the hold itself; parts 1 and up each hold at most 16 MiB of compressed
		// copied rows (plan-ABUSE-metadata-rows #3), with the hold's own columns
		// NULL. scope is the hold object's scope value, "network" or a list of
		// repo ids. renew_time set refuses a second renewal, and report_2258a's
		// year runs from report_time.
		newSqlMigration(`
			CREATE TABLE abuse_legal_hold (
				network_id uuid NOT NULL,
				case_id uuid NOT NULL,
				part integer NOT NULL,
				kind varchar(16) NULL,
				scope jsonb NULL,
				place_time timestamp NULL,
				report_time timestamp NULL,
				renew_time timestamp NULL,
				expire_time timestamp NULL,
				copied_rows bytea NULL,
				copy_state varchar(16) NULL,

				PRIMARY KEY (network_id, case_id, part)
			)
		`),
		// the deadline cursor
		newSqlMigration(`
			CREATE INDEX abuse_legal_hold_expire_time ON abuse_legal_hold (expire_time) WHERE part = 0
		`),

		// §21.10: every abuse-role read, by case id or join key, the key itself
		// never kept. Kept 3 years.
		newSqlMigration(`
			CREATE TABLE abuse_access_log (
				log_id uuid NOT NULL,
				reviewer_id uuid NOT NULL,
				lookup_kind varchar(16) NOT NULL,
				case_id uuid NULL,
				access_time timestamp NOT NULL,

				PRIMARY KEY (log_id)
			)
		`),
		newSqlMigration(`
			CREATE INDEX abuse_access_log_access_time ON abuse_access_log (access_time, log_id)
		`),

		// §21.9's legal requests and productions, each production signed off
		// by one legal and one engineering id (§21.10). Kept 3 years.
		newSqlMigration(`
			CREATE TABLE legal_request (
				request_id uuid NOT NULL,
				case_id uuid NOT NULL,
				instrument varchar(32) NOT NULL,
				country varchar(2) NOT NULL,
				network_ids uuid[] NOT NULL DEFAULT '{}',
				sealed bool NOT NULL DEFAULT false,
				seal_until timestamp NULL,
				due_time timestamp NULL,
				production_state varchar(16) NOT NULL,
				legal_signoff_id uuid NULL,
				engineering_signoff_id uuid NULL,
				create_time timestamp NOT NULL,
				close_time timestamp NULL,

				PRIMARY KEY (request_id)
			)
		`),
		newSqlMigration(`
			CREATE INDEX legal_request_case_id ON legal_request (case_id, request_id)
		`),
		// the production clocks
		newSqlMigration(`
			CREATE INDEX legal_request_due_time ON legal_request (due_time, request_id) WHERE close_time IS NULL
		`),
		// the 3-year retention cursor
		newSqlMigration(`
			CREATE INDEX legal_request_close_time ON legal_request (close_time, request_id) WHERE close_time IS NOT NULL
		`),
	}
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
