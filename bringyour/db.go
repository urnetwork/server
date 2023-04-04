package bringyour

import (
	"sync"
	"context"
	"fmt"

	// "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)



type SafePool struct {
	mutex sync.Mutex
	pool *pgxpool.Pool
}
func (self SafePool) init() {
	//   - pool_max_conns: integer greater than 0
//   - pool_min_conns: integer 0 or greater
//   - pool_max_conn_lifetime: duration string
//   - pool_max_conn_idle_time: duration string
//   - pool_health_check_period: duration string
//   - pool_max_conn_lifetime_jitter: duration string
	// postgres://jack:secret@pg.example.com:5432/mydb?sslmode=verify-ca&pool_max_conns=10
	// fixme
	postgresUrl := ""
	var err error
	self.pool, err = pgxpool.New(context.Background(), postgresUrl)
	if err != nil {
		panic(fmt.Sprintf("Unable to connect to database: %s", err))
	}

}

var safePool *SafePool = &SafePool{}


func pool() *pgxpool.Pool {
	safePool.mutex.Lock()
	defer safePool.mutex.Unlock()
	if safePool.pool == nil {
		safePool.init()
	}
	return safePool.pool
}



type DbRetryOptions struct {
	// rerun the entire callback on error
	rerunOnError bool
}
func newDbRetryOptions(rerunOnError bool) *DbRetryOptions {
	return &DbRetryOptions{
		rerunOnError: rerunOnError,
	}
}



func Db(callback func(context.Context, *pgxpool.Conn), options ...any) error {
	// fixme rerun callback on disconnect with a new connection

	context := context.Background()

	var conn *pgxpool.Conn
	var err error

	// fixme retry if error

	conn, err = pool().Acquire(context)
	if err != nil {
		return err
	}

	err = conn.Ping(context)
	if err != nil {
		// take the bad connection out of the pool
		pgxConn := conn.Hijack()
		pgxConn.Close(context)
		return err
	}

	defer conn.Release()
	callback(context, conn)
	return nil
}


func Tx(callback func(context.Context, *pgxpool.Conn), options ...any) error {
	// fixme
	return Db(callback)
}




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
	callback func(context.Context, *pgxpool.Conn)
}
func newCodeMigration(callback func(context.Context, *pgxpool.Conn)) *CodeMigration {
	return &CodeMigration{
		callback: callback,
	}
}




func ApplyMigrations() {
	Tx(func(context context.Context, conn *pgxpool.Conn) {
		conn.Exec(
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
	Db(func(context context.Context, conn *pgxpool.Conn) {
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


	Tx(func(context context.Context, conn *pgxpool.Conn) {
		conn.Exec(
			context,
			`INSERT INTO migration_audit (start_version_number, status) VALUES ($1, 'start')`,
			endVersionNumber,
		)
		for i := endVersionNumber; i < len(migrations); i += 1 {
			switch v := migrations[i].(type) {
				case SqlMigration:
					conn.Exec(context, v.sql)
				case CodeMigration:
					v.callback(context, conn)
				default:
					panic(fmt.Sprintf("Unknown migration type %T", v))
			}
		}
		conn.Exec(
			context,
			`INSERT INTO migration_audit (start_version_number, end_version_number, status) VALUES ($1, $2, 'success')`,
			endVersionNumber,
			len(migrations),
		)
	})
}

var migrations = []any{
	newSqlMigration(`CREATE TYPE audit_provider_event_type AS ENUM (
		'provider_offline',
		'provider_online_superspeed',
		'provider_online_not_superspeed'
	)`),
	newSqlMigration(`
		CREATE TABLE audit_provider_event (
			event_id uuid NOT NULL,
			event_time timestamp NOT NULL DEFAULT now(),
			network_id uuid NOT NULL,
			device_id uuid NOT NULL,
			event_type audit_provider_event_type NOT NULL,
			event_details VARCHAR(1024) NULL,
			country_name VARCHAR(128) NULL,
			region_name VARCHAR(128) NULL,
			city_name VARCHAR(128) NULL,

			PRIMARY KEY (event_id)
		)
	`),
	newSqlMigration(`
		CREATE INDEX audit_provider_event_stats_device_id ON audit_provider_event (event_time, device_id, event_id)
	`),
	newSqlMigration(`CREATE TYPE audit_extender_event_type AS ENUM (
		'extender_offline',
		'extender_online_superspeed',
		'extender_online_not_superspeed'
	)`),
	newSqlMigration(`
		CREATE TABLE audit_extender_event (
			event_id uuid NOT NULL,
			event_time timestamp NOT NULL DEFAULT now(),
			network_id uuid NOT NULL,
			extender_id uuid NOT NULL,
			event_type audit_extender_event_type NOT NULL,
			event_details VARCHAR(32) NULL,

			PRIMARY KEY (event_id)
		)
	`),
	newSqlMigration(`
		CREATE INDEX audit_extender_event_stats_extender_id ON audit_extender_event (event_time, extender_id, event_id)
	`),
	newSqlMigration(`CREATE TYPE audit_network_event_type AS ENUM (
		'network_created',
		'network_deleted'
	)`),
	newSqlMigration(`
		CREATE TABLE audit_network_event (
			event_id uuid NOT NULL,
			event_time timestamp NOT NULL DEFAULT now(),
			network_id uuid NOT NULL,
			event_type audit_network_event_type NOT NULL,
			event_details VARCHAR(32) NULL,

			PRIMARY KEY (event_id)
		)
	`),
	newSqlMigration(`
		CREATE INDEX audit_network_event_stats_network_id ON audit_network_event (event_time, network_id, event_id)
	`),
	newSqlMigration(`CREATE TYPE audit_device_event_type AS ENUM (
		'device_added',
		'device_removed'
	)`),
	newSqlMigration(`
		CREATE TABLE audit_device_event (
			event_id uuid NOT NULL,
			event_time timestamp NOT NULL DEFAULT now(),
			network_id uuid NOT NULL,
			device_id uuid NOT NULL,
			event_type audit_device_event_type NOT NULL,
			event_details VARCHAR(32) NULL,

			PRIMARY KEY (event_time, device_id, event_id),
			UNIQUE (event_id)
		)
	`),
	newSqlMigration(`
		CREATE INDEX audit_device_event_stats_device_id ON audit_device_event (event_time, device_id, event_id)
	`),
	newSqlMigration(`CREATE TYPE audit_contract_event_type AS ENUM (
		'contract_closed_success'
	)`),
	newSqlMigration(`
		CREATE TABLE audit_contract_event (
			event_id uuid NOT NULL,
			event_time timestamp NOT NULL DEFAULT now(),
			contract_id uuid NOT NULL,
			client_network_id uuid NOT NULL,
			client_device_id uuid NOT NULL,
			provider_network_id uuid NOT NULL,
			provider_device_id uuid NOT NULL,
			extender_network_id uuid NOT NULL,
			extender_id uuid NOT NULL,
			event_type audit_contract_event_type NOT NULL,
			event_details VARCHAR(32) NULL,
			transfer_bytes BIGINT NOT NULL DEFAULT 0,
			transfer_packets BIGINT NOT NULL DEFAULT 0,

			UNIQUE (event_id)
		)
	`),
	newSqlMigration(`
		CREATE INDEX audit_contract_event_stats ON audit_contract_event (event_time, transfer_bytes, transfer_packets)
	`),
	newSqlMigration(`
		CREATE INDEX audit_contract_event_stats_extender_id ON audit_contract_event (event_time, extender_id, transfer_bytes, transfer_packets)
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
		CREATE TYPE auth_type AS ENUM (
			'password',
			'apple',
			'google'
		)
	`),
	// password_hash: 32-bit argon2 hash digest
	newSqlMigration(`
		CREATE TABLE network_user (
			user_id uuid NOT NULL,
			user_name VARCHAR(128) NOT NULL,
			auth_type auth_type NOT NULL,
			user_auth VARCHAR(256) NULL,
			password_hash VARCHAR(64) NULL,
			auth_jwt VARCHAR(1024) NULL,
			validated BOOL NOT NULL DEFAULT false,

			PRIMARY KEY (user_id)
		)
	`),
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
	newSqlMigration(`
		CREATE TABLE user_auth_reset (
			user_auth_reset_id uuid NOT NULL,
			user_id uuid NOT NULL,
			reset_time timestamp NOT NULL DEFAULT now(),
			reset_code VARCHAR(128) NOT NULL,
			used BOOL NOT NULL DEFAULT false,

			PRIMARY KEY (user_auth_reset_id)
		)
	`),
	newSqlMigration(`
		CREATE INDEX user_auth_reset_user_id ON user_auth_reset (user_id, reset_time)
	`),
	newSqlMigration(`
		CREATE TABLE user_auth_validate (
			user_auth_validate_id uuid NOT NULL,
			user_id uuid NOT NULL,
			validate_time timestamp NOT NULL DEFAULT now(),
			validate_code VARCHAR(16) NOT NULL,
			used BOOL NOT NULL DEFAULT false,

			PRIMARY KEY (user_auth_validate_id)
		)
	`),
	newSqlMigration(`
		CREATE INDEX user_auth_validate_user_id ON user_auth_validate (user_id, validate_time)
	`),
}
