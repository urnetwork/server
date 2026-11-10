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

		// §7.8's one-command admin tokens (§2.8 api-11): each token's jti spent
		// once, in its command's own transaction, with the command's digest and
		// the answer a retry of the same jti gets. A row goes two hours after its
		// use, which is at least an hour past its token's exp, since a token
		// lives an hour and is used within it.
		newSqlMigration(`
			CREATE TABLE admin_token_use (
				jti varchar(64) NOT NULL,
				member_id uuid NOT NULL,
				cmd varchar(64) NOT NULL,
				result bytea NULL,
				use_time timestamp NOT NULL,

				PRIMARY KEY (jti)
			)
		`),
		newSqlMigration(`
			CREATE INDEX admin_token_use_use_time ON admin_token_use (use_time)
		`),
	}
}

// MG1's §18 part: network_client_lease (TEAM-M1-10).
func sidecarMg1LeaseMigrations() []any {
	return []any{}
}

// MG1's §20 part: network_sidecar_limit (QUOTA's tasks).
func sidecarMg1QuotaMigrations() []any {
	return []any{
		// §20.5's overrides (§2.8 api-13): a row raises one of a network's
		// limits (ops) or tightens it (§21), by the limit_key QuotaLimits and
		// QuotaRefusal carry, with a reason citing the ops change or the §21
		// case. A row lives until it is lifted, and goes with its network.
		// Readers take the whole table in one snapshot (NetworkSidecarLimits),
		// so it needs no index past its key.
		newSqlMigration(`
			CREATE TABLE network_sidecar_limit (
				network_id uuid NOT NULL,
				limit_key varchar(64) NOT NULL,
				limit_value bigint NOT NULL,
				reason varchar(256) NOT NULL,

				PRIMARY KEY (network_id, limit_key)
			)
		`),
	}
}

// MG2 (§19.7): what M2 first reads ungated.
func sidecarMg2Migrations() []any {
	return slices.Concat(
		sidecarMg2AclMigrations(),
		sidecarMg2DnsMigrations(),
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

// MG2's §12 part: dns_label alone, moved from MG5 by plan-DNS-labels-before-m5
// so labels exist from M2 (DNS-M5-09, DNS-M5-10). Every column is one of §2.8
// api-2's: the opaque label or opt-in alias, its network, kind and state, when
// the state last changed, which starts a retiring name's 90 days (D-DNS-10),
// and the dns serial of its last change the label map shows (DnsLabelMapSince),
// zero until its zone exists. A row is never deleted and never reassigned:
// labels and aliases share the key (root, label) in every state, so no name a
// row holds can be claimed again (I-14). A restore's CT replay inserts names
// with no network (§12.5). kind and state are varchar, never ENUM. Nothing reads
// it unless the binary links MG2 (server.SidecarMg2Linked).
func sidecarMg2DnsMigrations() []any {
	return []any{
		newSqlMigration(`
			CREATE TABLE dns_label (
				root varchar(256) NOT NULL,
				label varchar(63) NOT NULL,
				network_id uuid NULL,
				kind varchar(16) NOT NULL,
				state varchar(16) NOT NULL,
				state_time timestamp NOT NULL,
				change_serial bigint NOT NULL DEFAULT 0,

				PRIMARY KEY (root, label)
			)
		`),
		// a network's names: its label in each root, its alias, and a deletion's
		// tombstones
		newSqlMigration(`
			CREATE INDEX dns_label_network_id ON dns_label (network_id, root, kind) WHERE network_id IS NOT NULL
		`),
		// the label map's deltas, which tlsedge reads every 5 s
		// (plan-TLS-edge-data-source)
		newSqlMigration(`
			CREATE INDEX dns_label_change_serial ON dns_label (root, change_serial)
		`),
		// a name held in any root, which a rename and an alias claim refuse
		newSqlMigration(`
			CREATE INDEX dns_label_label ON dns_label (label)
		`),
	}
}

// MG2's §20 part: network_quota_state, network_service_usage,
// service_debit_journal and service_usage_batch (QUOTA's tasks). Every column is
// one of §20.14's and §2.8 api-13's. Nothing a running release reads or writes:
// the one change to an existing table is service_debit_journal's delete guard on
// transfer_balance, which refuses nothing while the journal is empty
// (db_service_debit_journal.go).
func sidecarMg2QuotaMigrations() []any {
	return []any{
		// §20.7's stored count and §20.6's spent mark, one row per network
		// that has had either, until the network is deleted
		newSqlMigration(`
			CREATE TABLE network_quota_state (
				network_id uuid NOT NULL,
				stored_byte_count bigint NOT NULL DEFAULT 0,
				storage_over bool NOT NULL DEFAULT false,
				spent_time timestamp NULL,

				PRIMARY KEY (network_id)
			)
		`),
		// OverLimit's read every 5 s: only the networks over their storage
		// limit or spent
		newSqlMigration(`
			CREATE INDEX network_quota_state_over_limit ON network_quota_state (network_id) WHERE storage_over OR spent_time IS NOT NULL
		`),
		// D-QUOTA-8's daily usage per network and meter, quantity in the
		// meter's unit (bytes for a transfer meter), kept 13 months
		newSqlMigration(`
			CREATE TABLE network_service_usage (
				network_id uuid NOT NULL,
				usage_day date NOT NULL,
				meter varchar(32) NOT NULL,
				quantity bigint NOT NULL DEFAULT 0,

				PRIMARY KEY (network_id, usage_day, meter)
			)
		`),
		// the 13 months' retention sweeps by day
		newSqlMigration(`
			CREATE INDEX network_service_usage_usage_day ON network_service_usage (usage_day)
		`),
		// §20.6's debits without payout: a row per (balance, usage), applied
		// by the flush and deleted once its Redis token is released. No
		// network or time column; batch_id is NULL for a relayed contract's
		// row, whose usage_id is its contract_id
		newSqlMigration(`
			CREATE TABLE service_debit_journal (
				usage_id uuid NOT NULL,
				balance_id uuid NOT NULL,
				meter varchar(32) NOT NULL,
				debit_byte_count bigint NOT NULL CHECK (debit_byte_count >= 0),
				batch_id uuid NULL,
				applied bool NOT NULL DEFAULT false,

				PRIMARY KEY (balance_id, usage_id)
			)
		`),
		// the flush's 16 shards, each a page by balance key: the shard is
		// transfer_debit_journal's CHECK, computed, since this journal has no
		// shard column
		newSqlMigration(`
			CREATE INDEX service_debit_journal_shard ON service_debit_journal ((get_byte(uuid_send(balance_id), 15) % 16), balance_id, usage_id)
		`),
		// §20.6's batches applied, by id, each producer's retry identity,
		// kept 24 h
		newSqlMigration(`
			CREATE TABLE service_usage_batch (
				batch_id uuid NOT NULL,
				producer varchar(32) NOT NULL,
				window_start timestamp NOT NULL,
				applied_time timestamp NOT NULL,

				PRIMARY KEY (batch_id)
			)
		`),
		// the 24 h retention sweeps by applied time
		newSqlMigration(`
			CREATE INDEX service_usage_batch_applied_time ON service_usage_batch (applied_time)
		`),
		// a balance with journal rows is never deleted
		newSqlMigration(serviceDebitJournalGuardSql),
	}
}

// MG5 (§19.7): what M5 first reads ungated. MG3 and MG4 create nothing.
func sidecarMg5Migrations() []any {
	return slices.Concat(
		sidecarMg5DnsMigrations(),
		sidecarMg5TlsMigrations(),
		sidecarMg5SmtpMigrations(),
		sidecarMg5ApprovalMigrations(),
		sidecarMg5AbuseMigrations(),
		[]any{
			// the group's marker (SidecarMg5Linked); it changes nothing
			newCodeMigration(sidecarMg5MigrationId, func(ctx context.Context) {}),
		},
	)
}

// MG5's §12 part: dns_* and network_dns_* (DNS-M5-09), M6's custom-host tables
// included, since a registered group never changes (D-OPS-6). Every column is
// one of §2.8 dns-1's: records, their kinds and bookkeeping times, serials, the
// journal of diffs, zone suspensions, the custom-host map and each lb
// interface's report. No column holds a client address or a query (I-13), and
// kinds and states are varchar, never ENUM. A serial is a bigint (plan reading
// 4), whose low 32 bits the SOA carries. dns_label is MG2's
// (plan-DNS-labels-before-m5). dns_restore_incident, dns_ns, dns_canary_verdict
// and network_dns_host_suspension wait for §2.8's v9 rows (plan-DNS-metadata-
// rows), which §22.7's guard needs before a table is created; until then a
// restore's posted diffs ride dns_root, the restore's incident record v8 names,
// and a host suspension is a withheld owner's row in network_dns_record.
// Nothing reads these tables unless the binary links MG5
// (server.SidecarMg5Linked).
func sidecarMg5DnsMigrations() []any {
	return []any{
		// one row a root (D-DNS-1). serial is the last committed write's or
		// restore's, and only §12.6's one write path raises it, under this row's
		// lock. apex_version is <root>'s own records' version and
		// custom_host_map_version the custom-host map's (plan readings 3 and 9),
		// both serials. reload_diffs holds the diffs edges post after a reload
		// across a restore, kept for the restore step's lists, and restore_lists
		// those lists (DNS-M5-15): each changed RRset by incident, network, owner
		// and type, flagged for an open §21 case, kept 400 days.
		newSqlMigration(`
			CREATE TABLE dns_root (
				root varchar(256) NOT NULL,
				serial bigint NOT NULL DEFAULT 0,
				restore_serial bigint NOT NULL DEFAULT 0,
				restoring bool NOT NULL DEFAULT false,
				restore_incident_id uuid NULL,
				apex_version bigint NOT NULL DEFAULT 0,
				custom_host_map_version bigint NOT NULL DEFAULT 0,
				reload_diffs jsonb NOT NULL DEFAULT '[]',
				restore_lists jsonb NOT NULL DEFAULT '[]',

				PRIMARY KEY (root)
			)
		`),
		// a network's zone in a root (plan reading 2: per-network state, not a
		// DNS zone): its version, the serial of its last write (reading 3), and
		// the state §12.7's system records are a function of. A zone suspension
		// (§12.9) is its reason and until.
		newSqlMigration(`
			CREATE TABLE network_dns_zone (
				root varchar(256) NOT NULL,
				network_id uuid NOT NULL,
				version bigint NOT NULL DEFAULT 0,
				create_time timestamp NOT NULL,
				edge_wildcard bool NOT NULL DEFAULT false,
				edge_apex bool NOT NULL DEFAULT false,
				mail bool NOT NULL DEFAULT false,
				suspend_reason varchar(64) NULL,
				suspend_until timestamp NULL,

				PRIMARY KEY (root, network_id)
			)
		`),
		newSqlMigration(`
			CREATE INDEX network_dns_zone_network_id ON network_dns_zone (network_id)
		`),
		newSqlMigration(`
			CREATE INDEX network_dns_zone_suspend_until ON network_dns_zone (suspend_until) WHERE suspend_until IS NOT NULL
		`),
		// every RRset of a root, one row each (owner, type), with no network for
		// <root>'s own. rdata is presentation form, one value a record, empty on
		// copy, edge, router and withheld rows. A challenge or canary expires at
		// expire_time, and a suspension withholds a row (withheld_time) from the
		// journal and every snapshot while keeping it for Restore (§12.9).
		newSqlMigration(`
			CREATE TABLE network_dns_record (
				root varchar(256) NOT NULL,
				owner varchar(256) NOT NULL,
				rrtype integer NOT NULL,
				network_id uuid NULL,
				kind varchar(16) NOT NULL,
				ttl bigint NOT NULL,
				rdata text[] NOT NULL DEFAULT '{}',
				create_time timestamp NOT NULL,
				expire_time timestamp NULL,
				withheld_time timestamp NULL,

				PRIMARY KEY (root, owner, rrtype)
			)
		`),
		newSqlMigration(`
			CREATE INDEX network_dns_record_network_id ON network_dns_record (network_id, root) WHERE network_id IS NOT NULL
		`),
		newSqlMigration(`
			CREATE INDEX network_dns_record_expire_time ON network_dns_record (expire_time) WHERE expire_time IS NOT NULL
		`),
		// one row a serial: the write's diff as the feed carries it (wire.go's
		// DnsJournalRow), kept 7 days (§12.14). A restore's jump has no row.
		newSqlMigration(`
			CREATE TABLE dns_zone_journal (
				root varchar(256) NOT NULL,
				serial bigint NOT NULL,
				network_id uuid NULL,
				commit_time timestamp NOT NULL,
				format integer NOT NULL,
				diff jsonb NOT NULL,

				PRIMARY KEY (root, serial)
			)
		`),
		newSqlMigration(`
			CREATE INDEX dns_zone_journal_commit_time ON dns_zone_journal (commit_time)
		`),
		// a network's writes by time, for §12.7's hourly caps
		newSqlMigration(`
			CREATE INDEX dns_zone_journal_network_id ON dns_zone_journal (network_id, commit_time) WHERE network_id IS NOT NULL
		`),
		// M6's custom hosts (§12.8, §13.4): a customer's <host>.<domain>, or an
		// apex <domain>, mapped to a host owner of the network, with its state,
		// CAA mode, chain check, the map version that last changed it and when
		// every interface was current for it.
		newSqlMigration(`
			CREATE TABLE network_dns_custom_host (
				host varchar(256) NOT NULL,
				root varchar(256) NOT NULL,
				network_id uuid NOT NULL,
				owner varchar(256) NOT NULL,
				state varchar(16) NOT NULL,
				caa_mode varchar(16) NOT NULL,
				effective_caa_chain_ok bool NOT NULL DEFAULT false,
				map_version bigint NOT NULL DEFAULT 0,
				ready_time timestamp NULL,
				create_time timestamp NOT NULL,

				PRIMARY KEY (host)
			)
		`),
		newSqlMigration(`
			CREATE INDEX network_dns_custom_host_network_id ON network_dns_custom_host (network_id)
		`),
		// the custom-host map's versions, kept 7 days (§12.14)
		newSqlMigration(`
			CREATE TABLE dns_custom_host_map (
				version bigint NOT NULL,
				digest bytea NOT NULL,
				create_time timestamp NOT NULL,

				PRIMARY KEY (version)
			)
		`),
		newSqlMigration(`
			CREATE INDEX dns_custom_host_map_create_time ON dns_custom_host_map (create_time)
		`),
		// each lb interface's report: the version it loaded, its digest and nginx
		// master, when it last polled, the last version a route probe passed and
		// when, and each added host's latest probe result, kept while the
		// interface is in the lb inventory (§12.14)
		newSqlMigration(`
			CREATE TABLE dns_custom_host_map_applied (
				interface_name varchar(128) NOT NULL,
				reported_version bigint NOT NULL DEFAULT 0,
				digest bytea NULL,
				nginx_master varchar(64) NULL,
				poll_time timestamp NULL,
				probe_version bigint NOT NULL DEFAULT 0,
				probe_time timestamp NULL,
				host_probes jsonb NOT NULL DEFAULT '{}',

				PRIMARY KEY (interface_name)
			)
		`),
	}
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

// MG5's §14 part: smtp_complaint and smtp_dmarc_daily, §14.14's report tables
// (SMTP-M5-06), which plan-SMTP-report-intake-home moved here from MG9 so
// mxedge's platform block counts reports from M5; the rest of smtp_* stays in
// MG9. Every column is one of §2.8 smtp-5's: a report's reporter, type and
// arrival, its attribution fields and join result, its sealed copy, and an
// aggregate's counts per network, day, reporter, header_from and report id, for
// URnetwork's sending addresses only. No column holds a recipient, a reporter's
// address, the last-hop Received IP or any other third-party address, a body or
// report text (D-SMTP-10, I-13). Types, results and kinds are varchar, never
// ENUM. Nothing reads these tables unless the binary links MG5
// (server.SidecarMg5Linked).
func sidecarMg5SmtpMigrations() []any {
	return []any{
		// an attributed report (smtp-5), or an aggregate's sealed copy under type
		// rua or tlsrpt, which counts as no complaint (plan-SMTP-small-readings
		// #14). reporter is the verified DKIM d= of the operator that sent it,
		// NULL for role mail, whose sender is never kept. message_id_hash is
		// SHA-256 of the reported Message-ID. sealed_report is the raw report
		// under the network mail key, NULL when no key was held and cleared at
		// 30 days (owner decision 78); the row stays 13 months.
		newSqlMigration(`
			CREATE TABLE smtp_complaint (
				complaint_id uuid NOT NULL,
				network_id uuid NOT NULL,
				reporter varchar(253) NULL,
				complaint_type varchar(32) NOT NULL,
				arrival_time timestamp NOT NULL,
				domain varchar(253) NULL,
				dkim_domain varchar(253) NULL,
				dkim_selector varchar(63) NULL,
				dkim_result varchar(32) NULL,
				message_id_hash bytea NULL,
				x_ur_ref varchar(64) NULL,
				feedback_id varchar(128) NULL,
				join_result varchar(32) NULL,
				sealed_report bytea NULL,

				PRIMARY KEY (complaint_id)
			)
		`),
		// one network's reports by arrival: GET /smtp/reports?since=, and
		// §14.8's rates and halts
		newSqlMigration(`
			CREATE INDEX smtp_complaint_network_id ON smtp_complaint (network_id, arrival_time)
		`),
		// §14.8's each (network, Message-ID hash) counting once
		newSqlMigration(`
			CREATE INDEX smtp_complaint_message_id_hash ON smtp_complaint (network_id, message_id_hash) WHERE message_id_hash IS NOT NULL
		`),
		// the 13-month retention cursor
		newSqlMigration(`
			CREATE INDEX smtp_complaint_arrival_time ON smtp_complaint (arrival_time)
		`),
		// the 30-day clearing of sealed copies, whose rows leave this index as
		// they clear
		newSqlMigration(`
			CREATE INDEX smtp_complaint_sealed_arrival_time ON smtp_complaint (arrival_time) WHERE sealed_report IS NOT NULL
		`),

		// an aggregate's counts (smtp-5), DMARC RUA or TLS-RPT by kind, one row
		// per network, day, reporter, header_from (a TLS-RPT policy domain) and
		// report id, and for TLS-RPT per policy type, '' for DMARC. day is the UTC
		// day the report covers. Only rows from URnetwork's sending addresses
		// reach here, so no third-party address does. A report counts once per
		// (reporter, report_id) whatever networks its rows name (D-SMTP-23),
		// which the store holds under a transaction-scoped lock on the pair;
		// kept 13 months.
		newSqlMigration(`
			CREATE TABLE smtp_dmarc_daily (
				network_id uuid NOT NULL,
				day date NOT NULL,
				reporter varchar(253) NOT NULL,
				header_from varchar(253) NOT NULL,
				report_id varchar(1024) NOT NULL,
				policy_type varchar(16) NOT NULL,
				kind varchar(16) NOT NULL,
				arrival_time timestamp NOT NULL,
				message_count bigint NOT NULL,
				dkim_pass_count bigint NOT NULL,
				spf_pass_count bigint NOT NULL,
				quarantine_count bigint NOT NULL,
				reject_count bigint NOT NULL,
				success_count bigint NOT NULL,
				failure_count bigint NOT NULL,

				PRIMARY KEY (network_id, day, reporter, header_from, report_id, policy_type)
			)
		`),
		// a report's rows, whose existence answers InsertAggregate's once
		newSqlMigration(`
			CREATE INDEX smtp_dmarc_daily_report ON smtp_dmarc_daily (reporter, report_id)
		`),
		// the 13-month retention cursor
		newSqlMigration(`
			CREATE INDEX smtp_dmarc_daily_day ON smtp_dmarc_daily (day)
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

// MG6's §7 part: the Team tables (TEAM-M6-01 creates all nine, the later TEAM
// tasks read and write them). Every column is one §2.8's rows name: api-2's
// team settings, api-3's members, invitations, IdP configuration, verified
// domains and SCIM changes, api-1's IdP identities, api-11's OAuth clients and
// api-8's webhooks. Groups, tags, device names and admissions are never here:
// they live in the config repo (owner decision 46). Roles are §7.5's seven as
// lowercase words, mirrored from the config repo (TEAM-M6-03). Nothing reads or
// writes these tables unless the binary links MG6 (server.SidecarMg6Linked), and
// each goes with its network.
func sidecarMg6TeamMigrations() []any {
	return []any{
		// §7.5's team: a network POST /network/team-create made, kept for the
		// network's life. Its admin_user_id is a synthetic owner-of-record that
		// never signs in (D-TEAM-2); sso_required is §7.6's sign-in rule, which
		// refuses every sign-in but the team's IdP
		newSqlMigration(`
			CREATE TABLE network_team (
				network_id uuid NOT NULL,
				sso_required bool NOT NULL DEFAULT false,

				PRIMARY KEY (network_id)
			)
		`),

		// §7.5's members: member_id is new per join and rides in tokens and
		// rev/ objects, so a re-invite never revives an old token. One live
		// membership per user and network; a removal deletes the row. pending
		// is §7.6's JIT state, and reauth_deadline bounds access after an IdP
		// disables the user where no SCIM runs
		newSqlMigration(`
			CREATE TABLE network_member (
				member_id uuid NOT NULL,
				network_id uuid NOT NULL,
				user_id uuid NOT NULL,
				email varchar(256) NULL,
				role varchar(32) NOT NULL,
				pending bool NOT NULL DEFAULT false,
				reauth_deadline timestamp NULL,

				PRIMARY KEY (member_id)
			)
		`),
		newSqlMigration(`
			CREATE UNIQUE INDEX network_member_network_id_user_id ON network_member (network_id, user_id)
		`),
		// a user's networks (POST /auth/network-switch, a user's deletion)
		newSqlMigration(`
			CREATE INDEX network_member_user_id ON network_member (user_id, network_id)
		`),

		// §7.5's invitations: only the token's SHA-256 is stored, never the token
		// or the link's fragment. A row goes when accepted or expired, at most
		// 30 days on
		newSqlMigration(`
			CREATE TABLE network_invite (
				invite_id uuid NOT NULL,
				network_id uuid NOT NULL,
				token_sha256 bytea NOT NULL,
				email varchar(256) NOT NULL,
				role varchar(32) NOT NULL,
				expire_time timestamp NOT NULL,

				PRIMARY KEY (invite_id),
				UNIQUE (token_sha256)
			)
		`),
		newSqlMigration(`
			CREATE INDEX network_invite_network_id ON network_invite (network_id, invite_id)
		`),
		newSqlMigration(`
			CREATE INDEX network_invite_expire_time ON network_invite (expire_time, invite_id)
		`),

		// §7.6's OIDC IdPs: issuer, client, and how the client authenticates,
		// private_key_jwt or a secret encrypted under a vault key, never in the
		// clear
		newSqlMigration(`
			CREATE TABLE network_idp (
				idp_id uuid NOT NULL,
				network_id uuid NOT NULL,
				issuer varchar(1024) NOT NULL,
				client_id varchar(1024) NOT NULL,
				client_auth varchar(32) NOT NULL,
				client_secret_ciphertext bytea NULL,

				PRIMARY KEY (idp_id)
			)
		`),
		newSqlMigration(`
			CREATE INDEX network_idp_network_id ON network_idp (network_id, idp_id)
		`),

		// §7.6's identity is (idp_id, sub), never an email (D-TEAM-4)
		newSqlMigration(`
			CREATE TABLE network_idp_identity (
				idp_id uuid NOT NULL,
				sub varchar(255) NOT NULL,
				user_id uuid NOT NULL,

				PRIMARY KEY (idp_id, sub)
			)
		`),
		newSqlMigration(`
			CREATE INDEX network_idp_identity_user_id ON network_idp_identity (user_id, idp_id)
		`),

		// §7.6's domains: the TXT _urnetwork-challenge.<domain> value a network
		// proves, checked daily and dropped after 7 failed days (fail_time starts
		// the failing run). One network per verified domain
		newSqlMigration(`
			CREATE TABLE network_domain (
				network_id uuid NOT NULL,
				domain varchar(253) NOT NULL,
				challenge varchar(64) NOT NULL,
				verify_time timestamp NULL,
				check_time timestamp NULL,
				fail_time timestamp NULL,

				PRIMARY KEY (network_id, domain)
			)
		`),
		newSqlMigration(`
			CREATE UNIQUE INDEX network_domain_verified_domain ON network_domain (domain) WHERE verify_time IS NOT NULL
		`),

		// §7.8's OAuth clients: ur-oauth-<id> with its secret's SHA-256, never
		// the secret, until revoked. A client's scopes are cut to its creating
		// member's role (§7.5), so the creator is a member_id, which a re-join
		// never reuses
		newSqlMigration(`
			CREATE TABLE network_oauth_client (
				oauth_client_id uuid NOT NULL,
				network_id uuid NOT NULL,
				secret_sha256 bytea NOT NULL,
				scopes jsonb NOT NULL,
				creator_member_id uuid NOT NULL,

				PRIMARY KEY (oauth_client_id)
			)
		`),
		newSqlMigration(`
			CREATE INDEX network_oauth_client_network_id ON network_oauth_client (network_id, oauth_client_id)
		`),

		// §7.6's SCIM changes awaiting an admin device, kept at most 7 days from
		// create_time; detail is the change as the server normalized it
		newSqlMigration(`
			CREATE TABLE network_scim_change (
				change_id uuid NOT NULL,
				network_id uuid NOT NULL,
				member_id uuid NULL,
				kind varchar(32) NOT NULL,
				detail jsonb NULL,
				create_time timestamp NOT NULL,

				PRIMARY KEY (change_id)
			)
		`),
		newSqlMigration(`
			CREATE INDEX network_scim_change_network_id ON network_scim_change (network_id, change_id)
		`),
		newSqlMigration(`
			CREATE INDEX network_scim_change_create_time ON network_scim_change (create_time, change_id)
		`),

		// §7.11's SIEM webhooks: the endpoint and its delivery state, the admin
		// event seq delivered through, the next try and the start of a failing
		// run, which retries for 24 hours
		newSqlMigration(`
			CREATE TABLE network_audit_webhook (
				webhook_id uuid NOT NULL,
				network_id uuid NOT NULL,
				url varchar(2048) NOT NULL,
				delivered_seq bigint NOT NULL DEFAULT 0,
				next_time timestamp NULL,
				fail_time timestamp NULL,

				PRIMARY KEY (webhook_id)
			)
		`),
		newSqlMigration(`
			CREATE INDEX network_audit_webhook_network_id ON network_audit_webhook (network_id, webhook_id)
		`),
		newSqlMigration(`
			CREATE INDEX network_audit_webhook_next_time ON network_audit_webhook (next_time, webhook_id) WHERE next_time IS NOT NULL
		`),
	}
}

// MG6's §8 part: network_acl_share (ACL's tasks).
func sidecarMg6AclMigrations() []any {
	return []any{}
}

// MG6's §17 part: network_bridge_share (BRIDGE's tasks). Every column is one of
// §2.8 api-9's: the share, the sharing network and the network it invited, its
// target and alias tables sealed under a key the invite code derives and the
// server cannot (§17.9, I-2), their versions, and the accept status, varchar
// and never ENUM. No prefix, address or key is in the clear. A row is kept until
// the share is revoked, and goes with either network. Nothing reads it unless
// the binary links MG6 (server.SidecarMg6Linked).
func sidecarMg6BridgeMigrations() []any {
	return []any{
		// network_id shares its targets; invited_network_id, the accepting
		// network, is bound at accept with its alias table and status, and
		// stays NULL until then
		newSqlMigration(`
			CREATE TABLE network_bridge_share (
				share_id uuid NOT NULL,
				network_id uuid NOT NULL,
				invited_network_id uuid NULL,
				target_table bytea NOT NULL,
				target_table_version bigint NOT NULL,
				alias_table bytea NULL,
				alias_table_version bigint NOT NULL DEFAULT 0,
				accept_status varchar(32) NULL,

				PRIMARY KEY (share_id)
			)
		`),
		// a network's deletion, either side
		newSqlMigration(`
			CREATE INDEX network_bridge_share_network_id ON network_bridge_share (network_id, share_id)
		`),
		newSqlMigration(`
			CREATE INDEX network_bridge_share_invited_network_id ON network_bridge_share (invited_network_id, share_id) WHERE invited_network_id IS NOT NULL
		`),
	}
}

// MG6's §20 part: transfer_balance.tier, transfer_balance's team_credit index,
// built concurrently, and stripe_meter_report (QUOTA's tasks).
func sidecarMg6QuotaMigrations() []any {
	return []any{
		// §20.5's Source (QUOTA-M6-01): the Team grant's mark, 'team' on the
		// period's grant and NULL on every other balance, the credit line
		// included, so pro readers stay right and only NetworkTierOf reads it.
		// Nullable with no default, so adding it rewrites no row. Catalog-only,
		// but every balance query queues behind ADD COLUMN's lock, so its wait
		// is bounded as grant_kind's was; a migrate that times out is re-run.
		newSqlMigration(`
			SET LOCAL lock_timeout = '2s';
			ALTER TABLE transfer_balance ADD COLUMN tier varchar(16) NULL
		`),
		// §20.9's draw order (D-QUOTA-9; QUOTA-M4-02): the escrow keyset pages
		// order and key by the team_credit flag first, so each page stays one
		// index scan with no sort, beside today's
		// transfer_balance_active_network_end_start_id, which the gate-off
		// pages keep using. The flag's expression is the queries' text
		// exactly (model.escrowDrawCreditSql), or the planner sorts. Built
		// concurrently on the existing table; mutable amounts stay out of
		// INCLUDE, as today's index's comment says, so a reservation update
		// adds no covering-index work. The escrow_draw_order gate reads false
		// until this group links, so nothing pages on the index before it is
		// built.
		newRestartableOnlineSqlMigration(
			`DROP INDEX CONCURRENTLY IF EXISTS transfer_balance_active_network_credit_end_start_id`,
			`CREATE INDEX CONCURRENTLY transfer_balance_active_network_credit_end_start_id ON transfer_balance (network_id, (grant_kind IS NOT DISTINCT FROM 'team_credit'), end_time, start_time, balance_id) WHERE active`,
			`CREATE INDEX transfer_balance_active_network_credit_end_start_id ON transfer_balance (network_id, (grant_kind IS NOT DISTINCT FROM 'team_credit'), end_time, start_time, balance_id) WHERE active`,
		),
	}
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
// partitioned mail_connection_record (MAIL-M7-21), §15.15's four tables
// exactly. Every column is one of §2.8's mx-2 or mx-3 (I-18); states and
// results are varchar, never ENUM. No column holds a sender's address, a
// burner label or any per-network mail address (owner decisions 32, 34); the
// one address here is a Team's own direct-MX receiver, which mx-3 names and
// §12.8 publishes in an A or AAAA record anyway. Nothing reads these tables
// unless the binary links MG7 (server.SidecarMg7Linked).
func sidecarMg7MailMigrations() []any {
	return []any{
		// mail-enabled networks (mx-3): POST /mail/enable writes the row, and
		// create_time starts §14.7's 24 h send gate. The row goes when mail is
		// turned off or its network is deleted.
		newSqlMigration(`
			CREATE TABLE mail_network (
				network_id uuid NOT NULL,
				create_time timestamp NOT NULL,

				PRIMARY KEY (network_id)
			)
		`),

		// receivers' mail host keys (mx-3), one row per key a receiver
		// published for a host, mx or mx.<scope>, through POST /dns/key-record,
		// whose TLSA 3 1 1 names it. state is published or retired; a retired
		// row goes 30 days after retire_time.
		newSqlMigration(`
			CREATE TABLE mail_host_key (
				network_id uuid NOT NULL,
				host varchar(66) NOT NULL,
				receiver_sidecar_id uuid NOT NULL,
				spki_sha256 bytea NOT NULL,
				state varchar(16) NOT NULL,
				publish_time timestamp NOT NULL,
				retire_time timestamp NULL,

				PRIMARY KEY (network_id, host, receiver_sidecar_id, spki_sha256)
			)
		`),
		// the route projection's receivers of a host, newest first
		// (plan-MAIL-hints #2)
		newSqlMigration(`
			CREATE INDEX mail_host_key_network_state ON mail_host_key (network_id, state, publish_time)
		`),
		// a removed receiver's keys, retired with it (I-15)
		newSqlMigration(`
			CREATE INDEX mail_host_key_receiver_sidecar_id ON mail_host_key (receiver_sidecar_id)
		`),
		// the 30-day sweep
		newSqlMigration(`
			CREATE INDEX mail_host_key_retire_time ON mail_host_key (retire_time) WHERE retire_time IS NOT NULL
		`),

		// a Team's direct MX (mx-3; owner decisions 33, 98): up to two of the
		// network's mail indexers answering TCP 25 themselves, each address the
		// text of a public unicast address. state is pending, verified or
		// failing; verify_time is the last probe that passed. A row goes on
		// DELETE or its receiver's removal.
		newSqlMigration(`
			CREATE TABLE mail_direct_mx (
				network_id uuid NOT NULL,
				receiver_sidecar_id uuid NOT NULL,
				address varchar(45) NOT NULL,
				state varchar(16) NOT NULL,
				verify_time timestamp NULL,

				PRIMARY KEY (network_id, receiver_sidecar_id, address)
			)
		`),
		// a removed receiver's rows (I-15)
		newSqlMigration(`
			CREATE INDEX mail_direct_mx_receiver_sidecar_id ON mail_direct_mx (receiver_sidecar_id)
		`),

		// the router's sessions (mx-2), one row each for a session whose SNI
		// named a network (plan-MAIL-small-readings #3), in 10 s batches off the
		// connection path: receiving network, start, bytes and a result word,
		// no sender address and no burner label. Hourly partitions of
		// start_time, unlogged, which the registry creates ahead and work.Mail-
		// Retention retires at 24 h once any §21.9 hold has copied its network's
		// rows (model/mail_connection_record_model.go). No row has an identity
		// of its own, so the table has no primary key.
		newSqlMigration(`
			CREATE TABLE mail_connection_record (
				network_id uuid NOT NULL,
				start_time timestamp NOT NULL,
				byte_count bigint NOT NULL,
				result varchar(32) NOT NULL
			) PARTITION BY RANGE (start_time)
		`),
		// one network's rows: abuse joins, per-network caps and a hold's copy
		newSqlMigration(`
			CREATE INDEX mail_connection_record_network_id ON mail_connection_record (network_id, start_time)
		`),
	}
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

// MG8's §18 part: sidecar_push_token (SDK-M8-09), §18.7's step 0 and §18.15's table exactly. Every column
// is one of §2.8 api-6's: a client's APNs or FCM token and platform, its row's revision, the Redis hash
// slot of its {scp_<C>} tag, when the row last changed and, after the client's removal, when it was
// removed. One row per client and no foreign key: each path that removes a client tombstones its row in
// that transaction (model.TombstonePushTokenInTx), and taskworker deletes it 24 h after the removal. A
// tombstone keeps client_id, rev and slot, and no platform or token. Nothing reads it unless the binary
// links MG8 (server.SidecarMg8Linked).
func sidecarMg8PushMigrations() []any {
	return []any{
		// rev: a new row's starts at its insert's time in milliseconds, each change adds one in the
		// statement that makes it, and §19.10's restore adds 2^32, always below 2^53 (§18.7). platform
		// is apns or fcm, and both it and token are null on a tombstone.
		newSqlMigration(`
			CREATE TABLE sidecar_push_token (
				client_id uuid NOT NULL,
				platform varchar(8) NULL,
				token text NULL,
				rev bigint NOT NULL,
				update_time timestamp NOT NULL,
				removed_time timestamp NULL,
				slot integer NOT NULL,

				PRIMARY KEY (client_id),
				CHECK ((platform IS NULL) = (token IS NULL)),
				CHECK (removed_time IS NULL OR token IS NULL),
				CHECK (0 < rev AND rev < 9007199254740992),
				CHECK (0 <= slot AND slot < 16384)
			)
		`),
		// §18.7's step 5: a slot's rows, read by a client_id cursor
		newSqlMigration(`
			CREATE INDEX sidecar_push_token_slot ON sidecar_push_token (slot, client_id)
		`),
		// §18.7's step 6: the rows changed since a time, read by an (update_time, client_id) cursor
		newSqlMigration(`
			CREATE INDEX sidecar_push_token_update_time ON sidecar_push_token (update_time, client_id)
		`),
		// the deletion 24 h after a client's removal
		newSqlMigration(`
			CREATE INDEX sidecar_push_token_removed_time ON sidecar_push_token (removed_time, client_id) WHERE removed_time IS NOT NULL
		`),
	}
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

// MG9's §14 part: smtp_send, smtp_halt and smtp_dkim_key (SMTP-M9-18), the rest
// of §14.14's tables after MG5's report tables (plan-SMTP-report-intake-home).
// Every column is one of §2.8's smtp-3, smtp-4 or smtp-6 (I-18): a send keeps
// the MX host and never its IP, and no table holds a recipient, a third-party
// address, a subject, a record length or a key that opens a session
// (D-SMTP-10). Results, triggers, states and algorithms are varchar, never
// ENUM. smtp_send is the only partitioned table here, daily by send_day: the
// registry makes leaves ahead and an insert that finds none makes its day's
// (model/smtp_send_model.go), and SMTP-M9-17's retention takes a day at 30 days
// once §21.9's holds have copied their networks' rows. Nothing reads these
// tables unless the binary links MG9 (server.SidecarMg9Linked).
func sidecarMg9SmtpMigrations() []any {
	return []any{
		// one send record (smtp-3), written after its connection ends: send_id
		// is the 12 random octets X-UR-Ref carries, send_day the UTC day the
		// connection started, which keys its partition. sending_address is the
		// edge's own lb address, never a third party's; mx_host the MX's name.
		// message_id_hash is SHA-256 of the proven Message-ID, x_ur_ref_key_id
		// the X-UR-Ref key's id, recipient_count and message_byte_count Z4's
		// proven counts, tls_version the IANA value and tls_level §14.4's 1 to
		// 4. retry_of_send_id names the send a preamble retried. The primary
		// key makes a batch's retry insert nothing twice.
		newSqlMigration(`
			CREATE TABLE smtp_send (
				send_day date NOT NULL,
				send_id bytea NOT NULL,
				network_id uuid NOT NULL,
				client_id uuid NOT NULL,
				sending_address inet NOT NULL,
				recipient_domain varchar(253) NOT NULL,
				mx_host varchar(253) NOT NULL,
				start_time timestamp NOT NULL,
				end_time timestamp NOT NULL,
				byte_count bigint NOT NULL,
				message_id_hash bytea NULL,
				x_ur_ref_key_id smallint NOT NULL,
				recipient_count bigint NOT NULL,
				message_byte_count bigint NOT NULL,
				tls_version integer NOT NULL,
				tls_level smallint NOT NULL,
				result varchar(32) NOT NULL,
				retry_of_send_id bytea NULL,

				PRIMARY KEY (send_day, send_id),
				CHECK (send_day = start_time::date)
			) PARTITION BY RANGE (send_day)
		`),
		// SmtpJoinKey's four selective fields (§14.8's joins): a verified
		// X-UR-Ref's send id, a reported Message-ID's hash, the network whose
		// selector signed, and the last-hop Received address in a window, the
		// network's also serving RecipientCount and the RUA reconciliation
		newSqlMigration(`
			CREATE INDEX smtp_send_send_id ON smtp_send (send_id)
		`),
		newSqlMigration(`
			CREATE INDEX smtp_send_message_id_hash ON smtp_send (message_id_hash) WHERE message_id_hash IS NOT NULL
		`),
		newSqlMigration(`
			CREATE INDEX smtp_send_network_id ON smtp_send (network_id, start_time)
		`),
		newSqlMigration(`
			CREATE INDEX smtp_send_sending_address ON smtp_send (sending_address, start_time)
		`),

		// a halt (smtp-4; D-SMTP-9): the network, or one burner label of it,
		// the §14.8 trigger and the report's arrival, from which it holds 24 h.
		// A halt set again under its id replaces the row. Kept 13 months.
		newSqlMigration(`
			CREATE TABLE smtp_halt (
				halt_id uuid NOT NULL,
				network_id uuid NOT NULL,
				label varchar(63) NULL,
				trigger varchar(32) NOT NULL,
				start_time timestamp NOT NULL,

				PRIMARY KEY (halt_id)
			)
		`),
		// the edges' 60 s reload of the halts still holding, and the 13-month
		// retention cursor
		newSqlMigration(`
			CREATE INDEX smtp_halt_start_time ON smtp_halt (start_time)
		`),
		// one network's halts: GET /smtp/status and §14.8's 3 halts in 30 days
		newSqlMigration(`
			CREATE INDEX smtp_halt_network_id ON smtp_halt (network_id, start_time)
		`),

		// a sending sidecar's DKIM public key under its opaque selector at its
		// d= (smtp-6; owner decision 36), a network's or a replying burner's,
		// both lowercase. algorithm is rsa-sha256 or ed25519-sha256, public_key
		// the DER SubjectPublicKeyInfo or the 32 Ed25519 octets. state is
		// published, retired (rotated out and still in DNS for 7 days) or
		// removed (out of DNS): retire_time is when it was rotated out or its
		// sidecar removed, remove_time when it left DNS, from which the row
		// keeps 30 days so late reports still check against it (§14.14).
		newSqlMigration(`
			CREATE TABLE smtp_dkim_key (
				selector varchar(63) NOT NULL,
				domain varchar(253) NOT NULL,
				network_id uuid NOT NULL,
				sidecar_id uuid NOT NULL,
				algorithm varchar(32) NOT NULL,
				public_key bytea NOT NULL,
				state varchar(16) NOT NULL,
				publish_time timestamp NOT NULL,
				retire_time timestamp NULL,
				remove_time timestamp NULL,

				PRIMARY KEY (selector, domain)
			)
		`),
		// a network's selectors, counted against dns_dkim_selectors
		newSqlMigration(`
			CREATE INDEX smtp_dkim_key_network_id ON smtp_dkim_key (network_id, state)
		`),
		// a removed sidecar's selectors, deleted from DNS in its removal's write
		// (I-15)
		newSqlMigration(`
			CREATE INDEX smtp_dkim_key_sidecar_id ON smtp_dkim_key (sidecar_id)
		`),
		// key expiry's two clocks: out of DNS 7 days after retiring, and the
		// row's end 30 days after that
		newSqlMigration(`
			CREATE INDEX smtp_dkim_key_retire_time ON smtp_dkim_key (retire_time) WHERE retire_time IS NOT NULL
		`),
		newSqlMigration(`
			CREATE INDEX smtp_dkim_key_remove_time ON smtp_dkim_key (remove_time) WHERE remove_time IS NOT NULL
		`),
	}
}
