package model

// Fixtures of the account deletion cleanup's tests (network_deletion_cleanup.go),
// shared with the controller, taskworker and bringyourctl tests. Every value is
// synthetic.

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// A network shaped like an account with one device: a top-level client and its
// child on the device, and a row in the tables the cleanup deletes from, by
// network, by user, by client and by device, and in the financial tables it
// keeps.
type Testing_DeletedNetworkFixture struct {
	NetworkId server.Id
	UserId    server.Id
	ClientIds []server.Id
	DeviceIds []server.Id
	// the clients' connections, whose location, latency and speed rows go
	// with them
	ConnectionIds []server.Id
	// the network's auth codes, whose roles go with them
	AuthCodeIds []server.Id
}

// Creates the network, its admin user (a guest, with no sign-in address) and
// its rows.
func Testing_CreateDeletedNetworkFixture(ctx context.Context, networkName string) *Testing_DeletedNetworkFixture {
	fixture := &Testing_DeletedNetworkFixture{
		NetworkId: server.NewId(),
		UserId:    server.NewId(),
	}
	Testing_CreateGuestNetwork(ctx, fixture.NetworkId, networkName, fixture.UserId)
	deviceId := server.NewId()
	clientId := server.NewId()
	Testing_CreateDevice(ctx, fixture.NetworkId, deviceId, clientId, "synthetic device", "synthetic spec")
	childClientId := server.NewId()
	fixture.ClientIds = []server.Id{clientId, childClientId}
	fixture.DeviceIds = []server.Id{deviceId}

	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		exec := func(sql string, args ...any) {
			server.RaisePgResult(tx.Exec(ctx, sql, args...))
		}
		exec(
			`
				INSERT INTO network_client (client_id, network_id, device_id, description, create_time, auth_time, source_client_id)
				VALUES ($1, $2, $3, 'synthetic child', $4, $4, $5)
			`,
			childClientId, fixture.NetworkId, deviceId, now, clientId,
		)

		// the network's own rows
		exec(`INSERT INTO account_preferences (network_id, product_updates) VALUES ($1, true)`, fixture.NetworkId)
		exec(`INSERT INTO account_api_key (api_key_id, network_id, api_key, name, create_time) VALUES ($1, $2, $3, 'synthetic', $4)`, server.NewId(), fixture.NetworkId, "synthetic-"+server.NewId().String(), now)
		exec(`INSERT INTO account_feedback (feedback_id, network_id, user_id, feedback_time) VALUES ($1, $2, $3, $4)`, server.NewId(), fixture.NetworkId, fixture.UserId, now)
		exec(`INSERT INTO network_referral_code (network_id, referral_code) VALUES ($1, $2)`, fixture.NetworkId, "SYN"+server.NewId().String()[24:])
		exec(`INSERT INTO network_referral (network_id, referral_network_id, create_time) VALUES ($1, $2, $3)`, fixture.NetworkId, server.NewId(), now)
		exec(`INSERT INTO exclude_network_client_location (network_id, client_location_id) VALUES ($1, $2)`, fixture.NetworkId, server.NewId())
		authCodeId := server.NewId()
		fixture.AuthCodeIds = []server.Id{authCodeId}
		exec(
			`
				INSERT INTO auth_code (auth_code_id, network_id, user_id, auth_code, create_time, end_time, uses, remaining_uses)
				VALUES ($1, $2, $3, $4, $5, $6, 1, 1)
			`,
			authCodeId, fixture.NetworkId, fixture.UserId, "synthetic-"+authCodeId.String(), now, now.Add(time.Hour),
		)
		exec(`INSERT INTO auth_code_role (auth_code_id, role) VALUES ($1, 'synthetic')`, authCodeId)
		exec(`INSERT INTO network_onboarding_event (event_id, network_id, name, at, received_at) VALUES ($1, $2, 'synthetic', $3, $3)`, server.NewId(), fixture.NetworkId, now)
		exec(`INSERT INTO device_add_history (network_id, add_time, device_add_id, code) VALUES ($1, $2, $3, 'synthetic')`, fixture.NetworkId, now, server.NewId())
		messageId := server.NewId()
		exec(
			`
				INSERT INTO account_message_outbox (message_id, message_key, network_id, user_auth, template_name, template_json, create_time)
				VALUES ($1, $2, $3, $4, 'synthetic', '{}'::jsonb, $5)
			`,
			messageId, messageId.String(), fixture.NetworkId, fmt.Sprintf("%s@example.com", fixture.NetworkId), now,
		)
		exec(`INSERT INTO network_top_level_client_limit (network_id, top_level_client_limit, update_time) VALUES ($1, 3, $2)`, fixture.NetworkId, now)
		exec(`INSERT INTO network_connection_reliability_window (network_id, bucket_number) VALUES ($1, 1)`, fixture.NetworkId)

		// the user's own rows
		exec(`INSERT INTO user_auth_reset (user_auth_reset_id, user_id, reset_code) VALUES ($1, $2, $3)`, server.NewId(), fixture.UserId, "synthetic-"+server.NewId().String())
		exec(`INSERT INTO user_auth_verify (user_auth_verify_id, user_id, verify_code) VALUES ($1, $2, $3)`, server.NewId(), fixture.UserId, server.NewId().String()[24:])
		exec(`INSERT INTO network_user_action_attempt (network_user_action_attempt_id, user_id, action, create_time) VALUES ($1, $2, 'synthetic', $3)`, server.NewId(), fixture.UserId, now)

		// each client's rows
		for _, clientId := range fixture.ClientIds {
			connectionId := server.NewId()
			fixture.ConnectionIds = append(fixture.ConnectionIds, connectionId)
			exec(
				`
					INSERT INTO network_client_connection (
						connection_id, client_id, connected, connect_time, disconnect_time,
						connection_host, connection_service, connection_block
					)
					VALUES ($1, $2, false, $3, $3, 'synthetic.example', 'connect', 'synthetic')
				`,
				connectionId, clientId, now,
			)
			exec(
				`
					INSERT INTO network_client_location (connection_id, client_id, city_location_id, region_location_id, country_location_id)
					VALUES ($1, $2, $3, $3, $3)
				`,
				connectionId, clientId, server.NewId(),
			)
			exec(`INSERT INTO network_client_latency (connection_id, latency_ms) VALUES ($1, 10)`, connectionId)
			exec(`INSERT INTO network_client_speed (connection_id, bytes_per_second) VALUES ($1, 1000)`, connectionId)
			exec(`INSERT INTO network_client_location_reliability (client_id, update_block_number, network_id) VALUES ($1, 1, $2)`, clientId, fixture.NetworkId)
			exec(`INSERT INTO client_tls_certificate (client_id, tls_certificate_pem, set_time) VALUES ($1, '\x00'::bytea, $2)`, clientId, now)
			exec(`INSERT INTO provide_key (client_id, provide_mode, secret_key) VALUES ($1, $2, '\x01'::bytea)`, clientId, ProvideModePublic)
			exec(`INSERT INTO provide_key_change (client_id, change_time) VALUES ($1, $2)`, clientId, now)
			exec(`INSERT INTO network_client_provider_intent (client_id, create_time) VALUES ($1, $2)`, clientId, now)
			exec(`INSERT INTO network_client_role (client_id, role) VALUES ($1, 'synthetic')`, clientId)
			exec(
				`
					INSERT INTO provider_egress_health (client_id, measured_at, ok_count, total_count, class_results, reputation_ok, reputation_total)
					VALUES ($1, $2, 1, 1, '{}'::jsonb, 1, 1)
				`,
				clientId, now,
			)
			exec(
				`
					INSERT INTO provider_egress_location (client_id, location_id, country_code, observed_at, update_time)
					VALUES ($1, $2, 'xx', $3, $3)
				`,
				clientId, server.NewId(), now,
			)
			exec(`INSERT INTO search_provider_stats (period_start, period_end, client_id, match_count) VALUES ($1, $2, $3, 1)`, now.Add(-time.Hour), now, clientId)
			exec(`INSERT INTO verify_provider_stats (period_start, period_end, client_id, assignments, confirmations) VALUES ($1, $2, $3, 1, 1)`, now.Add(-time.Hour), now, clientId)
			exec(
				`
					INSERT INTO network_client_data_usage (client_id, period_start, network_id, used_byte_count, update_time)
					VALUES ($1, $2, $3, 1, $2)
				`,
				clientId, now, fixture.NetworkId,
			)
			exec(
				`
					INSERT INTO client_reliability_running (client_id, lookback_index, network_id, independent_sum, reliability_sum)
					VALUES ($1, 0, $2, 1, 1)
				`,
				clientId, fixture.NetworkId,
			)
		}

		// the device's audit rows
		exec(`INSERT INTO audit_device_state (device_id, network_id, online, event_time) VALUES ($1, $2, false, $3)`, deviceId, fixture.NetworkId, now)
		exec(`INSERT INTO audit_device_event (event_id, event_time, network_id, device_id, event_type) VALUES ($1, $2, $3, $4, 'synthetic')`, server.NewId(), now, fixture.NetworkId, deviceId)
		exec(
			`
				INSERT INTO audit_provider_state (device_id, network_id, online, superspeed, country_name, region_name, city_name, event_time)
				VALUES ($1, $2, false, false, 'synthetic', 'synthetic', 'synthetic', $3)
			`,
			deviceId, fixture.NetworkId, now,
		)

		// the financial rows, which the cleanup keeps
		exec(
			`
				INSERT INTO transfer_balance (balance_id, network_id, start_time, end_time, start_balance_byte_count, net_revenue_nano_cents, balance_byte_count)
				VALUES ($1, $2, $3, $4, 1, 0, 1)
			`,
			server.NewId(), fixture.NetworkId, now, now.Add(24*time.Hour),
		)
		exec(`INSERT INTO subscription_payment (subscription_payment_id, network_id, user_id, create_time) VALUES ($1, $2, $3, $4)`, server.NewId(), fixture.NetworkId, fixture.UserId, now)
		walletId := server.NewId()
		exec(
			`
				INSERT INTO account_wallet (wallet_id, network_id, wallet_type, blockchain, wallet_address, default_token_type, active, create_time)
				VALUES ($1, $2, 'external', 'SOL', $3, 'USDC', true, $4)
			`,
			walletId, fixture.NetworkId, "synthetic-"+walletId.String(), now,
		)
		exec(`INSERT INTO payout_wallet (network_id, wallet_id) VALUES ($1, $2)`, fixture.NetworkId, walletId)
		exec(`INSERT INTO account_point (account_point_id, network_id, event, point_value, create_time) VALUES ($1, $2, 'synthetic', 1, $3)`, server.NewId(), fixture.NetworkId, now)
	})
	return fixture
}

// The rows left of the fixture in every table the cleanup deletes from, by
// table; a table with none is absent.
func Testing_CountDeletedNetworkFixtureRows(ctx context.Context, fixture *Testing_DeletedNetworkFixture) map[string]int64 {
	networkIds := idStrings([]server.Id{fixture.NetworkId})
	userIds := idStrings([]server.Id{fixture.UserId})
	clientIds := idStrings(fixture.ClientIds)
	deviceIds := idStrings(fixture.DeviceIds)
	connectionIds := idStrings(fixture.ConnectionIds)
	type countQuery struct {
		table string
		sql   string
		args  []any
	}
	queries := []countQuery{
		{table: "network_client", sql: `SELECT count(*) FROM network_client WHERE network_id = ANY($1) OR client_id = ANY($2)`, args: []any{networkIds, clientIds}},
		{table: "network_client_active", sql: `SELECT count(*) FROM network_client WHERE (network_id = ANY($1) OR client_id = ANY($2)) AND active`, args: []any{networkIds, clientIds}},
		{table: "device", sql: `SELECT count(*) FROM device WHERE network_id = ANY($1) OR device_id = ANY($2)`, args: []any{networkIds, deviceIds}},
		{table: "auth_code", sql: `SELECT count(*) FROM auth_code WHERE network_id = ANY($1) OR user_id = ANY($2)`, args: []any{networkIds, userIds}},
		{table: "auth_code_role", sql: `SELECT count(*) FROM auth_code_role WHERE auth_code_id = ANY($1)`, args: []any{idStrings(fixture.AuthCodeIds)}},
		{table: "network_client_latency", sql: `SELECT count(*) FROM network_client_latency WHERE connection_id = ANY($1)`, args: []any{connectionIds}},
		{table: "network_client_speed", sql: `SELECT count(*) FROM network_client_speed WHERE connection_id = ANY($1)`, args: []any{connectionIds}},
		{table: "network_client_connection", sql: `SELECT count(*) FROM network_client_connection WHERE client_id = ANY($1)`, args: []any{clientIds}},
	}
	for _, table := range deletedNetworkTables {
		queries = append(queries, countQuery{table: table.table, sql: table.countSql(), args: []any{networkIds}})
	}
	for _, table := range deletedUserTables {
		queries = append(queries, countQuery{table: table.table, sql: table.countSql(), args: []any{userIds}})
	}
	for _, table := range deletedClientHistoryTables {
		queries = append(queries, countQuery{table: table.table, sql: table.countSql(), args: []any{clientIds}})
	}
	for _, table := range deletedClientTables {
		queries = append(queries, countQuery{table: table, sql: fmt.Sprintf(`SELECT count(*) FROM %s WHERE client_id = ANY($1)`, table), args: []any{clientIds}})
	}
	for _, table := range deletedDeviceTables {
		queries = append(queries, countQuery{table: table.table, sql: fmt.Sprintf(`SELECT count(*) FROM %s WHERE device_id = ANY($1)`, table.table), args: []any{deviceIds}})
	}
	counts := map[string]int64{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, query := range queries {
			var count int64
			server.Raise(conn.QueryRow(ctx, query.sql, query.args...).Scan(&count))
			if 0 < count {
				counts[query.table] += count
			}
		}
	})
	return counts
}

// The fixture's rows in the financial tables the cleanup keeps, by table.
func Testing_CountDeletedNetworkFixtureKeptRows(ctx context.Context, fixture *Testing_DeletedNetworkFixture) map[string]int64 {
	counts := map[string]int64{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, table := range []string{"transfer_balance", "subscription_payment", "account_wallet", "payout_wallet", "account_point"} {
			var count int64
			server.Raise(conn.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE network_id = $1`, table), fixture.NetworkId).Scan(&count))
			counts[table] = count
		}
	})
	return counts
}
