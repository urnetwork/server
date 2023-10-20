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
    callback func(context.Context)
}
func newCodeMigration(callback func(context.Context)) *CodeMigration {
    return &CodeMigration{
        callback: callback,
    }
}


func DbVersion(ctx context.Context) int {
    Tx(ctx, func(tx PgTx) {
        tx.Exec(
            ctx,
            `
            CREATE TABLE IF NOT EXISTS migration_audit (
                migration_time timestamp NOT NULL DEFAULT now(),
                start_version_number int NOT NULL,
                end_version_number int NULL,
                status varchar(32) NOT NULL
            )
            `,
        )
    })

    var endVersionNumber int
    Db(ctx, func(conn PgConn) {
        conn.QueryRow(
            ctx,
            `
            SELECT COALESCE(MAX(end_version_number), 0) AS max_end_version_number
            FROM migration_audit
            WHERE status = 'success'
            `,
        ).Scan(&endVersionNumber)
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
                Tx(ctx, func(tx PgTx) {
                	defer func() {
	            		if err := recover(); err != nil {
	            			// print the sql for debugging
	            			Logger().Printf("%s\n", v.sql)
	            			panic(err)
	            		}
	            	}()
                    RaisePgResult(tx.Exec(ctx, v.sql))
                })
            case *CodeMigration:
                v.callback(ctx)
            default:
                panic(fmt.Errorf("Unknown migration type %T", v))
        }
        Tx(ctx, func(tx PgTx) {
            RaisePgResult(tx.Exec(
                ctx,
                `INSERT INTO migration_audit (start_version_number, end_version_number, status) VALUES ($1, $2, 'success')`,
                i,
                i + 1,
            ))
        })
    }
}


// style: use varchar not ENUM
// style: in queries with two+ tables, use fully qualified column names

var migrations = []any{
    // newSqlMigration(`CREATE TYPE audit_provider_event_type AS ENUM (
    //  'provider_offline',
    //  'provider_online_superspeed',
    //  'provider_online_not_superspeed'
    // )`),
    // newSqlMigration(`CREATE TYPE audit_event_type AS VARCHAR(64)`),
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

    // RENAME `validate` to `verify`
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
    newSqlMigration(`
        CREATE INDEX location_type_country_code_name ON location (location_type, country_code, location_name, country_location_id, region_location_id, location_id)
    `),

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

    // net_revenue_nano_cents is how much was received for this balance (revenue - intermediary fees)
    newSqlMigration(`
        CREATE TABLE transfer_balance (
            balance_id uuid NOT NULL,
            network_id uuid NOT NULL,
            start_time timestamp NOT NULL DEFAULT now(),
            end_time timestamp NOT NULL,
            start_balance_bytes bigint NOT NULL,
            net_revenue_nano_cents bigint NOT NULL,

            balance_bytes bigint NOT NULL,
            active bool GENERATED ALWAYS AS (0 < balance_bytes) STORED,

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

    newSqlMigration(`
        CREATE TABLE transfer_contract (
            contract_id uuid NOT NULL,
            source_network_id uuid NOT NULL,
            source_id uuid NOT NULL,
            destination_network_id uuid NOT NULL,
            destination_id uuid NOT NULL,
            open bool GENERATED ALWAYS AS (dispute = false AND outcome IS NULL) STORED,
            transfer_bytes bigint NOT NULL,
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
            used_transfer_bytes bigint NOT NULL,

            PRIMARY KEY (contract_id, party)
        )
    `),
    
    // creating an escrow must deduct the balances in the same transaction
    // settling an escrow must put `payout_bytes` into the target account_balances, and `balance_bytes - payout_bytes` back into the origin balance
    // primary key is (contract_id, balance_id) because there can be multiple balances used per contract_id
    newSqlMigration(`
        CREATE TABLE transfer_escrow (
            contract_id uuid NOT NULL,
            balance_id uuid NOT NULL,
            balance_bytes bigint NOT NULL,
            
            escrow_date timestamp NOT NULL DEFAULT now(),
            
            settled bool NOT NULL DEFAULT false,
            settle_time timestamp NULL,
            payout_bytes bigint NULL,

            PRIMARY KEY (contract_id, balance_id)
        )
    `),

    // note the unpaid balance is `provided_balance_bytes - paid_balance_bytes`
    // this equals SUM(transfer_escrow_sweep.payout_bytes) where payment_id IS NULL
    // the cents to pay out is SUM(transfer_escrow_sweep.net_revenue_cents)
    newSqlMigration(`
        CREATE TABLE account_balance (
            network_id uuid NOT NULL,
            provided_bytes bigint NOT NULL DEFAULT 0,
            provided_net_revenue_nano_cents bigint NOT NULL DEFAULT 0,
            paid_bytes bigint NOT NULL DEFAULT 0,
            paid_net_revenue_nano_cents bigint NOT NULL DEFAULT 0,

            PRIMARY KEY (network_id)
        )
    `),

    // sweeps escrow value into an account
    // the payment_id is the payment the swept value was paid out using
    // the value of the unpaid balance needs to go back to the escrows to where the balance came from
    // the cents contribution of each escrow is `(escrow.payout_bytes / balance.balance_bytes) * balance.net_revenue_cents`
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
            payout_bytes bigint NOT NULL,
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

    // payment record is what was submitted to the payment processor
    // payment receipt is the receipt from the payment processor
    newSqlMigration(`
        CREATE TABLE account_payment (
            payment_id uuid NOT NULL,
            payment_plan_id uuid NOT NULL,
            wallet_id uuid NOT NULL,
            payout_bytes bigint NOT NULL,
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

    // column `user_auth_attempt.client_ipv4` is deprecated; TODO remove at a future date
    // index `user_auth_attempt_client_ipv4` is deprecated; TODO remove at a future date
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
            balance_bytes bigint NOT NULL,
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

}
