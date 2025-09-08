package server

import (
	"context"
	"fmt"

	"github.com/golang/glog"
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
	Tx(ctx, func(tx PgTx) {
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
	Db(ctx, func(conn PgConn) {
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

func ApplyDbMigrations(ctx context.Context) {
	for i := DbVersion(ctx); i < len(migrations); i += 1 {
		Tx(ctx, func(tx PgTx) {
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
			Tx(ctx, func(tx PgTx) {
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
		Tx(ctx, func(tx PgTx) {
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
        ADD COLUMN wallet_blockchain text;
    `),

	newSqlMigration(`
        CREATE UNIQUE INDEX network_user_wallet_address_unique ON network_user (wallet_address)
        WHERE wallet_address IS NOT NULL;
    `),

	newSqlMigration(`
        ALTER TABLE account_wallet
        ADD CONSTRAINT unique_network_wallet_address
        UNIQUE (network_id, wallet_address);
    `),

	newSqlMigration(`
        ALTER TABLE account_wallet
        ADD COLUMN has_seeker_token boolean NOT NULL DEFAULT false;
    `),

	newSqlMigration(`
        ALTER TABLE network
        ADD COLUMN leaderboard_public boolean NOT NULL DEFAULT false;
    `),

	newSqlMigration(`
        ALTER TABLE network_point RENAME TO account_point
    `),

	newSqlMigration(`
		ALTER TABLE network_referral DROP CONSTRAINT network_referral_pkey;
		ALTER TABLE network_referral ADD PRIMARY KEY (network_id);
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
	    ADD COLUMN linked_network_id uuid NULL;
	`),
	newSqlMigration(`
		ALTER TABLE account_point
		ADD COLUMN account_payment_id uuid NULL;
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
}
