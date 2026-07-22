package server

import (
	"context"
	"fmt"

	"github.com/urnetwork/glog/v2026"
)

/*
Use manual editing to fix or backport changes.

To list all tables in Postgres:

```
SELECT * FROM pg_catalog.pg_tables
WHERE schemaname NOT IN  ('pg_catalog', 'information_schema')
```


*/

var DbMigrationVerbose = false

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
	callback func(context.Context)
}

func newCodeMigration(callback func(context.Context)) *CodeMigration {
	return &CodeMigration{
		callback: callback,
	}
}

func DbVersion(ctx context.Context) int {
	MaintenanceTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(
			ctx,
			`
            CREATE TABLE IF NOT EXISTS migration_audit (
                migration_time timestamp NOT NULL DEFAULT now(),
                start_version_number int NOT NULL,
                end_version_number int NULL,
                status varchar(32) NOT NULL
            )
            `,
		))
	})

	var endVersionNumber int
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(
			ctx,
			`
            SELECT COALESCE(MAX(end_version_number), 0) AS max_end_version_number
            FROM migration_audit
            WHERE status = 'success'
            `,
		)
		WithPgResult(result, err, func() {
			if result.Next() {
				Raise(result.Scan(&endVersionNumber))
			}
		})
	})

	return endVersionNumber
}

// MigrationCount is the number of migrations defined locally. The version a DB
// reaches after applying all of them is this count (versions are 1-indexed by
// end_version_number, which equals the migration's slice index + 1).
func MigrationCount() int {
	return len(migrations)
}

func ApplyDbMigrations(ctx context.Context) {
	ApplyDbMigrationsUpTo(ctx, len(migrations))
}

// ApplyDbMigrationsUpTo applies migrations until the DB reaches version upTo
// (i.e. it applies the migrations at slice indices [DbVersion, upTo)). Passing
// len(migrations) is equivalent to ApplyDbMigrations. Used by the schema audit
// to reconstruct the schema a given recorded version is supposed to produce.
func ApplyDbMigrationsUpTo(ctx context.Context, upTo int) {
	if upTo > len(migrations) {
		upTo = len(migrations)
	}
	for i := DbVersion(ctx); i < upTo; i += 1 {
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO migration_audit (start_version_number, status) VALUES ($1, 'start')`,
				i,
			))
		})
		switch v := migrations[i].(type) {
		case *SqlMigration:
			if DbMigrationVerbose {
				glog.Infof("[migrate][%d/%d]sql = %s\n", i+1, len(migrations), v.sql)
			}
			MaintenanceTx(ctx, func(tx PgTx) {
				defer func() {
					if err := recover(); err != nil {
						// print the sql for debugging
						glog.Infof("[migrate][%d/%d]err = %s; sql = %s\n", i+1, len(migrations), err, v.sql)
						panic(err)
					}
				}()
				RaisePgResult(tx.Exec(ctx, v.sql))
			})
		case *CodeMigration:
			if DbMigrationVerbose {
				glog.Infof("[migrate][%d/%d]code = %v\n", i+1, len(migrations), v.callback)
			}
			v.callback(ctx)
		default:
			panic(fmt.Errorf("Unknown migration type %T", v))
		}
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO migration_audit (start_version_number, end_version_number, status) VALUES ($1, $2, 'success')`,
				i,
				i+1,
			))
		})
	}
}

// style: use varchar not ENUM
// style: in queries with two+ tables, use fully qualified column names
// FIXME perf: CREATE INDEX should always use CONCURRENTLY - we need to use raw connections for the sql, and the db to mark raw connections as not read only
var migrations = []any{
	// newSqlMigration(`CREATE TYPE audit_provider_event_type AS ENUM (
	//  'provider_offline',
	//  'provider_online_superspeed',
	//  'provider_online_not_superspeed'
	// )`),
	// newSqlMigration(`CREATE TYPE audit_event_type AS varchar(64)`),
	// newSqlMigration(`CREATE TYPE audit_event_details_type AS TEXT`),
	newSqlMigration(`
        CREATE TABLE audit_provider_event (
            event_id uuid NOT NULL,
            event_time timestamp NOT NULL DEFAULT now(),
            network_id uuid NOT NULL,
            device_id uuid NOT NULL,
            event_type varchar(64) NOT NULL,
            event_details text NULL,
            country_name varchar(128) NOT NULL,
            region_name varchar(128) NOT NULL,
            city_name varchar(128) NOT NULL,

            PRIMARY KEY (event_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX audit_provider_event_stats_device_id ON audit_provider_event (event_time, device_id, event_id)
    `),
	// newSqlMigration(`CREATE TYPE audit_extender_event_type AS ENUM (
	//  'extender_offline',
	//  'extender_online_superspeed',
	//  'extender_online_not_superspeed'
	// )`),
	newSqlMigration(`
        CREATE TABLE audit_extender_event (
            event_id uuid NOT NULL,
            event_time timestamp NOT NULL DEFAULT now(),
            network_id uuid NOT NULL,
            extender_id uuid NOT NULL,
            event_type varchar(64) NOT NULL,
            event_details text NULL,

            PRIMARY KEY (event_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX audit_extender_event_stats_extender_id ON audit_extender_event (event_time, extender_id, event_id)
    `),
	// newSqlMigration(`CREATE TYPE audit_network_event_type AS ENUM (
	//  'network_created',
	//  'network_deleted'
	// )`),
	newSqlMigration(`
        CREATE TABLE audit_network_event (
            event_id uuid NOT NULL,
            event_time timestamp NOT NULL DEFAULT now(),
            network_id uuid NOT NULL,
            event_type varchar(64) NOT NULL,
            event_details text NULL,

            PRIMARY KEY (event_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX audit_network_event_stats_network_id ON audit_network_event (event_time, network_id, event_id)
    `),
	// newSqlMigration(`CREATE TYPE audit_device_event_type AS ENUM (
	//  'device_added',
	//  'device_removed'
	// )`),
	newSqlMigration(`
        CREATE TABLE audit_device_event (
            event_id uuid NOT NULL,
            event_time timestamp NOT NULL DEFAULT now(),
            network_id uuid NOT NULL,
            device_id uuid NOT NULL,
            event_type varchar(64) NOT NULL,
            event_details text NULL,

            PRIMARY KEY (event_time, device_id, event_id),
            UNIQUE (event_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX audit_device_event_stats_device_id ON audit_device_event (event_time, device_id, event_id)
    `),
	// newSqlMigration(`CREATE TYPE audit_contract_event_type AS ENUM (
	//  'contract_closed_success'
	// )`),
	// RENAMED `transfer_bytes` to `transfer_byte_count`
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
            event_type varchar(64) NOT NULL,
            event_details text NULL,
            transfer_bytes bigint NOT NULL DEFAULT 0,
            transfer_packets bigint NOT NULL DEFAULT 0,

            PRIMARY KEY (event_id)
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
            network_name varchar(256) NOT NULL,
            admin_user_id uuid NOT NULL,

            PRIMARY KEY (network_id),
            UNIQUE (network_name)
        )
    `),
	newSqlMigration(`
        CREATE INDEX network_admin_user_id ON network (admin_user_id, network_id)
    `),

	// DROPPED
	newSqlMigration(`
        CREATE TYPE auth_type AS ENUM (
            'password',
            'apple',
            'google'
        )
    `),
	// ALTER auth_type changed to varchar(32)
	// RENAME validate to verified
	// password_hash: 32-byte argon2 hash digest
	// password_salt: 32-byte random
	// FIXME add network_id
	newSqlMigration(`
        CREATE TABLE network_user (
            user_id uuid NOT NULL,
            user_name varchar(128) NOT NULL,
            auth_type auth_type NOT NULL,
            user_auth varchar(256) NULL,
            password_hash bytea NULL,
            password_salt bytea NULL,
            auth_jwt text NULL,
            validated bool NOT NULL DEFAULT false,

            PRIMARY KEY (user_id),
            UNIQUE (user_auth)
        )
    `),
	// the index of user_auth is covered by the unique index

	// MIGRATED client_ipv4/client_ip moved to client_address_hash/client_port
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
            user_auth varchar(256) NULL,
            attempt_time timestamp NOT NULL DEFAULT now(),
            client_ipv4 varchar(16) NOT NULL,
            success bool NOT NULL,

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
            reset_code varchar(256) NOT NULL,
            used bool NOT NULL DEFAULT false,

            PRIMARY KEY (user_auth_reset_id),
            UNIQUE (reset_code)
        )
    `),
	newSqlMigration(`
        CREATE INDEX user_auth_reset_user_id ON user_auth_reset (user_id, reset_code)
    `),
	// DROPPED and recreated as `user_auth_verify` (see below)
	newSqlMigration(`
        CREATE TABLE user_auth_validate (
            user_auth_validate_id uuid NOT NULL,
            user_id uuid NOT NULL,
            validate_time timestamp NOT NULL DEFAULT now(),
            validate_code varchar(16) NOT NULL,
            used bool NOT NULL DEFAULT false,

            PRIMARY KEY (user_auth_validate_id),
            UNIQUE (user_id, validate_code)
        )
    `),

	newSqlMigration(`
        CREATE TABLE account_feedback (
            feedback_id uuid NOT NULL,
            network_id uuid NOT NULL,
            user_id uuid NOT NULL,
            feedback_time timestamp NOT NULL DEFAULT now(),
            uses_personal bool NOT NULL DEFAULT false,
            uses_business bool NOT NULL DEFAULT false,
            needs_private bool NOT NULL DEFAULT false,
            needs_safe bool NOT NULL DEFAULT false,
            needs_global bool NOT NULL DEFAULT false,
            needs_collaborate bool NOT NULL DEFAULT false,
            needs_app_control bool NOT NULL DEFAULT false,
            needs_block_data_brokers bool NOT NULL DEFAULT false,
            needs_block_ads bool NOT NULL DEFAULT false,
            needs_focus bool NOT NULL DEFAULT false,
            needs_connect_servers bool NOT NULL DEFAULT false,
            needs_run_servers bool NOT NULL DEFAULT false,
            needs_prevent_cyber bool NOT NULL DEFAULT false,
            needs_audit bool NOT NULL DEFAULT false,
            needs_zero_trust bool NOT NULL DEFAULT false,
            needs_visualize bool NOT NULL DEFAULT false,
            needs_other text NULL,

            PRIMARY KEY (feedback_id)
        )
    `),
	newSqlMigration(`
        CREATE TABLE account_preferences (
            network_id uuid NOT NULL,
            product_updates bool NOT NULL DEFAULT false,

            PRIMARY KEY (network_id)
        )
    `),

	// DROPPED and re-created
	newSqlMigration(`
        CREATE TABLE search_value (
            realm varchar(16) NOT NULL,
            value_id uuid NOT NULL,
            value varchar(1024) NOT NULL,
            alias int NOT NULL DEFAULT 0,

            PRIMARY KEY(value_id, alias)
        )
    `),
	// DROPPED
	newSqlMigration(`
        CREATE INDEX search_value_realm_value ON search_value (realm, value, value_id, alias)
    `),

	// DROPPED and re-created
	newSqlMigration(`
        CREATE TABLE search_projection (
            realm varchar(16) NOT NULL,
            dim smallint NOT NULL,
            elen smallint NOT NULL,
            dord smallint NOT NULL,
            dlen smallint NOT NULL,
            vlen smallint NOT NULL,
            value_id uuid NOT NULL,
            alias int NOT NULL DEFAULT 0,

            PRIMARY KEY (realm, dim, elen, dord, dlen, vlen, value_id, alias)
        )
    `),
	// DROPPED
	newSqlMigration(`
        CREATE INDEX search_projection_value_id ON search_projection (value_id, alias)
    `),

	newSqlMigration(`
        ALTER TABLE network_user RENAME COLUMN validated TO verified
    `),
	newSqlMigration(`
        DROP TABLE user_auth_validate
    `),
	// verify_code: 4-byte hex
	newSqlMigration(`
        CREATE TABLE user_auth_verify (
            user_auth_verify_id uuid NOT NULL,
            user_id uuid NOT NULL,
            verify_time timestamp NOT NULL DEFAULT now(),
            verify_code varchar(16) NOT NULL,
            used bool NOT NULL DEFAULT false,

            PRIMARY KEY (user_auth_verify_id),
            UNIQUE (user_id, verify_code)
        )
    `),

	newSqlMigration(`
        ALTER TABLE audit_contract_event
        RENAME transfer_bytes TO transfer_byte_count
    `),

	// ADDED device_id
	// REMOVED device_spec
	// RESIZED device_spec to varchar(256)
	newSqlMigration(`
        CREATE TABLE network_client (
            client_id uuid NOT NULL,
            network_id uuid NOT NULL,
            active bool NOT NULL DEFAULT true,
            description varchar(256) NULL,
            device_spec varchar(64) NULL,
            create_time timestamp NOT NULL DEFAULT now(),
            auth_time timestamp NOT NULL DEFAULT now(),

            PRIMARY KEY (client_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX network_client_network_id_active ON network_client (network_id, active)
    `),
	newSqlMigration(`
        CREATE INDEX network_client_network_id_create_time ON network_client (network_id, create_time)
    `),

	// newSqlMigration(`
	//  CREATE TYPE provide_mode AS ENUM (
	//      'network',
	//      'ff',
	//      'public',
	//      'stream'
	//  )
	// `),

	// DROPPED use `provide_key`
	newSqlMigration(`
        CREATE TABLE client_provide (
            client_id uuid NOT NULL,
            provide_mode int NOT NULL DEFAULT 0,

            PRIMARY KEY (client_id)
        )
    `),

	newSqlMigration(`
        CREATE TABLE provide_key (
            client_id uuid NOT NULL,
            provide_mode int NOT NULL,
            secret_key bytea NOT NULL,

            PRIMARY KEY (client_id, provide_mode)
        )
    `),

	// `client_address` is ip:port
	// ipv6 is at most 45 chars, plus 7 for the :port
	// see https://stackoverflow.com/questions/166132/maximum-length-of-the-textual-representation-of-an-ipv6-address
	newSqlMigration(`
        CREATE TABLE network_client_connection (
            client_id uuid NOT NULL,
            connection_id uuid NOT NULL,
            connected bool NOT NULL DEFAULT true,
            connect_time timestamp NOT NULL,
            disconnect_time timestamp NULL,
            connection_host varchar(128) NOT NULL,
            connection_service varchar(128) NOT NULL,
            connection_block varchar(128) NOT NULL,
            client_address varchar(52) NOT NULL,

            PRIMARY KEY (connection_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX network_client_connection_client_id_connected ON network_client_connection (client_id, connected)
    `),

	// DROPPED
	newSqlMigration(`
        CREATE TABLE ip_location_lookup (
            ip_address varchar(64) NOT NULL,
            lookup_time timestamp NOT NULL DEFAULT now(),
            result_json text NOT NULL,

            PRIMARY KEY (ip_address, lookup_time)
        )
    `),

	// newSqlMigration(`
	//  CREATE TYPE location_type AS ENUM (
	//      'city',
	//      'region',
	//      'country'
	//  )
	// `),

	// DROPPED and recreated
	newSqlMigration(`
        CREATE TABLE location (
            location_id uuid NOT NULL,
            location_type varchar(16) NOT NULL,
            location_name varchar(128) NOT NULL,
            city_location_id uuid NULL,
            region_location_id uuid NULL,
            country_location_id uuid NULL,
            country_code char(2) NOT NULL,

            PRIMARY KEY (location_id)
        )
    `),
	// DROPPED and recreated
	newSqlMigration(`
        CREATE INDEX location_type_country_code_name ON location (location_type, country_code, location_name, country_location_id, region_location_id, location_id)
    `),

	// DROPPED and recreated
	newSqlMigration(`
        CREATE TABLE location_group (
            location_group_id uuid NOT NULL,
            location_group_name varchar(128) NOT NULL,
            promoted bool NOT NULL DEFAULT false,

            PRIMARY KEY (location_group_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX location_group_name ON location_group (location_group_name, location_group_id)
    `),

	newSqlMigration(`
        CREATE TABLE location_group_member (
            location_group_id uuid NOT NULL,
            location_id uuid NOT NULL,

            PRIMARY KEY (location_group_id, location_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX location_group_member_location_id ON location_group_member (location_id, location_group_id)
    `),

	// this denormalizes to speed up finding client_ids by location
	// (connection_id, client_id)
	// (city_location_id, region_location_id, country_location_id)
	newSqlMigration(`
        CREATE TABLE network_client_location (
            connection_id uuid NOT NULL,
            client_id uuid NOT NULL,
            city_location_id uuid NOT NULL,
            region_location_id uuid NOT NULL,
            country_location_id uuid NOT NULL,

            PRIMARY KEY (connection_id)
        )
    `),
	// 3 separate indexes: city location id, region, country
	newSqlMigration(`
        CREATE INDEX network_client_location_city_client_id ON network_client_location (city_location_id, client_id)
    `),
	newSqlMigration(`
        CREATE INDEX network_client_location_region_client_id ON network_client_location (region_location_id, client_id)
    `),
	newSqlMigration(`
        CREATE INDEX network_client_location_country_client_id ON network_client_location (country_location_id, client_id)
    `),

	// ALTERED No columns are allowed null
	newSqlMigration(`
        CREATE TABLE network_client_resident (
            client_id uuid NOT NULL,
            instance_id uuid NOT NULL,
            resident_id uuid NULL,
            resident_host varchar(128) NULL,
            resident_service varchar(128) NULL,
            resident_block varchar(128) NULL,

            PRIMARY KEY (client_id),
            UNIQUE (resident_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX network_client_resident_host_port ON network_client_resident (resident_host, resident_id)
    `),

	newSqlMigration(`
        CREATE TABLE network_client_resident_port (
            client_id uuid NOT NULL,
            resident_id uuid NOT NULL,
            resident_internal_port int NOT NULL,

            PRIMARY KEY (client_id, resident_id, resident_internal_port)
        )
    `),

	// ADDED subsidy_net_revenue_nano_cents
	// ADDED paid
	// net_revenue_nano_cents is how much was received for this balance (revenue - intermediary fees)
	newSqlMigration(`
        CREATE TABLE transfer_balance (
            balance_id uuid NOT NULL,
            network_id uuid NOT NULL,
            start_time timestamp NOT NULL DEFAULT now(),
            end_time timestamp NOT NULL,
            start_balance_byte_count bigint NOT NULL,
            net_revenue_nano_cents bigint NOT NULL,

            balance_byte_count bigint NOT NULL,
            active bool GENERATED ALWAYS AS (0 < balance_byte_count) STORED,

            PRIMARY KEY (balance_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX transfer_balance_network_id_active_end_time ON transfer_balance (network_id, active, end_time)
    `),

	// newSqlMigration(`
	//  CREATE TYPE contract_outcome AS ENUM (
	//      'settled',
	//      'dispute_resolved_to_source',
	//      'dispute_resolved_to_destination'
	//  )
	// `),

	// ADDED companion_contract_id
	// ADDED priority
	// ADDED payer_network_id
	// ADDED payee_network_id
	newSqlMigration(`
        CREATE TABLE transfer_contract (
            contract_id uuid NOT NULL,
            source_network_id uuid NOT NULL,
            source_id uuid NOT NULL,
            destination_network_id uuid NOT NULL,
            destination_id uuid NOT NULL,
            open bool GENERATED ALWAYS AS (dispute = false AND outcome IS NULL) STORED,
            transfer_byte_count bigint NOT NULL,
            create_time timestamp NOT NULL DEFAULT now(),
            close_time timestamp NULL,

            dispute bool NOT NULL DEFAULT false,
            outcome varchar(32) NULL,

            PRIMARY KEY (contract_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX transfer_contract_open_source_id ON transfer_contract (open, source_id, destination_id, contract_id)
    `),
	newSqlMigration(`
        CREATE INDEX transfer_contract_open_destination_id ON transfer_contract (open, destination_id, source_id, contract_id)
    `),

	// newSqlMigration(`
	//  CREATE TYPE contract_party AS ENUM (
	//      'source',
	//      'destination'
	//  )
	// `),

	newSqlMigration(`
        CREATE TABLE contract_close (
            contract_id uuid NOT NULL,
            close_time timestamp NOT NULL DEFAULT now(),
            party varchar(16) NOT NULL,
            used_transfer_byte_count bigint NOT NULL,

            PRIMARY KEY (contract_id, party)
        )
    `),

	// creating an escrow must deduct the balances in the same transaction
	// settling an escrow must put `payout_byte_count` into the target account_balances, and `balance_byte_count - payout_byte_count` back into the origin balance
	// primary key is (contract_id, balance_id) because there can be multiple balances used per contract_id
	newSqlMigration(`
        CREATE TABLE transfer_escrow (
            contract_id uuid NOT NULL,
            balance_id uuid NOT NULL,
            balance_byte_count bigint NOT NULL,

            escrow_date timestamp NOT NULL DEFAULT now(),

            settled bool NOT NULL DEFAULT false,
            settle_time timestamp NULL,
            payout_byte_count bigint NULL,

            PRIMARY KEY (contract_id, balance_id)
        )
    `),

	// note the unpaid balance is `provided_balance_byte_count - paid_balance_byte_count`
	// this equals SUM(transfer_escrow_sweep.payout_byte_count) where payment_id IS NULL
	// the cents to pay out is SUM(transfer_escrow_sweep.net_revenue_cents)
	newSqlMigration(`
        CREATE TABLE account_balance (
            network_id uuid NOT NULL,
            provided_byte_count bigint NOT NULL DEFAULT 0,
            provided_net_revenue_nano_cents bigint NOT NULL DEFAULT 0,
            paid_byte_count bigint NOT NULL DEFAULT 0,
            paid_net_revenue_nano_cents bigint NOT NULL DEFAULT 0,

            PRIMARY KEY (network_id)
        )
    `),

	// sweeps escrow value into an account
	// the payment_id is the payment the swept value was paid out using
	// the value of the unpaid balance needs to go back to the escrows to where the balance came from
	// the cents contribution of each escrow is `(escrow.payout_byte_count / balance.balance_byte_count) * balance.net_revenue_cents`
	//
	// payment flow:
	// 1. create an `account_payment` entry which is `complete=false`
	// 2. associate unpaid `transfer_escrow_sweep` to that payment_id
	// 3. complete the payment with the final amount, and update the `payment_record` for the pending payment
	// 4. when the payment is complete, mark the payment `complete=true`
	newSqlMigration(`
        CREATE TABLE transfer_escrow_sweep (
            contract_id uuid NOT NULL,
            balance_id uuid NOT NULL,
            network_id uuid NOT NULL,
            payout_byte_count bigint NOT NULL,
            payout_net_revenue_nano_cents bigint NOT NULL,
            sweep_time timestamp NOT NULL DEFAULT now(),

            payment_id uuid NULL,

            PRIMARY KEY (contract_id, balance_id, network_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX transfer_escrow_sweep_payment_id ON transfer_escrow_sweep (payment_id)
    `),

	// circle uses the circle sdk for usdc
	// newSqlMigration(`
	//  CREATE TYPE wallet_type AS ENUM (
	//      'circle_uc',
	//      'xch',
	//      'sol'
	//  )
	// `),

	newSqlMigration(`
        CREATE TABLE account_wallet (
            wallet_id uuid NOT NULL,
            network_id uuid NOT NULL,
            wallet_type varchar(32) NOT NULL,
            blockchain varchar(32) NOT NULL,
            wallet_address text NOT NULL,
            active bool NOT NULL DEFAULT true,
            default_token_type varchar(16) NOT NULL,
            create_time timestamp NOT NULL DEFAULT now(),

            PRIMARY KEY (wallet_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX account_wallet_active_network_id ON account_wallet (active, network_id, wallet_id)
    `),

	//  `circle_uc_user_id` is the user_id for the circle user-controlled platform
	// DROPPED and recreated
	newSqlMigration(`
        CREATE TABLE circle_uc (
            network_id uuid NOT NULL,
            circle_uc_user_id uuid NOT NULL,

            PRIMARY KEY (network_id)
        )
    `),

	// this is the preferred wallet for payout
	newSqlMigration(`
        CREATE TABLE payout_wallet (
            network_id uuid NOT NULL,
            wallet_id uuid NOT NULL,

            PRIMARY KEY (network_id)
        )
    `),

	// ADDED subsidy_payout_nano_cents
	// payment record is what was submitted to the payment processor
	// payment receipt is the receipt from the payment processor
	newSqlMigration(`
        CREATE TABLE account_payment (
            payment_id uuid NOT NULL,
            payment_plan_id uuid NOT NULL,
            wallet_id uuid NOT NULL,
            payout_byte_count bigint NOT NULL,
            payout_nano_cents bigint NOT NULL,
            min_sweep_time timestamp NOT NULL,
            create_time timestamp NOT NULL DEFAULT now(),

            payment_record text NULL,
            token_type varchar(16) NULL,
            token_amount double precision NULL,
            payment_time timestamp NULL,

            payment_receipt text NULL,
            completed bool NOT NULL DEFAULT false,
            complete_time timestamp NULL,

            canceled bool NOT NULL DEFAULT false,
            cancel_time timestamp NULL,

            PRIMARY KEY (payment_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX account_payment_payment_plan ON account_payment (payment_plan_id, payment_id)
    `),

	newSqlMigration(`
        ALTER TABLE user_auth_attempt ALTER client_ipv4 DROP NOT NULL
    `),
	newSqlMigration(`
        ALTER TABLE user_auth_attempt ADD COLUMN client_ip varchar(64) NULL
    `),
	newSqlMigration(`
        ALTER TABLE user_auth_attempt ADD COLUMN client_port int NULL
    `),
	newSqlMigration(`
        CREATE INDEX user_auth_attempt_client_address ON user_auth_attempt (client_ip, client_port, attempt_time)
    `),

	newSqlMigration(`
        CREATE TABLE transfer_balance_code (
            balance_code_id uuid NOT NULL,
            create_time timestamp NOT NULL,
            start_time timestamp NOT NULL,
            end_time timestamp NOT NULL,
            balance_byte_count bigint NOT NULL,
            net_revenue_nano_cents bigint NOT NULL,
            balance_code_secret varchar(128) NOT NULL,

            purchase_event_id varchar(128) NOT NULL,
            purchase_record text NOT NULL,
            purchase_email varchar(256) NOT NULL,

            redeem_time timestamp NULL,
            redeem_balance_id uuid NULL,

            notify_count int NOT NULL DEFAULT 0,
            next_notify_time timestamp NULL,

            redeemed bool GENERATED ALWAYS AS (redeem_balance_id IS NOT NULL) STORED,

            PRIMARY KEY (balance_code_id),
            UNIQUE (balance_code_secret),
            UNIQUE (purchase_event_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX transfer_balance_code_redeemed_time ON transfer_balance_code (redeemed, end_time, next_notify_time)
    `),

	newSqlMigration(`
        DROP INDEX search_value_realm_value
    `),
	newSqlMigration(`
        DROP TABLE search_value
    `),

	newSqlMigration(`
        CREATE TABLE search_value (
            realm varchar(32) NOT NULL,
            value_id uuid NOT NULL,
            value_variant int NOT NULL,
            value varchar(1024) NOT NULL,
            alias int NOT NULL DEFAULT 0,

            vlen smallint GENERATED ALWAYS AS (LENGTH(value)) STORED,

            PRIMARY KEY(realm, value_id, value_variant, alias)
        )
    `),
	newSqlMigration(`
        CREATE INDEX search_value_realm_vlen ON search_value (realm, vlen)
    `),

	newSqlMigration(`
        DROP INDEX search_projection_value_id
    `),
	newSqlMigration(`
        DROP TABLE search_projection
    `),

	newSqlMigration(`
        CREATE TABLE search_projection (
            realm varchar(32) NOT NULL,
            dim smallint NOT NULL,
            elen smallint NOT NULL,
            dord smallint NOT NULL,
            dlen smallint NOT NULL,
            vlen smallint NOT NULL,
            value_id uuid NOT NULL,
            value_variant int NOT NULL,
            alias int NOT NULL DEFAULT 0,

            PRIMARY KEY (realm, dim, elen, dord, dlen, vlen, value_id, value_variant, alias)
        )
    `),

	newSqlMigration(`
        ALTER TABLE network_user ALTER COLUMN auth_type TYPE varchar(32)
    `),

	newSqlMigration(`
        DROP TYPE auth_type
    `),

	newSqlMigration(`
        DROP INDEX location_type_country_code_name
    `),

	newSqlMigration(`
        DROP TABLE location
    `),

	newSqlMigration(`
        CREATE TABLE location (
            location_id uuid NOT NULL,
            location_type varchar(16) NOT NULL,
            location_name varchar(128) NOT NULL,
            city_location_id uuid NULL,
            region_location_id uuid NULL,
            country_location_id uuid NULL,
            country_code char(2) NOT NULL,
            location_full_name varchar(256) NOT NULL,

            PRIMARY KEY (location_id),
            UNIQUE(location_full_name)
        )
    `),
	newSqlMigration(`
        CREATE INDEX location_type_country_code_name ON location (location_type, country_code, location_name, country_location_id, region_location_id, location_id)
    `),

	newSqlMigration(`
        DROP INDEX location_group_name
    `),

	newSqlMigration(`
        DROP TABLE location_group
    `),

	newSqlMigration(`
        CREATE TABLE location_group (
            location_group_id uuid NOT NULL,
            location_group_name varchar(128) NOT NULL,
            promoted bool NOT NULL DEFAULT false,

            PRIMARY KEY (location_group_id),
            UNIQUE (location_group_name)
        )
    `),

	newSqlMigration(`
        ALTER TABLE network_client ALTER COLUMN device_spec TYPE varchar(256)
    `),

	newSqlMigration(`
        DROP TABLE circle_uc
    `),

	newSqlMigration(`
        CREATE TABLE circle_uc (
            network_id uuid NOT NULL,
            user_id uuid NOT NULL,
            circle_uc_user_id uuid NOT NULL,

            PRIMARY KEY (network_id, user_id)
        )
    `),

	newSqlMigration(`
        CREATE TABLE auth_code (
            auth_code_id uuid NOT NULL,
            network_id uuid NOT NULL,
            user_id uuid NOT NULL,
            auth_code varchar(1024) NOT NULL,
            create_time timestamp NOT NULL,
            end_time timestamp NOT NULL,
            uses int NOT NULL,
            remaining_uses int NOT NULL,
            active bool GENERATED ALWAYS AS (0 < remaining_uses) STORED,

            PRIMARY KEY (auth_code_id),
            UNIQUE (auth_code)
        )
    `),

	newSqlMigration(`
        CREATE INDEX auth_code_active_network_id ON auth_code (active, network_id, auth_code_id)
    `),

	newSqlMigration(`
        CREATE INDEX auth_code_active_user_id ON auth_code (active, user_id, auth_code_id)
    `),

	newSqlMigration(`
        CREATE TABLE auth_code_session (
            auth_code_id uuid NOT NULL,
            auth_session_id uuid NOT NULL,

            PRIMARY KEY (auth_code_id, auth_session_id)
        )
    `),

	newSqlMigration(`
        CREATE TABLE auth_session (
            auth_session_id uuid NOT NULL,
            active bool NOT NULL DEFAULT true,

            network_id uuid NOT NULL,
            user_id uuid NOT NULL,

            PRIMARY KEY (auth_session_id)
        )
    `),

	newSqlMigration(`
        CREATE INDEX auth_session_active_network_id ON auth_session (active, network_id, auth_session_id)
    `),

	newSqlMigration(`
        CREATE INDEX auth_session_active_user_id ON auth_session (active, user_id, auth_session_id)
    `),

	newSqlMigration(`
        CREATE TABLE auth_session_expiration (
            network_id uuid NOT NULL,
            expire_time timestamp NOT NULL,

            PRIMARY KEY (network_id)
        )
    `),

	newSqlMigration(`
        CREATE INDEX search_projection_value ON search_projection (realm, value_id, value_variant, alias)
    `),

	newSqlMigration(`
        CREATE TABLE privacy_agent_request (
            privacy_agent_request_id uuid NOT NULL,
            create_time timestamp NOT NULL DEFAULT now(),
            country_of_residence varchar(256) NOT NULL,
            region_of_residence varchar(256) NOT NULL,
            correspondence_email varchar(256) NOT NULL,
            consent bool NOT NULL,
            email_text_to varchar(256) NOT NULL,
            email_text_subject text NOT NULL,
            email_text_body text NOT NULL,
            service_name varchar(256) NOT NULL,
            service_user varchar(256) NOT NULL,

            PRIMARY KEY (privacy_agent_request_id)
        )
    `),

	// ALTERED create_time timestamp with time zone -> timestamp
	newSqlMigration(`
        CREATE TABLE complete_privacy_policy (
            privacy_policy_id uuid NOT NULL,
            create_time timestamp with time zone NOT NULL DEFAULT now(),
            service_name varchar(256) NOT NULL,
            pending bool NOT NULL,
            privacy_policy_text text NULL,

            PRIMARY KEY (privacy_policy_id)
        )
    `),

	newSqlMigration(`
        CREATE TABLE complete_privacy_policy_service_url (
            privacy_policy_id uuid NOT NULL,
            service_url text NOT NULL
        )
    `),

	newSqlMigration(`
        CREATE INDEX complete_privacy_policy_service_url_privacy_policy_id ON complete_privacy_policy_service_url (privacy_policy_id)
    `),

	newSqlMigration(`
        CREATE TABLE complete_privacy_policy_extracted_url (
            privacy_policy_id uuid NOT NULL,
            extracted_url text NOT NULL
        )
    `),

	newSqlMigration(`
        CREATE INDEX complete_privacy_policy_extracted_url_privacy_policy_id ON complete_privacy_policy_extracted_url (privacy_policy_id)
    `),

	newSqlMigration(`
        CREATE TABLE latest_complete_privacy_policy (
            service_name varchar(256) NOT NULL,
            privacy_policy_id uuid NOT NULL,

            PRIMARY KEY (service_name)
        )
    `),

	newSqlMigration(`
        ALTER TABLE network_client ADD COLUMN device_id uuid NULL
    `),

	newSqlMigration(`
        CREATE INDEX network_client_device_id ON network_client (device_id, client_id)
    `),

	// RESIZED device_spec to varchar(256)
	// ALTERED create_time timestamp with time zone -> timestamp
	newSqlMigration(`
        CREATE TABLE device (
            device_id uuid NOT NULL,
            network_id uuid NOT NULL,
            device_name varchar(256) NOT NULL,
            device_spec varchar(64) NOT NULL,
            create_time timestamp with time zone NOT NULL DEFAULT now(),

            PRIMARY KEY(device_id)
        )
    `),

	newSqlMigration(`
        CREATE TABLE device_association_name (
            device_association_id uuid NOT NULL,
            network_id uuid NOT NULL,
            device_name varchar(256) NOT NULL,

            PRIMARY KEY(device_association_id, network_id)
        )
    `),

	// ALTERED add_time timestamp with time zone -> timestamp
	newSqlMigration(`
        CREATE TABLE device_add_history (
            network_id uuid NOT NULL,
            add_time timestamp with time zone NOT NULL DEFAULT now(),
            device_add_id uuid NOT NULL,
            code varchar(1024),
            device_association_id uuid NULL,

            PRIMARY KEY(network_id, add_time, device_add_id)
        )
    `),

	newSqlMigration(`
        CREATE TABLE device_association_code (
            device_association_id uuid NOT NULL,
            code varchar(1024),
            code_type varchar(16),

            PRIMARY KEY(device_association_id),
            UNIQUE(code)
        )
    `),

	newSqlMigration(`
        CREATE INDEX device_association_code_type ON device_association_code (code_type)
    `),

	// RESIZED device_spec to varchar(256)
	// ALTERED create_time timestamp with time zone -> timestamp
	// ALTERED expire_time timestamp with time zone -> timestamp
	newSqlMigration(`
        CREATE TABLE device_adopt (
            device_association_id uuid NOT NULL,
            adopt_secret varchar(1024) NOT NULL,
            device_name varchar(256) NOT NULL,
            device_spec varchar(64) NOT NULL,
            owner_network_id uuid NULL,
            owner_user_id uuid NULL,
            device_id uuid NULL,
            client_id uuid NULL,
            confirmed bool NOT NULL DEFAULT false,
            create_time timestamp with time zone NOT NULL DEFAULT now(),
            expire_time timestamp with time zone NULL,

            PRIMARY KEY(device_association_id)
        )
    `),

	newSqlMigration(`
        CREATE INDEX device_adopt_owner_network_id ON device_adopt (owner_network_id, confirmed)
    `),

	newSqlMigration(`
        CREATE TABLE device_adopt_auth_session (
            device_association_id uuid NOT NULL,
            auth_session_id uuid NOT NULL,

            PRIMARY KEY(device_association_id, auth_session_id)
        )
    `),

	// ALTERED create_time timestamp with time zone -> timestamp
	newSqlMigration(`
        CREATE TABLE device_share (
            device_association_id uuid NOT NULL,
            device_name varchar(256) NOT NULL,
            source_network_id uuid NOT NULL,
            guest_network_id uuid NULL,
            client_id uuid NULL,
            confirmed bool NOT NULL DEFAULT false,
            create_time timestamp with time zone NOT NULL DEFAULT now(),

            PRIMARY KEY(device_association_id)
        )
    `),

	newSqlMigration(`
        CREATE INDEX device_share_source_network_id ON device_share (source_network_id, confirmed)
    `),

	newSqlMigration(`
        CREATE INDEX device_share_guest_network_id ON device_share (guest_network_id, confirmed)
    `),

	newSqlMigration(`
        ALTER TABLE device_adopt ALTER COLUMN device_spec TYPE varchar(256)
    `),

	newSqlMigration(`
        ALTER TABLE device ALTER COLUMN device_spec TYPE varchar(256)
    `),

	newCodeMigration(migration_20240124_PopulateDevice),

	// ALTERED the run_at_block size is 1 second
	// extract(epoch ...) is epoch in seconds
	// https://www.postgresql.org/docs/current/functions-datetime.html#FUNCTIONS-DATETIME-EXTRACT
	newSqlMigration(`
        CREATE TABLE pending_task (
            task_id uuid NOT NULL,
            function_name varchar(1024) NOT NULL,
            args_json TEXT NOT NULL,
            client_address varchar(128) NOT NULL,
            client_by_jwt_json TEXT NULL,
            run_at timestamp NOT NULL,
            run_at_block bigint GENERATED ALWAYS AS (extract(epoch from run_at) / 300) STORED,
            run_once_key varchar(1024) NULL,
            run_priority integer NOT NULL,
            run_max_time_seconds integer NOT NULL,

            claim_time timestamp NOT NULL,
            release_time timestamp NOT NULL,

            reschedule_error TEXT NULL,
            has_reschedule_error bool GENERATED ALWAYS AS (reschedule_error IS NOT NULL) STORED,

            PRIMARY KEY (task_id),
            UNIQUE (run_once_key)
        )
    `),
	newSqlMigration(`
        CREATE INDEX pending_task_run_at_block ON pending_task (run_at_block, run_priority, run_at)
    `),
	newSqlMigration(`
        CREATE INDEX pending_task_has_reschedule_error ON pending_task (has_reschedule_error, task_id)
    `),
	newSqlMigration(`
        CREATE INDEX pending_task_release_time ON pending_task (release_time, task_id)
    `),

	newSqlMigration(`
        CREATE TABLE finished_task (
            task_id uuid NOT NULL,
            function_name varchar(1024) NOT NULL,
            args_json TEXT NOT NULL,
            client_address varchar(128) NOT NULL,
            client_by_jwt_json TEXT NULL,
            run_at timestamp NOT NULL,
            run_once_key varchar(1024) NULL,
            run_priority integer NOT NULL,
            run_max_time_seconds integer NOT NULL,

            run_start_time timestamp NOT NULL,
            run_end_time timestamp NOT NULL,

            reschedule_error TEXT NULL,
            result_json TEXT NOT NULL,

            post_error TEXT NULL,
            post_completed bool NOT NULL DEFAULT true,

            PRIMARY KEY (task_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX finished_task_run_end_time ON finished_task (run_end_time, task_id)
    `),

	newSqlMigration(`
        CREATE INDEX auth_code_end_time ON auth_code (end_time, auth_code_id)
    `),

	newSqlMigration(`
        ALTER TABLE network_client DROP COLUMN device_spec
    `),

	// user_auth is stored trimmed in lower case
	// prior to this migration there were some cases where the value was not normalized
	newSqlMigration(`
        UPDATE network_user
        SET
            user_auth = lower(trim(user_auth))
        WHERE
            user_auth IS NOT NULL
    `),

	newSqlMigration(`
        CREATE TABLE subscription_payment (
            subscription_payment_id uuid NOT NULL,
            network_id uuid NOT NULL,
            user_id uuid NOT NULL,
            create_time timestamp NOT NULL default now(),

            PRIMARY KEY(subscription_payment_id)
        )
    `),

	newSqlMigration(`
        CREATE INDEX subscription_payment_network_id_create_time ON subscription_payment (network_id, create_time)
    `),

	newSqlMigration(`
        ALTER TABLE transfer_balance ADD COLUMN purchase_token varchar(1024) NULL
    `),

	newSqlMigration(`
        CREATE INDEX transfer_balance_purchase_token ON transfer_balance (purchase_token, end_time, start_time, balance_id)
    `),

	newSqlMigration(`
        ALTER TABLE transfer_contract ADD COLUMN companion_contract_id uuid NULL
    `),

	newSqlMigration(`
        CREATE INDEX transfer_contract_open_source_id_companion_contract_id ON transfer_contract (open, source_id, destination_id, companion_contract_id, create_time, contract_id)
    `),

	newSqlMigration(`
        ALTER TABLE complete_privacy_policy ALTER COLUMN create_time TYPE timestamp
    `),

	newSqlMigration(`
        ALTER TABLE device ALTER COLUMN create_time TYPE timestamp
    `),

	newSqlMigration(`
        ALTER TABLE device_add_history ALTER COLUMN add_time TYPE timestamp
    `),

	newSqlMigration(`
        ALTER TABLE device_adopt ALTER COLUMN create_time TYPE timestamp
    `),

	newSqlMigration(`
        ALTER TABLE device_adopt ALTER COLUMN expire_time TYPE timestamp
    `),

	newSqlMigration(`
        ALTER TABLE device_share ALTER COLUMN create_time TYPE timestamp
    `),

	newSqlMigration(`
        ALTER TABLE contract_close ADD COLUMN checkpoint bool NOT NULL DEFAULT false
    `),

	newSqlMigration(`
        DROP INDEX transfer_contract_open_source_id_companion_contract_id
    `),

	newSqlMigration(`
        CREATE INDEX transfer_contract_open_source_id_companion_contract_id ON transfer_contract (open, source_id, destination_id, companion_contract_id, close_time, create_time, contract_id)
    `),

	newSqlMigration(`
        ALTER TABLE network_client_resident ALTER COLUMN resident_id SET NOT NULL
    `),

	newSqlMigration(`
        ALTER TABLE network_client_resident ALTER COLUMN resident_host SET NOT NULL
    `),

	newSqlMigration(`
        ALTER TABLE network_client_resident ALTER COLUMN resident_service SET NOT NULL
    `),

	newSqlMigration(`
        ALTER TABLE network_client_resident ALTER COLUMN resident_block SET NOT NULL
    `),

	newSqlMigration(`
        CREATE INDEX network_client_connected_client_id ON network_client_connection (connected, client_id)
    `),

	newSqlMigration(`
        CREATE INDEX network_client_network_id_active_client_id ON network_client (network_id, active, client_id)
    `),
	newSqlMigration(`
        CREATE INDEX network_client_network_id_create_time_client_id ON network_client (network_id, create_time, client_id)
    `),
	newSqlMigration(`
        DROP INDEX network_client_network_id_active
    `),
	newSqlMigration(`
        DROP INDEX network_client_network_id_create_time
    `),

	newSqlMigration(`
        CREATE TABLE network_referral_code (
            network_id uuid NOT NULL,
            referral_code uuid NOT NULL,

            PRIMARY KEY (network_id),
            UNIQUE (referral_code)
        )
    `),
	newCodeMigration(migration_20240725_PopulateNetworkReferralCodes),

	newSqlMigration(`
        ALTER TABLE transfer_contract ADD COLUMN payer_network_id uuid NULL
    `),

	newSqlMigration(`
        CREATE TABLE network_client_handler (
            handler_id uuid NOT NULL,
            heartbeat_time timestamp NOT NULL,
            handler_host varchar(128),

            PRIMARY KEY (handler_id)
        )
    `),

	newSqlMigration(`
        CREATE INDEX network_client_handler_heartbeat_time ON network_client_handler (heartbeat_time, handler_id)
    `),

	newSqlMigration(`
        ALTER TABLE network_client_connection ADD COLUMN handler_id uuid NULL
    `),

	newSqlMigration(`
        CREATE INDEX network_client_connection_handler_id ON network_client_connection (handler_id, connection_id)
    `),

	newSqlMigration(`
        CREATE INDEX network_client_connection_disconnect_time ON network_client_connection (disconnect_time, connection_id)
    `),

	newSqlMigration(`
        DELETE FROM network_client_connection WHERE handler_id IS NULL
    `),

	newSqlMigration(`
	    DROP TABLE client_provide
	`),

	newSqlMigration(`
        CREATE TABLE audit_account_payment (
            event_id uuid NOT NULL,
            event_time timestamp NOT NULL DEFAULT now(),
            payment_id uuid NOT NULL,
            event_type varchar(64) NOT NULL,
            event_details text NULL,

            PRIMARY KEY (event_id)
        )
    `),

	// adds circle_wallet_id and populates values with existing ids
	newSqlMigration(`
        ALTER TABLE account_wallet ADD COLUMN circle_wallet_id uuid NULL
    `),

	newCodeMigration(migration_20240802_AccountPaymentPopulateCircleWalletId),

	newSqlMigration(`
        ALTER TABLE network_client_location
            ADD COLUMN net_type_vpn smallint NOT NULL DEFAULT 0,
            ADD COLUMN net_type_proxy smallint NOT NULL DEFAULT 0,
            ADD COLUMN net_type_tor smallint NOT NULL DEFAULT 0,
            ADD COLUMN net_type_relay smallint NOT NULL DEFAULT 0,
            ADD COLUMN net_type_hosting smallint NOT NULL DEFAULT 0,
            ADD COLUMN net_type_score smallint GENERATED ALWAYS AS (
                net_type_vpn +
                net_type_proxy +
                net_type_tor +
                net_type_relay +
                net_type_hosting
            ) STORED
    `),

	newSqlMigration(`
        ALTER TABLE ip_location_lookup ADD COLUMN valid bool NOT NULL DEFAULT true
    `),

	newSqlMigration(`
        CREATE INDEX transfer_escrow_sweep_network_id_payment_id_payout ON transfer_escrow_sweep (network_id, payment_id, payout_byte_count)
    `),

	// `subsidy_net_revenue_nano_cents` is the amount used for subsidy calculation
	// It is not used for revenue share, which is `net_revenue_nano_cents`
	newSqlMigration(`
        ALTER TABLE transfer_balance
        ADD COLUMN subsidy_net_revenue_nano_cents bigint NOT NULL DEFAULT 0,
        ADD COLUMN paid bool GENERATED ALWAYS AS (0 < subsidy_net_revenue_nano_cents OR 0 < net_revenue_nano_cents) STORED
    `),

	newSqlMigration(`
        CREATE TABLE subsidy_payment (
            payment_plan_id uuid NOT NULL,

            start_time timestamp NOT NULL,
            end_time timestamp NOT NULL,

            active_user_count bigint NOT NULL,
            paid_user_count bigint NOT NULL,

            net_payout_byte_count_paid bigint NOT NULL,
            net_payout_byte_count_unpaid bigint NOT NULL,
            net_revenue_nano_cents bigint NOT NULL,

            net_payout_nano_cents bigint NOT NULL,

            PRIMARY KEY (payment_plan_id)
        )
    `),

	newSqlMigration(`
        CREATE INDEX subsidy_payment_end_time_start_time ON subsidy_payment (end_time, start_time)
    `),

	newSqlMigration(`
        ALTER TABLE transfer_contract ADD priority int NOT NULL DEFAULT 100
    `),

	newSqlMigration(`
        CREATE TABLE subscription_renewal (
            network_id uuid NOT NULL,
            subscription_type varchar(32) NOT NULL,
            start_time timestamp NOT NULL,
            end_time timestamp NOT NULL,
            net_revenue_nano_cents bigint NOT NULL,
            purchase_token varchar(1024) NULL,

            PRIMARY KEY (network_id, subscription_type, end_time, start_time)
        )
    `),

	newSqlMigration(`
        ALTER TABLE account_payment ADD subsidy_payout_nano_cents bigint NOT NULL DEFAULT 0
    `),

	newSqlMigration(`
        ALTER TABLE network_client_connection ADD client_address_hash BYTEA NULL
    `),

	newSqlMigration(`
        CREATE INDEX network_client_connection_client_address_hash_connected ON network_client_connection (client_address_hash,connected)
    `),

	newSqlMigration(`
        ALTER TABLE account_payment ADD network_id uuid NULL
    `),

	newSqlMigration(`
        ALTER TABLE account_payment ALTER COLUMN wallet_id DROP NOT NULL
    `),

	newSqlMigration(`
        ALTER TABLE network ADD COLUMN create_time timestamp NOT NULL default now()
    `),

	newSqlMigration(`
        ALTER TABLE network_client_location ADD COLUMN network_id uuid NULL
    `),

	newSqlMigration(`
        ALTER TABLE network ADD COLUMN guest_upgrade_network_id uuid NULL
    `),

	newSqlMigration(`
        CREATE TABLE network_referral (
            network_id uuid NOT NULL,
            referral_network_id uuid NOT NULL,

            PRIMARY KEY (network_id, referral_network_id)
        )
    `),

	newSqlMigration(`
        ALTER TABLE subscription_renewal ADD COLUMN market varchar(64) NULL
    `),

	newSqlMigration(`
        ALTER TABLE subscription_renewal ADD COLUMN transaction_id varchar(64) NULL
    `),

	newSqlMigration(`
        ALTER TABLE network_referral ADD COLUMN create_time timestamp NOT NULL DEFAULT now()
    `),

	newSqlMigration(`
        -- drop constraint
        ALTER TABLE network_referral_code DROP CONSTRAINT IF EXISTS network_referral_code_referral_code_key;

        -- add temp col to hold the string representation of the UUIDs
        ALTER TABLE network_referral_code ADD COLUMN temp_referral_code varchar(64);

        -- insert strings into temp col
        UPDATE network_referral_code SET temp_referral_code = referral_code::text WHERE referral_code IS NOT NULL;

        -- drop the old referral_code column
        ALTER TABLE network_referral_code DROP COLUMN referral_code;

        -- add the new column, allowing null initially
        ALTER TABLE network_referral_code ADD COLUMN referral_code varchar(64) NULL;

        -- copy values from temporary col
        UPDATE network_referral_code SET referral_code = temp_referral_code;

        -- drop the temporary column
        ALTER TABLE network_referral_code DROP COLUMN temp_referral_code;

        -- add check constraint to prevent empty strings
        ALTER TABLE network_referral_code ADD CONSTRAINT check_referral_code_not_empty CHECK (referral_code <> '');

        -- set not null constraint
        ALTER TABLE network_referral_code ALTER COLUMN referral_code SET NOT NULL;

        -- network constraint
        ALTER TABLE network_referral_code ADD CONSTRAINT network_referral_code_referral_code_key UNIQUE (referral_code);
    `),

	newCodeMigration(migration_20250402_ReferralCodeToAlphaNumeric),

	newSqlMigration(
		`ALTER TABLE account_feedback ADD COLUMN star_count integer NOT NULL DEFAULT 0;`,
	),

	// RENAMED: account_point
	newSqlMigration(`
      CREATE TABLE network_point (
            network_id uuid NOT NULL,
            event varchar(64) NOT NULL,
            point_value int NOT NULL,
            create_time timestamp NOT NULL DEFAULT now(),

            PRIMARY KEY (network_id, event, create_time)
        )
    `),

	newSqlMigration(`
        CREATE TABLE stripe_customer (
            network_id uuid NOT NULL,
            customer_id varchar(64) NOT NULL,
            create_time timestamp NOT NULL DEFAULT now(),

            PRIMARY KEY (network_id, customer_id)
        )
    `),

	newSqlMigration(`
        ALTER TABLE stripe_customer ADD CONSTRAINT stripe_customer_id_unique UNIQUE (customer_id)
    `),

	newSqlMigration(`
        ALTER TABLE stripe_customer ADD COLUMN stripe_customer_email text NULL
    `),

	newSqlMigration(`
        DROP TABLE IF EXISTS stripe_customer
    `),

	newSqlMigration(`
        ALTER TABLE network_user
        ADD COLUMN wallet_address text,
        ADD COLUMN wallet_blockchain text
    `),

	newSqlMigration(`
        CREATE UNIQUE INDEX network_user_wallet_address_unique ON network_user (wallet_address)
        WHERE wallet_address IS NOT NULL
    `),

	newSqlMigration(`
        ALTER TABLE account_wallet
        ADD CONSTRAINT unique_network_wallet_address
        UNIQUE (network_id, wallet_address)
    `),

	newSqlMigration(`
        ALTER TABLE account_wallet
        ADD COLUMN has_seeker_token boolean NOT NULL DEFAULT false
    `),

	newSqlMigration(`
        ALTER TABLE network
        ADD COLUMN leaderboard_public boolean NOT NULL DEFAULT false
    `),

	newSqlMigration(`
        ALTER TABLE network_point RENAME TO account_point
    `),

	newSqlMigration(`
		ALTER TABLE network_referral
        DROP CONSTRAINT network_referral_pkey,
	    ADD PRIMARY KEY (network_id)
	`),

	newSqlMigration(`
        ALTER TABLE network_client_location
            ADD COLUMN net_type_score_speed smallint GENERATED ALWAYS AS (
                1 +
                net_type_vpn +
                net_type_proxy +
                net_type_tor +
                net_type_relay
                - net_type_hosting
            ) STORED
    `),

	newSqlMigration(`
        CREATE INDEX network_client_connection__connected_client_id ON network_client_connection (connected, client_id)
    `),

	newSqlMigration(`
        CREATE INDEX provide_key_provide_mode_client_id ON provide_key (provide_mode, client_id)
    `),

	newSqlMigration(`
        CREATE INDEX network_client_location_client_id ON network_client_location (client_id)
    `),

	newSqlMigration(`
        ALTER INDEX network_client_connection__connected_client_id RENAME TO network_client_connection_connected_client_id
    `),

	// for some reason this index is not in the main db
	// there might have been a bad migration at some point in the past
	newSqlMigration(`
        DROP INDEX IF EXISTS network_admin_user_id
    `),

	newSqlMigration(`
        CREATE INDEX network_admin_user_id ON network (admin_user_id, network_id)
    `),

	newSqlMigration(`
        CREATE INDEX account_payment_network_id_canceled ON account_payment (network_id, canceled)
    `),

	newSqlMigration(`
        CREATE INDEX network_client_connection_connected_connection_id ON network_client_connection (connected, connection_id)
    `),
	newSqlMigration(`
		ALTER TABLE account_point
	    ALTER COLUMN point_value TYPE bigint,
	    ADD COLUMN payment_plan_id uuid NULL,
	    ADD COLUMN linked_network_id uuid NULL
	`),
	newSqlMigration(`
		ALTER TABLE account_point
		ADD COLUMN account_payment_id uuid NULL
	`),
	newSqlMigration(`
		ALTER TABLE account_payment
		ADD COLUMN tx_hash TEXT NULL
	`),

	newSqlMigration(`
        DROP INDEX pending_task_run_at_block
    `),

	newSqlMigration(`
        ALTER TABLE pending_task
        DROP COLUMN run_at_block,
        ADD COLUMN run_at_block bigint GENERATED ALWAYS AS (extract(epoch from run_at) / 60) STORED
    `),

	newSqlMigration(`
        ALTER TABLE pending_task
        ADD COLUMN available_block bigint GENERATED ALWAYS AS (CASE WHEN (release_time <= run_at) THEN (1 + (extract(epoch from run_at) / 60)) ELSE (1 + (extract(epoch from release_time) / 60)) END) STORED
    `),

	newSqlMigration(`
        CREATE INDEX pending_task_available_block ON pending_task (available_block, run_priority, task_id)
    `),
	newSqlMigration(`
		ALTER TABLE network
		ADD COLUMN contains_profanity BOOLEAN NOT NULL DEFAULT false
	`),

	newSqlMigration(`
        CREATE INDEX transfer_balance_network_id_active_paid_end_time ON transfer_balance (network_id, active, paid, end_time)
    `),

	newSqlMigration(`
        DROP INDEX transfer_balance_network_id_active_end_time
    `),

	newSqlMigration(`
        CREATE INDEX transfer_balance_network_id_active_end_time ON transfer_balance (network_id, active, end_time)
    `),

	newSqlMigration(`
        DROP INDEX transfer_balance_network_id_active_paid_end_time
    `),

	// clean up unused index
	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_connection_client_address_hash_connected
    `),
	// clean up unused index
	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_connection_connected_client_id
    `),
	// clean up unused index
	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_connection_connected_connection_id
    `),
	// clean up unused index
	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_connection_disconnect_time
    `),
	// clean up unused index
	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_network_id_create_time_client_id
    `),
	// clean up unused index
	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_resident_host_port
    `),
	// clean up unused index
	newSqlMigration(`
        DROP INDEX IF EXISTS pending_task_release_time
    `),
	// clean up unused index
	newSqlMigration(`
        DROP INDEX IF EXISTS transfer_balance_purchase_token
    `),
	// clean up unused index
	newSqlMigration(`
        DROP INDEX IF EXISTS transfer_contract_open_destination_id
    `),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_connection_connected_client_id ON network_client_connection (connected, client_id)
    `),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_connection_connected_connection_id ON network_client_connection (connected, connection_id)
    `),
	// clean up unused index
	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_connected_client_id
    `),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_connection_disconnect_time_connection_id ON network_client_connection (disconnect_time, connection_id)
    `),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_resident_host ON network_client_resident (resident_host, resident_id)
    `),

	// clean up unused index
	newSqlMigration(`
        DROP INDEX network_client_connection_disconnect_time_connection_id
    `),
	newSqlMigration(`
        CREATE INDEX network_client_connection_connected_disconnect_time ON network_client_connection (connected, disconnect_time)
    `),

	newSqlMigration(`
        ALTER TABLE network_client_connection
        ALTER COLUMN client_address SET DEFAULT '',
        ADD COLUMN client_address_port int NOT NULL DEFAULT 0
    `),

	newSqlMigration(`
        ALTER TABLE network_client_connection
        DROP COLUMN client_address
    `),

	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_contract_open_create_time ON transfer_contract (open, create_time, contract_id)
    `),

	newSqlMigration(`
        ALTER TABLE user_auth_attempt
        ADD COLUMN client_address_hash BYTEA,
        ADD COLUMN client_address_port int NOT NULL DEFAULT 0
    `),

	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS user_auth_attempt_client_address_hash_client_port_attempt_time ON user_auth_attempt (client_address_hash, client_address_port, attempt_time)
    `),

	newSqlMigration(`
        DROP INDEX user_auth_attempt_client_address
    `),

	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_connection_client_address_hash_connected ON network_client_connection (client_address_hash, connected)
    `),

	newSqlMigration(`
	    ALTER TABLE user_auth_attempt
	    DROP COLUMN client_ip,
	    DROP COLUMN client_port
	`),

	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS user_auth_attempt_attempt_time_user_auth_attempt_id ON user_auth_attempt (attempt_time, user_auth_attempt_id)
    `),

	// clean up unused index
	newSqlMigration(`
        DROP INDEX user_auth_attempt_client_ipv4
    `),
	newSqlMigration(`
        ALTER TABLE user_auth_attempt
        DROP COLUMN client_ipv4
    `),

	newSqlMigration(`
        ALTER TABLE pending_task
        DROP COLUMN run_at_block,
        ADD COLUMN run_at_block bigint GENERATED ALWAYS AS (extract(epoch from run_at)) STORED,
        DROP COLUMN available_block,
        ADD COLUMN available_block bigint GENERATED ALWAYS AS (CASE WHEN (release_time <= run_at) THEN (1 + extract(epoch from run_at)) ELSE (1 + extract(epoch from release_time)) END) STORED
    `),

	newSqlMigration(`
        CREATE TABLE network_client_connection_error (
            error_time timestamp NOT NULL DEFAULT now(),
            network_id uuid NOT NULL,
            client_id uuid NOT NULL,
            connection_id uuid NOT NULL,
            operation varchar(16) NOT NULL,
            error_message text NOT NULL
        )
    `),
	newSqlMigration(`
        CREATE INDEX network_client_connection_error_error_time_network_id_client_id ON network_client_connection_error (error_time, network_id, client_id)
    `),
	newSqlMigration(`
        CREATE INDEX network_client_connection_error_network_id_error_time ON network_client_connection_error (network_id, error_time)
    `),
	newSqlMigration(`
        CREATE INDEX network_client_connection_error_client_id_error_time ON network_client_connection_error (client_id, error_time)
    `),

	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_active_network_id_create_time ON network_client (active, network_id, create_time)
    `),
	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_network_id_active_client_id
    `),

	newSqlMigration(`
        ALTER TABLE network_client
        ADD COLUMN source_client_id uuid NULL
    `),

	newSqlMigration(`
        DROP TABLE network_client_connection_error
    `),

	newSqlMigration(`
		CREATE TABLE exclude_network_client_location (
        	network_id uuid NOT NULL,
            client_location_id uuid NOT NULL,

            PRIMARY KEY (network_id, client_location_id)
        )
    `),

	newSqlMigration(`
        CREATE INDEX exclude_network_client_location_client_location_id_network_id ON exclude_network_client_location (client_location_id, network_id)
    `),

	newSqlMigration(`
		CREATE TABLE network_user_auth_sso
		(
			user_id uuid NOT NULL,
			auth_type varchar(32) NOT NULL,
			auth_jwt text NOT NULL,
			user_auth varchar(256) NOT NULL,
			create_time timestamp NOT NULL DEFAULT now(),

			PRIMARY KEY (user_id, auth_type),
			UNIQUE (user_auth, auth_type)
		)
	`),
	newSqlMigration(`
		CREATE TABLE network_user_auth_password
		(
			user_id uuid NOT NULL,
			user_auth varchar(256) NOT NULL,
			auth_type varchar(32) NOT NULL,
			password_hash bytea NULL,
            password_salt bytea NULL,
            verified bool NOT NULL DEFAULT false,
			create_time timestamp NOT NULL DEFAULT now(),

			PRIMARY KEY (user_id, auth_type),
			UNIQUE (user_auth, auth_type)
		)
	`),
	newSqlMigration(`
		CREATE TABLE network_user_auth_wallet
		(
			user_id uuid NOT NULL,
			wallet_address text NOT NULL,
			blockchain varchar(32) NOT NULL,
			create_time timestamp NOT NULL DEFAULT now(),

			PRIMARY KEY (user_id),
			UNIQUE (wallet_address, blockchain)
		)
	`),

	// UPDATE: PK changed to (block_number, client_address_hash, client_id)
	newSqlMigration(`
        CREATE TABLE client_reliability (
            block_number bigint NOT NULL,
            client_address_hash BYTEA NOT NULL,
            network_id uuid NOT NULL,
            client_id uuid NOT NULL,
            connection_new_count bigint NOT NULL DEFAULT 0,
            connection_established_count bigint NOT NULL DEFAULT 0,
            provide_enabled_count bigint NOT NULL DEFAULT 0,
            provide_changed_count bigint NOT NULL DEFAULT 0,
            receive_message_count bigint NOT NULL DEFAULT 0,
            receive_byte_count bigint NOT NULL DEFAULT 0,
            send_message_count bigint NOT NULL DEFAULT 0,
            send_byte_count bigint NOT NULL DEFAULT 0,
            valid bool GENERATED ALWAYS AS (
                connection_new_count = 0 AND
                1 <= connection_established_count AND
                1 <= provide_enabled_count AND
                provide_changed_count = 0 AND
                1 <= receive_message_count
            ) STORED,

            PRIMARY KEY (block_number, client_address_hash, network_id, client_id)
        )
    `),

	newSqlMigration(`
        CREATE INDEX client_reliability_valid_block_number_client_address_hash ON client_reliability (valid, block_number, client_address_hash)
    `),

	// independent_reliability_score: the total reliability score independent of normalization by ip hash or block window [0, inf)
	// reliability_score: the total reliability score normalized by ip hash [0, inf)
	// reliability_weight: the total reliability score normalized by ip hash and block window [0, 1]
	// UPDATED: add min_block_number
	// UPDATED: add max_block_number
	// UPDATED: add city_location_id
	// UPDATED: add region_location_id
	// UPDATED: add country_location_id
	newSqlMigration(`
        CREATE TABLE client_connection_reliability_score (
            client_id uuid NOT NULL,
            independent_reliability_score double precision NOT NULL,
            reliability_score double precision NOT NULL,
            reliability_weight double precision NOT NULL,

            PRIMARY KEY (client_id)
        )
    `),

	// the network values are the sum of all client scores for the network:
	// independent_reliability_score, reliability_score, reliability_weight
	// all of these values range [0, inf)
	// UPDATED: add country_location_id
	// UPDATED: change PK to (network_id, country_location_id)
	newSqlMigration(`
        CREATE TABLE network_connection_reliability_score (
            network_id uuid NOT NULL,
            independent_reliability_score double precision NOT NULL,
            reliability_score double precision NOT NULL,
            reliability_weight double precision NOT NULL,

            PRIMARY KEY (network_id)
        )
    `),

	newSqlMigration(`
        CREATE TABLE provide_key_change (
            client_id uuid NOT NULL,
            change_time timestamp NOT NULL DEFAULT now(),

            PRIMARY KEY (client_id, change_time)
        )
    `),

	newSqlMigration(`
        ALTER TABLE network_client_location
            ADD COLUMN net_type_hosting2 smallint NOT NULL DEFAULT 0
    `),

	newSqlMigration(`
        ALTER TABLE client_connection_reliability_score
            ADD COLUMN min_block_number bigint NOT NULL DEFAULT 0,
            ADD COLUMN max_block_number bigint NOT NULL DEFAULT 0
    `),

	newSqlMigration(`
        ALTER TABLE network_connection_reliability_score
            ADD COLUMN min_block_number bigint NOT NULL DEFAULT 0,
            ADD COLUMN max_block_number bigint NOT NULL DEFAULT 0
    `),

	newSqlMigration(`
        ALTER TABLE account_payment
        ADD COLUMN reliability_subsidy_nano_cents bigint NOT NULL DEFAULT 0
    `),

	newSqlMigration(`
        CREATE TABLE network_connection_reliability_window (
            network_id uuid NOT NULL,
            bucket_number bigint NOT NULL,
            reliability_weight double precision NOT NULL DEFAULT 0,
            client_count int NOT NULL DEFAULT 0,
            total_client_count int NOT NULL DEFAULT 0,

            PRIMARY KEY (network_id, bucket_number)
        )
    `),

	newSqlMigration(`
        ALTER TABLE client_reliability
            DROP CONSTRAINT client_reliability_pkey,
            ADD PRIMARY KEY (block_number, client_address_hash, client_id)
    `),

	newSqlMigration(`
        ALTER TABLE client_connection_reliability_score
            ADD COLUMN city_location_id uuid NOT NULL DEFAULT gen_random_uuid(),
            ADD COLUMN region_location_id uuid NOT NULL DEFAULT gen_random_uuid(),
            ADD COLUMN country_location_id uuid NOT NULL DEFAULT gen_random_uuid()
    `),

	newSqlMigration(`
        ALTER TABLE network_connection_reliability_score
            ADD COLUMN country_location_id uuid NOT NULL DEFAULT gen_random_uuid(),
            DROP CONSTRAINT network_connection_reliability_score_pkey,
            ADD PRIMARY KEY (network_id, country_location_id)
    `),

	// this table tells us if a connected client is valid (not messed up/suspicious from a routing perspective)
	// it has data from at least once connection for a disconnected client
	// it does not tell us if a disconnected client has ever been valid. The data is incomplete for this question.
	newSqlMigration(`
        CREATE TABLE network_client_location_reliability (
            client_id uuid NOT NULL,
            update_block_number bigint NOT NULL,
            city_location_id uuid,
            region_location_id uuid,
            country_location_id uuid,
            client_address_hash_count int NOT NULL DEFAULT 0,
            location_count int NOT NULL DEFAULT 0,
            valid bool GENERATED ALWAYS AS (
                country_location_id IS NOT NULL AND
                client_address_hash_count = 1 AND
                location_count = 1
            ) STORED,
            connected bool NOT NULL DEFAULT false,
            max_net_type_score smallint NOT NULL DEFAULT 0,
            max_net_type_score_speed smallint NOT NULL DEFAULT 0,

            PRIMARY KEY (client_id)
        )
    `),

	// DROPPED
	newSqlMigration(`
        CREATE INDEX network_client_location_reliability_valid_client_id ON network_client_location_reliability (valid, client_id)
    `),

	// DROPPED
	// RENAMED to network_client_location_reliability_connected_valid_country
	newSqlMigration(`
        CREATE INDEX network_client_location_reliability_connected_valid_location_id_client_id ON network_client_location_reliability (connected, valid, country_location_id, client_id)
    `),

	newSqlMigration(`
        CREATE INDEX network_client_location_reliability_update_block_number_client_id ON network_client_location_reliability (update_block_number, client_id)
    `),

	newSqlMigration(`
        CREATE TABLE network_client_location_reliability_multiplier (
            country_location_id uuid NOT NULL,
            reliability_multiplier double precision NOT NULL DEFAULT 1,

            PRIMARY KEY (country_location_id)
        )
    `),

	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS account_payment_completed_complete_time_contract_id ON account_payment (completed, complete_time)
    `),

	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_location_reliability_connected_client_id ON network_client_location_reliability (connected, client_id)
    `),

	newSqlMigration(`
        ALTER TABLE network_client_location_reliability
            ADD COLUMN network_id uuid NOT NULL DEFAULT gen_random_uuid()
    `),

	newSqlMigration(`
        ALTER INDEX network_client_location_reliability_connected_valid_location_id_client_id RENAME TO network_client_location_reliability_connected_valid_country
    `),

	// DROPPED
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_location_reliability_connected_valid_region ON network_client_location_reliability (connected, valid, region_location_id, client_id)
    `),

	// DROPPED
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_location_reliability_connected_valid_city ON network_client_location_reliability (connected, valid, city_location_id, client_id)
    `),

	// DROPPED
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_location_reliability_connected_valid_client_id ON network_client_location_reliability (connected, valid, client_id)
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_location_reliability_connected_client_id
    `),

	newSqlMigration(`
        CREATE TABLE network_client_latency (
            connection_id uuid NOT NULL,
            latency_ms integer NOT NULL,

            PRIMARY KEY (connection_id)
        )
    `),

	newSqlMigration(`
        CREATE TABLE network_client_speed (
            connection_id uuid NOT NULL,
            bytes_per_second bigint NOT NULL,

            PRIMARY KEY (connection_id)
        )
    `),

	newSqlMigration(`
        ALTER TABLE network_client_connection
            ADD COLUMN expected_latency_ms integer NOT NULL DEFAULT 0
    `),

	// relative latency is actual - expected
	newSqlMigration(`
        ALTER TABLE network_client_location_reliability
            ADD column max_bytes_per_second bigint NOT NULL DEFAULT 0,
            ADD column min_relative_latency_ms integer NOT NULL DEFAULT 0
    `),

	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_location_reliability_valid_connected_client_id ON network_client_location_reliability (valid, connected, client_id)
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_location_reliability_connected_valid_country
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_location_reliability_connected_valid_region
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_location_reliability_connected_valid_city
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_location_reliability_valid_client_id
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_location_reliability_connected_valid_client_id
    `),

	newSqlMigration(`
		CREATE TABLE solana_payment_intent (
		    payment_reference text PRIMARY KEY,
		    network_id uuid NOT NULL,
		    created_at timestamp NOT NULL DEFAULT now(),
		    expires_at timestamp,
		    tx_signature text
		);

        CREATE INDEX IF NOT EXISTS solana_payment_intent_payment_reference
            ON solana_payment_intent (payment_reference)
            WHERE tx_signature IS NULL;

        CREATE UNIQUE INDEX IF NOT EXISTS solana_payment_intent_tx_signature
            ON solana_payment_intent (tx_signature)
            WHERE tx_signature IS NOT NULL;
    `),

	newSqlMigration(`
        ALTER TABLE network_user_auth_sso
            ADD COLUMN product_updates_sync bool NOT NULL DEFAULT false
    `),

	newSqlMigration(`
        ALTER TABLE network_user_auth_password
            ADD COLUMN product_updates_sync bool NOT NULL DEFAULT false
    `),

	newSqlMigration(`
        CREATE INDEX network_user_auth_sso_product_updates_sync_user_id ON network_user_auth_sso (product_updates_sync, user_id)
    `),

	newSqlMigration(`
        CREATE INDEX network_user_auth_password_product_updates_sync_user_id ON network_user_auth_password (product_updates_sync, user_id)
    `),

	newSqlMigration(`
        DROP TABLE ip_location_lookup
    `),

	newSqlMigration(`
        ALTER TABLE network
            ADD COLUMN product_updates_sync bool NOT NULL DEFAULT false
    `),

	newSqlMigration(`
        CREATE INDEX network_product_updates_sync_admin_user_id ON network (product_updates_sync, admin_user_id)
    `),

	newSqlMigration(`
        ALTER TABLE network_client_location_reliability
            ADD COLUMN has_speed_test bool DEFAULT true,
            ADD COLUMN has_latency_test bool DEFAULT true
    `),

	newSqlMigration(`
		CREATE TABLE stripe_customer (
		    network_id uuid NOT NULL,
		    stripe_customer_id varchar(64) NOT NULL UNIQUE,
		    create_time timestamp NOT NULL DEFAULT now(),
		    PRIMARY KEY (network_id, stripe_customer_id)
		)
    `),

	newSqlMigration(`
	    ALTER TABLE stripe_customer
        ADD CONSTRAINT stripe_customer_network_id_unique UNIQUE (network_id)
	`),

	newSqlMigration(`
        ALTER TABLE network_client_location
            ADD COLUMN net_type_privacy smallint NOT NULL DEFAULT 0,
            ADD COLUMN net_type_virtual smallint NOT NULL DEFAULT 0,
            DROP COLUMN net_type_score,
            ADD COLUMN net_type_score smallint GENERATED ALWAYS AS (
                net_type_privacy +
                net_type_virtual +
                net_type_hosting
            ) STORED,
            DROP COLUMN net_type_score_speed,
            ADD COLUMN net_type_score_speed smallint GENERATED ALWAYS AS (
                1 +
                net_type_privacy +
                net_type_virtual +
                - net_type_hosting
            ) STORED
    `),

	newSqlMigration(`
	    ALTER TABLE network_client_location
	        DROP COLUMN net_type_vpn,
	        DROP COLUMN net_type_proxy,
	        DROP COLUMN net_type_tor,
	        DROP COLUMN net_type_relay,
	        DROP COLUMN net_type_hosting2
	`),

	newSqlMigration(`
        CREATE TABLE search_value_update (
            realm varchar(32) NOT NULL,
            update_id bigint GENERATED ALWAYS AS IDENTITY,
            value_id uuid NOT NULL,
            value_variant int,
            value varchar(1024),
            remove bool NOT NULL DEFAULT false,

            PRIMARY KEY (realm, update_id)
        )
    `),

	newSqlMigration(`
        CREATE UNIQUE INDEX search_value_update_realm_value_id_variant ON search_value_update (realm, value_id, value_variant)
    `),

	newSqlMigration(`
        ALTER TABLE network_client_location
            ADD COLUMN net_type_foreign smallint NOT NULL DEFAULT 0,
            DROP COLUMN net_type_score_speed,
            ADD COLUMN net_type_score_speed smallint GENERATED ALWAYS AS (
                net_type_privacy +
                net_type_virtual
            ) STORED
    `),

	newSqlMigration(`
        ALTER TABLE network_client_location
            DROP COLUMN net_type_score,
            ADD COLUMN net_type_score smallint GENERATED ALWAYS AS (
                net_type_privacy +
                net_type_virtual +
                net_type_hosting +
                net_type_foreign
            ) STORED
    `),

	newSqlMigration(`
		CREATE TABLE payment_report (
			payment_plan_id uuid NOT NULL,
			total_payout_nano_cents bigint NOT NULL,
			total_nano_points bigint NOT NULL,
			point_scale_factor double precision NOT NULL,
			payout_points_per_payout int NOT NULL,
			time_scale_factor double precision NOT NULL,
			create_time timestamp NOT NULL DEFAULT now(),

            PRIMARY KEY (payment_plan_id)
        )
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS transfer_balance_network_id_active_end_time
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS transfer_contract_open_source_id_companion_contract_id
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_location_city_client_id
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_location_country_client_id
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_location_region_client_id
    `),

	newSqlMigration(`
        ALTER TABLE network_client_resident_port
        DROP CONSTRAINT network_client_resident_port_pkey,
        ADD PRIMARY KEY (resident_id, resident_internal_port),
        ALTER COLUMN client_id DROP NOT NULL
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_resident_host
    `),

	newSqlMigration(`
        ALTER TABLE network_client_resident
        DROP CONSTRAINT network_client_resident_pkey,
        ADD PRIMARY KEY (resident_id),
        DROP CONSTRAINT network_client_resident_resident_id_key,
        ADD UNIQUE (client_id)
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_connection_client_id_connected
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_connection_connected_connection_id
    `),

	newSqlMigration(`
	    ALTER TABLE network_client_resident_port
	    DROP COLUMN client_id
	`),

	newSqlMigration(`
        DROP TABLE privacy_agent_request
    `),

	newSqlMigration(`
        DROP TABLE complete_privacy_policy
    `),

	newSqlMigration(`
        DROP TABLE complete_privacy_policy_service_url
    `),

	newSqlMigration(`
        DROP TABLE complete_privacy_policy_extracted_url
    `),

	newSqlMigration(`
        DROP TABLE latest_complete_privacy_policy
    `),

	newSqlMigration(`
        CREATE TABLE network_connection_reliability_window_score (
            network_id uuid NOT NULL,
            independent_reliability_score double precision NOT NULL,
            reliability_score double precision NOT NULL,
            reliability_weight double precision NOT NULL,
            min_block_number bigint NOT NULL DEFAULT 0,
            max_block_number bigint NOT NULL DEFAULT 0,
            country_location_id uuid NOT NULL DEFAULT gen_random_uuid(),

            PRIMARY KEY (network_id, country_location_id)
        )
    `),

	newSqlMigration(`
        ALTER TABLE account_point
        ADD COLUMN account_point_id uuid NOT NULL default gen_random_uuid()
    `),

	newSqlMigration(`
	    CREATE INDEX IF NOT EXISTS account_point_network_id_event ON account_point (network_id, event, create_time, account_point_id)
	`),

	newSqlMigration(`
	    ALTER TABLE account_point
	    DROP CONSTRAINT network_point_pkey,
	    ADD PRIMARY KEY (account_point_id)
	`),

	newSqlMigration(`
        ALTER TABLE network_client_resident
        ADD COLUMN internal_ports VARCHAR(128) NOT NULL DEFAULT ''
    `),

	newSqlMigration(`
        ALTER TABLE network_client_resident
        ADD COLUMN create_time timestamp NOT NULL DEFAULT now()
    `),

	newSqlMigration(`
        ALTER TABLE network_client_resident
        DROP CONSTRAINT network_client_resident_pkey,
        ADD PRIMARY KEY (client_id),
        DROP CONSTRAINT network_client_resident_client_id_key,
        ADD UNIQUE (resident_id)
    `),

	newSqlMigration(`
    ALTER TABLE client_connection_reliability_score
    ADD COLUMN independent_reliability_weight double precision NOT NULL DEFAULT 0
    `),

	newSqlMigration(`
    ALTER TABLE network_connection_reliability_score
    ADD COLUMN independent_reliability_weight double precision NOT NULL DEFAULT 0
    `),

	newSqlMigration(`
    ALTER TABLE network_connection_reliability_window_score
    ADD COLUMN independent_reliability_weight double precision NOT NULL DEFAULT 0
    `),

	newSqlMigration(`
	DROP TABLE network_client_resident
    `),

	newSqlMigration(`
	DROP TABLE network_client_resident_port
    `),

	newSqlMigration(`
        ALTER TABLE network_client_location
        DROP COLUMN net_type_score_speed,
        ADD COLUMN net_type_score_speed smallint GENERATED ALWAYS AS (
            net_type_privacy +
            net_type_virtual +
            net_type_foreign
        ) STORED
    `),

	newSqlMigration(`
        ALTER TABLE network_client_latency
        ADD COLUMN sample_count bigint NOT NULL DEFAULT 1
    `),

	newSqlMigration(`
        ALTER TABLE network_client_speed
        ADD COLUMN sample_count bigint NOT NULL DEFAULT 1
    `),

	newSqlMigration(`
		CREATE INDEX IF NOT EXISTS transfer_contract_open_payer_network_id_transfer_byte_count
        ON transfer_contract (open, payer_network_id, transfer_byte_count)
	`),

	newSqlMigration(`
		CREATE INDEX IF NOT EXISTS transfer_balance_active_network_id_start_end_time
        ON transfer_balance (active, network_id, start_time, end_time)
	`),

	newSqlMigration(`
		CREATE INDEX IF NOT EXISTS subscription_renewal_network_type_start_end
		ON subscription_renewal (network_id, subscription_type, start_time, end_time)
	`),

	newSqlMigration(`
		ALTER TABLE subscription_renewal ALTER COLUMN transaction_id TYPE VARCHAR(128);
	`),

	newSqlMigration(`
        ALTER TABLE client_connection_reliability_score
        ADD COLUMN lookback_index integer NOT NULL DEFAULT 0,
        DROP CONSTRAINT client_connection_reliability_score_pkey,
        ADD PRIMARY KEY (client_id, lookback_index)
    `),

	newSqlMigration(`
        CREATE TABLE proxy_device_config (
            proxy_id uuid NOT NULL,
            client_id uuid NOT NULL,
            instance_id uuid NOT NULL,
            config_json TEXT NOT NULL,

            PRIMARY KEY (proxy_id)
        )
    `),

	newSqlMigration(`
        CREATE UNIQUE INDEX proxy_device_config_client_id_instance_id ON proxy_device_config (client_id, instance_id)
    `),

	newSqlMigration(`
		ALTER TABLE transfer_balance_code ADD COLUMN network_id uuid NULL;
    `),

	newSqlMigration(`
		CREATE INDEX transfer_balance_code_network_id_end_time
		ON transfer_balance_code(network_id, end_time);
    `),

	newSqlMigration(`
        CREATE TABLE proxy_client (
            proxy_id uuid NOT NULL,
            client_id uuid NOT NULL,
            instance_id uuid NOT NULL,
            proxy_host varchar(128) NOT NULL,
            block varchar(128) NOT NULL,
            client_ipv4 bigint NULL,
            client_public_key varchar(128) NULL,
            proxy_client_json text NOT NULL,

            PRIMARY KEY (proxy_id)
        )
    `),

	newSqlMigration(`
        CREATE UNIQUE INDEX proxy_client_proxy_host_block_client_ipv4 ON proxy_client (proxy_host, block, client_ipv4)
    `),

	newSqlMigration(`
        CREATE UNIQUE INDEX proxy_client_proxy_host_block_client_public_key ON proxy_client (proxy_host, block, client_public_key)
    `),

	newSqlMigration(`
        CREATE INDEX proxy_client_client_id_instance_id_proxy_id ON proxy_client (client_id, instance_id, proxy_id)
    `),

	newSqlMigration(`
        CREATE TABLE proxy_client_change (
            proxy_host varchar(128) NOT NULL,
            block varchar(128) NOT NULL,
            change_id bigint GENERATED ALWAYS AS IDENTITY,
            proxy_id uuid NOT NULL,

            PRIMARY KEY (proxy_host, block, change_id)
        )
    `),

	newSqlMigration(`
        CREATE TABLE proxy_client_ipv4 (
            sequence_id bigint NOT NULL,
            client_ipv4 bigint NOT NULL,

            PRIMARY KEY (sequence_id, client_ipv4)
        )
    `),

	newSqlMigration(`
        CREATE UNIQUE INDEX IF NOT EXISTS proxy_client_client_id ON proxy_client (client_id)
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS proxy_client_client_id_instance_id_proxy_id
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS transfer_contract_open_payer_network_id_transfer_byte_count
    `),

	newSqlMigration(`
        DROP INDEX IF EXISTS transfer_contract_open_create_time
    `),

	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_contract_create_time ON transfer_contract (create_time, open, contract_id)
    `),

	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_contract_payer_network_id ON transfer_contract (payer_network_id, open, contract_id)
    `),

	newSqlMigration(`
        CREATE TABLE account_api_key (
            api_key_id uuid NOT NULL DEFAULT gen_random_uuid(),
            network_id uuid NOT NULL,
            api_key varchar(128) NOT NULL,
            name varchar(128) NOT NULL DEFAULT '',
            create_time timestamp NOT NULL DEFAULT now(),

            PRIMARY KEY (api_key_id),
            UNIQUE (api_key)
        )
    `),
	newSqlMigration(`
        CREATE INDEX account_api_key_network_id ON account_api_key (network_id, api_key_id)
    `),

	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_contract_open_payer_network_id_transfer_byte_count
        ON transfer_contract (open, payer_network_id, transfer_byte_count)
    `),

	// Per-client TLS certificate for the encryption handshake, keyed on
	// `client_id`, published via `EncryptedKey`. `tls_certificate_pem` is
	// concatenated PEM (leaf first); `client_key_signed_tls_certificate` is the
	// client's Ed25519 signature over the chain (nullable for older clients).
	newSqlMigration(`
        CREATE TABLE client_tls_certificate (
            client_id uuid NOT NULL,
            tls_certificate_pem bytea NOT NULL,
            client_key_signed_tls_certificate bytea NULL,
            set_time timestamp NOT NULL,

            PRIMARY KEY (client_id)
        )
    `),

	// a disputed contract is not `open` (`open` is generated as
	// `dispute = false AND outcome IS NULL`), so the expired dispute scan in
	// `ForceCloseOpenContractIds` needs its own index
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_contract_dispute_create_time
        ON transfer_contract (dispute, outcome, create_time)
    `),

	newSqlMigration(`
		CREATE TABLE wallet_auth_challenge (
			challenge_id uuid NOT NULL,
			challenge_value varchar(128) NOT NULL,
			wallet_address text NULL,
			blockchain varchar(32) NULL,
			create_time timestamp NOT NULL DEFAULT now(),
			expire_time timestamp NOT NULL,
			used bool NOT NULL DEFAULT false,

			PRIMARY KEY (challenge_id),
			UNIQUE (challenge_value)
		)
	`),
	newSqlMigration(`
		CREATE INDEX wallet_auth_challenge_expire_time_used
		ON wallet_auth_challenge (expire_time, used)
	`),
	newSqlMigration(`
		CREATE TABLE wallet_auth_challenge_attempt (
			wallet_auth_challenge_attempt_id uuid NOT NULL,
			client_address_hash bytea NOT NULL,
			client_address_port int NOT NULL,
			attempt_time timestamp NOT NULL DEFAULT now(),
			success bool NOT NULL DEFAULT false,

			PRIMARY KEY (wallet_auth_challenge_attempt_id)
		)
	`),
	newSqlMigration(`
		CREATE INDEX wallet_auth_challenge_attempt_client_address_hash_port_attempt_time
		ON wallet_auth_challenge_attempt (client_address_hash, client_address_port, attempt_time)
	`),

	// `/verify` published proofs (sn/VALIDATOR.md §6.2): one row per
	// completed (status=1) or expired (status=2) trail. `hops_json` is
	// `[{"client_id", "time_ms"}, ...]`; sigs are null when expired. Poison
	// trails (§9) are never written here.
	newSqlMigration(`
        CREATE TABLE verify_trail (
            trail_id uuid PRIMARY KEY,
            vpk bytea NOT NULL,
            server_key_id smallint NOT NULL,
            server_nonce bytea NOT NULL,
            depth smallint NOT NULL,
            status smallint NOT NULL,
            hops_json text NOT NULL,
            final_sig bytea,
            verifier_sig bytea,
            create_time timestamp NOT NULL,
            complete_time timestamp
        )
    `),

	// `/verify` per-provider stat rollups (sn/VALIDATOR.md §7), upserted
	// periodically from the redis histograms. This exact shape is consumed by
	// the subnet scoring pipeline — do not change columns in place.
	newSqlMigration(`
        CREATE TABLE verify_provider_stats (
            period_start timestamp NOT NULL,
            period_end timestamp NOT NULL,
            client_id uuid NOT NULL,
            assignments bigint NOT NULL,
            confirmations bigint NOT NULL,
            latency_p50_ms int,
            latency_p90_ms int,
            latency_p99_ms int,

            PRIMARY KEY (period_start, client_id)
        )
    `),

	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS verify_provider_stats_client_id_period_start
        ON verify_provider_stats (client_id, period_start)
    `),

	// Subtensor (st) settlement subsystem (PLAN.md §5/§6).
	// `st_wallet` is a network's subnet claim wallet — deliberately separate
	// from account_wallet/payout_wallet so the USDC payout planner never
	// sees subnet wallets (D-2).
	newSqlMigration(`
        CREATE TABLE st_wallet (
            network_id uuid NOT NULL,
            coldkey_ss58 varchar(64) NOT NULL,
            coldkey_pubkey bytea NOT NULL,
            set_time timestamp NOT NULL DEFAULT now(),

            PRIMARY KEY (network_id)
        )
    `),

	// mirror of the contract epoch machine; all *_block columns are contract
	// (EVM) block numbers — the contract clock is authoritative.
	// status: open | closed | committed | finalized
	newSqlMigration(`
        CREATE TABLE st_epoch (
            epoch bigint NOT NULL,
            start_block bigint NOT NULL,
            commit_deadline_block bigint NOT NULL,
            trails_deadline_block bigint NOT NULL,
            finalize_block bigint NOT NULL,
            status varchar(16) NOT NULL,
            finalized_time timestamp NULL,

            PRIMARY KEY (epoch)
        )
    `),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS st_epoch_status ON st_epoch (status, epoch)
    `),

	// one payout tree leaf per (epoch, no_id, coldkey) — the contract dedups
	// miner claims by (noId, coldkey). leaf_index is the deterministic input
	// order used to rebuild the tree byte-exactly. network_id is a
	// representative contributing network (informational).
	newSqlMigration(`
        CREATE TABLE st_payout_leaf (
            epoch bigint NOT NULL,
            no_id bigint NOT NULL,
            network_id uuid NOT NULL,
            coldkey bytea NOT NULL,
            share_bps int NOT NULL,
            leaf_index int NOT NULL,

            PRIMARY KEY (epoch, no_id, leaf_index),
            UNIQUE (epoch, no_id, coldkey)
        )
    `),

	// one row per attempted chain write (commit/deposit/finalize).
	// status: pending | confirmed | failed | skipped
	newSqlMigration(`
        CREATE TABLE st_publish (
            publish_id uuid NOT NULL,
            epoch bigint NOT NULL,
            kind varchar(32) NOT NULL,
            tx_hash varchar(80) NULL,
            status varchar(16) NOT NULL,
            error varchar(1024) NULL,
            create_time timestamp NOT NULL DEFAULT now(),
            update_time timestamp NOT NULL DEFAULT now(),

            PRIMARY KEY (publish_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS st_publish_epoch_kind ON st_publish (epoch, kind, create_time)
    `),

	// mirrored contract event log (eth_getLogs sync, SP-4). kind is the
	// contract event name; data_json the decoded args.
	newSqlMigration(`
        CREATE TABLE st_event (
            block_number bigint NOT NULL,
            log_index int NOT NULL,
            tx_hash varchar(80) NOT NULL,
            kind varchar(64) NOT NULL,
            data_json text NOT NULL,

            PRIMARY KEY (block_number, log_index)
        )
    `),

	// single-row high-water mark for the event sync (next block to scan)
	newSqlMigration(`
        CREATE TABLE st_chain_sync (
            singleton_id int NOT NULL DEFAULT 1 CHECK (singleton_id = 1),
            high_water_block bigint NOT NULL,
            update_time timestamp NOT NULL DEFAULT now(),

            PRIMARY KEY (singleton_id)
        )
    `),

	// the epoch share computation scans sweeps by time window
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_escrow_sweep_sweep_time
        ON transfer_escrow_sweep (sweep_time)
    `),

	// mirror of the on-chain head-binding registry (WHITEPAPER §8.4/§11.4):
	// a provider promoted to the head tier, keyed by its client public key
	// (ckey — the 32-byte Ed25519 key GetClientPublicKey returns, the
	// contract's `clientId`) and bound to a head-tier hotkey/uid. Driven by
	// HeadBound/HeadUnbound events in block order; `active` is false after an
	// unbind. update_block is the contract block of the last transition and
	// guards out-of-order replays. The epoch-close payout excludes active
	// ckeys from every pool (paid natively by Yuma — never paid twice). The
	// set is small (~200) so a full active read per close is fine.
	newSqlMigration(`
        CREATE TABLE st_head_binding (
            ckey bytea NOT NULL,
            hotkey bytea NOT NULL,
            uid bigint NOT NULL,
            active bool NOT NULL,
            update_block bigint NOT NULL,
            update_time timestamp NOT NULL DEFAULT now(),

            PRIMARY KEY (ckey)
        )
    `),

	// stable per-payment idempotency key for the payment processor submit.
	// Created on the first submit attempt and reused on retries, so a crash
	// between the processor call and recording `payment_record` cannot
	// double-send funds. Cleared together with `payment_record` when a failed
	// transaction is reset for a fresh attempt.
	newSqlMigration(`
        ALTER TABLE account_payment
        ADD COLUMN circle_idempotency_key uuid NULL
    `),

	// Per-provider statistics APIs (/stats/providers, /stats/providers-last-n,
	// /stats/provider-last-n) aggregate transfer_contract by destination_id
	// (the provider client) over a time window. These indexes turn the
	// per-provider scans into index ranges instead of full-table scans.
	// transfer bytes bucket by close_time (settled); contracts/clients by
	// create_time (opened).
	//
	// On the large existing tables (transfer_contract, network_client_connection)
	// these must be built manually with CREATE INDEX CONCURRENTLY out of band —
	// migrations run inside a transaction, where CONCURRENTLY is illegal and a
	// plain CREATE INDEX takes a write-blocking lock (see FIXME above). The
	// IF NOT EXISTS gate makes this migration a no-op once they are pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_contract_destination_id_create_time
        ON transfer_contract (destination_id, create_time)
    `),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_contract_destination_id_close_time
        ON transfer_contract (destination_id, close_time)
    `),
	// per-provider uptime / connected-events scans by client over a window
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_connection_client_id_connect_time
        ON network_client_connection (client_id, connect_time)
    `),

	// search-interest rollup. FindProviders2 increments a per-provider match
	// counter in redis on the hot path (never writes pg); RollupSearchProviderStats
	// drains those counters into this table once per hour. Same (period_start,
	// client_id) rollup shape as verify_provider_stats.
	newSqlMigration(`
        CREATE TABLE search_provider_stats (
            period_start timestamp NOT NULL,
            period_end timestamp NOT NULL,
            client_id uuid NOT NULL,
            match_count bigint NOT NULL,

            PRIMARY KEY (period_start, client_id)
        )
    `),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS search_provider_stats_client_id_period_start
        ON search_provider_stats (client_id, period_start)
    `),

	// wallet-login challenge nonces. Single-use, short-lived server-issued nonces
	// that the client includes in the message it signs for wallet login, so a
	// captured (message, signature) pair cannot be replayed (see handleLoginWallet).
	newSqlMigration(`
        CREATE TABLE auth_wallet_nonce (
            nonce varchar(256) NOT NULL,
            create_time timestamp NOT NULL DEFAULT now(),
            expire_time timestamp NOT NULL,
            used bool NOT NULL DEFAULT false,

            PRIMARY KEY (nonce)
        )
    `),

	// the task poll orders by (available_block, run_priority DESC,
	// run_max_time_seconds DESC) FOR UPDATE SKIP LOCKED. The old index
	// (available_block, run_priority, task_id) cannot produce that order
	// (mixed sort directions), so every poll fetched and sorted the entire
	// ready backlog before applying LIMIT. This index matches the poll's sort
	// exactly, so the poll streams in index order and stops at LIMIT.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS pending_task_poll_order
        ON pending_task (available_block, run_priority DESC, run_max_time_seconds DESC)
    `),
	newSqlMigration(`
        DROP INDEX IF EXISTS pending_task_available_block
    `),
	// queue tables live and die by dead-tuple density: every claim/release
	// updates rows and every finished task deletes one, so at the default 20%
	// scale factor the live set is buried in dead tuples between vacuums
	newSqlMigration(`
        ALTER TABLE pending_task SET (
            autovacuum_vacuum_scale_factor = 0.01,
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_analyze_scale_factor = 0.02
        )
    `),

	// GetTransferStats sums account_payment.payout_byte_count by
	// (network_id, completed); only (network_id, canceled) existed, so the
	// completed filter and the summed column both went to the heap
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS account_payment_network_id_completed
        ON account_payment (network_id, completed) INCLUDE (payout_byte_count)
    `),

	// RemoveCompletedContracts deletes transfer_balance by end_time; without
	// this index each pass was a full seq scan of the live balances
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_balance_end_time
        ON transfer_balance (end_time)
    `),

	// ForceCloseOpenContractIds polls `WHERE open AND create_time <= $1 ORDER
	// BY create_time LIMIT n`. The btree (create_time, open, contract_id)
	// walks every old *closed* contract to find the open ones. This partial
	// index contains only open contracts (small), is perfectly ordered for the
	// poll, and — unlike the dropped (open, create_time) full index — cannot
	// be chosen for queries that don't filter on open.
	//
	// transfer_contract is large: pre-create this manually with CREATE INDEX
	// CONCURRENTLY out of band (see FIXME above); the IF NOT EXISTS gate makes
	// this migration a no-op once it is pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_contract_open_partial_create_time
        ON transfer_contract (create_time) WHERE open
    `),

	// high-water mark for the client_reliability redis rollup
	// (RollupClientReliabilityStats): all blocks <= max_drained_block are
	// fully drained from redis into `client_reliability`. Score computations
	// clamp their block ranges to this mark so windows only span fully
	// materialized blocks.
	newSqlMigration(`
        CREATE TABLE client_reliability_rollup (
            singleton_id int NOT NULL DEFAULT 1 CHECK (singleton_id = 1),
            max_drained_block bigint NOT NULL,
            update_time timestamp NOT NULL DEFAULT now(),

            PRIMARY KEY (singleton_id)
        )
    `),

	// transfer_contract index consolidation. Every index on this table is
	// maintained on each of the ~20M contract inserts/day and again on every
	// close (the generated `open` column flips, so closes are never HOT).
	//
	// The expired-dispute scan (`WHERE dispute AND outcome IS NULL AND
	// create_time <= $1 ORDER BY create_time`) is the only user of the
	// full-width (dispute, outcome, create_time) index, which carries an entry
	// for every contract. This partial index has entries only for undecided
	// disputes (a tiny set) and provides the same order.
	//
	// transfer_contract is large: pre-create this manually with CREATE INDEX
	// CONCURRENTLY out of band (see FIXME above); the IF NOT EXISTS gate makes
	// this migration a no-op once it is pre-created. Created before the drops
	// below so the dispute scan never loses coverage.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_contract_dispute_partial_create_time
        ON transfer_contract (create_time) WHERE (dispute AND outcome IS NULL)
    `),
	newSqlMigration(`
        DROP INDEX IF EXISTS transfer_contract_dispute_create_time
    `),
	// the only payer_network_id query is the open-bytes SUM
	// (GetOpenTransferByteCount), which is served index-only by
	// (open, payer_network_id, transfer_byte_count) — this one is redundant
	newSqlMigration(`
        DROP INDEX IF EXISTS transfer_contract_payer_network_id
    `),

	// network peers: string roles and an identity principal per client,
	// assigned at creation and immutable after (see model/peer_model.go).
	// The role values have no meaning to the network.
	newSqlMigration(`
        CREATE TABLE network_client_role (
            client_id uuid NOT NULL,
            role varchar(128) NOT NULL,

            PRIMARY KEY (client_id, role)
        )
    `),
	newSqlMigration(`
        ALTER TABLE network_client ADD COLUMN principal varchar(256) NOT NULL DEFAULT ''
    `),
	// auth codes carry roles and a principal so that logins minted from the
	// code (and clients created by those sessions) inherit them
	newSqlMigration(`
        CREATE TABLE auth_code_role (
            auth_code_id uuid NOT NULL,
            role varchar(128) NOT NULL,

            PRIMARY KEY (auth_code_id, role)
        )
    `),
	newSqlMigration(`
        ALTER TABLE auth_code ADD COLUMN principal varchar(256) NOT NULL DEFAULT ''
    `),

	// the top-level client count checks (the `AuthNetworkClient` create limit
	// and `NetworkPeersEnabled`, see model/peer_model.go) count only active
	// top-level clients per network. The existing (network_id, active,
	// client_id) index has an entry for every client ever created and forces a
	// heap check of source_client_id per row; this partial index has entries
	// for exactly the qualifying rows and serves the count index-only.
	//
	// network_client is large: pre-create this manually with CREATE INDEX
	// CONCURRENTLY out of band (see FIXME above); the IF NOT EXISTS gate makes
	// this migration a no-op once it is pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_network_id_top_level
        ON network_client (network_id) WHERE (active = true AND source_client_id IS NULL)
    `),

	// feedback log upload metadata. The log file content itself is not stored
	// (see controller.UploadLogFile) - one row per accepted upload attempt.
	// UNIQUE (network_id, rate_bucket) is the rate limiter: at most one upload
	// per network per bucket (`model.FeedbackLogUploadRatePeriod`).
	newSqlMigration(`
        CREATE TABLE feedback_log_upload (
            feedback_log_upload_id uuid NOT NULL,
            feedback_id uuid NOT NULL,
            network_id uuid NOT NULL,
            user_id uuid NOT NULL,
            client_id uuid NULL,
            upload_time timestamp NOT NULL DEFAULT now(),
            rate_bucket bigint NOT NULL,
            content_type varchar(256) NOT NULL DEFAULT '',
            byte_count bigint NOT NULL DEFAULT 0,
            complete bool NOT NULL DEFAULT false,

            PRIMARY KEY (feedback_log_upload_id),
            UNIQUE (network_id, rate_bucket)
        )
    `),

	// drained-coverage ranges for the client_reliability redis rollup
	// (`RollupClientReliabilityStats`): a block inside a range had its redis
	// counters fully drained into `client_reliability`. Blocks after the first
	// range that fall outside every range were lost before draining (redis
	// restart/expiry, drain outage) and the reliability score denominators
	// (`reliabilityCoveredBlockCount`) skip them instead of counting them as
	// unreliable gaps. Blocks before the first range (pre-rollup history, and
	// fixture/backfill environments where the table is empty) count as
	// covered. max_block_number is inclusive.
	newSqlMigration(`
        CREATE TABLE client_reliability_sync (
            min_block_number bigint NOT NULL,
            max_block_number bigint NOT NULL,
            update_time timestamp NOT NULL DEFAULT now(),

            PRIMARY KEY (min_block_number)
        )
    `),

	// `pro` marks a transfer balance as carrying the Pro entitlement. A network is
	// Pro iff it has any IN-WINDOW balance with pro = true. model/pro_model.go is
	// the single place this is tracked; nothing else should infer Pro.
	//
	// Entitlement is time-based, not byte-based: a subscriber stays Pro for the
	// period they paid for even after spending the whole balance. (Note the
	// existing `active` column is GENERATED AS (0 < balance_byte_count) -- bytes
	// remaining -- so it must NOT be used for the Pro check.)
	//
	// Defaults to true so every existing balance keeps conferring Pro through the
	// migration and nobody is downgraded. The backfill below then clears it on the
	// balances that never conferred Pro: the unpaid grants (free tier, referral
	// bonuses) carry no revenue and were excluded by the old `paid` heuristic that
	// IsPro used. Existing data-code balances DO carry revenue, so they stay
	// pro = true and are grandfathered; going forward data codes insert pro = false,
	// so buying data never grants Pro.
	newSqlMigration(`
        ALTER TABLE transfer_balance
        ADD COLUMN pro bool NOT NULL DEFAULT true
    `),
	newSqlMigration(`
        UPDATE transfer_balance
        SET pro = false
        WHERE
            net_revenue_nano_cents <= 0 AND
            subsidy_net_revenue_nano_cents <= 0
    `),
	// supports the Pro lookup in pro_model.go: in-window pro balances for a network
	newSqlMigration(`
        CREATE INDEX transfer_balance_pro ON transfer_balance (network_id, pro, start_time, end_time)
    `),

	// the auth session revocation deny-list was never enforced at runtime
	// (the check was short-circuited for perf) and is removed rather than
	// carried as dead logic: jwts are validated by signature and expiry only.
	// auth_session grew without bound (one row per login, never read); the
	// related propagation tables were part of the same removed machinery.
	newSqlMigration(`
        DROP TABLE IF EXISTS auth_session
    `),
	newSqlMigration(`
        DROP TABLE IF EXISTS auth_session_expiration
    `),
	newSqlMigration(`
        DROP TABLE IF EXISTS auth_code_session
    `),
	newSqlMigration(`
        DROP TABLE IF EXISTS device_adopt_auth_session
    `),

	// one reconnect per block no longer invalidates the block. A reconnect is
	// real user impact (it drops the provider's live clients), so it stays
	// penalized when it repeats -- but a SINGLE reconnect used to make the
	// whole block invalid, and at the 0.99 hour threshold one invalid block
	// (59/60 = 0.983) takes a provider out of the market for an hour. Any
	// handler rotation, mobile blip, or NAT rebind did that.
	//
	// `valid` is a STORED generated column: changing a generation expression
	// rewrites the table, which is not an option at this size. DROP EXPRESSION
	// turns it into a plain column with no rewrite (catalog only) and keeps
	// the existing (valid, block_number, client_address_hash) index working;
	// rows already written keep the values the strict rule gave them, and the
	// writers below supply the value from here on. Requires pg 13+.
	// the allowance is a bind parameter, not baked into the function, so
	// `model.ReliabilityAllowDisconnectCountPerBlock` is the one definition
	// and changing it needs no migration
	newSqlMigration(`
        CREATE OR REPLACE FUNCTION client_reliability_valid(
            connection_new_count bigint,
            connection_established_count bigint,
            provide_enabled_count bigint,
            provide_changed_count bigint,
            receive_message_count bigint,
            allow_disconnect_count bigint
        ) RETURNS bool
        LANGUAGE sql IMMUTABLE
        AS '
            SELECT
                connection_new_count <= allow_disconnect_count AND
                1 <= connection_established_count AND
                1 <= provide_enabled_count AND
                provide_changed_count = 0 AND
                1 <= receive_message_count
        '
    `),
	newSqlMigration(`
        ALTER TABLE client_reliability ALTER COLUMN valid DROP EXPRESSION
    `),

	// per-block client counts, recorded by the reliability drain
	// (`RollupClientReliabilityStats`). A block whose valid client count
	// collapses relative to its neighbors was a platform event (a connect
	// deploy rotating handlers, an outage) rather than a client event: the
	// affected clients could not announce, and the ones that reconnected are
	// marked invalid by the `connection_new_count = 0` rule. Such blocks are
	// excused for everyone (see `reliabilityDegradedBlocks`) so a deploy does
	// not drop every provider below the reliability threshold.
	newSqlMigration(`
        CREATE TABLE client_reliability_block (
            block_number bigint NOT NULL,
            client_count bigint NOT NULL,
            valid_client_count bigint NOT NULL,

            PRIMARY KEY (block_number)
        )
    `),

	// when a client was deactivated (user removal, or the idle top-level
	// client marker in RemoveDisconnectedNetworkClients). The reap deletes an
	// inactive client `NetworkClientReapAfterDeactivate` after this time;
	// pre-migration rows (NULL) fall back to create_time, matching the old
	// reap behavior. Metadata-only ALTER (nullable, no default).
	newSqlMigration(`
        ALTER TABLE network_client ADD COLUMN deactivate_time timestamp NULL
    `),

	// What a Solana payment was FOR.
	//
	// The intent recorded only a reference, so the webhook had nothing to check an
	// arriving payment against. It coped by hardcoding `TokenAmount >= 40` and always
	// granting a YEAR. Two consequences, both bad:
	//
	//   - the $5 monthly option offered on the site took the customer's money and
	//     delivered NOTHING (5 < 40, so the transfer was ignored as "no matching USDC
	//     payment");
	//   - any payment of 40 or more bought a full year, however large or small.
	//
	// Recording the quoted price and plan lets the webhook verify that what arrived is
	// what was asked for, and grant the plan that was actually bought.
	//
	// The defaults preserve the old behavior for intents created before these columns
	// existed: 0 accepts any amount, and an empty plan means yearly.
	newSqlMigration(`
        ALTER TABLE solana_payment_intent
        ADD COLUMN expected_amount_usd double precision NOT NULL DEFAULT 0,
        ADD COLUMN subscription_plan varchar(32) NOT NULL DEFAULT ''
    `),

	// Per-table autovacuum for the big churn tables. Measured 2026-07-12
	// (xops/db/STORAGE1-REVIEW.md): every large table showed
	// last_autovacuum = NULL while small tables vacuumed fine the same day.
	// At the defaults a vacuum pass on these tables cannot finish: the 2ms
	// cost delay caps it at a few MB/s, and each pass re-scans the full
	// (bloated, multi-hundred-GB) indexes once per autovacuum_work_mem batch
	// of dead tuples — so passes take days and any restart loses them. Dead
	// space is never reclaimed and the planner runs blind (last_autoanalyze
	// was NULL too).
	//
	// The policy for the contract chain: no cost delay, and FIXED thresholds
	// instead of scale factors — a fixed threshold keeps the per-pass work
	// small enough to complete regardless of table size, and keeps triggering
	// correct even when a stats reset zeroes n_live_tup (which the
	// measurement showed). At ~20M contracts/day, vacuum lands ~4x/day and
	// an unthrottled pass over these indexes completes in minutes-to-tens of
	// minutes. The insert threshold keeps visibility-map/freeze work steady
	// on the append-heavy tables (index-only scans + wraparound headroom).
	//
	// These reloptions only take a SHARE UPDATE EXCLUSIVE lock (no
	// read/write blocking), so the migration is safe on hot tables.
	//
	// Deliberately NOT set here:
	//   - client_reliability: now range-partitioned. Its daily leaf partitions
	//     are created at runtime by the maintenance task
	//     (`ensureClientReliabilityPartition`), NOT by a migration, so their
	//     autovacuum cannot be set here (and reloptions on the empty parent do
	//     not drive partition autovacuum). Nor do they need tuning: each
	//     partition is ~1 day, mostly insert-only, and dropped at 30 days, so
	//     the defaults vacuum it fine — unlike the old 4TB table, whose vacuum
	//     never completed. If ever needed, tune via a WITH(...) on the partition
	//     CREATE, measure-first (see STORAGE.md §3).
	//   - pending_task: already tuned above.
	// The existing bloat is not fixed by this (autovacuum only reclaims going
	// forward): each of these tables needs a one-time manual
	// VACUUM (VERBOSE, ANALYZE), and pg_repack to shrink the files.
	newSqlMigration(`
        ALTER TABLE contract_close SET (
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_vacuum_scale_factor = 0,
            autovacuum_vacuum_threshold = 5000000,
            autovacuum_vacuum_insert_scale_factor = 0,
            autovacuum_vacuum_insert_threshold = 10000000,
            autovacuum_analyze_scale_factor = 0,
            autovacuum_analyze_threshold = 1000000
        )
    `),
	newSqlMigration(`
        ALTER TABLE transfer_contract SET (
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_vacuum_scale_factor = 0,
            autovacuum_vacuum_threshold = 5000000,
            autovacuum_vacuum_insert_scale_factor = 0,
            autovacuum_vacuum_insert_threshold = 10000000,
            autovacuum_analyze_scale_factor = 0,
            autovacuum_analyze_threshold = 1000000
        )
    `),
	newSqlMigration(`
        ALTER TABLE transfer_escrow SET (
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_vacuum_scale_factor = 0,
            autovacuum_vacuum_threshold = 5000000,
            autovacuum_vacuum_insert_scale_factor = 0,
            autovacuum_vacuum_insert_threshold = 10000000,
            autovacuum_analyze_scale_factor = 0,
            autovacuum_analyze_threshold = 1000000
        )
    `),
	newSqlMigration(`
        ALTER TABLE transfer_escrow_sweep SET (
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_vacuum_scale_factor = 0,
            autovacuum_vacuum_threshold = 5000000,
            autovacuum_vacuum_insert_scale_factor = 0,
            autovacuum_vacuum_insert_threshold = 10000000,
            autovacuum_analyze_scale_factor = 0,
            autovacuum_analyze_threshold = 1000000
        )
    `),
	newSqlMigration(`
        ALTER TABLE network_client_location_reliability SET (
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_vacuum_scale_factor = 0,
            autovacuum_vacuum_threshold = 5000000,
            autovacuum_vacuum_insert_scale_factor = 0,
            autovacuum_vacuum_insert_threshold = 10000000,
            autovacuum_analyze_scale_factor = 0,
            autovacuum_analyze_threshold = 1000000
        )
    `),

	// The client-lifecycle tables are an order of magnitude smaller but churn
	// constantly (connect/disconnect updates, the idle-client reap deletes),
	// and the measurement showed the same never-vacuumed state. The
	// pending_task-style small scale factors are right here: live-set-
	// proportional passes over tens of GB complete quickly unthrottled.
	newSqlMigration(`
        ALTER TABLE network_client SET (
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_vacuum_scale_factor = 0.01,
            autovacuum_analyze_scale_factor = 0.02
        )
    `),
	newSqlMigration(`
        ALTER TABLE provide_key SET (
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_vacuum_scale_factor = 0.01,
            autovacuum_analyze_scale_factor = 0.02
        )
    `),
	newSqlMigration(`
        ALTER TABLE device SET (
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_vacuum_scale_factor = 0.01,
            autovacuum_analyze_scale_factor = 0.02
        )
    `),

	// the net-escrow reconcile task (model/subscription_model.go
	// `openEscrowReservedByBalance`, rescheduled every 5 minutes) sums open
	// escrow per balance by joining the full transfer_escrow and
	// transfer_contract tables filtered on `outcome IS NULL`. That predicate is
	// deliberate: a disputed-but-unsettled contract has the generated `open`
	// column false yet still holds its reservation, so `open` -- and every index
	// led by it -- cannot be used. With nothing indexing `outcome`, the join
	// falls back to a seq scan of the two largest tables on every run.
	//
	// This partial index carries only the live set (open + disputed-unsettled,
	// i.e. exactly `outcome IS NULL`) -- small and roughly steady-state
	// regardless of table growth -- and keys on contract_id so the scan
	// enumerates it index-only and nested-loops into transfer_escrow's
	// (contract_id, balance_id) PK.
	//
	// transfer_contract is large: pre-create this manually with CREATE INDEX
	// CONCURRENTLY out of band (see FIXME above); the IF NOT EXISTS gate makes
	// this migration a no-op once it is pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_contract_outcome_null
        ON transfer_contract (contract_id) WHERE outcome IS NULL
    `),

	// restores the index dropped by the consolidation above (`DROP INDEX IF
	// EXISTS transfer_contract_open_source_id_companion_contract_id`). The
	// companion-pairing lookup in model/subscription_model.go
	// `CreateTransferEscrow` -- run on EVERY contract creation -- finds the
	// earliest source->dest contract with a null companion, matching either an
	// open contract OR a recently-closed one (`open = false AND close_time >=
	// $3`) and taking the earliest by create_time (LIMIT 1). Without this index
	// the closed branch degrades to scanning the pair's full closed-contract
	// history (or, via transfer_contract_create_time, an unbounded ordered scan
	// when the pair has no match) on that hot path. The composite makes both
	// branches a tight range: (open, source_id, destination_id,
	// companion_contract_id) equality plus the close_time range, with
	// create_time/contract_id trailing for the ORDER BY ... LIMIT 1 and an
	// index-only result.
	//
	// transfer_contract is large: pre-create this manually with CREATE INDEX
	// CONCURRENTLY out of band (see FIXME above); the IF NOT EXISTS gate makes
	// this migration a no-op once it is pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_contract_open_source_id_companion_contract_id
        ON transfer_contract (open, source_id, destination_id, companion_contract_id, close_time, create_time, contract_id)
    `),

	// restores the covering index dropped by the consolidation
	// (`DROP INDEX IF EXISTS network_client_network_id_active_client_id`, which
	// swapped it for `network_client_active_network_id_create_time (active,
	// network_id, create_time)` -- that serves the filter but does not cover
	// client_id). The StatsProviders active-provider enumeration
	// (model/provider_model.go: `SELECT client_id FROM network_client JOIN
	// provide_key ... WHERE network_id = $1 AND active = true GROUP BY
	// client_id`) then heap-fetches client_id per active client before the
	// semi-join; for large provider networks that dominates. With (network_id,
	// active, client_id) the network_client side is index-only and feeds
	// client_id straight into the provide_key PK (client_id, provide_mode)
	// semi-join.
	//
	// network_client is large: pre-create this manually with CREATE INDEX
	// CONCURRENTLY out of band (see FIXME above); the IF NOT EXISTS gate makes
	// this migration a no-op once it is pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_network_id_active_client_id
        ON network_client (network_id, active, client_id)
    `),

	// covering index for the provider-payout stats scan (model/provider_model.go
	// StatsProviders: `SUM(payout_net_revenue_nano_cents) FROM
	// transfer_escrow_sweep ... WHERE sweep_time >= $2`, joined to
	// transfer_contract for destination_id). The plain (sweep_time) index below
	// made that a range scan plus a heap fetch per row; this carries contract_id
	// and the summed payout in the index, so the sweep side is index-only and
	// only the transfer_contract PK join touches a heap. It leads with
	// sweep_time, so it strictly supersedes transfer_escrow_sweep_sweep_time
	// (dropped next) -- every (sweep_time) lookup is a prefix of this one.
	//
	// Created before the drop so the sweep_time scan never loses coverage.
	// transfer_escrow_sweep is large: pre-create this manually with CREATE INDEX
	// CONCURRENTLY out of band (see FIXME above); the IF NOT EXISTS gate makes
	// this migration a no-op once it is pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_escrow_sweep_sweep_time_covering
        ON transfer_escrow_sweep (sweep_time, contract_id, payout_net_revenue_nano_cents)
    `),
	newSqlMigration(`
        DROP INDEX IF EXISTS transfer_escrow_sweep_sweep_time
    `),

	// candidate scan for the idle top-level client reap
	// (model/network_client_model.go `RemoveDisconnectedNetworkClients`, run
	// every 5 minutes). Each batch marks active top-level clients unseen for
	// TopLevelClientIdleExpiration inactive: `active = true AND source_client_id
	// IS NULL AND auth_time < $1`, LIMIT, inside the UPDATE ... SET active =
	// false transaction. No existing index serves that predicate globally -- the
	// network_client_network_id_top_level partial has the right WHERE but is
	// keyed on network_id, and the reap does not filter by network -- so the
	// subquery scanned the entire active top-level population and heap-fetched
	// auth_time per row, holding ROW EXCLUSIVE on network_client for the scan's
	// duration (colliding with DDL on the table and pinning the xmin horizon so
	// autovacuum could not keep up). This partial is keyed on auth_time over
	// exactly the reap's (active, top-level) set, so a batch is a bounded ordered
	// range scan (oldest auth_time first) that stops at LIMIT. It is tiny and
	// self-maintaining: a row leaves it the moment the reap flips active = false.
	// Pair with `ORDER BY auth_time` in the reap subquery so the plan is the
	// ordered range scan.
	//
	// network_client is large: pre-create this manually with CREATE INDEX
	// CONCURRENTLY out of band (see FIXME above); the IF NOT EXISTS gate makes
	// this migration a no-op once it is pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_idle_top_level_auth_time
        ON network_client (auth_time) WHERE (active = true AND source_client_id IS NULL)
    `),

	// hard-reap of inactive clients (model/network_client_model.go
	// `RemoveDisconnectedNetworkClients`, every 5 minutes): DELETE ... WHERE
	// COALESCE(deactivate_time, create_time) < $1 AND active = false. No index
	// served it -- `active = false` seeks only via (active, network_id,
	// create_time) whose reap key is create_time, not the COALESCE expression --
	// so it scanned the entire active = false band (now fed by the mark step
	// above), evaluating COALESCE per row, unbatched, holding locks on the hot
	// table. This functional partial index over exactly the inactive set makes
	// the (now batched) delete an ordered range scan, oldest deactivation first.
	//
	// network_client is large: pre-create this manually with CREATE INDEX
	// CONCURRENTLY out of band (see FIXME above); the IF NOT EXISTS gate makes
	// this migration a no-op once it is pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_inactive_reap_time
        ON network_client (COALESCE(deactivate_time, create_time)) WHERE (active = false)
    `),

	// targeted cascades delete the proxy change-log by proxy_id
	// (model/network_client_proxy_model.go RemoveProxyDeviceConfig,
	// model/network_client_model.go removeProxyClientData): `DELETE FROM
	// proxy_client_change WHERE proxy_id = $1 / = ANY($1)`. The table had only
	// its PK (proxy_host, block, change_id), so proxy_id was unindexed and every
	// proxy-config removal / client reap seq-scanned the whole change log. (The
	// reads use the PK prefix and are unaffected.)
	//
	// proxy_client_change is large: pre-create this manually with CREATE INDEX
	// CONCURRENTLY out of band (see FIXME above); the IF NOT EXISTS gate makes
	// this migration a no-op once it is pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS proxy_client_change_proxy_id
        ON proxy_client_change (proxy_id)
    `),

	// st_event is the on-chain subnet event log (PK (block_number, log_index)),
	// growing unboundedly with the chain. model/st_model.go SumStDepositedRao
	// (`WHERE kind = 'Deposited'`, per deposit-idempotency check) and
	// GetHeadBoundCkeysInEpoch (`WHERE kind IN (...) AND block_number <= $1
	// ORDER BY block_number, log_index`, every epoch close) both scanned the
	// whole log because `kind` was unindexed. Leading with kind bounds both to
	// the matching events, with block_number/log_index giving the range bound
	// and the ordering. Pre-create CONCURRENTLY out of band if the log is large.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS st_event_kind_block
        ON st_event (kind, block_number, log_index)
    `),

	// GetStEpochClientReliability (model/st_model.go) selects the epoch's rollup
	// rows by interval overlap: `WHERE $1 < period_end AND period_start < $2`.
	// The only indexed side was `period_start < $2` (PK leads with period_start),
	// an unbounded-below range that at epoch close scans almost the whole table;
	// the selective bound `period_end > $1` led no index. This indexes period_end
	// so the recent-tail bound drives the scan. NOTE: verify_provider_stats has
	// no retention task and grows per period x provider -- a reap (mirroring
	// RemoveOldSearchProviderStats) is still recommended.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS verify_provider_stats_period_end
        ON verify_provider_stats (period_end)
    `),

	// CancelHungAccountPayments (model/account_payment_model.go, daily): `UPDATE
	// account_payment SET canceled = true ... WHERE NOT completed AND NOT
	// canceled AND create_time < $1`. No index served it; it scanned the whole
	// non-completed band, which grows monotonically (canceled rows stay
	// completed = false and account_payment is never deleted). This partial
	// indexes exactly the truly-pending set, ordered by create_time so the scan
	// is a tight range that stops early.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS account_payment_pending_create_time
        ON account_payment (create_time) WHERE (NOT completed AND NOT canceled)
    `),

	// UpdateClientReliabilityScores (model/network_client_reliability_model.go)
	// prunes superseded rows: `DELETE FROM client_connection_reliability_score
	// WHERE lookback_index = $1 AND max_block_number != $2`, once per lookback
	// each pass. The PK leads with client_id, so lookback_index was unindexed and
	// the delete seq-scanned the whole score table. This lets it seek
	// lookback_index = $1 and skip the current max_block_number. (The
	// network_connection_reliability_score / _window_score siblings delete on
	// `max_block_number != $1` with no equality anchor -- an index cannot serve a
	// bare !=, and they are small, so they are left as-is.)
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS client_connection_reliability_score_lookback_max_block
        ON client_connection_reliability_score (lookback_index, max_block_number)
    `),

	// the child-client reap (model/network_client_model.go
	// RemoveDisconnectedNetworkClients) deletes clients with a parent unseen
	// since minClientTime: `auth_time < $1 AND source_client_id IS NOT NULL AND
	// no live connection`. The inactive-reap partial added above is the OPPOSITE
	// predicate (source_client_id IS NULL), so this branch had no index and
	// seq-scanned the whole table every 5 minutes. This partial covers exactly
	// the child set; pair with the batched DELETE in the reap.
	//
	// network_client is large: pre-create CONCURRENTLY out of band; IF NOT
	// EXISTS makes this a no-op once pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_child_reap_auth_time
        ON network_client (auth_time) WHERE (source_client_id IS NOT NULL)
    `),

	// GetOverlappingTransferBalance (model/subscription_model.go, Google Play
	// renewal) looks up a paid balance by purchase_token overlapping an expiry.
	// The exact-fit index was dropped as "unused" (migration line 1882) but the
	// path is live; without it every renewal seq-scans transfer_balance (one of
	// the largest tables). Restored as a partial -- only paid balances carry a
	// token.
	//
	// transfer_balance is large: pre-create CONCURRENTLY out of band.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_balance_purchase_token
        ON transfer_balance (purchase_token, end_time, start_time, balance_id) WHERE purchase_token IS NOT NULL
    `),

	// GetAccountWalletByCircleId (model/account_wallet_model.go, Circle webhook)
	// filters account_wallet by circle_wallet_id, which was added without an
	// index -> seq scan per call. Partial since most wallets have no circle id.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS account_wallet_circle_wallet_id
        ON account_wallet (circle_wallet_id) WHERE circle_wallet_id IS NOT NULL
    `),

	// GetAllSeekerHolders (model/account_wallet_model.go, payout planning + the
	// daily free-grant path) filters `has_seeker_token = true`, unindexed ->
	// seq scan. Seeker holders are a small subset, so this partial is tiny.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS account_wallet_seeker_network
        ON account_wallet (network_id) WHERE has_seeker_token
    `),

	// referral cap-checks (model/network_referral_model.go CreateNetworkReferral,
	// model/network_referral_code_model.go ValidateReferralCode) filter
	// `referral_network_id = $1`. That column lost all coverage when the
	// composite PK was reduced to (network_id) (migration line 1749), so these
	// hot referral paths seq-scan network_referral.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_referral_referral_network_id
        ON network_referral (referral_network_id)
    `),

	// GetCircleUCByCircleUCUserId (model/circle_wallet_model.go, Circle webhook)
	// filters circle_uc by circle_uc_user_id; PK is (network_id), so it is
	// unindexed -> seq scan.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS circle_uc_circle_uc_user_id
        ON circle_uc (circle_uc_user_id)
    `),

	// the by-address auth-attempt lookback (model/auth_model_attempt.go) filters
	// `client_address_hash = $1 AND attempt_time >= X ORDER BY attempt_time
	// DESC`. The existing (client_address_hash, client_address_port,
	// attempt_time) index has the unconstrained port column between the hash and
	// the time, so it can seek the hash but cannot bound attempt_time or serve
	// the order -- it reads all history for the hash and sorts. This drops the
	// port so the range/order is index-served.
	//
	// user_auth_attempt is large: pre-create CONCURRENTLY out of band.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS user_auth_attempt_client_address_hash_attempt_time
        ON user_auth_attempt (client_address_hash, attempt_time)
    `),

	// reaper seq-scans: each of these deletes an old tail by a time column that
	// is not the leading (or any) index column, so the maintenance task scans
	// the whole growing table each run. A plain index on the time column makes
	// each an ordered range delete.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS wallet_auth_challenge_attempt_attempt_time
        ON wallet_auth_challenge_attempt (attempt_time)
    `),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS user_auth_verify_verify_time
        ON user_auth_verify (verify_time)
    `),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS provide_key_change_change_time
        ON provide_key_change (change_time)
    `),
	// the expired-intent cleanup filters `expires_at < $1 AND tx_signature IS
	// NULL`; the existing partial leads with payment_reference, so it scans every
	// unpaid intent. This partial leads with expires_at over the same unpaid set.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS solana_payment_intent_expires_at
        ON solana_payment_intent (expires_at) WHERE tx_signature IS NULL
    `),

	// audit stats (ComputeStats90) aggregations filter `event_type IN (...)`, but
	// event_type was in none of the _stats_ indexes, so each windowed scan heap-
	// fetched every row just to test event_type -- defeating an index-only scan
	// even though the other selected columns are already in-key. INCLUDE
	// event_type so the aggregations are index-only. This also makes the
	// audit_device_event stats index distinct from its identical PK (it was a
	// pure duplicate before). Each is dropped and recreated with the same name.
	//
	// deferred value: the /stats routes are gated, and index-only scans also
	// need the visibility map that only VACUUM sets on these never-vacuumed
	// tables -- so this pays off once stats re-enable and the audit tables are
	// vacuumed. These tables (esp. audit_contract_event) are large: DROP then
	// CREATE CONCURRENTLY out of band; the IF EXISTS/IF NOT EXISTS gates make the
	// migrations no-ops once done.
	newSqlMigration(`DROP INDEX IF EXISTS audit_provider_event_stats_device_id`),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS audit_provider_event_stats_device_id
        ON audit_provider_event (event_time, device_id, event_id) INCLUDE (event_type)
    `),
	newSqlMigration(`DROP INDEX IF EXISTS audit_extender_event_stats_extender_id`),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS audit_extender_event_stats_extender_id
        ON audit_extender_event (event_time, extender_id, event_id) INCLUDE (event_type)
    `),
	newSqlMigration(`DROP INDEX IF EXISTS audit_network_event_stats_network_id`),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS audit_network_event_stats_network_id
        ON audit_network_event (event_time, network_id, event_id) INCLUDE (event_type)
    `),
	newSqlMigration(`DROP INDEX IF EXISTS audit_device_event_stats_device_id`),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS audit_device_event_stats_device_id
        ON audit_device_event (event_time, device_id, event_id) INCLUDE (event_type)
    `),
	newSqlMigration(`DROP INDEX IF EXISTS audit_contract_event_stats`),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS audit_contract_event_stats
        ON audit_contract_event (event_time, transfer_byte_count, transfer_packets) INCLUDE (event_type)
    `),
	newSqlMigration(`DROP INDEX IF EXISTS audit_contract_event_stats_extender_id`),
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS audit_contract_event_stats_extender_id
        ON audit_contract_event (event_time, extender_id, transfer_byte_count, transfer_packets) INCLUDE (event_type)
    `),

	// RemoveExpiredWalletNonces (model/auth_wallet_nonce_model.go) reaps by
	// expire_time; auth_wallet_nonce had only its PK (nonce), so the reaper
	// seq-scanned. The table is live-written by the no-auth AuthWalletNonceCreate
	// route and had no reaper task wired at all, so it grew unboundedly -- now
	// batched + scheduled, driven by this index.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS auth_wallet_nonce_expire_time
        ON auth_wallet_nonce (expire_time)
    `),

	// RemoveOldNetworkReliabilityWindow reaps network_connection_reliability_window
	// by bucket_number, which is the SECOND PK column (PK is (network_id,
	// bucket_number)) with no other index -> full PK scan. This indexes
	// bucket_number so the reap is an ordered range scan (paired with ORDER BY
	// bucket_number in the query).
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_connection_reliability_window_bucket_number
        ON network_connection_reliability_window (bucket_number)
    `),

	// AddProTransferBalanceToAllNetworks (model/subscription_model.go) finds the
	// active supporters: `subscription_type = 'supporter' AND start_time <= now
	// AND now < end_time`, across all networks. The composite
	// subscription_renewal index leads with network_id, so it can't seek this
	// network-less filter -> full scan. This partial indexes the supporter subset
	// by end_time (the selective in-window bound), start_time trailing for the
	// filter.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS subscription_renewal_supporter_window
        ON subscription_renewal (end_time, start_time) WHERE subscription_type = 'supporter'
    `),

	// RefreshVerifyProxyEgress (model/verify_model.go) reads `SELECT client_id,
	// client_ipv4 FROM proxy_client WHERE client_ipv4 IS NOT NULL` each refresh;
	// client_ipv4 leads no index -> full scan. This partial (covering both
	// selected columns) makes it an index-only scan of just the assigned-ipv4
	// rows.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS proxy_client_client_ipv4
        ON proxy_client (client_id, client_ipv4) WHERE client_ipv4 IS NOT NULL
    `),

	// Back out the per-table autovacuum reloptions set earlier (v420 pending_task,
	// v447-v454 the contract-chain and client-lifecycle tables) and return every
	// table to the database-default autovacuum behavior. RESET reverts each named
	// storage parameter to its cluster default; it is a no-op for a parameter that
	// is not currently set, so these are safe regardless of which of the SET
	// migrations a given database actually applied.
	newSqlMigration(`
        ALTER TABLE pending_task RESET (
            autovacuum_vacuum_scale_factor,
            autovacuum_vacuum_cost_delay,
            autovacuum_analyze_scale_factor
        )
    `),
	newSqlMigration(`
        ALTER TABLE contract_close RESET (
            autovacuum_vacuum_cost_delay,
            autovacuum_vacuum_scale_factor,
            autovacuum_vacuum_threshold,
            autovacuum_vacuum_insert_scale_factor,
            autovacuum_vacuum_insert_threshold,
            autovacuum_analyze_scale_factor,
            autovacuum_analyze_threshold
        )
    `),
	newSqlMigration(`
        ALTER TABLE transfer_contract RESET (
            autovacuum_vacuum_cost_delay,
            autovacuum_vacuum_scale_factor,
            autovacuum_vacuum_threshold,
            autovacuum_vacuum_insert_scale_factor,
            autovacuum_vacuum_insert_threshold,
            autovacuum_analyze_scale_factor,
            autovacuum_analyze_threshold
        )
    `),
	newSqlMigration(`
        ALTER TABLE transfer_escrow RESET (
            autovacuum_vacuum_cost_delay,
            autovacuum_vacuum_scale_factor,
            autovacuum_vacuum_threshold,
            autovacuum_vacuum_insert_scale_factor,
            autovacuum_vacuum_insert_threshold,
            autovacuum_analyze_scale_factor,
            autovacuum_analyze_threshold
        )
    `),
	newSqlMigration(`
        ALTER TABLE transfer_escrow_sweep RESET (
            autovacuum_vacuum_cost_delay,
            autovacuum_vacuum_scale_factor,
            autovacuum_vacuum_threshold,
            autovacuum_vacuum_insert_scale_factor,
            autovacuum_vacuum_insert_threshold,
            autovacuum_analyze_scale_factor,
            autovacuum_analyze_threshold
        )
    `),
	newSqlMigration(`
        ALTER TABLE network_client_location_reliability RESET (
            autovacuum_vacuum_cost_delay,
            autovacuum_vacuum_scale_factor,
            autovacuum_vacuum_threshold,
            autovacuum_vacuum_insert_scale_factor,
            autovacuum_vacuum_insert_threshold,
            autovacuum_analyze_scale_factor,
            autovacuum_analyze_threshold
        )
    `),
	newSqlMigration(`
        ALTER TABLE network_client RESET (
            autovacuum_vacuum_cost_delay,
            autovacuum_vacuum_scale_factor,
            autovacuum_analyze_scale_factor
        )
    `),
	newSqlMigration(`
        ALTER TABLE provide_key RESET (
            autovacuum_vacuum_cost_delay,
            autovacuum_vacuum_scale_factor,
            autovacuum_analyze_scale_factor
        )
    `),
	newSqlMigration(`
        ALTER TABLE device RESET (
            autovacuum_vacuum_cost_delay,
            autovacuum_vacuum_scale_factor,
            autovacuum_analyze_scale_factor
        )
    `),

	// Denormalize transfer_contract.destination_id onto transfer_escrow_sweep so
	// the provider-payout stats (model/provider_model.go StatsProviders et al.)
	// filter and group by destination_id AND sweep_time on ONE table, instead of
	// range-scanning every sweep in the window and PK-joining transfer_contract
	// just to recover destination_id. settleEscrowInTx stamps it on new sweeps;
	// existing rows are backfilled by `bringyourctl backfill sweep-destination-id`.
	// Nullable (no default), so the ADD COLUMN is a fast metadata-only change;
	// old/orphan sweeps stay NULL and are excluded by the stats filter until
	// backfilled or reaped.
	newSqlMigration(`
        ALTER TABLE transfer_escrow_sweep ADD COLUMN destination_id uuid NULL
    `),
	// Covering index for the per-destination payout window scan: leads with
	// (destination_id, sweep_time) for the filter and carries the summed payout,
	// so the scan is index-only. transfer_escrow_sweep is large: pre-create this
	// manually with CREATE INDEX CONCURRENTLY out of band; the IF NOT EXISTS gate
	// makes this migration a no-op once it is pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_escrow_sweep_destination_id_sweep_time
        ON transfer_escrow_sweep (destination_id, sweep_time) INCLUDE (payout_net_revenue_nano_cents)
    `),

	// Indexed reap-eligibility for the transfer_contract retention reaper. reap_time
	// is the instant a contract becomes due for hard deletion: it is set to
	// complete_time + CompletedContractExpiration when the contract's payment
	// completes (CompletePayment), and to now() when an aged closed-but-never-
	// completed straggler is marked by the retention task. The reaper then deletes
	// by an index range-scan over reap_time instead of the old un-indexable
	// anti-join full scan over the whole old-closed table that caused a prod
	// incident. Nullable (no default), so the ADD COLUMN is a fast metadata-only
	// change; existing rows are seeded by `bringyourctl db backfill-contract-reap-time`.
	newSqlMigration(`
        ALTER TABLE transfer_contract ADD COLUMN reap_time timestamp NULL
    `),
	// Partial index driving the reaper's delete pass (reap_time IS NOT NULL AND
	// reap_time < now): only due/pending contracts are indexed, so the scan is a
	// bounded range. transfer_contract is a large table: pre-create this manually
	// with CREATE INDEX CONCURRENTLY out of band; the IF NOT EXISTS gate makes this
	// migration a no-op once it is pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_contract_reap_time
        ON transfer_contract (reap_time) WHERE reap_time IS NOT NULL
    `),
	// Partial index driving the reaper's assign pass (closed contracts not yet
	// reaped, ordered by create_time), so the straggler scan is a bounded range
	// instead of an anti-join. Same pre-create-CONCURRENTLY note as above.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_contract_reap_pending_create_time
        ON transfer_contract (create_time) WHERE reap_time IS NULL AND close_time IS NOT NULL
    `),
	// Per-request wallet lookups (MarkWalletSeekerHolder: UPDATE account_wallet
	// WHERE wallet_address = $1 AND network_id = $2) had no usable index —
	// account_wallet's other indexes lead with wallet_id or active — so every
	// call seq-scanned account_wallet. This composite matches both equality
	// predicates for an index seek. account_wallet can be large: pre-create
	// manually with CREATE INDEX CONCURRENTLY out of band; the IF NOT EXISTS gate
	// makes this migration a no-op once it is pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS account_wallet_wallet_address_network_id
        ON account_wallet (wallet_address, network_id)
    `),

	// Rolling incremental reliability-score maintenance
	// (UpdateClientReliabilityRunningInTx, model/network_client_reliability_model.go).
	// Instead of re-scanning the whole lookback window of the
	// ~hundreds-of-millions-row client_reliability table on every run, the score
	// computations keep a running per-(client, lookback) sum of the
	// location-INDEPENDENT reliability contributions and advance it by only the
	// blocks that entered/left the window since the last run, anchored by a
	// periodic full recompute. A drained block's client_reliability rows are
	// immutable (block finality + idempotent absolute-count drain) and
	// valid_client_count is block-local, so a block's per-client contribution is
	// deterministic; add-on-entry and subtract-on-exit therefore cancel exactly
	// (modulo float associativity, reset by the recompute). #1
	// (client_connection_reliability_score) and #3
	// (network_connection_reliability_window_score) are written from this table
	// joined to network_client_location_reliability at write time, so location
	// stays query-time (no semantic change vs the old full-window query).
	//
	// running per-client, location-independent sums; one row per (client, lookback)
	newSqlMigration(`
        CREATE TABLE client_reliability_running (
            client_id uuid NOT NULL,
            lookback_index int NOT NULL,
            network_id uuid NOT NULL,
            independent_sum double precision NOT NULL,
            reliability_sum double precision NOT NULL,

            PRIMARY KEY (client_id, lookback_index)
        )
    `),

	// per-lookback applied window bounds + recompute marker: the prior [min, max)
	// the rolling maintenance diffs against, and the block the last full recompute
	// anchored the sums at.
	newSqlMigration(`
        CREATE TABLE client_reliability_running_window (
            lookback_index int NOT NULL,
            min_block_number bigint NOT NULL,
            max_block_number bigint NOT NULL,
            last_recompute_block bigint NOT NULL,

            PRIMARY KEY (lookback_index)
        )
    `),

	// The payout planner (planPayments) re-picks sweeps whose payment was
	// canceled (CancelHungAccountPayments sets canceled=true but does not null the
	// sweep's payment_id). The payout query's canceled UNION arm drives from the
	// small canceled set joined to sweeps by payment_id; this partial index over
	// just the canceled rows makes finding those payment_ids an index-only scan
	// instead of a seq scan of account_payment. account_payment is large: pre-
	// create manually with CREATE INDEX CONCURRENTLY out of band; the IF NOT
	// EXISTS gate makes this migration a no-op once it is pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS account_payment_canceled_payment_id
        ON account_payment (payment_id) WHERE canceled
    `),

	// Consecutive-error count for task reschedules, driving exponential backoff
	// (task.go). Without it every task error retried within RescheduleTimeout
	// (~2s) forever: 8k payment tasks stuck on an external 429 rate limit
	// produced ~65 error-reschedule updates/sec, churning pending_task to ~94%
	// dead tuples and degrading the poll query (observed at 39% of all db exec
	// time). The count resets naturally when the task completes (row moves to
	// finished_task and is deleted). pending_task is small, so the ADD COLUMN
	// with default is instant.
	newSqlMigration(`
        ALTER TABLE pending_task ADD COLUMN reschedule_error_count int NOT NULL DEFAULT 0
    `),

	// Deliberate, targeted re-add of eager autovacuum for pending_task only,
	// after the 2026-07-13 blanket backout of per-table autovacuum tuning
	// (v494-502). pending_task is a small (~13k live rows) queue table churned by
	// every task claim/reschedule; at default autovacuum it was measured at ~94%
	// dead tuples, which degraded the poll query (ORDER BY available_block ...
	// FOR UPDATE SKIP LOCKED) to 646ms mean = 39% of all db exec time. The
	// reschedule backoff reduces the churn, but the queue head still needs eager
	// vacuuming to keep the poll index clean. Same settings the table had before
	// the backout.
	newSqlMigration(`
        ALTER TABLE pending_task SET (
            autovacuum_vacuum_scale_factor = 0.01,
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_analyze_scale_factor = 0.02
        )
    `),

	// Deliberate, targeted re-add of the per-table autovacuum tuning for the
	// high-churn large tables, after the 2026-07-13 blanket backout (v494-502).
	// Evidence from prod 2026-07-14: transfer_contract sat at 67M dead tuples
	// (the paced reap drain's wake) with autovacuum idle, because the default
	// scale factor (0.2) does not trigger until 122M dead on a 612M-row table --
	// dead-tuple debt accumulates for days while every query on the table
	// degrades. The fixed 5M-dead thresholds below (the same settings the
	// tables had before the backout) trigger vacuum at a bounded absolute debt
	// regardless of table size. Settings are identical to the historical
	// migrations that were RESET.
	newSqlMigration(`
        ALTER TABLE contract_close SET (
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_vacuum_scale_factor = 0,
            autovacuum_vacuum_threshold = 5000000,
            autovacuum_vacuum_insert_scale_factor = 0,
            autovacuum_vacuum_insert_threshold = 10000000,
            autovacuum_analyze_scale_factor = 0,
            autovacuum_analyze_threshold = 1000000
        )
    `),
	newSqlMigration(`
        ALTER TABLE transfer_contract SET (
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_vacuum_scale_factor = 0,
            autovacuum_vacuum_threshold = 5000000,
            autovacuum_vacuum_insert_scale_factor = 0,
            autovacuum_vacuum_insert_threshold = 10000000,
            autovacuum_analyze_scale_factor = 0,
            autovacuum_analyze_threshold = 1000000
        )
    `),
	newSqlMigration(`
        ALTER TABLE transfer_escrow SET (
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_vacuum_scale_factor = 0,
            autovacuum_vacuum_threshold = 5000000,
            autovacuum_vacuum_insert_scale_factor = 0,
            autovacuum_vacuum_insert_threshold = 10000000,
            autovacuum_analyze_scale_factor = 0,
            autovacuum_analyze_threshold = 1000000
        )
    `),
	newSqlMigration(`
        ALTER TABLE transfer_escrow_sweep SET (
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_vacuum_scale_factor = 0,
            autovacuum_vacuum_threshold = 5000000,
            autovacuum_vacuum_insert_scale_factor = 0,
            autovacuum_vacuum_insert_threshold = 10000000,
            autovacuum_analyze_scale_factor = 0,
            autovacuum_analyze_threshold = 1000000
        )
    `),
	newSqlMigration(`
        ALTER TABLE network_client_location_reliability SET (
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_vacuum_scale_factor = 0,
            autovacuum_vacuum_threshold = 5000000,
            autovacuum_vacuum_insert_scale_factor = 0,
            autovacuum_vacuum_insert_threshold = 10000000,
            autovacuum_analyze_scale_factor = 0,
            autovacuum_analyze_threshold = 1000000
        )
    `),
	newSqlMigration(`
        ALTER TABLE network_client SET (
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_vacuum_scale_factor = 0.01,
            autovacuum_analyze_scale_factor = 0.02
        )
    `),
	newSqlMigration(`
        ALTER TABLE provide_key SET (
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_vacuum_scale_factor = 0.01,
            autovacuum_analyze_scale_factor = 0.02
        )
    `),
	newSqlMigration(`
        ALTER TABLE device SET (
            autovacuum_vacuum_cost_delay = 0,
            autovacuum_vacuum_scale_factor = 0.01,
            autovacuum_analyze_scale_factor = 0.02
        )
    `),

	// Pace the giant-table vacuums. The fixed 5M thresholds above (restored
	// 2026-07-14) combined with cost_delay = 0 re-created the debt-clearing
	// behavior, but during the reap-drain era the cascade giants re-qualify
	// every couple of hours, and three concurrent full-speed vacuums of
	// multi-hundred-GB tables evict the buffer cache under live traffic
	// (observed: 130+ backends in BufferMapping LWLock waits). Two changes:
	// (1) the pure cascade victims (contract_close, transfer_escrow,
	// transfer_escrow_sweep) tolerate far more dead space than the
	// query-critical transfer_contract, so their trigger rises to 25M dead
	// (<1% of table) / 50M inserts; (2) all four giants get the standard
	// autovacuum cost_delay = 2ms pacing (~80MB/s) instead of unthrottled.
	// Small hot tables (pending_task, network_client, ...) keep delay 0.
	newSqlMigration(`
        ALTER TABLE contract_close SET (
            autovacuum_vacuum_cost_delay = 2,
            autovacuum_vacuum_threshold = 25000000,
            autovacuum_vacuum_insert_threshold = 50000000
        )
    `),
	newSqlMigration(`
        ALTER TABLE transfer_escrow SET (
            autovacuum_vacuum_cost_delay = 2,
            autovacuum_vacuum_threshold = 25000000,
            autovacuum_vacuum_insert_threshold = 50000000
        )
    `),
	newSqlMigration(`
        ALTER TABLE transfer_escrow_sweep SET (
            autovacuum_vacuum_cost_delay = 2,
            autovacuum_vacuum_threshold = 25000000,
            autovacuum_vacuum_insert_threshold = 50000000
        )
    `),
	newSqlMigration(`
        ALTER TABLE transfer_contract SET (
            autovacuum_vacuum_cost_delay = 2
        )
    `),

	// Dedicated pair-lookup index for the contract-creation hot path (the
	// "existing open contract for this (source, destination) pair" check, ~68
	// calls/sec): equality on the pair + ORDER BY create_time LIMIT 1 walks this
	// index natively and stops at the first live entry. The previous plan went
	// through the wider open_source_id index without create_time (sort over all
	// matches), which degraded ~5x under heavy close churn as dead entries
	// accumulated between vacuum index-cleanup cycles (observed 115ms -> 597ms =
	// 76% of all db time). The partial predicate keeps the index tiny (only
	// open, non-companion contracts). transfer_contract is large: PRE-CREATE
	// MANUALLY with CREATE INDEX CONCURRENTLY out of band — a plain build here
	// takes a SHARE lock that blocks all contract writes for the scan; the
	// IF NOT EXISTS gate makes this migration a no-op once pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_contract_pair_open_create_time
        ON transfer_contract (source_id, destination_id, create_time)
        WHERE open AND companion_contract_id IS NULL
    `),

	// Drop the never-read handler_id index from the hottest insert path.
	// Verified on prod 2026-07-15: 349 MB, idx_scan = 0 (lifetime), while
	// network_client_connection sustains hundreds of inserts/sec that each
	// maintain it. Its intended consumer (the handler-crash cleanup in
	// network_client_model.go, which deletes by handler_id via a temp-table
	// join) planned a seq scan of this <= few-million-row table anyway, so the
	// drop changes no read plan. Pre-drop manually with
	// DROP INDEX CONCURRENTLY out of band (a plain drop takes a brief ACCESS
	// EXCLUSIVE on the table and must queue behind in-flight queries); the
	// IF EXISTS gate makes this migration a no-op once pre-dropped.
	newSqlMigration(`
        DROP INDEX IF EXISTS network_client_connection_handler_id
    `),

	// The open-contract lookups for a client (`WHERE open AND (source_id = $1
	// OR destination_id = $1)`, the per-client contract-sync path at ~6 calls/s)
	// have no open-leading destination index on prod: the destination arm
	// bitmap-scans `transfer_contract_destination_id_close_time` with only
	// destination_id in the index condition, reading the client's ENTIRE
	// contract history (whales: millions of entries) and filtering `open` at
	// the heap -- measured 72ms mean for ~1.5 rows returned. This partial index
	// contains only open contracts, so the arm becomes a direct seek.
	// transfer_contract is large: pre-create manually with CREATE INDEX
	// CONCURRENTLY out of band; the IF NOT EXISTS gate makes this migration a
	// no-op once pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS transfer_contract_open_destination_partial
        ON transfer_contract (destination_id) WHERE open
    `),

	// Defuse the 2026-07-17 planner-stats landmine (monitor/SIGNALS.md 2.3/5.8): at
	// steady state only ~10-50k of 530M rows have open=true (~6e-5), so the
	// default ANALYZE sample (300 x 100 = 30k rows) can miss every one.
	// pg_stats then records n_distinct=1 / MCV {f}@1.0, the planner estimates
	// `open = true` as ~0 rows, and the pair lookups flip to walking the whole
	// open=true range of an unrelated index (O(open-set) per call) — latent at
	// 30k open, a 96-core CPU wall at 700k (observed 7-18s/call). Target 10000
	// samples 300 x 10000 = 3M rows: even at 5k open the expected sample hits
	// ~28 (P(miss) ~ 1e-12), so both values stay in the MCV list and the pair
	// indexes keep winning. Trade-off: each ANALYZE of this table now reads
	// ~3M pages instead of 30k — still concurrent (SHARE UPDATE EXCLUSIVE,
	// cost-throttled for autoanalyze), just slower. Takes effect at the next
	// ANALYZE (nightly pass or the 1M-mod reloption threshold). The ALTER is
	// an instant catalog update and idempotent: APPLY MANUALLY on prod ahead
	// of the next nightly analyze (each analyze at the default target has a
	// meaningful chance of re-arming the mine) — re-running it here is then a
	// no-op-equivalent.
	newSqlMigration(`
        ALTER TABLE transfer_contract ALTER COLUMN open SET STATISTICS 10000
    `),

	// drain-excused reconnects (CONNECTDRAIN2.md §3.1): a reconnect caused by
	// a server drain / migrate is recorded here instead of
	// `connection_new_count`, so it never enters `client_reliability_valid`
	// and cannot invalidate the block — non-invalidating by construction, no
	// change to the validity function. Metadata-only ALTER (default, no
	// rewrite).
	newSqlMigration(`
        ALTER TABLE client_reliability ADD COLUMN connection_excused_new_count bigint NOT NULL DEFAULT 0
    `),

	// block users marker (model/network_stats_model.go
	// `StampTopLevelClientContractTime`): the last time a transfer contract
	// was created with this top-level client — or one of its child clients —
	// as the paying side. The public block_users stat counts this marker, so
	// "user" means contract-creating usage: auth/connect events fire only at
	// session setup, so they miss a client in continuous use across a block
	// rollover and count connected-but-idle clients that transfer nothing.
	// Stamps are throttled to once per `clientAuthTimeRefreshMinInterval`
	// per identity. Metadata-only ALTER (nullable, no default): no rewrite.
	newSqlMigration(`
        ALTER TABLE network_client ADD COLUMN contract_time timestamp NULL
    `),

	// serves `CountTopLevelClientsWithContractSince`: the predicate matches
	// the count query exactly ($1 <= contract_time implies IS NOT NULL) so
	// the scan is a bounded ordered range. network_client is hot: pre-create
	// manually with CREATE INDEX CONCURRENTLY out of band; the IF NOT EXISTS
	// gate makes this migration a no-op once pre-created.
	newSqlMigration(`
        CREATE INDEX IF NOT EXISTS network_client_top_level_contract_time
        ON network_client (contract_time) WHERE (active = true AND source_client_id IS NULL AND contract_time IS NOT NULL)
    `),
}
