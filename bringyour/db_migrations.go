package bringyour

import (
	"fmt"
	"context"
)


/*
Use manual editing to fix or backport changes.

To list all tables in Postgres:

```
SELECT * FROM pg_catalog.pg_tables
WHERE schemaname NOT IN  ('pg_catalog', 'information_schema')
```


*/



type SqlMigration struct {
	sql string
}
func newSqlMigration(sql string) *SqlMigration {
	return &SqlMigration{
		sql: sql,
	}
}


// important these migration functions must be idempotent
type CodeMigration struct {
	callback func(context.Context, PgTx)
}
func newCodeMigration(callback func(context.Context, PgTx)) *CodeMigration {
	return &CodeMigration{
		callback: callback,
	}
}




func ApplyDbMigrations() {
	Tx(func(context context.Context, tx PgTx) {
		tx.Exec(
			context,
			`
			CREATE TABLE IF NOT EXISTS migration_audit (
			    migration_time timestamp NOT NULL DEFAULT now(),
			    start_version_number int NOT NULL,
			    end_version_number int NULL,
			    status VARCHAR(32) NOT NULL
			)
			`,
		)
	})

	var endVersionNumber int
	Db(func(context context.Context, conn PgConn) {
		var endVersionNumber int
		conn.QueryRow(
			context,
			`
			SELECT COALESCE(MAX(end_version_number), 0) AS max_end_version_number
			FROM migration_audit
			WHERE status = 'success'
			`,
		).Scan(&endVersionNumber)
	})


	Tx(func(context context.Context, tx PgTx) {
		tx.Exec(
			context,
			`INSERT INTO migration_audit (start_version_number, status) VALUES ($1, 'start')`,
			endVersionNumber,
		)
		for i := endVersionNumber; i < len(migrations); i += 1 {
			switch v := migrations[i].(type) {
				case *SqlMigration:
					_, err := tx.Exec(context, v.sql)
					Raise(err)
				case *CodeMigration:
					v.callback(context, tx)
				default:
					panic(fmt.Sprintf("Unknown migration type %T", v))
			}
		}
		tx.Exec(
			context,
			`INSERT INTO migration_audit (start_version_number, end_version_number, status) VALUES ($1, $2, 'success')`,
			endVersionNumber,
			len(migrations),
		)
	})
}

var migrations = []any{
	// newSqlMigration(`CREATE TYPE audit_provider_event_type AS ENUM (
	// 	'provider_offline',
	// 	'provider_online_superspeed',
	// 	'provider_online_not_superspeed'
	// )`),
	// newSqlMigration(`CREATE TYPE audit_event_type AS VARCHAR(64)`),
	// newSqlMigration(`CREATE TYPE audit_event_details_type AS TEXT`),
	newSqlMigration(`
		CREATE TABLE audit_provider_event (
			event_id uuid NOT NULL,
			event_time timestamp NOT NULL DEFAULT now(),
			network_id uuid NOT NULL,
			device_id uuid NOT NULL,
			event_type VARCHAR(64) NOT NULL,
			event_details TEXT NULL,
			country_name VARCHAR(128) NOT NULL,
			region_name VARCHAR(128) NOT NULL,
			city_name VARCHAR(128) NOT NULL,

			PRIMARY KEY (event_id)
		)
	`),
	newSqlMigration(`
		CREATE INDEX audit_provider_event_stats_device_id ON audit_provider_event (event_time, device_id, event_id)
	`),
	// newSqlMigration(`CREATE TYPE audit_extender_event_type AS ENUM (
	// 	'extender_offline',
	// 	'extender_online_superspeed',
	// 	'extender_online_not_superspeed'
	// )`),
	newSqlMigration(`
		CREATE TABLE audit_extender_event (
			event_id uuid NOT NULL,
			event_time timestamp NOT NULL DEFAULT now(),
			network_id uuid NOT NULL,
			extender_id uuid NOT NULL,
			event_type VARCHAR(64) NOT NULL,
			event_details TEXT NULL,

			PRIMARY KEY (event_id)
		)
	`),
	newSqlMigration(`
		CREATE INDEX audit_extender_event_stats_extender_id ON audit_extender_event (event_time, extender_id, event_id)
	`),
	// newSqlMigration(`CREATE TYPE audit_network_event_type AS ENUM (
	// 	'network_created',
	// 	'network_deleted'
	// )`),
	newSqlMigration(`
		CREATE TABLE audit_network_event (
			event_id uuid NOT NULL,
			event_time timestamp NOT NULL DEFAULT now(),
			network_id uuid NOT NULL,
			event_type VARCHAR(64) NOT NULL,
			event_details TEXT NULL,

			PRIMARY KEY (event_id)
		)
	`),
	newSqlMigration(`
		CREATE INDEX audit_network_event_stats_network_id ON audit_network_event (event_time, network_id, event_id)
	`),
	// newSqlMigration(`CREATE TYPE audit_device_event_type AS ENUM (
	// 	'device_added',
	// 	'device_removed'
	// )`),
	newSqlMigration(`
		CREATE TABLE audit_device_event (
			event_id uuid NOT NULL,
			event_time timestamp NOT NULL DEFAULT now(),
			network_id uuid NOT NULL,
			device_id uuid NOT NULL,
			event_type VARCHAR(64) NOT NULL,
			event_details TEXT NULL,

			PRIMARY KEY (event_time, device_id, event_id),
			UNIQUE (event_id)
		)
	`),
	newSqlMigration(`
		CREATE INDEX audit_device_event_stats_device_id ON audit_device_event (event_time, device_id, event_id)
	`),
	// newSqlMigration(`CREATE TYPE audit_contract_event_type AS ENUM (
	// 	'contract_closed_success'
	// )`),
	newSqlMigration(`
		CREATE TABLE audit_contract_event (
			event_id uuid NOT NULL,
			event_time timestamp NOT NULL DEFAULT now(),
			contract_id uuid NOT NULL,
			client_network_id uuid NOT NULL,
			client_device_id uuid NOT NULL,
			provider_network_id uuid NOT NULL,
			provider_device_id uuid NOT NULL,
			extender_network_id uuid NULL,
			extender_id uuid NULL,
			event_type VARCHAR(64) NOT NULL,
			event_details TEXT NULL,
			transfer_bytes BIGINT NOT NULL DEFAULT 0,
			transfer_packets BIGINT NOT NULL DEFAULT 0,

			UNIQUE (event_id)
		)
	`),
	newSqlMigration(`
		CREATE INDEX audit_contract_event_stats ON audit_contract_event (event_time, transfer_bytes, transfer_packets)
	`),
	newSqlMigration(`
		CREATE INDEX audit_contract_event_stats_extender_id
		ON audit_contract_event (event_time, extender_id, transfer_bytes, transfer_packets)
	`),

	newSqlMigration(`
		CREATE TABLE network (
			network_id uuid NOT NULL,
			network_name VARCHAR(256) NOT NULL,
			admin_user_id uuid NOT NULL,

			PRIMARY KEY (network_id),
			UNIQUE (network_name)
		)
	`),
	newSqlMigration(`
		CREATE INDEX network_admin_user_id ON network (admin_user_id, network_id)
	`),

	newSqlMigration(`
		CREATE TYPE auth_type AS ENUM (
			'password',
			'apple',
			'google'
		)
	`),
	// password_hash: 32-byte argon2 hash digest
	// password_salt: 32-byte random
	newSqlMigration(`
		CREATE TABLE network_user (
			user_id uuid NOT NULL,
			user_name VARCHAR(128) NOT NULL,
			auth_type auth_type NOT NULL,
			user_auth VARCHAR(256) NULL,
			password_hash bytea NULL,
			password_salt bytea NULL,
			auth_jwt TEXT NULL,
			validated BOOL NOT NULL DEFAULT false,

			PRIMARY KEY (user_id),
			UNIQUE (user_auth)
		)
	`),
	// the index of user_auth is covered by the unique index

	// an attempt any of:
	// - network create
	// - login
	// - password login
	// - reset
	// - validation
	// client_ipv4: 4 bytes stored in dot notation
	newSqlMigration(`
		CREATE TABLE user_auth_attempt (
			user_auth_attempt_id uuid NOT NULL,
			user_auth VARCHAR(256) NULL,
			attempt_time timestamp NOT NULL DEFAULT now(),
			client_ipv4 VARCHAR(16) NOT NULL,
			success BOOL NOT NULL,

			PRIMARY KEY (user_auth_attempt_id)
		)
	`),
	newSqlMigration(`
		CREATE INDEX user_auth_attempt_client_ipv4 ON user_auth_attempt (client_ipv4, attempt_time)
	`),
	newSqlMigration(`
		CREATE INDEX user_auth_attempt_user_auth ON user_auth_attempt (user_auth, attempt_time)
	`),
	// reset_code: 64-byte hex
	// reset codes must be globally unique because of the way they are used
	newSqlMigration(`
		CREATE TABLE user_auth_reset (
			user_auth_reset_id uuid NOT NULL,
			user_id uuid NOT NULL,
			reset_time timestamp NOT NULL DEFAULT now(),
			reset_code VARCHAR(256) NOT NULL,
			used BOOL NOT NULL DEFAULT false,

			PRIMARY KEY (user_auth_reset_id),
			UNIQUE (reset_code)
		)
	`),
	newSqlMigration(`
		CREATE INDEX user_auth_reset_user_id ON user_auth_reset (user_id, reset_code)
	`),
	// validate_code: 4-byte hex 
	newSqlMigration(`
		CREATE TABLE user_auth_validate (
			user_auth_validate_id uuid NOT NULL,
			user_id uuid NOT NULL,
			validate_time timestamp NOT NULL DEFAULT now(),
			validate_code VARCHAR(16) NOT NULL,
			used BOOL NOT NULL DEFAULT false,

			PRIMARY KEY (user_auth_validate_id),
			UNIQUE (user_id, validate_code)
		)
	`),
	// newSqlMigration(`
	// 	CREATE INDEX user_auth_validate_user_id ON user_auth_validate (user_id, validate_code)
	// `),

	newSqlMigration(`
		CREATE TABLE account_feedback (
			feedback_id uuid NOT NULL,
			network_id uuid NOT NULL,
			user_id uuid NOT NULL,
			feedback_time timestamp NOT NULL DEFAULT now(),
			uses_personal BOOL NOT NULL DEFAULT false,
			uses_business BOOL NOT NULL DEFAULT false,
			needs_private BOOL NOT NULL DEFAULT false,
			needs_safe BOOL NOT NULL DEFAULT false,
			needs_global BOOL NOT NULL DEFAULT false,
			needs_collaborate BOOL NOT NULL DEFAULT false,
			needs_app_control BOOL NOT NULL DEFAULT false,
			needs_block_data_brokers BOOL NOT NULL DEFAULT false,
			needs_block_ads BOOL NOT NULL DEFAULT false,
			needs_focus BOOL NOT NULL DEFAULT false,
			needs_connect_servers BOOL NOT NULL DEFAULT false,
			needs_run_servers BOOL NOT NULL DEFAULT false,
			needs_prevent_cyber BOOL NOT NULL DEFAULT false,
			needs_audit BOOL NOT NULL DEFAULT false,
			needs_zero_trust BOOL NOT NULL DEFAULT false,
			needs_visualize BOOL NOT NULL DEFAULT false,
			needs_other TEXT NULL,

			PRIMARY KEY (feedback_id)
		)
	`),
	newSqlMigration(`
		CREATE TABLE account_preferences (
			network_id uuid NOT NULL,
			product_updates BOOL NOT NULL DEFAULT false,

			PRIMARY KEY (network_id)
		)
	`),

	newSqlMigration(`
		CREATE TABLE search_value (
			realm VARCHAR(16) NOT NULL,
			value_id uuid NOT NULL,
			value VARCHAR(1024) NOT NULL,
			alias INT NOT NULL DEFAULT 0,

			PRIMARY KEY(value_id, alias)
		)
	`),
	newSqlMigration(`
		CREATE INDEX search_value_realm_value ON search_value (realm, value, value_id, alias)
	`),

	newSqlMigration(`
		CREATE TABLE search_projection (
			realm VARCHAR(16) NOT NULL,
		    dim SMALLINT NOT NULL,
		    elen SMALLINT NOT NULL,
		    dord SMALLINT NOT NULL,
		    dlen SMALLINT NOT NULL,
		    vlen SMALLINT NOT NULL,
		    value_id uuid NOT NULL,
		    alias INT NOT NULL DEFAULT 0,

		    PRIMARY KEY (realm, dim, elen, dord, dlen, vlen, value_id, alias)
		)
	`),
	newSqlMigration(`
		CREATE INDEX search_projection_value_id ON search_projection (value_id, alias)
	`),

}
